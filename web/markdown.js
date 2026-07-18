/**
 * Client markdown engine — the offline renderer behind the preview panes,
 * slides, and hover cards. Mirrors the server renderer (server/render.py):
 * escape first, then apply block + inline rules. Kept in lockstep — any rule
 * added here must land there too (and vice versa).
 *
 * The engine is deliberately decoupled from the app shell: link resolution
 * comes from an injected note index (`setNoteIndex`), navigation from an
 * injected opener (`setNoteOpener`). Nothing here touches app state.
 */
import { api, esc } from "/util.js";

/* Injected by the app shell: */
const noteIndex = { notes: [], aliases: {} };
let openNoteFn = () => {};

/** Refresh the title/stem/alias index used to resolve [[wiki-links]]. */
export function setNoteIndex(notes, aliases) {
  noteIndex.notes = notes || [];
  noteIndex.aliases = aliases || {};
}

/** Provide the navigation callback used by rendered wiki-links/embeds. */
export function setNoteOpener(fn) { openNoteFn = fn; }

export const isTableRow = (l) => { const s = l.trim(); return s.startsWith("|") && (s.match(/\|/g) || []).length >= 2; };
const isTableSep = (l) => { const s = l.trim(); return /^\|?[\s:|-]*-[\s:|-]*\|?$/.test(s) && s.includes("-") && s.includes("|"); };
const tableCells = (l) => l.trim().replace(/^\||\|$/g, "").split("|").map((c) => c.trim());
const _HL_KW = new Set(("const let var function func fn return if else elif for while class struct interface "
  + "type enum import export from package use pub async await new def lambda try except catch finally throw with "
  + "match case switch break continue in of is not and or public private protected static void int float double "
  + "string str bool true false True False None null nil self this super impl trait yield do then end module "
  + "namespace typedef extends implements").split(" "));
const _HL_TOKEN = /(\/\/[^\n]*|#[^\n]*|\/\*[\s\S]*?\*\/)|("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|`(?:\\.|[^`\\])*`)|(\b\d[\d_.]*(?:[eE][+-]?\d+)?\b|\b0[xX][0-9a-fA-F]+\b)|([A-Za-z_$][\w$]*)/g;
export function highlightCode(code) {
  let out = "", last = 0, m;
  _HL_TOKEN.lastIndex = 0;
  while ((m = _HL_TOKEN.exec(code))) {
    out += esc(code.slice(last, m.index));
    if (m[1]) out += `<span class="hl-com">${esc(m[1])}</span>`;
    else if (m[2]) out += `<span class="hl-str">${esc(m[2])}</span>`;
    else if (m[3]) out += `<span class="hl-num">${esc(m[3])}</span>`;
    else out += _HL_KW.has(m[4]) ? `<span class="hl-kw">${esc(m[4])}</span>` : esc(m[4]);
    last = m.index + m[0].length;
  }
  out += esc(code.slice(last));
  return out;
}
const IMAGE_EXTS = /\.(png|jpe?g|gif|webp|svg|avif)$/i;

/** Stable heading anchor — mirrors server render.heading_id() exactly. */
export function headingId(text) {
  const slug = text.trim().toLowerCase().replace(/[^\w\s-]/g, "")
    .replace(/[\s_]+/g, "-").replace(/^-+|-+$/g, "");
  return `h-${slug || "heading"}`;
}

export function mdToHtml(src) {
  // small, safe-ish markdown: escape first, then apply inline + block rules
  let resolved = new Set(noteIndex.notes.flatMap((n) => [
    (n.title || "").toLowerCase(), n.path.replace(/\.md$/, "").split("/").pop().toLowerCase()]));
  Object.keys(noteIndex.aliases).forEach((a) => resolved.add(a));
  const lines = src.split("\n");
  let html = "", listOpen = false;
  // footnote definitions ([^id]: text) render once, as a list at the end
  const footnotes = [];
  for (const l of lines) {
    const m = l.match(/^\[\^([\w-]+)\]:\s+(.*)$/);
    if (m) footnotes.push([m[1], m[2]]);
  }
  const inline = (t) => esc(t)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/!\[\[([^\[\]|]+?)\]\]/g, (_, src) =>
      `<img class="embed" src="/api/file/${encodeURI(src.trim())}" alt="${esc(src.trim())}" loading="lazy">`)
    .replace(/\[\[([^\]|]+?)(?:\|([^\]]+))?\]\]/g, (_, tgt, al) => {
      const base = tgt.split("#")[0].trim();
      const cls = resolved.has(base.toLowerCase()) ? "wikilink" : "wikilink unresolved";
      return `<a class="${cls}" data-target="${esc(base)}">${esc(al || tgt)}</a>`;
    })
    .replace(/\[\^([\w-]+)\]/g,
      '<sup class="fn-ref" id="fnref-$1"><a href="#fn-$1">$1</a></sup>')
    .replace(/(^|\s)#([A-Za-z][\w/-]*)/g, '$1<span class="tag">#$2</span>')
    .replace(/==([^=]+)==/g, "<mark>$1</mark>")
    .replace(/~~([^~]+)~~/g, "<del>$1</del>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(?<!\*)\*([^*]+)\*(?!\*)/g, "<em>$1</em>")
    .replace(/\[([^\]]+)\]\((https?:[^)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  const closeList = () => { if (listOpen) { html += "</ul>"; listOpen = false; } };
  for (let lineNo = 0; lineNo < lines.length; lineNo++) {
    const raw = lines[lineNo];
    if (/^\[\^[\w-]+\]:\s/.test(raw)) continue;   // definitions render at the end
    // whole-line ![[Note]] (non-image) → transclusion placeholder, hydrated async
    const em = raw.trim().match(/^!\[\[([^\[\]|]+?)\]\]$/);
    if (em && !IMAGE_EXTS.test(em[1].trim())) {
      closeList();
      html += `<div class="embed embed-pending" data-embed="${esc(em[1].split("#")[0].trim())}">${esc(em[1])} …</div>`;
      continue;
    }
    if (raw.trim().startsWith("```")) {
      closeList();
      const lang = raw.trim().slice(3).trim();
      let j = lineNo + 1; const buf = [];
      while (j < lines.length && !lines[j].trim().startsWith("```")) buf.push(lines[j++]);
      if (lang === "query") {
        // live query — placeholder now, results filled in by hydrateDynamicBlocks
        html += `<div class="query query-pending" data-q="${esc(buf.join("\n"))}">running query…</div>`;
        lineNo = j; continue;
      }
      const cls = lang ? ` class="lang-${esc(lang)}"` : "";
      html += `<pre><code${cls} data-lang="${esc(lang)}">${highlightCode(buf.join("\n"))}</code></pre>`;
      lineNo = j; continue;
    }
    if (isTableRow(raw) && lineNo + 1 < lines.length && isTableSep(lines[lineNo + 1])) {
      closeList();
      let j = lineNo + 2; const rows = [];
      while (j < lines.length && isTableRow(lines[j])) rows.push(lines[j++]);
      const th = tableCells(raw).map((c) => `<th>${inline(c)}</th>`).join("");
      const tb = rows.map((r) => `<tr>${tableCells(r).map((c) => `<td>${inline(c)}</td>`).join("")}</tr>`).join("");
      html += `<div class="table-wrap"><table><thead><tr>${th}</tr></thead><tbody>${tb}</tbody></table></div>`;
      lineNo = j - 1;   // the for-loop ++ lands on j
      continue;
    }
    const h = raw.match(/^(#{1,3})\s+(.+)$/);
    if (h) { closeList(); html += `<h${h[1].length} id="${headingId(h[2])}">${inline(h[2])}</h${h[1].length}>`; continue; }
    const task = raw.match(/^\s*[-*]\s+\[([ xX])\]\s+(.*)$/);
    if (task) {
      if (!listOpen) { html += "<ul>"; listOpen = true; }
      const done = task[1].toLowerCase() === "x";
      html += `<li class="task${done ? " done" : ""}"><input type="checkbox" class="task-box" `
        + `data-line="${lineNo}"${done ? " checked" : ""}>${inline(task[2])}</li>`;
      continue;
    }
    if (/^\s*[-*]\s+/.test(raw)) { if (!listOpen) { html += "<ul>"; listOpen = true; } html += `<li>${inline(raw.replace(/^\s*[-*]\s+/, ""))}</li>`; continue; }
    const cm = raw.match(/^\s*>\s*\[!(\w+)\]\s*(.*)$/);
    if (cm) {
      closeList();
      let j = lineNo + 1; const body = [];
      while (j < lines.length && /^\s*>/.test(lines[j])) { body.push(lines[j].replace(/^\s*>\s?/, "")); j++; }
      const kind = cm[1].toLowerCase();
      const title = cm[2].trim() || kind.charAt(0).toUpperCase() + kind.slice(1);
      const inner = body.filter((l) => l.trim() !== "").map((l) => `<p>${inline(l)}</p>`).join("");
      html += `<div class="callout callout-${esc(kind)}"><div class="callout-title">${inline(title)}</div><div class="callout-body">${inner}</div></div>`;
      lineNo = j - 1; continue;
    }
    if (/^\s*>\s?/.test(raw)) { closeList(); html += `<blockquote>${inline(raw.replace(/^\s*>\s?/, ""))}</blockquote>`; continue; }
    if (raw.trim() === "") { closeList(); continue; }
    closeList(); html += `<p>${inline(raw)}</p>`;
  }
  closeList();
  if (footnotes.length) {
    html += `<div class="footnotes"><hr><ol>` + footnotes.map(([id, text]) =>
      `<li id="fn-${esc(id)}">${inline(text)} <a class="fn-back" href="#fnref-${esc(id)}">↩</a></li>`
    ).join("") + `</ol></div>`;
  }
  return html;
}

/* Async pass over a rendered preview: fill in live-query results and note
   transclusions. Both are placeholders from mdToHtml so the synchronous render
   stays instant; `depth` guards embedded notes that embed further notes. */
export async function hydrateDynamicBlocks(root, depth = 0) {
  for (const el of root.querySelectorAll(".query-pending")) {
    el.classList.remove("query-pending");
    try {
      const res = await api("/query", { method: "POST", body: { block: el.dataset.q } });
      el.innerHTML = queryResultHtml(res);
      el.querySelectorAll("a.wikilink").forEach((a) => {
        a.onclick = (e) => { e.preventDefault(); openNoteFn(a.dataset.path); };
      });
    } catch (e) { el.textContent = `query failed: ${e.message}`; }
  }
  for (const el of root.querySelectorAll(".embed-pending")) {
    el.classList.remove("embed-pending");
    const target = el.dataset.embed;
    try {
      const path = await resolvePath(target);
      if (!path) { el.textContent = `![[${target}]] — not found`; el.classList.add("embed-missing"); continue; }
      if (depth >= 1) { el.textContent = `${target} (embed depth limit)`; el.classList.add("embed-cycle"); continue; }
      const n = await api(`/notes/${encodeURI(path)}`);
      if (n.locked) { el.textContent = `${target} 🔒`; continue; }
      el.innerHTML = `<div class="embed-title"><a class="wikilink" data-target="${esc(target)}">${esc(n.title || target)}</a></div>`
        + `<div class="md">${mdToHtml(n.body || "")}</div>`;
      el.querySelector("a.wikilink").onclick = (e) => { e.preventDefault(); openNoteFn(path); };
      await hydrateDynamicBlocks(el, depth + 1);
    } catch (e) { el.textContent = `![[${target}]] — unavailable`; }
  }
}

/** Render a /api/query result ({render, columns, rows, errors}) to HTML. */
export function queryResultHtml(res) {
  if (res.errors?.length) return `<span class="query-error">query error: ${esc(res.errors.join("; "))}</span>`;
  if (res.render === "count") return `<span class="query-count">${res.count}</span>`;
  if (res.render === "table") {
    const cols = res.columns;
    const th = cols.map((c) => `<th>${esc(c)}</th>`).join("");
    const rows = res.rows.map((r) => "<tr>" + cols.map((c) => {
      if (c === "title") return `<td><a class="wikilink" data-path="${esc(r.path)}">${esc(r.title || r.path)}</a></td>`;
      if (c === "tags") return `<td>${(r.tags || []).map((t) => `<span class="tag">#${esc(t)}</span>`).join(" ")}</td>`;
      return `<td>${esc(String(r[c] ?? ""))}</td>`;
    }).join("") + "</tr>").join("");
    return `<div class="table-wrap"><table><thead><tr>${th}</tr></thead><tbody>${rows}</tbody></table></div>`;
  }
  return "<ul>" + res.rows.map((r) =>
    `<li><a class="wikilink" data-path="${esc(r.path)}">${esc(r.title || r.path)}</a></li>`).join("") + "</ul>";
}

/** Resolve a wiki-link target (title / stem / alias) to a vault path. */
export async function resolvePath(target) {
  const low = target.toLowerCase();
  const hit = noteIndex.notes.find((n) => (n.title || "").toLowerCase() === low
    || n.path.replace(/\.md$/, "").split("/").pop().toLowerCase() === low);
  if (hit) return hit.path;
  return noteIndex.aliases[low] || null;
}

