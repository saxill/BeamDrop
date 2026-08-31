// Drives the real browser-side JS against a live webui.Serve instance over
// a real TLS WebSocket, using Node's native WebSocket client. Invoked by
// smoke_test.go as a child process; prints one JSON line per step to stdout
// so the Go test can assert on outcomes.
//
// This deliberately owns no protocol logic of its own. It used to
// reimplement offer/accept/chunk/done, which meant the smoke test proved
// that *this file* spoke the protocol correctly while app.js — the code an
// actual iPhone runs — did not. app.js never sent FILE_DONE on receive, so
// Go's SendFile blocked forever and every laptop→phone transfer hung, with
// a green smoke test the whole time. Everything below now goes through
// transfer.js, the same module app.js uses.
import { pairWithServer } from '../webui/static/pairing.js';
import { Transfers } from '../webui/static/transfer.js';

const [, , url, mode, arg1] = process.argv;

function log(obj) { console.log(JSON.stringify(obj)); }

const ws = new WebSocket(url);
ws.binaryType = 'arraybuffer';

// Node's native WebSocket (undici-backed) does not keep the event loop
// alive on its own — without a pending timer/interval, the process exits as
// soon as its own script finishes running, even while the socket is still
// open and messages are still expected. Every code path below terminates
// explicitly via process.exit(), so this interval only exists to stop that
// premature exit while we're waiting on the wire.
const keepAlive = setInterval(() => {}, 1 << 30);

function finish(code) {
  clearInterval(keepAlive);
  setTimeout(() => process.exit(code), 500);
}

ws.addEventListener('open', async () => {
  try {
    const session = await pairWithServer(ws, 'fake-iphone');
    log({ step: 'paired', code: session.code, peerName: session.peerName });

    const fs = await import('node:fs/promises');
    const path = await import('node:path');

    const transfers = new Transfers((frame) => ws.send(frame), {
      async onReceiveDone(offer, bytes, ok) {
        if (!ok) {
          log({ step: 'error', message: 'sha256 mismatch on received file' });
          finish(1);
          return;
        }
        await fs.writeFile(path.join(arg1, offer.name), bytes);
        log({ step: 'received', name: offer.name, bytes: bytes.length });
        finish(0);
      },
    });
    ws.addEventListener('message', (e) => transfers.handleFrame(e.data));

    if (mode === 'send') {
      const data = new Uint8Array(await fs.readFile(arg1));
      await transfers.sendFile({
        name: arg1.split('/').pop(),
        mime: 'application/octet-stream',
        bytes: data,
      });
      log({ step: 'sent', bytes: data.length });
      finish(0);
    } else if (mode !== 'receive') {
      throw new Error(`unknown mode: ${mode}`);
    }
    // In receive mode the onReceiveDone hook above ends the process.
  } catch (err) {
    log({ step: 'error', message: String((err && err.message) || err) });
    finish(1);
  }
});

ws.addEventListener('error', (e) => {
  log({ step: 'error', message: String((e && (e.message || e.error)) || e) });
});
