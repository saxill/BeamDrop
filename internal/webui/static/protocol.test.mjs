import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  FrameType, framed, decodeFrame, decodeFileOffer, decodeChunk,
  decodeFileDone, encodeFileAccept, bytesToHex,
} from './protocol.js';

test('framed prepends length prefix and decodeFrame inverts it', () => {
  const payload = new Uint8Array([FrameType.TEXT, 1, 2, 3]);
  const buf = framed(FrameType.TEXT, payload.subarray(1));
  const { type, payload: got } = decodeFrame(buf);
  assert.equal(type, FrameType.TEXT);
  assert.deepEqual(got, payload.subarray(1));
});

test('decodeFileOffer parses id/size/name/mime/sha256', () => {
  const name = new TextEncoder().encode('a.txt');
  const mime = new TextEncoder().encode('text/plain');
  const sha = new Uint8Array(32).fill(9);
  const payload = new Uint8Array(8 + 8 + 1 + name.length + 1 + mime.length + 32);
  const dv = new DataView(payload.buffer);
  dv.setBigUint64(0, 42n, true);
  dv.setBigUint64(8, 1000n, true);
  let off = 16;
  dv.setUint8(off, name.length); off += 1;
  payload.set(name, off); off += name.length;
  dv.setUint8(off, mime.length); off += 1;
  payload.set(mime, off); off += mime.length;
  payload.set(sha, off);

  const got = decodeFileOffer(payload);
  assert.equal(got.id, 42n);
  assert.equal(got.size, 1000n);
  assert.equal(got.name, 'a.txt');
  assert.equal(got.mime, 'text/plain');
  assert.deepEqual(got.sha256, sha);
});

test('decodeChunk parses id/offset/data', () => {
  const payload = new Uint8Array(16 + 3);
  const dv = new DataView(payload.buffer);
  dv.setBigUint64(0, 7n, true);
  dv.setBigUint64(8, 64n, true);
  payload.set([1, 2, 3], 16);
  const got = decodeChunk(payload);
  assert.equal(got.id, 7n);
  assert.equal(got.offset, 64n);
  assert.deepEqual(got.data, new Uint8Array([1, 2, 3]));
});

test('decodeFileDone parses id/sha256', () => {
  const sha = new Uint8Array(32).fill(5);
  const payload = new Uint8Array(8 + 32);
  new DataView(payload.buffer).setBigUint64(0, 3n, true);
  payload.set(sha, 8);
  const got = decodeFileDone(payload);
  assert.equal(got.id, 3n);
  assert.deepEqual(got.sha256, sha);
});

test('encodeFileAccept matches Go FileAcceptPayload layout (16 bytes: id, resumeFrom)', () => {
  const out = encodeFileAccept(11n, 0n);
  assert.equal(out.length, 16);
  const dv = new DataView(out.buffer);
  assert.equal(dv.getBigUint64(0, true), 11n);
  assert.equal(dv.getBigUint64(8, true), 0n);
});

test('bytesToHex', () => {
  assert.equal(bytesToHex(new Uint8Array([0, 255, 16])), '00ff10');
});
