/**
 * Journal heatmap — a 12-week activity grid of your daily notes, GitHub-style.
 * Each cell is a day; filled cells are days with a daily note. Click a filled
 * cell to open that day. Refreshes after every save.
 */
const WEEKS = 12;
const DAY_MS = 24 * 60 * 60 * 1000;

/* Local calendar date — matches the server's daily-note naming. Never use
   toISOString() here: it is UTC and shifts the day for non-UTC users. */
const localISO = (d) =>
  `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;

export function activate(grimoire) {
  let body = null;

  async function refresh() {
    if (!body) return;
    let dates;
    try { dates = new Set(await grimoire.api("/daily/dates")); }
    catch { body.textContent = "heatmap unavailable"; return; }

    // grid columns are weeks, rows are weekdays (Mon..Sun), ending today
    const today = new Date();
    const cells = [];
    const start = new Date(today.getTime() - (WEEKS * 7 - 1) * DAY_MS);
    // align the first column to a Monday
    while (start.getDay() !== 1) start.setTime(start.getTime() - DAY_MS);
    for (let d = new Date(start); d <= today; d.setTime(d.getTime() + DAY_MS)) {
      const iso = localISO(d);
      cells.push({ iso, has: dates.has(iso) });
    }
    const streak = currentStreak(dates, today);
    body.innerHTML = `
      <div class="jh-grid" role="img" aria-label="daily note activity">${cells.map((c) =>
        `<span class="jh-cell${c.has ? " on" : ""}" data-date="${c.iso}" title="${c.iso}"></span>`
      ).join("")}</div>
      <div class="jh-foot">${dates.size} days journaled${streak > 1 ? ` · ${streak}-day streak 🔥` : ""}</div>`;
    body.querySelectorAll(".jh-cell.on").forEach((el) => {
      el.onclick = () => grimoire.openNote(`journal/${el.dataset.date}.md`);
    });
  }

  grimoire.registerPanel({
    id: "journal-heatmap",
    title: "📆 Journal",
    render(el) { body = el; refresh(); },
  });
  grimoire.on("note-save", refresh);
}

function currentStreak(dates, today) {
  let streak = 0;
  for (let t = today.getTime(); ; t -= DAY_MS) {
    const iso = localISO(new Date(t));
    if (dates.has(iso)) streak += 1;
    else if (streak > 0 || iso !== localISO(today)) break;
    // (a missing *today* doesn't break a streak that ended yesterday)
  }
  return streak;
}
