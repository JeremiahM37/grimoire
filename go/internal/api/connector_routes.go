package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/auth"
	"github.com/JeremiahM37/grimoire/go/internal/connectors"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Configuring connectors over HTTP.
//
// Administrators only, for two reasons that are not the same: a connector
// holds a credential, and a connector decides what enters the vault. The first
// is obvious; the second matters more, because a connector writing into a
// shared space is writing into everyone's search results.
//
// /api/connectors/kinds is what makes "configure your own" real: it returns
// each source's fields, help text and where to get its credential, so the
// console renders a form it does not have hardcoded and a new source kind
// needs no console change at all.

func (s *Server) connectorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/connectors/kinds", s.adminOnly(s.connectorKinds))
	mux.HandleFunc("GET /api/connectors", s.adminOnly(s.listConnectors))
	mux.HandleFunc("POST /api/connectors", s.adminOnly(s.createConnector))
	mux.HandleFunc("PUT /api/connectors/{id}", s.adminOnly(s.updateConnector))
	mux.HandleFunc("DELETE /api/connectors/{id}", s.adminOnly(s.deleteConnector))
	mux.HandleFunc("POST /api/connectors/{id}/run", s.adminOnly(s.runConnector))
}

func (s *Server) connectorKinds(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, connectors.Kinds())
}

func (s *Server) listConnectors(w http.ResponseWriter, _ *http.Request) {
	if s.Connectors == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	list, err := s.Connectors.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []connectors.Connector{}
	}
	writeJSON(w, http.StatusOK, list)
}

type connectorIn struct {
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Config   map[string]string `json:"config"`
	Secret   string            `json:"secret"`
	Prefix   string            `json:"prefix"`
	Interval int               `json:"interval"`
	Enabled  *bool             `json:"enabled"`
}

func (s *Server) createConnector(w http.ResponseWriter, r *http.Request) {
	if s.Connectors == nil {
		writeErr(w, http.StatusNotImplemented, "connectors are unavailable")
		return
	}
	var in connectorIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	kind, err := connectors.Get(in.Kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := connectors.Config{}
	for k, v := range in.Config {
		cfg[k] = v
	}
	if err := connectors.Validate(in.Kind, cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	prefix := strings.Trim(strings.TrimSpace(in.Prefix), "/")
	if prefix == "" {
		prefix = kind.Describe().DefaultPrefix
	}
	// A connector writes notes, so its destination has to be a path the vault
	// will accept — checked now rather than discovered at the first sync.
	if _, err := s.Vault.SafePath(prefix + "/probe.md"); err != nil {
		writeErr(w, http.StatusBadRequest, "destination: "+err.Error())
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = kind.Describe().Name
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	c := connectors.Connector{
		ID: newConnectorID(), Kind: kind.Kind(), Name: name, Config: cfg,
		Secret: strings.TrimSpace(in.Secret), Prefix: prefix,
		Interval: in.Interval, Enabled: enabled, LastOK: true,
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.Connectors.Save(c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateConnector(w http.ResponseWriter, r *http.Request) {
	if s.Connectors == nil {
		writeErr(w, http.StatusNotImplemented, "connectors are unavailable")
		return
	}
	c, err := s.Connectors.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var in connectorIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Name != "" {
		c.Name = strings.TrimSpace(in.Name)
	}
	if in.Config != nil {
		cfg := connectors.Config{}
		for k, v := range in.Config {
			cfg[k] = v
		}
		if err := connectors.Validate(c.Kind, cfg); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		c.Config = cfg
	}
	if in.Secret != "" {
		c.Secret = strings.TrimSpace(in.Secret)
	}
	if in.Interval > 0 {
		c.Interval = in.Interval
	}
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	}
	if err := s.Connectors.Save(c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteConnector(w http.ResponseWriter, r *http.Request) {
	if s.Connectors == nil {
		writeErr(w, http.StatusNotImplemented, "connectors are unavailable")
		return
	}
	id := r.PathValue("id")
	// The notes it pulled are kept unless asked otherwise. They are ordinary
	// notes now — someone may have linked to one — and deleting a connector
	// should not silently delete a chunk of the vault.
	if truthy(r.URL.Query().Get("purge")) {
		paths, err := s.Connectors.Paths(id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, p := range paths {
			_ = s.DeleteNote(p)
		}
	}
	if err := s.Connectors.Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) runConnector(w http.ResponseWriter, r *http.Request) {
	if s.Runner == nil {
		writeErr(w, http.StatusNotImplemented, "connectors are unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, err := s.Runner.Run(ctx, r.PathValue("id"))
	if err != nil {
		// The run's outcome is already recorded on the connector; this reports
		// it to whoever pressed the button, with the same words.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": err.Error(), "written": res.Written, "skipped": res.Skipped})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "written": res.Written, "skipped": res.Skipped, "cursor": res.Cursor})
}

// ------------------------------------------------------- connectors.Writer

// WriteNote implements connectors.Writer: a pulled document becomes an
// ordinary note, indexed like any other.
func (s *Server) WriteNote(path, body string, frontmatter map[string]any) (string, error) {
	rel := normPath(path)
	fm := markdown.NewFrontmatter()
	for k, v := range frontmatter {
		switch t := v.(type) {
		case string:
			fm.Set(k, t)
		case bool:
			fm.Set(k, t)
		case int:
			fm.Set(k, fmt.Sprint(t))
		default:
			fm.Set(k, fmt.Sprint(t))
		}
	}
	if _, err := s.Vault.Write(rel, body, fm); err != nil {
		return "", err
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		return "", err
	}
	return rel, nil
}

// DeleteNote implements connectors.Writer.
func (s *Server) DeleteNote(path string) error {
	rel := normPath(path)
	if err := s.Vault.Delete(rel); err != nil {
		return err
	}
	return s.Index.Remove(rel)
}

// SecretsForConnectors adapts the credential vault to what the runner needs.
//
// The value is resolved server-side and handed to the connector for one
// request. It is never written to the connector row, never returned by the
// API, and never lands in a note — which is the same promise the broker makes
// to agents, kept for connectors too.
type SecretsForConnectors struct{ Server *Server }

func (s SecretsForConnectors) Get(name string) (string, error) {
	return s.Server.Secrets.Get(name)
}

func newConnectorID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// connectorSpaceHint is used by the console to explain where documents land.
var _ = auth.CommonsID
