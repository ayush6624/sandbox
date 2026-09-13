#!/usr/bin/env python3
"""Measure E2B snapshot fan-out through command readiness.

The benchmark creates one stateful source sandbox, snapshots it, and then
creates independent sandboxes from that snapshot at increasing concurrency.
Every clone verifies a sentinel from the source before it is counted ready.
All sandboxes and the persistent snapshot are deleted in finally blocks.
"""

from __future__ import annotations

import argparse
import asyncio
import importlib.metadata
import json
import os
import statistics
import time
import uuid
from dataclasses import asdict, dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from dotenv import load_dotenv
from e2b import AsyncSandbox


@dataclass
class ItemResult:
    index: int
    create_ms: float | None = None
    command_ready_ms: float | None = None
    ok: bool = False
    error: str | None = None


def percentile(values: list[float], p: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = min(len(ordered) - 1, int((p / 100) * len(ordered)))
    return ordered[index]


def concise_error(exc: BaseException) -> str:
    return f"{type(exc).__name__}: {str(exc)[:240]}"


async def kill_all(sandboxes: list[AsyncSandbox]) -> list[str]:
    async def kill_one(sandbox: AsyncSandbox) -> str | None:
        try:
            await sandbox.kill()
            return None
        except Exception as exc:  # Cleanup must continue across failures.
            return concise_error(exc)

    errors = await asyncio.gather(*(kill_one(sandbox) for sandbox in sandboxes))
    return [error for error in errors if error is not None]


async def run_batch(
    *,
    source: str | None,
    count: int,
    round_number: int,
    run_id: str,
) -> dict[str, Any]:
    mode = "snapshot" if source else "default"
    live: list[AsyncSandbox] = []
    started = time.perf_counter()

    async def create_one(index: int) -> ItemResult:
        item = ItemResult(index=index)
        item_started = time.perf_counter()
        try:
            sandbox = await AsyncSandbox.create(
                source,
                timeout=300,
                metadata={
                    "benchmark": "e2b-snapshot-fanout",
                    "benchmark_run": run_id,
                    "benchmark_mode": mode,
                    "benchmark_round": str(round_number),
                },
            )
            live.append(sandbox)
            item.create_ms = (time.perf_counter() - item_started) * 1000
            command = "cat /tmp/e2b-snapshot-sentinel" if source else "printf default-ready"
            result = await sandbox.commands.run(command, timeout=60)
            expected = run_id if source else "default-ready"
            if result.stdout.strip() != expected:
                raise RuntimeError(f"unexpected readiness output: {result.stdout.strip()!r}")
            item.command_ready_ms = (time.perf_counter() - item_started) * 1000
            item.ok = True
        except Exception as exc:
            item.error = concise_error(exc)
        return item

    items = await asyncio.gather(*(create_one(index) for index in range(count)))
    makespan_ms = (time.perf_counter() - started) * 1000
    cleanup_errors = await kill_all(live)
    ready = [item.command_ready_ms for item in items if item.command_ready_ms is not None]
    creates = [item.create_ms for item in items if item.create_ms is not None]
    ok = sum(item.ok for item in items)
    row = {
        "mode": mode,
        "round": round_number,
        "count": count,
        "ok": ok,
        "failed": count - ok,
        "makespan_ms": round(makespan_ms, 1),
        "create_p50_ms": round(statistics.median(creates), 1) if creates else None,
        "create_p95_ms": round(percentile(creates, 95), 1) if creates else None,
        "ready_p50_ms": round(statistics.median(ready), 1) if ready else None,
        "ready_p95_ms": round(percentile(ready, 95), 1) if ready else None,
        "cleanup_errors": cleanup_errors,
        "items": [asdict(item) for item in items],
    }
    print(
        f"{mode:8s} r{round_number} n={count:2d} ready={makespan_ms:7.1f}ms "
        f"ok={ok}/{count} p50={row['ready_p50_ms']}ms p95={row['ready_p95_ms']}ms"
    )
    return row


async def benchmark(args: argparse.Namespace) -> dict[str, Any]:
    run_id = f"e2b-{datetime.now(UTC).strftime('%Y%m%dT%H%M%SZ')}-{uuid.uuid4().hex[:6]}"
    source: AsyncSandbox | None = None
    snapshot_id: str | None = None
    rows: list[dict[str, Any]] = []
    cleanup_errors: list[str] = []
    try:
        started = time.perf_counter()
        source = await AsyncSandbox.create(
            timeout=300,
            metadata={"benchmark": "e2b-snapshot-source", "benchmark_run": run_id},
        )
        source_create_ms = (time.perf_counter() - started) * 1000
        await source.commands.run(f"printf %s {run_id} >/tmp/e2b-snapshot-sentinel")

        started = time.perf_counter()
        snapshot = await source.create_snapshot(name=f"codex-{run_id}")
        snapshot_create_ms = (time.perf_counter() - started) * 1000
        snapshot_id = snapshot.snapshot_id
        print(f"source create={source_create_ms:.1f}ms snapshot={snapshot_create_ms:.1f}ms")

        for round_number in range(1, args.rounds + 1):
            for count in args.counts:
                rows.append(
                    await run_batch(
                        source=snapshot_id,
                        count=count,
                        round_number=round_number,
                        run_id=run_id,
                    )
                )

        if args.baseline:
            for round_number in range(1, args.rounds + 1):
                rows.append(
                    await run_batch(
                        source=None,
                        count=max(args.counts),
                        round_number=round_number,
                        run_id=run_id,
                    )
                )

        return {
            "run_id": run_id,
            "started_at": datetime.now(UTC).isoformat(),
            "provider": "e2b",
            "sdk_version": importlib.metadata.version("e2b"),
            "counts": args.counts,
            "rounds": args.rounds,
            "source_create_ms": round(source_create_ms, 1),
            "snapshot_create_ms": round(snapshot_create_ms, 1),
            "rows": rows,
            "cleanup_errors": cleanup_errors,
        }
    finally:
        if source is not None:
            try:
                await source.kill()
            except Exception as exc:
                cleanup_errors.append(concise_error(exc))
        if snapshot_id is not None:
            try:
                await AsyncSandbox.delete_snapshot(snapshot_id)
            except Exception as exc:
                cleanup_errors.append(concise_error(exc))


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--counts", default="1,4,8,16,32")
    parser.add_argument("--rounds", type=int, default=3)
    parser.add_argument("--baseline", action="store_true")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.counts = [int(value) for value in args.counts.split(",")]
    if args.rounds < 1 or any(count < 1 for count in args.counts):
        parser.error("counts and rounds must be positive")
    return args


def main() -> None:
    args = parse_args()
    load_dotenv(Path.cwd() / ".env")
    if not os.getenv("E2B_API_KEY"):
        raise SystemExit("E2B_API_KEY is missing from the root .env")
    report = asyncio.run(benchmark(args))
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + "\n")
    print(f"saved {args.output}")


if __name__ == "__main__":
    main()
