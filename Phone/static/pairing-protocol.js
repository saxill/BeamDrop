import { x25519, blake2b, hmac, sha256 } from './vendor/beamdrop-crypto.mjs';

function cmpBytes(a, b) {
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return a[i] - b[i];
  }
  return 0;
}

export function sortPubkeys(a, b) {
  return cmpBytes(b, a) < 0 ? [b, a] : [a, b];
}

export async function deriveCode(initPub, respPub) {
  const [lo, hi] = sortPubkeys(initPub, respPub);
  const h = blake2b(concat(lo, hi), { dkLen: 32 });
  const n = new DataView(h.buffer, h.byteOffset, h.byteLength).getBigUint64(0, true);
  return (n % 1_000_000n).toString().padStart(6, '0');
}

export function deriveSharedKey(myPriv, theirPub, code) {
  const ecdh = x25519.getSharedSecret(myPriv, theirPub);
  return blake2b(concat(ecdh, new TextEncoder().encode(code)), { dkLen: 32 });
}

export function computeHMAC(sharedKey, initNonce, respNonce) {
  return hmac(sha256, sharedKey, concat(initNonce, respNonce));
}

export function verifyHMAC(sharedKey, initNonce, respNonce, got) {
  const want = computeHMAC(sharedKey, initNonce, respNonce);
  if (want.length !== got.length) return false;
  let diff = 0;
  for (let i = 0; i < want.length; i++) diff |= want[i] ^ got[i];
  return diff === 0;
}

function concat(...parts) {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) { out.set(p, off); off += p.length; }
  return out;
}

// --- frame payload codecs (payload only — no [len][type] prefix) ---

export function encodeHello(name, mode, capabilities) {
  const nameBytes = new TextEncoder().encode(name);
  const out = new Uint8Array(1 + 2 + 1 + nameBytes.length);
  const dv = new DataView(out.buffer);
  dv.setUint8(0, mode);
  dv.setUint16(1, capabilities, true);
  dv.setUint8(3, nameBytes.length);
  out.set(nameBytes, 4);
  return out;
}

export function decodeHello(payload) {
  const dv = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
  const mode = dv.getUint8(0);
  const capabilities = dv.getUint16(1, true);
  const nameLen = dv.getUint8(3);
  const name = new TextDecoder().decode(payload.subarray(4, 4 + nameLen));
  return { mode, capabilities, name };
}

export function encodePairChallenge(nonce) {
  return new Uint8Array(nonce);
}

export function decodePairChallenge(payload) {
  return { nonce: new Uint8Array(payload.subarray(0, 32)) };
}

export function encodePairResponse(responderNonce, mac) {
  return concat(responderNonce, mac);
}

export function decodePairResponse(payload) {
  return {
    responderNonce: new Uint8Array(payload.subarray(0, 32)),
    hmac: new Uint8Array(payload.subarray(32, 64)),
  };
}

export function encodePairOK(name, pubkey) {
  const nameBytes = new TextEncoder().encode(name);
  const out = new Uint8Array(1 + nameBytes.length + 32);
  out[0] = nameBytes.length;
  out.set(nameBytes, 1);
  out.set(pubkey, 1 + nameBytes.length);
  return out;
}

export function decodePairOK(payload) {
  const nameLen = payload[0];
  const name = new TextDecoder().decode(payload.subarray(1, 1 + nameLen));
  const pubkey = new Uint8Array(payload.subarray(1 + nameLen, 1 + nameLen + 32));
  return { name, pubkey };
}
