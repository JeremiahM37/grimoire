/**
 * Accounts, spaces and API keys, in the console.
 *
 * These existed only as an API and a CLI, which meant "multi-user" was a
 * feature only someone with a shell could operate — and the person who
 * administers a team's notes is not necessarily that person. The screen is
 * deliberately plain: it does one thing per row, and every destructive action
 * says what it will do before doing it.
 */
import { $, api, toast, esc } from "/util.js";

export async function openAdmin() {
  $("#inspect-title").textContent = "👥 People & spaces";
  $("#inspect-modal").classList.remove("hidden");
  await render();
}

async function render() {
  const body = $("#inspect-body");
  body.innerHTML = '<p class="vault-note">Loading…</p>';
  let me, users, spaces, keys;
  try {
    me = await api("/me");
    [users, spaces, keys] = await Promise.all([
      me.multi_user ? api("/users") : Promise.resolve([]),
      api("/spaces"),
      api("/keys").catch(() => []),
    ]);
  } catch (e) {
    body.innerHTML = `<p class="vault-note">${esc(e.message)}</p>`;
    return;
  }

  body.innerHTML = `
    ${me.multi_user ? "" : `
      <p class="vault-note"><b>This instance is single-user.</b> Nothing is gated
      and every request is treated as the owner's. Creating the first account
      turns on sign-in — after which every other account is created here.</p>`}
    <div class="pr-clabel">Accounts</div>
    <div id="ad-users">${users.length
      ? users.map(userRow).join("")
      : '<p class="vault-note">No accounts yet.</p>'}</div>
    <div class="conn-form">
      <label>New account
        <input id="ad-name" placeholder="name (letters, numbers, dot, dash)"></label>
      <label>Password (10 characters or more)
        <input id="ad-pass" type="password" placeholder="a long passphrase"></label>
      <label><input type="checkbox" id="ad-admin"> administrator</label>
      <button class="btn" id="ad-add">${users.length ? "Add account" : "Create the first account"}</button>
    </div>

    <div class="pr-clabel">Spaces</div>
    <p class="vault-note">A space is a folder of the vault plus the people who
      may see it. Everything outside one is the commons, which everyone shares.</p>
    <div id="ad-spaces">${spaces.map(spaceRow).join("") || ""}</div>
    ${me.admin && me.multi_user ? `
      <div class="conn-form">
        <label>New space name<input id="ad-sname" placeholder="Engineering"></label>
        <label>Folder<input id="ad-sprefix" placeholder="team/eng"></label>
        <button class="btn" id="ad-sadd">Create space</button>
      </div>` : ""}

    <div class="pr-clabel">API keys ${me.multi_user ? "" : "(for agents)"}</div>
    <p class="vault-note">An agent authenticates with a key. It is shown once —
      it is stored hashed, so it cannot be shown again.</p>
    <div id="ad-keys">${keys.length
      ? keys.map(keyRow).join("")
      : '<p class="vault-note">No keys yet.</p>'}</div>
    <div class="ask-input-row">
      <input id="ad-klabel" placeholder="what will use this key? e.g. claude-code">
      <button class="btn" id="ad-kadd">Create key</button>
    </div>`;

  $("#ad-add").onclick = async () => {
    try {
      await api("/users", { method: "POST", body: {
        name: $("#ad-name").value.trim(),
        display: $("#ad-name").value.trim(),
        password: $("#ad-pass").value,
        role: $("#ad-admin").checked ? "admin" : "member",
      }});
      toast("account created");
      render();
    } catch (e) { toast(e.message, true); }
  };
  const sadd = $("#ad-sadd");
  if (sadd) sadd.onclick = async () => {
    try {
      await api("/spaces", { method: "POST", body: {
        name: $("#ad-sname").value.trim(), prefix: $("#ad-sprefix").value.trim() }});
      toast("space created");
      render();
    } catch (e) { toast(e.message, true); }
  };
  $("#ad-kadd").onclick = async () => {
    try {
      const out = await api("/keys", { method: "POST",
        body: { label: $("#ad-klabel").value.trim() } });
      // Shown once, in a form that can be copied before it is gone.
      $("#ad-keys").insertAdjacentHTML("afterbegin",
        `<div class="v-row"><span class="ad-newkey">${esc(out.key)}</span>
         <span class="pm">copy this now — it cannot be shown again</span></div>`);
      toast("key created");
    } catch (e) { toast(e.message, true); }
  };
  body.querySelectorAll(".ad-deluser").forEach((b) => (b.onclick = async () => {
    if (!confirm(`Remove ${b.dataset.name}? Their notes stay in the vault.`)) return;
    try { await api(`/users/${b.dataset.id}`, { method: "DELETE" }); render(); }
    catch (e) { toast(e.message, true); }
  }));
  body.querySelectorAll(".ad-delkey").forEach((b) => (b.onclick = async () => {
    await api(`/keys/${b.dataset.id}`, { method: "DELETE" });
    render();
  }));
  body.querySelectorAll(".ad-members").forEach((b) => (b.onclick = () => members(b.dataset.id, b.dataset.name)));
}

function userRow(u) {
  return `<div class="v-row"><span>${u.role === "admin" ? "🛡" : "👤"}
    <b>${esc(u.name)}</b> <span class="pm">${esc(u.role)}</span></span>
    <button class="icon danger ad-deluser" data-id="${esc(u.id)}"
      data-name="${esc(u.name)}" title="remove">✕</button></div>`;
}

function spaceRow(s) {
  return `<div class="v-row"><span><b>${esc(s.name)}</b>
    <span class="pm">${esc(s.prefix || "(everything else)")} · ${esc(s.kind)}</span></span>
    ${s.kind === "shared"
      ? `<button class="btn ad-members" data-id="${esc(s.id)}" data-name="${esc(s.name)}">Members</button>`
      : ""}</div>`;
}

function keyRow(k) {
  return `<div class="v-row"><span><b>${esc(k.label)}</b>
    <span class="pm">created ${esc((k.created || "").slice(0, 10))}${
      k.last_used ? " · last used " + esc(k.last_used.slice(0, 10)) : " · never used"}</span></span>
    <button class="icon danger ad-delkey" data-id="${esc(k.id)}" title="revoke">✕</button></div>`;
}

async function members(spaceId, name) {
  const list = await api(`/spaces/${spaceId}/members`).catch(() => []);
  const who = prompt(
    `${name} — members: ${list.map((m) => m.name + " (" + m.role + ")").join(", ") || "none"}\n\n` +
    `Add an account by name (prefix with "read:" for read-only, "-" to remove):`);
  if (!who) return;
  try {
    if (who.startsWith("-")) {
      const target = list.find((m) => m.name === who.slice(1).trim());
      if (target) await api(`/spaces/${spaceId}/members/${target.user}`, { method: "DELETE" });
    } else {
      const readOnly = who.startsWith("read:");
      await api(`/spaces/${spaceId}/members`, { method: "POST", body: {
        user: readOnly ? who.slice(5).trim() : who.trim(),
        role: readOnly ? "reader" : "writer" }});
    }
    toast("membership updated");
  } catch (e) { toast(e.message, true); }
}
