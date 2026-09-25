package oauth

import (
	"html/template"
	"net/http"
)

// requireAdminAPI gates the JSON client-management endpoints the same way
// internal/api's requireAdminToken gates cmd/grimoire's admin surface:
// header first, query parameter as a fallback for the admin page's own
// fetch() calls in environments where setting a header is awkward.
func (h *Handler) requireAdminAPI(w http.ResponseWriter, r *http.Request) bool {
	token := r.Header.Get("X-Grimoire-Admin")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if h.checkAdminToken(token) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="grimoire-admin"`)
	writeOAuthErr(w, http.StatusUnauthorized, "unauthorized", "the Grimoire admin token is required")
	return false
}

// adminList answers the connected-clients table: every registered client,
// its redirect URIs, the scopes any live token actually carries, and when it
// was last used.
func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminAPI(w, r) {
		return
	}
	clients, err := h.Store.ListClients()
	if err != nil {
		writeOAuthErr(w, http.StatusInternalServerError, "server_error", "could not list clients")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

// adminRevoke is the Revoke button: every access and refresh token issued to
// one client, gone. The client registration itself is left alone — see
// Store.RevokeClient.
func (h *Handler) adminRevoke(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminAPI(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := h.Store.RevokeClient(id); err != nil {
		writeOAuthErr(w, http.StatusInternalServerError, "server_error", "could not revoke")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

func (h *Handler) adminPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = adminPageTmpl.Execute(w, nil)
}

// adminPageTmpl is a plain page with no build step and no framework, per
// CONTRIBUTING.md's JS style rule — a fetch() call and a table, styled
// inline. The admin token is kept in sessionStorage only (never a cookie,
// never in a URL that could end up in a log), and every request sends it as
// a header, not a query string.
var adminPageTmpl = template.Must(template.New("admin").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Grimoire — connected clients</title>
<style>
body{font-family:system-ui,sans-serif;max-width:56rem;margin:2rem auto;padding:0 1rem;color:#1a1a1a}
table{width:100%;border-collapse:collapse;margin-top:1rem}
th,td{text-align:left;padding:.5rem;border-bottom:1px solid #e0e0e0;vertical-align:top;font-size:.9em}
th{color:#555;font-weight:600}
button{padding:.3rem .7rem;border-radius:5px;border:1px solid #c33;background:#fff;color:#c33;cursor:pointer}
button:hover{background:#fdecea}
.scope{display:inline-block;background:#eef2ff;color:#334;border-radius:4px;padding:.1rem .4rem;margin:.1rem .2rem 0 0;font-size:.8em}
#gate{max-width:24rem}
input[type=password]{width:100%;padding:.4rem;box-sizing:border-box;margin:.4rem 0}
</style></head><body>
<h2>Connected MCP clients</h2>
<div id="gate">
<label>Grimoire admin token<input type="password" id="token" autocomplete="off"></label>
<button id="load" style="color:#1a7f37;border-color:#1a7f37">Load</button>
</div>
<table id="table" style="display:none">
<thead><tr><th>Client</th><th>Redirect host(s)</th><th>Scopes</th><th>Last used</th><th></th></tr></thead>
<tbody id="rows"></tbody>
</table>
<p id="empty" style="display:none;color:#555">No connected clients yet.</p>
<p id="err" style="display:none;color:#8a1f11"></p>
<script>
function tok(){ return sessionStorage.getItem('grimoire_admin_token') || ''; }
function esc(s){ const d=document.createElement('div'); d.textContent=s==null?'':String(s); return d.innerHTML; }
async function load(){
  const t = document.getElementById('token').value.trim();
  if (t) sessionStorage.setItem('grimoire_admin_token', t);
  const err = document.getElementById('err');
  err.style.display = 'none';
  try {
    const res = await fetch('/admin/oauth/clients', { headers: { 'X-Grimoire-Admin': tok() } });
    if (!res.ok) { err.textContent = 'Could not load clients (' + res.status + ').'; err.style.display='block'; return; }
    const data = await res.json();
    render(data.clients || []);
  } catch (e) { err.textContent = 'Request failed.'; err.style.display = 'block'; }
}
function render(clients){
  const rows = document.getElementById('rows');
  rows.innerHTML = '';
  document.getElementById('table').style.display = clients.length ? '' : 'none';
  document.getElementById('empty').style.display = clients.length ? 'none' : '';
  for (const c of clients) {
    const tr = document.createElement('tr');
    const scopes = (c.Scopes || []).map(s => '<span class="scope">' + esc(s) + '</span>').join('');
    const hosts = (c.RedirectURIs || []).map(u => { try { return new URL(u).host; } catch(e) { return u; } }).join(', ');
    const lastUsed = c.LastUsed && c.LastUsed !== '0001-01-01T00:00:00Z' ? esc(c.LastUsed) : 'never';
    tr.innerHTML = '<td>' + esc(c.Name) + '<br><small style="color:#888">' + esc(c.ID) + '</small></td>' +
      '<td>' + esc(hosts) + '</td><td>' + (scopes || '<em style="color:#888">none active</em>') + '</td>' +
      '<td>' + lastUsed + '</td><td></td>';
    const td = tr.lastElementChild;
    const btn = document.createElement('button');
    btn.textContent = 'Revoke';
    btn.onclick = async () => {
      if (!confirm('Revoke all tokens for ' + c.Name + '?')) return;
      await fetch('/admin/oauth/clients/' + encodeURIComponent(c.ID) + '/revoke', {
        method: 'POST', headers: { 'X-Grimoire-Admin': tok() } });
      load();
    };
    td.appendChild(btn);
    rows.appendChild(tr);
  }
}
document.getElementById('load').addEventListener('click', load);
if (tok()) { document.getElementById('token').value = tok(); load(); }
</script>
</body></html>`))
