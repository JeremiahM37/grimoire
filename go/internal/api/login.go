package api

import "net/http"

// The token is submitted in a body, never reflected into HTML or a URL.
func tokenLoginPage(w http.ResponseWriter, invalid bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusUnauthorized)
	message := "Enter the access token configured by your Grimoire server's owner."
	if invalid {
		message = "That access token was not accepted. Try again."
	}
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in · Grimoire</title><style>
*{box-sizing:border-box}body{margin:0;min-height:100dvh;display:grid;place-items:center;background:#10131b;color:#eef0f5;font:16px system-ui;padding:20px}main{width:min(100%,420px);padding:24px;background:#1b2130;border:1px solid #394257;border-radius:16px}h1{font-size:24px}p{line-height:1.5;color:#b9c1d3}label{display:grid;gap:8px}input,button{font:inherit;min-width:0;width:100%;padding:12px;border-radius:8px;border:1px solid #68748e}button{margin-top:16px;background:#d5c5ff;color:#191123;cursor:pointer}</style><main><h1>Sign in to Grimoire</h1><p role="status">` + message + `</p><form method="post" action="/auth/token"><label>Access token<input name="token" type="password" autocomplete="current-password" required autofocus></label><button type="submit">Connect</button></form></main></html>`))
}

func setAuthCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: authCookie, Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
}
