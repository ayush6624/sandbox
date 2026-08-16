"""A Harbor execution backend backed by Firecracker microVM sandboxes.

Harbor (the harness behind Terminal-Bench 2.0) runs each task in a container it
provisions through a pluggable *environment*. Third-party environments need no
fork: `harbor run --environment-import-path <module>:<Class>` loads any class
that satisfies `harbor.environments.base.BaseEnvironment`, and `type()` returns
a plain `str` precisely so out-of-tree backends can name themselves.

This is the same shape as Harbor's own `EC2Environment`: the task's
`docker-compose.yaml` runs **inside** the VM (Docker-in-microVM) and
`DinDComposeOps` supplies the compose-level operations on top of two primitives
we implement — run a shell command on the VM, and move files to/from it. The
difference is what the VM costs: an EC2 instance is ~40 s and a billed hour, a
sandbox is ~12 ms p50 from the ready pool and is metered per second.

    pip install httpx                       # plus harbor itself
    export SANDBOX_API_URL=http://10.160.0.100:9090
    export SANDBOX_API_KEY=...

    harbor run \
      --dataset terminal-bench@2.0 \
      --agent oracle \
      --environment-import-path examples.rl_environments.harbor_sandbox_env:SandboxEnvironment \
      --n-concurrent 32

Scope of this version — deliberate, and each one fails loudly rather than
silently:
  * compose tasks and single-Dockerfile tasks (build or prebuilt image);
  * no GPU/TPU, no Windows, no network-policy enforcement (declared as
    unsupported capabilities, so Harbor refuses such a task up front instead of
    running it wrongly);
  * task cpus/memory_mb size the **VM**, not a cgroup inside it. Requesting more
    than the sandbox template's default forces a cold boot (Firecracker bakes
    vcpus/mem into a snapshot, so an override cannot be served from the golden
    snapshot).
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import shlex
from pathlib import Path
from typing import Any, override

from harbor.environments.base import BaseEnvironment, ExecResult
from harbor.environments.capabilities import (
    EnvironmentCapabilities,
    EnvironmentResourceCapabilities,
)
from harbor.environments.compose_service_ops import (
    ComposeServiceOpsMixin,
    ComposeServiceTransport,
)
from harbor.environments.definition import should_use_prebuilt_docker_image
from harbor.environments.dind_compose import DinDComposeOps
from harbor.environments.docker import (
    COMPOSE_BUILD_PATH,
    COMPOSE_PREBUILT_PATH,
)
from harbor.models.task.config import EnvironmentConfig, TaskOS
from harbor.models.trial.paths import EnvironmentPaths, TrialPaths

from .sandbox_client import SandboxClient, SandboxError, acquire_with_backoff

SANDBOX_ROOT = "/home/sandbox/harbor"
"""Everything for one trial lives under here in the guest: the staged compose
overlays, the task's environment dir, and scratch for file transfers."""

DOCKER_READY_TIMEOUT_SEC = 180
COMPOSE_UP_TIMEOUT_SEC = 600


class _SandboxComposeOps(DinDComposeOps):
    """The five provider primitives `DinDComposeOps` builds compose ops on."""

    # Our transfers go through the API rather than a mounted filesystem, so the
    # default `docker compose cp` timeouts are raised a little.
    _CP_FILE_TIMEOUT_SEC = 120
    _CP_DIR_TIMEOUT_SEC = 300

    def __init__(self, env: "SandboxEnvironment") -> None:
        self._env = env

    @override
    async def _compose_exec(
        self, subcommand: list[str], timeout_sec: int | None = None
    ) -> ExecResult:
        command = " ".join(
            ["docker", "compose", *self._env.compose_file_flags(), *map(shlex.quote, subcommand)]
        )
        return await self._host_exec(command, timeout_sec=timeout_sec)

    @override
    async def _host_exec(
        self, command: str, timeout_sec: int | None = None
    ) -> ExecResult:
        return await self._env.host_exec(command, timeout_sec=timeout_sec)

    @override
    async def _stage_file_to_host(self, source_path: Path | str, host_path: str) -> None:
        await self._env.upload_file_to_host(source_path, host_path)

    @override
    async def _stage_dir_to_host(self, source_dir: Path | str, host_dir: str) -> None:
        await self._env.upload_dir_to_host(source_dir, host_dir)

    @override
    async def _fetch_file_from_host(self, host_path: str, target_path: Path | str) -> None:
        await self._env.download_file_from_host(host_path, target_path)

    @override
    async def _fetch_dir_from_host(self, host_dir: str, target_dir: Path | str) -> None:
        await self._env.download_dir_from_host(host_dir, target_dir)


class SandboxEnvironment(ComposeServiceOpsMixin, BaseEnvironment):
    """One Firecracker microVM per trial, running the task's compose project."""

    def __init__(
        self,
        environment_dir: Path,
        environment_name: str,
        session_id: str,
        trial_paths: TrialPaths,
        task_env_config: EnvironmentConfig,
        *args: Any,
        api_url: str | None = None,
        api_key: str | None = None,
        snapshot_id: str | None = None,
        template_vcpus: int = 2,
        template_memory_mib: int = 1024,
        ttl_seconds: int = 3 * 60 * 60,
        **kwargs: Any,
    ) -> None:
        super().__init__(
            environment_dir=environment_dir,
            environment_name=environment_name,
            session_id=session_id,
            trial_paths=trial_paths,
            task_env_config=task_env_config,
            **kwargs,
        )
        self._api_url = api_url or os.environ.get("SANDBOX_API_URL")
        self._api_key = api_key or os.environ.get("SANDBOX_API_KEY")
        # A prepared snapshot is how Docker (and any other per-experiment
        # tooling) gets into the guest: provision one sandbox by hand, snapshot
        # it, and clone every trial from it. Firecracker bakes vcpus/mem into a
        # snapshot, so the snapshot also FIXES the trial size — which is why the
        # resource overrides below are skipped entirely on this path.
        self._snapshot_id = snapshot_id or os.environ.get("SANDBOX_SNAPSHOT_ID")
        # The template's defaults. A task that fits inside them is served from
        # the ready pool; a task that needs more pays a cold boot.
        self._template_vcpus = template_vcpus
        self._template_memory_mib = template_memory_mib
        # A TTL is a safety net, not the teardown path: `stop()` deletes the
        # sandbox. This is what stops a crashed harness from leaking VMs.
        self._ttl_seconds = ttl_seconds

        self._client: SandboxClient | None = None
        self._sandbox_id: str | None = None
        self._compose_ops = _SandboxComposeOps(self)
        self._use_prebuilt = False
        # Set when this trial was cloned from a task snapshot, i.e. the image is
        # already built in the guest and the build step can be skipped.
        self._image_prebaked = False
        self._cache_disabled = bool(os.environ.get("SANDBOX_NO_TASK_SNAPSHOT"))

    # ── identity & declarations ──────────────────────────────────────────

    @staticmethod
    @override
    def type() -> str:
        return "sandbox-firecracker"

    @classmethod
    @override
    def preflight(cls) -> None:
        if not os.environ.get("SANDBOX_API_URL"):
            raise SystemExit(
                "The sandbox environment requires SANDBOX_API_URL (the gateway "
                "address, e.g. http://10.160.0.100:9090). Set it and retry."
            )
        if not os.environ.get("SANDBOX_API_KEY"):
            raise SystemExit("The sandbox environment requires SANDBOX_API_KEY.")

    @classmethod
    @override
    def resource_capabilities(cls) -> EnvironmentResourceCapabilities:
        # Task cpus/memory size the guest VM, which is a hard boundary for
        # memory (admission-checked) and a share-based one for CPU (the fleet
        # deliberately oversubscribes CPU ~6:1, so a *limit* would be a lie).
        return EnvironmentResourceCapabilities(
            cpu_request=True,
            memory_request=True,
            memory_limit=True,
        )

    @property
    @override
    def capabilities(self) -> EnvironmentCapabilities:
        return EnvironmentCapabilities(
            docker_compose=True,
            # Not yet: egress control needs per-sandbox iptables policy on the
            # worker. Declaring it false makes Harbor refuse a task that needs
            # network isolation rather than run it unisolated.
            disable_internet=False,
            network_allowlist=False,
            gpus=False,
            tpus=False,
            windows=False,
            mounted=False,
        )

    @override
    def _validate_definition(self) -> None:
        if self.os is not TaskOS.LINUX:
            raise ValueError(
                f"The sandbox environment runs Linux guests only (task requested {self.os})."
            )
        if not (
            self._environment_docker_compose_path.exists()
            or (self.environment_dir / "Dockerfile").exists()
            or self.task_env_config.docker_image
        ):
            raise FileNotFoundError(
                f"{self.environment_dir} has neither docker-compose.yaml nor "
                "Dockerfile, and the task declares no docker_image."
            )

    # ── task-snapshot cache ──────────────────────────────────────────────
    #
    # This is the local equivalent of what a provider with a template-build API
    # gets for free. Harbor's E2BEnvironment turns a task's Dockerfile into a
    # native template keyed by a content hash and skips the build whenever that
    # alias already exists, so an image build is paid once per task ever. We have
    # no template-build API (`/v1/templates` is read-only), and the Docker-in-VM
    # route otherwise pays a full `docker compose build` inside the guest on
    # every single trial.
    #
    # A snapshot is the primitive that closes the gap: build the task's image
    # once in a real sandbox, snapshot it, and clone every later trial from that
    # instead — a ~1 s restore rather than a 30-60 s build. The cache maps a hash
    # of the task's environment directory to the snapshot id, so editing a task's
    # Dockerfile invalidates it exactly the way E2B's alias hash does.

    def _env_hash(self) -> str:
        """Hash the build inputs: the environment dir, plus the base snapshot.

        The base snapshot is part of the key because the built image lives
        *inside* it — rebuild the Docker base and every task snapshot derived
        from it describes a different machine.
        """
        digest = hashlib.sha256()
        digest.update((self._snapshot_id or "no-base").encode())
        digest.update(str(self.task_env_config.docker_image or "").encode())
        for path in sorted(
            p for p in self.environment_dir.rglob("*") if p.is_file()
        ):
            digest.update(str(path.relative_to(self.environment_dir)).encode())
            digest.update(path.read_bytes())
        return digest.hexdigest()[:16]

    def _cache_path(self) -> Path:
        root = os.environ.get("SANDBOX_TASK_SNAPSHOT_CACHE")
        if root:
            return Path(root)
        return Path.home() / ".cache" / "sandbox-harbor" / "task-snapshots.json"

    def _read_cache(self) -> dict[str, Any]:
        try:
            return json.loads(self._cache_path().read_text())
        except (OSError, ValueError):
            return {}

    def _lookup_task_snapshot(self) -> str | None:
        if not self._snapshot_id or self._cache_disabled:
            return None
        entry = self._read_cache().get(self._env_hash())
        return entry.get("snapshot_id") if isinstance(entry, dict) else None

    def _record_task_snapshot(self, snapshot_id: str) -> None:
        """Write the mapping, tolerating a concurrent writer.

        Two trials of the same task that both miss the cache each build and each
        snapshot; last writer wins and the loser's snapshot is simply unused.
        That wastes one build, never correctness, so it isn't worth a lock —
        an oracle sweep runs one trial per task anyway.
        """
        path = self._cache_path()
        cache = self._read_cache()
        cache[self._env_hash()] = {
            "snapshot_id": snapshot_id,
            "task": self.environment_name,
            "base_snapshot": self._snapshot_id,
        }
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            tmp = path.with_suffix(".tmp.%d" % os.getpid())
            tmp.write_text(json.dumps(cache, indent=2, sort_keys=True))
            tmp.replace(path)
        except OSError as exc:
            self.logger.warning("could not record task snapshot %s: %s", snapshot_id, exc)

    def _forget_task_snapshot(self) -> None:
        path = self._cache_path()
        cache = self._read_cache()
        if cache.pop(self._env_hash(), None) is None:
            return
        try:
            tmp = path.with_suffix(".tmp.%d" % os.getpid())
            tmp.write_text(json.dumps(cache, indent=2, sort_keys=True))
            tmp.replace(path)
        except OSError:
            pass

    # ── guest paths ──────────────────────────────────────────────────────

    @property
    def _project_name(self) -> str:
        """Compose project name, derived from the TASK rather than the trial.

        Every trial gets its own microVM, so there is exactly one compose
        project per guest and nothing to collide with — while a per-trial name
        would defeat the task-snapshot cache entirely: compose tags built images
        `<project>-<service>`, so a name carrying the trial id makes the image
        baked into the snapshot unreachable by the next trial, which then
        rebuilds it. It also keeps the staged paths below identical across
        trials, so a snapshot's staged compose files land where the clone
        expects them.
        """
        # Compose project names allow [a-z0-9_-]; task names may carry others.
        cleaned = "".join(
            char if char.isalnum() or char in "-_" else "-"
            for char in self.environment_name.lower()
        )
        return cleaned.strip("-_") or "harbor"

    @property
    def _session_dir(self) -> str:
        return f"{SANDBOX_ROOT}/{self._project_name}"

    @property
    def _compose_dir(self) -> str:
        return f"{self._session_dir}/compose"

    @property
    def _environment_dir_guest(self) -> str:
        return f"{self._session_dir}/environment"

    @property
    def _environment_docker_compose_path(self) -> Path:
        return self.environment_dir / "docker-compose.yaml"

    def compose_file_flags(self) -> list[str]:
        """`-f` flags for every overlay, in precedence order."""
        overlay = (
            COMPOSE_PREBUILT_PATH.name if self._use_prebuilt else COMPOSE_BUILD_PATH.name
        )
        files = [f"{self._compose_dir}/{overlay}"]
        if self._environment_docker_compose_path.exists():
            files.append(f"{self._environment_dir_guest}/docker-compose.yaml")
        flags: list[str] = ["-p", self._project_name]
        for path in files:
            flags.extend(["-f", path])
        return flags

    # ── primitives used by _SandboxComposeOps ────────────────────────────

    def _require(self) -> tuple[SandboxClient, str]:
        if self._client is None or self._sandbox_id is None:
            raise RuntimeError("sandbox environment used before start()")
        return self._client, self._sandbox_id

    async def host_exec(
        self, command: str, timeout_sec: int | None = None
    ) -> ExecResult:
        """Run a command on the DinD host — i.e. inside the guest VM itself."""
        client, sandbox_id = self._require()
        env_vars = {
            "CONTEXT_DIR": self._environment_dir_guest,
            "PREBUILT_IMAGE_NAME": self.task_env_config.docker_image or "",
        }
        result = await client.exec(
            sandbox_id,
            command,
            cwd=self._compose_dir,
            env=env_vars,
            timeout_sec=int(timeout_sec or 300),
        )
        return ExecResult(
            stdout=result.stdout, stderr=result.stderr, return_code=result.exit_code
        )

    async def upload_file_to_host(self, source_path: Path | str, host_path: str) -> None:
        client, sandbox_id = self._require()
        data = Path(source_path).read_bytes()
        await client.write_file(sandbox_id, host_path, data)

    async def upload_dir_to_host(self, source_dir: Path | str, host_dir: str) -> None:
        client, sandbox_id = self._require()
        await client.upload_dir(sandbox_id, str(source_dir), host_dir)

    async def download_file_from_host(
        self, host_path: str, target_path: Path | str
    ) -> None:
        client, sandbox_id = self._require()
        data = await client.read_file(sandbox_id, host_path)
        target = Path(target_path)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)

    async def download_dir_from_host(
        self, host_dir: str, target_dir: Path | str
    ) -> None:
        client, sandbox_id = self._require()
        await client.download_dir(sandbox_id, host_dir, str(target_dir))

    # ── lifecycle ────────────────────────────────────────────────────────

    @override
    async def start(self, force_build: bool) -> None:
        self._client = SandboxClient(self._api_url, self._api_key)

        # Only override resources when the task genuinely needs more than the
        # template gives — an override costs the hot-create path. A snapshot
        # source takes neither: the API rejects vcpu/memory on a restore with
        # 400, because a restored VM runs whatever its snapshot baked.
        vcpus: int | None = None
        memory_mib: int | None = None
        if not self._snapshot_id:
            wanted_vcpus = self._effective_cpus
            wanted_memory = self._effective_memory_mb
            if wanted_vcpus and wanted_vcpus > self._template_vcpus:
                vcpus = wanted_vcpus
            if wanted_memory and wanted_memory > self._template_memory_mib:
                memory_mib = wanted_memory
        elif self._effective_memory_mb and self._effective_memory_mb > self._template_memory_mib:
            # Not fatal — the snapshot may well have been prepared larger than
            # the template — but a task asking for more than the snapshot was
            # built with will OOM inside the guest, and that reads as a task
            # failure rather than a sizing mistake. Say so once, up front.
            self.logger.warning(
                "task %s wants %s MiB but trials are cloned from snapshot %s, whose "
                "memory was fixed when it was prepared; rebuild the snapshot larger "
                "if this task OOMs",
                self.environment_name,
                self._effective_memory_mb,
                self._snapshot_id,
            )

        # Prefer this task's own snapshot (image already built) over the plain
        # Docker base. On any failure fall back to the base and rebuild: a task
        # snapshot can legitimately go missing — deleted by hand, or created on
        # another worker and not yet replicated to the durability bucket — and
        # "rebuild it" is always a correct answer, where failing the trial isn't.
        task_snapshot = self._lookup_task_snapshot()
        try:
            sandbox = await self._acquire(task_snapshot or self._snapshot_id, vcpus, memory_mib)
            self._image_prebaked = task_snapshot is not None
        except SandboxError as exc:
            if task_snapshot is None:
                raise
            self.logger.warning(
                "task snapshot %s unusable (%s); forgetting it and rebuilding from %s",
                task_snapshot, exc, self._snapshot_id,
            )
            self._forget_task_snapshot()
            sandbox = await self._acquire(self._snapshot_id, vcpus, memory_mib)
            self._image_prebaked = False

        self._sandbox_id = sandbox.id
        self.logger.info(
            "sandbox %s up for %s (%s vcpu / %s MiB, image %s)",
            sandbox.id,
            self.session_id,
            sandbox.vcpus or self._template_vcpus,
            sandbox.memory_mib or self._template_memory_mib,
            "prebaked" if self._image_prebaked else "to build",
        )

        await self._bootstrap_dirs()
        await self._ensure_docker_ready()
        await self._stage_and_up(force_build)

    async def _acquire(
        self, snapshot_id: str | None, vcpus: int | None, memory_mib: int | None
    ):
        assert self._client is not None
        return await acquire_with_backoff(
            self._client,
            snapshot_id=snapshot_id,
            name=self.environment_name[:64],
            ttl_seconds=self._ttl_seconds,
            # Never idle-hibernate mid-trial: an agent thinking for two minutes
            # between tool calls looks exactly like an idle sandbox. The legacy
            # API spells that -1, but /v1 rejects negative lifecycle durations,
            # so the portable way to say it is a window outliving the trial.
            idle_timeout_seconds=self._ttl_seconds + 3600,
            vcpus=vcpus,
            memory_mib=memory_mib,
            metadata={
                "harbor_session": self.session_id[:128],
                "harbor_task": self.environment_name[:128],
                **({"harbor_trial": str(self.context_id)} if self.context_id else {}),
            },
        )

    async def _bootstrap_dirs(self) -> None:
        """Create the session tree before anything else runs in the guest.

        Every other guest command goes through `host_exec`, which sets the
        compose directory as its cwd — including the one in `_stage_and_up`
        that would have created it. So the very first command has to be
        cwd-less, or the adapter can never bootstrap: sandboxd passes the
        requested cwd straight to `cmd.Dir`, and Go reports a failed chdir as
        `fork/exec /bin/bash: no such file or directory`, which reads as a guest
        with no shell rather than a directory that isn't there yet.
        """
        client, sandbox_id = self._require()
        result = await client.exec(
            sandbox_id,
            f"mkdir -p {shlex.quote(self._compose_dir)} "
            f"{shlex.quote(self._environment_dir_guest)}",
            timeout_sec=60,
        )
        result.check("bootstrap session directories")

    async def _ensure_docker_ready(self) -> None:
        """Wait for dockerd, which a snapshot-restored guest normally resumes
        with already running.

        A clone resumes on a fresh network identity (new guest IP, re-added
        eth0), so if the daemon did come back unhappy the fix is a restart, not
        more waiting — try that once mid-wait rather than burning the whole
        timeout on a daemon that will never recover on its own.
        """
        deadline = asyncio.get_running_loop().time() + DOCKER_READY_TIMEOUT_SEC
        kicked = False
        last = ""
        while asyncio.get_running_loop().time() < deadline:
            probe = await self.host_exec("docker info >/dev/null 2>&1", timeout_sec=30)
            if probe.return_code == 0:
                return
            last = f"{probe.stdout} {probe.stderr}"
            if not kicked:
                kicked = True
                await self.host_exec(
                    "sudo systemctl restart docker 2>&1 || sudo systemctl start docker 2>&1",
                    timeout_sec=60,
                )
                continue
            await asyncio.sleep(2)
        source = (
            f"snapshot {self._snapshot_id}"
            if self._snapshot_id
            else "the plain template (SANDBOX_SNAPSHOT_ID unset), which has no Docker"
        )
        raise RuntimeError(
            f"Docker never became ready in the guest. Trials are cloned from {source}; "
            f"prepare a snapshot with Docker installed first. Last probe: {last}"
        )

    async def _stage_and_up(self, force_build: bool) -> None:
        prepare = await self.host_exec(
            f"rm -rf {shlex.quote(self._session_dir)} && "
            f"mkdir -p {shlex.quote(self._compose_dir)} "
            f"{shlex.quote(self._environment_dir_guest)}",
            timeout_sec=60,
        )
        if prepare.return_code != 0:
            raise RuntimeError(
                f"could not prepare {self._session_dir}: {prepare.stdout} {prepare.stderr}"
            )

        for overlay in (COMPOSE_BUILD_PATH, COMPOSE_PREBUILT_PATH):
            await self.upload_file_to_host(overlay, f"{self._compose_dir}/{overlay.name}")
        await self.upload_dir_to_host(self.environment_dir, self._environment_dir_guest)
        await self._compose_ops._stage_env_compose_file(self._compose_dir)

        self._use_prebuilt = should_use_prebuilt_docker_image(
            self.environment_dir,
            docker_image=self.task_env_config.docker_image,
            force_build=force_build,
        )

        # Get the task's image into the guest, then snapshot the guest so later
        # trials skip that step entirely. Which step it is depends on the task:
        # Terminal-Bench 2.0 tasks mostly ship a published image
        # (`docker_image` in task.toml, e.g. ghcr.io/laude-institute/...), so the
        # per-trial cost is a PULL; tasks with only a Dockerfile pay a build.
        # Both are worth caching and both must happen before `up`, so that the
        # snapshot captures the image with no containers running — a clone then
        # starts its own rather than resuming this trial's.
        if self._image_prebaked:
            # Cloned from this task's snapshot: the image is already here, and
            # `up` finds it by tag — which is why _project_name is derived from
            # the task rather than the trial.
            pass
        elif self._use_prebuilt:
            pull = await self._compose_ops._compose_exec(
                ["pull", "--quiet"], timeout_sec=round(self.task_env_config.build_timeout_sec)
            )
            if pull.return_code != 0:
                # Not fatal: `up` pulls implicitly too. Skip the snapshot so we
                # never cache a guest whose image is missing or half-pulled.
                self.logger.warning(
                    "docker compose pull failed (up will retry): %s %s",
                    pull.stdout, pull.stderr,
                )
            else:
                await self._cache_task_snapshot()
        else:
            build = await self._compose_ops._compose_exec(
                ["build"], timeout_sec=round(self.task_env_config.build_timeout_sec)
            )
            if build.return_code != 0:
                raise RuntimeError(
                    f"docker compose build failed in sandbox {self._sandbox_id}: "
                    f"{build.stdout} {build.stderr}"
                )
            await self._cache_task_snapshot()

        up = await self._compose_ops._compose_exec(
            ["up", "-d"], timeout_sec=COMPOSE_UP_TIMEOUT_SEC
        )
        if up.return_code != 0:
            raise RuntimeError(
                f"docker compose up failed in sandbox {self._sandbox_id}: "
                f"{up.stdout} {up.stderr}"
            )
        await self._wait_for_main()
        await self._ensure_harbor_dirs()
        # Prebuilt-image tasks have no build context, so their environment/ dir
        # has to land in the container after it is up. The base method no-ops
        # for build-from-Dockerfile tasks.
        await self._upload_environment_dir_after_start()

    async def _cache_task_snapshot(self) -> None:
        """Snapshot this guest so later trials of the task skip the image build."""
        if self._cache_disabled or not self._snapshot_id:
            return
        client, sandbox_id = self._require()
        try:
            snapshot = await client.snapshot(
                sandbox_id, name=f"harbor-task-{self._project_name}"[:64]
            )
        except SandboxError as exc:
            self.logger.warning("could not snapshot the built task image: %s", exc)
            return
        snapshot_id = snapshot.get("id")
        if not snapshot_id:
            return
        self._record_task_snapshot(snapshot_id)
        self.logger.info(
            "cached task %s as snapshot %s; later trials clone it instead of building",
            self.environment_name, snapshot_id,
        )

    async def _ensure_harbor_dirs(self) -> None:
        """Create Harbor's fixed directory tree inside the main service.

        Harbor's own Docker backend gets /logs/{agent,verifier,artifacts} from
        bind mounts it controls; a provider that runs the task's compose project
        unmodified gets no such thing, so every provider backend creates them
        itself after `up` (see blaxel.py, tensorlake.py). Skipping this does not
        fail at `up` — it fails much later and misleadingly, when the agent
        redirects its stdout into /logs/agent and the verifier's reward.txt has
        nowhere to land, surfacing as DownloadVerifierDirError.
        """
        paths = EnvironmentPaths.for_os(self.os)
        dirs = " ".join(
            shlex.quote(str(path))
            for path in (
                paths.agent_dir,
                paths.verifier_dir,
                paths.artifacts_dir,
                paths.tests_dir,
                paths.solution_dir,
            )
        )
        # 777 because the task's own image decides what user the agent and the
        # verifier run as, and they both have to write here.
        result = await self._compose_ops.exec(
            f"mkdir -p {dirs} && chmod 777 {shlex.quote(str(paths.logs_dir))} "
            f"{shlex.quote(str(paths.logs_dir))}/*",
            timeout_sec=60,
        )
        if result.return_code != 0:
            raise RuntimeError(
                f"could not create Harbor directories in the main service: "
                f"{result.stdout} {result.stderr}"
            )

    async def _wait_for_main(self, timeout_sec: int = 120) -> None:
        deadline = asyncio.get_running_loop().time() + timeout_sec
        while asyncio.get_running_loop().time() < deadline:
            probe = await self._compose_ops.exec("true", timeout_sec=20)
            if probe.return_code == 0:
                return
            await asyncio.sleep(2)
        raise RuntimeError("the main compose service never became executable")

    @override
    async def stop(self, delete: bool) -> None:
        """Deleting the sandbox destroys the whole compose project with it, so
        `docker compose down` is only needed when the VM is being kept for
        debugging."""
        if self._client is None or self._sandbox_id is None:
            return
        try:
            if not delete:
                await self._compose_ops._compose_exec(["down", "-v"], timeout_sec=180)
                return
            await self._client.terminate(self._sandbox_id)
            self.logger.info("sandbox %s destroyed", self._sandbox_id)
        finally:
            if delete:
                self._sandbox_id = None
            await self._client.aclose()
            self._client = None

    # ── BaseEnvironment surface (delegated to the compose ops) ───────────

    @override
    async def exec(
        self,
        command: str,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        timeout_sec: int | None = None,
        user: str | int | None = None,
    ) -> ExecResult:
        return await self._compose_ops.exec(
            command,
            cwd=cwd or self.task_env_config.workdir,
            env=self._merge_env(env),
            timeout_sec=timeout_sec,
            user=user if user is not None else self.default_user,
        )

    def _merge_env(self, env: dict[str, str] | None) -> dict[str, str]:
        merged = dict(self._persistent_env)
        if env:
            merged.update(env)
        return merged

    @override
    async def upload_file(self, source_path: Path | str, target_path: str) -> None:
        await self._compose_ops.upload_file(source_path, target_path)

    @override
    async def upload_dir(self, source_dir: Path | str, target_dir: str) -> None:
        await self._compose_ops.upload_dir(source_dir, target_dir)

    @override
    async def download_file(self, source_path: str, target_path: Path | str) -> None:
        await self._compose_ops.download_file(source_path, target_path)

    @override
    async def download_dir(self, source_dir: str, target_dir: Path | str) -> None:
        await self._compose_ops.download_dir(source_dir, target_dir)

    @override
    def _compose_service_transport(self, service: str | None) -> ComposeServiceTransport:
        return self._compose_ops
