#!/usr/bin/env python3
"""Verify full RAM and disk hashes through a two-worker hibernation round trip.

Run on the control host with HOST_TOKEN. Uses only sandbox APIs; performs no
worker restart, firewall change, or cache eviction. Hash probes read all 256 MiB
of the retained random buffer, so this is a correctness proof, not a latency
benchmark of a small working set. Cleanup reclaims a released record if needed.
Use --transport peer to require peer reads and automatic source cleanup, or
--transport fallback to delete the source checkpoint before each adoption and
require a GCS fallback. --transport peer-loss removes a retained source after
adoption reports ready and before the first hash probe. Every mode verifies the
same complete hashes; peer-loss counters do not distinguish faults from hydration.
Release waits for backup by default. Use --handoff async with peer transport to
verify readiness while backup continues; source-removal modes require durability.
"""
import argparse
import base64
import json
import math
import os
from pathlib import Path
import re
import shlex
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


WORKLOAD = r"""const crypto=require('crypto'),fs=require('fs'),http=require('http');
const memory=crypto.randomBytes(256*1024*1024), nonce=crypto.randomUUID();
const diskPath='/tmp/lazy-adoption-proof-disk';
fs.writeFileSync(diskPath,crypto.randomBytes(8*1024*1024));
let ticks=0;setInterval(()=>ticks++,100);
http.createServer((req,res)=>{
const disk=fs.readFileSync(diskPath);
res.setHeader('Content-Type','application/json');
res.end(JSON.stringify({nonce,ticks,pid:process.pid,memory_bytes:memory.length,
disk_bytes:disk.length,memory:crypto.createHash('sha256').update(memory).digest('hex'),
disk:crypto.createHash('sha256').update(disk).digest('hex')}));
}).listen(3333,'127.0.0.1');"""
PROBE = "curl --max-time 180 -fsS http://127.0.0.1:3333"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-url", required=True)
    parser.add_argument("--target-url", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--delay-seconds", type=float, default=2)
    parser.add_argument("--transport", choices=("peer", "fallback", "peer-loss"),
                        help="require peer hits, force fallback before adoption, or remove the peer after readiness")
    parser.add_argument("--handoff", choices=("async", "durable"), default="durable",
                        help="return release before backup completes, or require durable backup (default)")
    args = parser.parse_args()
    if args.handoff == "async" and args.transport in ("fallback", "peer-loss"):
        parser.error("source-removal transports require --handoff durable")
    if args.delay_seconds < 0.5:
        parser.error("delay-seconds must be at least 0.5 to observe timer progress")
    hosts = [args.source_url.rstrip("/"), args.target_url.rstrip("/")]
    for host in hosts:
        parsed = urllib.parse.urlparse(host)
        if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.path or parsed.query or parsed.fragment:
            parser.error("worker URLs must be HTTP(S) origins without credentials or query strings")
    if hosts[0] == hosts[1]:
        parser.error("source and target must be different workers")
    token = os.environ.get("HOST_TOKEN")
    if not token:
        parser.error("HOST_TOKEN is required")
    run_id = "lazy-memory-proof-" + str(uuid.uuid4())
    create_key = str(uuid.uuid4())
    labels = {"benchmark": "lazy-adoption-memory-proof", "benchmark_run_id": run_id}
    create_body = {"source": {"type": "default"}, "lifecycle": {"idle_timeout_seconds": 0}, "metadata": labels}
    report = {"run_id": run_id, "source_url": hosts[0], "target_url": hosts[1],
              "release": os.environ.get("SANDBOX_RELEASE"), "passed": False,
              "transport": args.transport, "handoff": args.handoff,
              "workload": {"memory_bytes": 256 * 1024 * 1024, "disk_bytes": 8 * 1024 * 1024,
                           "hash": "sha256 of every byte", "delay_seconds": args.delay_seconds},
              "moves": [], "api_calls": [], "peer_generations": [], "cleanup": [], "cleanup_errors": []}
    sandbox = None
    create_attempted = False

    def save():
        args.output.parent.mkdir(parents=True, exist_ok=True)
        temporary = args.output.with_suffix(args.output.suffix + ".tmp")
        temporary.write_text(json.dumps(report, indent=2) + "\n")
        temporary.replace(args.output)

    def api(base, method, path, body=None, key=None, response_headers=None, raw=False, timeout=240):
        headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
        if key:
            headers["Idempotency-Key"] = key
        request = urllib.request.Request(base + path, method=method, headers=headers,
            data=None if body is None else json.dumps(body).encode())
        started = time.monotonic()
        observation = {"host": base, "method": method, "path": path}
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                observation["status"] = response.status
                if response_headers is not None:
                    generation = response.headers.get("X-Sandbox-Hibernation-Generation")
                    if generation:
                        response_headers["generation"] = generation
                data = response.read()
                if raw:
                    return data.decode()
                return json.loads(data) if data else None
        except urllib.error.HTTPError as error:
            observation["status"] = error.code
            error.msg = f"{error.reason}: {error.read().decode(errors='replace')}"
            raise
        finally:
            observation["duration_ms"] = round((time.monotonic() - started) * 1000, 2)
            report["api_calls"].append(observation)

    def metrics(base):
        captured = {"captured_at_unix": time.time(), "values": {}}
        for line in api(base, "GET", "/metrics", raw=True).splitlines():
            match = re.fullmatch(r"(sandbox_hib_peer_[a-z_]+)\s+([0-9.eE+\-]+)", line)
            if match:
                value = float(match[2])
                if not math.isfinite(value) or value < 0:
                    raise RuntimeError("invalid peer metric " + match[1])
                captured["values"][match[1]] = value
        for name in ("chunks_total", "artifacts_total", "fallbacks_total"):
            if "sandbox_hib_peer_" + name not in captured["values"]:
                raise RuntimeError("missing peer metric sandbox_hib_peer_" + name)
        return captured

    def generation_absent(base, generation, timeout=15):
        try:
            api(base, "GET", f"/internal/v1/hibernations/{generation}", timeout=timeout)
            return False
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return True
            raise

    def wait_generation_absent(base, generation):
        started = time.monotonic()
        deadline = started + 120
        while time.monotonic() < deadline:
            remaining = max(0.001, deadline - time.monotonic())
            if generation_absent(base, generation, timeout=min(15, remaining)):
                return round((time.monotonic() - started) * 1000, 2)
            time.sleep(min(0.2, max(0, deadline - time.monotonic())))
        raise RuntimeError("generation retained after 120 seconds waiting for backup and cache acknowledgment")

    def delete_generation(entry):
        try:
            api(entry["source_url"], "DELETE", f"/internal/v1/hibernations/{entry['generation']}")
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise
        entry["cleanup_wait_ms"] = wait_generation_absent(entry["source_url"], entry["generation"])
        entry["deleted"] = True

    def guest(base, command):
        result = api(base, "POST", f"/sandboxes/{sandbox}/exec", {"cmd": command})
        if result.get("exit_code") != 0:
            raise RuntimeError("guest command failed: " + json.dumps(result))
        return json.loads(result["stdout"].strip())

    def owned(row):
        if not isinstance(row, dict) or row.get("metadata", {}).get("benchmark_run_id") != run_id:
            raise RuntimeError("sandbox metadata does not establish ownership for this run")
        return row

    def lookup(base, resource):
        try:
            return api(base, "GET", f"/sandboxes/{resource}")
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return None
            raise

    def verify(value, previous):
        for key in ("nonce", "memory", "disk", "memory_bytes", "disk_bytes", "pid"):
            if value.get(key) != report["expected"].get(key):
                raise RuntimeError(f"restored {key} differs from source: {value.get(key)!r}")
        if not isinstance(value.get("ticks"), int) or value["ticks"] < previous["ticks"]:
            raise RuntimeError("guest timer moved backwards")

    def move(source, target, previous):
        row = {"source_url": source, "target_url": target, "before_release": previous}
        report["moves"].append(row)
        if args.transport:
            row["metrics_before"] = {host: metrics(host) for host in (source, target)}
        started = time.monotonic()
        release_headers = {}
        release_path = f"/sandboxes/{sandbox}/release"
        if args.handoff == "durable":
            release_path += "?durability=required"
        api(source, "POST", release_path, response_headers=release_headers)
        row["release_ms"] = round((time.monotonic() - started) * 1000, 2)
        generation = release_headers.get("generation")
        if generation:
            if str(uuid.UUID(generation)) != generation:
                raise RuntimeError("release returned a noncanonical generation UUID")
            retained = {"source_url": source, "generation": generation}
            report["peer_generations"].append(retained)
            row["peer_generation"] = generation
            save()
        elif args.transport:
            raise RuntimeError("release returned no peer generation header")
        if lookup(source, sandbox) is not None:
            raise RuntimeError("release left a local sandbox row")
        if args.transport == "fallback":
            delete_generation(retained)
            row["source_generation_removed_before_adopt"] = True
        save()
        started = time.monotonic()
        adopted = owned(api(target, "POST", f"/sandboxes/{sandbox}/adopt"))
        row["adopt_api_ms"] = round((time.monotonic() - started) * 1000, 2)
        if adopted["id"] != sandbox or adopted.get("status") != "running":
            raise RuntimeError("adopt did not return the original running sandbox")
        row["adopted"] = {key: adopted.get(key) for key in ("id", "status", "guest_ip", "tap_device", "base_snapshot_id")}
        if args.transport == "peer-loss":
            row["target_metrics_after_ready"] = metrics(target)
            row["source_generation_present_after_ready"] = not generation_absent(source, generation)
            save()
            if not row["source_generation_present_after_ready"]:
                raise RuntimeError("source generation already absent after ready; peer-loss was not exercised")
            delete_generation(retained)
            row["source_generation_removed_after_ready"] = True
            row["source_generation_removed_at_unix"] = time.time()
            save()
        started = time.monotonic()
        row["immediate"] = guest(target, PROBE)
        row["immediate_full_hash_ms"] = round((time.monotonic() - started) * 1000, 2)
        verify(row["immediate"], previous)
        save()
        time.sleep(args.delay_seconds)
        started = time.monotonic()
        row["delayed"] = guest(target, PROBE)
        row["delayed_full_hash_ms"] = round((time.monotonic() - started) * 1000, 2)
        verify(row["delayed"], row["immediate"])
        if row["delayed"]["ticks"] <= row["immediate"]["ticks"]:
            raise RuntimeError("guest timer did not progress after adoption")
        if args.transport == "peer":
            row["generation_cleanup_wait_ms"] = wait_generation_absent(source, generation)
            retained["acknowledged"] = True
            row["source_generation_acknowledged"] = True
        if args.transport:
            row["metrics_after"] = {host: metrics(host) for host in (source, target)}
            row["metric_deltas"] = {}
            for host in (source, target):
                before = row["metrics_before"][host]["values"]
                after = row["metrics_after"][host]["values"]
                deltas = {name: value - before[name] for name, value in after.items() if name in before}
                if any(value < 0 for name, value in deltas.items() if name.endswith("_total")):
                    raise RuntimeError("peer counters reset during the proof")
                row["metric_deltas"][host] = deltas
            save()
            required = ("chunks_total", "artifacts_total") if args.transport in ("peer", "peer-loss") else ("fallbacks_total",)
            for name in required:
                if row["metric_deltas"][target]["sandbox_hib_peer_" + name] <= 0:
                    raise RuntimeError("transport proof observed no target " + name)
            if args.transport == "peer-loss":
                before = row["metrics_before"][target]["values"]
                ready = row["target_metrics_after_ready"]["values"]
                for name in ("chunks_total", "artifacts_total"):
                    if ready["sandbox_hib_peer_" + name] <= before["sandbox_hib_peer_" + name]:
                        raise RuntimeError("peer-loss observed no initial target " + name)
                row["fallbacks_after_ready"] = row["metrics_after"][target]["values"]["sandbox_hib_peer_fallbacks_total"] - ready["sandbox_hib_peer_fallbacks_total"]
                row["fallback_attribution"] = "aggregate VM source and background hydration; does not establish guest-fault attribution"
        save()
        print(f"Full RAM/disk hashes and timer verified on {target}", flush=True)
        return row["delayed"]

    def cleanup():
        for entry in report["peer_generations"]:
            try:
                delete_generation(entry)
            except Exception as error:
                report["cleanup_errors"].append("generation " + entry["generation"] + ": " + str(error))
        resources = set([sandbox] if sandbox else [])
        if create_attempted and sandbox is None:
            try:
                # Recover a lost create response with the original durable request identity.
                recovered = api(hosts[0], "POST", "/v1/sandboxes", create_body, create_key)
                resources.add(recovered["id"])
            except Exception as error:
                report["cleanup_errors"].append("recover create response: " + str(error))
        for host in hosts:
            try:
                for row in api(host, "GET", "/sandboxes") or []:
                    if row.get("metadata", {}).get("benchmark_run_id") == run_id:
                        resources.add(row["id"])
            except Exception as error:
                report["cleanup_errors"].append("inventory " + host + ": " + str(error))
        for resource in resources:
            entry = {"sandbox_id": resource, "deleted_from": [], "reclaimed": False}
            report["cleanup"].append(entry)
            try:
                for host in hosts:
                    current = lookup(host, resource)
                    if current is None:
                        continue
                    owned(current)
                    api(host, "DELETE", f"/sandboxes/{resource}")
                    entry["deleted_from"].append(host)
                if not entry["deleted_from"]:
                    # A successful release followed by failed adoption has no local row.
                    failures = []
                    for host in hosts:
                        try:
                            owned(api(host, "POST", f"/sandboxes/{resource}/adopt"))
                            api(host, "DELETE", f"/sandboxes/{resource}")
                            entry["deleted_from"].append(host)
                            entry["reclaimed"] = True
                            break
                        except urllib.error.HTTPError as error:
                            if error.code != 404:
                                failures.append(host + ": " + str(error))
                    if not entry["deleted_from"]:
                        if failures:
                            raise RuntimeError("could not reclaim released record: " + "; ".join(failures))
                        entry["no_adoptable_record_on_either_host"] = True
                for host in hosts:
                    if lookup(host, resource) is not None:
                        raise RuntimeError("sandbox survived cleanup on " + host)
            except Exception as error:
                report["cleanup_errors"].append(resource + ": " + str(error))

    try:
        save()
        create_attempted = True
        started = time.monotonic()
        created = api(hosts[0], "POST", "/v1/sandboxes", create_body, create_key)
        sandbox = created["id"]
        report["sandbox_id"] = sandbox
        report["create_ms"] = round((time.monotonic() - started) * 1000, 2)
        owned(lookup(hosts[0], sandbox))
        save()
        encoded = base64.b64encode(WORKLOAD.encode()).decode()
        command = (f"printf %s {shlex.quote(encoded)} | base64 -d > /tmp/lazy-adoption-proof.js; "
                   "nohup node /tmp/lazy-adoption-proof.js >/tmp/lazy-adoption-proof.log 2>&1 & "
                   f"for i in $(seq 1 300); do {PROBE} && exit 0; sleep .1; done; "
                   "cat /tmp/lazy-adoption-proof.log; exit 1")
        report["expected"] = guest(hosts[0], command)
        if report["expected"].get("memory_bytes") != 256 * 1024 * 1024 or report["expected"].get("disk_bytes") != 8 * 1024 * 1024:
            raise RuntimeError("guest did not allocate the requested memory and disk payloads")
        save()
        after = move(hosts[0], hosts[1], report["expected"])
        move(hosts[1], hosts[0], after)
        report["passed"] = True
    except (Exception, KeyboardInterrupt) as error:
        report["error"] = str(error) or type(error).__name__
    finally:
        cleanup()
        report["passed"] = report["passed"] and not report["cleanup_errors"]
        save()
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
