/* Grimoire service worker — offline shell */
const CACHE = "grimoire-v19";
const SHELL = ["/", "/style.css", "/app.js", "/editor.js", "/plugins.js", "/canvas.js", "/util.js", "/markdown.js", "/signin.js", "/vendor/editor.js", "/icon.svg", "/manifest.webmanifest"];
self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)));
  self.skipWaiting();
});
self.addEventListener("activate", (e) => {
  e.waitUntil(caches.keys().then((ks) => Promise.all(ks.filter((k) => k !== CACHE).map((k) => caches.delete(k)))));
  self.clients.claim();
});
self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith("/api")) return;   // never cache data
  e.respondWith(
    // revalidate against the server rather than reusing a heuristically-cached
    // copy — otherwise this network-first handler still returns stale shell
    // assets straight out of the browser's HTTP cache after a deploy
    fetch(e.request, { cache: "no-cache" }).then((r) => {
      const copy = r.clone(); caches.open(CACHE).then((c) => c.put(e.request, copy)); return r;
    }).catch(() => caches.match(e.request).then((m) => m || caches.match("/")))
  );
});
