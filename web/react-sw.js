const CACHE = 'grimoire-react-718bc5f00d07dc4196a6';
const PRECACHE = ['/', '/manifest.webmanifest', '/icon.svg', '/apple-touch-icon.png', "/assets/index-B9lKq8Ip.js", "/assets/BanksPanel-Cgcd6L4k.js", "/assets/DisputesPanel-DZupPkFg.js", "/assets/GraphPanel-DrsSZLhv.js", "/assets/HelpPanel--lLEWPfV.js", "/assets/Markdown-DwHbhT_9.js", "/assets/MemoryUsePanel-CxVGNHoW.js", "/assets/OperatorSurfaces-CsAIsskk.js", "/assets/Slides-BLM_x0rs.js", "/assets/SyncSettings-CgORv9gc.js", "/assets/TagBrowser-BaiJysgL.js", "/assets/TasksPanel-slwZHzTk.js", "/assets/knowledge-D6viE8qW.js", "/assets/GraphPanel-B8FRyQwo.css", "/assets/graph.worker-COffs3pt.js", "/assets/index-CwohHZ15.css", "/vendor/editor.js"];
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