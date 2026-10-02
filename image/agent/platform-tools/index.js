/**
 * platform-tools — the root agent's only path to the platform.
 *
 * The agent asks for intent ("spawn an agent to do X"); it never sees a
 * namespace, a pod, or a Role. Every tool here is a thin call to the workspace
 * broker, which derives the workspace from the capability token and builds the
 * Kubernetes object itself (scarab/docs/handoff.md §6.1, §6.3).
 *
 * This extension is loaded only for the root agent. Workers deliberately do not
 * get it: recursive delegation goes through the root agent (§8.3).
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool } from "@earendil-works/pi-coding-agent";
import { BrokerError, callBroker, resultRoot, truncate } from "./broker.js";

/** A model-facing text result with no structured details. */
function textResult(text) {
  return { content: [{ type: "text", text }], details: undefined };
}

/**
 * Errors reach the model as the broker's own agent-safe message. A BrokerError
 * already carries a stable code and a non-leaking message; anything else is
 * collapsed, because an internal error can name resources the agent has no
 * business seeing.
 */
function describe(error) {
  if (error instanceof BrokerError) return error.message;
  return "the broker request failed";
}

const spawnAgent = defineTool({
  name: "agents_spawn",
  label: "Spawn agent",
  description:
    "Spawn a worker agent in this workspace to carry out a task in parallel. " +
    "The worker shares /workspace and writes its result to " +
    "<SCARAB_RESULT_ROOT>/<agentId>/result.md when it finishes. Returns an agent id. " +
    "Use agents_list to follow progress and agents_logs to read output.",
  parameters: Type.Object({
    task: Type.String({ description: "What the worker should do. Be specific and self-contained." }),
    modelProfile: Type.Optional(Type.String({ description: "Model profile hint, e.g. \"coding\"." })),
    cpu: Type.Optional(Type.String({ description: "CPU limit hint, e.g. \"300m\". Bounded by the workspace." })),
    memory: Type.Optional(Type.String({ description: "Memory limit hint, e.g. \"512Mi\". Bounded by the workspace." })),
    timeoutSeconds: Type.Optional(Type.Number({
      description:
        "How long the worker may run, in seconds. Minimum 60, default 1800. A few " +
        "seconds is too short for a worker to finish even one model call.",
    })),
  }),

  async execute(_toolCallId, params, signal) {
    const body = { task: params.task };
    if (params.modelProfile) body.modelProfile = params.modelProfile;
    if (params.cpu || params.memory) {
      body.resources = {};
      if (params.cpu) body.resources.cpu = params.cpu;
      if (params.memory) body.resources.memory = params.memory;
    }
    if (params.timeoutSeconds) body.timeoutSeconds = params.timeoutSeconds;

    try {
      const { agentId } = await callBroker("/agents", { method: "POST", body, signal });
      return textResult(
        `Spawned agent ${agentId}. It will write its result to ` +
          `${resultRoot()}/${agentId}/result.md when it finishes.`,
      );
    } catch (error) {
      throw new Error(`agents_spawn failed: ${describe(error)}`, { cause: error });
    }
  },
});

const listAgents = defineTool({
  name: "agents_list",
  label: "List agents",
  description: "List the worker agents in this workspace and their state.",
  parameters: Type.Object({}),

  async execute(_toolCallId, _params, signal) {
    try {
      const agents = await callBroker("/agents", { signal });
      if (!agents || agents.length === 0) return textResult("No agents have been spawned.");
      const lines = agents.map((agent) => {
        const task = agent.task ? `  ${truncate(agent.task, 200)}` : "";
        return `${agent.id}  ${agent.state}${task}`;
      });
      return textResult(lines.join("\n"));
    } catch (error) {
      throw new Error(`agents_list failed: ${describe(error)}`, { cause: error });
    }
  },
});

const agentLogs = defineTool({
  name: "agents_logs",
  label: "Agent logs",
  description: "Read a worker agent's output. Use the id returned by agents_spawn or agents_list.",
  parameters: Type.Object({
    agentId: Type.String({ description: "The agent id." }),
  }),

  async execute(_toolCallId, params, signal) {
    try {
      const logs = await callBroker(`/agents/${encodeURIComponent(params.agentId)}/logs`, {
        signal,
        parse: "text",
      });
      return textResult(truncate(logs ?? "(no output yet)"));
    } catch (error) {
      throw new Error(`agents_logs failed: ${describe(error)}`, { cause: error });
    }
  },
});

const stopAgent = defineTool({
  name: "agents_stop",
  label: "Stop agent",
  description: "Stop a worker agent and free its share of the workspace budget.",
  parameters: Type.Object({
    agentId: Type.String({ description: "The agent id." }),
  }),

  async execute(_toolCallId, params, signal) {
    try {
      await callBroker(`/agents/${encodeURIComponent(params.agentId)}`, { method: "DELETE", signal });
      return textResult(`Stopped agent ${params.agentId}.`);
    } catch (error) {
      throw new Error(`agents_stop failed: ${describe(error)}`, { cause: error });
    }
  },
});

export default function platformTools(pi) {
  const tools = [spawnAgent, listAgents, agentLogs, stopAgent];
  for (const tool of tools) pi.registerTool(tool);

  // stderr is diagnostics, never protocol, and the bridge drains it. Emitting one
  // line lets the image smoke test prove the tools registered rather than merely
  // that the module parsed.
  console.error(`platform-tools: registered ${tools.map((tool) => tool.name).join(", ")}`);
}
