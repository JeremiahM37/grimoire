/**
 * Secret-vault UI — init/unlock/lock, secrets CRUD, scoped grants, audit log,
 * passphrase rotation. Pure modal logic over the /api/vault + /api/secrets
 * endpoints; the cryptography all lives server-side (see SECURITY.md). This
 * module never sees or stores a secret value beyond the field the user typed.
 */
import { $, api, toast, esc } from "/util.js";

/* ---------- secret vault ---------- */
$("#vault-open").onclick = openVault;
$("#vault-close").onclick = () => $("#vault-modal").classList.add("hidden");
$("#vault-modal").onclick = (e) => { if (e.target.id === "vault-modal") $("#vault-modal").classList.add("hidden"); };
export async function openVault() {
  $("#vault-modal").classList.remove("hidden");
  const st = await api("/vault/status");
  const b = $("#vault-body");
  if (!st.initialized) {
    b.innerHTML = `<p class="vault-note">Set a passphrase to create your encrypted secret vault. It's never stored — if you forget it, the secrets are gone.</p>
      <div class="ask-input-row"><input id="v-pass" type="password" placeholder="new passphrase (8+ chars)">
      <button id="v-init" class="btn">Create</button></div>`;
    $("#v-init").onclick = async () => {
      try { await api("/vault/init", { method: "POST", body: { passphrase: $("#v-pass").value } }); openVault(); }
      catch (e) { toast(e.message, true); }
    };
  } else if (!st.unlocked) {
    b.innerHTML = `<p class="vault-note">Vault is locked.</p>
      <div class="ask-input-row"><input id="v-pass" type="password" placeholder="passphrase">
      <button id="v-unlock" class="btn">Unlock</button></div>`;
    $("#v-pass").onkeydown = (e) => { if (e.key === "Enter") $("#v-unlock").click(); };
    $("#v-unlock").onclick = async () => {
      try { await api("/vault/unlock", { method: "POST", body: { passphrase: $("#v-pass").value } }); openVault(); }
      catch (e) { toast(e.message, true); }
    };
  } else {
    const secrets = await api("/secrets");
    const grants = await api("/grants").catch(() => []);
    const audit = await api("/audit").catch(() => []);
    const aicon = { broker: "📤", grant: "🎟", revoke: "✕", revoke_all: "✕",
                    set: "➕", delete: "🗑", change_passphrase: "🔑" };
    b.innerHTML = `<div class="vault-actions"><span class="vault-note">${secrets.length} secret(s) — values are never shown. Your AI can use them via scoped grants.</span>
      <button id="v-lock" class="icon" title="lock">🔒 Lock</button></div>
      <div id="v-list">${secrets.map((s) => `<div class="v-row"><span>🔑 ${esc(s.name)}</span>
        <button class="icon danger v-del" data-n="${esc(s.name)}">🗑</button></div>`).join("") || '<div class="vault-note">No secrets yet.</div>'}</div>
      <div class="ask-input-row"><input id="v-name" placeholder="name (e.g. github)">
      <input id="v-val" type="password" placeholder="value / token"><button id="v-add" class="btn">Add</button></div>
      ${grants.length ? `<div class="pr-clabel">Active grants</div>
        <div id="v-grants">${grants.map((g) => `<div class="v-row"><span>🎟 ${esc(g.grantee)} → ${esc(g.secret)}
          <span class="pm">${g.expired ? "expired" : g.expires_in + "s"}</span></span>
          <button class="icon danger v-revoke" data-t="${esc(g.token)}" title="revoke">✕</button></div>`).join("")}</div>` : ""}
      <details id="v-audit"><summary>Audit log · every secret use, granted, revoked (${audit.length})</summary>
        ${audit.length ? audit.map((a) => `<div class="v-arow"><span class="pm">${esc((a.ts || "").replace("T", " "))}</span>
          <span class="a-act">${aicon[a.action] || "•"} ${esc(a.action)}</span>${a.secret ? " <code>" + esc(a.secret) + "</code>" : ""}
          ${a.detail ? `<span class="pm">${esc(a.detail)}</span>` : ""}</div>`).join("")
          : '<div class="vault-note">Nothing yet — brokered calls, grants and revocations all land here.</div>'}</details>
      <div class="vault-actions" style="margin-top:10px"><button id="v-changepass" class="btn">Change passphrase…</button></div>`;
    $("#v-lock").onclick = async () => { await api("/vault/lock", { method: "POST" }); openVault(); toast("Vault locked"); };
    $("#v-add").onclick = async () => {
      const name = $("#v-name").value.trim(), value = $("#v-val").value;
      if (!name || !value) return toast("name and value required", true);
      try { await api("/secrets", { method: "POST", body: { name, value } }); openVault(); toast("Secret added"); }
      catch (e) { toast(e.message, true); }
    };
    b.querySelectorAll(".v-del").forEach((x) => (x.onclick = async () => {
      await api(`/secrets/${encodeURIComponent(x.dataset.n)}`, { method: "DELETE" }); openVault();
    }));
    b.querySelectorAll(".v-revoke").forEach((x) => (x.onclick = async () => {
      await api(`/grants/${encodeURIComponent(x.dataset.t)}`, { method: "DELETE" }); openVault(); toast("Grant revoked");
    }));
    $("#v-changepass").onclick = async () => {
      const oldp = prompt("Current passphrase:"); if (!oldp) return;
      const newp = prompt("New passphrase (8+ chars):"); if (!newp) return;
      try {
        const r = await api("/vault/change-passphrase", { method: "POST", body: { old: oldp, new: newp } });
        toast(`Passphrase changed (re-sealed ${r.reencrypted_notes} encrypted note(s))`);
      } catch (e) { toast(e.message, true); }
    };
  }
}
