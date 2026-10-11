package bank

import (
	"errors"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// Origin taint for bank content.
//
// A note carries its origin and fences untrusted text (see internal/trust). A
// bank used to lose that: a fact retained from a web page or a connector was
// recalled exactly like one a person typed, and so was every observation,
// reflection and mental model built from it. A summary of an injected passage
// is itself an injection channel, so taint has to travel with the content.
//
// The rules, all enforced in this package:
//
//   - Taint is decided at retain time from the item's declared origin. An item
//     with no origin, or a self origin ("self", "user", "me", "local"), is a
//     direct statement and is trusted. Anything else is untrusted. An item may
//     also DOWNGRADE itself with trust "untrusted". An item cannot upgrade
//     itself: a caller-supplied "trusted" is ignored, because an agent that
//     could mark its own pulled text trusted would defeat the whole mechanism.
//   - Taint is a max over inputs. An observation, reflection or mental model
//     derived from any untrusted fact or observation is untrusted, and taint
//     is never dropped by summarising. Taint is sticky.
//   - Untrusted content is labelled with trust and origin on every recall
//     result, and is fenced (trust.Fence) wherever it is rendered into a
//     prompt, the same way note memory is.
//   - Untrusted material never becomes or edits a directive, never supersedes
//     or retires a trusted observation, and never auto-refreshes a model a
//     person wrote.
//   - Only a person clears taint, with a recorded reason (PromoteUnit).
//
// What this relies on: the caller declaring the source truthfully. Nothing
// here can see that a string came from a web page. The default for an item
// that declares nothing is trusted, as it is for a note with no origin.

// ErrTainted refuses a change that would make untrusted content into an
// instruction or overwrite trusted content with it.
var ErrTainted = errors.New("derived from untrusted content")

// ErrPersonOnly refuses a promotion requested by anything other than a person.
var ErrPersonOnly = errors.New("only a person may promote untrusted content")

// Trust values an item may declare.
const (
	trustDeclaredTrusted   = "trusted"
	trustDeclaredUntrusted = "untrusted"
)

// tntKey is the trailer key that carries taint on a fact or observation.
// `tnt=untrusted` marks an untrusted unit; `tnt=promoted promo=<reason>` marks
// one a person has cleared. Absent means trusted, which is how every unit
// written before taint existed reads, so old files do not change.
const (
	tntKey        = "tnt"
	tntUntrusted  = "untrusted"
	tntPromoted   = "promoted"
	promoReasonKV = "promo"
	originKV      = "org"
)

// validTrust reports whether a declared trust value is one an item may carry.
func validTrust(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", trustDeclaredTrusted, trustDeclaredUntrusted:
		return true
	}
	return false
}

// itemUntrusted decides whether content retained with this item is untrusted.
// It is the only place the retain path makes that decision.
func itemUntrusted(it Item) bool {
	if strings.ToLower(strings.TrimSpace(it.Trust)) == trustDeclaredUntrusted {
		return true
	}
	return trust.FromOrigin(it.Origin) == trust.Untrusted
}

// itemOrigin is the origin recorded for untrusted content, trimmed. Trusted
// content keeps no origin, so an old file and a new trusted one look the same.
func itemOrigin(it Item) string {
	if !itemUntrusted(it) {
		return ""
	}
	return strings.TrimSpace(it.Origin)
}

// anyUntrusted reports whether any item in a group is untrusted, and the
// origin of the first untrusted one (the label a fence will carry).
func anyUntrusted(items []Item) (bool, string) {
	for _, it := range items {
		if itemUntrusted(it) {
			return true, itemOrigin(it)
		}
	}
	return false, ""
}

// docUntrustedFM reads a document's taint from its frontmatter, using the
// same keys the vault reads for every note (`origin` and `trust`), so a bank
// document and a note agree about what is untrusted.
func docUntrustedFM(fm *markdown.Frontmatter) bool {
	return trust.Of(fm.StringVal("origin"), fm.StringVal("trust")) == trust.Untrusted
}

// anyTainted reports whether any of the named units is untrusted. Ids that do
// not resolve count as untrusted: a derivation from something the bank cannot
// place is not one to trust.
func (e *Engine) anyTainted(bankID string, ids []string) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	c, err := e.cache(bankID)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		p, ok := c.byID[id]
		if !ok || c.units[p].Tainted {
			return true, nil
		}
	}
	return false, nil
}

// PromoteUnit clears the taint on one fact or observation. Only a person may
// do it, and the reason is written into the file, where it can be read back
// and reviewed. Authorship is not changed: a promoted model observation stays
// the model's, with its text now trusted.
func (e *Engine) PromoteUnit(bankID, id, reason string, person bool) error {
	if !person {
		return ErrPersonOnly
	}
	reason = oneLine(reason)
	if reason == "" {
		return invalid("promotion needs a reason")
	}
	c, err := e.cache(bankID)
	if err != nil {
		return err
	}
	p, ok := c.byID[id]
	if !ok {
		return ErrNotFound
	}
	u := c.units[p]
	if !u.Tainted {
		return invalid("unit %s is already trusted", id)
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	if u.Type == "observation" {
		of, old, err := e.readObservations(bankID)
		if err != nil {
			return err
		}
		i := -1
		for k := range of.Current {
			if of.Current[k].ID == id {
				i = k
			}
		}
		if i < 0 {
			return ErrNotFound
		}
		of.Current[i].Tainted, of.Current[i].Promo = false, reason
		return e.writeObservations(bankID, of, old)
	}
	n, err := e.Vault.Read(u.Path)
	if err != nil {
		return err
	}
	ff := ParseFacts(n.Body, bankID, u.Doc)
	found := false
	for k := range ff.Facts {
		if ff.Facts[k].ID == id {
			ff.Facts[k].Tainted, ff.Facts[k].Promo = false, reason
			found = true
		}
	}
	if !found {
		return ErrNotFound
	}
	if _, err := e.Vault.Write(u.Path, FormatFacts(u.Doc, ff), n.Frontmatter); err != nil {
		return err
	}
	_, err = e.Index.Upsert(u.Path)
	return err
}

// fenceUnit renders one untrusted passage for a prompt. Trusted text passes
// through byte-identical, so a vault with no connectors costs nothing.
func fenceUnit(n int, t bool, origin, text string) string {
	if !t {
		return text
	}
	return trust.Fence(n, origin, text)
}

// trustLabel is the word a recall result carries for a unit's level.
func trustLabel(untrusted bool) string {
	if untrusted {
		return trust.NameUntrusted
	}
	return trust.NameTrusted
}

// taintFact marks a fact as drawn from untrusted content. Taint is sticky: a
// fact already tainted keeps its origin, and an untainted one takes the
// group's.
func taintFact(f Fact, origin string) Fact {
	if !f.Tainted && f.Promo == "" {
		f.Tainted = true
	}
	if f.Tainted && f.Origin == "" {
		f.Origin = origin
	}
	return f
}

// docFrontmatter is the frontmatter a document is written with. A document
// that is no longer untrusted loses its taint keys, which the merge with the
// existing file would otherwise keep.
func (e *Engine) docFrontmatter(rel string, d *Document) *markdown.Frontmatter {
	fm := e.keepFrontmatter(rel, d.Frontmatter())
	if !d.Untrusted {
		fm.Delete("trust")
		fm.Delete("origin")
	}
	return fm
}

// docOrigin reads a stored document's origin and whether it is untrusted, for
// fencing its chunks on the way out.
func (e *Engine) docOrigin(bankID, doc string) (string, bool) {
	n, err := e.Vault.Read(DocumentPath(bankID, doc))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(n.Frontmatter.StringVal("origin")), docUntrustedFM(n.Frontmatter)
}

// taintedAmong reports whether any of the named batch units is untrusted.
func taintedAmong(batch map[string]*unit, ids []string) bool {
	for _, id := range ids {
		if u := batch[id]; u != nil && u.Tainted {
			return true
		}
	}
	return false
}

// anyTainted reports whether the cache holds any untrusted unit.
func (c *bankCache) anyTainted() bool {
	for i := range c.units {
		if c.units[i].Tainted {
			return true
		}
	}
	return false
}

// anyUntrustedModel reports whether any mental model in the bank is untrusted.
func (e *Engine) anyUntrustedModel(bankID string) bool {
	models, err := e.ListModels(bankID, ModelQuery{})
	if err != nil {
		return false
	}
	for _, m := range models {
		if m.Untrusted {
			return true
		}
	}
	return false
}
