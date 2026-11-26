package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// Secret files: the credentials agents actually reach for.
//
// Agents on this kind of host read keys out of plaintext files — a YAML of API
// keys, a .env — dozens of times a month, and never through the vault, because
// the vault's agent path was an MCP call and what they needed was a value in a
// shell command. Importing a file puts every value in it into the vault under
// files/<name>/<key>, and records, without any value, which file each came
// from. `grimoire run --file PATH -- cmd` then hands that file's values to one
// command as environment variables, and the context hook can point an agent
// that tries to cat the file at that command instead.

// vaultedFile is one imported file in the index: where it was, and which
// environment variable each of its values arrives as. No values.
type vaultedFile struct {
	Prefix string            `json:"prefix"`
	Vars   map[string]string `json:"vars"` // ENV_VAR -> secret name
}

// vaultedIndexPath is where the list of imported files is kept, beside the
// rest of the per-user Grimoire configuration.
func vaultedIndexPath() string {
	if p := strings.TrimSpace(os.Getenv("GRIMOIRE_VAULTED_FILES")); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "grimoire", "vaulted-files.json")
}

func loadVaulted() map[string]vaultedFile {
	out := map[string]vaultedFile{}
	if b, err := os.ReadFile(vaultedIndexPath()); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveVaulted(idx map[string]vaultedFile) error {
	p := vaultedIndexPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(idx, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// flattenSecretFile reads a .env, .json or simple YAML file into dotted keys
// and scalar values. YAML here means what credential files are: nested
// mappings of scalars. A list, a block scalar or anything else it does not
// understand is skipped rather than guessed at.
func flattenSecretFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		out := map[string]string{}
		flattenJSON("", doc, out)
		return out, nil
	case ".yaml", ".yml":
		return flattenYAML(string(raw)), nil
	default:
		return flattenDotenv(string(raw)), nil
	}
}

func flattenJSON(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenJSON(key, child, out)
		}
	case string:
		if prefix != "" && x != "" {
			out[prefix] = x
		}
	case float64, bool:
		if prefix != "" {
			out[prefix] = fmt.Sprint(x)
		}
	}
}

func flattenDotenv(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if key = strings.TrimSpace(key); ok && key != "" {
			if val = unquoteScalar(strings.TrimSpace(val)); val != "" {
				out[key] = val
			}
		}
	}
	return out
}

func flattenYAML(text string) map[string]string {
	out := map[string]string{}
	type level struct {
		indent int
		key    string
	}
	var stack []level
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" || strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			continue
		}
		indent := len(line) - len(trimmed)
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok || strings.ContainsAny(key, " \t\"'{[") && !strings.HasPrefix(key, "\"") {
			continue
		}
		key = unquoteScalar(strings.TrimSpace(key))
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		val = strings.TrimSpace(val)
		if q := quotedPrefix(val); q != "" {
			val = q // anything after the closing quote is a comment
		} else if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		if val == "" {
			stack = append(stack, level{indent, key})
			continue
		}
		if val == "|" || val == ">" || strings.HasPrefix(val, "|") || strings.HasPrefix(val, ">") || strings.HasPrefix(val, "[") || strings.HasPrefix(val, "{") || strings.HasPrefix(val, "&") || strings.HasPrefix(val, "*") {
			continue
		}
		parts := make([]string, 0, len(stack)+1)
		for _, l := range stack {
			parts = append(parts, l.key)
		}
		if v := unquoteScalar(val); v != "" && v != "null" && v != "~" {
			out[strings.Join(append(parts, key), ".")] = v
		}
	}
	return out
}

// quotedPrefix returns a value's leading quoted scalar, quotes included, or
// "" when it does not start with one.
func quotedPrefix(v string) string {
	if v == "" || (v[0] != '"' && v[0] != '\'') {
		return ""
	}
	q := v[0]
	for i := 1; i < len(v); i++ {
		switch {
		case q == '"' && v[i] == '\\':
			i++
		case v[i] == q && q == '\'' && i+1 < len(v) && v[i+1] == '\'':
			i++ // '' is an escaped quote
		case v[i] == q:
			return v[:i+1]
		}
	}
	return ""
}

func unquoteScalar(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		inner := v[1 : len(v)-1]
		if v[0] == '"' {
			var s string
			if json.Unmarshal([]byte(v), &s) == nil {
				return s
			}
		}
		return strings.ReplaceAll(inner, "''", "'")
	}
	return v
}

// importSecretFile copies every value in a file into the vault and records
// the file in the index. The file is left where it is: removing a plaintext
// copy a running service still reads is the owner's step, not an import's.
func importSecretFile(v *secrets.Vault, path string, flags map[string]string) int {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail("%v", err)
	}
	values, err := flattenSecretFile(abs)
	if err != nil {
		return fail("%v", err)
	}
	if len(values) == 0 {
		return fail("no values found in %s", abs)
	}
	stem := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	prefix := flags["prefix"]
	if prefix == "" {
		prefix = "files/" + stem
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	entry := vaultedFile{Prefix: prefix, Vars: map[string]string{}}
	for _, k := range keys {
		name := prefix + "/" + k
		note := "imported from " + abs + " (" + k + ")"
		if !credentialKey(k) {
			// A URL, a user name or a host travels with the file so a command
			// gets everything it needs, but it is not a secret, and the dream's
			// sweep must not report every note that mentions it.
			note = secrets.ConfigNotePrefix + note
		}
		if err := v.PutVersioned(name, values[k], map[string]any{
			secrets.MetaNote: note,
		}, "imported from "+filepath.Base(abs)); err != nil {
			return fail("%v", err)
		}
		entry.Vars[envName(k)] = name
	}
	idx := loadVaulted()
	idx[abs] = entry
	if err := saveVaulted(idx); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("imported %d value(s) from %s as %s/…\n", len(keys), abs, prefix)
	fmt.Printf("use them with: grimoire run --file %s -- COMMAND\n", abs)
	vars := make([]string, 0, len(entry.Vars))
	for name := range entry.Vars {
		vars = append(vars, "$"+name)
	}
	sort.Strings(vars)
	fmt.Printf("variables: %s\n", strings.Join(vars, " "))
	return 0
}

// credentialKey is secrets.CredentialKey: whether an imported key's value is
// a credential, or config stored beside one.
func credentialKey(k string) bool { return secrets.CredentialKey(k) }

// fileSecrets returns the secret names `run --file` hands over for a file,
// as NAME=ENVVAR pairs.
func fileSecrets(path string) ([]string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	idx := loadVaulted()
	entry, ok := idx[abs]
	if !ok {
		for p, e := range idx {
			if real, err := filepath.EvalSymlinks(p); err == nil && real == abs {
				entry, ok = e, true
				break
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("%s has not been imported; run: grimoire secret import %s", abs, abs)
	}
	out := make([]string, 0, len(entry.Vars))
	for envVar, name := range entry.Vars {
		out = append(out, name+"="+envVar)
	}
	sort.Strings(out)
	return out, nil
}
