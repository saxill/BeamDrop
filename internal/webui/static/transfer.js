// The browser side of the file-transfer state machine.
//
// This has no DOM and does not create its own socket: it takes a send
// function and reports progress through hooks. That is deliberate. The
// previous arrangement had app.js implement the protocol for the browser
// and internal/smoke/fake_iphone.mjs implement it again for the test, and
// the two drifted — app.js never sent FILE_DONE when it received a file,
// so Go's SendFile blocked forever waiting for one and every laptop→phone
// transfer hung. The test could not catch it because the test was exercising
// the other implementation. There is now one.
//
// Mirrors internal/engine/engine.go's SendFile and receiveFile.

import {
  FrameType, framed, decodeFrame, decodeFileOffer, decodeChunk, decodeFileDone,
  encodeFileAccept, encodeFileOffer, encodeChunk, encodeFileDone, encodeError,
  encodeText, decodeText, decodeHistory, decodePushKey, encodePushSubscribe,
  encodeFileRequest,
  bytesToHex,
} from './protocol.js';

const CHUNK_SIZE = 64 * 1024;

async function sha256(bytes) {
  return new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  // A send that fails at the accept step never awaits the done promise, and
  // an unawaited rejection terminates the process under node's default
  // --unhandled-rejections=throw. Marking it handled here is harmless:
  // awaiting the original later still rethrows.
  promise.catch(() => {});
  return { promise, resolve, reject };
}

export class Transfers {
  // send(frameBytes) puts one frame on the wire.
  // hooks: onReceiveStart(offer), onReceiveDone(offer, bytes, ok),
  //        onSendStart(name, size), onSendDone(name, size), onError(message)
  constructor(send, hooks = {}) {
    this.send = send;
    this.hooks = hooks;
    this.inbound = new Map();  // id -> {offer, buf, received}
    this.outbound = new Map(); // id -> {accept, done}
  }

  // handleFrame takes one raw WebSocket message. Call it for every message
  // after pairing completes.
  handleFrame(data) {
    const { type, payload } = decodeFrame(data);
    switch (type) {
      case FrameType.FILE_OFFER:  return this._onOffer(decodeFileOffer(payload));
      case FrameType.CHUNK:       return this._onChunk(decodeChunk(payload));
      case FrameType.FILE_ACCEPT: return this._onAccept(payload);
      case FrameType.FILE_DONE:   return this._onDone(decodeFileDone(payload));
      case FrameType.ERROR:       return this._onError(payload);
      case FrameType.TEXT:        return this.hooks.onText?.(decodeText(payload));
      case FrameType.HISTORY:     return this.hooks.onHistory?.(decodeHistory(payload));
      case FrameType.PUSH_KEY:    return this.hooks.onPushKey?.(decodePushKey(payload));
      // ACK is advisory — the Go engine sends one per chunk and nothing
      // waits on them.
      default: return undefined;
    }
  }

  // requestHistory asks the portal what has already passed between us, so
  // reopening the app shows the conversation instead of a blank page.
  requestHistory() {
    this.send(framed(FrameType.HISTORY_REQUEST, new Uint8Array(0)));
  }

  // Ask the portal to send a file it already has. The answer is an ordinary
  // FILE_OFFER, so it arrives through the normal receive path and ends up
  // with the same Save link as anything else.
  requestFile(name) {
    this.send(framed(FrameType.FILE_REQUEST, encodeFileRequest(name)));
  }

  // Push registration goes over this socket rather than an HTTP endpoint so
  // it inherits the pairing that already happened here.
  requestPushKey() {
    this.send(framed(FrameType.PUSH_KEY_REQUEST, new Uint8Array(0)));
  }

  sendPushSubscription(sub) {
    this.send(framed(FrameType.PUSH_SUBSCRIBE, encodePushSubscribe(sub)));
  }

  // --- receiving ---

  _onOffer(offer) {
    this.inbound.set(String(offer.id), {
      offer,
      buf: new Uint8Array(Number(offer.size)),
      received: 0,
    });
    this.hooks.onReceiveStart?.(offer);
    this.send(framed(FrameType.FILE_ACCEPT, encodeFileAccept(offer.id, 0n)));
  }

  _onChunk(chunk) {
    const key = String(chunk.id);
    const t = this.inbound.get(key);
    if (!t) return; // a chunk for something we never accepted
    const offset = Number(chunk.offset);
    if (offset + chunk.data.length > t.buf.length) {
      // A chunk past the size the offer declared. Refuse rather than let
      // set() throw out of the message handler.
      this.inbound.delete(key);
      this.send(framed(FrameType.ERROR, encodeError(6, 'chunk past declared size')));
      this.hooks.onReceiveDone?.(t.offer, null, false);
      return;
    }
    t.buf.set(chunk.data, offset);
    t.received += chunk.data.length;
    if (t.received < t.buf.length) return;

    this.inbound.delete(key);
    // Hashing is async, so this tail runs detached from handleFrame.
    (async () => {
      const got = await sha256(t.buf);
      const ok = bytesToHex(got) === bytesToHex(t.offer.sha256);
      if (ok) {
        // This is the frame the Go sender blocks on. Without it SendFile
        // never returns and the portal's :send appears to hang forever.
        this.send(framed(FrameType.FILE_DONE, encodeFileDone(t.offer.id, got)));
      } else {
        // Tell the sender instead of leaving it waiting. Code 3 is what
        // engine.go uses for a hash mismatch.
        this.send(framed(FrameType.ERROR, encodeError(3, 'hash mismatch')));
      }
      this.hooks.onReceiveDone?.(t.offer, t.buf, ok);
    })();
  }

  // sendText is fire-and-forget: TEXT carries no id, so unlike a file
  // there is no acknowledgement to wait for.
  sendText(body) {
    this.send(framed(FrameType.TEXT, encodeText(body)));
  }

  // --- sending ---

  // sendFile streams one file and resolves only once the peer confirms it
  // with FILE_DONE. Resolving earlier would report a transfer as landed
  // while the peer could still reject it on a checksum mismatch.
  async sendFile({ name, mime, bytes }) {
    const id = crypto.getRandomValues(new BigUint64Array(1))[0];
    const key = String(id);
    const entry = { accept: deferred(), done: deferred() };
    this.outbound.set(key, entry);

    this.hooks.onSendStart?.(name, bytes.length);
    try {
      this.send(framed(FrameType.FILE_OFFER, encodeFileOffer({
        id, size: bytes.length, name, mime, sha256: await sha256(bytes),
      })));

      // Wait for FILE_ACCEPT before streaming, matching engine.go's
      // SendFile. The Go receiver does buffer early chunks, but a receiver
      // that wants to decline has no way to say so once bytes are already
      // in flight.
      await entry.accept.promise;

      for (let off = 0; off < bytes.length; off += CHUNK_SIZE) {
        const slice = bytes.subarray(off, Math.min(off + CHUNK_SIZE, bytes.length));
        this.send(framed(FrameType.CHUNK, encodeChunk(id, off, slice)));
        // Yield between chunks so the UI stays responsive and incoming
        // frames still get processed while a large file goes out.
        await new Promise((r) => setTimeout(r, 0));
      }

      await entry.done.promise;
      this.hooks.onSendDone?.(name, bytes.length);
    } finally {
      this.outbound.delete(key);
    }
  }

  _onAccept(payload) {
    const dv = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
    this.outbound.get(String(dv.getBigUint64(0, true)))?.accept.resolve();
  }

  _onDone(done) {
    this.outbound.get(String(done.id))?.done.resolve();
  }

  _onError(payload) {
    // [code:u8][message] — no length prefix, the frame bounds it.
    const message = payload.length > 1
      ? new TextDecoder().decode(payload.subarray(1))
      : 'peer reported an error';
    const err = new Error(message);
    // ERROR carries no transfer id, so it can only be applied to every
    // send in flight — which is also why the engine allows just one.
    for (const entry of this.outbound.values()) {
      entry.accept.reject(err);
      entry.done.reject(err);
    }
    this.hooks.onError?.(message);
  }
}
