// Tests for the broker client, run with `node --test`.
//
// The client is where the token, the URL and the error mapping live, so this is
// where the extension is actually worth testing. It has no Pi dependency, which
// is why plain Node can load it.

import assert from "node:assert/strict";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { BrokerError, brokerUrl, callBroker, readToken, resultRoot, truncate } from "./broker.js";

const BROKER = "http://broker-demo.scarab.svc.cluster.local:8443";

async function tokenFile(contents) {
  const dir = await mkdtemp(join(tmpdir(), "scarab-tools-"));
  const path = join(dir, "token");
  await writeFile(path, contents);
  return path;
}

function env(overrides = {}) {
  return { SCARAB_BROKER_URL: BROKER, ...overrides };
}

test("brokerUrl trims trailing slashes and requires the variable", () => {
  assert.equal(brokerUrl({ SCARAB_BROKER_URL: `${BROKER}/` }), BROKER);
  assert.throws(() => brokerUrl({}), BrokerError);
});

// A TLS broker whose certificate this process was never told to trust fails the
// handshake, and that error says nothing about trust. These pin the version that
// does.
test("brokerUrl refuses https without a trust anchor", () => {
  const https = "https://broker-demo.scarab.svc.cluster.local:8443";

  assert.throws(() => brokerUrl({ SCARAB_BROKER_URL: https }), /cannot be trusted/);
  assert.equal(brokerUrl({ SCARAB_BROKER_URL: https, SCARAB_BROKER_CA: "/ca.crt" }), https);
  assert.equal(brokerUrl({ SCARAB_BROKER_URL: https, NODE_EXTRA_CA_CERTS: "/ca.crt" }), https);
  // Plaintext needs no anchor, and an unset CA is how that is expressed.
  assert.equal(brokerUrl({ SCARAB_BROKER_URL: BROKER }), BROKER);
});

test("resultRoot defaults to the workspace agents directory", () => {
  assert.equal(resultRoot({}), "/workspace/.agents");
  assert.equal(resultRoot({ SCARAB_RESULT_ROOT: "/workspace/.agents/" }), "/workspace/.agents");
});

test("readToken reads and trims the capability token", async () => {
  const path = await tokenFile("  header.payload.signature\n");
  assert.equal(await readToken({ SCARAB_TOKEN_PATH: path }), "header.payload.signature");
});

test("readToken requires the variable", async () => {
  await assert.rejects(() => readToken({}), BrokerError);
});

test("readToken does not echo the file when it is missing", async () => {
  await assert.rejects(
    () => readToken({ SCARAB_TOKEN_PATH: "/nonexistent/scarab/token" }),
    (error) => {
      assert.ok(error instanceof BrokerError);
      assert.doesNotMatch(error.message, /nonexistent/);
      return true;
    },
  );
});

test("readToken rejects an empty token", async () => {
  const path = await tokenFile("   \n");
  await assert.rejects(() => readToken({ SCARAB_TOKEN_PATH: path }), BrokerError);
});

test("callBroker sends the token and parses JSON", async () => {
  const path = await tokenFile("the-token");
  let seen;
  const fetchImpl = async (url, init) => {
    seen = { url, init };
    return new Response(JSON.stringify({ agentId: "abcd1234abcd1234" }), {
      status: 201,
      headers: { "content-type": "application/json" },
    });
  };

  const result = await callBroker("/agents", {
    method: "POST",
    body: { task: "inspect" },
    env: env({ SCARAB_TOKEN_PATH: path }),
    fetchImpl,
  });

  assert.deepEqual(result, { agentId: "abcd1234abcd1234" });
  assert.equal(seen.url, `${BROKER}/agents`);
  assert.equal(seen.init.method, "POST");
  assert.equal(seen.init.headers.authorization, "Bearer the-token");
  assert.equal(seen.init.headers["content-type"], "application/json");
  assert.equal(seen.init.body, JSON.stringify({ task: "inspect" }));
});

test("callBroker maps a broker error to an agent-safe BrokerError", async () => {
  const path = await tokenFile("the-token");
  const fetchImpl = async () =>
    new Response(JSON.stringify({ code: "quota_exceeded", message: "workspace memory quota reached (1Gi of 1Gi requested)" }), {
      status: 409,
      headers: { "content-type": "application/json" },
    });

  await assert.rejects(
    () => callBroker("/agents", { method: "POST", env: env({ SCARAB_TOKEN_PATH: path }), fetchImpl }),
    (error) => {
      assert.ok(error instanceof BrokerError);
      assert.equal(error.code, "quota_exceeded");
      assert.equal(error.status, 409);
      assert.match(error.message, /memory quota/);
      assert.doesNotMatch(error.message, /the-token/);
      return true;
    },
  );
});

test("callBroker does not echo a non-JSON error body", async () => {
  const path = await tokenFile("the-token");
  const fetchImpl = async () => new Response("<html>502 Bad Gateway</html>", { status: 502 });

  await assert.rejects(
    () => callBroker("/agents", { env: env({ SCARAB_TOKEN_PATH: path }), fetchImpl }),
    (error) => {
      assert.equal(error.message, "broker request failed (502)");
      return true;
    },
  );
});

test("callBroker returns undefined for 204", async () => {
  const path = await tokenFile("the-token");
  const fetchImpl = async () => new Response(null, { status: 204 });
  const result = await callBroker("/agents/x", {
    method: "DELETE",
    env: env({ SCARAB_TOKEN_PATH: path }),
    fetchImpl,
  });
  assert.equal(result, undefined);
});

test("callBroker can return text, for logs", async () => {
  const path = await tokenFile("the-token");
  const fetchImpl = async () => new Response("worker output", { status: 200 });
  const result = await callBroker("/agents/x/logs", {
    env: env({ SCARAB_TOKEN_PATH: path }),
    fetchImpl,
    parse: "text",
  });
  assert.equal(result, "worker output");
});

test("truncate keeps small text and marks large text", () => {
  assert.equal(truncate("short", 10), "short");
  const long = truncate("x".repeat(20), 10);
  assert.match(long, /^x{10}\n\n\[truncated: 10 more characters\]$/);
});
