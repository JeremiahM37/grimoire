package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Streamable-HTTP transport for the MCP server.
//
// README has documented `GRIMOIRE_MCP_TRANSPORT=http ./grimoire-mcp` serving at
// 127.0.0.1:9112/mcp since before the Go rewrite; the rewrite shipped stdio
// only, so the variable did nothing and the documented address answered
// nothing. The dispatch logic is shared with the stdio loop — this is a second
// transport over the same handle(), never a second implementation.
//
// It binds loopback by default for the reason the README already gives: there
// is no authentication here, so exposing it is the reverse proxy's job.

// MaxRequestBytes bounds a single JSON-RPC frame. Notes can be large, so this
// matches the stdio scanner's ceiling rather than a typical API default.
const MaxRequestBytes = 16 << 20

// HTTPHandler serves JSON-RPC over POST.
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if len(raw) > MaxRequestBytes {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			// A malformed frame gets a JSON-RPC parse error rather than a bare
			// 400, because the client is speaking JSON-RPC and can act on it.
			writeRPC(w, http.StatusBadRequest, &response{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "parse error"},
			})
			return
		}
		resp := s.handle(req)
		if resp == nil {
			// A notification has no id and takes no reply.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, http.StatusOK, resp)
	})
	return mux
}

func writeRPC(w http.ResponseWriter, status int, resp *response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// ListenAndServe runs the HTTP transport until the process is stopped.
// Deliberately not named ServeHTTP: this type is not an http.Handler, and a
// method with that name and a different signature reads like a broken one.
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.HTTPHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
