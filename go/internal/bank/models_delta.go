package bank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Delta refresh: instead of rewriting a mental model's answer, apply
// targeted edits to the sections that the facts and observations new since
// the last refresh bear on. Sections a person wrote or changed are never
// touched; an edit aimed at one waits as a pending proposal, the same path a
// full refresh takes over a person's text.
//
// Which sections are a person's is read from the file alone: the model
// records each section's text sum when it writes (section_sums), and a
// section whose text no longer matches, or that was never recorded, is theirs.
// Which facts are new is read from `seen`, a fingerprint list the refresh
// recorded.

// maxSeen bounds the fingerprint list; a model over it can only full-refresh.
const maxSeen = 3000

// maxDeltaItems bounds how many new items one delta shows the model.
const maxDeltaItems = 40

const deltaSystem = "You maintain a standing answer to a question, written as markdown sections. New facts have " +
	"arrived. Reply with JSON {\"edits\":[{\"op\":\"append|replace|add\",\"section\":\"<heading text>\"," +
	"\"text\":\"<markdown>\"}]}. append adds text to the end of an existing section, replace rewrites an existing " +
	"section's body (heading stays), add creates a new section at the end. Edit only what the new facts change; " +
	"leave everything else alone. If nothing changes reply {\"edits\":[]}."

func setOrDelete(fm *markdown.Frontmatter, key string, on bool, val string) {
	if on {
		fm.Set(key, val)
	} else {
		fm.Delete(key)
	}
}

func setListOrDelete(fm *markdown.Frontmatter, key string, xs []string) {
	if len(xs) > 0 {
		fm.Set(key, listValue(xs))
	} else {
		fm.Delete(key)
	}
}

func unitPrint(u *unit) string {
	h := sha256.Sum256([]byte(u.ID + "\x00" + u.Text))
	return "s" + hex.EncodeToString(h[:])[:8]
}

// seenPrints fingerprints every in-scope unit; nil when there are too many.
func (c *bankCache) seenPrints(m *MentalModel) []string {
	var out []string
	for i := range c.units {
		if m.inScope(&c.units[i]) {
			out = append(out, unitPrint(&c.units[i]))
		}
	}
	if len(out) > maxSeen {
		return nil
	}
	sort.Strings(out)
	return out
}

// mdSection is one heading and the text under it. The first section may have
// no heading: it is the text before the first one.
type mdSection struct {
	Head string
	Text string // trimmed; includes the heading line
}

func parseMdSections(body string) []mdSection {
	var out []mdSection
	var cur []string
	head := ""
	fence := false
	flush := func() {
		t := strings.TrimSpace(strings.Join(cur, "\n"))
		if t != "" || head != "" {
			out = append(out, mdSection{Head: head, Text: t})
		}
		cur = nil
	}
	for _, ln := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(ln, "#") {
			if t := strings.TrimLeft(ln, "#"); t != ln[1:] || strings.HasPrefix(t, " ") {
				if strings.HasPrefix(t, " ") {
					flush()
					head = strings.TrimSpace(strings.TrimRight(t, "# "))
				}
			}
		}
		cur = append(cur, ln)
	}
	flush()
	return out
}

func renderMdSections(ss []mdSection) string {
	parts := make([]string, 0, len(ss))
	for _, s := range ss {
		parts = append(parts, strings.TrimSpace(s.Text))
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// sectionKeys gives each mdSection a stable key: its heading, numbered when a
// heading repeats.
func mdSectionKeys(ss []mdSection) []string {
	seen := map[string]int{}
	keys := make([]string, len(ss))
	for i, s := range ss {
		h := strings.ToLower(s.Head)
		seen[h]++
		k := h
		if seen[h] > 1 {
			k += "#" + strconv.Itoa(seen[h])
		}
		sum := sha256.Sum256([]byte(k))
		keys[i] = "k" + hex.EncodeToString(sum[:])[:6]
	}
	return keys
}

func secSum(t string) string { return bodySum(t)[:8] }

// mdSectionSumList records each section as written: key_textsum.
func mdSectionSumList(ss []mdSection, keep map[string]string, human map[string]bool) []string {
	keys := mdSectionKeys(ss)
	out := make([]string, len(ss))
	for i, s := range ss {
		sum := secSum(s.Text)
		if human[keys[i]] {
			sum = "x"
			if old, ok := keep[keys[i]]; ok {
				sum = old
			}
		}
		out[i] = keys[i] + "_" + sum
	}
	return out
}

func parseSectionSums(xs []string) map[string]string {
	out := map[string]string{}
	for _, x := range xs {
		if k, v, ok := strings.Cut(x, "_"); ok {
			out[k] = v
		}
	}
	return out
}

// humanSections is the set of section keys a person owns in the model's body.
func (m *MentalModel) humanSections(ss []mdSection) map[string]bool {
	human := map[string]bool{}
	if !m.edited() {
		return human
	}
	keys := mdSectionKeys(ss)
	if len(m.sectionSums) == 0 {
		for _, k := range keys {
			human[k] = true
		}
		return human
	}
	rec := parseSectionSums(m.sectionSums)
	for i, s := range ss {
		if sum, ok := rec[keys[i]]; !ok || sum != secSum(s.Text) {
			human[keys[i]] = true
		}
	}
	return human
}

type deltaEdit struct {
	Op      string `json:"op"`
	Section string `json:"section"`
	Text    string `json:"text"`
}

// applyEdits applies edits to a body. Edits whose target section is in
// `human` are returned instead of applied.
func applyEdits(body string, edits []deltaEdit, human map[string]bool) (string, []deltaEdit, int) {
	ss := parseMdSections(body)
	var held []deltaEdit
	applied := 0
	for _, ed := range edits {
		text := strings.TrimSpace(ed.Text)
		head := strings.TrimSpace(ed.Section)
		if text == "" {
			continue
		}
		idx := -1
		keys := mdSectionKeys(ss)
		want := mdSectionKeys([]mdSection{{Head: head}})[0]
		for i, k := range keys {
			if k == want {
				idx = i
				break
			}
		}
		switch strings.ToLower(ed.Op) {
		case "append", "replace":
			if idx < 0 {
				ss = append(ss, mdSection{Head: head, Text: "## " + head + "\n\n" + text})
				applied++
				continue
			}
			if human[keys[idx]] {
				held = append(held, ed)
				continue
			}
			if strings.ToLower(ed.Op) == "append" {
				ss[idx].Text = strings.TrimSpace(ss[idx].Text) + "\n\n" + text
			} else if ss[idx].Head != "" {
				ss[idx].Text = "## " + ss[idx].Head + "\n\n" + text
			} else {
				ss[idx].Text = text
			}
			applied++
		case "add":
			if idx >= 0 {
				if human[keys[idx]] {
					held = append(held, ed)
					continue
				}
				ss[idx].Text = strings.TrimSpace(ss[idx].Text) + "\n\n" + text
			} else {
				ss = append(ss, mdSection{Head: head, Text: "## " + head + "\n\n" + text})
			}
			applied++
		}
	}
	return renderMdSections(ss), held, applied
}

// newUnits are the in-scope units whose fingerprint the last refresh had not
// recorded, oldest file order.
func (c *bankCache) newUnits(m *MentalModel) []*unit {
	seen := make(map[string]bool, len(m.seen))
	for _, s := range m.seen {
		seen[s] = true
	}
	var out []*unit
	for i := range c.units {
		u := &c.units[i]
		if m.inScope(u) && !u.DocRemoved && !seen[unitPrint(u)] {
			out = append(out, u)
		}
	}
	return out
}

// deltaEdits asks the model for section edits, falling back to a rule that
// lists the new items under "Recent additions".
func (e *Engine) deltaEdits(ctx context.Context, m *MentalModel, fresh []*unit, usage *Usage) []deltaEdit {
	items := fresh
	if len(items) > maxDeltaItems {
		items = items[:maxDeltaItems]
	}
	if e.AI.Available() {
		var b strings.Builder
		fmt.Fprintf(&b, "Question: %s\n\nCurrent answer:\n%s\n\nNew facts:\n", m.Question, m.Body)
		for _, u := range items {
			b.WriteString("- " + clip(u.Text, 300) + "\n")
		}
		client := e.AI.WithSurface("bank.refresh_delta", "")
		comp, err := client.CompleteWith(ctx, b.String(), ai.CompleteOpts{System: deltaSystem,
			Temperature: ai.Temp(0.2), MaxTokens: 1200, JSON: true})
		if err == nil {
			var out struct {
				Edits []deltaEdit `json:"edits"`
			}
			if json.Unmarshal([]byte(extractJSONObject(comp.Text)), &out) == nil {
				return out.Edits
			}
		}
	}
	var b strings.Builder
	for _, u := range items {
		b.WriteString("- " + oneLine(u.Text) + "\n")
	}
	return []deltaEdit{{Op: "append", Section: "Recent additions", Text: b.String()}}
}

// refreshDelta is RefreshModel's delta path.
func (e *Engine) refreshDelta(ctx context.Context, bankID, id string, m *MentalModel, c *bankCache, out *RefreshOutcome) (bool, error) {
	if strings.TrimSpace(m.Body) == "" || len(m.seen) == 0 {
		return false, nil // nothing to edit yet: take the full path
	}
	out.Mode = "delta"
	fresh := c.newUnits(m)
	if len(fresh) == 0 {
		out.Outcome = "unchanged"
		return true, nil
	}
	edits := e.deltaEdits(ctx, m, fresh, &out.Usage)
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	// Re-read under the lock: a person may have edited while the model thought.
	m, n, err := e.readModel(bankID, id)
	if err != nil {
		return true, err
	}
	ss := parseMdSections(m.Body)
	human := m.humanSections(ss)
	newBody, held, applied := applyEdits(m.Body, edits, human)
	now := e.now().Format(time.RFC3339)
	sig := c.scopeSignature(m)
	based := append([]string{}, m.BasedOn...)
	have := map[string]bool{}
	for _, b := range based {
		have[b] = true
	}
	for _, u := range fresh {
		if !have[u.ID] {
			based = append(based, u.ID)
		}
	}
	out.BasedOn = based
	out.Held = len(held)
	if applied > 0 {
		if e.History != nil && strings.TrimSpace(n.Body) != "" {
			e.History.Snapshot(n.Path, n.Body)
		}
		nss := parseMdSections(newBody)
		m.sectionSums = mdSectionSumList(nss, parseSectionSums(m.sectionSums), human)
		if len(human) == 0 {
			m.bodySum = bodySum(newBody)
		} // else the stale sum keeps the body reading as a person's
		m.Version++
		m.LastRefreshed, m.scopeSig, m.BasedOn, m.seen = now, sig, based, c.seenPrints(m)
		if _, err := e.Vault.Write(n.Path, newBody, m.frontmatter(n.Frontmatter)); err != nil {
			return true, err
		}
		if _, err := e.Index.Upsert(n.Path); err != nil {
			return true, err
		}
		out.Outcome = "delta"
	}
	if len(held) > 0 {
		full, _, _ := applyEdits(newBody, held, nil)
		if err := e.writeProposal(bankID, id, m, full, based, sig, c.seenPrints(m), now); err != nil {
			return true, err
		}
		if applied == 0 {
			out.Outcome = "proposed"
		}
	} else if applied == 0 {
		out.Outcome = "unchanged"
	}
	out.Version = m.Version
	return true, nil
}
