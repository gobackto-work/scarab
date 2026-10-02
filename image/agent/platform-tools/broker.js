// Broker client for the platform-tools extension.
//
// This module is deliberately free of any Pi dependency, so it can be unit
// tested with plain `node --test`. It is the only place that knows the broker's
// URL, the capability token, and the broker's error shape.
//
// Two things it must never do: log or return the capability token, and turn a
// broker error into anything other than the broker's own agent-safe message.

/** An error the agent is allowed to see. */
export class BrokerError extends Error {
  constructor(message, { code, status } = {}) {
    super(message);
    this.name = "BrokerError";
    this.code = code;
    this.status = status;
  }
}

// stripTrailingSlashes avoids a regular expression: sonarjs flags `/\/+$/` for
// super-linear backtracking, and a loop is clearer about what it does.
function stripTrailingSlashes(value) {
  let end = value.length;
  while (end > 0 && value[end - 1] === "/") end--;
  return value.slice(0, end);
}

/**
 * The broker's base URL, from the environment pestilence sets on the pod.
 *
 * When the broker is on https, something has to have told this process to trust its
 * certificate. That is NODE_EXTRA_CA_CERTS, which the bridge sets from
 * SCARAB_BROKER_CA when it starts Pi. Saying so here turns that misconfiguration
 * into an error which names it, instead of a handshake failure that reads like a
 * network problem.
 */
export function brokerUrl(env = process.env) {
  const raw = env.SCARAB_BROKER_URL;
  if (!raw) throw new BrokerError("SCARAB_BROKER_URL is not set");
  const url = stripTrailingSlashes(raw);
  if (url.startsWith("https:") && !env.NODE_EXTRA_CA_CERTS && !env.SCARAB_BROKER_CA) {
    throw new BrokerError(
      "the broker URL is https but neither SCARAB_BROKER_CA nor NODE_EXTRA_CA_CERTS is set, so its certificate cannot be trusted",
    );
  }
  return url;
}

/** Where workers write their results, on the shared workspace volume. */
export function resultRoot(env = process.env) {
  return stripTrailingSlashes(env.SCARAB_RESULT_ROOT || "/workspace/.agents");
}

/**
 * Read the workspace capability token.
 *
 * It is read per call rather than cached, so a rotated token is picked up
 * without restarting Pi. The token is never logged and never included in an
 * error.
 */
export async function readToken(env = process.env) {
  const path = env.SCARAB_TOKEN_PATH;
  if (!path) throw new BrokerError("SCARAB_TOKEN_PATH is not set");
  const { readFile } = await import("node:fs/promises");
  let token;
  try {
    token = (await readFile(path, "utf8")).trim();
  } catch {
    // The path is not secret; the token is. Never echo the file's contents.
    throw new BrokerError("capability token is not available");
  }
  if (!token) throw new BrokerError("capability token is empty");
  return token;
}

/**
 * Call the broker, deriving the workspace from the token (never from an
 * argument). Returns parsed JSON, text, or undefined for 204.
 */
export async function callBroker(
  path,
  { method = "GET", body, signal, env = process.env, fetchImpl = fetch, parse = "json" } = {},
) {
  const token = await readToken(env);
  const headers = { authorization: `Bearer ${token}` };
  if (body !== undefined) headers["content-type"] = "application/json";

  const response = await fetchImpl(`${brokerUrl(env)}${path}`, {
    method,
    headers,
    signal,
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (!response.ok) {
    throw await toBrokerError(response);
  }
  if (response.status === 204) return undefined;

  const text = await response.text();
  if (text === "") return undefined;
  return parse === "text" ? text : JSON.parse(text);
}

/** Turn a broker error response into an agent-safe BrokerError. */
async function toBrokerError(response) {
  let code;
  let message;
  try {
    const payload = await response.json();
    code = payload?.code;
    message = payload?.message;
  } catch {
    // Not JSON; fall through to a generic message rather than echoing a body.
  }
  return new BrokerError(message || `broker request failed (${response.status})`, {
    code,
    status: response.status,
  });
}

/** Truncate a model-facing string, telling the model where the rest lives. */
export function truncate(text, limit = 64 * 1024) {
  if (text.length <= limit) return text;
  return `${text.slice(0, limit)}\n\n[truncated: ${text.length - limit} more characters]`;
}
