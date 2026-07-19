/* Grimoire console PWA — vanilla ES module, offline-capable, no build step
   (the one vendored artifact is the optional CM6 live editor, see web/editor.js) */
import { $, api, toast, toastAction, esc, slugify } from "/util.js";
import { mdToHtml, hydrateDynamicBlocks, headingId, setNoteIndex, setNoteOpener } from "/markdown.js";
import { Editor } from "/editor.js";
import { Plugins } from "/plugins.js";
import { initCanvas, openCanvasPicker, createCanvas } from "/canvas.js";
import { openGraph, initGraph } from "/graph.js";
import { openVault } from "/vaultui.js";
const state = { path: null, notes: [], dirty: false, saveTimer: null, frontmatter: {}, templates: [], aliases: {}, allTags: [] };


/* ---------- note list ---------- */
async function loadList() {
  state.notes = await api("/notes");
  setNoteIndex(state.notes, state.aliases);        // markdown engine's link index
  renderList(state.notes);
  api("/aliases").then((a) => { state.aliases = a || {}; setNoteIndex(state.notes, state.aliases); }).catch(() => {});
  api("/tags").then((t) => (state.allTags = t || [])).catch(() => {});
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
      setNoteIndex(state.notes, state.aliases);
      // leave an active tag filter OR an active search alone — re-rendering the
      // full list here wiped search results and keyboard selection mid-use
      // (surfaced as a CI flake; it was a real clobber, not test timing)
      if (!state.filterTag && !$("#search").value.trim()) renderList(notes);
      $("#stat").textContent = `${h.notes} notes · ${h.tags} tags · ${h.unresolved_links} unlinked`;
    } else {
      state.rev = h.rev;
    }
  } catch {}
}
setInterval(pollRev, 5000);
document.addEventListener("visibilitychange", () => { if (!document.hidden) pollRev(); });
function noteRow(n, snippets) {
  const row = document.createElement("div");
  row.className = "note-row" + (n.path === state.path ? " active" : "");
  row.dataset.path = n.path;
  const memBadge = n.path.startsWith("memory/") ? '<span class="mem-badge" title="agent memory">🤖</span>' : "";
  row.innerHTML = `<div class="t">${n.pinned ? '<span class="pin">📌</span>' : ""}${memBadge}${esc(n.title || n.path)}</div>` +
    (snippets && n.snippet ? `<div class="snip">${n.snippet.replace(/\[(.*?)\]/g, "<b>$1</b>")}</div>`
      : `<div class="m">${esc(n.path)}</div>`);
  row.onclick = (e) => (e.ctrlKey || e.metaKey) ? openSplit(n.path) : openNote(n.path);
  row.oncontextmenu = (e) => { e.preventDefault(); showContext(n.path, e.clientX, e.clientY); };
  return row;
}

/* Folder collapse state persists per device. */
const foldState = JSON.parse(localStorage.getItem("grimoire-folds") || "{}");

function renderList(notes, snippets = false) {
  const el = $("#note-list");
  el.innerHTML = "";
  if (!notes.length) { el.innerHTML = '<div class="note-row m">No notes yet.</div>'; return; }
  // group by top-level folder — a classic file-explorer tree, while
  // search results and tag filters stay flat for scannability
  const grouped = !snippets;
  if (!grouped) {
    for (const n of notes) el.appendChild(noteRow(n, snippets));
    listSel = -1;
    return;
  }
  const root = [], folders = new Map();
  for (const n of notes) {
    const slash = n.path.indexOf("/");
    if (slash === -1) { root.push(n); continue; }
    const dir = n.path.slice(0, slash);
    if (!folders.has(dir)) folders.set(dir, []);
    folders.get(dir).push(n);
  }
  for (const n of root) el.appendChild(noteRow(n, snippets));
  for (const [dir, items] of [...folders.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
    const det = document.createElement("details");
    det.className = "folder";
    det.open = foldState[dir] !== false;   // default open
    det.innerHTML = `<summary class="folder-head">▸ ${esc(dir)}/ <span class="m">${items.length}</span></summary>`;
    det.ontoggle = () => { foldState[dir] = det.open; localStorage.setItem("grimoire-folds", JSON.stringify(foldState)); };
    for (const n of items) det.appendChild(noteRow(n, snippets));
    el.appendChild(det);
  }
  listSel = -1;
}

/* keyboard navigation of the note list (↑/↓ move, Enter opens) */
let listSel = -1;
function moveListSel(delta) {
  const rows = [...$("#note-list").querySelectorAll(".note-row[data-path]")];
  if (!rows.length) return;
  rows.forEach((r) => r.classList.remove("kbd-sel"));
  listSel = Math.max(0, Math.min(rows.length - 1, listSel + delta));
  const row = rows[listSel];
  row.classList.add("kbd-sel");
  row.scrollIntoView({ block: "nearest" });
}
function openListSel(split) {
  const rows = [...$("#note-list").querySelectorAll(".note-row[data-path]")];
  const row = rows[listSel] || rows[0];
  if (row) split ? openSplit(row.dataset.path) : openNote(row.dataset.path);
}
function listNavKey(e) {
  if (e.key === "ArrowDown") { e.preventDefault(); moveListSel(listSel < 0 ? 0 : 1); }
  else if (e.key === "ArrowUp") { e.preventDefault(); moveListSel(-1); }
  else if (e.key === "Enter" && listSel >= 0) { e.preventDefault(); openListSel(e.ctrlKey || e.metaKey); }
}
$("#search").addEventListener("keydown", listNavKey);
$("#note-list").tabIndex = 0;
$("#note-list").addEventListener("keydown", listNavKey);

/* ---------- note actions by path (context menu) ---------- */
async function pinByPath(path) { await api(`/notes/${encodeURI(path)}/pin`, { method: "POST" }); loadList(); }
async function duplicateByPath(path) {
  try { const n = await api(`/notes/${encodeURI(path)}/duplicate`, { method: "POST" }); await loadList(); openNote(n.path); toast("Duplicated"); }
  catch (e) { toast(e.message, true); }
}
async function renameNote(path) {
  const n = await api(`/notes/${encodeURI(path)}`);
  const cur = n.title || path.replace(/\.md$/, "").split("/").pop();
  const to = prompt("Rename note to:", cur);
  if (!to || to === cur) return;
  try {
    // rename = update the display title AND move the file to a matching slug
    if (!n.locked) {
      await api(`/notes/${encodeURI(path)}`, { method: "PUT",
        body: { body: n.body, frontmatter: { ...(n.frontmatter || {}), title: to } } });
    }
    const dir = path.includes("/") ? path.slice(0, path.lastIndexOf("/") + 1) : "";
    const r = await api(`/notes/${encodeURI(path)}/rename`, { method: "POST", body: { to: dir + slugify(to) + ".md" } });
    await loadList();
    if (state.path === path) openNote(r.path);
    toast("Renamed");
  } catch (e) { toast(e.message, true); }
}
async function deleteNoteByPath(path) {
  if (!confirm("Move this note to trash?")) return;
  try {
    const r = await api(`/notes/${encodeURI(path)}`, { method: "DELETE" });
    if (state.path === path) { state.path = null; $("#title").value = ""; $("#content").value = ""; $("#content").readOnly = false; $("#backlinks").innerHTML = ""; $("#unlinked").innerHTML = ""; }
    loadList();
    toastAction("Moved to trash", "Undo", async () => {
      const n = await api(`/trash/${r.trashed}/restore`, { method: "POST" });
      await loadList(); openNote(n.path); toast("Restored");
    });
  } catch (e) { toast(e.message, true); }
}
const ctxMenu = $("#ctx-menu");
function showContext(path, x, y) {
  const items = [
    ["⊞ Open in split", () => openSplit(path)],
    ["📌 Pin / unpin", () => pinByPath(path)],
    ["⧉ Duplicate", () => duplicateByPath(path)],
    ["✎ Rename…", () => renameNote(path)],
    ["🗑 Delete", () => deleteNoteByPath(path)],
  ];
  ctxMenu.innerHTML = items.map((it, i) => `<div class="mi" data-i="${i}">${it[0]}</div>`).join("");
  ctxMenu.querySelectorAll(".mi").forEach((el, i) =>
    (el.onclick = () => { ctxMenu.classList.add("hidden"); items[i][1](); }));
  ctxMenu.style.top = Math.min(y, innerHeight - 190) + "px";
  ctxMenu.style.left = Math.min(x, innerWidth - 180) + "px";
  ctxMenu.style.right = "";
  ctxMenu.classList.remove("hidden");
}
addEventListener("click", (e) => { if (!e.target.closest("#ctx-menu")) ctxMenu.classList.add("hidden"); });

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
    // recover unsaved work (crash / offline reload) for this exact note
    const draft = getDraft();
    if (draft && draft.path === n.path && !n.encrypted && draft.content !== (n.body || "")) {
      $("#content").value = draft.content;
      if (draft.title) $("#title").value = draft.title;
      state.dirty = true; toast("Restored unsaved changes"); save();
    }
  }
  Editor.sync();                       // push the loaded body into the live editor
  Editor.setReadOnly(state.locked);
  renderProvenance(n);
  Plugins.emit("note-open", { path: n.path, title: n.title });
  updatePrivateToggle();
  updateWordCount();
  renderBacklinks(n.backlinks || []);
  renderOutgoing(n.links || []);
  $("#unlinked").innerHTML = "";
  if (!state.locked) api(`/notes/${encodeURI(n.path)}/unlinked`).then(renderUnlinked).catch(() => {});
  state.filterTag = null;
  $("#tag-filter-bar")?.remove();
  renderList(state.notes);
  setSaveState("");
  closeSidebarMobile();
  if (!$("#preview").classList.contains("hidden")) renderPreview();
  location.hash = encodeURI(path);
}
function setSaveState(s) { $("#save-state").textContent = s; }
/* offline-safe drafts: persist the in-flight edit so a crash / offline reload
   never loses work, and retry saving when connectivity returns. */
function saveDraft() {
  // NEVER persist an encrypted note's decrypted plaintext to localStorage
  if (!state.path || state.locked || state.encrypted) return;
  try { localStorage.setItem("grimoire-draft", JSON.stringify(
    { path: state.path, title: $("#title").value, content: $("#content").value })); } catch {}
}
function clearDraft() { try { localStorage.removeItem("grimoire-draft"); } catch {} }
function getDraft() { try { return JSON.parse(localStorage.getItem("grimoire-draft") || "null"); } catch { return null; } }
function scheduleSave() {
  state.dirty = true; setSaveState("…"); saveDraft();
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
    state.dirty = false; setSaveState("saved"); clearDraft();
    Plugins.emit("note-save", { path: state.path });
    delete hoverCache[state.path];   // preview cache may be stale now
    setTimeout(() => setSaveState(""), 1200);
    // refresh both link panels — renderBacklinks REPLACES the container, so
    // outgoing must be re-rendered too (it silently vanished after autosaves).
    // Guard on the path: a save can land after the user switched notes.
    const savedPath = state.path;
    const fresh = n && await api(`/notes/${encodeURI(savedPath)}`);
    if (fresh && state.path === savedPath) {
      renderBacklinks(fresh.backlinks || []);
      renderOutgoing(fresh.links || []);
    }
    loadList();
  } catch (e) {
    // keep the draft + dirty flag; we'll retry when back online
    setSaveState("offline ⟳"); saveDraft();
  }
}
// retry any pending save the moment connectivity returns
addEventListener("online", () => { if (state.dirty) save(); });

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
  if (!state.path || !confirm("Move this note to trash?")) return;
  const r = await api(`/notes/${encodeURI(state.path)}`, { method: "DELETE" });
  state.path = null; state.dirty = false; state.locked = false;
  $("#title").value = ""; $("#content").value = ""; $("#content").readOnly = false;
  Editor.sync(); Editor.setReadOnly(false);
  $("#backlinks").innerHTML = "";
  loadList();
  toastAction("Moved to trash", "Undo", async () => {
    try {
      const n = await api(`/trash/${r.trashed}/restore`, { method: "POST" });
      await loadList(); openNote(n.path); toast("Restored");
    } catch (e) { toast(e.message, true); }
  });
}

/* ---------- backlinks ---------- */
function renderBacklinks(bl) {
  const el = $("#backlinks");
  if (!bl.length) { el.innerHTML = '<h4>Backlinks</h4><div class="empty">Nothing links here yet.</div>'; return; }
  el.innerHTML = "<h4>Backlinks</h4>" +
    bl.map((b) => `<a data-p="${esc(b.path)}">← ${esc(b.title)}</a>`).join("");
  el.querySelectorAll("a").forEach((a) => (a.onclick = () => openNote(a.dataset.p)));
}

/* ---------- note hover previews (desktop) ---------- */
let hoverTimer, hideTimer;
const hoverCache = {};
function resolveTargetPath(target) {
  const t = (target || "").toLowerCase();
  const hit = state.notes.find((n) => (n.title || "").toLowerCase() === t
    || n.path.replace(/\.md$/, "").split("/").pop().toLowerCase() === t);
  return hit ? hit.path : (state.aliases[t] || null);
}
function wireHoverPreviews(container) {
  container.querySelectorAll("a.wikilink:not(.unresolved)").forEach((a) => {
    a.addEventListener("mouseenter", () => {
      clearTimeout(hideTimer); clearTimeout(hoverTimer);
      hoverTimer = setTimeout(() => showHoverPreview(a.dataset.target, a), 320);
    });
    a.addEventListener("mouseleave", () => { clearTimeout(hoverTimer); scheduleHideHover(); });
  });
  container.querySelectorAll("#backlinks a[data-p], a[data-p]").forEach(() => {});
}
async function showHoverPreview(target, anchor) {
  const path = resolveTargetPath(target);
  if (!path) return;
  let note = hoverCache[path];
  if (!note) { try { note = await api(`/notes/${encodeURI(path)}`); hoverCache[path] = note; } catch { return; } }
  const box = $("#hover-preview");
  const snippet = (note.body || "").replace(/^#\s.*$/m, "").trim().slice(0, 280);
  box.innerHTML = `<div class="hp-title">${esc(note.title || path)}</div><div class="md hp-body">${mdToHtml(snippet)}</div>`;
  const r = anchor.getBoundingClientRect();
  box.style.left = Math.min(r.left, innerWidth - 340) + "px";
  box.style.top = Math.min(r.bottom + 6, innerHeight - 220) + "px";
  box.classList.remove("hidden");
  box.onmouseenter = () => clearTimeout(hideTimer);
  box.onmouseleave = scheduleHideHover;
}
function scheduleHideHover() { clearTimeout(hideTimer); hideTimer = setTimeout(() => $("#hover-preview").classList.add("hidden"), 220); }

/* ---------- unlinked mentions ---------- */
function renderUnlinked(items) {
  const el = $("#unlinked");
  if (!items || !items.length) { el.innerHTML = ""; return; }
  el.innerHTML = "<h4>Unlinked mentions</h4>" + items.map((u) =>
    `<div class="unlinked-row"><div class="ul-top"><a data-p="${esc(u.path)}">${esc(u.title)}</a>
      <button class="link-btn" data-p="${esc(u.path)}" data-n="${esc(u.name)}">🔗 link</button></div>
      <div class="ctx">${esc(u.context)}</div></div>`).join("");
  el.querySelectorAll("a[data-p]").forEach((a) => (a.onclick = () => openNote(a.dataset.p)));
  el.querySelectorAll(".link-btn").forEach((b) => (b.onclick = async () => {
    try {
      await api(`/notes/${encodeURI(state.path)}/link`, { method: "POST", body: { source: b.dataset.p, name: b.dataset.n } });
      toast("Linked 🔗");
      const n = await api(`/notes/${encodeURI(state.path)}`);
      renderBacklinks(n.backlinks || []);
      api(`/notes/${encodeURI(state.path)}/unlinked`).then(renderUnlinked).catch(() => {});
    } catch (e) { toast(e.message, true); }
  }));
}

/* ---------- preview (offline markdown → html) ---------- */
function renderPreview() {
  $("#preview").innerHTML = `<div class="md">${mdToHtml($("#content").value)}</div>`;
  $("#preview").querySelectorAll("a.wikilink").forEach((a) => {
    a.onclick = (e) => { e.preventDefault(); resolveAndOpen(a.dataset.target); };
  });
  hydrateDynamicBlocks($("#preview"));
  Plugins.renderFences($("#preview"));
  wireHoverPreviews($("#preview"));
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
  Editor.sync();
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
  const aliasPath = state.aliases[target.toLowerCase()];
  if (aliasPath) return openNote(aliasPath);
  // create-on-click for unresolved links
  if (confirm(`"${target}" doesn't exist yet. Create it?`)) {
    const n = await api("/notes", { method: "POST", body: { title: target, body: `# ${target}\n\n` } });
    await loadList(); openNote(n.path);
  }
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

/* ---------- find & replace ---------- */
function openFind() {
  $("#find-bar").classList.remove("hidden");
  const sel = ta.value.slice(ta.selectionStart, ta.selectionEnd);
  if (sel && !sel.includes("\n")) $("#find-input").value = sel;
  $("#find-input").focus(); $("#find-input").select();
  updateFindCount();
}
function closeFind() { $("#find-bar").classList.add("hidden"); ta.focus(); }
function findMatches() {
  const q = $("#find-input").value; if (!q) return [];
  const hay = ta.value.toLowerCase(), needle = q.toLowerCase(), idxs = [];
  let i = hay.indexOf(needle);
  while (i !== -1) { idxs.push(i); i = hay.indexOf(needle, i + Math.max(1, needle.length)); }
  return idxs;
}
function updateFindCount() {
  const q = $("#find-input").value;
  $("#find-count").textContent = q ? String(findMatches().length) : "";
}
function findNext(dir = 1) {
  const q = $("#find-input").value; if (!q) return;
  const m = findMatches(); if (!m.length) return;
  let target;
  if (dir > 0) { target = m.find((i) => i >= ta.selectionStart + 1); if (target === undefined) target = m[0]; }
  else { const before = m.filter((i) => i < ta.selectionStart); target = before.length ? before[before.length - 1] : m[m.length - 1]; }
  ta.focus(); ta.setSelectionRange(target, target + q.length);
  const lineNo = ta.value.slice(0, target).split("\n").length - 1;
  const lh = parseFloat(getComputedStyle(ta).lineHeight) || 24;
  ta.scrollTop = Math.max(0, lineNo * lh - ta.clientHeight / 3);
}
function replaceOne() {
  const q = $("#find-input").value, r = $("#replace-input").value;
  if (!q) return;
  const sel = ta.value.slice(ta.selectionStart, ta.selectionEnd);
  if (sel.toLowerCase() === q.toLowerCase()) {
    const s = ta.selectionStart;
    ta.value = ta.value.slice(0, s) + r + ta.value.slice(ta.selectionEnd);
    ta.setSelectionRange(s, s + r.length);
    scheduleSave();
  }
  findNext(1); updateFindCount();
}
function replaceAll() {
  const q = $("#find-input").value, r = $("#replace-input").value;
  if (!q) return;
  const re = new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi");
  const n = (ta.value.match(re) || []).length;
  if (!n) return toast("No matches");
  ta.value = ta.value.replace(re, r);
  scheduleSave(); updateFindCount(); toast(`Replaced ${n}`);
}
$("#find-bar").addEventListener("keydown", (e) => { if (e.key === "Escape") { e.preventDefault(); closeFind(); } });
$("#find-close").onclick = closeFind;
$("#find-next").onclick = () => findNext(1);
$("#find-prev").onclick = () => findNext(-1);
$("#find-replace").onclick = replaceOne;
$("#find-all").onclick = replaceAll;
$("#find-input").addEventListener("input", updateFindCount);
$("#find-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); findNext(e.shiftKey ? -1 : 1); }
  else if (e.key === "Escape") { e.preventDefault(); closeFind(); }
});
$("#replace-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); replaceOne(); }
  else if (e.key === "Escape") { e.preventDefault(); closeFind(); }
});
addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "f" && !e.shiftKey) {
    e.preventDefault(); openFind();
  }
});

function updateWordCount() {
  if (state.locked) { $("#wordcount").textContent = ""; return; }
  const words = (ta.value.trim().match(/\S+/g) || []).length;
  const mins = Math.max(1, Math.round(words / 200));
  $("#wordcount").textContent = words ? `${words} words · ${mins} min` : "";
}
ta.addEventListener("input", updateWordCount);
function surround(pre, post = pre, placeholder = "") {
  if (Editor.surround(pre, post, placeholder)) return;   // live editor handled it
  const s = ta.selectionStart, e = ta.selectionEnd, v = ta.value;
  const sel = v.slice(s, e) || placeholder;
  ta.value = v.slice(0, s) + pre + sel + post + v.slice(e);
  ta.selectionStart = s + pre.length;
  ta.selectionEnd = s + pre.length + sel.length;
  ta.focus(); scheduleSave();
}
function prefixLine(prefix) {
  if (Editor.prefixLine(prefix)) return;                 // live editor handled it
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
  if (Editor.insert(text)) return;                       // live editor handled it
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
function _positionComplete(box) {
  const rect = ta.getBoundingClientRect();
  box.style.left = rect.left + 20 + "px";
  box.style.top = rect.top + 40 + "px";
  box.classList.remove("hidden");
}
async function maybeComplete() {
  const pos = ta.selectionStart;
  const before = ta.value.slice(0, pos);
  const box = $("#complete");
  // [[ wiki-link completion
  const m = before.match(/\[\[([^\]|\n]*)$/);
  if (m) {
    const res = await api(`/complete?q=${encodeURIComponent(m[1])}`);
    if (!res.length) return hideComplete();
    box.innerHTML = res.map((r, idx) =>
      `<div class="c${idx === 0 ? " sel" : ""}" data-stem="${esc(r.stem)}">${esc(r.title)}</div>`).join("");
    box.querySelectorAll(".c").forEach((c) => (c.onclick = () => insertLink(c.dataset.stem, m.index)));
    return _positionComplete(box);
  }
  // #tag completion — suggest existing tags
  const tm = before.match(/(?:^|\s)#([A-Za-z][\w/-]*)$/);
  if (tm) {
    const q = tm[1].toLowerCase();
    const matches = (state.allTags || [])
      .filter((t) => t.tag.toLowerCase().startsWith(q) && t.tag.toLowerCase() !== q).slice(0, 10);
    if (!matches.length) return hideComplete();
    box.innerHTML = matches.map((r, idx) =>
      `<div class="c${idx === 0 ? " sel" : ""}" data-tag="${esc(r.tag)}">#${esc(r.tag)} <span class="pm">${r.c}</span></div>`).join("");
    box.querySelectorAll(".c").forEach((c) => (c.onclick = () => insertTag(c.dataset.tag)));
    return _positionComplete(box);
  }
  hideComplete();
}
function insertTag(tag) {
  const pos = ta.selectionStart;
  const before = ta.value.slice(0, pos).replace(/#[A-Za-z][\w/-]*$/, `#${tag}`);
  ta.value = before + ta.value.slice(pos);
  ta.selectionStart = ta.selectionEnd = before.length;
  hideComplete(); scheduleSave(); ta.focus();
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

/* Outgoing links — the counterpart of the backlinks panel. */
function renderOutgoing(links) {
  const el = $("#backlinks");
  const resolved = links.filter((l) => l.resolved);
  const dangling = links.filter((l) => !l.resolved);
  if (!links.length) return;
  const box = document.createElement("div");
  box.className = "outgoing";
  box.innerHTML = `<div class="bl-title">Links from this note</div>` +
    resolved.map((l) => `<a class="wikilink" data-path="${esc(l.dst)}">→ ${esc(l.target)}</a> `).join("") +
    dangling.map((l) => `<span class="unresolved" title="not created yet">→ ${esc(l.target)}</span> `).join("");
  box.querySelectorAll("a.wikilink").forEach((a) => (a.onclick = () => openNote(a.dataset.path)));
  el.appendChild(box);
}

/* ---------- version history (automatic file-recovery snapshots) ---------- */
async function openHistory() {
  if (!state.path) return toast("Open a note first", true);
  if (state.dirty) await save();
  $("#history-modal").classList.remove("hidden");
  const b = $("#history-body");
  const versions = await api(`/notes/${encodeURI(state.path)}/history`);
  if (!versions.length) { b.innerHTML = '<p class="vault-note">No versions yet — history is written on every content-changing save.</p>'; return; }
  b.innerHTML = versions.map((v) => `
    <div class="v-row"><span>🕘 ${new Date(v.ts * 1000).toLocaleString()} <span class="pm">${v.size} B</span></span>
      <span><button class="btn h-view" data-id="${esc(v.id)}">View</button>
      <button class="btn h-restore" data-id="${esc(v.id)}">Restore</button></span></div>`).join("")
    + '<pre id="history-preview" class="hidden"></pre>';
  b.querySelectorAll(".h-view").forEach((x) => (x.onclick = async () => {
    try {
      const v = await api(`/notes/${encodeURI(state.path)}/history/${x.dataset.id}`);
      const pre = $("#history-preview");
      pre.classList.remove("hidden"); pre.textContent = v.body;
    } catch (e) { toast(e.message, true); }
  }));
  b.querySelectorAll(".h-restore").forEach((x) => (x.onclick = async () => {
    if (!confirm("Restore this version? The current text is kept in history.")) return;
    await api(`/notes/${encodeURI(state.path)}/history/${x.dataset.id}/restore`, { method: "POST" });
    $("#history-modal").classList.add("hidden");
    toast("Restored — the replaced version stays in history");
    openNote(state.path);
  }));
}
$("#history-close").onclick = () => $("#history-modal").classList.add("hidden");

/* ---------- note composer (extract / merge) ---------- */
async function extractSelection() {
  const sel = Editor.isLive ? Editor.live.getSelection().text
    : ta.value.slice(ta.selectionStart, ta.selectionEnd);
  if (!sel.trim()) return toast("Select some text to extract first", true);
  const title = prompt("Title for the extracted note:");
  if (!title) return;
  try {
    await api("/notes", { method: "POST", body: { title, body: sel.trim() + "\n" } });
    if (Editor.isLive) Editor.live.replaceSelection(`[[${title}]]`);
    else {
      const s0 = ta.selectionStart, e0 = ta.selectionEnd;
      ta.value = ta.value.slice(0, s0) + `[[${title}]]` + ta.value.slice(e0);
    }
    Editor.sync(); scheduleSave(); await loadList();
    toast(`Extracted to "${title}" and linked`);
  } catch (e) { toast(e.message, true); }
}

async function mergeIntoNote() {
  if (!state.path) return toast("Open a note first", true);
  const target = prompt("Merge this note INTO which note? (title)");
  if (!target) return;
  const hit = state.notes.find((n) => (n.title || "").toLowerCase() === target.toLowerCase());
  if (!hit) return toast(`No note titled "${target}"`, true);
  if (hit.path === state.path) return toast("Cannot merge a note into itself", true);
  if (!confirm(`Append this note's content to "${hit.title}" and move this note to trash?`)) return;
  try {
    if (state.dirty) await save();
    const me = await api(`/notes/${encodeURI(state.path)}`);
    const them = await api(`/notes/${encodeURI(hit.path)}`);
    await api(`/notes/${encodeURI(hit.path)}`, { method: "PUT",
      body: { body: them.body.replace(/\n+$/, "") + `\n\n## ${me.title}\n\n` + me.body } });
    await api(`/notes/${encodeURI(state.path)}`, { method: "DELETE" });
    toast(`Merged into "${hit.title}" (original in trash)`);
    await loadList(); openNote(hit.path);
  } catch (e) { toast(e.message, true); }
}

/* ---------- unique (Zettelkasten timestamp) note ---------- */
async function newUniqueNote() {
  const stamp = new Date().toISOString().replace(/[-:T]/g, "").slice(0, 12);
  const title = prompt("Title (optional):") || "";
  try {
    const n = await api("/notes", { method: "POST",
      body: { path: `zettel/${stamp}${title ? "-" + title.toLowerCase().replace(/[^a-z0-9]+/g, "-") : ""}.md`,
              title: title || stamp, body: `# ${title || stamp}\n\n` } });
    await loadList(); openNote(n.path);
  } catch (e) { toast(e.message, true); }
}

/* ---------- slides (present the current note) ---------- */
function presentNote() {
  if (!state.path) return toast("Open a note first", true);
  const src = $("#content").value;
  // slides split on --- lines; fall back to one slide per H2 section
  let parts = src.split(/\n---\n/);
  if (parts.length === 1) parts = src.split(/\n(?=## )/);
  const overlay = document.createElement("div");
  overlay.id = "slides";
  overlay.innerHTML = `<div class="slide md"></div>
    <div class="slide-nav"><span id="slide-pos"></span> · ←/→ · Esc to exit</div>`;
  document.body.appendChild(overlay);
  let cur = 0;
  const show = (i) => {
    cur = Math.max(0, Math.min(parts.length - 1, i));
    overlay.querySelector(".slide").innerHTML = mdToHtml(parts[cur]);
    overlay.querySelector("#slide-pos").textContent = `${cur + 1} / ${parts.length}`;
  };
  const onKey = (e) => {
    if (e.key === "Escape") { overlay.remove(); removeEventListener("keydown", onKey, true); }
    else if (e.key === "ArrowRight" || e.key === " ") { e.preventDefault(); show(cur + 1); }
    else if (e.key === "ArrowLeft") { e.preventDefault(); show(cur - 1); }
  };
  addEventListener("keydown", onKey, true);
  overlay.onclick = (e) => { if (e.target === overlay) show(cur + 1); };
  show(0);
}

/* Agent-memory provenance — memories are ordinary notes, but the human
   should always see at a glance that an agent wrote here, and be one click
   from the version history that makes agent writes reviewable. */
function renderProvenance(n) {
  const el = $("#provenance");
  const fm = n.frontmatter || {};
  if (!fm.memory && !n.path.startsWith("memory/")) { el.classList.add("hidden"); return; }
  el.classList.remove("hidden");
  el.innerHTML = `🤖 <b>agent memory</b> — last written by <code>${esc(fm.agent || "unknown")}</code>`
    + (fm.task ? ` (task <code>${esc(fm.task)}</code>)` : "")
    + ` · every entry is editable — <a id="prov-history">history & rollback</a>`;
  $("#prov-history").onclick = () => openHistory();
}

/* Retrieval inspection — show exactly which chunks the agent's ask/RAG layer
   would receive for a query. Trust surface: no hidden context. */
async function openInspect() {
  const q = prompt("What would the agent see for…");
  if (!q) return;
  $("#inspect-title").textContent = "🔎 What the agent sees";
  $("#inspect-modal").classList.remove("hidden");
  const b = $("#inspect-body");
  b.innerHTML = '<p class="vault-note">Retrieving…</p>';
  try {
    const chunks = await api(`/retrieve?q=${encodeURIComponent(q)}&k=8`);
    if (!chunks.length) { b.innerHTML = '<p class="vault-note">Nothing retrieved — the agent would answer from nothing.</p>'; return; }
    b.innerHTML = `<p class="vault-note">Top ${chunks.length} chunks for <b>${esc(q)}</b> — this is the agent's entire retrieved context (private notes excluded, exactly as the agent sees it):</p>`
      + chunks.map((c) => `
        <div class="inspect-chunk">
          <div class="ic-head"><a class="wikilink" data-path="${esc(c.path)}">${esc(c.title || c.path)}</a>
            <span class="ic-score">${(c.score * 100).toFixed(0)}%</span></div>
          <div class="ic-text">${esc(c.chunk)}</div>
        </div>`).join("");
    b.querySelectorAll("a.wikilink").forEach((a) => (a.onclick = () => {
      $("#inspect-modal").classList.add("hidden"); openNote(a.dataset.path);
    }));
  } catch (e) { b.innerHTML = `<p class="vault-note">${esc(e.message)}</p>`; }
}
$("#inspect-close").onclick = () => $("#inspect-modal").classList.add("hidden");

/* Agent briefing viewer — the standing context agents receive from the
   get_briefing MCP tool (pinned + onboarding + recent memories). The human
   should be able to read exactly what agents are told first. */
async function openBriefing() {
  $("#inspect-title").textContent = "📋 Agent briefing (what agents get first)";
  $("#inspect-modal").classList.remove("hidden");
  const b = $("#inspect-body");
  b.innerHTML = '<p class="vault-note">Loading…</p>';
  try {
    const br = await api("/briefing");
    const section = (label, notes) => notes.length
      ? `<p class="vault-note"><b>${label}</b></p>` + notes.map((n) => `
          <div class="inspect-chunk">
            <div class="ic-head"><a class="wikilink" data-path="${esc(n.path)}">${esc(n.title || n.path)}</a></div>
            <div class="ic-text">${esc(n.body.slice(0, 400))}</div>
          </div>`).join("")
      : "";
    b.innerHTML = section("Pinned", br.pinned)
      + section("Onboarding", br.onboarding)
      + section("Recent agent memories", br.recent_memories)
      || '<p class="vault-note">Nothing yet — pin a note, tag one #onboarding, or let an agent remember something.</p>';
    b.querySelectorAll("a.wikilink").forEach((a) => (a.onclick = () => {
      $("#inspect-modal").classList.add("hidden"); openNote(a.dataset.path);
    }));
  } catch (e) { b.innerHTML = `<p class="vault-note">${esc(e.message)}</p>`; }
}

/* ---------- command palette (Ctrl/Cmd-K quick switcher) ---------- */
const COMMANDS = [
  { icon: "＋", name: "New note", run: newNote },
  { icon: "◈", name: "Open today's daily note", run: openDaily },
  { icon: "✦", name: "Ask your notes", run: () => $("#ask-open").click() },
  { icon: "◉", name: "Open graph view", run: openGraph },
  { icon: "◐", name: "Toggle preview", run: () => $("#preview-toggle").click() },
  { icon: "🔐", name: "Open secret vault", run: openVault },
  { icon: "⚖", name: "Agent grants & audit (credential console)", run: openVault },
  { icon: "🔎", name: "What would the agent see… (retrieval inspection)", run: openInspect },
  { icon: "📋", name: "Agent briefing (standing context)", run: openBriefing },
  { icon: "🤖", name: "Agent memories (recent)", run: async () => {
    const mems = await api("/memory");
    if (!mems.length) return toast("No agent memories yet — agents write them via the MCP remember tool");
    openNote(mems[0].path);
  } },
  { icon: "🎙", name: "Record audio memo", run: () => $("#audio-memo").click() },
  { icon: "🗂", name: "Save current note as template", run: saveAsTemplate },
  { icon: "⇩", name: "Export note as HTML (print to PDF)", run: exportNote },
  { icon: "⚙", name: "Open settings", run: openSettings },
  { icon: "🔒", name: "Encrypt this note (at rest)", run: () => cryptNote("encrypt") },
  { icon: "🔓", name: "Decrypt this note", run: () => cryptNote("decrypt") },
  { icon: "🗑", name: "Open trash", run: openTrash },
  { icon: "🕘", name: "Version history (this note)", run: openHistory },
  { icon: "✂", name: "Extract selection to new note", run: extractSelection },
  { icon: "🔀", name: "Merge this note into another", run: mergeIntoNote },
  { icon: "🧩", name: "New unique (Zettel) note", run: newUniqueNote },
  { icon: "🎞", name: "Present this note (slides)", run: presentNote },
  { icon: "🗺", name: "Open canvas…", run: openCanvasPicker },
  { icon: "🗺", name: "New canvas", run: createCanvas },
  { icon: "🔌", name: "Create a plugin (skeleton in your vault)", run: async () => {
    const name = prompt("Plugin name (lowercase, hyphens):");
    if (!name) return;
    try {
      const r = await api("/plugins/scaffold", { method: "POST", body: { name } });
      toast(`Created ${r.path} — enable it in Settings → Plugins`);
    } catch (e) { toast(e.message, true); }
  } },
  { icon: "📌", name: "Pin / unpin this note", run: togglePin },
  { icon: "📅", name: "Open calendar", run: () => openCalendar() },
  { icon: "◀", name: "Previous day (daily note)", run: () => shiftDaily(-1) },
  { icon: "▶", name: "Next day (daily note)", run: () => shiftDaily(1) },
  { icon: "📆", name: "Insert today's date", run: () => insertAtCursor(_isoLocal(new Date())) },
  { icon: "☑", name: "Open tasks (all notes)", run: openTasks },
  { icon: "🏷", name: "Browse tags", run: openTagBrowser },
  { icon: "⌨", name: "Keyboard shortcuts & help", run: openHelp },
  { icon: "ⓘ", name: "Edit note properties", run: openProps },
  { icon: "🔍", name: "Find & replace in note",
    run: () => { Editor.isLive ? Editor.openSearch() : openFind(); } },
  { icon: "🎲", name: "Open random note", run: openRandom },
  { icon: "⧉", name: "Duplicate this note", run: duplicateNote },
  { icon: "⊞", name: "Split view: open current note on the right", run: () => openSplit(state.path) },
  { icon: "⊟", name: "Close split view", run: closeSplit },
  { icon: "⇤", name: "Toggle sidebar", run: toggleSidebar },
  { icon: "🧘", name: "Toggle focus mode (distraction-free)", run: toggleZen },
  { icon: "⬇", name: "Export whole vault (.zip)", run: () => { location.href = "/api/export/vault"; } },
  { icon: "⬆", name: "Import vault from .zip", run: importVault },
  { icon: "🔄", name: "Sync now (with configured peer)", run: syncNow },
  { icon: "🏷", name: "Rename a tag (across all notes)", run: renameTag },
];
async function syncNow() {
  try {
    toast("Syncing…");
    const s = await api("/sync/now", { method: "POST" });
    toast(`Synced: ↓${s.pulled} ↑${s.pushed}${s.conflicts ? ` · ${s.conflicts} conflict(s)` : ""}`);
    await loadList();
  } catch (e) { toast(e.message, true); }
}
function importVault() {
  const inp = document.createElement("input");
  inp.type = "file"; inp.accept = ".zip,application/zip";
  inp.onchange = async () => {
    const f = inp.files[0]; if (!f) return;
    const fd = new FormData(); fd.append("file", f, f.name);
    toast("Importing…");
    try {
      const r = await fetch("/api/import/vault", { method: "POST", body: fd });
      if (!r.ok) throw new Error((await r.json().catch(() => ({}))).detail || r.statusText);
      const j = await r.json();
      toast(`Imported ${j.imported} file(s)${j.skipped ? `, skipped ${j.skipped}` : ""}`);
      await loadList();
    } catch (e) { toast("Import failed: " + e.message, true); }
  };
  inp.click();
}
async function renameTag() {
  const old = prompt("Rename which tag? (without #)");
  if (!old) return;
  const nw = prompt(`Rename #${old.replace(/^#/, "")} to: (without #)`);
  if (!nw) return;
  try {
    const r = await api("/tags/rename", { method: "POST", body: { old, new: nw } });
    toast(`Renamed #${r.renamed} → #${r.to} in ${r.notes} note(s)`);
    await loadList();
    if (state.path) openNote(state.path);
  } catch (e) { toast(e.message, true); }
}
async function openRandom() {
  try { const r = await api("/notes/random"); openNote(r.path); }
  catch (e) { toast(e.message, true); }
}
async function duplicateNote() {
  if (!state.path) return toast("Open a note first", true);
  try {
    const n = await api(`/notes/${encodeURI(state.path)}/duplicate`, { method: "POST" });
    await loadList(); openNote(n.path); toast("Duplicated");
  } catch (e) { toast(e.message, true); }
}
function openHelp() { $("#help-modal").classList.remove("hidden"); }
$("#help-close").onclick = () => $("#help-modal").classList.add("hidden");
$("#help-modal").onclick = (e) => { if (e.target.id === "help-modal") $("#help-modal").classList.add("hidden"); };
// "?" opens help — but not while typing in a field
addEventListener("keydown", (e) => {
  if (e.key !== "?" || e.metaKey || e.ctrlKey || e.altKey) return;
  const el = document.activeElement, tag = el && el.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || (el && el.isContentEditable)) return;
  e.preventDefault(); openHelp();
});
async function togglePin() {
  if (!state.path) return toast("Open a note first", true);
  try {
    const r = await api(`/notes/${encodeURI(state.path)}/pin`, { method: "POST" });
    toast(r.pinned ? "Pinned 📌" : "Unpinned");
    loadList();
  } catch (e) { toast(e.message, true); }
}
$("#trash-close").onclick = () => $("#trash-modal").classList.add("hidden");
$("#trash-modal").onclick = (e) => { if (e.target.id === "trash-modal") $("#trash-modal").classList.add("hidden"); };
async function openTrash() {
  $("#trash-modal").classList.remove("hidden");
  const items = await api("/trash");
  const b = $("#trash-body");
  if (!items.length) { b.innerHTML = '<p class="vault-note">Trash is empty.</p>'; return; }
  b.innerHTML = items.map((t) => `<div class="v-row"><span>🗒 ${esc(t.title)} <span class="pm">${esc(t.deleted_at)}</span></span>
    <span><button class="btn t-restore" data-id="${esc(t.id)}">Restore</button>
    <button class="icon danger t-purge" data-id="${esc(t.id)}" title="delete forever">🗑</button></span></div>`).join("");
  b.querySelectorAll(".t-restore").forEach((x) => (x.onclick = async () => {
    const n = await api(`/trash/${x.dataset.id}/restore`, { method: "POST" });
    await loadList(); openTrash(); toast("Restored"); openNote(n.path);
  }));
  b.querySelectorAll(".t-purge").forEach((x) => (x.onclick = async () => {
    if (!confirm("Delete forever? This cannot be undone.")) return;
    await api(`/trash/${x.dataset.id}`, { method: "DELETE" }); openTrash();
  }));
}
async function cryptNote(which) {
  if (!state.path) return toast("Open a note first", true);
  if (state.dirty) await save();
  try {
    await api(`/notes/${encodeURI(state.path)}/${which}`, { method: "POST" });
    // reflect the new at-rest state *synchronously* — openNote below is async, and
    // until it lands the editor still holds plaintext. Without this, a keystroke in
    // that window would persist an encrypted note's plaintext to a localStorage draft.
    state.encrypted = which === "encrypt";
    if (state.encrypted) clearDraft();   // drop any pre-encryption plaintext draft
    toast(which === "encrypt" ? "Encrypted at rest 🔒" : "Decrypted 🔓");
    await openNote(state.path);
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
  // normalize away spaces/punctuation so "focus mode" matches "focus-mode (…)"
  const norm = (s) => s.toLowerCase().replace(/[^a-z0-9]/g, "");
  needle = norm(needle); hay = norm(hay);
  if (!needle) return 1;
  let i = 0, score = 0, streak = 0;
  for (const ch of hay) {
    if (i < needle.length && ch === needle[i]) { i++; streak++; score += streak; }
    else streak = 0;
  }
  return i === needle.length ? score : 0;
}
function renderPalette(q) {
  const cmds = [...COMMANDS, ...Plugins.registry.commands]
    .map((c) => ({ ...c, kind: "cmd", label: c.name, s: fuzzy(q, c.name) }));
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
  Editor.sync(); scheduleSave();
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

/* ---------- tag browser ---------- */
$("#tags-browser-close").onclick = () => $("#tags-browser-modal").classList.add("hidden");
$("#tags-browser-modal").onclick = (e) => { if (e.target.id === "tags-browser-modal") $("#tags-browser-modal").classList.add("hidden"); };
async function openTagBrowser() {
  $("#tags-browser-modal").classList.remove("hidden");
  const tags = await api("/tags");
  state.allTags = tags;
  $("#tags-browser-count").textContent = `${tags.length}`;
  const b = $("#tags-browser-body");
  b.innerHTML = tags.length
    ? `<div class="tag-cloud">${tags.map((t) =>
        `<button class="tag-chip" data-t="${esc(t.tag)}">#${esc(t.tag)} <span>${t.c}</span></button>`).join("")}</div>`
    : '<p class="vault-note">No tags yet. Add <code>#tags</code> to your notes.</p>';
  b.querySelectorAll(".tag-chip").forEach((c) => (c.onclick = () => {
    $("#tags-browser-modal").classList.add("hidden"); filterByTag(c.dataset.t);
  }));
}

/* ---------- tasks (aggregated across notes) ---------- */
$("#tasks-close").onclick = () => $("#tasks-modal").classList.add("hidden");
$("#tasks-modal").onclick = (e) => { if (e.target.id === "tasks-modal") $("#tasks-modal").classList.add("hidden"); };
$("#tasks-done").onchange = renderTasks;
async function openTasks() {
  $("#tasks-modal").classList.remove("hidden");
  await renderTasks();
}
async function renderTasks() {
  const showDone = $("#tasks-done").checked;
  const tasks = await api(`/tasks?include_done=${showDone}`);
  const open = tasks.filter((t) => !t.done).length;
  $("#tasks-count").textContent = `${open} open`;
  const b = $("#tasks-body");
  if (!tasks.length) { b.innerHTML = '<p class="vault-note">No tasks yet. Add <code>- [ ] a todo</code> to any note.</p>'; return; }
  const byNote = {};
  for (const t of tasks) (byNote[t.path] ||= { title: t.title, items: [] }).items.push(t);
  b.innerHTML = Object.entries(byNote).map(([path, g]) =>
    `<div class="task-group"><div class="tg-head" data-p="${esc(path)}">${esc(g.title)}</div>`
    + g.items.map((t) =>
      `<label class="tg-item${t.done ? " done" : ""}"><input type="checkbox" class="tg-box"${t.done ? " checked" : ""} `
      + `data-p="${esc(t.path)}" data-line="${t.line}"><span class="tg-text" data-p="${esc(t.path)}" data-line="${t.line}">${esc(t.text)}</span></label>`).join("")
    + `</div>`).join("");
  b.querySelectorAll(".tg-box").forEach((x) => (x.onchange = () =>
    toggleTaskInNote(x.dataset.p, +x.dataset.line, x.checked).then(renderTasks)));
  b.querySelectorAll(".tg-text, .tg-head").forEach((x) => (x.onclick = () => {
    $("#tasks-modal").classList.add("hidden");
    openNote(x.dataset.p).then(() => {
      if (x.dataset.line !== undefined) scrollToHeading(+x.dataset.line, 0);
    });
  }));
}
async function toggleTaskInNote(path, line, done) {
  const n = await api(`/notes/${encodeURI(path)}`);
  if (n.locked) { toast("Note is locked", true); return; }
  const lines = (n.body || "").split("\n");
  if (line < 0 || line >= lines.length) return;
  lines[line] = lines[line].replace(/^(\s*[-*]\s+)\[[ xX]\]/, (_, p) => p + (done ? "[x]" : "[ ]"));
  await api(`/notes/${encodeURI(path)}`, { method: "PUT", body: { body: lines.join("\n"), frontmatter: n.frontmatter } });
  if (path === state.path) openNote(path);   // keep the open editor in sync
}

/* ---------- calendar (daily notes) ---------- */
let calYear, calMonth;   // month currently shown
const MONTHS = ["January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December"];
$("#cal-close").onclick = () => $("#calendar-modal").classList.add("hidden");
$("#calendar-modal").onclick = (e) => { if (e.target.id === "calendar-modal") $("#calendar-modal").classList.add("hidden"); };
$("#cal-prev").onclick = () => { if (--calMonth < 0) { calMonth = 11; calYear--; } renderCalendar(); };
$("#cal-next").onclick = () => { if (++calMonth > 11) { calMonth = 0; calYear++; } renderCalendar(); };
async function openCalendar() {
  $("#calendar-modal").classList.remove("hidden");
  const now = new Date();
  calYear = now.getFullYear(); calMonth = now.getMonth();
  await renderCalendar();
}
async function renderCalendar() {
  const dates = new Set(await api("/daily/dates").catch(() => []));
  const pad = (n) => String(n).padStart(2, "0");
  const iso = (d) => `${calYear}-${pad(calMonth + 1)}-${pad(d)}`;
  const todayIso = (() => { const n = new Date(); return `${n.getFullYear()}-${pad(n.getMonth() + 1)}-${pad(n.getDate())}`; })();
  $("#cal-title").textContent = `${MONTHS[calMonth]} ${calYear}`;
  const first = new Date(calYear, calMonth, 1).getDay();     // 0=Sun
  const days = new Date(calYear, calMonth + 1, 0).getDate(); // last day
  let cells = ["S", "M", "T", "W", "T", "F", "S"].map((d) => `<div class="cal-dow">${d}</div>`).join("");
  for (let i = 0; i < first; i++) cells += '<div class="cal-cell empty"></div>';
  for (let d = 1; d <= days; d++) {
    const id = iso(d);
    const cls = "cal-cell" + (dates.has(id) ? " has" : "") + (id === todayIso ? " today" : "");
    cells += `<div class="${cls}" data-d="${id}">${d}</div>`;
  }
  $("#calendar-body").innerHTML = `<div class="cal-grid">${cells}</div>`;
  $("#calendar-body").querySelectorAll(".cal-cell[data-d]").forEach((c) =>
    (c.onclick = () => openDay(c.dataset.d)));
}
async function openDay(date) {
  $("#calendar-modal").classList.add("hidden");
  const d = await api(`/daily?date=${date}`);
  await loadList(); openNote(d.path);
}
const _isoLocal = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
function shiftDaily(delta) {
  // base on the open daily note if there is one, else today
  let base = null;
  const m = (state.path || "").match(/(\d{4})-(\d{2})-(\d{2})\.md$/);
  const d = m ? new Date(+m[1], +m[2] - 1, +m[3], 12) : new Date();
  d.setDate(d.getDate() + delta);
  openDay(_isoLocal(d));
}

/* ---------- settings ---------- */
$("#settings-close").onclick = () => $("#settings-modal").classList.add("hidden");
$("#settings-modal").onclick = (e) => { if (e.target.id === "settings-modal") $("#settings-modal").classList.add("hidden"); };
/* Plugin enable/disable toggles inside the settings modal. Vault plugins carry
   an explicit warning: enabling one runs third-party code in the app. */
async function renderPluginSettings() {
  const box = $("#set-plugins");
  try {
    const list = await api("/plugins");
    if (!list.length) { box.innerHTML = '<p class="vault-note">No plugins installed. Drop one into <code>plugins/</code> in your vault.</p>'; return; }
    box.innerHTML = "<p class=\"vault-note\"><b>Plugins</b> — changes apply after reload.</p>" + list.map((p) => `
      <label class="set-row plugin-row">
        <span>${esc(p.name)} <small>${esc(p.version)}${p.source === "vault" ? " · vault" : ""}</small>
          <br><small class="dim">${esc(p.description)}</small></span>
        <input type="checkbox" data-plugin="${esc(p.name)}" data-source="${esc(p.source)}"${p.enabled ? " checked" : ""}>
      </label>`).join("");
    box.querySelectorAll("input[data-plugin]").forEach((cb) => {
      cb.onchange = async () => {
        if (cb.checked && cb.dataset.source === "vault" &&
            !confirm(`"${cb.dataset.plugin}" is a vault plugin — it runs third-party code inside Grimoire. Enable it?`)) {
          cb.checked = false; return;
        }
        try {
          await api(`/plugins/${cb.dataset.plugin}/enable`, { method: "POST", body: { enabled: cb.checked } });
          toast(`${cb.dataset.plugin} ${cb.checked ? "enabled" : "disabled"} — reload to apply`);
        } catch (e) { toast(e.message, true); cb.checked = !cb.checked; }
      };
    });
  } catch { box.innerHTML = ""; }
}

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
    <label class="set-row"><span>Editor (this device)</span>
      <select id="set-editor">
        <option value="live"${Editor.mode === "live" ? " selected" : ""}>live preview</option>
        <option value="classic"${Editor.mode === "classic" ? " selected" : ""}>classic (plain text)</option>
      </select></label>
    <div id="set-plugins"><p class="vault-note">Loading plugins…</p></div>
    <div class="ask-input-row"><button id="set-save" class="btn full">Save</button></div>`;
  renderPluginSettings();
  $("#set-editor").onchange = (e) => Editor.setMode(e.target.value);
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

/* ---------- properties / frontmatter editor ---------- */
const PROP_HIDDEN = new Set(["title", "tags", "aliases", "pinned", "private", "created", "updated", "encrypted"]);
$("#props-btn").onclick = openProps;
$("#props-close").onclick = () => $("#props-modal").classList.add("hidden");
$("#props-modal").onclick = (e) => { if (e.target.id === "props-modal") $("#props-modal").classList.add("hidden"); };
function _prCustomRow(k = "", v = "") {
  return `<div class="pr-crow"><input class="pr-ck" value="${esc(k)}" placeholder="key">`
    + `<input class="pr-cv" value="${esc(typeof v === "object" ? JSON.stringify(v) : v)}" placeholder="value">`
    + `<button class="icon danger pr-del" title="remove">🗑</button></div>`;
}
function openProps() {
  if (!state.path) return toast("Open a note first", true);
  if (state.locked) return toast("Unlock the vault to edit properties", true);
  const fm = state.frontmatter || {};
  const list = (v) => Array.isArray(v) ? v.join(", ") : (v ? String(v) : "");
  const custom = Object.entries(fm).filter(([k]) => !PROP_HIDDEN.has(k));
  $("#props-body").innerHTML = `
    <label class="set-row"><span>Title</span><input id="pr-title" value="${esc(fm.title || $("#title").value || "")}"></label>
    <label class="set-row"><span>Tags</span><input id="pr-tags" value="${esc(list(fm.tags))}" placeholder="comma, separated"></label>
    <label class="set-row"><span>Aliases</span><input id="pr-aliases" value="${esc(list(fm.aliases))}" placeholder="comma, separated"></label>
    <div class="set-row"><span>Flags</span><span class="pr-flags">
      <label class="chk"><input type="checkbox" id="pr-pinned"${fm.pinned ? " checked" : ""}> pinned</label>
      <label class="chk"><input type="checkbox" id="pr-private"${fm.private ? " checked" : ""}> private</label></span></div>
    <div class="pr-clabel">Custom fields</div>
    <div id="pr-custom">${custom.map((c) => _prCustomRow(c[0], c[1])).join("")}</div>
    <button id="pr-add" class="btn">+ field</button>
    <p class="vault-note">Created ${esc(fm.created || "—")} · Updated ${esc(fm.updated || "—")}</p>
    <div class="ask-input-row"><button id="pr-save" class="btn full">Save properties</button></div>`;
  $("#props-modal").classList.remove("hidden");
  const wireDel = () => $("#pr-custom").querySelectorAll(".pr-del").forEach((b) =>
    (b.onclick = () => { b.closest(".pr-crow").remove(); }));
  wireDel();
  $("#pr-add").onclick = () => { $("#pr-custom").insertAdjacentHTML("beforeend", _prCustomRow()); wireDel(); };
  $("#pr-save").onclick = saveProps;
}
async function saveProps() {
  const fm = { ...(state.frontmatter || {}) };
  const newFm = {};
  // custom fields first (title/tags/etc. override below)
  $("#pr-custom").querySelectorAll(".pr-crow").forEach((r) => {
    const k = r.querySelector(".pr-ck").value.trim();
    if (k && !PROP_HIDDEN.has(k)) newFm[k] = r.querySelector(".pr-cv").value;
  });
  const title = $("#pr-title").value.trim();
  if (title) newFm.title = title;
  const tags = $("#pr-tags").value.split(",").map((s) => s.trim()).filter(Boolean);
  if (tags.length) newFm.tags = tags;
  const aliases = $("#pr-aliases").value.split(",").map((s) => s.trim()).filter(Boolean);
  if (aliases.length) newFm.aliases = aliases;
  if ($("#pr-pinned").checked) newFm.pinned = true;
  if ($("#pr-private").checked) newFm.private = true;
  if (fm.created) newFm.created = fm.created;              // preserve creation stamp
  try {
    const n = await api(`/notes/${encodeURI(state.path)}`, { method: "PUT",
      body: { body: $("#content").value, frontmatter: newFm } });
    state.frontmatter = n.frontmatter || newFm;
    $("#title").value = n.title || title;
    updatePrivateToggle();
    $("#props-modal").classList.add("hidden");
    toast("Properties saved");
    loadList(); refreshTemplates();
  } catch (e) { toast(e.message, true); }
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
  const cur = localStorage.getItem("grimoire-theme") || "auto";
  const next = THEMES[(THEMES.indexOf(cur) + 1) % THEMES.length];
  localStorage.setItem("grimoire-theme", next);
  applyTheme(next); toast(`Theme: ${next}`);
};
applyTheme(localStorage.getItem("grimoire-theme") || "auto");

/* ---------- focus / zen mode ---------- */
function setZen(on) {
  $("#app").classList.toggle("zen", on);
  $("#zen-exit").classList.toggle("hidden", !on);
  if (on) $("#content").focus();
}
function toggleZen() { setZen(!$("#app").classList.contains("zen")); }
$("#zen-exit").onclick = () => setZen(false);
addEventListener("keydown", (e) => {
  if (e.key === "Escape" && $("#app").classList.contains("zen")) { e.preventDefault(); setZen(false); }
});

/* ---------- collapsible sidebar (desktop) ---------- */
function setSidebarCollapsed(on) {
  $("#app").classList.toggle("sidebar-collapsed", on);
  localStorage.setItem("grimoire-side-collapsed", on ? "1" : "");
  $("#sidebar-toggle").textContent = on ? "⇥" : "⇤";
  $("#sidebar-toggle").title = (on ? "show" : "hide") + " sidebar (Ctrl+\\)";
}
function toggleSidebar() {
  if (isNarrow()) { $("#menu-open").click(); return; }   // phones use the overlay
  setSidebarCollapsed(!$("#app").classList.contains("sidebar-collapsed"));
}
$("#sidebar-toggle").onclick = toggleSidebar;
addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === "\\") { e.preventDefault(); toggleSidebar(); }
});
if (localStorage.getItem("grimoire-side-collapsed")) setSidebarCollapsed(true);

/* ---------- drag-to-resize (sidebar + split divider) ---------- */
function makeResizer(handle, onDrag) {
  handle.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    handle.classList.add("dragging"); document.body.classList.add("resizing");
    const move = (ev) => onDrag(ev.clientX);
    const up = (ev) => {
      handle.releasePointerCapture(e.pointerId);
      handle.classList.remove("dragging"); document.body.classList.remove("resizing");
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", up);
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", up);
  });
}
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
// sidebar width — persisted
const savedSide = localStorage.getItem("grimoire-side-w");
if (savedSide) document.documentElement.style.setProperty("--side-w", savedSide + "px");
makeResizer($("#sidebar-resize"), (x) => {
  const w = clamp(Math.round(x), 200, Math.min(560, innerWidth - 360));
  document.documentElement.style.setProperty("--side-w", w + "px");
  localStorage.setItem("grimoire-side-w", w);
});
// split divider — middle pane width (px); resets to 50/50 each split
makeResizer($("#split-resize"), (x) => {
  const sideW = $("#sidebar").getBoundingClientRect().width;
  const avail = innerWidth - sideW;
  const mainW = clamp(Math.round(x - sideW), 280, avail - 280);
  document.documentElement.style.setProperty("--main-w", mainW + "px");
});

/* ---------- split view (second editor pane) ---------- */
const pane2 = { path: null, dirty: false, frontmatter: {}, saveTimer: null, locked: false };
const ta2 = $("#content2");
const isNarrow = () => matchMedia("(max-width: 780px)").matches;
function openSplit(path) {
  path = path || state.path;
  if (!path) return toast("Open a note first", true);
  if (isNarrow()) return openNote(path);   // no split on phones
  document.documentElement.style.setProperty("--main-w", "1fr");   // start balanced
  $("#app").classList.add("split");
  loadPane2(path);
}
async function loadPane2(path) {
  if (pane2.dirty) await savePane2();
  const n = await api(`/notes/${encodeURI(path)}`);
  pane2.path = n.path; pane2.dirty = false; pane2.frontmatter = n.frontmatter || {}; pane2.locked = !!n.locked;
  $("#title2").value = n.title || "";
  if (pane2.locked) { ta2.value = "🔒 encrypted — unlock in the main pane to edit."; ta2.readOnly = true; }
  else { ta2.value = n.body || ""; ta2.readOnly = false; }
  $("#save-state2").textContent = "";
  if (!$("#preview2").classList.contains("hidden")) renderPreview2();
}
function closeSplit() {
  if (pane2.dirty) savePane2();
  $("#app").classList.remove("split");
  pane2.path = null;
}
function schedule2() {
  pane2.dirty = true; $("#save-state2").textContent = "…";
  clearTimeout(pane2.saveTimer); pane2.saveTimer = setTimeout(savePane2, 700);
}
async function savePane2() {
  if (!pane2.path || !pane2.dirty || pane2.locked) return;
  clearTimeout(pane2.saveTimer);
  try {
    const title = $("#title2").value.trim();
    const fm = { ...pane2.frontmatter };
    if (title) fm.title = title; else delete fm.title;
    await api(`/notes/${encodeURI(pane2.path)}`, { method: "PUT", body: { body: ta2.value, frontmatter: fm } });
    pane2.dirty = false; $("#save-state2").textContent = "saved";
    setTimeout(() => $("#save-state2").textContent = "", 1200);
    // if the same note is open in the main pane, pull the update in
    if (pane2.path === state.path && !state.locked) {
      const m = await api(`/notes/${encodeURI(state.path)}`);
      if (!state.dirty) { $("#content").value = m.body || ""; Editor.sync(); }
    }
    loadList();
  } catch (e) { $("#save-state2").textContent = "!"; toast(e.message, true); }
}
function renderPreview2() {
  $("#preview2").innerHTML = `<div class="md">${mdToHtml(ta2.value)}</div>`;
  $("#preview2").querySelectorAll("a.wikilink").forEach((a) =>
    (a.onclick = (e) => { e.preventDefault(); resolveIntoPane2(a.dataset.target); }));
  hydrateDynamicBlocks($("#preview2"));
  Plugins.renderFences($("#preview2"));
}
function resolveIntoPane2(target) {
  const hit = state.notes.find((n) => (n.title || "").toLowerCase() === target.toLowerCase()
    || n.path.replace(/\.md$/, "").split("/").pop().toLowerCase() === target.toLowerCase());
  const aliasPath = state.aliases[target.toLowerCase()];
  if (hit) loadPane2(hit.path); else if (aliasPath) loadPane2(aliasPath);
}
$("#split-btn").onclick = () => openSplit(state.path);
$("#editor2-close").onclick = closeSplit;
$("#preview-toggle2").onclick = () => {
  const pv = $("#preview2"), t = $("#content2");
  if (pv.classList.contains("hidden")) { renderPreview2(); pv.classList.remove("hidden"); t.classList.add("hidden"); }
  else { pv.classList.add("hidden"); t.classList.remove("hidden"); }
};
$("#title2").oninput = schedule2;
ta2.addEventListener("input", () => { schedule2(); if (!$("#preview2").classList.contains("hidden")) renderPreview2(); });
addEventListener("beforeunload", () => { if (pane2.dirty) savePane2(); });

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
  if (h && h !== state.path)
    openNote(h).catch(() => toast(`"${h}" not found`, true));   // list may be stale
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
    toast("Shared to Grimoire"); await loadList(); openNote(r.path);
    return true;
  } catch { return false; }
}

/* Snippets offered by the in-editor slash menu (beyond commands + templates). */
const SLASH_SNIPPETS = [
  { name: "date", detail: "today's date", insert: () => new Date().toISOString().slice(0, 10) },
  { name: "task", detail: "checkbox item", insert: () => "- [ ] " },
  { name: "table", detail: "2×2 table", insert: () => "| A | B |\n|---|---|\n|  |  |\n" },
  { name: "callout", detail: "note callout", insert: () => "> [!note] Title\n> body\n" },
  { name: "query", detail: "live query block", insert: () => "```query\ntag: \n```\n" },
  { name: "footnote", detail: "footnote pair", insert: () => "[^1]\n\n[^1]: " },
  { name: "hr", detail: "divider", insert: () => "\n---\n" },
];

function slashCommandItems(q) {
  const needle = q.toLowerCase();
  const snippets = SLASH_SNIPPETS
    .filter((s) => s.name.includes(needle))
    .map((s) => ({ name: s.name, detail: s.detail, insert: s.insert() }));
  const pluginSnips = Plugins.registry.slashSnippets
    .filter((sn) => sn.name.includes(needle))
    .map((sn) => ({ name: sn.name, detail: sn.detail || "",
                    insert: typeof sn.insert === "function" ? sn.insert() : sn.insert }));
  const commands = [...COMMANDS, ...Plugins.registry.commands]
    .filter((c) => c.name.toLowerCase().includes(needle))
    .slice(0, 8)
    .map((c) => ({ name: c.name.toLowerCase().replace(/[^\w]+/g, "-").replace(/^-|-$/g, ""),
                   detail: c.name, run: c.run }));
  return [...snippets, ...pluginSnips, ...commands].slice(0, 12);
}

/* Sidebar sections contributed by plugins. */
function renderPluginPanels() {
  const hostEl = $("#plugin-panels");
  if (!hostEl) return;
  hostEl.innerHTML = "";
  for (const p of Plugins.registry.panels) {
    const sec = document.createElement("section");
    sec.className = "plugin-panel";
    sec.innerHTML = `<div class="pp-title">${esc(p.title)}</div><div class="pp-body"></div>`;
    hostEl.appendChild(sec);
    try { p.render(sec.querySelector(".pp-body")); }
    catch (e) { sec.querySelector(".pp-body").textContent = `panel failed: ${e.message}`; }
  }
}

/* Edge-swipe navigation on phones: swipe right from the left edge opens the
   sidebar; swipe left anywhere on the open sidebar closes it. Pointer events
   only — no library, no interference with text selection (edge-start only). */
(function wireEdgeSwipe() {
  let startX = null, startY = null, tracking = false;
  addEventListener("touchstart", (e) => {
    if (matchMedia("(min-width: 781px)").matches) return;
    const t = e.touches[0];
    const sidebarOpen = $("#sidebar").classList.contains("open");
    // start tracking from the left screen edge (open) or on the sidebar (close)
    if ((!sidebarOpen && t.clientX < 24) || (sidebarOpen && e.target.closest("#sidebar"))) {
      startX = t.clientX; startY = t.clientY; tracking = true;
    }
  }, { passive: true });
  addEventListener("touchend", (e) => {
    if (!tracking) return;
    tracking = false;
    const t = e.changedTouches[0];
    const dx = t.clientX - startX, dy = t.clientY - startY;
    if (Math.abs(dx) < 60 || Math.abs(dy) > Math.abs(dx)) return;   // not a horizontal swipe
    const sidebar = $("#sidebar");
    if (dx > 0 && !sidebar.classList.contains("open")) {
      sidebar.classList.add("open"); $("#app").classList.add("side-open");
    } else if (dx < 0 && sidebar.classList.contains("open")) {
      sidebar.classList.remove("open"); $("#app").classList.remove("side-open");
    }
  }, { passive: true });
})();

(async function boot() {
  setNoteOpener(openNote);                          // wiki-link clicks in previews
  initGraph({ openNote, currentPath: () => state.path });
  await loadList();
  refreshTemplates();
  await Editor.init({
    onInput: () => { scheduleSave(); updateWordCount();
      if (!$("#preview").classList.contains("hidden")) renderPreview(); },
    onSave: () => save(),
    onOpenLink: (target, opts) => opts?.split ? resolveIntoPane2(target) : resolveAndOpen(target),
    onTagClick: (tag) => filterByTag(tag),
    getLinkCompletions: (q) => {
      const needle = q.toLowerCase();
      const titles = state.notes.map((n) => n.title || n.path.replace(/\.md$/, ""));
      return titles.filter((t) => t.toLowerCase().includes(needle)).slice(0, 10);
    },
    getTagCompletions: (q) => (state.allTags || [])
      .map((t) => t.tag).filter((t) => t.toLowerCase().startsWith(q.toLowerCase())).slice(0, 10),
    getSlashCommands: slashCommandItems,
    onFiles: (files) => { for (const f of files) uploadAttachment(f); },
  });
  initCanvas({ api, toast, openNote, esc });
  await Plugins.init({
    api,
    toast,
    openNote,
    getCurrentNote: () => state.path
      ? { path: state.path, title: $("#title").value, body: $("#content").value }
      : null,
    insertText: insertAtCursor,
    renderPanels: renderPluginPanels,
    onCommandsChanged: () => {},        // palette reads the registry lazily
  });
  renderPluginPanels();
  if (await handleShareTarget()) return;
  const hash = decodeURI(location.hash.slice(1));
  if (hash) await openNote(hash).catch(() => state.notes[0] && openNote(state.notes[0].path));
  else if (state.notes[0]) openNote(state.notes[0].path);
  // readiness beacon: handlers are wired and the editor is mounted (e2e + probes)
  document.body.dataset.ready = "1";
})();
