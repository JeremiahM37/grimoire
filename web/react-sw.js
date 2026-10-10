const CACHE = 'grimoire-react-d078dd31136063e5265e';
const PRECACHE = ['/', '/manifest.webmanifest', '/icon.svg', '/apple-touch-icon.png', "/assets/index-BC3GIfHk.js", "/assets/BanksPanel-BDCF_dEy.js", "/assets/GraphPanel-iYUfYtHa.js", "/assets/HelpPanel-DQPHsYvH.js", "/assets/Markdown-Bkd0q9ZX.js", "/assets/MemoryUsePanel-Bkj5thPI.js", "/assets/OperatorSurfaces-Dm6s384L.js", "/assets/Slides-D5ZZ2aRK.js", "/assets/SyncSettings-BirtFoqJ.js", "/assets/TagBrowser-BX-JWG5p.js", "/assets/TasksPanel-C77HHxDs.js", "/assets/knowledge-B9286WdO.js", "/assets/graph.worker-COffs3pt.js", "/assets/index-a_VcpSrL.css", "/vendor/editor.js"];
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