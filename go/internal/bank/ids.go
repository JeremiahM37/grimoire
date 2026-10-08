// Package bank is the memory-bank engine: named, isolated memories that an
// agent fills by handing over raw content (a conversation, a document) and
// queries with a hybrid recall.
//
// A bank differs from the per-fact `remember` memory in who does the
// distilling. `remember` stores what an agent already decided was worth
// keeping; a bank takes the transcript itself, extracts facts from it with a
// model (or with rules when there is none), resolves the people and things
// they mention into entities, and retrieves over four signals at once —
// meaning, words, the entity graph and time.
//
// The vault is still the truth. Everything a model produced lands in markdown
// under banks/<bank>/: the profile in bank.md, each source as retained in
// documents/, and the extracted facts — one bullet each, with a machine
// trailer — in facts/. The SQLite tables this package writes are caches built
// from those files by the ordinary index pass, so a reindex rebuilds a bank
// without calling a model, a person can read or correct any fact in an
// editor, and what a person wrote outranks what a model inferred (see
// authority.go).
package bank

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Root is the vault folder banks live under.
const Root = "banks"

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)

// ValidID reports whether id may name a bank: lowercase letters, digits and
// `._:-`, starting with a letter or digit, at most 64 characters.
//
// `:` is allowed because the natural way to name a family of banks is a
// namespace ("coding-agent:grimoire"), and it is mapped to `__` on disk. For
// that mapping to be reversible an id may not itself contain `__`, nor an
// underscore next to a colon. `..` is refused so no id reads as a parent
// directory to anything downstream.
func ValidID(id string) bool {
	return idRE.MatchString(id) && !strings.Contains(id, "..") && !strings.Contains(id, "__") &&
		!strings.Contains(id, "_:") && !strings.Contains(id, ":_")
}

func dirOf(id string) string { return strings.ReplaceAll(id, ":", "__") }

// IDFromDir reverses dirOf.
func IDFromDir(dir string) string { return strings.ReplaceAll(dir, "__", ":") }

// Prefix is the vault path prefix that holds a bank. Access control applies to
// it exactly as to any other folder of notes.
func Prefix(id string) string { return Root + "/" + dirOf(id) + "/" }

// ProfilePath is the bank's bank.md.
func ProfilePath(id string) string { return Prefix(id) + "bank.md" }

// DocumentPath is where a retained document's source text lives.
func DocumentPath(id, doc string) string { return Prefix(id) + "documents/" + fileOf(doc) + ".md" }

// FactsPath is where a document's extracted facts live.
func FactsPath(id, doc string) string { return Prefix(id) + "facts/" + fileOf(doc) + ".md" }

var plainDocRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// fileOf maps a caller's document id to a file stem. Ids are whatever the
// caller uses — a session id, a URL, a path — so anything that is not already
// a safe stem is slugged and suffixed with a hash of the original, which keeps
// two ids that slug alike from sharing a file. The real id is stored in the
// file's frontmatter, so this mapping never has to be reversed.
func fileOf(doc string) string {
	if plainDocRE.MatchString(doc) && !strings.HasSuffix(doc, ".md") {
		return doc
	}
	var b strings.Builder
	for _, r := range strings.ToLower(doc) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 60 {
			break
		}
	}
	stem := strings.Trim(b.String(), "-")
	if stem == "" {
		stem = "doc"
	}
	return stem + "-" + shortHash(doc, 10)
}

// PathKind classifies a vault path inside a bank.
type PathKind int

const (
	NotBank PathKind = iota
	ProfileKind
	DocumentKind
	FactsKind
	ObservationsKind
	ModelKind
	ProposalKind
	OtherKind
)

// ParsePath splits a vault path into the bank it belongs to and what kind of
// bank file it is.
func ParsePath(rel string) (bankID string, kind PathKind) {
	if !strings.HasPrefix(rel, Root+"/") {
		return "", NotBank
	}
	rest := rel[len(Root)+1:]
	dir, tail, ok := strings.Cut(rest, "/")
	if !ok || dir == "" {
		return "", NotBank
	}
	id := IDFromDir(dir)
	if !ValidID(id) {
		return "", NotBank
	}
	switch {
	case tail == "bank.md":
		return id, ProfileKind
	case strings.HasPrefix(tail, "documents/") && !strings.Contains(tail[len("documents/"):], "/"):
		return id, DocumentKind
	case strings.HasPrefix(tail, "facts/") && !strings.Contains(tail[len("facts/"):], "/"):
		return id, FactsKind
	case tail == "observations.md":
		return id, ObservationsKind
	case strings.HasPrefix(tail, "models/") && strings.HasSuffix(tail, ".md"):
		return id, ModelKind
	case strings.HasPrefix(tail, "proposals/") && strings.HasSuffix(tail, ".md"):
		return id, ProposalKind
	}
	return id, OtherKind
}

// IsBankPath reports whether a vault path lives under banks/.
func IsBankPath(rel string) bool { return strings.HasPrefix(rel, Root+"/") }

func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// ChunkID names one chunk of one document. `_` separates the parts, so `~` and
// `_` inside them are escaped; the encoding is injective, which a chunk id has
// to be because it is how a recalled fact points back at its source.
func ChunkID(bankID, doc string, idx int) string {
	esc := strings.NewReplacer("~", "~7E", "_", "~5F")
	return esc.Replace(bankID) + "_" + esc.Replace(doc) + "_" + itoa(idx)
}

// ParseChunkID reverses ChunkID.
func ParseChunkID(id string) (bankID, doc string, idx int, ok bool) {
	parts := strings.Split(id, "_")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	un := strings.NewReplacer("~5F", "_", "~7E", "~")
	n, err := atoi(parts[2])
	if err != nil {
		return "", "", 0, false
	}
	return un.Replace(parts[0]), un.Replace(parts[1]), n, true
}
