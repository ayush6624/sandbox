/**
 * Destructive-but-self-cleaning /v1 fleet contract probe.
 *
 * SANDBOX_API_URL=http://gateway:9090 SANDBOX_API_KEY=... npm run v1-contract
 */
import assert from "node:assert/strict";

const baseUrl = (process.env.SANDBOX_API_URL ?? "").replace(/\/$/, "");
const apiKey = process.env.SANDBOX_API_KEY ?? "";
if (!baseUrl || !apiKey) {
  throw new Error("SANDBOX_API_URL and SANDBOX_API_KEY are required");
}

type Json = Record<string, any>;
const cleanupIds = new Set<string>();
let snapshotId = "";
const runId = `v1-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
const key = (action: string): string => `${runId}-${action}`;

async function request(
  method: string,
  path: string,
  body?: unknown,
  idempotencyKey?: string,
): Promise<{ response: Response; json: any }> {
  const headers: Record<string, string> = {
    Authorization: `Bearer ${apiKey}`,
  };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
  const response = await fetch(`${baseUrl}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  let json: any = undefined;
  if (text) {
    try {
      json = JSON.parse(text);
    } catch {
      throw new Error(`${method} ${path}: invalid JSON (${response.status}): ${text}`);
    }
  }
  return { response, json };
}

function expectStatus(actual: Response, expected: number, body?: unknown): void {
  assert.equal(actual.status, expected, `status ${actual.status}, body=${JSON.stringify(body)}`);
  assert.ok(actual.headers.get("x-request-id"), "X-Request-Id missing");
}

async function deleteSandbox(id: string): Promise<void> {
  const { response, json } = await request("DELETE", `/v1/sandboxes/${id}`, undefined, key(`cleanup-${id}`));
  assert.ok(response.status === 204 || response.status === 404, JSON.stringify(json));
  cleanupIds.delete(id);
}

async function main(): Promise<void> {
  const missingKey = await request("POST", "/v1/sandboxes", {});
  expectStatus(missingKey.response, 400, missingKey.json);
  assert.match(missingKey.response.headers.get("content-type") ?? "", /^application\/problem\+json/);
  assert.equal(missingKey.json.code, "idempotency_key_required");
  assert.equal(missingKey.json.request_id, missingKey.response.headers.get("x-request-id"));

  const templates = await request("GET", "/v1/templates?page_size=1");
  expectStatus(templates.response, 200, templates.json);
  assert.equal(templates.json.templates[0].id, "default");

  const createBody = {
    name: "v1-contract-source",
    source: { type: "template", id: "default" },
    lifecycle: { ttl_seconds: 900, idle_timeout_seconds: 600 },
    metadata: { probe: "v1-contract", run_id: runId, role: "source" },
  };
  const created = await request("POST", "/v1/sandboxes", createBody, key("create"));
  expectStatus(created.response, 201, created.json);
  const source = created.json as Json;
  cleanupIds.add(source.id);
  assert.equal(source.source.type, "template");
  assert.equal(source.metadata.probe, "v1-contract");
  for (const internal of ["pid", "vm_id", "socket_path", "tap_device", "guest_ip", "rootfs_path", "host_addr"]) {
    assert.ok(!(internal in source), `public sandbox leaked ${internal}`);
  }

  const replay = await request("POST", "/v1/sandboxes", createBody, key("create"));
  expectStatus(replay.response, 201, replay.json);
  assert.equal(replay.json.id, source.id);
  assert.equal(replay.response.headers.get("idempotency-replayed"), "true");

  const filtered = await request("GET", `/v1/sandboxes?page_size=1&source_type=template&metadata.run_id=${runId}`);
  expectStatus(filtered.response, 200, filtered.json);
  assert.ok(filtered.json.sandboxes.some((sandbox: Json) => sandbox.id === source.id));

  const patched = await request(
    "PATCH",
    `/v1/sandboxes/${source.id}`,
    { name: "v1-contract-renamed", metadata: { probe: "v1-contract", run_id: runId, phase: "patched" } },
    key("patch"),
  );
  expectStatus(patched.response, 200, patched.json);
  assert.equal(patched.json.name, "v1-contract-renamed");
  assert.equal(patched.json.metadata.phase, "patched");

  const port = await request(
    "POST",
    `/v1/sandboxes/${source.id}/port-forwards`,
    { guest_port: 3000 },
    key("port"),
  );
  expectStatus(port.response, 201, port.json);
  assert.equal(port.json.sandbox_id, source.id);
  assert.equal(port.json.status, "active");

  const paused = await request("POST", `/v1/sandboxes/${source.id}:pause`, undefined, key("pause"));
  expectStatus(paused.response, 200, paused.json);
  assert.equal(paused.json.status, "paused");
  const resumed = await request("POST", `/v1/sandboxes/${source.id}:resume`, undefined, key("resume"));
  expectStatus(resumed.response, 200, resumed.json);
  assert.equal(resumed.json.status, "running");

  const snapshot = await request(
    "POST",
    `/v1/sandboxes/${source.id}/snapshots`,
    { name: "v1-contract-snapshot", retention_seconds: 900 },
    key("snapshot"),
  );
  expectStatus(snapshot.response, 201, snapshot.json);
  snapshotId = snapshot.json.id;
  assert.equal(snapshot.json.source_sandbox_id, source.id);
  assert.ok(["local", "durable"].includes(snapshot.json.state));
  assert.ok(snapshot.json.expires_at);

  const batch = await request(
    "POST",
    "/v1/sandbox-batches",
    {
      count: 2,
      sandbox: {
        source: { type: "snapshot", id: snapshotId },
        lifecycle: { ttl_seconds: 900 },
        metadata: { probe: "v1-contract", run_id: runId, role: "batch" },
      },
      max_parallelism: 2,
    },
    key("batch"),
  );
  expectStatus(batch.response, 202, batch.json);
  assert.equal(batch.json.requested, 2);

  let operation: Json = batch.json;
  for (let attempt = 0; attempt < 120 && !operation.completed_at; attempt++) {
    await new Promise((resolve) => setTimeout(resolve, 500));
    const polled = await request("GET", `/v1/operations/${operation.id}`);
    expectStatus(polled.response, 200, polled.json);
    operation = polled.json;
  }
  assert.ok(operation.completed_at, "batch operation did not complete");
  assert.equal(operation.results.length, 2);
  assert.equal(operation.succeeded + operation.failed, 2);
  for (const item of operation.results) {
    assert.ok(Number.isInteger(item.index));
    if (item.sandbox) cleanupIds.add(item.sandbox.id);
    else assert.ok(item.error?.code);
  }
  assert.equal(operation.failed, 0, JSON.stringify(operation.results));

  const inUse = await request("DELETE", `/v1/snapshots/${snapshotId}`, undefined, key("snapshot-in-use"));
  expectStatus(inUse.response, 409, inUse.json);
  assert.equal(inUse.json.code, "conflict");

  for (const id of [...cleanupIds].filter((id) => id !== source.id)) await deleteSandbox(id);
  const deletedSnapshot = await request(
    "DELETE",
    `/v1/snapshots/${snapshotId}`,
    undefined,
    key("snapshot-delete"),
  );
  expectStatus(deletedSnapshot.response, 204, deletedSnapshot.json);
  snapshotId = "";
  await deleteSandbox(source.id);
  console.log("v1 contract probe passed");
}

main().catch(async (error) => {
  console.error(error);
  for (const id of [...cleanupIds]) {
    try {
      await deleteSandbox(id);
    } catch (cleanupError) {
      console.error(`cleanup sandbox ${id}:`, cleanupError);
    }
  }
  if (snapshotId) {
    try {
      await request("DELETE", `/v1/snapshots/${snapshotId}`, undefined, key(`cleanup-snapshot-${snapshotId}`));
    } catch (cleanupError) {
      console.error(`cleanup snapshot ${snapshotId}:`, cleanupError);
    }
  }
  process.exitCode = 1;
});
