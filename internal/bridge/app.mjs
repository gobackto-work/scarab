// Pure rendering logic for the workspace page.
//
// No DOM and no Pi dependency, so it is unit-tested with plain `node --test`. The
// page keeps only the DOM plumbing.
//
// The shape here is driven by the lint thresholds: `renderRecord` and
// `renderHistory` were single large functions (cyclomatic 34 and 18, cognitive 36
// and 43) and are now dispatch tables of small handlers. That is not cosmetic --
// each record type's rendering is now separately readable, and the page's
// behaviour is the union of things small enough to check one at a time.

/** The text of a Pi message, whose `content` is a string or an array of blocks. */
export function messageText(message) {
  if (!message) return "";
  const content = message.content;
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .filter((block) => block && block.type === "text" && typeof block.text === "string")
    .map((block) => block.text)
    .join("");
}

/** The visible reasoning of a message, if any. */
export function thinkingText(message) {
  if (!message || !Array.isArray(message.content)) return "";
  return message.content
    .filter((block) => block && block.type === "thinking" && typeof block.thinking === "string")
    .map((block) => block.thinking)
    .join("");
}

/** The tool calls a message made. */
export function toolCalls(message) {
  if (!message || !Array.isArray(message.content)) return [];
  return message.content
    .filter((block) => block && block.type === "toolCall")
    .map((block) => ({ name: block.name, args: block.arguments }));
}

/** The text a tool result carries. */
export function toolResultText(message) {
  if (!message || !Array.isArray(message.content)) return "";
  return contentText(message.content);
}

function contentText(content) {
  return content
    .filter((block) => block && block.type === "text" && typeof block.text === "string")
    .map((block) => block.text)
    .join("");
}

/** Truncate a model-facing string, saying how much was dropped. */
export function truncate(text, limit = 2000) {
  const s = String(text ?? "");
  if (s.length <= limit) return s;
  return `${s.slice(0, limit)}… [${s.length - limit} more characters]`;
}

/** One line describing a tool call's arguments, bounded. */
export function describeToolCall(args) {
  if (args === undefined) return "";
  try {
    return truncate(JSON.stringify(args), 300);
  } catch {
    return "";
  }
}

// nestedMessage digs one level into a JSON-encoded message, when that is what it
// is. Providers nest their error JSON inside a JSON string, which is why this
// exists at all.
function nestedMessage(value) {
  try {
    const parsed = JSON.parse(value);
    const message = parsed?.error?.message;
    return typeof message === "string" ? String(message) : "";
  } catch {
    return "";
  }
}

/** Unwrap the nested JSON Pi puts in errorMessage, down to the provider's words. */
export function describeError(raw) {
  if (!raw) return "the model call failed";

  const fallback = String(raw).slice(0, 500);
  let outer;
  try {
    outer = JSON.parse(raw);
  } catch {
    return fallback;
  }

  const inner = outer?.error;
  if (!inner) return fallback;
  if (typeof inner.message === "string") {
    return nestedMessage(inner.message) || inner.message.slice(0, 500);
  }
  if (inner.code) return "error " + inner.code;
  return fallback;
}

// ---------------------------------------------------------------------------
// live records
// ---------------------------------------------------------------------------

function renderMessageStart(record) {
  if (record.message?.role !== "user") return null;
  const text = messageText(record.message);
  return text ? { text: "\n> " + text + "\n", cls: "user" } : null;
}

function renderMessageUpdate(record) {
  const event = record.assistantMessageEvent;
  if (!event) return null;
  // Reasoning is shown, dimmed: without it a thinking model looks stalled.
  if (event.type === "thinking_delta" && event.delta) {
    return { text: event.delta, cls: "think" };
  }
  if (event.type === "text_delta") return { text: event.delta ?? "", cls: "" };
  return null;
}

function renderMessageEnd(record) {
  const message = record.message;
  if (message?.role !== "assistant") return null;
  // A failed model call produces no text, so without this the page looks dead.
  if (message.stopReason === "error") {
    return { text: "\n[model error] " + describeError(message.errorMessage) + "\n", cls: "err" };
  }
  if (message.stopReason === "aborted") return { text: "\n[stopped]\n", cls: "meta" };
  return null;
}

function renderAutoRetry(record) {
  const delay = Math.round((record.delayMs || 0) / 1000);
  const attempt = record.attempt ? ` (attempt ${record.attempt}/${record.maxAttempts ?? "?"})` : "";
  const reason = describeError(record.errorMessage);
  return { text: `\n[retrying in ${delay}s${attempt}: ${reason}]\n`, cls: "err" };
}

function renderToolStart(record) {
  const args = describeToolCall(record.args);
  return { text: "\n[tool] " + record.toolName + (args ? " " + args : "") + "\n", cls: "tool" };
}

function renderToolEnd(record) {
  const body = truncate(toolResultText(record.result), 1200);
  if (!body) return null;
  const label = `\n[tool result: ${record.toolName}${record.isError ? " (error)" : ""}]\n`;
  return { text: label + body + "\n", cls: record.isError ? "err" : "toolresult" };
}

function renderModelKeyOk(record) {
  const model = record.model ? " / " + record.model : " (Pi default model)";
  return { text: `\n[using ${record.provider}${model}]\n`, cls: "meta" };
}

const RECORD_RENDERERS = {
  message_start: renderMessageStart,
  message_update: renderMessageUpdate,
  message_end: renderMessageEnd,
  auto_retry_start: renderAutoRetry,
  tool_execution_start: renderToolStart,
  tool_execution_end: renderToolEnd,
  bridge_error: (record) => ({ text: `\n[error] ${record.error}\n`, cls: "err" }),
  model_key_ok: renderModelKeyOk,
  agent_settled: () => ({ text: "\n", cls: "meta" }),
};

/**
 * Turn one bridge record into a display instruction, or null when the record needs
 * no rendering. `cls` maps to a CSS class in the page.
 */
export function renderRecord(record) {
  if (!record || typeof record.type !== "string") return null;
  const render = RECORD_RENDERERS[record.type];
  return render ? render(record) : null;
}

// ---------------------------------------------------------------------------
// history
// ---------------------------------------------------------------------------

function historyUser(message) {
  const text = messageText(message);
  return text ? [{ text: "\n> " + text + "\n", cls: "user" }] : [];
}

function historyAssistant(message) {
  const out = [];

  const thinking = thinkingText(message);
  if (thinking) out.push({ text: "\n" + thinking + "\n", cls: "think" });

  for (const call of toolCalls(message)) {
    const args = describeToolCall(call.args);
    out.push({ text: "\n[tool] " + call.name + (args ? " " + args : "") + "\n", cls: "tool" });
  }

  const text = messageText(message);
  if (text) out.push({ text, cls: "" });

  if (message.stopReason === "error") {
    out.push({ text: "\n[model error] " + describeError(message.errorMessage) + "\n", cls: "err" });
  }
  if (message.stopReason === "aborted") out.push({ text: "\n[stopped]\n", cls: "meta" });
  return out;
}

function historyToolResult(message) {
  const body = truncate(toolResultText(message), 1200);
  if (!body) return [];
  const label = `\n[tool result: ${message.toolName}${message.isError ? " (error)" : ""}]\n`;
  return [{ text: label + body + "\n", cls: message.isError ? "err" : "toolresult" }];
}

const HISTORY_RENDERERS = {
  user: historyUser,
  assistant: historyAssistant,
  toolResult: historyToolResult,
};

/**
 * Replay a conversation, so a reloaded page shows the history instead of a blank
 * transcript. Returns a list of display instructions in order.
 */
export function renderHistory(messages) {
  if (!Array.isArray(messages)) return [];
  const out = [];
  for (const message of messages) {
    const render = message && HISTORY_RENDERERS[message.role];
    if (render) out.push(...render(message));
  }
  return out;
}

// ---------------------------------------------------------------------------
// model catalog
// ---------------------------------------------------------------------------

/**
 * Group Pi's model catalog by provider, for the page's provider and model inputs.
 *
 * Data-driven on purpose: the hardcoded provider list omitted every provider that
 * authenticates with a token, which is most of them.
 */
export function catalog(models) {
  if (!Array.isArray(models)) return { providers: [], modelsByProvider: {} };

  const modelsByProvider = {};
  for (const model of models) {
    if (!model || typeof model.provider !== "string" || typeof model.id !== "string") continue;
    const existing = modelsByProvider[model.provider];
    if (existing) existing.push(model.id);
    else modelsByProvider[model.provider] = [model.id];
  }
  for (const ids of Object.values(modelsByProvider)) ids.sort();
  return { providers: Object.keys(modelsByProvider).sort(), modelsByProvider };
}

/**
 * The provider ids the pinned Pi version knows.
 *
 * Extracted from Pi's own bundle rather than hand-written, because a hand-written
 * list is what caused the bug: nine providers were offered and every token-auth
 * provider was missing. Refresh it when Pi is upgraded, with the command in
 * docs/verification.md.
 *
 * It is a convenience list, not a gate: the bridge accepts any provider id and Pi
 * is the authority, so an id missing here can still be typed.
 */
export const KNOWN_PROVIDERS = [
  "amazon-bedrock", "ant-ling", "anthropic", "azure-openai-responses", "baseten", "cerebras",
  "cloudflare-ai-gateway", "cloudflare-workers-ai", "deepseek", "fireworks", "github-copilot",
  "google", "google-vertex", "groq", "huggingface", "kimi-coding", "meta", "minimax", "minimax-cn",
  "mistral", "moonshotai", "moonshotai-cn", "nvidia", "openai", "openai-codex", "opencode",
  "opencode-go", "openrouter", "qwen-token-plan", "qwen-token-plan-cn",
  "qwen-token-plan-individual", "radius", "together", "vercel-ai-gateway", "xai", "xiaomi",
  "xiaomi-token-plan-ams", "xiaomi-token-plan-cn", "xiaomi-token-plan-sgp", "zai", "zai-coding-cn",
];

/** Every provider to offer: those already configured first, then the rest. */
export function providerOptions(configured, known = KNOWN_PROVIDERS) {
  const all = new Set([...(configured || []), ...known]);
  return [...all].sort();
}

/**
 * Whether Pi's reported model is a real selection.
 *
 * `get_state` returns a placeholder with provider and id `"unknown"` when nothing
 * is selected. Writing that into the form looks like a real choice, and it matches
 * no entry in the provider list — so the list reads as empty and the field reads as
 * broken.
 */
export function isRealModel(model) {
  const real = (value) => typeof value === "string" && value !== "" && value !== "unknown";
  return Boolean(model) && real(model.provider) && real(model.id);
}
