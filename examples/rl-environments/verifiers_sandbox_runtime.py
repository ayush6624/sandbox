"""A `verifiers` v1 execution runtime backed by Firecracker microVM sandboxes.

Prime Intellect's `verifiers` is the environment/eval library behind the
Environments Hub, `prime-rl`, and Zapier's AutomationBench. Its v1 harnesses
execute rollout programs through a pluggable `Runtime`
(`verifiers/v1/runtimes/base.py`); the shipped ones are `subprocess`, `docker`,
`modal`, and `prime`. This adds a fifth backed by our fleet.

**Registration needs a two-line patch, unlike Harbor.** `verifiers`'
`RuntimeConfig` is a closed discriminated union, so there is no import-path
escape hatch. In `verifiers/v1/runtimes/__init__.py`:

    from examples.rl_environments.verifiers_sandbox_runtime import (
        SandboxConfig, SandboxRuntime, SandboxRuntimeInfo,
    )

    RuntimeConfig = Annotated[
        SubprocessConfig | DockerConfig | PrimeConfig | ModalConfig | SandboxConfig,
        Field(discriminator="type"),
    ]
    RuntimeInfo = Annotated[
        SubprocessRuntimeInfo | DockerRuntimeInfo | PrimeRuntimeInfo
        | ModalRuntimeInfo | SandboxRuntimeInfo,
        Field(discriminator="type"),
    ]

    def _runtime_cls(config):
        if isinstance(config, SandboxConfig):
            return SandboxRuntime
        ...

Then select it in an environment/eval config with `{"type": "sandbox"}`.

Why this is worth wiring up for RL specifically: a rollout alternates between
*the policy thinking* (seconds to minutes, no guest work) and *the environment
executing* (milliseconds). `pause()`/`resume()` bill nothing for the frozen
span, and the usage ledger meters per VM lifetime — so a group of 16 rollouts
that spends most of its wall-clock waiting on the sampler costs ~nothing in
sandbox time. `snapshot()` + create-from-snapshot gives per-task state reset
without re-running setup: prepare a task's world once, then clone it per
rollout (~764 ms/sandbox measured flat from N=4 up).
"""

from __future__ import annotations

import asyncio
import base64
import contextlib
import logging
import os
import shlex
from pathlib import PurePosixPath
from typing import ClassVar, Literal

from pydantic import Field

from verifiers.v1.errors import SandboxError as VerifiersSandboxError
from verifiers.v1.runtimes.base import (
    SERVICE_PORT,
    BaseRuntimeInfo,
    NetworkPolicyConfig,
    ProgramResult,
    Runtime,
)

from .sandbox_client import SandboxClient, SandboxError, acquire_with_backoff

logger = logging.getLogger(__name__)


class SandboxConfig(NetworkPolicyConfig):
    type: Literal["sandbox"] = "sandbox"

    api_url: str | None = None
    """Gateway address. Defaults to $SANDBOX_API_URL."""
    api_key: str | None = None
    """Bearer token. Defaults to $SANDBOX_API_KEY."""

    workdir: str = "/home/sandbox/app"
    """Working directory for rollout programs. Exists in the base image."""

    snapshot_id: str | None = None
    """Create from this snapshot instead of the default template. This is the
    per-task-state lever: bake a prepared world once, clone it per rollout."""

    vcpus: int | None = None
    memory_mib: int | None = None
    """Omit both to be served from the ready pool (~12 ms p50). Setting either
    forces a cold boot — Firecracker bakes vcpus/mem into snapshots."""

    ttl_seconds: int = 3 * 60 * 60
    """Backstop against leaked VMs if a trainer dies mid-rollout; teardown is
    still the normal path."""

    idle_timeout_seconds: int = -1
    """-1 = never auto-pause. A policy thinking between tool calls is
    indistinguishable from an idle sandbox, and an unexpected freeze mid-rollout
    would show up as one slow step. Pause explicitly instead (`pause_between`)."""

    pause_between: bool = False
    """Freeze the VM while the policy is sampling and thaw it on the next
    program. Trades ~80 ms of wake latency for zero billed time in between —
    worth it when a rollout step's think time dwarfs its exec time."""

    create_timeout_sec: float = 300.0
    exec_timeout_sec: int = 900


class SandboxRuntimeInfo(SandboxConfig, BaseRuntimeInfo):
    sandbox_id: str | None = None
    from_snapshot: bool = False


class SandboxRuntime(Runtime):
    """One microVM per rollout, addressed over the authenticated HTTP API."""

    is_local: ClassVar[bool] = False
    """Remote: programs inside cannot reach a host-bound URL directly, so the
    harness uses its host-side Tunnel inward and `expose()` outward."""

    def __init__(self, config: SandboxConfig, name: str | None = None) -> None:
        super().__init__(name)
        self.config = config
        self.info = SandboxRuntimeInfo(**config.model_dump())
        self._client: SandboxClient | None = None
        self._paused = False

    # ── lifecycle ────────────────────────────────────────────────────────

    async def start(self) -> None:
        if self.config.network_restricted:
            # Fail loudly: silently ignoring an egress policy would run
            # untrusted rollout code with full internet access.
            raise VerifiersSandboxError(
                "the sandbox runtime does not implement per-sandbox egress "
                "policy yet; use allow=['*'] or pick a runtime that does"
            )
        self._client = SandboxClient(
            self.config.api_url or os.environ.get("SANDBOX_API_URL"),
            self.config.api_key or os.environ.get("SANDBOX_API_KEY"),
        )
        try:
            sandbox = await acquire_with_backoff(
                self._client,
                name=self.name[:64],
                snapshot_id=self.config.snapshot_id,
                ttl_seconds=self.config.ttl_seconds,
                idle_timeout_seconds=self.config.idle_timeout_seconds,
                vcpus=self.config.vcpus,
                memory_mib=self.config.memory_mib,
                metadata={"verifiers_runtime": self.name[:128]},
            )
        except SandboxError as exc:
            await self._client.aclose()
            self._client = None
            raise VerifiersSandboxError(f"could not start a sandbox: {exc}") from exc

        self.info.id = sandbox.id
        self.info.sandbox_id = sandbox.id
        self.info.from_snapshot = self.config.snapshot_id is not None
        logger.info("sandbox: %s up for %s", sandbox.id, self.name)

        await self._exec(f"mkdir -p {shlex.quote(self.config.workdir)}", timeout_sec=60)

    async def teardown(self) -> None:
        """Async because freeing the VM is an API call. `stop()` shields this
        from cancellation, so a Ctrl-C cannot leak a running VM."""
        client, sandbox_id = self._client, self.info.sandbox_id
        if client is None or sandbox_id is None:
            return
        try:
            await client.terminate(sandbox_id)
            logger.info("sandbox: %s destroyed", sandbox_id)
        except SandboxError as exc:  # best-effort: the TTL is the backstop
            logger.warning("sandbox: could not destroy %s: %s", sandbox_id, exc)
        finally:
            await client.aclose()
            self._client = None

    def _require(self) -> tuple[SandboxClient, str]:
        if self._client is None or self.info.sandbox_id is None:
            raise VerifiersSandboxError("runtime used before start()")
        return self._client, self.info.sandbox_id

    # ── execution ────────────────────────────────────────────────────────

    async def _exec(
        self, command: str, *, env: dict[str, str] | None = None, timeout_sec: int
    ) -> ProgramResult:
        client, sandbox_id = self._require()
        await self._thaw()
        try:
            result = await client.exec(
                sandbox_id,
                command,
                cwd=self.config.workdir,
                env=env,
                timeout_sec=timeout_sec,
            )
        except SandboxError as exc:
            raise VerifiersSandboxError(f"exec failed: {exc}") from exc
        if result.timed_out:
            raise VerifiersSandboxError(
                f"command exceeded {timeout_sec}s: {command[:200]}"
            )
        return ProgramResult(
            exit_code=result.exit_code, stdout=result.stdout, stderr=result.stderr
        )

    async def run(self, argv: list[str], env: dict[str, str]) -> ProgramResult:
        """Short, idempotent infra operations (writes, installs, probes)."""
        return await self._exec(
            shlex.join(argv), env=env, timeout_sec=self.config.exec_timeout_sec
        )

    async def run_program(
        self, argv: list[str], env: dict[str, str]
    ) -> ProgramResult:
        """The rollout itself. Same transport, but it must never be replayed —
        so this override exists to make that explicit, and it pauses the VM
        afterwards when configured, since the policy is about to think."""
        result = await self._exec(
            shlex.join(argv), env=env, timeout_sec=self.config.exec_timeout_sec
        )
        if self.config.pause_between:
            await self._freeze()
        return result

    async def run_background(
        self, argv: list[str], env: dict[str, str], log: str
    ) -> None:
        """Start a colocated tool server. `setsid` + a detached redirect keeps
        it alive past this request — sandboxd kills a command's whole process
        group when the exec that started it returns."""
        command = (
            f"setsid nohup {shlex.join(argv)} "
            f"> {shlex.quote(log)} 2>&1 < /dev/null & echo $!"
        )
        result = await self._exec(command, env=env, timeout_sec=60)
        if result.exit_code != 0:
            raise VerifiersSandboxError(
                f"could not start background program: {result.stderr[:500]}"
            )

    # ── files ────────────────────────────────────────────────────────────

    async def _read(self, path: str) -> bytes:
        client, sandbox_id = self._require()
        await self._thaw()
        try:
            return await client.read_file(sandbox_id, self._abs(path))
        except SandboxError as exc:
            raise VerifiersSandboxError(f"read {path}: {exc}") from exc

    async def write(self, path: str, data: bytes) -> None:
        client, sandbox_id = self._require()
        await self._thaw()
        target = self._abs(path)
        try:
            # Ensure the parent exists first: the file API creates parent
            # directories, but a caller-supplied nested path is cheap to guard.
            parent = str(PurePosixPath(target).parent)
            await client.exec(
                sandbox_id, f"mkdir -p {shlex.quote(parent)}", timeout_sec=30
            )
            await client.write_file(sandbox_id, target, data)
        except SandboxError as exc:
            raise VerifiersSandboxError(f"write {path}: {exc}") from exc

    def _abs(self, path: str) -> str:
        return path if path.startswith("/") else f"{self.config.workdir}/{path}"

    # ── networking ───────────────────────────────────────────────────────

    @property
    def published_port(self) -> int | None:
        return SERVICE_PORT

    async def expose(self, port: int) -> str | None:
        """Publish a guest port so a host-side harness can reach a tool server
        running in the sandbox."""
        client, sandbox_id = self._require()
        await self._thaw()
        try:
            forward = await client.create_port_forward(sandbox_id, port)
        except SandboxError as exc:
            raise VerifiersSandboxError(f"could not expose port {port}: {exc}") from exc
        url = forward.get("url")
        if url:
            return url
        host_port = forward.get("host_port")
        if host_port and (address := forward.get("address")):
            return f"http://{address}"
        if host_port:
            # Worker-local only: reachable from the worker, not from an
            # arbitrary host. Surface it rather than pretending it is a URL.
            logger.warning(
                "sandbox: port %s mapped to worker-local port %s with no public "
                "URL — an off-worker harness cannot reach it",
                port,
                host_port,
            )
            return None
        return None

    # ── pause / resume ───────────────────────────────────────────────────

    async def _freeze(self) -> None:
        client, sandbox_id = self._require()
        if self._paused:
            return
        with contextlib.suppress(SandboxError):
            await client.pause(sandbox_id)
            self._paused = True

    async def _thaw(self) -> None:
        """Explicit resume before use. Any agent-bound request would wake the
        sandbox transparently anyway; doing it here keeps the wake latency out
        of the middle of a timed exec."""
        if not self._paused:
            return
        client, sandbox_id = self._require()
        try:
            await client.resume(sandbox_id)
        except SandboxError as exc:
            raise VerifiersSandboxError(f"could not resume {sandbox_id}: {exc}") from exc
        self._paused = False


async def prepare_task_snapshot(
    client: SandboxClient, setup_script: str, *, name: str = "task-prepared"
) -> str:
    """Run setup once, snapshot the result, return the snapshot id.

    The RL pattern this enables: pay a task's setup cost (clone a repo, install
    deps, seed a database) one time, then start every rollout in that group from
    the snapshot. Restores are identity-neutral clones, so N rollouts get N
    independent worlds with the same prepared state.
    """
    sandbox = await client.create(ttl_seconds=3600, idle_timeout_seconds=-1)
    try:
        encoded = base64.b64encode(setup_script.encode()).decode()
        result = await client.exec(
            sandbox.id,
            f"echo {encoded} | base64 -d > /tmp/setup.sh && bash /tmp/setup.sh",
            timeout_sec=1800,
        )
        result.check("task setup")
        snapshot = await client.snapshot(sandbox.id, name=name)
        return snapshot["id"]
    finally:
        await client.terminate(sandbox.id)


async def fanout_rollouts(
    client: SandboxClient, snapshot_id: str, count: int, *, max_parallelism: int = 8
) -> list[str]:
    """Clone `count` rollout sandboxes from one prepared snapshot.

    Batch create is bounded server-side (`max_parallelism`, and the gateway's
    own create queue), so this is back-pressure-aware in a way that
    `asyncio.gather` over N creates is not.
    """
    sandboxes = await client.create_many(
        count,
        max_parallelism=max_parallelism,
        snapshot_id=snapshot_id,
        idle_timeout_seconds=-1,
        ttl_seconds=3600,
    )
    return [sandbox.id for sandbox in sandboxes]


if __name__ == "__main__":  # smoke test against a live fleet

    async def _main() -> None:
        config = SandboxConfig()
        runtime = SandboxRuntime(config, name="smoke")
        await runtime.start()
        try:
            print((await runtime.run(["python3", "--version"], {})).stdout.strip())
            await runtime.write("hello.txt", b"from the harness\n")
            print((await runtime._read("hello.txt")).decode().strip())
        finally:
            await runtime.stop()

    asyncio.run(_main())
