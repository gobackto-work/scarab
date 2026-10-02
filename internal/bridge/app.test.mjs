// Tests for the page's rendering logic, run with `node --test`.
//
// These exist because two bugs came from this logic living inline in the HTML and
// only being exercised by a human in a browser: a user message rendered blank
// because Pi sends `content` as an array of blocks, and a failed model call
// rendered as nothing at all.

import assert from "node:assert/strict";
import test from "node:test";

import {
  isRealModel,
  catalog,
  describeError,
  messageText,
  providerOptions,
  renderHistory,
  renderRecord,
  thinkingText,
  toolCalls,
} from "./app.mjs";

test("messageText handles string content", () => {
  assert.equal(messageText({ content: "hello" }), "hello");
});

test("messageText handles the array of blocks Pi actually sends", () => {
  const message = { role: "user", content: [{ type: "text", text: "reply with ok" }] };
  assert.equal(messageText(message), "reply with ok");
});

test("messageText joins multiple text blocks and ignores others", () => {
  const message = {
    content: [
      { type: "text", text: "one " },
      { type: "thinking", thinking: "ignored" },
      { type: "text", text: "two" },
    ],
  };
  assert.equal(messageText(message), "one two");
});

test("messageText is empty for missing or unknown content", () => {
  assert.equal(messageText(undefined), "");
  assert.equal(messageText({ content: 42 }), "");
});

// The regression: a user message must render, not produce an empty prompt line.
test("a user message renders its text", () => {
  const out = renderRecord({
    type: "message_start",
    message: { role: "user", content: [{ type: "text", text: "spawn an agent" }] },
  });
  assert.ok(out, "user message produced no output");
  assert.equal(out.cls, "user");
  assert.match(out.text, /spawn an agent/);
});

test("an assistant message_start renders nothing", () => {
  assert.equal(renderRecord({ type: "message_start", message: { role: "assistant", content: [] } }), null);
});

test("a text delta renders verbatim", () => {
  const out = renderRecord({
    type: "message_update",
    assistantMessageEvent: { type: "text_delta", delta: "hi" },
  });
  assert.equal(out.text, "hi");
  assert.equal(out.cls, "");
});

// The other regression: a failed model call must be visible.
test("a model error renders the provider's own words", () => {
  const out = renderRecord({
    type: "message_end",
    message: {
      role: "assistant",
      stopReason: "error",
      errorMessage: JSON.stringify({
        error: { message: JSON.stringify({ error: { code: 429, message: "quota exceeded" } }) },
      }),
    },
  });
  assert.ok(out, "model error produced no output");
  assert.equal(out.cls, "err");
  assert.match(out.text, /quota exceeded/);
});

test("a successful message_end renders nothing", () => {
  assert.equal(renderRecord({ type: "message_end", message: { role: "assistant", stopReason: "endTurn" } }), null);
});

test("auto_retry_start names the attempt and unwraps the error", () => {
  const out = renderRecord({
    type: "auto_retry_start",
    attempt: 1,
    maxAttempts: 3,
    delayMs: 2000,
    errorMessage: JSON.stringify({ error: { message: "overloaded" } }),
  });
  assert.match(out.text, /attempt 1\/3/);
  assert.match(out.text, /2s/);
  assert.match(out.text, /overloaded/);
});

test("describeError falls back rather than throwing", () => {
  assert.equal(describeError(undefined), "the model call failed");
  assert.equal(describeError("not json"), "not json");
  assert.equal(describeError(JSON.stringify({ error: { code: 503 } })), "error 503");
});

test("tools, bridge errors and settlement render", () => {
  assert.match(renderRecord({ type: "tool_execution_start", toolName: "bash" }).text, /\[tool\] bash/);
  assert.match(renderRecord({ type: "bridge_error", error: "pi is not running" }).text, /pi is not running/);
  assert.equal(renderRecord({ type: "agent_settled" }).cls, "meta");
});

test("unknown records render nothing", () => {
  assert.equal(renderRecord({ type: "usage_update" }), null);
  assert.equal(renderRecord(null), null);
  assert.equal(renderRecord({}), null);
});

// --- thinking, tools, history and the catalog ---------------------------------

test("thinking and tool calls are extracted from a message", () => {
  const message = {
    role: "assistant",
    content: [
      { type: "thinking", thinking: "weighing options" },
      { type: "toolCall", name: "bash", arguments: { command: "ls" } },
      { type: "text", text: "done" },
    ],
  };
  assert.equal(thinkingText(message), "weighing options");
  assert.deepEqual(toolCalls(message), [{ name: "bash", args: { command: "ls" } }]);
  assert.equal(messageText(message), "done");
});

// A thinking model looks stalled without its reasoning, so this must render.
test("a thinking delta renders dimmed", () => {
  const out = renderRecord({
    type: "message_update",
    assistantMessageEvent: { type: "thinking_delta", delta: "considering…" },
  });
  assert.equal(out.cls, "think");
  assert.equal(out.text, "considering…");
});

test("a tool call renders with its arguments", () => {
  const out = renderRecord({
    type: "tool_execution_start",
    toolName: "bash",
    args: { command: "npm install react" },
  });
  assert.equal(out.cls, "tool");
  assert.match(out.text, /npm install react/);
});

test("a tool result renders, and an error result is flagged", () => {
  const ok = renderRecord({
    type: "tool_execution_end",
    toolName: "bash",
    isError: false,
    result: { content: [{ type: "text", text: "total 0" }] },
  });
  assert.equal(ok.cls, "toolresult");
  assert.match(ok.text, /total 0/);

  const bad = renderRecord({
    type: "tool_execution_end",
    toolName: "bash",
    isError: true,
    result: { content: [{ type: "text", text: "command not found" }] },
  });
  assert.equal(bad.cls, "err");
  assert.match(bad.text, /error/);
});

test("an aborted turn says so", () => {
  const out = renderRecord({ type: "message_end", message: { role: "assistant", stopReason: "aborted" } });
  assert.equal(out.cls, "meta");
  assert.match(out.text, /stopped/);
});

// The reload case: the page must be able to rebuild the conversation.
test("history replays user, thinking, tools and text in order", () => {
  const messages = [
    { role: "user", content: [{ type: "text", text: "spawn an agent" }] },
    {
      role: "assistant",
      content: [
        { type: "thinking", thinking: "I should use the tool" },
        { type: "toolCall", name: "agents_spawn", arguments: { task: "x" } },
        { type: "text", text: "Spawned it." },
      ],
      stopReason: "stop",
    },
    { role: "toolResult", toolName: "agents_spawn", content: [{ type: "text", text: "agentId: abc" }], isError: false },
  ];

  const out = renderHistory(messages);
  assert.equal(out[0].cls, "user");
  assert.match(out[0].text, /spawn an agent/);

  const classes = out.map((item) => item.cls);
  assert.deepEqual(classes, ["user", "think", "tool", "", "toolresult"]);
  assert.match(out[4].text, /agentId: abc/);
});

test("history tolerates junk", () => {
  assert.deepEqual(renderHistory(undefined), []);
  assert.deepEqual(renderHistory([null, { role: "system", content: "x" }]), []);
});

// The hardcoded provider list omitted every token-auth provider; this is the fix.
test("the catalog groups models by provider", () => {
  const { providers, modelsByProvider } = catalog([
    { provider: "openrouter", id: "z/model" },
    { provider: "google", id: "gemini-3.8-flash" },
    { provider: "google", id: "gemini-2.5-flash" },
    { provider: "anthropic", id: "claude-sonnet-4-5" },
    { provider: "broken" },
  ]);
  assert.deepEqual(providers, ["anthropic", "google", "openrouter"]);
  assert.deepEqual(modelsByProvider.google, ["gemini-2.5-flash", "gemini-3.8-flash"]);
});

test("the catalog tolerates junk", () => {
  assert.deepEqual(catalog(undefined), { providers: [], modelsByProvider: {} });
  assert.deepEqual(catalog([null, 42]), { providers: [], modelsByProvider: {} });
});

// The bug: the offered list was nine providers, so a user could not even select
// the provider they had a token for.
test("provider options include configured providers and every known one", () => {
  const options = providerOptions(["openrouter"]);
  assert.ok(options.includes("openrouter"));
  assert.ok(options.includes("anthropic"));
  assert.ok(options.includes("moonshotai"), "a token-auth provider must be offered");
  assert.ok(options.length > 30, `expected the full list, got ${options.length}`);
  assert.deepEqual(options, [...options].sort());
});

test("provider options tolerate junk", () => {
  assert.ok(providerOptions(undefined).length > 30);
});

// get_state reports provider/id "unknown" when nothing is selected. Prefilling from
// that made the provider field read "unknown" and no chip match it, which looks
// like an empty list.
test("the unknown placeholder is not a real model", () => {
  assert.equal(isRealModel({ provider: "unknown", id: "unknown" }), false);
  assert.equal(isRealModel({ provider: "unknown", id: "gemini-flash-latest" }), false);
  assert.equal(isRealModel({ provider: "google", id: "unknown" }), false);
  assert.equal(isRealModel({ provider: "", id: "" }), false);
  assert.equal(isRealModel(undefined), false);
  assert.equal(isRealModel({ provider: "openrouter", id: "thinkingmachines/inkling:free" }), true);
});
