package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"html/template"
	"net/http"
	"strings"
)

// Owner is who is trusted to approve a consent request. Grimoire-mcp's OAuth
// server has exactly one owner concept — this is a single-tenant broker for
// one person's vault, not an accounts system — so Owner is a yes/no plus a
// display name, not a role.
type Owner struct {
	Verified bool
	Name     string
	Via      string // "tailscale" or "admin-token"
}

// identifyOwner asks the identity resolver (normally the Tailscale backend
// from internal/identity) who the peer is, and accepts the answer only if
// that login is on GRIMOIRE_OAUTH_ALLOWED_LOGINS. Being on the tailnet is not
// enough by itself — a tailnet can have other members — so the allowlist is
// what turns "Tailscale can name this caller" into "this caller may grant
// access to the vault."
func (h *Handler) identifyOwner(r *http.Request) (Owner, bool) {
	if h.Identity == nil || !h.Identity.Enabled() || len(h.AllowedLogins) == 0 {
		return Owner{}, false
	}
	id, ok := h.Identity.Identify(r)
	if !ok || !id.Verified {
		return Owner{}, false
	}
	login := id.User
	if login == "" {
		login = id.Subject
	}
	for _, allowed := range h.AllowedLogins {
		if strings.EqualFold(strings.TrimSpace(allowed), login) {
			return Owner{Verified: true, Name: login, Via: "tailscale"}, true
		}
	}
	return Owner{}, false
}

// checkAdminToken is the fallback path when Tailscale identity is unset,
// disabled, or answers with a login not on the allowlist: whoever can
// present the Grimoire admin token is the owner. Same constant-time,
// hash-first comparison internal/api's requireAdminToken uses.
func (h *Handler) checkAdminToken(presented string) bool {
	if h.AdminToken == "" || presented == "" {
		return false
	}
	want := sha256.Sum256([]byte(h.AdminToken))
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

var scopeInfo = []struct {
	Scope       string
	Label       string
	Description string
	DefaultOn   bool
}{
	{ScopeNotesRead, "Read notes", "Search, read and browse notes, documents and the knowledge graph.", true},
	{ScopeNotesWrite, "Write notes", "Create notes, edit them, and capture new ones.", true},
	{ScopeMemory, "Agent memory", "Read and write agent memory (remember / recall / facts).", true},
	{ScopeCredentials, "Credential broker", "Use, list and request credential grants. This can spend credentials you've separately authorized — leave it off unless this connector specifically needs it.", false},
}

// consentPage renders the approve/deny form. name/redirectHost/scopes are
// pre-escaped nowhere here — html/template escapes on render, so a
// malicious client_name from Dynamic Client Registration is inert on the
// page that has to display it.
var consentPage = template.Must(template.New("consent").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect to Grimoire</title>
<style>
body{font-family:system-ui,sans-serif;max-width:36rem;margin:2rem auto;padding:0 1rem;color:#1a1a1a;background:#fff}
.client{border:1px solid #ddd;border-radius:8px;padding:1rem;margin:1rem 0}
.warn{color:#8a5300;background:#fff6e5;border:1px solid #f0d38a;border-radius:6px;padding:.6rem 1rem;margin:1rem 0}
.err{color:#8a1f11;background:#fdecea;border:1px solid #f3b4ab;border-radius:6px;padding:.6rem 1rem;margin:1rem 0}
label.scope{display:block;margin:.4rem 0;padding:.4rem;border-radius:4px}
label.scope:hover{background:#f5f5f5}
.desc{color:#555;font-size:.9em;margin-left:1.6rem}
.owner{font-size:.9em;color:#555}
button{font-size:1rem;padding:.5rem 1.2rem;border-radius:6px;border:1px solid #ccc;cursor:pointer;margin-right:.5rem}
button.approve{background:#1a7f37;color:#fff;border-color:#1a7f37}
button.deny{background:#fff;color:#333}
input[type=text],input[type=password]{width:100%;padding:.4rem;box-sizing:border-box;margin:.3rem 0 .8rem}
</style></head><body>
<h2>Connect to your Grimoire</h2>
<p>An application is asking for access to your knowledge base and memory.</p>
<div class="client">
  <strong>{{.ClientName}}</strong><br>
  <span class="owner">will redirect to <code>{{.RedirectHost}}</code> after you decide.</span>
</div>
{{if .Owner.Verified}}
<p class="owner">Signed in as <strong>{{.Owner.Name}}</strong> (verified via {{.Owner.Via}}).</p>
{{else}}
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<p class="owner">Not recognized over Tailscale. Enter the Grimoire admin token to continue.</p>
{{end}}
<form method="post" action="/oauth/authorize">
<input type="hidden" name="req" value="{{.ReqToken}}">
{{if not .Owner.Verified}}
<label>Admin token
<input type="password" name="admin_token" autocomplete="off" required>
</label>
{{end}}
<p><strong>This connector is asking for:</strong></p>
{{range .Scopes}}
<label class="scope"><input type="checkbox" name="scope" value="{{.Scope}}" {{if .Checked}}checked{{end}}> {{.Label}}</label>
<div class="desc">{{.Description}}</div>
{{end}}
<p>
<button class="approve" type="submit" name="decision" value="approve">Approve</button>
<button class="deny" type="submit" name="decision" value="deny">Deny</button>
</p>
</form>
</body></html>`))

// errorPage is used when the request cannot be trusted enough to redirect
// the browser anywhere — an unknown client_id or a redirect_uri that is not
// exactly what the client registered. Redirecting in either case would be
// handing a browser to a URI nobody vouched for, which is exactly the open
// redirect OAuth 2.1 requires refusing.
var errorPage = template.Must(template.New("error").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Cannot connect</title>
<style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1rem;color:#1a1a1a}
.err{color:#8a1f11;background:#fdecea;border:1px solid #f3b4ab;border-radius:6px;padding:1rem}</style>
</head><body><h2>Cannot connect</h2><div class="err">{{.}}</div></body></html>`))

type scopeView struct {
	Scope       string
	Label       string
	Description string
	Checked     bool
}

func scopeViews(requested []string) []scopeView {
	req := map[string]bool{}
	for _, s := range requested {
		req[s] = true
	}
	hasRequest := len(requested) > 0
	out := make([]scopeView, 0, len(scopeInfo))
	for _, si := range scopeInfo {
		checked := si.DefaultOn
		if hasRequest {
			// A client that named specific scopes gets exactly those
			// pre-checked — never more than it asked for — except
			// credentials, which stays off regardless of what was
			// requested until the owner deliberately ticks it.
			checked = req[si.Scope] && si.Scope != ScopeCredentials
		}
		out = append(out, scopeView{Scope: si.Scope, Label: si.Label, Description: si.Description, Checked: checked})
	}
	return out
}
