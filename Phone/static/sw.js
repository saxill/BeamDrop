// Caches the app shell so opening beamdrop from the home screen shows the
// interface immediately rather than a white page while the laptop is
// found — and shows something rather than a browser error when the laptop
// is off entirely.
//
// Only the shell. Nothing that has been transferred is stored here: files
// arrive over the WebSocket into memory and are saved by you, and putting
// them in a cache would quietly keep copies on the phone that nothing ever
// clears.

const CACHE = 'beamdrop-shell-v3';
const SHELL = [
  './',
  './index.html',
  './app.js',
  './transfer.js',
  './protocol.js',
  './pairing.js',
  './pairing-protocol.js',
  './vendor/beamdrop-crypto.mjs',
  './manifest.webmanifest',
  './icons/icon-192.png',
];

self.addEventListener('install', (e) => {
  // One missing file must not fail the whole install and leave the app
  // uncacheable, so each is added individually and failures are ignored.
  e.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    await Promise.all(SHELL.map((url) => cache.add(url).catch(() => {})));
    self.skipWaiting();
  })());
});

self.addEventListener('activate', (e) => {
  e.waitUntil((async () => {
    for (const key of await caches.keys()) {
      if (key !== CACHE) await caches.delete(key);
    }
    await self.clients.claim();
  })());
});

// A push is the only thing that reaches the phone while the app is closed —
// iOS suspends a home-screen app within seconds of backgrounding it, taking
// the WebSocket with it.
self.addEventListener('push', (e) => {
  let n = { title: 'beamdrop', body: 'Something arrived' };
  try {
    if (e.data) n = { ...n, ...e.data.json() };
  } catch (err) {
    // A push whose body we cannot parse still means something happened, so
    // show the default rather than nothing. Showing nothing is also a spec
    // violation: permission was granted on the promise that every push is
    // user-visible, and browsers revoke it for apps that stay silent.
  }
  e.waitUntil(self.registration.showNotification(n.title, {
    body: n.body,
    icon: './icons/icon-192.png',
    badge: './icons/icon-192.png',
    // Same tag replaces rather than stacks, so ten photos are one line
    // instead of ten.
    tag: n.tag || 'beamdrop',
    renotify: true,
  }));
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  // Focus the app if it is already open rather than opening a second copy.
  e.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const c of all) {
      if ('focus' in c) return c.focus();
    }
    if (self.clients.openWindow) return self.clients.openWindow('./');
  })());
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  // Never cache the socket or anything with a token in it.
  if (e.request.method !== 'GET' || url.pathname.startsWith('/ws') ||
      url.pathname.startsWith('/api') || url.pathname.startsWith('/upload') ||
      url.searchParams.has('token')) {
    return;
  }
  // Network first, so a rebuilt page is picked up on the next load rather
  // than being pinned to whatever was cached the first time.
  e.respondWith((async () => {
    try {
      const fresh = await fetch(e.request);
      const cache = await caches.open(CACHE);
      cache.put(e.request, fresh.clone()).catch(() => {});
      return fresh;
    } catch (err) {
      const hit = await caches.match(e.request);
      if (hit) return hit;
      throw err;
    }
  })());
});
