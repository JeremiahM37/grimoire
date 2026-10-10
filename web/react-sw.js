const CACHE = 'grimoire-react-a2d914eb40949ca34a1b';
const PRECACHE = ['/', '/manifest.webmanifest', '/icon.svg', '/apple-touch-icon.png', "/assets/index-BxkamyNa.js", "/assets/BanksPanel-BSUatzjM.js", "/assets/GraphPanel-A5qcEZTf.js", "/assets/HelpPanel-1qmKRzee.js", "/assets/Markdown-BHSa2ZtR.js", "/assets/OperatorSurfaces-1DHjlb4E.js", "/assets/Slides-B76a2nzK.js", "/assets/SyncSettings-BalxwFYw.js", "/assets/TagBrowser-Clopcz5Y.js", "/assets/TasksPanel-V-sEzJ9T.js", "/assets/knowledge-XbwWbqfC.js", "/assets/graph.worker-COffs3pt.js", "/assets/index-C4elHLME.css", "/vendor/editor.js"];
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