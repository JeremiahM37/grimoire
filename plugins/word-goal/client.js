/**
 * Word goal — a daily writing target with a progress bar. Progress is the word
 * count of today's daily note (the one place daily writing accrues). The goal
 * is per-device (localStorage); click the number to change it.
 */
const GOAL_KEY = "grimoire-word-goal";

export function activate(grimoire) {
  let body = null;
  const goal = () => Math.max(50, parseInt(localStorage.getItem(GOAL_KEY) || "250", 10));

  // local calendar date — matches the server's daily-note naming (never UTC:
  // toISOString() shifted the date for non-UTC users and, worse, GET /api/daily
  // CREATES today's note as a side effect, so a read-only panel mutated vaults)
  const localISO = (d = new Date()) =>
    `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;

  async function refresh() {
    if (!body) return;
    let words = 0;
    try {
      // read-only: only fetch the note if today's daily already exists
      const dates = await grimoire.api("/daily/dates");
      if (dates.includes(localISO())) {
        // safe because it exists: /daily only creates when the note is absent,
        // and this also honors a custom GRIMOIRE_DAILY_DIR
        const daily = await grimoire.api(`/daily?date=${localISO()}`);
        words = (daily.body?.trim().match(/\S+/g) || []).length;
      }
    } catch { /* no daily note yet → 0 words */ }
    const pct = Math.min(100, Math.round((words / goal()) * 100));
    body.innerHTML = `
      <div class="wg-row">
        <span>${words} / <a class="wg-goal" title="change goal">${goal()}</a> words</span>
        <span>${pct >= 100 ? "✅" : `${pct}%`}</span>
      </div>
      <div class="wg-bar" style="background:var(--paper2);border-radius:6px;height:8px;overflow:hidden">
        <div style="width:${pct}%;height:100%;background:var(--accent);transition:width .3s"></div>
      </div>`;
    body.querySelector(".wg-goal").onclick = () => {
      const g = prompt("Daily word goal:", String(goal()));
      if (g && parseInt(g, 10) > 0) { localStorage.setItem(GOAL_KEY, String(parseInt(g, 10))); refresh(); }
    };
  }

  grimoire.registerPanel({
    id: "word-goal",
    title: "✍ Today's goal",
    render(el) { body = el; refresh(); },
  });
  grimoire.on("note-save", refresh);
}
