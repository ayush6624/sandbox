import base64
import hashlib
import json
import math
import os
from pathlib import Path
import re
import selectors
import subprocess
import sys
import time

OUTPUT_LIMIT = 65536
RECEIPT_LIMIT = 262144


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def durable_file(path, data):
    with open(path, "xb") as output:
        output.write(data)
        output.flush()
        os.fsync(output.fileno())


def json_bytes(value):
    return (json.dumps(value, separators=(",", ":"), allow_nan=False) + "\n").encode()


def read_bounded(path, limit):
    with open(path, "rb") as source:
        data = source.read(limit + 1)
    if len(data) > limit:
        raise ValueError("receipt file exceeds size limit")
    return data


def output_receipt(data, truncated):
    return {
        "base64": base64.b64encode(data).decode("ascii"),
        "sha256": hashlib.sha256(data).hexdigest(),
        "bytes": len(data),
        "truncated": truncated,
    }


def validate_output(value, data):
    if not isinstance(value, dict) or type(value.get("truncated")) is not bool:
        raise ValueError("invalid output receipt")
    if type(value.get("bytes")) is not int or value["bytes"] != len(data):
        raise ValueError("invalid output byte count")
    if value["truncated"] and len(data) != OUTPUT_LIMIT:
        raise ValueError("invalid truncated output size")
    if value != output_receipt(data, value["truncated"]):
        raise ValueError("output receipt does not match stored bytes")


def read_completed(directory, identity):
    started = json.loads(read_bounded(directory / "identity.json", 4096))
    if started != identity:
        raise ValueError("reservation identity mismatch")
    receipt = json.loads(read_bounded(directory / "completed.json", RECEIPT_LIMIT))
    if not isinstance(receipt, dict):
        raise ValueError("invalid terminal receipt")
    if any(receipt.get(key) != value for key, value in identity.items()):
        raise ValueError("terminal identity mismatch")
    if receipt.get("kind") != "completed" or not isinstance(receipt.get("result"), dict):
        raise ValueError("missing terminal command result")
    result = receipt["result"]
    if type(result.get("exitCode")) is not int or not 0 <= result["exitCode"] <= 255:
        raise ValueError("invalid exit code")
    duration = result.get("durationMs")
    if type(duration) not in (int, float) or not math.isfinite(duration) or duration < 0:
        raise ValueError("invalid duration")
    for stream in ("stdout", "stderr"):
        data = read_bounded(directory / (stream + ".bin"), OUTPUT_LIMIT)
        validate_output(result.get(stream), data)
    return receipt


def capture(script):
    started = time.monotonic()
    child = subprocess.Popen(
        ["bash", "-lc", script], stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    buffers = {"stdout": bytearray(), "stderr": bytearray()}
    truncated = {"stdout": False, "stderr": False}
    with selectors.DefaultSelector() as selector:
        selector.register(child.stdout, selectors.EVENT_READ, "stdout")
        selector.register(child.stderr, selectors.EVENT_READ, "stderr")
        while selector.get_map():
            for key, _ in selector.select():
                data = os.read(key.fd, 8192)
                if not data:
                    selector.unregister(key.fileobj)
                    key.fileobj.close()
                    continue
                name = key.data
                room = OUTPUT_LIMIT - len(buffers[name])
                buffers[name].extend(data[:room])
                if len(data) > room:
                    truncated[name] = True
    code = child.wait()
    if code < 0:
        code = 128 - code
    return {
        "exitCode": code,
        "durationMs": (time.monotonic() - started) * 1000,
        **{name: output_receipt(data, truncated[name]) for name, data in buffers.items()},
    }, buffers


def execute(request):
    if not isinstance(request, dict) or request.get("version") != 1:
        raise ValueError("invalid request version")
    run_id, index = request.get("run_id"), request.get("index")
    sandbox_id, script = request.get("sandbox_id"), request.get("script")
    if not isinstance(run_id, str) or re.fullmatch(r"[A-Za-z0-9_-]{1,128}", run_id) is None:
        raise ValueError("invalid run identity")
    if type(index) is not int or not 0 <= index < 32:
        raise ValueError("invalid member index")
    if not isinstance(sandbox_id, str) or not 1 <= len(sandbox_id) <= 256:
        raise ValueError("invalid sandbox identity")
    if not isinstance(script, str) or len(script.encode()) > 65536:
        raise ValueError("invalid script")
    digest = hashlib.sha256(script.encode()).hexdigest()
    if request.get("script_sha256") != digest:
        raise ValueError("script digest mismatch")
    identity = {
        "version": 1, "run_id": run_id, "index": index,
        "sandbox_id": sandbox_id, "script_sha256": digest,
    }
    root = Path(os.environ.get("WORKSPACE_RECEIPT_ROOT", "/home/sandbox/.workspace-attempts"))
    run = root / run_id
    directory = run / str(index)
    try:
        root.mkdir(parents=True, exist_ok=True)
        sync_directory(root.parent)
        run.mkdir(exist_ok=True)
        sync_directory(root)
        try:
            directory.mkdir()
        except FileExistsError:
            return read_completed(directory, identity)
        sync_directory(run)
        durable_file(directory / "identity.json", json_bytes(identity))
        sync_directory(directory)
        result, buffers = capture(script)
        for name, data in buffers.items():
            durable_file(directory / (name + ".bin"), data)
        sync_directory(directory)
        receipt = {**identity, "kind": "completed", "result": result}
        durable_file(directory / "completed.tmp", json_bytes(receipt))
        os.rename(directory / "completed.tmp", directory / "completed.json")
        sync_directory(directory)
        return receipt
    except (OSError, ValueError, TypeError, KeyError) as error:
        return {**identity, "kind": "uncertain", "reason": str(error)[:2048]}


def main():
    raw = base64.b64decode(sys.argv[1], validate=True)
    if len(raw) > 524288:
        raise ValueError("request exceeds size limit")
    request = json.loads(raw)
    print(json.dumps(execute(request), separators=(",", ":"), allow_nan=False))


if __name__ == "__main__":
    main()
