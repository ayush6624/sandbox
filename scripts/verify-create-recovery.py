#!/usr/bin/env python3
"""Run a gateway crash/lost-response drill against two idle dev workers.

Run on the Linux control VM after deploying the new runtime. HOST_TOKEN and
GATEWAY_CONTROL_TOKEN are read from the environment and never written to reports.
The primary gateway and worker heartbeat configuration are not changed.
"""
import argparse
import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


def request(base, method, path, token, body=None, key=None, timeout=120):
    data = body if isinstance(body, bytes) else None if body is None else json.dumps(body).encode()
    headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
    if key:
        headers["Idempotency-Key"] = key
    req = urllib.request.Request(base.rstrip("/") + path, method=method, headers=headers, data=data)
    try:
        response = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, dict(response.headers), response.read()


def parsed_response(result, expected=200):
    status, _, body = result
    if status != expected:
        raise RuntimeError(f"HTTP {status}, expected {expected}: {body.decode(errors='replace')[:500]}")
    return json.loads(body) if body else None


def normalized(base):
    return base.rstrip("/") if "://" in base else "http://" + base.rstrip("/")


def saved_response(result):
    status, headers, body = result
    return {"status": status, "body_base64": base64.b64encode(body).decode(),
            "body_sha256": hashlib.sha256(body).hexdigest(),
            "location": headers.get("Location", headers.get("location"))}


class ResponseBarrier:
    def __init__(self):
        self.lock = threading.Lock()
        self.held = threading.Event()
        self.release = threading.Event()
        self.commands = []
        self.winner = None

    def observe(self, worker, path, body, response):
        if path != "/internal/v1/create-commands":
            return
        status, _, raw = response
        command, outcomes = json.loads(body), json.loads(raw)
        if not isinstance(outcomes, list):
            raise RuntimeError("worker command response must be an outcome array")
        ids = [item["Sandbox"]["id"] for item in outcomes if item.get("Sandbox")]
        entry = {"worker_url": worker, "command": command, "status": status,
                 "sandbox_ids": ids, "outcomes": outcomes, "at": time.time()}
        with self.lock:
            self.commands.append(entry)
            hold = self.winner is None and 200 <= status < 300 and ids
            if hold:
                self.winner = entry
                self.held.set()
        if hold:
            self.release.wait()


def start_proxy(worker, worker_token, barrier):
    class Proxy(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            self.forward()

        def do_GET(self):
            self.forward()

        def do_DELETE(self):
            self.forward()

        def do_PATCH(self):
            self.forward()

        def forward(self):
            body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            try:
                response = request(worker, self.command, self.path, worker_token, body or None)
                barrier.observe(worker, self.path, body, response)
                status, headers, data = response
                self.send_response(status)
                self.send_header("Content-Type", headers.get("Content-Type", "application/json"))
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
            except (BrokenPipeError, ConnectionResetError):
                pass  # The intentionally killed gateway cannot receive the held reply.
            except Exception as error:
                self.send_error(502, str(error))

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Proxy)
    server.daemon_threads = False
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, "http://127.0.0.1:" + str(server.server_port)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-url", required=True)
    parser.add_argument("--target-url", required=True)
    parser.add_argument("--gateway-url", default="http://10.128.0.100:9090")
    parser.add_argument("--binary", default="/usr/local/bin/sandbox")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--count", type=int, default=3)
    parser.add_argument("--snapshot-id", help="optional existing durable snapshot for a nine-member clone batch")
    parser.add_argument("--timeout", type=int, default=300)
    args = parser.parse_args()
    if sys.platform != "linux":
        parser.error("run this verifier on the Linux control VM")
    if not 1 <= args.count <= 8 or args.timeout < 30:
        parser.error("count must be 1..8 and timeout at least 30 seconds")
    workers = [normalized(args.source_url), normalized(args.target_url)]
    if workers[0] == workers[1]:
        parser.error("source and target must be distinct workers")
    worker_token = os.environ["HOST_TOKEN"]
    primary_control = os.environ["GATEWAY_CONTROL_TOKEN"]
    client_token, control_token = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    run_id = "create-recovery-" + str(uuid.uuid4())
    state = args.output.resolve().parent / (args.output.stem + "-" + run_id)
    state.mkdir(parents=True, mode=0o700)
    client_file, control_file = state / "client.token", state / "control.token"
    for path, value in ((client_file, client_token), (control_file, control_token)):
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as out:
            out.write(value + "\n")
    report = {"run_id": run_id, "passed": False, "cleanup_errors": [], "workers": workers,
              "state_directory": str(state), "phases": [], "gateway_pids": []}
    barrier, proxies, process, log = ResponseBarrier(), [], None, None
    with socket.socket() as candidate:
        candidate.bind(("127.0.0.1", 0))
        gateway_port = candidate.getsockname()[1]
    gateway = "http://127.0.0.1:" + str(gateway_port)
    deadline = time.monotonic() + args.timeout

    def save():
        report["commands"] = list(barrier.commands)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        temp = args.output.with_suffix(args.output.suffix + ".tmp")
        temp.write_text(json.dumps(report, indent=2) + "\n")
        temp.replace(args.output)

    def inventory():
        found = {}
        for worker in workers:
            rows = parsed_response(request(worker, "GET", "/sandboxes", worker_token))
            for row in rows:
                if row.get("metadata", {}).get("benchmark_run_id") == run_id:
                    if row["id"] in found:
                        raise RuntimeError("sandbox ID appears on two workers")
                    found[row["id"]] = (worker, row)
        return found

    def wait(check, label):
        while time.monotonic() < deadline:
            value = check()
            if value:
                return value
            time.sleep(0.1)
        raise RuntimeError("timed out: " + label)

    def stop_gateway(kill=False):
        nonlocal process, log
        if process is not None:
            if process.poll() is None:
                process.send_signal(signal.SIGKILL if kill else signal.SIGTERM)
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            process = None
        if log is not None:
            log.close()
            log = None

    def start_gateway(registrations):
        nonlocal process, log
        # exec preserves this PID. Linux kills it if the verifier itself dies,
        # without preexec_fn (unsafe once proxy threads are running).
        guard = ("import ctypes,os,signal,sys; "
                 "ctypes.CDLL(None,use_errno=True).prctl(1,signal.SIGKILL); "
                 "expected=int(sys.argv[1]); "
                 "sys.exit(1) if os.getppid()!=expected else None; "
                 "os.execv(sys.argv[2],sys.argv[2:])")
        command = [sys.executable, "-c", guard, str(os.getpid()), str(Path(args.binary).resolve()),
                   "gateway", "--listen", "127.0.0.1:" + str(gateway_port),
                   "--management-transport", "private_proxy", "--token-file", str(client_file),
                   "--worker-token-file", str(control_file), "--heartbeat-ttl", str(args.timeout + 30) + "s",
                   "--queue-wait", "30s", "--operation-db", str(state / "operations.db")]
        log = (state / "gateway.log").open("ab")
        child_env = {key: value for key, value in os.environ.items()
                     if key not in {"HOST_TOKEN", "GATEWAY_CONTROL_TOKEN", "GATEWAY_TOKEN", "SANDBOX_API_KEY"}}
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=child_env)
        report["gateway_pids"].append(process.pid)

        def ready():
            if process.poll() is not None:
                raise RuntimeError("isolated gateway exited; inspect " + str(state / "gateway.log"))
            try:
                return request(gateway, "GET", "/internal/v1/hosts", control_token, timeout=1)[0] == 200
            except (urllib.error.URLError, TimeoutError):
                return False
        wait(ready, "isolated gateway startup")
        for registration in registrations:
            status, _, body = request(gateway, "POST", "/internal/v1/hosts:register", control_token, registration)
            if status not in (200, 204):
                raise RuntimeError(f"worker registration HTTP {status}: {body[:300]!r}")

    def create_body(source=None):
        body = {"metadata": {"benchmark": "create-recovery", "benchmark_run_id": run_id},
                "lifecycle": {"idle_timeout_seconds": 0}}
        if source:
            body["source"] = {"type": "snapshot", "id": source}
        return body

    def batch(count, source=None, crash=False):
        key = str(uuid.uuid4())
        body = json.dumps({"count": count, "max_parallelism": min(count, 16),
                           "sandbox": create_body(source)}, separators=(",", ":")).encode()
        first = request(gateway, "POST", "/v1/sandbox-batches", client_token, body, key)
        accepted = parsed_response(first, 202)
        op_path = "/v1/operations/" + accepted["id"]
        phase = {"kind": "snapshot" if source else "default", "operation_id": accepted["id"],
                 "accepted": saved_response(first), "requested": count}
        report["phases"].append(phase)
        save()
        if crash:
            wait(barrier.held.is_set, "successful worker response held before gateway receipt")
            phase["held_response"] = barrier.winner
            phase["before_kill"] = parsed_response(request(gateway, "GET", op_path, client_token))
            if phase["before_kill"].get("completed_at"):
                raise RuntimeError("operation completed despite held worker response; fault boundary missed")
            save()
            stop_gateway(kill=True)
            barrier.release.set()
            start_gateway(registrations)
            print("restarted isolated gateway after losing a committed worker response", flush=True)
        replay = request(gateway, "POST", "/v1/sandbox-batches", client_token, body, key)
        phase["replayed"] = saved_response(replay)
        if replay[0] != 202 or replay[2] != first[2] or saved_response(replay)["location"] != saved_response(first)["location"]:
            raise RuntimeError("batch acceptance did not replay exactly after restart")
        if {k.lower(): v for k, v in replay[1].items()}.get("idempotency-replayed") != "true":
            raise RuntimeError("batch replay response is missing Idempotency-Replayed")
        changed = json.loads(body)
        changed["sandbox"]["name"] = "conflicting-request"
        phase["conflict_status"] = request(gateway, "POST", "/v1/sandbox-batches", client_token, changed, key)[0]
        if phase["conflict_status"] != 409:
            raise RuntimeError("changed body did not conflict after restart")

        def completed():
            op = parsed_response(request(gateway, "GET", op_path, client_token))
            return op if op.get("completed_at") else None
        operation = wait(completed, "batch completion")
        phase["completed"] = operation
        results = operation.get("results", [])
        ids = [item["sandbox"]["id"] for item in results if item.get("sandbox")]
        if operation.get("failed") or len(ids) != count or len(set(ids)) != count:
            raise RuntimeError("operation did not return exactly the requested distinct sandboxes")
        if sorted(item["index"] for item in results) != list(range(count)):
            raise RuntimeError("operation lost or duplicated result indices")
        live = inventory()
        if not set(ids) <= live.keys():
            raise RuntimeError("operation result does not match actual worker inventory")
        if crash and not set(barrier.winner["sandbox_ids"]) <= set(ids):
            raise RuntimeError("recovery replaced the allocation from the lost response")
        for sandbox_id in ids:
            worker, _ = live[sandbox_id]
            result = parsed_response(request(worker, "POST", f"/sandboxes/{sandbox_id}/exec", worker_token,
                {"cmd": "printf '%s' " + run_id}))
            if result.get("exit_code") != 0 or result.get("stdout") != run_id:
                raise RuntimeError("recovered sandbox failed guest execution")
        phase["inventory_ids"] = sorted(ids)
        save()
        return ids

    try:
        hosts = parsed_response(request(args.gateway_url, "GET", "/internal/v1/hosts", primary_control))
        registrations = []
        report["hosts"] = []
        for worker in workers:
            host = next((host for host in hosts if normalized(host["addr"]) == worker), None)
            if not host or not host.get("alive") or not host.get("registry_id"):
                raise RuntimeError("live worker registry_id missing; deploy the new runtime first: " + worker)
            if host.get("free", 0) < args.count:
                raise RuntimeError("worker lacks capacity for this bounded recovery drill: " + worker)
            proxy, addr = start_proxy(worker, worker_token, barrier)
            proxies.append(proxy)
            registrations.append({"host_id": host["id"], "registry_id": host["registry_id"], "addr": addr,
                "control_token": worker_token, "release": host.get("release", ""),
                "slots_total": host["slots_total"], "slots_used": host["slots_used"],
                "slots_free": host["free"], "warm_ready": host.get("warm_ready", 0),
                "warm_ready_by_template": {"default": host.get("warm_ready", 0)}, "sandbox_ids": []})
            report["hosts"].append({key: host[key] for key in ("id", "registry_id", "addr", "release") if key in host})
        if inventory():
            raise RuntimeError("fresh run identity unexpectedly already exists")
        start_gateway(registrations)
        expected = batch(args.count, crash=True)
        if args.snapshot_id:
            expected += batch(9, source=args.snapshot_id)
        # Synchronous public creates must also retain their original response.
        single_key = str(uuid.uuid4())
        single_body = json.dumps(create_body(), separators=(",", ":")).encode()
        single_first = request(gateway, "POST", "/v1/sandboxes", client_token, single_body, single_key)
        single = parsed_response(single_first, 201)
        expected.append(single["id"])
        stop_gateway(kill=True)
        start_gateway(registrations)
        single_replay = request(gateway, "POST", "/v1/sandboxes", client_token, single_body, single_key)
        if single_replay[0] != 201 or single_replay[2] != single_first[2]:
            raise RuntimeError("single create did not replay its original 201 after restart")
        report["phases"].append({"kind": "single", "sandbox_id": single["id"],
                                "accepted": saved_response(single_first), "replayed": saved_response(single_replay)})
        live = inventory()
        if set(live) != set(expected):
            raise RuntimeError("worker inventory contains extra allocations for this run")
        single_worker, _ = live[single["id"]]
        parsed_response(request(single_worker, "DELETE", "/sandboxes/" + single["id"], worker_token), 204)
        single_deleted_replay = request(gateway, "POST", "/v1/sandboxes", client_token, single_body, single_key)
        if single_deleted_replay[0] != 201 or single_deleted_replay[2] != single_first[2] or single["id"] in inventory():
            raise RuntimeError("single create replay after deletion lost history or recreated its sandbox")
        report["phases"][-1]["replayed_after_delete"] = saved_response(single_deleted_replay)
        # Delete the original held-response allocation, then replay its exact
        # worker command. Historical success must survive live-row deletion.
        winner = barrier.winner
        for sandbox_id in winner["sandbox_ids"]:
            parsed_response(request(winner["worker_url"], "DELETE", "/sandboxes/" + sandbox_id, worker_token), 204)
        historical = parsed_response(request(winner["worker_url"], "POST", "/internal/v1/create-commands",
                                             worker_token, winner["command"]))
        historical_ids = [item["Sandbox"]["id"] for item in historical if item.get("Sandbox")]
        if historical_ids != winner["sandbox_ids"] or set(historical_ids) & inventory().keys():
            raise RuntimeError("worker replay after deletion replaced or lost its historical allocation")
        report["historical_replay_ids"] = historical_ids
        report["passed"] = True
    except Exception as error:
        report["error"] = str(error)
    finally:
        barrier.release.set()
        stop_gateway()
        for proxy in proxies:
            proxy.shutdown()
            proxy.server_close()  # Joins in-flight upstream requests before inventory cleanup.
        try:
            for sandbox_id, (worker, _) in inventory().items():
                status, _, data = request(worker, "DELETE", "/sandboxes/" + sandbox_id, worker_token)
                if status not in (204, 404):
                    report["cleanup_errors"].append(f"delete {sandbox_id}: HTTP {status} {data[:200]!r}")
            remaining = sorted(inventory())
            report["remaining_run_ids"] = remaining
            if remaining:
                report["cleanup_errors"].append("run resources remain after cleanup")
        except Exception as error:
            report["cleanup_errors"].append(str(error))
        for path in (client_file, control_file):
            path.unlink(missing_ok=True)
        report["passed"] = report["passed"] and not report["cleanup_errors"]
        save()
    print(json.dumps({"passed": report["passed"], "report": str(args.output),
                      "error": report.get("error"), "cleanup_errors": report["cleanup_errors"]}), flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
