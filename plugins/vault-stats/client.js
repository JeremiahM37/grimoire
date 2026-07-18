/**
 * Vault stats — a small sidebar dashboard: total notes, tag leaderboard.
 * Refreshes after every save so the numbers stay honest.
 */
const esc = (s) => String(s).replace(/[&<>"']/g,
  (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

export function activate(grimoire) {
  let body = null;

  async function refresh() {
    if (!body) return;
    try {
      const [notes, tags] = await Promise.all([
        grimoire.api("/notes?limit=1000"), grimoire.api("/tags"),
      ]);
      const top = (tags || []).slice(0, 5);
      body.innerHTML = `
        <div>${notes.length} notes · ${(tags || []).length} tags</div>
        ${top.length ? `<div style="margin-top:6px">${top.map((t) =>
          `<span class="tag" style="margin-right:6px">#${esc(t.tag)} <small>${t.c}</small></span>`
        ).join("")}</div>` : ""}`;
    } catch (e) {
      body.textContent = "stats unavailable";
    }
  }

  grimoire.registerPanel({
    id: "vault-stats",
    title: "📊 Vault",
    render(el) { body = el; refresh(); },
  });
  grimoire.on("note-save", refresh);
}
