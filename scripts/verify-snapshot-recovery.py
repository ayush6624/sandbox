#!/usr/bin/env python3
"""Interrupt uploads and restart an idle dev worker, then verify a remote restore.

Runs from the control VM. Temporarily rejects root-owned outbound HTTPS on the
source worker; a systemd timer removes the rule after three minutes if interrupted.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import shlex
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-url", required=True)
    parser.add_argument("--target-url", required=True)
    parser.add_argument("--source-ssh", required=True)
    parser.add_argument("--identity", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if urllib.parse.urlparse(args.source_url).hostname != args.source_ssh.split("@")[-1]:
        parser.error("source API and SSH must address the same worker")
    if args.source_url.rstrip("/") == args.target_url.rstrip("/"):
        parser.error("source and target must be different workers")
    key = os.environ["SANDBOX_API_KEY"]
    run_id = "snapshot-recovery-" + str(uuid.uuid4())
    report = {"run_id": run_id, "source_url": args.source_url, "target_url": args.target_url,
        "release": os.environ.get("SANDBOX_RELEASE"), "passed": False, "cleanup_errors": [], "observations": []}
    source = snapshot = clone = None
    fault_started = False

    def api(base, method, path, body=None):
        request = urllib.request.Request(base.rstrip("/") + path, method=method,
            headers={"Authorization": "Bearer " + key, "Content-Type": "application/json", "Idempotency-Key": str(uuid.uuid4())},
            data=None if body is None else json.dumps(body).encode())
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                data = response.read()
                return json.loads(data) if data else None
        except urllib.error.HTTPError as error:
            error.msg = f"{error.reason}: {error.read().decode(errors='replace')}"
            raise

    def ssh(command):
        return subprocess.check_output(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
            "-i", args.identity, args.source_ssh, shlex.join(command)], text=True, timeout=30).strip()

    rule = ["OUTPUT", "-p", "tcp", "--dport", "443", "-m", "owner", "--uid-owner", "0",
        "-m", "comment", "--comment", run_id, "-j", "REJECT", "--reject-with", "tcp-reset"]

    def remove_fault():
        # Delete only this invocation's rule. The timed cleanup is independent
        # of the SSH connection and remains armed until this removal succeeds.
        def matching_rules():
            rules = [shlex.split(line) for line in ssh(["sudo", "iptables", "-S", "OUTPUT"]).splitlines()]
            return [entry for entry in rules if "--comment" in entry and entry[entry.index("--comment") + 1] == run_id]

        for entry in matching_rules():
            try:
                ssh(["sudo", "iptables", "-D", *entry[1:]])
            except subprocess.CalledProcessError:
                if entry in matching_rules():
                    raise
        timer = run_id + ".timer"
        if ssh(["sudo", "systemctl", "show", "--property=LoadState", "--value", timer]) != "not-found":
            ssh(["sudo", "systemctl", "stop", timer])

    def observe(label):
        value = api(args.source_url, "GET", f"/v1/snapshots/{snapshot}")
        report["observations"].append({"phase": label, "at": time.time(), "snapshot": value})
        return value

    def wait_for(label, predicate, timeout=180):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                value = observe(label)
                if predicate(value):
                    return value
            except (urllib.error.URLError, TimeoutError, ConnectionError) as error:
                report["observations"].append({"phase": label, "error": str(error)})
            time.sleep(0.5)
        raise RuntimeError("timed out: " + label)

    def guest(base, sandbox, command):
        result = api(base, "POST", f"/sandboxes/{sandbox}/exec", {"cmd": command})
        if result["exit_code"] != 0:
            raise RuntimeError("guest command failed: " + str(result))
        return result["stdout"].strip()

    def delete(base, path):
        try:
            api(base, "DELETE", path)
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise
        try:
            api(base, "GET", path)
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return
            raise
        raise RuntimeError("resource survived deletion: " + path)

    try:
        existing = api(args.source_url, "GET", "/sandboxes")
        if any(sandbox["status"] != "hibernated" for sandbox in existing):
            raise RuntimeError("source worker must have no active user sandboxes before this restart test")
        report["source_pid_before"] = int(ssh(["pgrep", "-x", "sandbox"]))
        source = api(args.source_url, "POST", "/sandboxes", {"hibernate_after_sec": -1,
            "metadata": {"benchmark": "snapshot-recovery", "benchmark_run_id": run_id}})["id"]
        report["source_id"] = source
        script = '''const crypto=require('crypto'),fs=require('fs'),http=require('http');
const memory=crypto.randomBytes(256*1024*1024), nonce=crypto.randomUUID();
fs.writeFileSync('/tmp/recovery-disk',crypto.randomBytes(8*1024*1024));
let ticks=0;setInterval(()=>ticks++,100);
http.createServer((req,res)=>res.end(JSON.stringify({nonce,ticks,
memory:crypto.createHash('sha256').update(memory).digest('hex'),
disk:crypto.createHash('sha256').update(fs.readFileSync('/tmp/recovery-disk')).digest('hex')}))).listen(3333,'127.0.0.1');'''
        encoded = base64.b64encode(script.encode()).decode()
        command = f"echo {shlex.quote(encoded)} | base64 -d > /tmp/recovery.js; nohup node /tmp/recovery.js >/tmp/recovery.log 2>&1 & for i in $(seq 1 200); do curl -fsS http://127.0.0.1:3333 && exit 0; sleep .1; done; cat /tmp/recovery.log; exit 1"
        expected = json.loads(guest(args.source_url, source, command))
        report["expected"] = expected
        ssh(["sudo", "systemd-run", "--quiet", "--unit", run_id, "--on-active=180s",
            "/usr/sbin/iptables", "-D", *rule])
        fault_started = True
        ssh(["sudo", "iptables", "-I", *rule])
        print("storage interruption installed with automatic cleanup", flush=True)
        started = time.monotonic()
        snapshot = api(args.source_url, "POST", f"/v1/sandboxes/{source}/snapshots", {})["id"]
        report["snapshot_id"] = snapshot
        report["local_capture_ms"] = round((time.monotonic() - started) * 1000)
        before = wait_for("retry-before-restart", lambda s: s.get("upload", {}).get("state") == "retrying")
        assert before["state"] == "local"
        report["attempts_before_restart"] = before["upload"]["attempts"]
        ssh(["sudo", "kill", "-KILL", str(report["source_pid_before"])])
        after = wait_for("recovered-after-restart", lambda s: s.get("upload", {}).get("attempts", 0) > report["attempts_before_restart"])
        report["source_pid_after"] = int(ssh(["pgrep", "-x", "sandbox"]))
        assert report["source_pid_after"] != report["source_pid_before"]
        assert after["state"] == "local"
        print("new worker process resumed the persisted upload", flush=True)
        remove_fault()
        fault_started = False
        durable = wait_for("durable", lambda s: s["state"] == "durable", timeout=300)
        assert "upload" not in durable
        clone = api(args.target_url, "POST", "/v1/sandboxes", {"source": {"type": "snapshot", "id": snapshot},
            "lifecycle": {"idle_timeout_seconds": 0}, "metadata": {"benchmark_run_id": run_id}})["id"]
        report["clone_id"] = clone
        restored = json.loads(guest(args.target_url, clone, "curl -fsS http://127.0.0.1:3333"))
        report["restored"] = restored
        assert all(restored[key] == expected[key] for key in ("nonce", "memory", "disk"))
        assert restored["ticks"] >= expected["ticks"]
        print("remote restore preserved memory, process state and disk bytes", flush=True)
        report["passed"] = True
    except Exception as error:
        report["error"] = str(error)
    finally:
        if fault_started:
            try:
                remove_fault()
            except Exception as error:
                report["cleanup_errors"].append("storage rule removal: " + str(error))
        for base, path in ([(args.target_url, f"/v1/sandboxes/{clone}")] if clone else []) + ([(args.source_url, f"/sandboxes/{source}")] if source else []) + ([(args.source_url, f"/v1/snapshots/{snapshot}"), (args.target_url, f"/v1/snapshots/{snapshot}")] if snapshot else []):
            try:
                delete(base, path)
            except Exception as error:
                report["cleanup_errors"].append(str(error))
        report["passed"] = report["passed"] and not report["cleanup_errors"]
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + "\n")
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
