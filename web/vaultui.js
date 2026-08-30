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
/* How long a grant has left, from the absolute expiry the API returns.
   This read g.expires_in and g.expired, which the API has never sent — so
   every grant in the credential console rendered "undefineds" and an expired
   one looked identical to a live one. The console's whole job is showing which
   agent holds what and for how long, so this was the one field that had to be
   right. */
function grantRemaining(g) {
  const left = Math.round((g.expires_at || 0) - Date.now() / 1000);
  if (left <= 0) return "expired";
  if (left < 60) return left + "s left";
  if (left < 3600) return Math.round(left / 60) + "m left";
  if (left < 86400) return Math.round(left / 3600) + "h left";
  return Math.round(left / 86400) + "d left";
}


/* One secret's row: the name, and the facts that decide whether to rotate it.
   Deliberately no value and no control that could reveal one. */
function secretRow(s, d) {
  const info = d || {};
  const badge = {
    expired: '<span class="v-bad">expired</span>',
    expiring: '<span class="v-warn">expires soon</span>',
    stale: '<span class="v-warn">rotation due</span>',
  }[info.status] || "";
  const bits = [];
  if (info.expires) bits.push("expires " + esc(info.expires));
  bits.push(info.uses ? `${info.uses} use${info.uses === 1 ? "" : "s"}` : "never used");
  if (info.versions) bits.push(`${info.versions} prior`);
  return `<div class="v-row"><span>🔑 ${esc(s.name)} ${badge}
      <br><span class="pm">${bits.join(" · ")}${info.note ? " · " + esc(info.note) : ""}</span></span>
    <span>${info.versions ? `<button class="icon v-hist" data-n="${esc(s.name)}"
        title="previous values">🕘</button>` : ""}
      <button class="icon danger v-del" data-n="${esc(s.name)}">🗑</button></span>
    </div><div class="v-hist-box" id="hist-${cssId(s.name)}"></div>`;
}

/* A secret name may contain characters that are not valid in an id. */
function cssId(name) {
  return name.replace(/[^A-Za-z0-9_-]/g, "_");
}

/* Findings are shown masked, because a panel that printed the key it found
   would copy the leak onto another screen. */
function showScan(out) {
  const b = $("#vault-body");
  const box = document.createElement("div");
  box.innerHTML = `<div class="pr-clabel">Credentials found in notes</div>
    ${out.findings.length ? out.findings.map((f) => `<div class="v-arow">
        <span class="${f.confidence === "high" ? "v-bad" : "v-warn"}">${esc(f.confidence)}</span>
        <span><b>${esc(f.kind)}</b> — ${esc(f.path)}:${f.line}</span>
        <code>${esc(f.masked)}</code></div>`).join("")
      : `<div class="vault-note">Scanned ${out.scanned} notes; nothing that looks
         like a credential is sitting in them.</div>`}
    ${out.findings.length ? `<p class="vault-note">Rotate anything real at the
      issuer first — a key that has been in a synced note should be treated as
      disclosed — then store it here and edit the note.</p>` : ""}`;
  b.appendChild(box);
  box.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

/* What a grant has left in COUNT, alongside what it has left in time.
   A TTL bounds nothing about volume: fifteen minutes is fifteen minutes in
   which an agent may make any number of calls. */
function grantUses(g) {
  if (!g.max_uses) return g.uses ? ` · ${g.uses} use${g.uses === 1 ? "" : "s"}` : "";
  const left = g.max_uses - (g.uses || 0);
  return ` · ${left} of ${g.max_uses} use${g.max_uses === 1 ? "" : "s"} left`;
}

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
    // The admin surface can be gated separately from reading (GRIMOIRE_ADMIN_TOKEN),
    // which is the recommended shape for an instance reachable over a tailnet.
    // /vault/status answers for anyone, so we get this far and then /secrets
    // 401s — and an unhandled rejection here left the modal OPEN and EMPTY,
    // which reads as a broken app rather than as a closed door. Say which it is.
    let secrets;
    try {
      secrets = await api("/secrets");
    } catch (e) {
      b.innerHTML = (e.gate === "admin" || e.status === 401 || e.status === 403)
        ? `<p class="vault-note">The credential vault is part of this instance's
             <b>administrative surface</b>, which needs its own token
             (<code>GRIMOIRE_ADMIN_TOKEN</code>). Notes and retrieval stay open;
             the levers do not. Open the console from a client that carries the
             token to manage secrets and grants.</p>`
        : `<p class="vault-note">${esc(e.message)}</p>`;
      return;
    }
    const grants = await api("/grants").catch(() => []);
    const audit = await api("/audit").catch(() => []);
    // Everything about the secrets except their values: what expires, what
    // nothing has used, what has a value worth rolling back to. These are the
    // facts a rotation decision needs, and none of them require seeing a key.
    const detail = await api("/secrets/details").catch(() => ({ secrets: [] }));
    const byName = {};
    for (const d of detail.secrets || []) byName[d.name] = d;
    const aicon = { broker: "📤", grant: "🎟", revoke: "✕", revoke_all: "✕",
                    set: "➕", delete: "🗑", change_passphrase: "🔑" };
    b.innerHTML = `<div class="vault-actions"><span class="vault-note">${secrets.length} secret(s) — values are never shown. Your AI can use them via scoped grants.${
        detail.needs_attention ? ` <b class="v-attn">${detail.needs_attention} need attention.</b>` : ""}</span>
      <button id="v-scan" class="icon" title="find credentials pasted into notes">🔎 Scan notes</button>
      <button id="v-lock" class="icon" title="lock">🔒 Lock</button></div>
      <div id="v-list">${secrets.map((s) => secretRow(s, byName[s.name])).join("")
        || '<div class="vault-note">No secrets yet.</div>'}</div>
      <div class="ask-input-row"><input id="v-name" placeholder="name (e.g. github)">
      <input id="v-val" type="password" placeholder="value / token"><button id="v-add" class="btn">Add</button></div>
      ${grants.length ? `<div class="pr-clabel">Active grants</div>
        <div id="v-grants">${grants.map((g) => `<div class="v-row"><span>🎟 ${esc(g.grantee)} → ${esc(g.secret)}
          <span class="pm">${grantRemaining(g)}${grantUses(g)}</span></span>
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
    b.querySelectorAll(".v-hist").forEach((x) => (x.onclick = async () => {
      const name = x.dataset.n;
      const out = await api(`/secrets/versions?name=${encodeURIComponent(name)}`)
        .catch(() => ({ versions: [] }));
      const box = b.querySelector(`#hist-${cssId(name)}`);
      if (!out.versions.length) {
        box.innerHTML = '<div class="vault-note">No previous values.</div>';
        return;
      }
      box.innerHTML = out.versions.map((v, i) => `<div class="v-arow">
        <span class="pm">${esc((v.at || "").replace("T", " ").replace("Z", ""))}</span>
        <span>${esc(v.note || "")}</span>
        <button class="btn v-restore" data-n="${esc(name)}" data-i="${i}">Restore</button>
      </div>`).join("");
      box.querySelectorAll(".v-restore").forEach((r) => (r.onclick = async () => {
        // The value being replaced is retained too, so this is undoable — say
        // so, because "restore" otherwise reads as destructive.
        if (!confirm(`Make that value current again for "${r.dataset.n}"?\n\n`
          + "The value it replaces is kept, so this can be undone.")) return;
        try {
          await api("/secrets/restore", { method: "POST",
            body: { name: r.dataset.n, version: Number(r.dataset.i) } });
          toast("Restored"); openVault();
        } catch (e) { toast(e.message, true); }
      }));
    }));
    $("#v-scan").onclick = async () => {
      const btn = $("#v-scan");
      btn.disabled = true; btn.textContent = "scanning…";
      try { showScan(await api("/secrets/scan")); }
      catch (e) { toast(e.message, true); }
      btn.disabled = false; btn.textContent = "🔎 Scan notes";
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
