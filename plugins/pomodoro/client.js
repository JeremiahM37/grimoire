/**
 * Pomodoro timer — a sidebar panel with a 25-minute focus timer. When a
 * session completes, a log line is appended to today's daily note through the
 * capture API, so your focus history lives in your notes like everything else.
 */
const WORK_MINUTES = 25;

export function activate(grimoire) {
  let remaining = WORK_MINUTES * 60;
  let timer = null;
  let display = null;

  const fmt = (s) => `${String(Math.floor(s / 60)).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;

  const tick = async () => {
    remaining -= 1;
    if (display) display.textContent = fmt(remaining);
    if (remaining > 0) return;
    stop();
    grimoire.toast("🍅 Pomodoro complete — logged to today's note");
    try {
      await grimoire.api("/capture", {
        method: "POST", body: { text: `🍅 ${WORK_MINUTES}m focus session`, source: "pomodoro" },
      });
    } catch { /* daily append is best-effort */ }
    remaining = WORK_MINUTES * 60;
    if (display) display.textContent = fmt(remaining);
  };

  function start(button) {
    if (timer) return;
    timer = setInterval(tick, 1000);
    button.textContent = "⏸ pause";
  }
  function stop(button) {
    clearInterval(timer); timer = null;
    if (button) button.textContent = "▶ start";
  }

  grimoire.registerPanel({
    id: "pomodoro",
    title: "🍅 Pomodoro",
    render(el) {
      el.innerHTML = `
        <div style="display:flex;align-items:center;gap:10px">
          <b class="pomo-time" style="font:600 20px var(--mono)">${fmt(remaining)}</b>
          <button class="btn pomo-toggle">▶ start</button>
          <button class="btn pomo-reset" title="reset">↺</button>
        </div>`;
      display = el.querySelector(".pomo-time");
      const toggle = el.querySelector(".pomo-toggle");
      toggle.onclick = () => (timer ? stop(toggle) : start(toggle));
      el.querySelector(".pomo-reset").onclick = () => {
        stop(toggle); remaining = WORK_MINUTES * 60; display.textContent = fmt(remaining);
      };
    },
  });

  grimoire.registerCommand({
    icon: "🍅", name: "Start pomodoro (25 min)",
    run: () => document.querySelector(".pomo-toggle")?.click(),
  });
}
