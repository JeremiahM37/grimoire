/**
 * Kanban boards for Grimoire.
 *
 * A ```kanban fence renders as a board. Simple text format:
 *
 *     ```kanban
 *     ## Todo
 *     - draft the intro
 *     - [[Research Note]] read sources
 *     ## Doing
 *     - outline chapters
 *     ## Done
 *     - pick a topic
 *     ```
 *
 * [[Wiki-links]] inside cards are clickable. Rendering is read-only in the
 * preview — edit the fence text to move cards (the source stays plain
 * markdown, greppable and diffable).
 */
const esc = (s) => s.replace(/[&<>"']/g,
  (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

function parseBoard(source) {
  const cols = [];
  for (const raw of source.split("\n")) {
    const col = raw.match(/^##\s+(.+)$/);
    if (col) { cols.push({ title: col[1].trim(), cards: [] }); continue; }
    const card = raw.match(/^\s*[-*]\s+(.*)$/);
    if (card && cols.length) cols[cols.length - 1].cards.push(card[1]);
  }
  return cols;
}

export function activate(grimoire) {
  grimoire.registerFenceRenderer("kanban", (el, source) => {
    const cols = parseBoard(source);
    if (!cols.length) { el.textContent = "kanban: add columns with '## Name'"; return; }
    el.innerHTML = `<div class="kb-board">` + cols.map((c) => `
      <div class="kb-col">
        <div class="kb-col-title">${esc(c.title)} <span class="kb-count">${c.cards.length}</span></div>
        ${c.cards.map((card) => `<div class="kb-card">${esc(card)
          .replace(/\[\[([^\]|]+?)(?:\|([^\]]+))?\]\]/g,
            (_, t, a) => `<a class="kb-link" data-target="${esc(t.trim())}">${esc(a || t)}</a>`)
        }</div>`).join("")}
      </div>`).join("") + `</div>`;
    el.querySelectorAll(".kb-link").forEach((a) => {
      a.onclick = () => grimoire.openNote(a.dataset.target + ".md")
        .catch?.(() => grimoire.toast(`note "${a.dataset.target}" not found`, true));
    });
  });

  grimoire.registerSlashSnippet({
    name: "kanban", detail: "kanban board",
    insert: "```kanban\n## Todo\n- first card\n## Doing\n## Done\n```\n",
  });
}
