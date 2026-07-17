/* mnemo PWA — vanilla ES module, offline-capable, no build step */
const $ = (s) => document.querySelector(s);
const state = { path: null, notes: [], dirty: false, saveTimer: null, frontmatter: {}, templates: [] };

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
  state.locked = !!n.locked; state.encrypted = !!n.encrypted;
  $("#title").value = n.title || "";
  if (state.locked) {
    $("#content").value = "🔒 This note is encrypted at rest.\n\nUnlock the secret vault (🔐 in the sidebar) to view and edit it.";
    $("#content").readOnly = true;
  } else {
    $("#content").value = n.body || "";
    $("#content").readOnly = false;
  }
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
  if (!state.path || !state.dirty || state.locked) return;
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
  $("#preview").querySelectorAll(".task-box").forEach((box) => {
    box.onchange = () => toggleTask(+box.dataset.line, box.checked);
  });
}

// flip a `- [ ]` ↔ `- [x]` on a specific source line and persist
function toggleTask(lineNo, done) {
  const lines = $("#content").value.split("\n");
  if (lineNo < 0 || lineNo >= lines.length) return;
  lines[lineNo] = lines[lineNo].replace(/^(\s*[-*]\s+)\[[ xX]\]/,
    (_, pre) => pre + (done ? "[x]" : "[ ]"));
  $("#content").value = lines.join("\n");
  renderPreview();
  scheduleSave();
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
    .replace(/!\[\[([^\[\]|]+?)\]\]/g, (_, src) =>
      `<img class="embed" src="/api/file/${encodeURI(src.trim())}" alt="${esc(src.trim())}" loading="lazy">`)
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
  for (let lineNo = 0; lineNo < lines.length; lineNo++) {
    const raw = lines[lineNo];
    if (raw.trim().startsWith("```")) { closeList(); inCode = !inCode; html += inCode ? "<pre><code>" : "</code></pre>"; continue; }
    if (inCode) { html += esc(raw) + "\n"; continue; }
    const h = raw.match(/^(#{1,3})\s+(.+)$/);
    if (h) { closeList(); html += `<h${h[1].length}>${inline(h[2])}</h${h[1].length}>`; continue; }
    const task = raw.match(/^\s*[-*]\s+\[([ xX])\]\s+(.*)$/);
    if (task) {
      if (!listOpen) { html += "<ul>"; listOpen = true; }
      const done = task[1].toLowerCase() === "x";
      html += `<li class="task${done ? " done" : ""}"><input type="checkbox" class="task-box" `
        + `data-line="${lineNo}"${done ? " checked" : ""}>${inline(task[2])}</li>`;
      continue;
    }
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

/* ---------- editor: toolbar, smart lists, tab ---------- */
const ta = $("#content");
function surround(pre, post = pre, placeholder = "") {
  const s = ta.selectionStart, e = ta.selectionEnd, v = ta.value;
  const sel = v.slice(s, e) || placeholder;
  ta.value = v.slice(0, s) + pre + sel + post + v.slice(e);
  ta.selectionStart = s + pre.length;
  ta.selectionEnd = s + pre.length + sel.length;
  ta.focus(); scheduleSave();
}
function prefixLine(prefix) {
  const s = ta.selectionStart, v = ta.value;
  const lineStart = v.lastIndexOf("\n", s - 1) + 1;
  ta.value = v.slice(0, lineStart) + prefix + v.slice(lineStart);
  ta.selectionStart = ta.selectionEnd = s + prefix.length;
  ta.focus(); scheduleSave();
}
const TB = {
  bold: () => surround("**", "**", "bold"),
  italic: () => surround("*", "*", "italic"),
  code: () => surround("`", "`", "code"),
  link: () => surround("[[", "]]", "note"),
  h: () => prefixLine("# "),
  ul: () => prefixLine("- "),
  task: () => prefixLine("- [ ] "),
  quote: () => prefixLine("> "),
};
$("#ed-toolbar").querySelectorAll(".tb").forEach((b) =>
  (b.onmousedown = (e) => { e.preventDefault(); TB[b.dataset.md]?.(); }));

function insertAtCursor(text) {
  const s = ta.selectionStart, e = ta.selectionEnd, v = ta.value;
  ta.value = v.slice(0, s) + text + v.slice(e);
  ta.selectionStart = ta.selectionEnd = s + text.length;
  ta.focus(); scheduleSave();
}
async function uploadAttachment(file) {
  const fd = new FormData();
  fd.append("file", file, file.name || "pasted.png");
  toast("Uploading…");
  try {
    const r = await fetch("/api/attach", { method: "POST", body: fd });
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).detail || r.statusText);
    const j = await r.json();
    insertAtCursor((j.is_image ? "!" : "") + `[[${j.path}]]`);
    toast(j.is_image ? "Image embedded" : "File attached");
  } catch (err) { toast("Upload failed: " + err.message, true); }
}
// paste an image straight into a note
ta.addEventListener("paste", (e) => {
  const item = [...(e.clipboardData?.items || [])].find((i) => i.kind === "file" && i.type.startsWith("image/"));
  if (!item) return;
  e.preventDefault();
  const f = item.getAsFile(); if (f) uploadAttachment(f);
});
// drag-and-drop files onto the editor
["dragover", "drop"].forEach((ev) => ta.addEventListener(ev, (e) => {
  if (e.dataTransfer && [...e.dataTransfer.types].includes("Files")) e.preventDefault();
}));
ta.addEventListener("drop", (e) => {
  const files = [...(e.dataTransfer?.files || [])];
  if (files.length) { e.preventDefault(); files.forEach(uploadAttachment); }
});

// Enter continues lists/tasks; Tab indents — only when autocomplete isn't showing
ta.addEventListener("keydown", (e) => {
  if (!$("#complete").classList.contains("hidden")) return;   // let autocomplete win
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "b") { e.preventDefault(); return TB.bold(); }
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "i") { e.preventDefault(); return TB.italic(); }
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "l") { e.preventDefault(); return TB.link(); }
  const v = ta.value, s = ta.selectionStart;
  const lineStart = v.lastIndexOf("\n", s - 1) + 1;
  const line = v.slice(lineStart, s);
  if (e.key === "Tab") {
    e.preventDefault();
    if (e.shiftKey) {
      if (v.slice(lineStart, lineStart + 2) === "  ") {
        ta.value = v.slice(0, lineStart) + v.slice(lineStart + 2);
        ta.selectionStart = ta.selectionEnd = Math.max(lineStart, s - 2);
      }
    } else {
      ta.value = v.slice(0, s) + "  " + v.slice(ta.selectionEnd);
      ta.selectionStart = ta.selectionEnd = s + 2;
    }
    scheduleSave(); return;
  }
  if (e.key === "Enter" && !e.shiftKey) {
    const m = line.match(/^(\s*)([-*]\s(?:\[[ xX]\]\s)?)(.*)$/);
    if (m) {
      e.preventDefault();
      if (m[3].trim() === "") {
        // empty list item → end the list (clear the marker)
        ta.value = v.slice(0, lineStart) + v.slice(s);
        ta.selectionStart = ta.selectionEnd = lineStart;
      } else {
        // continue the list; a checked task continues as an unchecked one
        const marker = m[2].replace(/\[[xX]\]/, "[ ]");
        const ins = "\n" + m[1] + marker;
        ta.value = v.slice(0, s) + ins + v.slice(ta.selectionEnd);
        ta.selectionStart = ta.selectionEnd = s + ins.length;
      }
      scheduleSave(); return;
    }
  }
});

/* ---------- [[ autocomplete ---------- */
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

/* ---------- command palette (Ctrl/Cmd-K quick switcher) ---------- */
const COMMANDS = [
  { icon: "＋", name: "New note", run: newNote },
  { icon: "◈", name: "Open today's daily note", run: openDaily },
  { icon: "✦", name: "Ask your notes", run: () => $("#ask-open").click() },
  { icon: "◉", name: "Open graph view", run: openGraph },
  { icon: "◐", name: "Toggle preview", run: () => $("#preview-toggle").click() },
  { icon: "🔐", name: "Open secret vault", run: openVault },
  { icon: "🎙", name: "Record audio memo", run: () => $("#audio-memo").click() },
  { icon: "🗂", name: "Save current note as template", run: saveAsTemplate },
  { icon: "⇩", name: "Export note as HTML (print to PDF)", run: exportNote },
  { icon: "⚙", name: "Open settings", run: openSettings },
  { icon: "🔒", name: "Encrypt this note (at rest)", run: () => cryptNote("encrypt") },
  { icon: "🔓", name: "Decrypt this note", run: () => cryptNote("decrypt") },
];
async function cryptNote(which) {
  if (!state.path) return toast("Open a note first", true);
  if (state.dirty) await save();
  try {
    await api(`/notes/${encodeURI(state.path)}/${which}`, { method: "POST" });
    toast(which === "encrypt" ? "Encrypted at rest 🔒" : "Decrypted 🔓");
    openNote(state.path);
  } catch (e) {
    if (/423|lock/i.test(e.message)) { toast("Unlock the secret vault first", true); openVault(); }
    else toast(e.message, true);
  }
}
function exportNote() {
  if (!state.path) return toast("Open a note first", true);
  window.open(`/notes/${encodeURI(state.path)}/export.html`, "_blank");
}
async function refreshTemplates() {
  try { state.templates = await api("/templates"); } catch { state.templates = []; }
}
async function saveAsTemplate() {
  if (!state.path) return toast("Open a note first", true);
  const name = prompt("Template name:", $("#title").value || "");
  if (!name) return;
  try {
    await api("/templates", { method: "POST", body: { name, body: $("#content").value } });
    await refreshTemplates(); toast(`Saved template “${name}”`);
  } catch (e) { toast(e.message, true); }
}
async function newFromTemplate(tplPath) {
  const title = prompt("Title for the new note:");
  if (!title) return;
  try {
    const n = await api("/templates/apply", { method: "POST", body: { template: tplPath, title } });
    await loadList(); openNote(n.path);
  } catch (e) { toast(e.message, true); }
}
let palIdx = 0, palItems = [];
function openPalette() {
  $("#palette").classList.remove("hidden");
  const inp = $("#palette-input"); inp.value = ""; inp.focus();
  renderPalette("");
}
function closePalette() { $("#palette").classList.add("hidden"); }
function fuzzy(needle, hay) {
  needle = needle.toLowerCase(); hay = hay.toLowerCase();
  if (!needle) return 1;
  let i = 0, score = 0, streak = 0;
  for (const ch of hay) {
    if (i < needle.length && ch === needle[i]) { i++; streak++; score += streak; }
    else streak = 0;
  }
  return i === needle.length ? score : 0;
}
function renderPalette(q) {
  const cmds = COMMANDS.map((c) => ({ ...c, kind: "cmd", label: c.name, s: fuzzy(q, c.name) }));
  const notes = state.notes.map((n) => ({
    kind: "note", label: n.title || n.path, path: n.path,
    s: fuzzy(q, (n.title || "") + " " + n.path) }));
  const tpls = (state.templates || []).map((t) => ({
    kind: "template", label: `New from: ${t.name}`, tpl: t.path,
    s: fuzzy(q, "new from template " + t.name) }));
  palItems = [...cmds, ...tpls, ...notes].filter((x) => x.s > 0)
    .sort((a, b) => b.s - a.s).slice(0, 40);
  palIdx = 0;
  const el = $("#palette-list");
  const ICON = { cmd: (it) => it.icon, template: () => "🗂", note: () => "◦" };
  const KIND = { cmd: "command", template: "template", note: "note" };
  el.innerHTML = palItems.map((it, i) =>
    `<div class="pal-item${i === 0 ? " sel" : ""}" data-i="${i}">`
    + `<span class="pk">${(ICON[it.kind] || ICON.note)(it)}</span>`
    + `<span>${esc(it.label)}</span>`
    + `<span class="pm">${KIND[it.kind] || ""}</span>`
    + `</div>`).join("") || '<div class="pal-item">No matches</div>';
  el.querySelectorAll(".pal-item[data-i]").forEach((d) =>
    (d.onclick = () => runPalette(+d.dataset.i)));
}
function runPalette(i) {
  const it = palItems[i]; if (!it) return;
  closePalette();
  if (it.kind === "note") openNote(it.path);
  else if (it.kind === "template") newFromTemplate(it.tpl);
  else it.run();
}
$("#palette-open").onclick = openPalette;
$("#palette-input").oninput = (e) => renderPalette(e.target.value.trim());
$("#palette").onclick = (e) => { if (e.target.id === "palette") closePalette(); };
$("#palette-input").onkeydown = (e) => {
  const items = [...$("#palette-list").querySelectorAll(".pal-item[data-i]")];
  if (e.key === "ArrowDown") { e.preventDefault(); palIdx = Math.min(palIdx + 1, items.length - 1); }
  else if (e.key === "ArrowUp") { e.preventDefault(); palIdx = Math.max(palIdx - 1, 0); }
  else if (e.key === "Enter") { e.preventDefault(); return runPalette(palIdx); }
  else if (e.key === "Escape") { return closePalette(); }
  else return;
  items.forEach((x) => x.classList.remove("sel"));
  if (items[palIdx]) { items[palIdx].classList.add("sel"); items[palIdx].scrollIntoView({ block: "nearest" }); }
};
addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    $("#palette").classList.contains("hidden") ? openPalette() : closePalette();
  }
});

/* ---------- graph view (canvas force-directed, no deps) ---------- */
let graphAnim = null;
$("#graph-open").onclick = openGraph;
$("#graph-close").onclick = closeGraph;
$("#graph-modal").onclick = (e) => { if (e.target.id === "graph-modal") closeGraph(); };
function closeGraph() {
  $("#graph-modal").classList.add("hidden");
  if (graphAnim) { cancelAnimationFrame(graphAnim); graphAnim = null; }
}
async function openGraph() {
  const g = await api("/graph");
  $("#graph-modal").classList.remove("hidden");
  const cv = $("#graph-canvas"), ctx = cv.getContext("2d");
  const dpr = devicePixelRatio || 1;
  const fit = () => {
    const r = cv.getBoundingClientRect();
    cv.width = r.width * dpr; cv.height = r.height * dpr;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    return { w: r.width, h: r.height };
  };
  let { w, h } = fit();
  const idx = new Map(g.nodes.map((n, i) => [n.id, i]));
  const deg = new Map();
  for (const e of g.edges) { deg.set(e.src, (deg.get(e.src) || 0) + 1); deg.set(e.dst, (deg.get(e.dst) || 0) + 1); }
  // deterministic seed positions (no Math.random — spread on a phyllotaxis spiral)
  const N = g.nodes.length || 1;
  const nodes = g.nodes.map((n, i) => {
    const a = i * 2.399963, rad = 10 + 16 * Math.sqrt(i);
    return { ...n, x: w / 2 + rad * Math.cos(a), y: h / 2 + rad * Math.sin(a),
             vx: 0, vy: 0, d: deg.get(n.id) || 0 };
  });
  const edges = g.edges.filter((e) => idx.has(e.src) && idx.has(e.dst))
    .map((e) => [idx.get(e.src), idx.get(e.dst)]);
  $("#graph-stat").textContent = `${nodes.length} notes · ${edges.length} links`;

  let alpha = 1;
  const step = () => {
    alpha *= 0.985;
    const k = 0.9;
    // repulsion (O(n²) — fine for a personal vault)
    for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) {
      const a = nodes[i], b = nodes[j];
      let dx = a.x - b.x, dy = a.y - b.y, d2 = dx * dx + dy * dy || 0.01;
      const f = (2600 * alpha) / d2, d = Math.sqrt(d2);
      dx /= d; dy /= d; a.vx += dx * f; a.vy += dy * f; b.vx -= dx * f; b.vy -= dy * f;
    }
    // spring attraction along links
    for (const [i, j] of edges) {
      const a = nodes[i], b = nodes[j];
      let dx = b.x - a.x, dy = b.y - a.y, d = Math.hypot(dx, dy) || 0.01;
      const f = (d - 70) * 0.02 * alpha; dx /= d; dy /= d;
      a.vx += dx * f; a.vy += dy * f; b.vx -= dx * f; b.vy -= dy * f;
    }
    // gravity to center + integrate
    for (const n of nodes) {
      n.vx += (w / 2 - n.x) * 0.002 * alpha; n.vy += (h / 2 - n.y) * 0.002 * alpha;
      n.x += n.vx * k; n.y += n.vy * k; n.vx *= 0.85; n.vy *= 0.85;
      n.x = Math.max(14, Math.min(w - 14, n.x)); n.y = Math.max(14, Math.min(h - 14, n.y));
    }
    draw();
    if (alpha > 0.02) graphAnim = requestAnimationFrame(step);
  };
  const cssVar = (v) => getComputedStyle(document.body).getPropertyValue(v).trim();
  function draw() {
    ctx.clearRect(0, 0, w, h);
    ctx.strokeStyle = cssVar("--line"); ctx.lineWidth = 1; ctx.globalAlpha = 0.7;
    for (const [i, j] of edges) { ctx.beginPath(); ctx.moveTo(nodes[i].x, nodes[i].y); ctx.lineTo(nodes[j].x, nodes[j].y); ctx.stroke(); }
    ctx.globalAlpha = 1;
    for (const n of nodes) {
      const r = 4 + Math.min(9, n.d * 1.6);
      const active = n.id === state.path;
      ctx.beginPath(); ctx.arc(n.x, n.y, r, 0, 7);
      ctx.fillStyle = active ? cssVar("--accent") : cssVar("--link"); ctx.fill();
      if (r >= 7 || active) {
        ctx.fillStyle = cssVar("--ink"); ctx.font = "12px var(--sans)"; ctx.textAlign = "center";
        ctx.fillText((n.title || n.id).slice(0, 22), n.x, n.y - r - 4);
      }
    }
  }
  cv.onclick = (ev) => {
    const r = cv.getBoundingClientRect(), mx = ev.clientX - r.left, my = ev.clientY - r.top;
    let hit = null, best = 400;
    for (const n of nodes) { const d = (n.x - mx) ** 2 + (n.y - my) ** 2; if (d < best) { best = d; hit = n; } }
    if (hit) { closeGraph(); openNote(hit.id); }
  };
  addEventListener("resize", () => { if (!$("#graph-modal").classList.contains("hidden")) ({ w, h } = fit()); }, { once: true });
  draw();   // paint an initial frame immediately (before the first rAF tick)
  step();
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

/* ---------- outline / table of contents ---------- */
function buildOutline() {
  const lines = $("#content").value.split("\n");
  const items = [];
  let inCode = false;
  lines.forEach((ln, i) => {
    if (ln.trim().startsWith("```")) { inCode = !inCode; return; }
    if (inCode) return;
    const m = ln.match(/^(#{1,3})\s+(.+)$/);
    if (m) items.push({ level: m[1].length, text: m[2].trim(), line: i, hIdx: items.length });
  });
  return items;
}
$("#outline-btn").onclick = (e) => {
  const box = $("#outline");
  const items = buildOutline();
  box.innerHTML = items.length
    ? items.map((it) =>
        `<div class="mi ol-l${it.level}" data-line="${it.line}" data-h="${it.hIdx}">${esc(it.text)}</div>`).join("")
    : '<div class="mi empty">No headings</div>';
  box.querySelectorAll(".mi[data-line]").forEach((d) =>
    (d.onclick = () => { scrollToHeading(+d.dataset.line, +d.dataset.h); box.classList.add("hidden"); }));
  const r = e.target.getBoundingClientRect();
  box.style.top = r.bottom + 4 + "px"; box.style.right = (innerWidth - r.right) + "px"; box.style.left = "";
  box.classList.toggle("hidden");
};
addEventListener("click", (e) => { if (!e.target.closest("#outline-btn,#outline")) $("#outline").classList.add("hidden"); });
function scrollToHeading(lineNo, hIdx) {
  if (!$("#preview").classList.contains("hidden")) {
    const hs = $("#preview").querySelectorAll("h1,h2,h3");
    if (hs[hIdx]) hs[hIdx].scrollIntoView({ behavior: "smooth", block: "start" });
    return;
  }
  // edit mode: put the caret on the heading line and scroll it into view
  const lines = $("#content").value.split("\n");
  const offset = lines.slice(0, lineNo).reduce((a, l) => a + l.length + 1, 0);
  ta.focus();
  ta.selectionStart = ta.selectionEnd = offset;
  const style = getComputedStyle(ta);
  const lh = parseFloat(style.lineHeight) || 24;
  ta.scrollTop = Math.max(0, lineNo * lh - ta.clientHeight / 3);
}

/* ---------- settings ---------- */
$("#settings-close").onclick = () => $("#settings-modal").classList.add("hidden");
$("#settings-modal").onclick = (e) => { if (e.target.id === "settings-modal") $("#settings-modal").classList.add("hidden"); };
async function openSettings() {
  $("#settings-modal").classList.remove("hidden");
  const st = await api("/settings");
  const s = st.settings;
  const opt = (v, cur) => `<option value="${v}"${v === cur ? " selected" : ""}>${v || "auto"}</option>`;
  $("#settings-body").innerHTML = `
    <p class="vault-note">AI answers currently use: <b>${esc(st.answer_backend)}</b>${st.answer_backend === "extractive" ? " (no LLM reachable — set an Ollama URL below for generative answers)" : ""}.</p>
    <label class="set-row"><span>Answer backend</span>
      <select id="set-llm">${["", "ollama", "claude"].map((v) => opt(v, s.llm)).join("")}</select></label>
    <label class="set-row"><span>Answer model</span>
      <input id="set-model" value="${esc(s.llm_model)}" placeholder="qwen3.5:4b"></label>
    <label class="set-row"><span>Ollama URL</span>
      <input id="set-ollama" value="${esc(s.ollama_url)}" placeholder="http://host:11434"></label>
    <label class="set-row"><span>Whisper URL</span>
      <input id="set-whisper" value="${esc(s.whisper_url)}" placeholder="(optional) OpenAI-compatible"></label>
    <p class="vault-note">Embedding model: <code>${esc(s.embed_model)}</code> (fixed — changing it needs a reindex).</p>
    <div class="ask-input-row"><button id="set-save" class="btn full">Save</button></div>`;
  $("#set-save").onclick = async () => {
    try {
      const r = await api("/settings", { method: "PUT", body: {
        llm: $("#set-llm").value, llm_model: $("#set-model").value.trim(),
        ollama_url: $("#set-ollama").value.trim(), whisper_url: $("#set-whisper").value.trim() } });
      toast(`Saved — answers: ${r.answer_backend}`);
      $("#settings-modal").classList.add("hidden");
    } catch (e) { toast(e.message, true); }
  };
}

/* ---------- theme (auto / light / dark, persisted) ---------- */
const THEMES = ["auto", "light", "dark"];
const THEME_ICON = { auto: "◐", light: "☀", dark: "☾" };
function applyTheme(t) {
  if (t === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", t);
  const btn = $("#theme-toggle");
  if (btn) { btn.textContent = THEME_ICON[t] || "◐"; btn.title = `theme: ${t}`; }
}
$("#theme-toggle").onclick = () => {
  const cur = localStorage.getItem("mnemo-theme") || "auto";
  const next = THEMES[(THEMES.indexOf(cur) + 1) % THEMES.length];
  localStorage.setItem("mnemo-theme", next);
  applyTheme(next); toast(`Theme: ${next}`);
};
applyTheme(localStorage.getItem("mnemo-theme") || "auto");

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
  refreshTemplates();
  if (await handleShareTarget()) return;
  const hash = decodeURI(location.hash.slice(1));
  if (hash && state.notes.some((n) => n.path === hash)) openNote(hash);
  else if (state.notes[0]) openNote(state.notes[0].path);
})();
