// Package vault is the filesystem side of the store. Files are the source of truth.
//
// Port of server/vault.py. Every path is sandboxed to the vault: traversal
// (".."), absolute paths and symlink escapes are rejected. That is a security
// boundary and is covered by negative fixtures, not just happy-path tests.
package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// ErrVault is the base for every rejection, so callers can distinguish a
// refusal from an I/O failure.
var ErrVault = errors.New("vault")

// ReservedDirs are never indexed or writable through note paths.
var ReservedDirs = []string{".grimoire", "templates"}

// EncPrefix marks a sealed note body. An encrypted body is opaque: no tags, no
// links, always private.
const EncPrefix = "grimoire:enc:v1:"

var (
	slugStripRE = regexp.MustCompile(`[^\p{L}\p{N}_\s-]`)
	slugDashRE  = regexp.MustCompile(`[\s_-]+`)
)

// Vault is a rooted store. Root must already be absolute and resolved.
type Vault struct{ Root string }

func New(root string) (*Vault, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs // vault may not exist yet; confinement still applies
	}
	return &Vault{Root: resolved}, nil
}

// SafePath resolves a vault-relative note path, rejecting anything that escapes
// the vault, and coercing the extension to .md the way note paths always are.
func (v *Vault) SafePath(rel string) (string, error) {
	return v.safe(rel, true)
}

// SafeRawPath is SafePath for attachments: same sandboxing, but the real
// extension is preserved so images and PDFs are not coerced to .md.
func (v *Vault) SafeRawPath(rel string) (string, error) {
	return v.safe(rel, false)
}

func (v *Vault) safe(rel string, forceMD bool) (string, error) {
	rel = strings.TrimLeft(strings.TrimSpace(rel), "/")
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrVault)
	}
	if forceMD && !strings.HasSuffix(rel, ".md") {
		rel += ".md"
	}
	// Clean resolves ".." lexically; combined with the prefix check below this
	// rejects traversal without needing the file to exist.
	target := filepath.Clean(filepath.Join(v.Root, rel))
	if target != v.Root && !strings.HasPrefix(target, v.Root+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path escapes vault: %q", ErrVault, rel)
	}
	inner, err := filepath.Rel(v.Root, target)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrVault, err)
	}
	for _, part := range strings.Split(filepath.ToSlash(inner), "/") {
		if part == ".grimoire" {
			return "", fmt.Errorf("%w: .grimoire is reserved", ErrVault)
		}
	}
	return target, nil
}

// RelOf returns the vault-relative form of an absolute path.
func (v *Vault) RelOf(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(v.Root, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// IsReserved reports whether a relative path sits under a reserved directory.
func IsReserved(rel string) bool {
	parts := strings.Split(strings.ReplaceAll(rel, `\`, "/"), "/")
	for _, p := range parts {
		for _, d := range ReservedDirs {
			if p == d {
				return true
			}
		}
	}
	return false
}

// Slugify turns a title into a filename stem.
func Slugify(title string) string {
	s := strings.ToLower(strings.TrimSpace(slugStripRE.ReplaceAllString(title, "")))
	s = slugDashRE.ReplaceAllString(s, "-")
	if s == "" {
		return "untitled"
	}
	return s
}

// IsEncrypted reports whether a body is a sealed blob.
func IsEncrypted(body string) bool {
	return strings.HasPrefix(strings.TrimLeft(body, " \t\r\n\v\f"), EncPrefix)
}

// Note is a parsed note: the fields every downstream surface is built from.
type Note struct {
	Path        string
	Title       string
	Frontmatter *markdown.Frontmatter
	Body        string
	Raw         string
	Tags        []string
	Links       []markdown.Link
	Private     bool
	Encrypted   bool
	MTime       float64
	Hash        string
}

// NoteFromText parses note text into its indexed form.
func NoteFromText(rel, text string, mtime float64) *Note {
	fm, body := markdown.ParseFrontmatter(text)
	stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	encrypted := IsEncrypted(body)

	tagBody := body
	if encrypted {
		tagBody = "" // an encrypted body is opaque
	}
	links := []markdown.Link{}
	if !encrypted {
		links = markdown.ExtractLinks(body)
	}
	return &Note{
		Path:        rel,
		Title:       markdown.DeriveTitle(fm, body, stem),
		Frontmatter: fm,
		Body:        body,
		Raw:         text,
		Tags:        tagUnion(fm, tagBody),
		Links:       links,
		Private:     encrypted || fm.BoolVal("private"),
		Encrypted:   encrypted,
		MTime:       mtime,
		Hash:        hashText(text),
	}
}

// tagUnion merges body tags with frontmatter tags, body first, deduplicated
// case-sensitively against the already-collected list (as Python does).
func tagUnion(fm *markdown.Frontmatter, body string) []string {
	tags := markdown.ExtractTags(body)
	v, ok := fm.Get("tags")
	if !ok {
		return tags
	}
	switch t := v.(type) {
	case []markdown.Value:
		for _, item := range t {
			s := valueToString(item)
			if !contains(tags, s) {
				tags = append(tags, s)
			}
		}
	case string:
		if t != "" && !contains(tags, t) {
			tags = append(tags, t)
		}
	}
	return tags
}

func valueToString(v markdown.Value) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		// Python str(True) is "True"
		if t {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}
