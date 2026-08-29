/**
 * What the AI in this knowledge base has cost, and what agents have done to it.
 *
 * The scope caption is not decoration and is not optional. Grimoire is mounted
 * BY agents; it never sees the conversation an agent has with its own provider,
 * so this is NOT anybody's total AI spend. It is the calls Grimoire made
 * itself — answering, reranking, classifying — on a key the operator
 * configured. A screen that showed a dollar figure without saying that would be
 * reporting something untrue, so the caption ships with the number.
 *
 * The same rule covers unpriced models: a call whose price is unknown is shown
 * as unknown, never as $0.00. Zero presented as a total makes an unmetered
 * provider look free, which is the expensive direction to be wrong in.
 */
import { $, api, esc } from "/util.js";

const WINDOWS = [
  ["24h", "24 hours"],
  ["7d", "7 days"],
  ["30d", "30 days"],
  ["90d", "90 days"],
  ["all", "All time"],
];

let currentWindow = "30d";

export async function openUsage() {
  $("#inspect-title").textContent = "📊 AI usage";
  $("#inspect-modal").classList.remove("hidden");
  await render();
}

const money = (n) => "$" + (n < 0.01 && n > 0 ? n.toFixed(4) : n.toFixed(2));
const num = (n) => (n ?? 0).toLocaleString();

/** Tokens are long numbers nobody reads; k/M is what a person compares. */
function tokens(n) {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n ?? 0);
}

function rollupTable(title, rows, unit = "") {
  if (!rows || !rows.length) return "";
  const max = Math.max(...rows.map((r) => r.cost || 0), 0.0001);
  return `
    <div class="pr-clabel">${esc(title)}</div>
    <table class="usage-table">
      <thead><tr><th>${esc(unit || title)}</th><th>calls</th><th>tokens in/out</th><th>cost</th></tr></thead>
      <tbody>${rows.map((r) => `
        <tr>
          <td>${esc(r.key)}</td>
          <td class="num">${num(r.calls)}</td>
          <td class="num">${tokens(r.input_tokens)} / ${tokens(r.output_tokens)}</td>
          <td class="num">
            ${r.unpriced_calls === r.calls
              ? '<span class="usage-unknown" title="No price on file for this model">not priced</span>'
              : money(r.cost) + (r.unpriced_calls
                  ? ` <span class="usage-unknown" title="${r.unpriced_calls} call(s) have no price on file">+${r.unpriced_calls}?</span>`
                  : "")}
            <span class="usage-bar" style="width:${Math.round((r.cost / max) * 60)}px"></span>
          </td>
        </tr>`).join("")}
      </tbody>
    </table>`;
}

async function render() {
  const body = $("#inspect-body");
  body.innerHTML = '<p class="vault-note">Loading…</p>';
  let usage, agents;
  try {
    [usage, agents] = await Promise.all([
      api(`/usage?since=${currentWindow}`),
      api(`/usage/agents?since=${currentWindow}`).catch(() => ({ agents: [] })),
    ]);
  } catch (e) {
    body.innerHTML = `<p class="vault-note">${esc(e.message)}</p>`;
    return;
  }
  const s = usage.summary || {};

  const picker = WINDOWS.map(([k, label]) =>
    `<button class="usage-win${k === currentWindow ? " on" : ""}" data-win="${k}">${esc(label)}</button>`
  ).join("");

  // "At least" rather than a flat total whenever some call could not be priced.
  const totalLabel = s.unpriced_calls
    ? `at least ${money(s.cost || 0)}`
    : money(s.cost || 0);

  body.innerHTML = `
    <div class="usage-wins">${picker}</div>

    <p class="vault-note"><b>What this is.</b> ${esc(usage.scope || "")}</p>

    <div class="usage-cards">
      <div class="usage-card"><b>${totalLabel}</b><span>cost</span></div>
      <div class="usage-card"><b>${num(s.calls)}</b><span>model calls</span></div>
      <div class="usage-card"><b>${tokens(s.input_tokens)}</b><span>tokens in</span></div>
      <div class="usage-card"><b>${tokens(s.output_tokens)}</b><span>tokens out</span></div>
      ${s.errors ? `<div class="usage-card err"><b>${num(s.errors)}</b><span>failed</span></div>` : ""}
    </div>

    ${s.unpriced_calls ? `<p class="vault-note">${num(s.unpriced_calls)} call(s) used a
      model with no price on file, so the real total is higher than the figure above.
      Routed providers price per model they choose; guessing would be fiction.</p>` : ""}

    ${!s.calls ? `<p class="vault-note">No model calls recorded in this window.
      Grimoire only calls a model when you ask it to — <code>ask_notes</code>,
      reranking, intent classification. Searching and reading notes cost nothing.</p>` : ""}

    ${rollupTable("By provider", s.by_provider, "provider")}
    ${rollupTable("By surface", s.by_surface, "what asked")}
    ${rollupTable("By model", s.by_model, "model")}

    <div class="pr-clabel">Agents in this knowledge base</div>
    ${(agents.agents || []).length ? `
      <table class="usage-table">
        <thead><tr><th>agent</th><th>facts</th><th>disputed</th><th>model calls</th><th>cost</th><th>last seen</th></tr></thead>
        <tbody>${agents.agents.map((a) => `
          <tr>
            <td>${esc(a.agent)}</td>
            <td class="num">${num(a.facts)}</td>
            <td class="num">${a.challenges ? `<span class="usage-unknown">${num(a.challenges)}</span>` : "—"}</td>
            <td class="num">${num(a.model_calls)}</td>
            <td class="num">${a.model_cost ? money(a.model_cost) : "—"}</td>
            <td>${esc(a.last_seen || "—")}</td>
          </tr>`).join("")}
        </tbody>
      </table>
      <p class="vault-note">${esc(agents.scope || "")}
        ${agents.credential_uses ? `<b>${num(agents.credential_uses)}</b> credential use(s) brokered.` : ""}</p>`
      : '<p class="vault-note">No agent has written to this vault yet.</p>'}

    ${(usage.recent || []).length ? `
      <div class="pr-clabel">Recent calls</div>
      <table class="usage-table">
        <thead><tr><th>when</th><th>surface</th><th>model</th><th>tokens</th><th>ms</th><th>cost</th></tr></thead>
        <tbody>${usage.recent.slice(0, 25).map((c) => `
          <tr${c.error ? ' class="err"' : ""}>
            <td>${esc((c.at || "").replace("T", " ").slice(0, 16))}</td>
            <td>${esc(c.surface || "—")}</td>
            <td>${esc(c.model || "—")}</td>
            <td class="num">${tokens(c.input_tokens)}/${tokens(c.output_tokens)}</td>
            <td class="num">${num(c.latency_ms)}</td>
            <td class="num">${c.cost_known ? money(c.cost) : '<span class="usage-unknown">?</span>'}</td>
          </tr>
          ${c.error ? `<tr class="err"><td colspan="6">${esc(c.error)}</td></tr>` : ""}`).join("")}
        </tbody>
      </table>` : ""}

    <p class="vault-note">Prices last checked ${esc(usage.prices_updated || "—")}.
      They are a reference table, not a bill: providers change rates and negotiate
      them. Reconcile against your provider's own invoice.</p>`;

  body.querySelectorAll(".usage-win").forEach((b) =>
    b.addEventListener("click", () => {
      currentWindow = b.dataset.win;
      render();
    })
  );
}
