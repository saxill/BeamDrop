import { pairWithServer } from './pairing.js';
import { Transfers } from './transfer.js';

// The page is a conversation with your laptop: one feed of everything that
// has passed between the two, and one input that takes either a message or
// a file. The previous version was a drop target above a list of downloads,
// which made sending a file easy and sending a line of text impossible —
// even though TEXT has been in the wire protocol from the start.

const dot = document.getElementById('dot');
const statusEl = document.getElementById('status');
const feed = document.getElementById('feed');
const picker = document.getElementById('picker');
const msg = document.getElementById('msg');

const DEVICE_NAME = localStorage.getItem('beamdrop-name') || 'iPhone';

let ws;
let transfers = null;
let retryDelay = 1000;

function setStatus(text, connected) {
  statusEl.textContent = text;
  dot.classList.toggle('on', !!connected);
}

// --- feed ---------------------------------------------------------------

// Cards awaiting a file they asked for, keyed by name. A fetched file comes
// back as a plain FILE_OFFER carrying no reference to the card that asked
// for it, so the name is the only thing joining the two.
const pendingFetch = new Map();

// fetchDone fills in the card that asked, and reports whether this file was
// a fetch at all.
//
// The result has to land *in that card*. The first version appended a fresh
// card at the end of the feed instead and left the button reading "Open"
// again — so from where you were looking, nothing happened, and the obvious
// response was to tap again. The portal's log showed four identical
// requests two seconds apart.
function fetchDone(offer, bytes, ok) {
  const pend = pendingFetch.get(offer.name);
  if (!pend) return false;
  pendingFetch.delete(offer.name);

  if (!ok) {
    pend.button.disabled = false;
    pend.button.textContent = 'Failed';
    return true;
  }

  const url = URL.createObjectURL(new Blob([bytes], {
    type: offer.mime || 'application/octet-stream',
  }));
  if (IMAGE.test(offer.name)) {
    const img = document.createElement('img');
    img.src = url;
    pend.card.insertBefore(img, pend.card.firstChild);
  }
  // Becomes a Save link rather than staying a button: the file is here now,
  // so offering to fetch it again is the one thing that is not useful.
  const a = document.createElement('a');
  a.className = 'save';
  a.href = url;
  a.download = offer.name;
  a.textContent = 'Save';
  pend.button.replaceWith(a);
  pend.card.closest('.row')?.scrollIntoView({ block: 'nearest' });
  return true;
}

// History lives in its own container, always first in the feed. Keeping it
// separate is what lets a reconnect replace it wholesale — appending the
// same history again on every dropped socket would grow a duplicate
// conversation, and a phone drops its socket constantly.
function historyBox() {
  let box = document.getElementById('history');
  if (!box) {
    box = document.createElement('div');
    box.id = 'history';
    feed.insertBefore(box, feed.firstChild);
  }
  return box;
}

// scrollToBottom jumps the feed to the newest message. It is deferred to the
// next animation frame because iOS's -webkit-overflow-scrolling: touch can
// ignore a scrollTop write that lands in the same frame as a layout change —
// the new height is not settled yet, so the write is a no-op and the latest
// message stays below the fold.
function scrollToBottom() {
  requestAnimationFrame(() => {
    feed.scrollTop = feed.scrollHeight;
  });
}

function card(mine, parent) {
  feed.querySelector('.empty')?.remove();
  // Follow the bottom only if the reader is already there. Yanking the view
  // away from someone scrolled up through history is worse than a missed
  // update, so decide before appending.
  const atBottom = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 120;

  const row = document.createElement('div');
  row.className = 'row' + (mine ? ' mine' : '');
  const box = document.createElement('div');
  box.className = 'card';
  row.appendChild(box);
  (parent || feed).appendChild(row);

  if (atBottom && !parent) scrollToBottom();
  return box;
}

function when(unixSeconds) {
  const d = new Date(unixSeconds * 1000);
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  return sameDay
    ? d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
    : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' +
      d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
}

function renderHistory(entries) {
  const box = historyBox();
  box.textContent = '';
  if (!entries.length) {
    // Nothing ever happened, so leave the empty-state message alone.
    if (!feed.querySelector('.row')) showEmpty();
    return;
  }
  feed.querySelector('.empty')?.remove();

  for (const h of entries) {
    const c = card(!!h.outbound, box);
    const name = document.createElement('div');
    name.className = 'name';
    if (h.kind === 'file') {
      name.textContent = '📎 ' + (h.name || 'file');
    } else {
      name.textContent = h.text || '';
    }
    c.appendChild(name);
    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.textContent = h.kind === 'file' && h.size
      ? `${human(Number(h.size))} · ${when(h.at)}`
      : when(h.at);
    c.appendChild(meta);

    // The page holds no bytes for anything it did not receive this session,
    // so a past file needs fetching before it can be opened or saved. Not
    // done automatically: pulling every photo in the history down a
    // cellular connection on every reconnect would be worse than the tap.
    if (h.kind === 'file' && h.name) {
      c.classList.add('fetchable');
      const get = document.createElement('button');
      get.className = 'get';
      get.type = 'button';
      get.textContent = 'Open';
      get.addEventListener('click', () => {
        if (!transfers) {
          setStatus('not connected', false);
          return;
        }
        get.disabled = true;
        get.textContent = 'Fetching…';
        // The arriving file is an ordinary FILE_OFFER with no link back to
        // this card, so the name is what reunites them — see fetchDone.
        pendingFetch.set(h.name, { button: get, card: c });
        transfers.requestFile(h.name);
      });
      c.appendChild(get);
    }
  }

  const mark = document.createElement('div');
  mark.className = 'divider';
  mark.textContent = 'earlier';
  box.insertBefore(mark, box.firstChild);

  scrollToBottom();
}

function showEmpty() {
  if (feed.querySelector('.empty')) return;
  const d = document.createElement('div');
  d.className = 'empty';
  d.textContent = 'Nothing yet — send a file or a message.';
  feed.appendChild(d);
}

function textCard(body, mine) {
  const box = card(mine);
  const p = document.createElement('div');
  p.className = 'name';
  p.textContent = body; // textContent, not innerHTML: this is remote input
  box.appendChild(p);
  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.textContent = mine ? 'sent' : 'from laptop';
  box.appendChild(meta);
  return box;
}

function fileCard(name, size, mine) {
  const box = card(mine);
  const n = document.createElement('div');
  n.className = 'name';
  n.textContent = '📎 ' + name;
  box.appendChild(n);
  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.textContent = `${human(size)} · ${mine ? 'sending…' : 'receiving…'}`;
  box.appendChild(meta);
  return { box, meta };
}

function human(n) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + ' GB';
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
  if (n >= 1 << 10) return Math.round(n / (1 << 10)) + ' KB';
  return n + ' B';
}

const IMAGE = /\.(jpe?g|png|gif|webp|bmp|heic)$/i;

// --- connection ---------------------------------------------------------

function connect() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/ws`);
  ws.binaryType = 'arraybuffer';

  ws.onopen = async () => {
    setStatus('pairing…', false);
    try {
      const session = await pairWithServer(ws, DEVICE_NAME);
      setStatus(session.peerName, true);
      retryDelay = 1000;
      transfers = newTransfers();
      ws.onmessage = (e) => transfers.handleFrame(e.data);
      // Ask what already happened. Without this the app opens blank every
      // time — anything sent while it was closed, or before the phone last
      // reconnected, was simply invisible.
      transfers.requestHistory();
      transfers.requestPushKey();
    } catch (err) {
      setStatus('pairing failed', false);
      ws.close();
    }
  };

  ws.onclose = () => {
    transfers = null;
    setStatus('reconnecting…', false);
    setTimeout(connect, retryDelay);
    // Back off to 30s rather than hammering a laptop that is asleep, which
    // on a phone also means not burning the battery.
    retryDelay = Math.min(retryDelay * 2, 30_000);
  };
  ws.onerror = () => setStatus('connection error', false);
}

function newTransfers() {
  const rows = new Map();

  return new Transfers((frame) => ws.send(frame), {
    onText(body) {
      textCard(body, false);
    },
    onHistory(entries) {
      renderHistory(entries);
    },
    onPushKey(key) {
      onVapidKey(key);
    },
    onReceiveStart(offer) {
      // A file we asked for belongs to the history card that asked, not to
      // a new one at the end of the feed.
      if (pendingFetch.has(offer.name)) return;
      rows.set(String(offer.id), fileCard(offer.name, Number(offer.size), false));
    },
    onReceiveDone(offer, bytes, ok) {
      if (fetchDone(offer, bytes, ok)) return;
      const entry = rows.get(String(offer.id)) || fileCard(offer.name, Number(offer.size), false);
      rows.delete(String(offer.id));
      if (!ok) {
        entry.meta.textContent = 'checksum mismatch — discarded';
        return;
      }
      const url = URL.createObjectURL(new Blob([bytes], { type: offer.mime || 'application/octet-stream' }));
      if (IMAGE.test(offer.name)) {
        const img = document.createElement('img');
        img.src = url;
        entry.box.insertBefore(img, entry.box.firstChild);
      }
      // iOS gives a web page no way to write to Photos or Files, so the most
      // it can offer is a link the user saves themselves.
      const a = document.createElement('a');
      a.className = 'save';
      a.href = url;
      a.download = offer.name;
      a.textContent = 'Save';
      entry.meta.textContent = `${human(bytes.length)} · `;
      entry.meta.appendChild(a);
    },
  });
}

// --- sending ------------------------------------------------------------

function sendText() {
  const body = msg.value.trim();
  if (!body) return;
  if (!transfers) {
    setStatus('not connected', false);
    return;
  }
  transfers.sendText(body);
  textCard(body, true);
  msg.value = '';
  msg.style.height = 'auto';
}

async function sendFile(f) {
  if (!transfers) {
    setStatus('not connected', false);
    return;
  }
  const entry = fileCard(f.name, f.size, true);
  try {
    const bytes = new Uint8Array(await f.arrayBuffer());
    await transfers.sendFile({ name: f.name, mime: f.type, bytes });
    // Only once the laptop confirms the checksum. Saying "sent" when the
    // bytes merely left the phone would be a claim the user cannot check.
    entry.meta.textContent = `${human(f.size)} · sent`;
  } catch (err) {
    entry.meta.textContent = err.message;
  }
}

document.getElementById('send').addEventListener('click', sendText);
document.getElementById('attach').addEventListener('click', () => picker.click());
picker.addEventListener('change', () => {
  for (const f of picker.files) sendFile(f);
  picker.value = ''; // so picking the same file twice fires change again
});

msg.addEventListener('keydown', (e) => {
  // Enter sends, shift-enter is a newline — as everywhere else.
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    sendText();
  }
});
msg.addEventListener('input', () => {
  msg.style.height = 'auto';
  msg.style.height = Math.min(msg.scrollHeight, 112) + 'px';
});

// Dropping works in a laptop browser; on a phone the + button is the way in.
document.addEventListener('dragover', (e) => e.preventDefault());
document.addEventListener('drop', (e) => {
  e.preventDefault();
  for (const f of e.dataTransfer.files) sendFile(f);
});

// --- notifications ------------------------------------------------------
//
// Without these the app only tells you about a file while it is open, which
// on iOS means almost never: the system suspends a home-screen app within
// seconds of it going to the background, taking the WebSocket with it.
//
// Three things have to be true and none can be forced from here, so every
// step degrades to hiding the button rather than showing an error:
//   - the page must be served over a publicly trusted certificate;
//   - on iOS, the app must have been added to the home screen (16.4+);
//   - the user must grant permission, which can only be asked for from a
//     tap. Asking on load is both against the rules and rude.

const bell = document.getElementById('bell');
let vapidKey = null;

function pushSupported() {
  return 'serviceWorker' in navigator && 'PushManager' in window &&
    'Notification' in window && window.isSecureContext;
}

async function onVapidKey(key) {
  // An empty key means the portal cannot do push at all — no config
  // directory, most likely. Nothing to offer, so offer nothing.
  if (!key || !pushSupported()) return;
  vapidKey = key;

  if (Notification.permission === 'denied') return;
  if (Notification.permission === 'granted') {
    // Already allowed: re-register silently. Subscriptions expire and the
    // portal may have been reinstalled, and re-sending is cheap.
    await subscribe();
    return;
  }
  bell.hidden = false;
}

// The VAPID key arrives base64url; pushManager wants raw bytes.
function urlBase64ToUint8Array(base64) {
  const pad = '='.repeat((4 - (base64.length % 4)) % 4);
  const raw = atob((base64 + pad).replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

async function subscribe() {
  if (!vapidKey || !transfers) return false;
  try {
    const reg = await navigator.serviceWorker.ready;
    const sub = await reg.pushManager.getSubscription() ||
      await reg.pushManager.subscribe({
        // Required to be true, and honest: every push we send is shown.
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array(vapidKey),
      });
    const j = sub.toJSON();
    transfers.sendPushSubscription({
      endpoint: sub.endpoint,
      p256dh: j.keys?.p256dh || '',
      auth: j.keys?.auth || '',
    });
    bell.hidden = true;
    return true;
  } catch (err) {
    setStatus('notifications unavailable', true);
    return false;
  }
}

bell?.addEventListener('click', async () => {
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') {
    bell.hidden = true; // asking twice after a no is just nagging
    return;
  }
  await subscribe();
});

// Register the shell cache so the app opens instantly from the home screen.
// Best-effort: a service worker needs a secure context, and a self-signed
// certificate that was tapped through does not always qualify. Failing to
// register must not stop the page working.
if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("sw.js").catch(() => {});
}

connect();
