import { readFileSync } from 'node:fs';
import { defineConfig, type Plugin } from 'vite';

function reactServiceWorker(): Plugin {
  return {
    name: 'grimoire-react-service-worker',
    apply: 'build',
    generateBundle(_, bundle) {
      const editorSource = readFileSync(new URL('../web/vendor/editor.js', import.meta.url), 'utf8');
      this.emitFile({ type: 'asset', fileName: 'vendor/editor.js', source: editorSource });
      const assets = [...Object.keys(bundle), 'vendor/editor.js']
        .filter(name => /\.(?:js|css)$/.test(name))
        .map(name => '/' + name);
      const version = assets.join('|').replace(/[^a-zA-Z0-9]/g, '').slice(-32) || 'shell';
      const source = `const CACHE = 'grimoire-react-${version}';
const PRECACHE = ['/', '/manifest.webmanifest', '/icon.svg', ${assets.map(asset => JSON.stringify(asset)).join(', ')}];
self.addEventListener('install', event => event.waitUntil(caches.open(CACHE).then(cache => cache.addAll(PRECACHE)).then(() => self.skipWaiting())));
self.addEventListener('activate', event => event.waitUntil(caches.keys().then(keys => Promise.all(keys.filter(key => key.startsWith('grimoire-react-') && key !== CACHE).map(key => caches.delete(key)))).then(() => self.clients.claim())));
self.addEventListener('fetch', event => {
  if (event.request.method !== 'GET') return;
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith('/api/')) return;
  if (event.request.mode === 'navigate') {
    event.respondWith(fetch(event.request).catch(() => caches.match('/')));
    return;
  }
  event.respondWith(caches.match(event.request).then(hit => hit || fetch(event.request).then(response => {
    if (response.ok) void caches.open(CACHE).then(cache => cache.put(event.request, response.clone()));
    return response;
  })));
});`;
      this.emitFile({ type: 'asset', fileName: 'react-sw.js', source });
    },
  };
}

export default defineConfig({
  plugins: [reactServiceWorker()],
  build: { outDir: 'dist', emptyOutDir: true },
});
