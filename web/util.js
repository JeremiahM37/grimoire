/**
 * Shared low-level helpers: DOM query, authenticated API fetch, toasts,
 * HTML escaping, slugs. Imported by every other module — keep this file
 * dependency-free so nothing can cycle back into it.
 */
export const $ = (s) => document.querySelector(s);

/** Authenticated fetch against /api/*; throws Error(detail) on non-2xx. */
export async function api(path, opts = {}) {
  const r = await fetch(`/api${path}`, {
    headers: { "Content-Type": "application/json" },
    ...opts, body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  if (!r.ok) {
    let m = r.statusText; try { m = (await r.json()).detail || m; } catch {}
    throw new Error(m);
  }
  return r.status === 204 ? null : r.json();
}

export function toast(msg, err = false) {
  const t = document.createElement("div");
  t.className = "toast" + (err ? " err" : "");
  t.textContent = msg; $("#toast").appendChild(t);
  setTimeout(() => t.remove(), 3000);
}

/** HTML-escape untrusted text before it goes anywhere near innerHTML. */
export const esc = (s) => { const d = document.createElement("i"); d.textContent = s ?? ""; return d.innerHTML; };

/** A toast with one action button (e.g. "Moved to trash — Undo"). */
export function toastAction(msg, label, fn, ms = 6000) {
  const t = document.createElement("div");
  t.className = "toast";
  t.innerHTML = `<span>${esc(msg)}</span><button class="toast-btn">${esc(label)}</button>`;
  t.querySelector(".toast-btn").onclick = () => { t.remove(); fn(); };
  $("#toast").appendChild(t);
  setTimeout(() => t.remove(), ms);
}

export const slugify = (s) =>
  (s.toLowerCase().replace(/[^\w\s-]/g, "").trim().replace(/[\s_-]+/g, "-") || "untitled");
