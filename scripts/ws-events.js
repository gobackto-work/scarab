// Dumps every bridge event for one prompt, so the shape of the user message can be
// seen rather than assumed.

const ws = new WebSocket("ws://127.0.0.1:8000/ws");
let sent = false;

ws.onmessage = (event) => {
  const record = JSON.parse(event.data);

  if (record.type === "bridge") {
    if (!sent) {
      sent = true;
      ws.send(JSON.stringify({ type: "prompt", message: "reply with the single word: ok" }));
    }
    return;
  }
  if (record.type === "bridge_error") {
    console.log("BRIDGE ERROR:", record.error);
    return;
  }

  console.log(record.type.padEnd(22), JSON.stringify(record).slice(0, 260));

  if (record.type === "agent_settled") {
    ws.close();
    process.exit(0);
  }
};

ws.onerror = () => {
  console.log("websocket error");
  process.exit(1);
};

setTimeout(() => {
  console.log("[timeout]");
  process.exit(2);
}, 60000);
