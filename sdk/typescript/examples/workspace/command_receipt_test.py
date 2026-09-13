import base64
import hashlib
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import tempfile
import time
import unittest

HELPER = Path(__file__).with_name("command-receipt.py")


class CommandReceiptTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.env = {**os.environ, "WORKSPACE_RECEIPT_ROOT": str(self.root / "receipts")}

    def command(self, script, sandbox="sandbox-1"):
        request = {
            "version": 1, "run_id": "run-1", "index": 0,
            "sandbox_id": sandbox, "script": script,
            "script_sha256": hashlib.sha256(script.encode()).hexdigest(),
        }
        encoded = base64.b64encode(json.dumps(request).encode()).decode()
        return [sys.executable, str(HELPER), encoded]

    def run_command(self, script, sandbox="sandbox-1"):
        result = subprocess.run(self.command(script, sandbox), env=self.env, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def start_command(self, script):
        child = subprocess.Popen(
            self.command(script), env=self.env, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, start_new_session=True,
        )

        def stop():
            if child.poll() is None:
                os.killpg(child.pid, signal.SIGKILL)
            child.communicate(timeout=5)

        self.addCleanup(stop)
        return child

    def wait_file(self, path):
        deadline = time.monotonic() + 5
        while not path.exists():
            if time.monotonic() >= deadline:
                self.fail("command did not reach its effect barrier")
            time.sleep(0.01)

    def test_lost_response_replays_result_without_repeating_effect(self):
        effects = self.root / "effects"
        script = f"printf 'once\\n' >> {shlex.quote(str(effects))}; printf answer; exit 7"
        first = subprocess.run(self.command(script), env=self.env, stdout=subprocess.DEVNULL, timeout=10)
        self.assertEqual(first.returncode, 0)
        receipt = self.run_command(script)
        self.assertEqual(receipt["kind"], "completed")
        self.assertEqual(receipt["result"]["exitCode"], 7)
        self.assertEqual(base64.b64decode(receipt["result"]["stdout"]["base64"]), b"answer")
        self.assertEqual(self.run_command(script), receipt)
        self.assertEqual(effects.read_text(), "once\n")

    def test_binary_output_is_capped_while_both_streams_drain(self):
        program = "import os; os.write(1, b'\\xff' * 1000000); os.write(2, b'\\x00' * 2000000)"
        receipt = self.run_command(f"{shlex.quote(sys.executable)} -c {shlex.quote(program)}")
        self.assertEqual(receipt["kind"], "completed")
        self.assertEqual(receipt["result"]["exitCode"], 0)
        for name, byte in (("stdout", b"\xff"), ("stderr", b"\x00")):
            output = receipt["result"][name]
            data = base64.b64decode(output["base64"])
            self.assertEqual(data, byte * 65536)
            self.assertEqual(output["bytes"], 65536)
            self.assertEqual(output["sha256"], hashlib.sha256(data).hexdigest())
            self.assertTrue(output["truncated"])
            self.assertEqual((self.root / "receipts/run-1/0" / (name + ".bin")).stat().st_size, 65536)

    def test_killed_wrapper_leaves_uncertain_reservation(self):
        effect = self.root / "effect"
        script = f"printf once > {shlex.quote(str(effect))}; sleep 60"
        child = self.start_command(script)
        self.wait_file(effect)
        os.killpg(child.pid, signal.SIGKILL)
        child.communicate(timeout=5)
        receipt = self.run_command(script)
        self.assertEqual(receipt["kind"], "uncertain")
        self.assertNotIn("result", receipt)
        self.assertEqual(effect.read_text(), "once")
        self.assertFalse((self.root / "receipts/run-1/0/completed.json").exists())

    def test_parallel_claim_executes_once(self):
        effect, release = self.root / "effect", self.root / "release"
        script = (
            f"printf 'once\\n' >> {shlex.quote(str(effect))}; "
            f"while ! test -e {shlex.quote(str(release))}; do sleep 0.02; done; printf done"
        )
        child = self.start_command(script)
        self.wait_file(effect)
        self.assertEqual(self.run_command(script)["kind"], "uncertain")
        release.touch()
        stdout, stderr = child.communicate(timeout=5)
        self.assertEqual(child.returncode, 0, stderr)
        first = json.loads(stdout)
        self.assertEqual(first["kind"], "completed")
        self.assertEqual(self.run_command(script), first)
        self.assertEqual(effect.read_text(), "once\n")

    def test_empty_or_malformed_reservation_never_executes(self):
        directory = self.root / "receipts/run-1/0"
        directory.mkdir(parents=True)
        effect = self.root / "effect"
        script = f"touch {shlex.quote(str(effect))}"
        self.assertEqual(self.run_command(script)["kind"], "uncertain")
        (directory / "identity.json").write_text("{")
        self.assertEqual(self.run_command(script)["kind"], "uncertain")
        self.assertFalse(effect.exists())

    def test_identity_and_corrupt_output_never_reexecute(self):
        effect = self.root / "effect"
        script = f"printf 'once\\n' >> {shlex.quote(str(effect))}; printf original"
        self.assertEqual(self.run_command(script)["kind"], "completed")
        self.assertEqual(self.run_command(script, "different-sandbox")["kind"], "uncertain")
        self.assertEqual(self.run_command(script + "; printf changed")["kind"], "uncertain")
        (self.root / "receipts/run-1/0/stdout.bin").write_bytes(b"corrupt")
        self.assertEqual(self.run_command(script)["kind"], "uncertain")
        self.assertEqual(effect.read_text(), "once\n")

    def test_typescript_bridge_and_result_parser(self):
        program = r"""
import assert from 'node:assert/strict';
import { execSync } from 'node:child_process';
import { executeReceiptedCommand, parseCompletedCommandResult } from './examples/workspace/command-receipt.ts';
let change = value => value;
const sandbox = { id: 'sandbox-bridge', commands: { async run(command) {
  const raw = execSync(command, { env: process.env, encoding: 'utf8', maxBuffer: 1048576 });
  return { stdout: JSON.stringify(change(JSON.parse(raw))), stderr: '', exitCode: 0, durationMs: 1 };
} } };
const input = { runId: 'bridge-run', index: 0, script: "printf '%s' 'quote \" and $HOME and `literal`'; exit 9", timeoutMs: 10000 };
const receipt = await executeReceiptedCommand(sandbox, input);
assert.equal(receipt.kind, 'completed');
assert.equal(receipt.result.exitCode, 9);
assert.equal(Buffer.from(receipt.result.stdout.base64, 'base64').toString(), 'quote " and $HOME and `literal`');
assert.deepEqual(parseCompletedCommandResult(receipt.result), receipt.result);
for (const corrupt of [
  v => { v.stdout.sha256 = '0'.repeat(64); },
  v => { v.stdout.base64 += '\n'; },
  v => { v.stdout.bytes += 1; },
  v => { v.stdout.bytes = 65537; },
  v => { v.stdout.truncated = true; },
  v => { v.stderr.truncated = 'false'; },
  v => { v.durationMs = Number.NaN; },
  v => { v.durationMs = -1; },
  v => { v.exitCode = 0.5; },
]) {
  const value = structuredClone(receipt.result);
  corrupt(value);
  assert.throws(() => parseCompletedCommandResult(value));
}
for (const field of ['run_id', 'index', 'sandbox_id', 'script_sha256', 'version']) {
  change = value => ({ ...value, [field]: 'wrong' });
  await assert.rejects(executeReceiptedCommand(sandbox, input), /identity mismatch/);
}
"""
        result = subprocess.run(
            ["node", "--import", "tsx", "--input-type=module", "-e", program],
            cwd=HELPER.resolve().parents[2], env=self.env, capture_output=True, timeout=20,
        )
        self.assertEqual(result.returncode, 0, result.stderr.decode())

    def test_command_inherits_wrapper_process_group(self):
        program = "import os; print(os.getpgrp())"
        child = self.start_command(f"{shlex.quote(sys.executable)} -c {shlex.quote(program)}")
        stdout, stderr = child.communicate(timeout=5)
        self.assertEqual(child.returncode, 0, stderr)
        receipt = json.loads(stdout)
        group = int(base64.b64decode(receipt["result"]["stdout"]["base64"]))
        self.assertEqual(group, child.pid)


if __name__ == "__main__":
    unittest.main()
