import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  sortPubkeys, deriveCode, computeHMAC, verifyHMAC, deriveSharedKey,
  encodeHello, decodeHello, encodePairChallenge, decodePairChallenge,
  encodePairResponse, decodePairResponse, encodePairOK, decodePairOK,
} from './pairing-protocol.js';
import { x25519 } from './vendor/beamdrop-crypto.mjs';

test('sortPubkeys orders lexicographically regardless of call order', () => {
  const a = new Uint8Array(32).fill(1);
  const b = new Uint8Array(32).fill(2);
  const [lo1, hi1] = sortPubkeys(a, b);
  const [lo2, hi2] = sortPubkeys(b, a);
  assert.deepEqual(lo1, lo2);
  assert.deepEqual(hi1, hi2);
  assert.deepEqual(lo1, a);
});

test('deriveCode is symmetric and 6 digits', async () => {
  const a = new Uint8Array(32).fill(3);
  const b = new Uint8Array(32).fill(7);
  const c1 = await deriveCode(a, b);
  const c2 = await deriveCode(b, a);
  assert.equal(c1, c2);
  assert.match(c1, /^\d{6}$/);
});

test('HMAC round-trips and rejects tampering', () => {
  const key = new Uint8Array(32).fill(9);
  const n1 = new Uint8Array(32).fill(1);
  const n2 = new Uint8Array(32).fill(2);
  const mac = computeHMAC(key, n1, n2);
  assert.equal(mac.length, 32);
  assert.ok(verifyHMAC(key, n1, n2, mac));
  const bad = new Uint8Array(mac);
  bad[0] ^= 0xff;
  assert.ok(!verifyHMAC(key, n1, n2, bad));
});

test('deriveSharedKey matches between two X25519 peers', () => {
  const aPriv = x25519.utils.randomPrivateKey();
  const aPub = x25519.getPublicKey(aPriv);
  const bPriv = x25519.utils.randomPrivateKey();
  const bPub = x25519.getPublicKey(bPriv);
  const k1 = deriveSharedKey(aPriv, bPub, '123456');
  const k2 = deriveSharedKey(bPriv, aPub, '123456');
  assert.deepEqual(k1, k2);
});

test('HELLO round-trips', () => {
  const payload = encodeHello('iPhone', 0x08, 0);
  const got = decodeHello(payload);
  assert.equal(got.name, 'iPhone');
  assert.equal(got.mode, 0x08);
});

test('PAIR_CHALLENGE round-trips', () => {
  const nonce = new Uint8Array(32).fill(5);
  const got = decodePairChallenge(encodePairChallenge(nonce));
  assert.deepEqual(got.nonce, nonce);
});

test('PAIR_RESPONSE round-trips', () => {
  const rn = new Uint8Array(32).fill(6);
  const mac = new Uint8Array(32).fill(7);
  const got = decodePairResponse(encodePairResponse(rn, mac));
  assert.deepEqual(got.responderNonce, rn);
  assert.deepEqual(got.hmac, mac);
});

test('PAIR_OK round-trips', () => {
  const pub = new Uint8Array(32).fill(8);
  const got = decodePairOK(encodePairOK('iPhone', pub));
  assert.equal(got.name, 'iPhone');
  assert.deepEqual(got.pubkey, pub);
});
