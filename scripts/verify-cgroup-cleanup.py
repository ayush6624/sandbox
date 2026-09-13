#!/usr/bin/env python3
"""Verify snapshot/terminate releases VM cgroups, from a fleet control VM."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request
import uuid

MEMORY_PROBE = r'''
import json
from pathlib import Path
import subprocess
pid = subprocess.check_output(["pgrep", "-x", "sandbox"], text=True).splitlines()[0]
relative = Path(f"/proc/{pid}/cgroup").read_text().strip().split("::", 1)[1]
parent = (Path("/sys/fs/cgroup") / relative.lstrip("/")).parent
release = next((value.split("=", 1)[1] for value in Path(f"/proc/{pid}/environ").read_text().split("\0") if value.startswith("SANDBOX_RELEASE=")), None)
leaves = []
for child in parent.iterdir():
    if not child.is_dir() or not (child / "memory.max").exists():
        continue
    maximum = (child / "memory.max").read_text().strip()
    if maximum == "max":
        continue
    events = dict(line.split() for line in (child / "cgroup.events").read_text().splitlines())
    leaves.append({"id": child.name, "limit_bytes": int(maximum), "populated": events["populated"] == "1"})
print(json.dumps({"parent": str(parent), "release": release, "task_limit_bytes": int((parent / "memory.max").read_text()), "task_memory_events": dict((key, int(value)) for key, value in (line.split() for line in (parent / "memory.events").read_text().splitlines())), "committed_bytes": sum(x["limit_bytes"] for x in leaves), "leaves": leaves}))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api-url", required=True)
    parser.add_argument("--worker-ssh", required=True)
    parser.add_argument("--identity", required=True)
    parser.add_argument("--cycles", type=int, default=3)
    parser.add_argument("--fillers", type=int, default=0, help="Hold this many default-size idle sandboxes during verification")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.cycles < 1:
        parser.error("--cycles must be positive")
    if args.fillers < 0:
        parser.error("--fillers must be nonnegative")
    key = os.environ["SANDBOX_API_KEY"]

    def api(method, path, body=None):
        req = urllib.request.Request(args.api_url.rstrip("/") + path, method=method,
            headers={"Authorization": "Bearer " + key, "Content-Type": "application/json", "Idempotency-Key": str(uuid.uuid4())},
            data=None if body is None else json.dumps(body).encode())
        try:
            with urllib.request.urlopen(req, timeout=120) as response:
                raw = response.read()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as error:
            error.msg = f"{error.reason}: {error.read().decode(errors='replace')}"
            raise

    def memory():
        output = subprocess.check_output(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
            "-i", args.identity, args.worker_ssh, "sudo python3 -"], input=MEMORY_PROBE, text=True, timeout=30)
        return json.loads(output)

    def delete(path):
        try:
            api("DELETE", path)
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise
        try:
            api("GET", path)
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return
            raise
        raise RuntimeError("resource survived deletion: " + path)

    def settled_memory(baseline):
        deadline = time.monotonic() + 30
        while True:
            after = memory()
            clean = after["parent"] == baseline["parent"] and after["committed_bytes"] == baseline["committed_bytes"] and not any(not x["populated"] for x in after["leaves"])
            if clean or time.monotonic() >= deadline:
                return after, clean
            time.sleep(1)

    report = {"run_id": str(uuid.uuid4()), "api_url": args.api_url, "cycles": [], "filler_ids": [], "passed": False}
    original = None
    try:
        original = memory()
        for _ in range(args.fillers):
            filler = api("POST", "/sandboxes", {"hibernate_after_sec": -1,
                "metadata": {"benchmark": "cgroup-cleanup", "benchmark_run_id": report["run_id"], "role": "filler"}})
            report["filler_ids"].append(filler["id"])
        if args.fillers:
            # Default-sized fillers each reserve the same amount as a warm VM.
            expected = dict(original)
            sizes = {leaf["limit_bytes"] for leaf in original["leaves"]}
            if len(sizes) != 1:
                raise RuntimeError("occupancy verification requires a uniform warm pool baseline")
            expected["committed_bytes"] += args.fillers * sizes.pop()
            baseline, ready = settled_memory(expected)
            if not ready:
                raise RuntimeError("filler reservations and warm pool did not settle")
        else:
            baseline = original
        report["original_baseline"] = original
        report["baseline"] = baseline
        for index in range(args.cycles):
            row = {"cycle": index + 1, "cleanup_errors": []}
            report["cycles"].append(row)
            sandbox = snapshot = None
            try:
                before = memory()
                row["before"] = before
                sandbox = api("POST", "/sandboxes", {"hibernate_after_sec": -1,
                    "metadata": {"benchmark": "cgroup-cleanup", "benchmark_run_id": report["run_id"]}})["id"]
                row["sandbox_id"] = sandbox
                command = "nohup node -e 'const m=Buffer.alloc(256*1024*1024); require(\"crypto\").randomFillSync(m); require(\"fs\").writeFileSync(\"/tmp/cgroup-ready\",\"ok\"); setInterval(()=>{for(let i=0;i<m.length;i+=4096)m[i]^=1},50)' >/tmp/cgroup-holder.log 2>&1 & for i in $(seq 1 200); do test -f /tmp/cgroup-ready && exit 0; sleep .1; done; cat /tmp/cgroup-holder.log; exit 1"
                result = api("POST", f"/sandboxes/{sandbox}/exec", {"cmd": command})
                if result["exit_code"] != 0:
                    raise RuntimeError("guest preparation failed")
                snapshot = api("POST", f"/v1/sandboxes/{sandbox}/snapshots", {})["id"]
                row["snapshot_id"] = snapshot
                during = memory()
                row["during_snapshot"] = during
                baseline_ids = {leaf["id"] for leaf in before["leaves"]}
                new_leaves = [leaf for leaf in during["leaves"] if leaf["id"] not in baseline_ids]
                if not (during["parent"] == before["parent"]
                        and len(during["leaves"]) == len(before["leaves"]) + 1
                        and during["committed_bytes"] > before["committed_bytes"]
                        and len(new_leaves) == 1 and new_leaves[0]["populated"]):
                    raise RuntimeError("source VM reservation was not observed on the SSH-inspected worker")
                row["snapshot_state_at_termination"] = api("GET", f"/v1/snapshots/{snapshot}")["state"]
                if row["snapshot_state_at_termination"] != "local":
                    raise RuntimeError("upload-pending precondition was not observed")
                delete(f"/sandboxes/{sandbox}")
                sandbox = None
            finally:
                for path in ([f"/sandboxes/{sandbox}"] if sandbox else []) + ([f"/v1/snapshots/{snapshot}"] if snapshot else []):
                    try:
                        delete(path)
                    except Exception as error:
                        row["cleanup_errors"].append(str(error))
            after, clean = settled_memory(baseline)
            row["after"] = after
            row["passed"] = not row["cleanup_errors"] and clean
            print(f"cycle {index + 1}: passed={row['passed']} committed_mib={after['committed_bytes'] // 1048576}", flush=True)
            if not row["passed"]:
                raise RuntimeError("VM cgroup reservations did not return to a clean baseline")
        report["passed"] = True
    except Exception as error:
        report["error"] = str(error)
    finally:
        report["filler_cleanup_errors"] = []
        for filler in report["filler_ids"]:
            try:
                delete(f"/sandboxes/{filler}")
            except Exception as error:
                report["filler_cleanup_errors"].append(str(error))
        if report["filler_ids"] and original:
            try:
                report["final"], clean = settled_memory(original)
                report["passed"] = report["passed"] and clean and not report["filler_cleanup_errors"]
            except Exception as error:
                report["passed"] = False
                report["filler_cleanup_errors"].append(str(error))
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + "\n")
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
