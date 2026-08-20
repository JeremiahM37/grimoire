/**
 * Connector configuration, rendered from what the server says it supports.
 *
 * The form is built from /api/connectors/kinds — fields, help text, and where
 * to get each credential — so adding a source kind to the server adds it here
 * with no console change. That is the difference between "we support Slack"
 * and "you can configure a source".
 */
import { $, api, toast, esc } from "/util.js";

let kinds = [];

export async function openConnectors() {
  const modal = $("#inspect-modal");
  $("#inspect-title").textContent = "🔌 Connectors";
  modal.classList.remove("hidden");
  const body = $("#inspect-body");
  body.innerHTML = '<p class="vault-note">Loading…</p>';
  try {
    [kinds] = await Promise.all([api("/connectors/kinds")]);
    await render();
  } catch (e) {
    body.innerHTML = `<p class="vault-note">${esc(e.message)}</p>`;
  }
}

async function render() {
  const body = $("#inspect-body");
  const list = await api("/connectors");
  body.innerHTML = `
    <p class="vault-note">Connectors pull documents from other systems into
      your vault as ordinary notes — searchable, editable, and yours.</p>
    <div id="conn-list">${list.length ? list.map(row).join("") :
      '<p class="vault-note">Nothing configured yet.</p>'}</div>
    <div class="pr-clabel">Add a connector</div>
    <div class="ask-input-row">
      <select id="conn-kind">${kinds.map((k) =>
        `<option value="${esc(k.kind)}">${esc(k.name)}</option>`).join("")}</select>
      <button class="btn" id="conn-new">Configure…</button>
    </div>
    <div id="conn-form"></div>`;

  $("#conn-new").onclick = () => form($("#conn-kind").value);
  body.querySelectorAll(".conn-run").forEach((b) => (b.onclick = async () => {
    b.disabled = true;
    b.textContent = "syncing…";
    try {
      const out = await api(`/connectors/${b.dataset.id}/run`, { method: "POST" });
      toast(out.ok ? `pulled ${out.written} (skipped ${out.skipped})` : out.error, !out.ok);
    } catch (e) { toast(e.message, true); }
    render();
  }));
  body.querySelectorAll(".conn-del").forEach((b) => (b.onclick = async () => {
    if (!confirm("Remove this connector? The notes it pulled are kept.")) return;
    await api(`/connectors/${b.dataset.id}`, { method: "DELETE" });
    render();
  }));
}

function row(c) {
  const when = c.last_run ? c.last_run.replace("T", " ").replace("Z", "") : "never";
  const state = c.last_ok
    ? `<span class="pm">last run ${esc(when)} · ${c.docs} documents</span>`
    : `<span class="conn-err" title="${esc(c.last_error)}">failed: ${esc(
        (c.last_error || "").slice(0, 80))}</span>`;
  return `<div class="v-row conn-row">
    <span><b>${esc(c.name)}</b> <span class="pm">${esc(c.kind)} → ${esc(c.prefix)}/</span><br>${state}</span>
    <span>
      <button class="btn conn-run" data-id="${esc(c.id)}">Sync now</button>
      <button class="icon danger conn-del" data-id="${esc(c.id)}" title="remove">✕</button>
    </span></div>`;
}

function form(kindName) {
  const k = kinds.find((x) => x.kind === kindName);
  if (!k) return;
  const el = $("#conn-form");
  el.innerHTML = `
    <div class="conn-form">
      <p class="vault-note">${esc(k.help)}</p>
      ${k.secret_help ? `<p class="vault-note"><b>Credential:</b> ${esc(k.secret_help)}
        Add it in the 🔐 Vault first, then name it below.</p>` : ""}
      <label>Name<input id="cf-name" placeholder="${esc(k.name)}"></label>
      ${k.fields.map((f) => `<label>${esc(f.label)}${f.required ? " *" : ""}
        <input id="cf-${esc(f.name)}" placeholder="${esc(f.placeholder || "")}">
        ${f.help ? `<span class="pm">${esc(f.help)}</span>` : ""}</label>`).join("")}
      ${k.secret_help ? `<label>Vault credential name
        <input id="cf-secret" placeholder="e.g. slack-token"></label>` : ""}
      <label>Destination folder<input id="cf-prefix" placeholder="${esc(k.default_prefix)}"></label>
      <label>Sync every (minutes, 0 = manual)<input id="cf-interval" type="number" value="60"></label>
      <button class="btn" id="cf-save">Save connector</button>
    </div>`;
  $("#cf-save").onclick = async () => {
    const config = {};
    for (const f of k.fields) config[f.name] = $("#cf-" + f.name).value.trim();
    try {
      await api("/connectors", {
        method: "POST",
        body: {
          kind: k.kind,
          name: $("#cf-name").value.trim() || k.name,
          config,
          secret: $("#cf-secret")?.value.trim() || "",
          prefix: $("#cf-prefix").value.trim(),
          interval: Math.max(0, parseInt($("#cf-interval").value || "0", 10)) * 60,
        },
      });
      toast("connector saved");
      el.innerHTML = "";
      render();
    } catch (e) { toast(e.message, true); }
  };
}
