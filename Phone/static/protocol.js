export const FrameType = {
  HELLO: 0x01, PAIR_CHALLENGE: 0x02, PAIR_RESPONSE: 0x03, PAIR_OK: 0x04,
  FILE_OFFER: 0x10, FILE_ACCEPT: 0x11, CHUNK: 0x12, ACK: 0x13, FILE_DONE: 0x14,
  TEXT: 0x80, HISTORY_REQUEST: 0x81, HISTORY: 0x82,
  PUSH_KEY_REQUEST: 0x83, PUSH_KEY: 0x84, PUSH_SUBSCRIBE: 0x85,
  FILE_REQUEST: 0x86,
  ERROR: 0xf0,
};

// framed prepends [len:u32 LE] and the type byte to a type-less payload,
// matching the Go-side frame.WriteFrame contract:
// [len:u32 LE][type:u8][payload...], where len counts type+payload.
export function framed(type, payload) {
  const out = new Uint8Array(4 + 1 + payload.length);
  new DataView(out.buffer).setUint32(0, 1 + payload.length, true);
  out[4] = type;
  out.set(payload, 5);
  return out;
}

export function decodeFrame(buf) {
  const b = new Uint8Array(buf);
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  const len = dv.getUint32(0, true);
  return { type: b[4], payload: b.subarray(5, 4 + len) };
}

export function decodeFileOffer(payload) {
  const dv = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
  const id = dv.getBigUint64(0, true);
  const size = dv.getBigUint64(8, true);
  let off = 16;
  const nameLen = payload[off]; off += 1;
  const name = new TextDecoder().decode(payload.subarray(off, off + nameLen)); off += nameLen;
  const mimeLen = payload[off]; off += 1;
  const mime = new TextDecoder().decode(payload.subarray(off, off + mimeLen)); off += mimeLen;
  const sha256 = new Uint8Array(payload.subarray(off, off + 32));
  return { id, size, name, mime, sha256 };
}

export function decodeChunk(payload) {
  const dv = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
  const id = dv.getBigUint64(0, true);
  const offset = dv.getBigUint64(8, true);
  const data = new Uint8Array(payload.subarray(16));
  return { id, offset, data };
}

export function decodeFileDone(payload) {
  const dv = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
  const id = dv.getBigUint64(0, true);
  const sha256 = new Uint8Array(payload.subarray(8, 40));
  return { id, sha256 };
}

export function encodeFileAccept(id, resumeFrom) {
  const out = new Uint8Array(16);
  const dv = new DataView(out.buffer);
  dv.setBigUint64(0, id, true);
  dv.setBigUint64(8, resumeFrom, true);
  return out;
}

// encodeFileOffer mirrors decodeFileOffer and Go's frame.encodePayload:
// [id:8][size:8][nameLen:1][name][mimeLen:1][mime][sha256:32].
export function encodeFileOffer({ id, size, name, mime, sha256 }) {
  const nameBytes = new TextEncoder().encode(name);
  // The spec caps mime at 64 bytes and Go's encoder enforces that, so stay
  // inside what the other end will also produce. A browser's file.type is
  // never close to this.
  const mimeBytes = new TextEncoder().encode(mime || 'application/octet-stream').subarray(0, 64);
  if (nameBytes.length > 255) throw new Error('file name too long for one length byte');
  const out = new Uint8Array(8 + 8 + 1 + nameBytes.length + 1 + mimeBytes.length + 32);
  const dv = new DataView(out.buffer);
  dv.setBigUint64(0, id, true);
  dv.setBigUint64(8, BigInt(size), true);
  let off = 16;
  out[off] = nameBytes.length; off += 1;
  out.set(nameBytes, off); off += nameBytes.length;
  out[off] = mimeBytes.length; off += 1;
  out.set(mimeBytes, off); off += mimeBytes.length;
  out.set(sha256, off);
  return out;
}

export function encodeChunk(id, offset, data) {
  const out = new Uint8Array(16 + data.length);
  const dv = new DataView(out.buffer);
  dv.setBigUint64(0, id, true);
  dv.setBigUint64(8, BigInt(offset), true);
  out.set(data, 16);
  return out;
}

// encodeFileDone is what a receiver sends back once every byte has arrived
// and the hash checks out. Go's SendFile blocks until it sees this frame,
// so a receiver that never sends one hangs the sender indefinitely.
export function encodeFileDone(id, sha256) {
  const out = new Uint8Array(8 + 32);
  new DataView(out.buffer).setBigUint64(0, id, true);
  out.set(sha256, 8);
  return out;
}

// encodeError mirrors Go's ErrorPayload: [code:u8][message]. The message
// carries no length prefix — the frame's own length bounds it.
export function encodeError(code, message) {
  const msg = new TextEncoder().encode(message).subarray(0, 255);
  const out = new Uint8Array(1 + msg.length);
  out[0] = code;
  out.set(msg, 1);
  return out;
}

// TEXT carries the message bytes with no header at all — the frame's own
// length bounds it.
export function encodeText(body) {
  return new TextEncoder().encode(body);
}

// HISTORY is the one frame carrying JSON rather than the packed encoding.
// The list is variable-length with heterogeneous records, so a hand-rolled
// binary format would buy nothing but a second decoder to keep in step with
// the Go side's.
export function decodeHistory(payload) {
  if (payload.length === 0) return [];
  const entries = JSON.parse(new TextDecoder().decode(payload));
  return Array.isArray(entries) ? entries : [];
}

// FILE_REQUEST is the name and nothing else — the frame's own length says
// how long it is, same as TEXT.
export function encodeFileRequest(name) {
  return new TextEncoder().encode(name);
}

export function decodePushKey(payload) {
  return new TextDecoder().decode(payload);
}

export function encodePushSubscribe(sub) {
  return new TextEncoder().encode(JSON.stringify(sub));
}

export function decodeText(payload) {
  return new TextDecoder().decode(payload);
}

export function bytesToHex(b) {
  return Array.from(b).map((x) => x.toString(16).padStart(2, '0')).join('');
}
