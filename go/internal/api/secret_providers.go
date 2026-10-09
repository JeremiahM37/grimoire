package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/secrets/providers"
)

// External password-manager providers and links. See secrets/external.go.
//
//	GET    /api/secrets/providers             admin — configured providers + supported kinds (no unlock material)
//	POST   /api/secrets/providers             admin — add or replace one
//	DELETE /api/secrets/providers/{name}      admin — refused while handles still link to it
//	POST   /api/secrets/providers/{name}/test admin — reachable and unlocked?
//	POST   /api/secrets/link                  admin — point a handle at an external item
//	POST   /api/secrets/{name}/unlink         admin
//
// No route here ever returns a resolved value or any stored unlock material.

func (s *Server) listProviders(w http.ResponseWriter, _ *http.Request) {
	ps, err := s.Secrets.Providers()
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	kinds := []map[string]any{}
	for _, k := range providers.Kinds() {
		kinds = append(kinds, map[string]any{"kind": k.Name, "scheme": k.Scheme + "://", "summary": k.Summary})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": ps, "kinds": kinds})
}

type providerIn struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Settings map[string]string `json:"settings"`
	Secrets  map[string]string `json:"secrets"`
}

func (s *Server) addProvider(w http.ResponseWriter, r *http.Request) {
	var in providerIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Name == "" {
		in.Name = in.Kind
	}
	err := s.Secrets.AddProvider(providers.Config{Name: in.Name, Kind: in.Kind,
		Settings: in.Settings, Secrets: in.Secrets})
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Broker.Record("provider_add", "", "provider="+in.Name+" kind="+in.Kind)
	writeJSON(w, http.StatusCreated, map[string]string{"name": in.Name, "kind": in.Kind})
}

func (s *Server) removeProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.Secrets.RemoveProvider(name)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Broker.Record("provider_remove", "", "provider="+name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testProvider(w http.ResponseWriter, r *http.Request) {
	msg, err := s.Secrets.TestProvider(r.PathValue("name"))
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		// 200 with ok:false: the test ran; the answer is "not usable right now"
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": msg})
}

type secretLinkIn struct {
	Name     string `json:"name"`
	Ref      string `json:"ref"`
	Provider string `json:"provider"`
	Note     string `json:"note"`
}

func (s *Server) linkSecret(w http.ResponseWriter, r *http.Request) {
	var in secretLinkIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Ref) == "" {
		writeErr(w, http.StatusBadRequest, "name and ref required")
		return
	}
	prov, err := s.Secrets.ResolveProviderName(in.Ref, in.Provider)
	if err == nil {
		err = s.Secrets.Link(in.Name, prov, in.Ref, in.Note)
	}
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Broker.Record("link", in.Name, "provider="+prov+" ref="+in.Ref)
	writeJSON(w, http.StatusCreated, map[string]string{"name": in.Name, "provider": prov, "ref": in.Ref})
}

func (s *Server) unlinkSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.Secrets.Unlink(name)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Broker.Record("unlink", name, "")
	w.WriteHeader(http.StatusNoContent)
}
