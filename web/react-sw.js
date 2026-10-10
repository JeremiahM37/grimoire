const CACHE = 'grimoire-react-e4e79c3b379a0f4d1f14';
const PRECACHE = ['/', '/manifest.webmanifest', '/icon.svg', '/apple-touch-icon.png', "/assets/index-8VrtPX8O.js", "/assets/BanksPanel-DtJK7zek.js", "/assets/DisputesPanel-CQPFXgsp.js", "/assets/GraphPanel-CIUyG_i5.js", "/assets/HelpPanel-PNH_cv0F.js", "/assets/Markdown-CQstWfci.js", "/assets/MemoryUsePanel-Da_cP3qQ.js", "/assets/OperatorSurfaces-CLNy_DYP.js", "/assets/Slides-BSetdS1v.js", "/assets/SyncSettings-DlPl_zgt.js", "/assets/TagBrowser-BhZLUGU4.js", "/assets/TasksPanel-DP9X1S2Z.js", "/assets/knowledge-BrYF8Wa-.js", "/assets/GraphPanel-B8FRyQwo.css", "/assets/graph.worker-COffs3pt.js", "/assets/index-CwohHZ15.css", "/vendor/editor.js"];
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