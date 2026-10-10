const CACHE = 'grimoire-react-79539ac8e311e8ea5fd9';
const PRECACHE = ['/', '/manifest.webmanifest', '/icon.svg', '/apple-touch-icon.png', "/assets/index-430qmwLz.js", "/assets/BanksPanel-CJ9oV3Xf.js", "/assets/DisputesPanel-gikGnQPc.js", "/assets/GraphPanel-CRVWLLTi.js", "/assets/HelpPanel-vTHQhKqR.js", "/assets/Markdown-CE-yxpaC.js", "/assets/MemoryUsePanel-D2aHUcUi.js", "/assets/OperatorSurfaces-Br_OFOhX.js", "/assets/Slides-BkJRBnCS.js", "/assets/SyncSettings-DwKHElQB.js", "/assets/TagBrowser-DKd85aZF.js", "/assets/TasksPanel-CXNM4Ktq.js", "/assets/knowledge-QZ6jZkT-.js", "/assets/graph.worker-COffs3pt.js", "/assets/index-CXHsz7B7.css", "/vendor/editor.js"];
self.addEventListener('install', event => event.waitUntil(caches.open(CACHE).then(cache => cache.addAll(PRECACHE)).then(() => self.skipWaiting())));
self.addEventListener('activate', event => event.waitUntil(caches.keys().then(keys => Promise.all(keys.filter(key => key.startsWith('grimoire-react-') && key !== CACHE).map(key => caches.delete(key)))).then(() => self.clients.claim())));
self.addEventListener('fetch', event => {
  if (event.request.method !== 'GET') return;
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith('/api/')) return;
  if (event.request.mode === 'navigate' && (url.pathname === '/' || url.pathname === '/index.html')) {
    // The cached shell is versioned with its assets (a deploy changes this file, so a new worker
    // installs a fresh copy): serve it at once instead of waiting on the network.
    event.respondWith(caches.match('/').then(hit => hit || fetch(event.request)));
    return;
  }
  if (!PRECACHE.includes(url.pathname) || url.pathname === '/') return;
  event.respondWith(caches.match(event.request).then(hit => hit || fetch(event.request).then(response => {
    if (response.ok) void caches.open(CACHE).then(cache => cache.put(event.request, response.clone()));
    return response;
  })));
});