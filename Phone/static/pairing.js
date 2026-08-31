import {
  sortPubkeys, deriveCode, deriveSharedKey, computeHMAC, verifyHMAC,
  encodeHello, decodeHello, encodePairChallenge, decodePairChallenge,
  encodePairResponse, decodePairResponse, encodePairOK, decodePairOK,
} from './pairing-protocol.js';
import { x25519 } from './vendor/beamdrop-crypto.mjs';
import { framed, FrameType } from './protocol.js';

function makeReader(ws) {
  const queue = [];
  const waiters = [];
  const listener = (e) => {
    if (waiters.length) waiters.shift()(e.data);
    else queue.push(e.data);
  };
  ws.addEventListener('message', listener);
  return {
    next: () => new Promise((resolve) => {
      if (queue.length) resolve(queue.shift());
      else waiters.push(resolve);
    }),
    stop: () => ws.removeEventListener('message', listener),
  };
}

const IDENTITY_KEY = 'beamdrop.identity';

// loadOrCreateIdentity keeps this browser's private key across page loads.
//
// A fresh key per load would make the laptop see an unrecognised stranger
// every time — so its known-peers store could never match and you would
// confirm a 6-digit code on every reload and every reconnect. iOS drops the
// WebSocket whenever Safari goes to the background, so that is constantly.
//
// The key sits in localStorage, scoped to the laptop's https origin. That
// is the same bet the laptop makes storing peers in its config dir: TOFU
// assumes the endpoints themselves are not compromised.
function loadOrCreateIdentity() {
  try {
    const stored = localStorage.getItem(IDENTITY_KEY);
    if (stored) {
      const priv = Uint8Array.from(atob(stored), (c) => c.charCodeAt(0));
      if (priv.length === 32) return priv;
    }
  } catch (e) {
    // Private browsing blocks localStorage. Fall through to an ephemeral
    // key: re-confirming a code beats not connecting at all.
  }
  const priv = x25519.utils.randomPrivateKey();
  try {
    localStorage.setItem(IDENTITY_KEY, btoa(String.fromCharCode(...priv)));
  } catch (e) { /* ephemeral this session */ }
  return priv;
}

// pairWithServer runs the full pairing ceremony as the initiator (the
// browser always opens the WebSocket and therefore always writes
// first — matches the "iPhone opens first, acts as initiator" rule).
// Mirrors internal/engine/engine.go's pair() step for step.
export async function pairWithServer(ws, myName) {
  const reader = makeReader(ws);
  try {
    const priv = loadOrCreateIdentity();
    const pub = x25519.getPublicKey(priv);

    // Step 1: HELLO (write first), then read server's HELLO.
    ws.send(framed(FrameType.HELLO, encodeHello(myName, 0x08, 0)));
    const helloMsg = await reader.next();
    const { type: helloType, payload: helloPayload } = decodeFrame(helloMsg);
    if (helloType !== FrameType.HELLO) throw new Error('expected HELLO');
    const peerHello = decodeHello(helloPayload);

    // Step 2: raw 32-byte pubkey exchange (write first, then read).
    ws.send(pub);
    const peerPubMsg = await reader.next();
    const peerPub = new Uint8Array(peerPubMsg);
    if (peerPub.length !== 32) throw new Error('bad peer pubkey length');

    const code = await deriveCode(pub, peerPub);
    const sharedKey = deriveSharedKey(priv, peerPub, code);

    // Step 3: PAIR_CHALLENGE/PAIR_RESPONSE (write challenge, read response).
    const initNonce = crypto.getRandomValues(new Uint8Array(32));
    ws.send(framed(FrameType.PAIR_CHALLENGE, encodePairChallenge(initNonce)));
    const respMsg = await reader.next();
    const { type: respType, payload: respPayload } = decodeFrame(respMsg);
    if (respType !== FrameType.PAIR_RESPONSE) throw new Error('expected PAIR_RESPONSE');
    const { responderNonce, hmac: gotHMAC } = decodePairResponse(respPayload);
    if (!verifyHMAC(sharedKey, initNonce, responderNonce, gotHMAC)) {
      throw new Error('pairing HMAC verification failed');
    }

    // Step 4: PAIR_OK (write first, then read + verify peer pubkey).
    ws.send(framed(FrameType.PAIR_OK, encodePairOK(myName, pub)));
    const okMsg = await reader.next();
    const { type: okType, payload: okPayload } = decodeFrame(okMsg);
    if (okType !== FrameType.PAIR_OK) throw new Error('expected PAIR_OK');
    const gotOK = decodePairOK(okPayload);
    if (!bytesEqual(gotOK.pubkey, peerPub)) throw new Error('peer pubkey mismatch');

    return { sharedKey, code, peerName: peerHello.name, peerPub };
  } finally {
    reader.stop();
  }
}

function decodeFrame(buf) {
  const b = new Uint8Array(buf);
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  const len = dv.getUint32(0, true);
  return { type: b[4], payload: b.subarray(5, 4 + len) };
}

function bytesEqual(a, b) {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}
