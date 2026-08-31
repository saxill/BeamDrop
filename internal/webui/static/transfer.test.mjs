import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';

import { Transfers } from './transfer.js';
import {
  FrameType, framed, decodeFrame, decodeFileOffer, decodeChunk, decodeFileDone,
  encodeFileOffer, encodeFileAccept, encodeChunk, encodeFileDone, encodeError,
  bytesToHex,
} from './protocol.js';

function sha256(bytes) {
  return new Uint8Array(createHash('sha256').update(bytes).digest());
}

// wire collects everything a Transfers instance writes, decoded.
function wire() {
  const frames = [];
  const send = (buf) => frames.push(decodeFrame(buf));
  return { frames, send, ofType: (t) => frames.filter((f) => f.type === t) };
}

const settle = () => new Promise((r) => setTimeout(r, 0));

// until polls for a condition. Completing a receive involves
// crypto.subtle.digest, which resolves on the threadpool an unpredictable
// number of ticks later, so counting ticks makes these tests flaky.
async function until(cond, what, timeoutMs = 2000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (cond()) return;
    await new Promise((r) => setTimeout(r, 5));
  }
  assert.fail(`timed out waiting for ${what}`);
}

test('a completed inbound transfer answers with FILE_DONE', async () => {
  // The regression this file exists for: the Go sender blocks in SendFile
  // until it sees FILE_DONE. app.js used to never send one, so every
  // laptop→phone transfer hung with the phone showing "receiving…".
  const w = wire();
  const received = [];
  const t = new Transfers(w.send, { onReceiveDone: (offer, bytes, ok) => received.push({ offer, bytes, ok }) });

  const payload = new Uint8Array(1000).fill(7);
  const id = 42n;
  t.handleFrame(framed(FrameType.FILE_OFFER, encodeFileOffer({
    id, size: payload.length, name: 'a.bin', mime: 'application/octet-stream', sha256: sha256(payload),
  })));

  assert.equal(w.ofType(FrameType.FILE_ACCEPT).length, 1, 'should accept the offer immediately');

  t.handleFrame(framed(FrameType.CHUNK, encodeChunk(id, 0, payload)));
  await until(() => w.ofType(FrameType.FILE_DONE).length === 1, 'FILE_DONE');

  const dones = w.ofType(FrameType.FILE_DONE);
  assert.equal(dones.length, 1, 'a finished receive must answer with FILE_DONE');
  assert.equal(bytesToHex(decodeFileDone(dones[0].payload).sha256), bytesToHex(sha256(payload)));
  assert.equal(received.length, 1);
  assert.ok(received[0].ok);
  assert.deepEqual(received[0].bytes, payload);
});

test('a corrupted inbound transfer answers with ERROR, not FILE_DONE', async () => {
  const w = wire();
  const results = [];
  const t = new Transfers(w.send, { onReceiveDone: (offer, bytes, ok) => results.push(ok) });

  const payload = new Uint8Array(64).fill(1);
  const id = 9n;
  t.handleFrame(framed(FrameType.FILE_OFFER, encodeFileOffer({
    id, size: payload.length, name: 'bad.bin', mime: '', sha256: sha256(new Uint8Array(64).fill(2)),
  })));
  t.handleFrame(framed(FrameType.CHUNK, encodeChunk(id, 0, payload)));
  await until(() => results.length === 1, 'the receive to finish');

  assert.equal(w.ofType(FrameType.FILE_DONE).length, 0, 'must not confirm a file that failed its checksum');
  assert.equal(w.ofType(FrameType.ERROR).length, 1, 'the sender has to be told, or it waits forever');
  assert.deepEqual(results, [false]);
});

test('inbound reassembles multiple chunks in order', async () => {
  const w = wire();
  const got = [];
  const t = new Transfers(w.send, { onReceiveDone: (o, bytes, ok) => got.push({ bytes, ok }) });

  const payload = new Uint8Array(200_000);
  for (let i = 0; i < payload.length; i++) payload[i] = i % 251;
  const id = 5n;
  t.handleFrame(framed(FrameType.FILE_OFFER, encodeFileOffer({
    id, size: payload.length, name: 'big.bin', mime: '', sha256: sha256(payload),
  })));
  for (let off = 0; off < payload.length; off += 65536) {
    t.handleFrame(framed(FrameType.CHUNK, encodeChunk(id, off, payload.subarray(off, Math.min(off + 65536, payload.length)))));
  }
  await until(() => got.length === 1, 'the receive to finish');

  assert.equal(got.length, 1);
  assert.ok(got[0].ok);
  assert.deepEqual(got[0].bytes, payload);
});

test('a chunk past the declared size is refused rather than thrown', async () => {
  const w = wire();
  const t = new Transfers(w.send, {});
  const id = 11n;
  t.handleFrame(framed(FrameType.FILE_OFFER, encodeFileOffer({
    id, size: 10, name: 'small.bin', mime: '', sha256: new Uint8Array(32),
  })));
  // Would be a RangeError out of the message handler without the guard.
  t.handleFrame(framed(FrameType.CHUNK, encodeChunk(id, 0, new Uint8Array(64))));
  await until(() => w.ofType(FrameType.ERROR).length === 1, 'the refusal');
});

test('sendFile waits for FILE_ACCEPT before streaming chunks', async () => {
  const w = wire();
  const t = new Transfers(w.send, {});
  const bytes = new Uint8Array(100).fill(3);

  const p = t.sendFile({ name: 'out.bin', mime: 'text/plain', bytes });
  await until(() => w.ofType(FrameType.FILE_OFFER).length === 1, 'FILE_OFFER');

  assert.equal(w.ofType(FrameType.FILE_OFFER).length, 1);
  assert.equal(w.ofType(FrameType.CHUNK).length, 0, 'chunks must not fly before the peer accepts');

  const offer = decodeFileOffer(w.ofType(FrameType.FILE_OFFER)[0].payload);
  assert.equal(offer.name, 'out.bin');
  assert.equal(Number(offer.size), bytes.length);
  assert.equal(bytesToHex(offer.sha256), bytesToHex(sha256(bytes)));

  t.handleFrame(framed(FrameType.FILE_ACCEPT, encodeFileAccept(offer.id, 0n)));
  await until(() => w.ofType(FrameType.CHUNK).length === 1, 'the chunk');
  const chunks = w.ofType(FrameType.CHUNK);
  assert.equal(chunks.length, 1);
  assert.deepEqual(decodeChunk(chunks[0].payload).data, bytes);

  // Still pending: the peer has not confirmed the file yet.
  let settled = false;
  p.then(() => { settled = true; }, () => { settled = true; });
  await settle();
  assert.equal(settled, false, 'sendFile must not resolve before FILE_DONE');

  t.handleFrame(framed(FrameType.FILE_DONE, encodeFileDone(offer.id, sha256(bytes))));
  await p;
});

test('sendFile rejects when the peer reports an error', async () => {
  const w = wire();
  const t = new Transfers(w.send, {});
  const p = t.sendFile({ name: 'x.bin', mime: '', bytes: new Uint8Array(10) });
  await until(() => w.ofType(FrameType.FILE_OFFER).length === 1, 'FILE_OFFER');

  t.handleFrame(framed(FrameType.ERROR, encodeError(3, 'hash mismatch')));
  await assert.rejects(p, /hash mismatch/);
});

test('advisory ACK frames are ignored', async () => {
  const w = wire();
  const t = new Transfers(w.send, {});
  // The Go receiver sends one ACK per chunk; nothing waits on them and
  // they must not disturb the state machine.
  assert.doesNotThrow(() => t.handleFrame(framed(FrameType.ACK, new Uint8Array(16))));
  assert.equal(w.frames.length, 0);
});

test('text messages round-trip through the same state machine', async () => {
  const w = wire();
  const got = [];
  const t = new Transfers(w.send, { onText: (body) => got.push(body) });

  t.sendText('https://example.com/a-link');
  const sent = w.ofType(FrameType.TEXT);
  assert.equal(sent.length, 1, 'sendText must put a TEXT frame on the wire');
  assert.equal(new TextDecoder().decode(sent[0].payload), 'https://example.com/a-link');

  // And inbound TEXT reaches the hook rather than being swallowed as an
  // unknown frame type, which is what happened before.
  t.handleFrame(framed(FrameType.TEXT, new TextEncoder().encode('hello from the laptop')));
  assert.deepEqual(got, ['hello from the laptop']);
});
