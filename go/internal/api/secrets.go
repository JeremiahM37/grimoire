package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// The vault, grant and broker surface. Port of server/routers/secrets.py.
//
// Every handler here answers 423 (Locked) rather than 401/403 when the vault is
// locked: the console distinguishes "you need to unlock" from "you may not do
// this", and so does the MCP tool contract.

func (s *Server) vaultLocked(w http.ResponseWriter, err error) bool {
	if errors.Is(err, secrets.ErrLocked) {
		writeErr(w, http.StatusLocked, "vault locked")
		return true
	}
	return false
}

func (s *Server) vaultStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Secrets.Status())
}

type passphraseIn struct {
	Passphrase string `json:"passphrase"`
	Old        string `json:"old"`
	New        string `json:"new"`
}

func (s *Server) vaultInit(w http.ResponseWriter, r *http.Request) {
	var in passphraseIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.Secrets.Initialize(in.Passphrase); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Secrets.Status())
}

func (s *Server) vaultUnlock(w http.ResponseWriter, r *http.Request) {
	var in passphraseIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.Secrets.Unlock(in.Passphrase); err != nil {
		// a wrong passphrase and a lockout are both 403: neither should leak
		// whether the guess was close, only that it failed
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Secrets.Status())
}

func (s *Server) vaultLock(w http.ResponseWriter, _ *http.Request) {
	s.Secrets.Lock()
	writeJSON(w, http.StatusOK, s.Secrets.Status())
}

func (s *Server) listSecrets(w http.ResponseWriter, _ *http.Request) {
	names, err := s.Secrets.ListNames()
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// names only — a values endpoint would defeat the entire design
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	writeJSON(w, http.StatusOK, out)
}

type secretIn struct {
	Name  string         `json:"name"`
	Value string         `json:"value"`
	Meta  map[string]any `json:"meta"`
}

func (s *Server) addSecret(w http.ResponseWriter, r *http.Request) {
	var in secretIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	err := s.Secrets.Put(in.Name, in.Value, in.Meta)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Broker.Record("set", in.Name, "")
	writeJSON(w, http.StatusCreated, map[string]string{"name": in.Name})
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	err := s.Secrets.Delete(r.PathValue("name"))
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Broker.Record("delete", r.PathValue("name"), "")
	w.WriteHeader(http.StatusNoContent)
}

type grantIn struct {
	Grantee    string `json:"grantee"`
	Scope      string `json:"scope"`
	TTLSeconds int    `json:"ttl_seconds"`
}

func (s *Server) makeGrant(w http.ResponseWriter, r *http.Request) {
	var in grantIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.TTLSeconds == 0 {
		in.TTLSeconds = 900
	}
	token, err := s.Broker.Grant(r.PathValue("name"), in.Grantee, in.Scope, in.TTLSeconds)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grant": token, "expires_in": in.TTLSeconds})
}

func (s *Server) listGrants(w http.ResponseWriter, _ *http.Request) {
	grants, err := s.Broker.List()
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	err := s.Broker.Revoke(r.PathValue("token"))
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) revokeAllGrants(w http.ResponseWriter, _ *http.Request) {
	err := s.Broker.RevokeAll()
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

type brokerIn struct {
	Grant  string `json:"grant"`
	Method string `json:"method"`
	URL    string `json:"url"`
	Header string `json:"header"`
	Body   string `json:"body"`
}

// brokerUse is the USE-not-READ endpoint: grimoire makes the call with the
// secret injected, and the caller never sees the value.
func (s *Server) brokerUse(w http.ResponseWriter, r *http.Request) {
	var in brokerIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(in.URL) == "" {
		writeErr(w, http.StatusBadRequest, "url required")
		return
	}
	res, err := s.Broker.Use(in.Grant, in.Method, in.URL, in.Header, in.Body)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		// 403, not 400: the request was well-formed, the grant just doesn't
		// permit it — and the message says which rule refused
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries, err := s.Broker.Audit(limit)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// changePassphrase rotates the vault key and re-seals every encrypted note.
func (s *Server) changePassphrase(w http.ResponseWriter, r *http.Request) {
	var in passphraseIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	reseal := func(oldKey, newKey []byte) error {
		rels, err := s.Vault.Walk()
		if err != nil {
			return err
		}
		for _, rel := range rels {
			note, err := s.Vault.Read(rel)
			if err != nil || !note.Encrypted {
				continue
			}
			body, err := secrets.ResealWith(oldKey, newKey, note.Body)
			if err != nil {
				return err
			}
			if _, err := s.Vault.Write(rel, body, note.Frontmatter); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.Secrets.ChangePassphrase(in.Old, in.New, reseal); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.Index.Reindex(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Secrets.Status())
}
