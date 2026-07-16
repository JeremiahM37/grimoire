/* mnemo PWA — vanilla ES module, offline-capable, no build step */
const $ = (s) => document.querySelector(s);
const state = { path: null, notes: [], dirty: false, saveTimer: null, frontmatter: {} };

async function api(path, opts = {}) {
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
function toast(msg, err = false) {
  const t = document.createElement("div");
  t.className = "toast" + (err ? " err" : "");
  t.textContent = msg; $("#toast").appendChild(t);
  setTimeout(() => t.remove(), 3000);
}
const esc = (s) => { const d = document.createElement("i"); d.textContent = s ?? ""; return d.innerHTML; };

/* ---------- note list ---------- */
async function loadList() {
  state.notes = await api("/notes");
  renderList(state.notes);
  const h = await api("/health");
  state.rev = h.rev;
  $("#stat").textContent = `${h.notes} notes · ${h.tags} tags · ${h.unresolved_links} unlinked`;
}

/* Live sync: notice notes created/edited/deleted OUTSIDE this tab
   (device sync, MCP agent, external editor). Poll the cheap health rev
   only while the tab is visible; refresh the list when it changes. */
async function pollRev() {
  if (document.hidden || state.dirty) return;   // don't fight an in-progress edit
  try {
    const h = await api("/health");
    if (state.rev !== undefined && h.rev !== state.rev) {
      state.rev = h.rev;
      const notes = await api("/notes");
      state.notes = notes;
      if (!state.filterTag) renderList(notes);   // leave an active tag filter alone
      $("#stat").textContent = `${h.notes} notes · ${h.tags} tags · ${h.unresolved_links} unlinked`;
    } else {
      state.rev = h.rev;
    }
  } catch {}
}
setInterval(pollRev, 5000);
document.addEventListener("visibilitychange", () => { if (!document.hidden) pollRev(); });
function renderList(notes, snippets = false) {
  const el = $("#note-list");
  el.innerHTML = "";
  if (!notes.length) { el.innerHTML = '<div class="note-row m">No notes yet.</div>'; return; }
  for (const n of notes) {
    const row = document.createElement("div");
    row.className = "note-row" + (n.path === state.path ? " active" : "");
    row.innerHTML = `<div class="t">${esc(n.title || n.path)}</div>` +
      (snippets && n.snippet ? `<div class="snip">${n.snippet.replace(/\[(.*?)\]/g, "<b>$1</b>")}</div>`
        : `<div class="m">${esc(n.path)}</div>`);
    row.onclick = () => openNote(n.path);
    el.appendChild(row);
  }
}

/* ---------- open / save ---------- */
async function openNote(path) {
  if (state.dirty) await save();
  const n = await api(`/notes/${encodeURI(path)}`);
  state.path = n.path; state.dirty = false; state.frontmatter = n.frontmatter || {};
  $("#title").value = n.title || "";
  $("#content").value = n.body || "";
  updatePrivateToggle();
  renderBacklinks(n.backlinks || []);
  state.filterTag = null;
  $("#tag-filter-bar")?.remove();
  renderList(state.notes);
  setSaveState("");
  closeSidebarMobile();
  if (!$("#preview").classList.contains("hidden")) renderPreview();
  location.hash = encodeURI(path);
}
function setSaveState(s) { $("#save-state").textContent = s; }
function scheduleSave() {
  state.dirty = true; setSaveState("…");
  clearTimeout(state.saveTimer);
  state.saveTimer = setTimeout(save, 700);
}
async function save() {
  if (!state.path || !state.dirty) return;
  clearTimeout(state.saveTimer);
  try {
    const title = $("#title").value.trim();
    const fm = { ...state.frontmatter };
    if (title) fm.title = title; else delete fm.title;
    const n = await api(`/notes/${encodeURI(state.path)}`, {
      method: "PUT", body: { body: $("#content").value, frontmatter: fm } });
    state.dirty = false; setSaveState("saved");
    setTimeout(() => setSaveState(""), 1200);
    renderBacklinks(n && (await api(`/notes/${encodeURI(state.path)}`)).backlinks || []);
    loadList();
  } catch (e) { setSaveState("!"); toast(e.message, true); }
}

async function newNote() {
  const title = prompt("New note title:");
  if (!title) return;
  try {
    const n = await api("/notes", { method: "POST", body: { title, body: `# ${title}\n\n` } });
    await loadList(); openNote(n.path);
  } catch (e) { toast(e.message, true); }
}
async function openDaily() {
  const d = await api("/daily");
  await loadList(); openNote(d.path);
}
async function deleteNote() {
  if (!state.path || !confirm("Delete this note? The .md file is removed.")) return;
  await api(`/notes/${encodeURI(state.path)}`, { method: "DELETE" });
  state.path = null; state.dirty = false;
  $("#title").value = ""; $("#content").value = ""; $("#backlinks").innerHTML = "";
  toast("Deleted"); loadList();
}

/* ---------- backlinks ---------- */
function renderBacklinks(bl) {
  const el = $("#backlinks");
  if (!bl.length) { el.innerHTML = '<h4>Backlinks</h4><div class="empty">Nothing links here yet.</div>'; return; }
  el.innerHTML = "<h4>Backlinks</h4>" +
    bl.map((b) => `<a data-p="${esc(b.path)}">← ${esc(b.title)}</a>`).join("");
  el.querySelectorAll("a").forEach((a) => (a.onclick = () => openNote(a.dataset.p)));
}

/* ---------- preview (offline markdown → html) ---------- */
function renderPreview() {
  $("#preview").innerHTML = `<div class="md">${mdToHtml($("#content").value)}</div>`;
  $("#preview").querySelectorAll("a.wikilink").forEach((a) => {
    a.onclick = (e) => { e.preventDefault(); resolveAndOpen(a.dataset.target); };
  });
  $("#preview").querySelectorAll(".tag").forEach((t) => {
    t.style.cursor = "pointer";
    t.onclick = () => filterByTag(t.textContent.replace(/^#/, ""));
  });
}

async function filterByTag(tag) {
  const notes = await api(`/notes?tag=${encodeURIComponent(tag)}`);
  state.filterTag = tag;
  renderTagFilterBar(tag);
  renderList(notes);
  closeSidebarMobile();
  $("#sidebar").classList.add("open"); $("#app").classList.add("side-open");
}
function renderTagFilterBar(tag) {
  let bar = $("#tag-filter-bar");
  if (!bar) {
    bar = document.createElement("div"); bar.id = "tag-filter-bar";
    $("#note-list").before(bar);
  }
  bar.innerHTML = `<span>#${esc(tag)}</span><button id="clear-tag">✕ clear</button>`;
  $("#clear-tag").onclick = () => { state.filterTag = null; bar.remove(); loadList(); };
}
async function resolveAndOpen(target) {
  const hit = state.notes.find((n) => (n.title || "").toLowerCase() === target.toLowerCase()
    || n.path.replace(/\.md$/, "").split("/").pop().toLowerCase() === target.toLowerCase());
  if (hit) return openNote(hit.path);
  // create-on-click for unresolved links
  if (confirm(`"${target}" doesn't exist yet. Create it?`)) {
    const n = await api("/notes", { method: "POST", body: { title: target, body: `# ${target}\n\n` } });
    await loadList(); openNote(n.path);
  }
}
function mdToHtml(src) {
  // small, safe-ish markdown: escape first, then apply inline + block rules
  let resolved = new Set(state.notes.flatMap((n) => [
    (n.title || "").toLowerCase(), n.path.replace(/\.md$/, "").split("/").pop().toLowerCase()]));
  const lines = src.split("\n");
  let html = "", inCode = false, listOpen = false;
  const inline = (t) => esc(t)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\[\[([^\]|]+?)(?:\|([^\]]+))?\]\]/g, (_, tgt, al) => {
      const base = tgt.split("#")[0].trim();
      const cls = resolved.has(base.toLowerCase()) ? "wikilink" : "wikilink unresolved";
      return `<a class="${cls}" data-target="${esc(base)}">${esc(al || tgt)}</a>`;
    })
    .replace(/(^|\s)#([A-Za-z][\w/-]*)/g, '$1<span class="tag">#$2</span>')
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(?<!\*)\*([^*]+)\*(?!\*)/g, "<em>$1</em>")
    .replace(/\[([^\]]+)\]\((https?:[^)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  const closeList = () => { if (listOpen) { html += "</ul>"; listOpen = false; } };
  for (const raw of lines) {
    if (raw.trim().startsWith("```")) { closeList(); inCode = !inCode; html += inCode ? "<pre><code>" : "</code></pre>"; continue; }
    if (inCode) { html += esc(raw) + "\n"; continue; }
    const h = raw.match(/^(#{1,3})\s+(.+)$/);
    if (h) { closeList(); html += `<h${h[1].length}>${inline(h[2])}</h${h[1].length}>`; continue; }
    if (/^\s*[-*]\s+/.test(raw)) { if (!listOpen) { html += "<ul>"; listOpen = true; } html += `<li>${inline(raw.replace(/^\s*[-*]\s+/, ""))}</li>`; continue; }
    if (/^\s*>\s?/.test(raw)) { closeList(); html += `<blockquote>${inline(raw.replace(/^\s*>\s?/, ""))}</blockquote>`; continue; }
    if (raw.trim() === "") { closeList(); continue; }
    closeList(); html += `<p>${inline(raw)}</p>`;
  }
  closeList();
  return html;
}

/* ---------- search ---------- */
let searchTimer;
$("#search").oninput = (e) => {
  clearTimeout(searchTimer);
  const q = e.target.value.trim();
  if (state.filterTag) { state.filterTag = null; $("#tag-filter-bar")?.remove(); }
  searchTimer = setTimeout(async () => {
    if (!q) return renderList(state.notes);
    const res = await api(`/search?q=${encodeURIComponent(q)}`);
    renderList(res, true);
  }, 200);
};

/* ---------- [[ autocomplete ---------- */
const ta = $("#content");
ta.addEventListener("input", () => { scheduleSave(); maybeComplete(); });
ta.addEventListener("keydown", (e) => {
  const box = $("#complete");
  if (box.classList.contains("hidden")) return;
  const items = [...box.querySelectorAll(".c")];
  let i = items.findIndex((x) => x.classList.contains("sel"));
  if (e.key === "ArrowDown") { e.preventDefault(); i = (i + 1) % items.length; }
  else if (e.key === "ArrowUp") { e.preventDefault(); i = (i - 1 + items.length) % items.length; }
  else if (e.key === "Enter" || e.key === "Tab") { e.preventDefault(); if (items[i] || items[0]) (items[i] || items[0]).click(); return; }
  else if (e.key === "Escape") { hideComplete(); return; }
  else return;
  items.forEach((x) => x.classList.remove("sel")); if (items[i]) items[i].classList.add("sel");
});
async function maybeComplete() {
  const pos = ta.selectionStart;
  const before = ta.value.slice(0, pos);
  const m = before.match(/\[\[([^\]|\n]*)$/);
  if (!m) return hideComplete();
  const res = await api(`/complete?q=${encodeURIComponent(m[1])}`);
  const box = $("#complete");
  if (!res.length) return hideComplete();
  box.innerHTML = res.map((r, idx) =>
    `<div class="c${idx === 0 ? " sel" : ""}" data-stem="${esc(r.stem)}">${esc(r.title)}</div>`).join("");
  box.querySelectorAll(".c").forEach((c) => (c.onclick = () => insertLink(c.dataset.stem, m.index)));
  // position near the caret (approx: below the textarea top)
  const rect = ta.getBoundingClientRect();
  box.style.left = rect.left + 20 + "px";
  box.style.top = rect.top + 40 + "px";
  box.classList.remove("hidden");
}
function insertLink(stem, start) {
  const pos = ta.selectionStart;
  const before = ta.value.slice(0, pos).replace(/\[\[[^\]|\n]*$/, `[[${stem}]]`);
  ta.value = before + ta.value.slice(pos);
  const np = before.length;
  ta.selectionStart = ta.selectionEnd = np;
  hideComplete(); scheduleSave(); ta.focus();
}
function hideComplete() { $("#complete").classList.add("hidden"); }

/* ---------- private toggle ---------- */
function updatePrivateToggle() {
  const on = !!state.frontmatter.private;
  const b = $("#private-toggle");
  b.textContent = on ? "🔒" : "🔓";
  b.classList.toggle("on", on);
  b.title = on ? "private (hidden from AI)" : "make private";
}
$("#private-toggle").onclick = async () => {
  if (!state.path) return;
  state.frontmatter = { ...state.frontmatter, private: !state.frontmatter.private };
  if (!state.frontmatter.private) delete state.frontmatter.private;
  updatePrivateToggle();
  state.dirty = true; await save();
  toast(state.frontmatter.private ? "Private — excluded from AI" : "No longer private");
};

/* ---------- ask your notes ---------- */
$("#ask-open").onclick = () => { $("#ask-modal").classList.remove("hidden"); $("#ask-q").focus(); };
$("#ask-close").onclick = () => $("#ask-modal").classList.add("hidden");
$("#ask-modal").onclick = (e) => { if (e.target.id === "ask-modal") $("#ask-modal").classList.add("hidden"); };
$("#ask-q").onkeydown = (e) => { if (e.key === "Enter") doAsk(); };
$("#ask-go").onclick = doAsk;
async function doAsk() {
  const q = $("#ask-q").value.trim();
  if (!q) return;
  $("#ask-answer").innerHTML = '<span class="thinking">thinking…</span>';
  $("#ask-cites").innerHTML = "";
  try {
    const r = await api("/ask", { method: "POST",
      body: { q, include_private: $("#ask-priv").checked } });
    $("#ask-answer").textContent = r.answer;
    $("#ask-cites").innerHTML = r.citations.map((c) =>
      `<a class="cite" data-p="${esc(c.path)}">↳ ${esc(c.title)} <span class="sc">${(c.score * 100 | 0)}%</span></a>`).join("");
    $("#ask-cites").querySelectorAll(".cite").forEach((a) =>
      (a.onclick = () => { $("#ask-modal").classList.add("hidden"); openNote(a.dataset.p); }));
  } catch (e) { $("#ask-answer").textContent = "Error: " + e.message; }
}

/* ---------- audio memo ---------- */
let mediaRec = null, chunks = [];
$("#audio-memo").onclick = async () => {
  const btn = $("#audio-memo");
  if (mediaRec && mediaRec.state === "recording") { mediaRec.stop(); return; }
  try {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    mediaRec = new MediaRecorder(stream);
    chunks = [];
    mediaRec.ondataavailable = (e) => chunks.push(e.data);
    mediaRec.onstop = async () => {
      stream.getTracks().forEach((t) => t.stop());
      btn.textContent = "🎙 Memo"; btn.classList.remove("rec");
      const blob = new Blob(chunks, { type: "audio/webm" });
      const fd = new FormData(); fd.append("file", blob, "memo.webm");
      toast("Transcribing memo…");
      try {
        const r = await fetch("/api/audio", { method: "POST", body: fd }).then((x) => x.json());
        await loadList(); openNote(r.path); toast("Memo saved");
      } catch (e) { toast("Memo failed: " + e.message, true); }
    };
    mediaRec.start(); btn.textContent = "⏹ Stop"; btn.classList.add("rec");
    toast("Recording… tap Stop when done");
  } catch (e) { toast("Mic unavailable: " + e.message, true); }
};

/* ---------- secret vault ---------- */
$("#vault-open").onclick = openVault;
$("#vault-close").onclick = () => $("#vault-modal").classList.add("hidden");
$("#vault-modal").onclick = (e) => { if (e.target.id === "vault-modal") $("#vault-modal").classList.add("hidden"); };
async function openVault() {
  $("#vault-modal").classList.remove("hidden");
  const st = await api("/vault/status");
  const b = $("#vault-body");
  if (!st.initialized) {
    b.innerHTML = `<p class="vault-note">Set a passphrase to create your encrypted secret vault. It's never stored — if you forget it, the secrets are gone.</p>
      <div class="ask-input-row"><input id="v-pass" type="password" placeholder="new passphrase (8+ chars)">
      <button id="v-init" class="btn">Create</button></div>`;
    $("#v-init").onclick = async () => {
      try { await api("/vault/init", { method: "POST", body: { passphrase: $("#v-pass").value } }); openVault(); }
      catch (e) { toast(e.message, true); }
    };
  } else if (!st.unlocked) {
    b.innerHTML = `<p class="vault-note">Vault is locked.</p>
      <div class="ask-input-row"><input id="v-pass" type="password" placeholder="passphrase">
      <button id="v-unlock" class="btn">Unlock</button></div>`;
    $("#v-pass").onkeydown = (e) => { if (e.key === "Enter") $("#v-unlock").click(); };
    $("#v-unlock").onclick = async () => {
      try { await api("/vault/unlock", { method: "POST", body: { passphrase: $("#v-pass").value } }); openVault(); }
      catch (e) { toast(e.message, true); }
    };
  } else {
    const secrets = await api("/secrets");
    b.innerHTML = `<div class="vault-actions"><span class="vault-note">${secrets.length} secret(s) — values are never shown. Your AI can use them via scoped grants.</span>
      <button id="v-lock" class="icon" title="lock">🔒 Lock</button></div>
      <div id="v-list">${secrets.map((s) => `<div class="v-row"><span>🔑 ${esc(s.name)}</span>
        <button class="icon danger v-del" data-n="${esc(s.name)}">🗑</button></div>`).join("") || '<div class="vault-note">No secrets yet.</div>'}</div>
      <div class="ask-input-row"><input id="v-name" placeholder="name (e.g. github)">
      <input id="v-val" type="password" placeholder="value / token"><button id="v-add" class="btn">Add</button></div>`;
    $("#v-lock").onclick = async () => { await api("/vault/lock", { method: "POST" }); openVault(); toast("Vault locked"); };
    $("#v-add").onclick = async () => {
      const name = $("#v-name").value.trim(), value = $("#v-val").value;
      if (!name || !value) return toast("name and value required", true);
      try { await api("/secrets", { method: "POST", body: { name, value } }); openVault(); toast("Secret added"); }
      catch (e) { toast(e.message, true); }
    };
    b.querySelectorAll(".v-del").forEach((x) => (x.onclick = async () => {
      await api(`/secrets/${encodeURIComponent(x.dataset.n)}`, { method: "DELETE" }); openVault();
    }));
  }
}

/* ---------- inline AI actions ---------- */
$("#ai-btn").onclick = (e) => {
  const m = $("#ai-menu"), r = e.target.getBoundingClientRect();
  m.style.top = r.bottom + 4 + "px"; m.style.right = (innerWidth - r.right) + "px"; m.style.left = "";
  m.classList.toggle("hidden");
};
$("#ai-menu").querySelectorAll(".mi").forEach((mi) => (mi.onclick = () => runAction(mi.dataset.a)));
addEventListener("click", (e) => { if (!e.target.closest("#ai-btn,#ai-menu")) $("#ai-menu").classList.add("hidden"); });
async function runAction(action) {
  $("#ai-menu").classList.add("hidden");
  const sel = getSelection().toString();
  const text = sel || $("#content").value;
  if (!text.trim()) return toast("Nothing to work with", true);
  toast("✦ " + action + "…");
  try {
    const r = await api("/actions", { method: "POST", body: { action, text } });
    if (r.error) return toast(r.error, true);
    if (action === "tags") {
      const tags = r.result.map((t) => "#" + t).join(" ");
      insertAtEnd("\n\n" + tags + "\n"); toast("Tags added");
    } else {
      insertAtEnd("\n\n---\n" + r.result + "\n"); toast(action + " inserted");
    }
  } catch (e) { toast(e.message, true); }
}
function insertAtEnd(s) {
  const ta = $("#content"); ta.value = ta.value.replace(/\n+$/, "") + s;
  scheduleSave();
}

/* ---------- chrome ---------- */
$("#title").oninput = scheduleSave;
$("#new-note").onclick = newNote;
$("#daily").onclick = openDaily;
$("#delete-note").onclick = deleteNote;
$("#preview-toggle").onclick = () => {
  const pv = $("#preview"), tae = $("#content");
  if (pv.classList.contains("hidden")) { renderPreview(); pv.classList.remove("hidden"); tae.classList.add("hidden"); }
  else { pv.classList.add("hidden"); tae.classList.remove("hidden"); }
};
$("#menu-open").onclick = () => { $("#sidebar").classList.add("open"); $("#app").classList.add("side-open"); };
$("#menu-close").onclick = closeSidebarMobile;
function closeSidebarMobile() { $("#sidebar").classList.remove("open"); $("#app").classList.remove("side-open"); }
addEventListener("beforeunload", () => { if (state.dirty) save(); });
// deep-linkable notes: browser back/forward and shared #note URLs re-open the note
addEventListener("hashchange", () => {
  const h = decodeURI(location.hash.slice(1));
  if (h && h !== state.path && state.notes.some((n) => n.path === h)) openNote(h);
});
if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js");

async function handleShareTarget() {
  const p = new URLSearchParams(location.search);
  if (!p.has("text") && !p.has("url") && !p.has("title")) return false;
  const text = [p.get("text"), p.get("url")].filter(Boolean).join("\n\n");
  try {
    const r = await api("/capture", { method: "POST",
      body: { text: text || p.get("title"), title: p.get("title"), url: p.get("url"), source: "share" } });
    history.replaceState(null, "", "/");
    toast("Shared to mnemo"); await loadList(); openNote(r.path);
    return true;
  } catch { return false; }
}

(async function boot() {
  await loadList();
  if (await handleShareTarget()) return;
  const hash = decodeURI(location.hash.slice(1));
  if (hash && state.notes.some((n) => n.path === hash)) openNote(hash);
  else if (state.notes[0]) openNote(state.notes[0].path);
})();
