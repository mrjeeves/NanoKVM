const endpoint = process.argv[2] ?? "ws://127.0.0.1:47000/api/storage/remote-media";
const session = process.argv[3] ?? `smoke-${Date.now()}`;
const size = Number(process.argv[4] ?? 4 * 1024 * 1024 + 137);
const socket = new WebSocket(endpoint);

socket.binaryType = "arraybuffer";
socket.addEventListener("open", () => {
  console.log("opened");
  socket.send(JSON.stringify({
    session,
    name: "allmystuff-smoke.img",
    size,
    cdrom: false,
    source: "remote-smoke-test",
    label: "AllMyStuff smoke test",
  }));
});

socket.addEventListener("message", ({ data }) => {
  const text = typeof data === "string" ? data : new TextDecoder().decode(data);
  const request = JSON.parse(text);
  if (request.kind !== "read") {
    console.log(text);
    return;
  }

  const reply = new Uint8Array(8 + request.length);
  new DataView(reply.buffer).setBigUint64(0, BigInt(request.id), false);
  for (let index = 0; index < request.length; index += 1) {
    reply[8 + index] = (request.offset + index) % 251;
  }
  socket.send(reply);
});

socket.addEventListener("error", (event) => {
  console.error("remote-media WebSocket error", event.error ?? event.message ?? event.type);
  process.exitCode = 1;
});
socket.addEventListener("close", ({ code, reason }) => {
  console.log(`closed ${code} ${reason}`);
  process.exit();
});

process.on("SIGINT", () => socket.close(1000, "smoke test complete"));
