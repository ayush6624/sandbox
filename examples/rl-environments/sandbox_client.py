"""Minimal async Python client for the sandbox v1 API.

The repo ships a TypeScript SDK; the RL/eval ecosystem (Harbor, verifiers,
prime-rl) is Python, so this is the thin bridge. It is deliberately small —
lifecycle over `/v1`, exec/files over the unversioned agent-proxy routes — and
depends only on `httpx`, which both Harbor and verifiers already pull in.

    export SANDBOX_API_URL=http://10.160.0.100:9090
    export SANDBOX_API_KEY=...

    async with SandboxClient() as client:
        sb = await client.create(ttl_seconds=1800)
        print((await sb.exec("uname -a")).stdout)
        await sb.terminate()

Field names mirror `api/openapi.yaml` exactly (`resources.vcpu`,
`lifecycle.ttl_seconds`, `source.type`), so drift shows up as a 400 rather
than as silently ignored options.
"""

from __future__ import annotations

import asyncio
import json
import os
import uuid
from dataclasses import dataclass, field
from typing import Any, AsyncIterator, Literal

import httpx

DEFAULT_TIMEOUT = 30.0
CREATE_TIMEOUT = 300.0
"""Creates block until the guest agent answers /health, and a cold path plus a
queue wait can take minutes on a saturated fleet — the SDK uses 300 s too."""


class SandboxError(RuntimeError):
    def __init__(self, message: str, *, status: int | None = None, body: str = "") -> None:
        super().__init__(message)
        self.status = status
        self.body = body

    @property
    def is_capacity(self) -> bool:
        """503/429 mean 'no room right now', not 'broken' — callers back off."""
        return self.status in (429, 503)


class NotFoundError(SandboxError):
    """A 404 from the gateway is a PROVEN absence; 503 is indeterminate."""


@dataclass
class ExecResult:
    stdout: str
    stderr: str
    exit_code: int
    timed_out: bool = False
    duration_ms: int = 0

    def check(self, cmd: str = "") -> "ExecResult":
        if self.timed_out:
            raise SandboxError(f"command timed out: {cmd}\n{self.stderr[-2000:]}")
        if self.exit_code != 0:
            raise SandboxError(
                f"command exited {self.exit_code}: {cmd}\n{self.stderr[-2000:]}"
            )
        return self


@dataclass
class Sandbox:
    id: str
    status: str
    raw: dict[str, Any] = field(default_factory=dict)
    _client: "SandboxClient" = field(repr=False, default=None)  # type: ignore[assignment]

    @property
    def vcpus(self) -> int:
        return int(self.raw.get("resources", {}).get("vcpu", 0))

    @property
    def memory_mib(self) -> int:
        return int(self.raw.get("resources", {}).get("memory_mib", 0))

    async def exec(
        self,
        cmd: str,
        *,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        timeout_sec: int = 60,
    ) -> ExecResult:
        return await self._client.exec(
            self.id, cmd, cwd=cwd, env=env, timeout_sec=timeout_sec
        )

    async def read_file(self, path: str) -> bytes:
        return await self._client.read_file(self.id, path)

    async def write_file(self, path: str, data: bytes | str) -> None:
        await self._client.write_file(self.id, path, data)

    async def snapshot(self, name: str | None = None) -> dict[str, Any]:
        return await self._client.snapshot(self.id, name=name)

    async def pause(self) -> None:
        await self._client.pause(self.id)

    async def resume(self) -> None:
        await self._client.resume(self.id)

    async def terminate(self) -> None:
        await self._client.terminate(self.id)


class SandboxClient:
    def __init__(
        self,
        base_url: str | None = None,
        api_key: str | None = None,
        *,
        timeout: float = DEFAULT_TIMEOUT,
    ) -> None:
        url = base_url or os.environ.get("SANDBOX_API_URL")
        if not url:
            raise SandboxError("set SANDBOX_API_URL or pass base_url")
        self._base_url = url.rstrip("/")
        self._api_key = api_key or os.environ.get("SANDBOX_API_KEY", "")
        headers = {"content-type": "application/json"}
        if self._api_key:
            headers["authorization"] = f"Bearer {self._api_key}"
        # A generous pool: a fan-out of 32 sandboxes drives 32 concurrent execs,
        # and httpx's default 10 connections would serialize them.
        self._http = httpx.AsyncClient(
            base_url=self._base_url,
            headers=headers,
            timeout=timeout,
            limits=httpx.Limits(max_connections=64, max_keepalive_connections=32),
        )

    async def __aenter__(self) -> "SandboxClient":
        return self

    async def __aexit__(self, *_exc: object) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        await self._http.aclose()

    # ── transport ────────────────────────────────────────────────────────

    async def _request(
        self,
        method: str,
        path: str,
        *,
        json_body: Any = None,
        content: bytes | None = None,
        params: dict[str, str] | None = None,
        timeout: float | None = None,
        idempotent: bool = False,
    ) -> httpx.Response:
        headers: dict[str, str] = {}
        if idempotent:
            # Mutations carry an idempotency key so a retried create can't
            # leave a second orphaned VM behind.
            headers["idempotency-key"] = str(uuid.uuid4())
        if content is not None:
            headers["content-type"] = "application/octet-stream"
        try:
            response = await self._http.request(
                method,
                path,
                json=json_body,
                content=content,
                params=params,
                headers=headers,
                timeout=timeout,
            )
        except httpx.HTTPError as exc:  # transport failure, no status
            raise SandboxError(f"{method} {path}: {exc}") from exc
        if response.status_code >= 400:
            body = response.text[:2000]
            detail = body
            try:  # RFC 7807 problem details
                detail = json.loads(body).get("detail", body)
            except (ValueError, AttributeError):
                pass
            cls = NotFoundError if response.status_code == 404 else SandboxError
            raise cls(
                f"{method} {path} -> {response.status_code}: {detail}",
                status=response.status_code,
                body=body,
            )
        return response

    # ── lifecycle (/v1) ──────────────────────────────────────────────────

    async def create(
        self,
        *,
        snapshot_id: str | None = None,
        template_id: str | None = None,
        name: str | None = None,
        ttl_seconds: int | None = None,
        idle_timeout_seconds: int | None = None,
        vcpus: int | None = None,
        memory_mib: int | None = None,
        metadata: dict[str, str] | None = None,
    ) -> Sandbox:
        """Create one sandbox.

        Omit `vcpus`/`memory_mib` to get the golden-snapshot hot path (~12 ms
        p50 on the production fleet); setting either forces a cold boot,
        because Firecracker bakes vcpus/mem into a snapshot.
        """
        body = self._create_body(
            snapshot_id=snapshot_id,
            template_id=template_id,
            name=name,
            ttl_seconds=ttl_seconds,
            idle_timeout_seconds=idle_timeout_seconds,
            vcpus=vcpus,
            memory_mib=memory_mib,
            metadata=metadata,
        )
        response = await self._request(
            "POST", "/v1/sandboxes", json_body=body,
            timeout=CREATE_TIMEOUT, idempotent=True,
        )
        return self._sandbox(response.json())

    async def create_many(
        self, count: int, *, max_parallelism: int | None = None, **create_kwargs: Any
    ) -> list[Sandbox]:
        """Batch-create via `/v1/sandbox-batches` and wait for the operation.

        Every requested index comes back as either a sandbox or a structured
        error — partial success is never a mysteriously short list. Failures are
        logged and dropped here; callers that need per-index attribution should
        read the operation themselves.
        """
        body = {
            "count": count,
            "sandbox": self._create_body(**create_kwargs),
        }
        if max_parallelism is not None:
            body["max_parallelism"] = max_parallelism
        response = await self._request(
            "POST", "/v1/sandbox-batches", json_body=body,
            timeout=CREATE_TIMEOUT, idempotent=True,
        )
        operation = response.json()
        operation = await self._wait_operation(operation["id"])
        out: list[Sandbox] = []
        for item in operation.get("results", []):
            value = item.get("value")
            if value:
                out.append(self._sandbox(value))
        return out

    async def _wait_operation(
        self, operation_id: str, *, poll_interval: float = 1.0, timeout: float = CREATE_TIMEOUT
    ) -> dict[str, Any]:
        deadline = asyncio.get_running_loop().time() + timeout
        terminal = {"succeeded", "partially_succeeded", "failed"}
        while True:
            response = await self._request("GET", f"/v1/operations/{operation_id}")
            operation = response.json()
            if operation.get("status") in terminal:
                return operation
            if asyncio.get_running_loop().time() > deadline:
                raise SandboxError(f"operation {operation_id} did not settle in {timeout}s")
            await asyncio.sleep(poll_interval)

    def _create_body(
        self,
        *,
        snapshot_id: str | None = None,
        template_id: str | None = None,
        name: str | None = None,
        ttl_seconds: int | None = None,
        idle_timeout_seconds: int | None = None,
        vcpus: int | None = None,
        memory_mib: int | None = None,
        metadata: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        body: dict[str, Any] = {}
        if name is not None:
            body["name"] = name
        if snapshot_id is not None:
            body["source"] = {"type": "snapshot", "id": snapshot_id}
        elif template_id is not None:
            body["source"] = {"type": "template", "id": template_id}
        lifecycle: dict[str, int] = {}
        if ttl_seconds is not None:
            lifecycle["ttl_seconds"] = ttl_seconds
        if idle_timeout_seconds is not None:
            lifecycle["idle_timeout_seconds"] = idle_timeout_seconds
        if lifecycle:
            body["lifecycle"] = lifecycle
        resources: dict[str, int] = {}
        if vcpus is not None:
            resources["vcpu"] = vcpus
        if memory_mib is not None:
            resources["memory_mib"] = memory_mib
        if resources:
            body["resources"] = resources
        if metadata:
            body["metadata"] = metadata
        return body

    def _sandbox(self, raw: dict[str, Any]) -> Sandbox:
        return Sandbox(id=raw["id"], status=raw.get("status", "running"), raw=raw, _client=self)

    async def get(self, sandbox_id: str) -> Sandbox:
        response = await self._request("GET", f"/v1/sandboxes/{sandbox_id}")
        return self._sandbox(response.json())

    async def pause(self, sandbox_id: str) -> None:
        await self._request(
            "POST", f"/v1/sandboxes/{sandbox_id}:pause", idempotent=True, timeout=120.0
        )

    async def resume(self, sandbox_id: str) -> None:
        await self._request(
            "POST", f"/v1/sandboxes/{sandbox_id}:resume", idempotent=True, timeout=120.0
        )

    async def terminate(self, sandbox_id: str, *, ignore_missing: bool = True) -> None:
        try:
            await self._request(
                "DELETE", f"/v1/sandboxes/{sandbox_id}", idempotent=True, timeout=120.0
            )
        except NotFoundError:
            if not ignore_missing:
                raise

    async def snapshot(self, sandbox_id: str, *, name: str | None = None) -> dict[str, Any]:
        body = {"name": name} if name else {}
        response = await self._request(
            "POST", f"/v1/sandboxes/{sandbox_id}/snapshots",
            json_body=body, timeout=CREATE_TIMEOUT, idempotent=True,
        )
        return response.json()

    async def create_port_forward(
        self, sandbox_id: str, guest_port: int, *, mode: str | None = None
    ) -> dict[str, Any]:
        """Expose a guest port. The response may carry `url` (public HTTP),
        `host_port` (worker-local), or `address` (raw TCP) — address types
        accumulate on one mapping, so read the field you intend to use rather
        than assuming only one is present."""
        body: dict[str, Any] = {"guest_port": guest_port}
        if mode:
            body["mode"] = mode
        response = await self._request(
            "POST", f"/v1/sandboxes/{sandbox_id}/port-forwards",
            json_body=body, idempotent=True, timeout=60.0,
        )
        return response.json()

    async def usage(self, sandbox_id: str) -> dict[str, Any]:
        """Billable usage for one sandbox — vcpu_seconds / memory_mib_seconds are
        the billed quantities; cpu_seconds is recorded but never billed."""
        response = await self._request("GET", f"/v1/sandboxes/{sandbox_id}/usage")
        return response.json()

    # ── in-guest operations (agent proxy) ────────────────────────────────
    #
    # These are the unversioned routes the TS SDK also uses: the host server
    # proxies them to sandboxd on :8090 inside the guest.

    async def exec(
        self,
        sandbox_id: str,
        cmd: str,
        *,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        timeout_sec: int = 60,
    ) -> ExecResult:
        body: dict[str, Any] = {"cmd": cmd, "timeout_sec": timeout_sec}
        if cwd:
            body["cwd"] = cwd
        if env:
            body["env"] = env
        # Give the HTTP call headroom over the guest-side timeout so a command
        # that runs to its limit reports timed_out instead of a transport error.
        response = await self._request(
            "POST", f"/sandboxes/{sandbox_id}/exec",
            json_body=body, timeout=timeout_sec + 30,
        )
        raw = response.json()
        return ExecResult(
            stdout=raw.get("stdout", ""),
            stderr=raw.get("stderr", ""),
            exit_code=raw.get("exit_code", 0),
            timed_out=raw.get("timed_out", False),
            duration_ms=raw.get("duration_ms", 0),
        )

    async def exec_stream(
        self,
        sandbox_id: str,
        cmd: str,
        *,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        timeout_sec: int = 600,
    ) -> AsyncIterator[dict[str, Any]]:
        """Stream NDJSON `ExecEvent` lines. Long agent runs use this so the
        2 MiB per-stream output cap on buffered exec can't truncate a trace.

        Absent fields are zero values — never assume a key is present.
        """
        body: dict[str, Any] = {"cmd": cmd, "timeout_sec": timeout_sec}
        if cwd:
            body["cwd"] = cwd
        if env:
            body["env"] = env
        async with self._http.stream(
            "POST",
            f"/sandboxes/{sandbox_id}/exec/stream",
            json=body,
            timeout=timeout_sec + 30,
        ) as response:
            if response.status_code >= 400:
                await response.aread()
                raise SandboxError(
                    f"exec/stream -> {response.status_code}",
                    status=response.status_code,
                    body=response.text[:2000],
                )
            async for line in response.aiter_lines():
                if line.strip():
                    yield json.loads(line)

    async def read_file(self, sandbox_id: str, path: str) -> bytes:
        response = await self._request(
            "GET", f"/sandboxes/{sandbox_id}/files", params={"path": path}
        )
        return response.content

    async def write_file(self, sandbox_id: str, path: str, data: bytes | str) -> None:
        payload = data.encode() if isinstance(data, str) else data
        await self._request(
            "PUT", f"/sandboxes/{sandbox_id}/files",
            params={"path": path}, content=payload, timeout=120.0,
        )

    async def list_dir(self, sandbox_id: str, path: str) -> list[dict[str, Any]]:
        response = await self._request(
            "GET", f"/sandboxes/{sandbox_id}/dir", params={"path": path}
        )
        return response.json()

    # ── tar-based directory transfer ─────────────────────────────────────
    #
    # The file API is single-file, so directories move as a base64 tarball
    # through exec. Fine for task definitions and result bundles (a few MiB);
    # for anything large, expose a port and stream instead.

    async def upload_dir(self, sandbox_id: str, local_dir: str, remote_dir: str) -> None:
        import base64
        import io
        import tarfile

        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w:gz") as tar:
            tar.add(local_dir, arcname=".")
        encoded = base64.b64encode(buffer.getvalue()).decode()
        staged = f"/tmp/upload-{uuid.uuid4().hex}.tar.gz.b64"
        await self.write_file(sandbox_id, staged, encoded)
        result = await self.exec(
            sandbox_id,
            f"mkdir -p {remote_dir!r} && base64 -d {staged!r} | tar -xzf - -C {remote_dir!r} "
            f"&& rm -f {staged!r}",
            timeout_sec=300,
        )
        result.check("upload_dir")

    async def download_dir(self, sandbox_id: str, remote_dir: str, local_dir: str) -> None:
        import base64
        import io
        import tarfile

        staged = f"/tmp/download-{uuid.uuid4().hex}.tar.gz.b64"
        result = await self.exec(
            sandbox_id,
            f"tar -czf - -C {remote_dir!r} . | base64 -w0 > {staged!r}",
            timeout_sec=300,
        )
        result.check("download_dir")
        encoded = await self.read_file(sandbox_id, staged)
        await self.exec(sandbox_id, f"rm -f {staged!r}", timeout_sec=30)
        buffer = io.BytesIO(base64.b64decode(encoded))
        os.makedirs(local_dir, exist_ok=True)
        with tarfile.open(fileobj=buffer, mode="r:gz") as tar:
            # Python 3.12+ validates member paths; older versions do not, and
            # these archives come from a sandbox we do not fully trust.
            extract_kwargs: dict[str, Any] = {}
            if hasattr(tarfile, "data_filter"):
                extract_kwargs["filter"] = "data"
            tar.extractall(local_dir, **extract_kwargs)


async def acquire_with_backoff(
    client: SandboxClient,
    *,
    attempts: int = 6,
    base_delay: float = 2.0,
    **create_kwargs: Any,
) -> Sandbox:
    """Create a sandbox, retrying only capacity errors (503/429).

    A benchmark that fans out past the fleet's free slots will meet the
    gateway's bounded queue and then a 503 + Retry-After. That is back-pressure,
    not failure — retry it. A genuine 4xx/5xx is raised immediately.
    """
    delay = base_delay
    last: SandboxError | None = None
    for _ in range(attempts):
        try:
            return await client.create(**create_kwargs)
        except SandboxError as exc:
            if not exc.is_capacity:
                raise
            last = exc
            await asyncio.sleep(delay)
            delay = min(delay * 2, 60.0)
    raise last or SandboxError("could not acquire a sandbox")


Status = Literal["running", "paused", "deleting"]
