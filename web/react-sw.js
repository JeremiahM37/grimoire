const CACHE = 'grimoire-react-7156abb149e150e96e7f';
const PRECACHE = ['/', '/manifest.webmanifest', '/icon.svg', '/apple-touch-icon.png', "/assets/index-DL6dMCHi.js", "/assets/BanksPanel-K82zgNFo.js", "/assets/DisputesPanel-Ut_-riFI.js", "/assets/GraphPanel-DJ7l0Rc-.js", "/assets/HelpPanel-C-BI5q7m.js", "/assets/Markdown-Bv19ZReg.js", "/assets/MemoryUsePanel-DNeIMCJ7.js", "/assets/OperatorSurfaces-xQRrnz-3.js", "/assets/Slides-C9t03onV.js", "/assets/SyncSettings-158vMsDG.js", "/assets/TagBrowser-DJhnX_Oo.js", "/assets/TasksPanel-Bgr3JEuy.js", "/assets/knowledge-B52VgT41.js", "/assets/GraphPanel-B8FRyQwo.css", "/assets/graph.worker-COffs3pt.js", "/assets/index-BgAswdmj.css", "/vendor/editor.js"];
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