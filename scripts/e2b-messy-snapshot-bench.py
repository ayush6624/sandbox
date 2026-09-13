#!/usr/bin/env python3
"""Stress E2B snapshot fan-out with dirty memory and filesystem state."""

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


HOLDER = r'''
import hashlib
import json
import os
import sqlite3
import sys
import time
from pathlib import Path

memory_mib, disk_mib, small_files, sqlite_mib, run_id = sys.argv[1:]
memory_mib = int(memory_mib)
disk_mib = int(disk_mib)
small_files = int(small_files)
sqlite_mib = int(sqlite_mib)
root = Path('/home/user/e2b-messy')
root.mkdir(parents=True, exist_ok=True)

# Fill anonymous memory in small chunks to avoid a second memory-sized temporary
# allocation. Random contents prevent zero-page and compression shortcuts.
dirty = bytearray(memory_mib * 1024 * 1024)
memory_hash = hashlib.sha256()
for offset in range(0, len(dirty), 1024 * 1024):
    chunk = os.urandom(min(1024 * 1024, len(dirty) - offset))
    dirty[offset:offset + len(chunk)] = chunk
    memory_hash.update(chunk)

disk_hash = hashlib.sha256()
with (root / 'dirty.bin').open('wb') as handle:
    for _ in range(disk_mib):
        chunk = os.urandom(1024 * 1024)
        handle.write(chunk)
        disk_hash.update(chunk)

small_root = root / 'small'
small_root.mkdir(exist_ok=True)
for index in range(small_files):
    seed = hashlib.sha256(f'{run_id}:{index}'.encode()).digest()
    (small_root / f'{index:05d}.bin').write_bytes(seed * 128)

db = sqlite3.connect(root / 'state.db')
db.execute('pragma journal_mode=wal')
db.execute('pragma wal_autocheckpoint=0')
db.execute('create table if not exists payloads (id integer primary key, payload blob not null)')
rows = sqlite_mib * 512
for start in range(0, rows, 256):
    db.executemany(
        'insert into payloads(payload) values (?)',
        [(os.urandom(2048),) for _ in range(min(256, rows - start))],
    )
    db.commit()

manifest = {
    'run_id': run_id,
    'memory_mib': memory_mib,
    'memory_sha256_at_start': memory_hash.hexdigest(),
    'disk_mib': disk_mib,
    'disk_sha256': disk_hash.hexdigest(),
    'small_files': small_files,
    'sqlite_rows': rows,
}
(root / 'manifest.json').write_text(json.dumps(manifest))

cycle = 0
while True:
    # Touch every anonymous page continuously. A restored process cannot advance
    # this counter until all of its dirty memory has been faulted back in.
    for offset in range(0, len(dirty), 4096):
        dirty[offset] = (dirty[offset] + 1) & 0xff
    cycle += 1
    db.execute('insert into payloads(payload) values (?)', (f'heartbeat-{cycle}'.encode(),))
    db.commit()
    heartbeat_tmp = root / 'heartbeat.tmp'
    heartbeat_tmp.write_text(json.dumps({'cycle': cycle, 'time_ns': time.time_ns()}))
    heartbeat_tmp.replace(root / 'heartbeat.json')
    if cycle == 1:
        (root / 'ready').write_text('ready')
    time.sleep(0.05)
'''


VERIFY = r'''
import hashlib
import json
import os
import sqlite3
import time
from pathlib import Path

root = Path('/home/user/e2b-messy')
manifest = json.loads((root / 'manifest.json').read_text())
pid = int(Path('/tmp/e2b-messy.pid').read_text())
os.kill(pid, 0)
first_cycle = json.loads((root / 'heartbeat.json').read_text())['cycle']
deadline = time.time() + 60
last_cycle = first_cycle
while time.time() < deadline and last_cycle <= first_cycle:
    time.sleep(0.05)
    last_cycle = json.loads((root / 'heartbeat.json').read_text())['cycle']
if last_cycle <= first_cycle:
    raise RuntimeError('restored dirty-memory process did not complete another full sweep')

digest = hashlib.sha256()
with (root / 'dirty.bin').open('rb') as handle:
    while chunk := handle.read(1024 * 1024):
        digest.update(chunk)
if digest.hexdigest() != manifest['disk_sha256']:
    raise RuntimeError('large dirty-file checksum mismatch')

small_count = sum(1 for _ in (root / 'small').iterdir())
if small_count != manifest['small_files']:
    raise RuntimeError(f'small-file count mismatch: {small_count}')

db = sqlite3.connect(root / 'state.db')
integrity = db.execute('pragma integrity_check').fetchone()[0]
rows = db.execute('select count(*) from payloads').fetchone()[0]
if integrity != 'ok' or rows < manifest['sqlite_rows']:
    raise RuntimeError(f'sqlite verification failed: integrity={integrity} rows={rows}')

print(json.dumps({
    'memory_mib': manifest['memory_mib'],
    'disk_mib': manifest['disk_mib'],
    'small_files': small_count,
    'sqlite_rows': rows,
    'memory_cycle_before': first_cycle,
    'memory_cycle_after': last_cycle,
}))
'''


@dataclass
class ItemResult:
    index: int
    create_ms: float | None = None
    command_ready_ms: float | None = None
    hydrated_ms: float | None = None
    verification_ms: float | None = None
    ok: bool = False
    verification: dict[str, Any] | None = None
    error: str | None = None


def concise_error(exc: BaseException) -> str:
    return f"{type(exc).__name__}: {str(exc)[:300]}"


def p95(values: list[float]) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, int(0.95 * len(ordered)))]


async def kill_all(sandboxes: list[AsyncSandbox]) -> list[str]:
    async def kill_one(sandbox: AsyncSandbox) -> str | None:
        try:
            await sandbox.kill()
            return None
        except Exception as exc:
            return concise_error(exc)

    results = await asyncio.gather(*(kill_one(sandbox) for sandbox in sandboxes))
    return [result for result in results if result is not None]


async def run_batch(snapshot_id: str, count: int, round_number: int, run_id: str) -> dict[str, Any]:
    live: list[AsyncSandbox] = []
    batch_started = time.perf_counter()

    async def create_one(index: int) -> ItemResult:
        item = ItemResult(index=index)
        started = time.perf_counter()
        try:
            sandbox = await AsyncSandbox.create(
                snapshot_id,
                timeout=300,
                metadata={
                    'benchmark': 'e2b-messy-snapshot',
                    'benchmark_run': run_id,
                    'benchmark_round': str(round_number),
                },
            )
            live.append(sandbox)
            item.create_ms = (time.perf_counter() - started) * 1000
            result = await sandbox.commands.run(
                "test -f /home/user/e2b-messy/ready && "
                "kill -0 $(cat /tmp/e2b-messy.pid) && printf messy-ready",
                timeout=60,
            )
            if result.stdout.strip() != 'messy-ready':
                raise RuntimeError(f'unexpected readiness output: {result.stdout.strip()!r}')
            item.command_ready_ms = (time.perf_counter() - started) * 1000

            verify_started = time.perf_counter()
            verified = await sandbox.commands.run('python3 /tmp/e2b-messy-verify.py', timeout=120)
            item.verification_ms = (time.perf_counter() - verify_started) * 1000
            item.hydrated_ms = (time.perf_counter() - started) * 1000
            item.verification = json.loads(verified.stdout.strip())
            item.ok = True
        except Exception as exc:
            item.error = concise_error(exc)
        return item

    items = await asyncio.gather(*(create_one(index) for index in range(count)))
    makespan_ms = (time.perf_counter() - batch_started) * 1000
    cleanup_errors = await kill_all(live)
    ready = [item.command_ready_ms for item in items if item.command_ready_ms is not None]
    hydrated = [item.hydrated_ms for item in items if item.hydrated_ms is not None]
    ok = sum(item.ok for item in items)
    row = {
        'round': round_number,
        'count': count,
        'ok': ok,
        'failed': count - ok,
        'command_ready_makespan_ms': round(max(ready), 1) if ready else None,
        'command_ready_p50_ms': round(statistics.median(ready), 1) if ready else None,
        'command_ready_p95_ms': round(p95(ready), 1) if ready else None,
        'hydrated_makespan_ms': round(max(hydrated), 1) if hydrated else None,
        'hydrated_p50_ms': round(statistics.median(hydrated), 1) if hydrated else None,
        'hydrated_p95_ms': round(p95(hydrated), 1) if hydrated else None,
        'batch_wall_ms': round(makespan_ms, 1),
        'cleanup_errors': cleanup_errors,
        'items': [asdict(item) for item in items],
    }
    print(
        f"r{round_number} n={count:2d} command-ready={row['command_ready_makespan_ms']}ms "
        f"hydrated={row['hydrated_makespan_ms']}ms ok={ok}/{count}"
    )
    return row


async def benchmark(args: argparse.Namespace) -> dict[str, Any]:
    run_id = f"e2b-messy-{datetime.now(UTC).strftime('%Y%m%dT%H%M%SZ')}-{uuid.uuid4().hex[:6]}"
    source: AsyncSandbox | None = None
    snapshot_id: str | None = None
    cleanup_errors: list[str] = []
    try:
        started = time.perf_counter()
        source = await AsyncSandbox.create(
            timeout=300,
            metadata={'benchmark': 'e2b-messy-source', 'benchmark_run': run_id},
        )
        source_create_ms = (time.perf_counter() - started) * 1000
        await source.files.write('/tmp/e2b-messy-holder.py', HOLDER)
        await source.files.write('/tmp/e2b-messy-verify.py', VERIFY)
        setup_started = time.perf_counter()
        await source.commands.run(
            f"nohup python3 /tmp/e2b-messy-holder.py {args.memory_mib} {args.disk_mib} "
            f"{args.small_files} {args.sqlite_mib} {run_id} >/tmp/e2b-messy.log 2>&1 & "
            "echo $! >/tmp/e2b-messy.pid; "
            "for i in $(seq 1 240); do test -f /home/user/e2b-messy/ready && exit 0; "
            "sleep .25; done; cat /tmp/e2b-messy.log; exit 1",
            timeout=90,
        )
        setup_ms = (time.perf_counter() - setup_started) * 1000
        source_stats = await source.commands.run(
            "du -sm /home/user/e2b-messy | cut -f1; "
            "awk '/MemAvailable/ {print $2}' /proc/meminfo; "
            "cat /home/user/e2b-messy/heartbeat.json"
        )

        snapshot_started = time.perf_counter()
        snapshot = await source.create_snapshot(name=f'codex-{run_id}')
        snapshot_create_ms = (time.perf_counter() - snapshot_started) * 1000
        snapshot_id = snapshot.snapshot_id

        # Persistent snapshots survive source deletion. Releasing it gives the
        # clone fan-out the account's complete 20-sandbox quota.
        await source.kill()
        source = None
        await asyncio.sleep(1)

        print(
            f"source={source_create_ms:.1f}ms setup={setup_ms:.1f}ms "
            f"snapshot={snapshot_create_ms:.1f}ms stats={source_stats.stdout.strip()!r}"
        )
        rows: list[dict[str, Any]] = []
        for round_number in range(1, args.rounds + 1):
            for count in args.counts:
                rows.append(await run_batch(snapshot_id, count, round_number, run_id))

        return {
            'run_id': run_id,
            'started_at': datetime.now(UTC).isoformat(),
            'provider': 'e2b',
            'sdk_version': importlib.metadata.version('e2b'),
            'workload': {
                'memory_mib': args.memory_mib,
                'disk_mib': args.disk_mib,
                'small_files': args.small_files,
                'sqlite_mib': args.sqlite_mib,
            },
            'source_create_ms': round(source_create_ms, 1),
            'source_setup_ms': round(setup_ms, 1),
            'source_stats': source_stats.stdout.strip().splitlines(),
            'snapshot_create_ms': round(snapshot_create_ms, 1),
            'rows': rows,
            'cleanup_errors': cleanup_errors,
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
    parser.add_argument('--counts', default='1,4,8,16,20')
    parser.add_argument('--rounds', type=int, default=2)
    parser.add_argument('--memory-mib', type=int, default=256)
    parser.add_argument('--disk-mib', type=int, default=384)
    parser.add_argument('--small-files', type=int, default=5000)
    parser.add_argument('--sqlite-mib', type=int, default=32)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.counts = [int(value) for value in args.counts.split(',')]
    if args.rounds < 1 or any(value < 1 for value in args.counts):
        parser.error('counts and rounds must be positive')
    return args


def main() -> None:
    args = parse_args()
    load_dotenv(Path.cwd() / '.env')
    if not os.getenv('E2B_API_KEY'):
        raise SystemExit('E2B_API_KEY is missing from the root .env')
    report = asyncio.run(benchmark(args))
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + '\n')
    print(f"saved {args.output}")


if __name__ == '__main__':
    main()
