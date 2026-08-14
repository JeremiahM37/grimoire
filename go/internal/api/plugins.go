package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Plugin API: list, enable/disable, and path-confined asset serving.
//
// The trust split is the point: built-ins ship with the server and the on-topic
// ones are enabled by default; VAULT plugins are untrusted code that arrived
// with someone's notes, so they are discovered but DISABLED until the human
// turns them on, and a disabled plugin serves nothing at all.

var pluginNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)

// defaultEnabledBuiltins are on out of the box. The bar: does it make the notes
// richer? Content renderers are invisible until you use their syntax; the
// productivity widgets ship enable-able, not enabled, so a fresh vault looks
// like a focused tool rather than a kitchen sink.
var defaultEnabledBuiltins = map[string]bool{
	"katex": true, "mermaid": true, "kanban": true, "vault-stats": true,
}

type pluginInfo struct {
	Name        string  `json:"name"`
	Version     string  `json:"version"`
	Description string  `json:"description"`
	Source      string  `json:"source"`
	Client      string  `json:"client"`
	Styles      *string `json:"styles"`
	Enabled     bool    `json:"enabled"`
	ClientURL   string  `json:"client_url"`
	StylesURL   *string `json:"styles_url"`
}

func (s *Server) pluginStatePath() string {
	return filepath.Join(s.Vault.Root, ".grimoire", "plugins.json")
}

func (s *Server) loadPluginState() map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(s.pluginStatePath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *Server) savePluginState(state map[string]bool) error {
	if err := os.MkdirAll(filepath.Dir(s.pluginStatePath()), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.pluginStatePath(), raw, 0o644)
}

// readManifest loads and validates one plugin. Anything broken returns nil — a
// malformed plugin must never take the app down.
func readManifest(dir, source string) *pluginInfo {
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil
	}
	var data struct {
		Name        string `json:"name"`
		Version     any    `json:"version"`
		Description string `json:"description"`
		Client      string `json:"client"`
		Styles      any    `json:"styles"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	base := filepath.Base(dir)
	// the manifest must match its directory, or a plugin could claim another's
	// name and shadow it
	if data.Name != base || !pluginNameRE.MatchString(data.Name) {
		return nil
	}
	client := data.Client
	if client == "" {
		client = "client.js"
	}
	if info, err := os.Stat(filepath.Join(dir, client)); err != nil || info.IsDir() {
		return nil
	}
	version := "0.0.0"
	if v, ok := data.Version.(string); ok && v != "" {
		version = v
	}
	var styles *string
	if v, ok := data.Styles.(string); ok && v != "" {
		styles = &v
	}
	return &pluginInfo{
		Name: data.Name, Version: version, Description: data.Description,
		Source: source, Client: client, Styles: styles,
	}
}

func (s *Server) discoverPlugins() []pluginInfo {
	state := s.loadPluginState()
	var out []pluginInfo
	seen := map[string]bool{}

	for _, src := range []struct{ source, root string }{
		{"builtin", s.PluginDir},
		{"vault", filepath.Join(s.Vault.Root, "plugins")},
	} {
		if src.root == "" {
			continue
		}
		entries, err := os.ReadDir(src.root)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if seen[name] {
				continue
			}
			info := readManifest(filepath.Join(src.root, name), src.source)
			if info == nil {
				continue
			}
			seen[name] = true
			// built-ins default per the on-topic list; vault plugins default
			// to DISABLED because they are untrusted code
			info.Enabled = src.source == "builtin" && defaultEnabledBuiltins[name]
			if v, ok := state[name]; ok {
				info.Enabled = v
			}
			info.ClientURL = "/plugins/" + name + "/" + info.Client
			if info.Styles != nil {
				u := "/plugins/" + name + "/" + *info.Styles
				info.StylesURL = &u
			}
			out = append(out, *info)
		}
	}
	if out == nil {
		out = []pluginInfo{}
	}
	return out
}

func (s *Server) listPlugins(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.discoverPlugins())
}

func (s *Server) enablePlugin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	found := false
	for _, p := range s.discoverPlugins() {
		if p.Name == name {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "no such plugin")
		return
	}
	state := s.loadPluginState()
	state[name] = body.Enabled
	if err := s.savePluginState(state); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "enabled": body.Enabled})
}

type scaffoldIn struct {
	Name string `json:"name"`
}

// scaffoldPlugin writes a hello-world vault plugin, disabled until enabled.
func (s *Server) scaffoldPlugin(w http.ResponseWriter, r *http.Request) {
	var in scaffoldIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !pluginNameRE.MatchString(name) {
		writeErr(w, http.StatusBadRequest,
			"name must be lowercase letters, digits and hyphens")
		return
	}
	dir := filepath.Join(s.Vault.Root, "plugins", name)
	if _, err := os.Stat(dir); err == nil {
		writeErr(w, http.StatusBadRequest, "plugin already exists")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	manifest := map[string]any{
		"name": name, "version": "0.1.0",
		"description": "a new grimoire plugin", "client": "client.js",
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	client := "// " + name + " — a grimoire plugin.\n" +
		"export function activate(api) {\n" +
		"  api.registerCommand('" + name + ": hello', () => alert('hello from " + name + "'));\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "client.js"), []byte(client), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": name, "source": "vault", "enabled": false,
		"path": "plugins/" + name,
	})
}

// servePluginAsset serves a plugin file, confined to that plugin's directory.
// A disabled plugin serves NOTHING: leaving assets reachable would let
// untrusted vault code run via a direct URL even while switched off.
func (s *Server) servePluginAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rel := r.PathValue("rel")

	var plugin *pluginInfo
	all := s.discoverPlugins()
	for i := range all {
		if all[i].Name == name {
			plugin = &all[i]
			break
		}
	}
	if plugin == nil || !plugin.Enabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	root := s.PluginDir
	if plugin.Source == "vault" {
		root = filepath.Join(s.Vault.Root, "plugins")
	}
	base := filepath.Join(root, name)
	target := filepath.Clean(filepath.Join(base, rel))
	// path confinement: a plugin may only serve its own files
	if target != base && !strings.HasPrefix(target, base+string(filepath.Separator)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, target)
}
