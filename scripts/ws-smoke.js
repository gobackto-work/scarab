// Drives the bridge's WebSocket exactly as the browser does, from inside the
// root-agent pod. Verifies the whole UI path: greeting, selecting a model without
// re-sending the key, a prompt, streamed text, and error reporting.

const url = "ws://127.0.0.1:8000/ws";
const model = process.env.TEST_MODEL || "gemini-flash-latest";

const ws = new WebSocket(url);
let prompted = false;

ws.onmessage = (event) => {
  const record = JSON.parse(event.data);

  if (record.type === "bridge") {
    console.log("greeting:", JSON.stringify(record));
    ws.send(JSON.stringify({ type: "model_key", provider: "google", apiKey: "", model }));
    return;
  }
  if (record.type === "model_key_ok") {
    console.log("model set:", JSON.stringify(record));
    prompted = true;
    setTimeout(() => {
      ws.send(JSON.stringify({ type: "prompt", message: "Reply with exactly: hello from the workspace" }));
    }, 5000);
    return;
  }
  if (record.type === "bridge_error") {
    console.log("BRIDGE ERROR:", record.error);
    return;
  }
  if (record.type === "message_update" && record.assistantMessageEvent?.type === "text_delta") {
    process.stdout.write(record.assistantMessageEvent.delta);
    return;
  }
  if (record.type === "message_end" && record.message?.stopReason === "error") {
    console.log("\nMODEL ERROR:", String(record.message.errorMessage).slice(0, 400));
    return;
  }
  if (record.type === "agent_settled") {
    console.log("\n[settled]");
    ws.close();
    process.exit(0);
  }
};

ws.onerror = () => {
  console.log("websocket error");
  process.exit(1);
};

setTimeout(() => {
  console.log(`\n[timeout; prompted=${prompted}]`);
  process.exit(2);
}, 90000);
