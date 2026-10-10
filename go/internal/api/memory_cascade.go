package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Cascade forgetting.
//
// A retracted fact is still in the store in several shapes: the bullet itself,
// the struck-through belief in the note's snapshot history, facts and
// observations a memory bank consolidated from it, mental models and proposals
// that were answered from it, dream reports that quoted it, and the vectors
// and full-text rows the index built from every one of those. Plain forget
// touches only the first. POST /api/memory/forget with cascade=true reaches
// the rest, and then checks, by searching the index again, that nothing it
// was asked to remove can still be found. The result is a receipt in the vault
// that says what was done and what is left, and never contains the text.
//
// The rules are the bank's, applied across the vault:
//
//   - an agent's own entry that carries the text is redacted in place, or
//     removed when redaction would leave it carrying the text;
//   - a person's entry is never altered by an agent-initiated forget. It gets a
//     challenge entry that disputes it, and the original stays for the person
//     to settle. A human-initiated forget cascades fully;
//   - the target itself is removed outright, not struck through. A struck
//     bullet keeps its text, which is the one thing a cascade exists to stop;
//   - snapshot history is scrubbed of the forgotten lines, and bank layers are
//     handled by the bank engine.
//
// "Carries" is deliberately narrow. A text carries the forgotten information
// when it contains the forgotten text as whole words, or a run of at least five
// words of it that has three or more content words. A paraphrase is not caught
// by that rule; the vector check in verification reports one as residual
// instead of removing it, because a meaning-level match is a judgement and
// this path is not allowed to make judgements about a person's notes.

// receiptsDir is the vault folder receipts are written to. It is not a memory
// path, so receipts are indexed like any note; add it to .stignore to keep them
// off a synced device.
const receiptsDir = "Memory Receipts"

// forgottenMark replaces removed text where a line must stay.
const forgottenMark = "[forgotten]"

// paraphraseCosine is the similarity at which verification reports a vector
// hit as residual.
const paraphraseCosine = 0.9

// stopWords are removed before the content-word test for a fragment.
var stopWords = map[string]bool{"a": true, "an": true, "the": true, "is": true, "are": true,
	"was": true, "were": true, "to": true, "of": true, "in": true, "on": true, "at": true,
	"and": true, "or": true, "for": true, "with": true, "as": true, "by": true, "it": true,
	"its": true, "he": true, "she": true, "they": true, "his": true, "her": true, "that": true,
	"this": true, "be": true, "has": true, "have": true, "had": true, "from": true, "not": true,
	"no": true, "yes": true, "so": true, "but": true, "if": true, "than": true, "then": true}

// forgetMatch is the test for "carries the forgotten text", and the redaction
// that removes it. Both are one regular expression, so a redaction is exactly
// the thing that stopped matching.
type forgetMatch struct {
	re *regexp.Regexp
}

// newForgetMatch builds the matcher for one forgotten text. It refuses text too
// short to match safely: a three-word match would take half the store with it.
func newForgetMatch(text string) (*forgetMatch, bool) {
	words := strings.Fields(memory.Normalize(text))
	if len(strings.Join(words, " ")) < 12 || len(words) < 3 {
		return nil, false
	}
	const sep = `[^\p{L}\p{N}]+`
	phrase := func(ws []string) string {
		q := make([]string, len(ws))
		for i, w := range ws {
			q[i] = regexp.QuoteMeta(w)
		}
		return strings.Join(q, sep)
	}
	alts := []string{phrase(words)}
	if len(words) >= 6 {
		var grams []string
		for i := 0; i+5 <= len(words); i++ {
			content := 0
			for _, w := range words[i : i+5] {
				if !stopWords[w] && len([]rune(w)) >= 4 {
					content++
				}
			}
			if content >= 3 {
				grams = append(grams, phrase(words[i:i+5]))
			}
		}
		sort.SliceStable(grams, func(i, j int) bool { return len(grams[i]) > len(grams[j]) })
		alts = append(alts, grams...)
	}
	pat := `(?i)(^|[^\p{L}\p{N}])(?:` + strings.Join(alts, "|") + `)([^\p{L}\p{N}]|$)`
	return &forgetMatch{re: regexp.MustCompile(pat)}, true
}

func (m *forgetMatch) carries(text string) bool { return m.re.MatchString(text) }

// redact replaces each match with the mark, keeping the characters either side.
func (m *forgetMatch) redact(text string) string {
	return m.re.ReplaceAllString(text, "${1}"+forgottenMark+"${2}")
}

// redactLines is redact applied line by line, for a body that must keep its
// structure. A line that still carries after redaction is replaced whole.
func (m *forgetMatch) redactLines(body string) (string, bool) {
	lines := strings.Split(body, "\n")
	changed := false
	for i, ln := range lines {
		if !m.carries(ln) {
			continue
		}
		changed = true
		if r := m.redact(ln); !m.carries(r) {
			lines[i] = r
		} else {
			lines[i] = "- " + forgottenMark
		}
	}
	return strings.Join(lines, "\n"), changed
}

// forgetAction is one artefact a cascade touched or left.
type forgetAction struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`
	Path   string `json:"path,omitempty"`
	Action string `json:"action"`
}

// residualHit is something the verification still found.
type residualHit struct {
	Store  string `json:"store"`
	Path   string `json:"path,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Reason string `json:"reason"`
}

// forgetVerification is what re-searching the index found after the cascade.
type forgetVerification struct {
	Checked  []string      `json:"checked"`
	Vector   string        `json:"vector"`
	Residual []residualHit `json:"residual"`
	// Hidden counts residual hits in notes the caller cannot read. Their paths
	// are not disclosed, so the count is all that can be said.
	Hidden int  `json:"hidden"`
	OK     bool `json:"ok"`
}

type forgetIn struct {
	Path    string `json:"path"`
	ID      string `json:"id"`
	Agent   string `json:"agent,omitempty"`
	Hard    bool   `json:"hard,omitempty"`
	Cascade bool   `json:"cascade,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// forgetPost is POST /api/memory/forget. Without cascade it is the same
// operation as DELETE /api/memory/entry, and answers the same way.
func (s *Server) forgetPost(w http.ResponseWriter, r *http.Request) {
	var in forgetIn
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	note, id := normPath(in.Path), strings.TrimSpace(in.ID)
	if note == "" || id == "" {
		writeErr(w, http.StatusBadRequest, "path and id are required")
		return
	}
	if !index.IsMemoryPath(note) {
		writeErr(w, http.StatusBadRequest, "not a memory note")
		return
	}
	if in.Cascade {
		s.cascadeForget(w, r, note, id, in)
		return
	}
	s.forgetOne(w, r, note, id, in.Hard, in.Agent)
}

// isAgentInitiated reports whether the request came from an agent. A caller
// with no agent identity, or the command line, is the person; the same trust
// the attribution of a retraction already rests on.
func isAgentInitiated(r *http.Request) (string, bool) {
	name := agentFor(r)
	if name == "" || name == "cli" || name == "human" {
		return "", false
	}
	return name, true
}

// cascadeForget is the cascade. Dry runs compute the same plan and write
// nothing, not even a receipt.
func (s *Server) cascadeForget(w http.ResponseWriter, r *http.Request, note, id string, in forgetIn) {
	if !s.requireWrite(w, r, note) {
		return
	}
	existing, readErr := s.Vault.Read(note)
	var target memory.Entry
	found := false
	if readErr == nil {
		for _, e := range memory.Parse(existing.Body) {
			if e.ID == id {
				target, found = e, true
				break
			}
		}
	}
	if !found {
		// A repeat of a forget that already happened: nothing is left to find
		// by its text, so answer with the receipt that recorded it.
		if rc, ok := s.receiptFor(r, id); ok {
			writeJSON(w, http.StatusOK, map[string]any{"path": note, "id": id,
				"cascade": true, "already_forgotten": true, "receipt": rc.Receipt,
				"status": rc.Status})
			return
		}
		writeErr(w, http.StatusNotFound, "no such memory entry")
		return
	}
	agentName, agent := isAgentInitiated(r)
	if agent && target.HumanAuthored() {
		writeErr(w, http.StatusForbidden,
			"a person wrote this entry; an agent cannot cascade-forget it")
		return
	}
	match, ok := newForgetMatch(target.Text)
	if !ok {
		writeErr(w, http.StatusBadRequest,
			"the entry's text is too short to cascade by content; use a plain forget")
		return
	}
	who := "human"
	if agent {
		who = agentName
	}
	now := vault.Now()
	stamp := now.Format(memory.StampFormat)
	dry := in.DryRun
	var acts []forgetAction
	add := func(kind, ref, path, action string) {
		acts = append(acts, forgetAction{Kind: kind, Ref: ref, Path: path, Action: action})
	}

	// Memory entries, in every note the caller may write. An entry the caller
	// may not write is reported and left; verification counts it if it stays.
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            index.Filter{IncludePrivate: true, IgnoreACLs: true},
		IncludeSuperseded: true, IncludeExpired: true, Now: now,
		Limit: index.DefaultScanLimit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	pending := map[string]bool{}
	for _, h := range hits {
		if h.Challenges != "" {
			pending[h.Challenges] = true
		}
	}
	removed := map[string]bool{id: true}
	var changedNotes []string
	for _, h := range hits {
		if h.Note == note && h.ID == id {
			continue
		}
		carrier := match.carries(h.Text)
		relink := h.Superseded() && h.SupersededBy == id
		if !carrier && !relink {
			continue
		}
		if !s.canWrite(r, h.Note) {
			add("memory", h.ID, h.Note, "skipped_no_access")
			continue
		}
		humanAgent := h.HumanAuthored() && agent
		if relink && !humanAgent {
			add("memory", h.ID, h.Note, "relinked")
			if !dry {
				if err := s.mutateEntry(h.Note, h.ID, func(e *memory.Entry) {
					e.SupersededBy = "retracted:cascade"
					e.SupersededAt = stamp
				}); err != nil {
					writeErr(w, statusForEntryErr(err), entryErrMsg(err))
					return
				}
				changedNotes = append(changedNotes, h.Note)
			}
		}
		if !carrier {
			continue
		}
		if humanAgent {
			if pending[h.ID] {
				add("memory", h.ID, h.Note, "challenge_pending")
				continue
			}
			text := match.redact(h.Text)
			if match.carries(text) {
				text = forgottenMark
			}
			pending[h.ID] = true
			add("memory", h.ID, h.Note, "challenged")
			if !dry {
				c := memory.Entry{ID: memory.DeriveID(stamp, who, text), Text: text,
					Agent: who, Stamp: stamp, Challenges: h.ID}
				if err := s.appendLine(h.Note, c); err != nil {
					writeErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				changedNotes = append(changedNotes, h.Note)
			}
			continue
		}
		removed[h.ID] = true
		redacted := match.redact(h.Text)
		if redacted == forgottenMark || match.carries(redacted) || strings.TrimSpace(strings.ReplaceAll(redacted, forgottenMark, "")) == "" {
			add("memory", h.ID, h.Note, "removed")
			if !dry {
				if err := s.removeEntry(h.Note, h.ID); err != nil {
					writeErr(w, statusForEntryErr(err), entryErrMsg(err))
					return
				}
				changedNotes = append(changedNotes, h.Note)
			}
			continue
		}
		add("memory", h.ID, h.Note, "redacted")
		if !dry {
			if err := s.mutateEntry(h.Note, h.ID, func(e *memory.Entry) {
				human := e.HumanAuthored()
				e.Text = redacted
				if human {
					// A person's entry keeps its id and authority: the id no
					// longer matches its text, which would otherwise read as a
					// hand edit and be attributed to the wrong author.
					e.Human = true
				} else {
					e.ID = memory.DeriveID(e.Stamp, e.Agent, e.Text)
				}
			}); err != nil {
				writeErr(w, statusForEntryErr(err), entryErrMsg(err))
				return
			}
			changedNotes = append(changedNotes, h.Note)
		}
	}

	// The target: removed outright.
	add("memory", id, note, "removed")
	if !dry {
		if err := s.removeEntry(note, id); err != nil {
			writeErr(w, statusForEntryErr(err), entryErrMsg(err))
			return
		}
		changedNotes = append(changedNotes, note)
	}

	// Memory banks, through the bank engine.
	if s.Banks != nil {
		banks, err := s.Banks.ListBanks()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, b := range banks {
			bacts, err := s.Banks.CascadeForget(b.ID, match.carries, match.redact,
				bank.CascadeOptions{Agent: agent, DryRun: dry})
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			for _, a := range bacts {
				acts = append(acts, forgetAction{Kind: a.Kind, Ref: a.Ref, Path: a.Path, Action: a.Action})
			}
		}
	}

	// Other derived notes: dream reports quote what they found. Anything else
	// that carries the text is a note a person wrote, and is only reported.
	walked, err := s.Vault.Walk()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	reportDir := strings.Trim(s.dreamReportDir(), "/") + "/"
	for _, rel := range walked {
		if index.IsMemoryPath(rel) || index.IsBankPath(rel) || strings.HasPrefix(rel, receiptsDir+"/") ||
			!strings.HasPrefix(rel, reportDir) {
			continue
		}
		n, err := s.Vault.Read(rel)
		if err != nil {
			continue
		}
		out, changed := match.redactLines(n.Body)
		if !changed {
			continue
		}
		add("report", "", rel, "redacted")
		if !dry {
			s.snapshot(rel, n.Body)
			if _, err := s.Vault.Write(rel, out, n.Frontmatter.Clone()); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if _, err := s.Index.Upsert(rel); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			changedNotes = append(changedNotes, rel)
		}
	}

	resp := map[string]any{"path": note, "id": id, "cascade": true, "dry_run": dry,
		"initiator": who, "actions": acts}
	if dry {
		resp["status"] = "dry_run"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Snapshot history holds the pre-edit bodies, which still carry the text.
	scrubbed := 0
	if s.History != nil {
		n, err := s.History.Rewrite(func(_, body string) (string, bool) { return match.redactLines(body) })
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		scrubbed = n
	}
	if scrubbed > 0 {
		acts = append(acts, forgetAction{Kind: "history", Action: "scrubbed",
			Path: fmt.Sprintf("%d version(s)", scrubbed)})
	}
	if len(changedNotes) > 0 || scrubbed > 0 {
		// Vector rows of bank text are keyed by a hash of the embedded text,
		// which cannot be matched back to the forgotten one. Dropping the cache
		// costs a re-embed; keeping it could keep an embedding of the text.
		if err := s.Index.DB.Exec("DELETE FROM bank_vec_cache"); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.Index.InvalidateCache()
	s.profileMemo.reset()

	ver := s.verifyForget(r, match, target.Text, removed)
	receipt, err := s.writeReceipt(now, id, note, match, target.Text, who, acts, ver)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := "clean"
	if !ver.OK {
		status = "residual"
	}
	resp["actions"] = acts
	resp["verification"] = ver
	resp["receipt"] = receipt
	resp["status"] = status
	writeJSON(w, http.StatusOK, resp)
}

// snapshot keeps a note's previous body in history before a cascade edits it.
func (s *Server) snapshot(rel, body string) {
	if s.History != nil {
		s.History.Snapshot(rel, body)
	}
}

// appendLine adds one entry to the end of a memory note.
func (s *Server) appendLine(rel string, e memory.Entry) error {
	existing, err := s.Vault.Read(rel)
	if err != nil {
		return err
	}
	s.snapshot(rel, existing.Body)
	e.ID = uniqueID(memory.Parse(existing.Body), e.ID)
	body := memory.Append(existing.Body, e)
	if _, err := s.Vault.Write(rel, body, existing.Frontmatter.Clone()); err != nil {
		return err
	}
	_, err = s.Index.Upsert(rel)
	return err
}

// verifyForget searches the index again for what the cascade was asked to
// remove. It reads the same tables a recall reads, so a hit here is a hit a
// recall could return.
func (s *Server) verifyForget(r *http.Request, m *forgetMatch, text string, removed map[string]bool) forgetVerification {
	ver := forgetVerification{Checked: []string{}}
	var res []residualHit
	add := func(h residualHit) { res = append(res, h) }

	// Candidates are gathered while the index is being read and judged after:
	// the read-access check is itself a query, and the store has one connection.
	var found []residualHit
	emit := func(h residualHit) { found = append(found, h) }
	flush := func() {
		for _, h := range found {
			if h.Path != "" && !s.canRead(r, h.Path) {
				ver.Hidden++
				continue
			}
			add(h)
		}
	}

	// Memory entries, through the index's own query.
	ver.Checked = append(ver.Checked, "memory")
	all, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            index.Filter{IncludePrivate: true, IgnoreACLs: true},
		IncludeSuperseded: true, IncludeExpired: true, Now: vault.Now(),
		Limit: index.DefaultScanLimit,
	})
	if err == nil {
		for _, h := range all {
			if m.carries(h.Text) {
				emit(residualHit{Store: "memory", Path: h.Note, Ref: h.ID, Reason: "entry still carries the text"})
			}
		}
	}

	// Entity edges of removed entries.
	ver.Checked = append(ver.Checked, "entity")
	for id := range removed {
		rows, err := s.Index.DB.Query("SELECT note FROM memory_entities WHERE id=?", id)
		if err != nil {
			continue
		}
		for rows.Next() {
			var note string
			if rows.Scan(&note) == nil {
				emit(residualHit{Store: "entity", Path: note, Ref: id, Reason: "entity edge for a removed entry"})
			}
		}
		rows.Close()
	}

	// Full-text search over every note, then line-level confirmation.
	ver.Checked = append(ver.Checked, "fts")
	if q := ftsQuery(text); q != "" {
		rows, err := s.Index.DB.Query("SELECT path, body FROM fts WHERE fts MATCH ?", q)
		if err == nil {
			for rows.Next() {
				var path, body string
				if rows.Scan(&path, &body) == nil && lineCarries(m, body) {
					emit(residualHit{Store: "fts", Path: path, Reason: "note text still matches"})
				}
			}
			rows.Close()
		}
	}

	// Bank text: facts, observations, models and proposals in the index.
	ver.Checked = append(ver.Checked, "bank_fts")
	if q := ftsQuery(text); q != "" {
		rows, err := s.Index.DB.Query("SELECT bank_units.path, bank_units.id, bank_units.text FROM bank_units_fts "+
			"JOIN bank_units ON bank_units.rid = bank_units_fts.rowid WHERE bank_units_fts MATCH ?", q)
		if err == nil {
			for rows.Next() {
				var path, id, t string
				if rows.Scan(&path, &id, &t) == nil && m.carries(t) {
					emit(residualHit{Store: "bank_fts", Path: path, Ref: id, Reason: "bank unit still carries the text"})
				}
			}
			rows.Close()
		}
	}

	// Source documents a bank retained: reported, never edited.
	ver.Checked = append(ver.Checked, "bank_chunks")
	if rows, err := s.Index.DB.Query("SELECT path, doc, text FROM bank_chunks"); err == nil {
		for rows.Next() {
			var path, doc, t string
			if rows.Scan(&path, &doc, &t) == nil && m.carries(t) {
				emit(residualHit{Store: "bank_source", Path: path, Ref: doc,
					Reason: "source document still holds the text; the cascade does not edit sources"})
			}
		}
		rows.Close()
	}

	// Snapshot history.
	ver.Checked = append(ver.Checked, "history")
	if s.History != nil {
		s.History.Scan(func(rel, body string) {
			if lineCarries(m, body) {
				emit(residualHit{Store: "history", Path: rel, Reason: "snapshot still holds the text"})
			}
		})
	}

	// Vectors: a paraphrase the text rule cannot see is found by meaning.
	if s.Index.Emb == nil {
		ver.Vector = "not run: no embedder configured"
	} else {
		ver.Checked = append(ver.Checked, "vector")
		ver.Vector = "run"
		qv := s.Index.Emb.Embed([]string{text})
		if len(qv) == 1 && len(qv[0]) > 0 {
			q := qv[0]
			if rows, err := s.Index.DB.Query("SELECT note, chunk_idx, embedding FROM vectors WHERE embedding IS NOT NULL"); err == nil {
				for rows.Next() {
					var note string
					var idx int
					var blob []byte
					if rows.Scan(&note, &idx, &blob) == nil {
						if c := index.Cosine(q, index.Unpack(blob)); c >= paraphraseCosine {
							emit(residualHit{Store: "vector", Path: note, Ref: fmt.Sprint(idx),
								Reason: fmt.Sprintf("chunk is close in meaning to the text (cosine %.2f)", c)})
						}
					}
				}
				rows.Close()
			}
		}
	}

	flush()
	sort.SliceStable(res, func(i, j int) bool {
		if res[i].Store != res[j].Store {
			return res[i].Store < res[j].Store
		}
		return res[i].Path+res[i].Ref < res[j].Path+res[j].Ref
	})
	ver.Residual = dedupeHits(res)
	if ver.Residual == nil {
		ver.Residual = []residualHit{}
	}
	ver.OK = len(ver.Residual) == 0 && ver.Hidden == 0
	return ver
}

func dedupeHits(in []residualHit) []residualHit {
	seen := map[string]bool{}
	var out []residualHit
	for _, h := range in {
		k := h.Store + "\x00" + h.Path + "\x00" + h.Ref
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, h)
	}
	return out
}

// lineCarries reports whether any line of body carries the text.
func lineCarries(m *forgetMatch, body string) bool {
	for _, ln := range strings.Split(body, "\n") {
		if m.carries(ln) {
			return true
		}
	}
	return false
}

// ftsQuery turns the forgotten text's content words into an FTS5 conjunction.
// It is a candidate filter only; lineCarries confirms each row.
func ftsQuery(text string) string {
	var terms []string
	for _, w := range strings.Fields(memory.Normalize(text)) {
		if stopWords[w] || len([]rune(w)) < 3 {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, "")+`"`)
		if len(terms) == 8 {
			break
		}
	}
	return strings.Join(terms, " AND ")
}

// saltedHash identifies forgotten text without holding it.
func saltedHash(salt, text string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + memory.Normalize(text)))
	return hex.EncodeToString(sum[:])
}

// receiptDoc is the machine copy of a receipt. It holds no forgotten text:
// the target is identified by its id and a salted hash, and every artefact by
// its path and id.
type receiptDoc struct {
	ID           string             `json:"id"`
	TargetID     string             `json:"target_id"`
	TargetPath   string             `json:"target_path"`
	TargetHash   string             `json:"target_hash"`
	Salt         string             `json:"salt"`
	TargetTokens int                `json:"target_tokens"`
	Initiator    string             `json:"initiator"`
	At           string             `json:"at"`
	Actions      []forgetAction     `json:"actions"`
	Verification forgetVerification `json:"verification"`
	Status       string             `json:"status"`
}

// receiptRef is what the list endpoint returns.
type receiptRef struct {
	Receipt  string `json:"receipt"`
	ID       string `json:"id"`
	TargetID string `json:"target_id"`
	Path     string `json:"path"`
	At       string `json:"at"`
	Status   string `json:"status"`
	Residual int    `json:"residual"`
	Hidden   int    `json:"hidden"`
}

func (s *Server) receiptDir() string { return filepath.Join(s.Vault.Root, receiptsDir) }

// writeReceipt writes the markdown note and the JSON copy, and returns the
// vault path of the note.
func (s *Server) writeReceipt(now time.Time, targetID, note string, m *forgetMatch, text, who string,
	acts []forgetAction, ver forgetVerification) (string, error) {
	var sb [16]byte
	if _, err := rand.Read(sb[:]); err != nil {
		return "", err
	}
	salt := hex.EncodeToString(sb[:])
	status := "clean"
	if !ver.OK {
		status = "residual"
	}
	ts := now.Format("20060102-150405")
	base := "forget-" + targetID + "-" + ts
	if _, err := os.Stat(filepath.Join(s.receiptDir(), base+".json")); err == nil {
		base += "-" + salt[:4]
	}
	doc := receiptDoc{ID: base, TargetID: targetID, TargetPath: note,
		TargetHash: saltedHash(salt, text), Salt: salt,
		TargetTokens: len(strings.Fields(memory.Normalize(text))),
		Initiator:    who, At: now.Format(memory.StampFormat), Actions: acts,
		Verification: ver, Status: status}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.receiptDir(), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(s.receiptDir(), base+".json"), b, 0o600); err != nil {
		return "", err
	}
	rel := receiptsDir + "/" + base + ".md"
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Forget receipt "+targetID)
	fm.Set("receipt_for", targetID)
	fm.Set("status", status)
	if _, err := s.Vault.Write(rel, receiptMarkdown(doc), fm); err != nil {
		return "", err
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		return "", err
	}
	return rel, nil
}

// receiptMarkdown renders the human copy. Every line is derived from the
// receipt's structure; none of it is the forgotten text.
func receiptMarkdown(d receiptDoc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Forget receipt %s\n\n", d.TargetID)
	fmt.Fprintf(&b, "- **Status:** %s\n- **At:** %s\n- **Initiated by:** %s\n", d.Status, d.At, d.Initiator)
	fmt.Fprintf(&b, "- **Forgotten entry:** %s in `%s`\n", d.TargetID, d.TargetPath)
	fmt.Fprintf(&b, "- **Forgotten text:** %d word(s), salted sha256 `%s` (salt `%s`); the text is not kept here\n\n",
		d.TargetTokens, d.TargetHash, d.Salt)
	b.WriteString("## Actions\n\n")
	if len(d.Actions) == 0 {
		b.WriteString("None.\n\n")
	}
	for _, a := range d.Actions {
		fmt.Fprintf(&b, "- %s %s `%s` — %s\n", a.Kind, a.Ref, a.Path, a.Action)
	}
	b.WriteString("\n## Verification\n\n")
	fmt.Fprintf(&b, "Searched: %s. Vector: %s.\n\n", strings.Join(d.Verification.Checked, ", "), d.Verification.Vector)
	if len(d.Verification.Residual) == 0 && d.Verification.Hidden == 0 {
		b.WriteString("No residual hits.\n")
	}
	for _, h := range d.Verification.Residual {
		fmt.Fprintf(&b, "- residual [%s] `%s` %s — %s\n", h.Store, h.Path, h.Ref, h.Reason)
	}
	if d.Verification.Hidden > 0 {
		fmt.Fprintf(&b, "- %d residual hit(s) in notes the requester cannot read\n", d.Verification.Hidden)
	}
	return b.String()
}

// receiptFor finds the receipt of an earlier forget of id that the caller may read.
func (s *Server) receiptFor(r *http.Request, id string) (receiptRef, bool) {
	refs := s.listReceipts(r)
	for _, rc := range refs {
		if rc.TargetID == id {
			return rc, true
		}
	}
	return receiptRef{}, false
}

// listReceipts reads the receipt copies, newest first, keeping only those whose
// forgotten entry the caller may read.
func (s *Server) listReceipts(r *http.Request) []receiptRef {
	ents, err := os.ReadDir(s.receiptDir())
	if err != nil {
		return nil
	}
	var out []receiptRef
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.receiptDir(), e.Name()))
		if err != nil {
			continue
		}
		var d receiptDoc
		if json.Unmarshal(b, &d) != nil || d.ID == "" {
			continue
		}
		if r != nil && !s.canRead(r, d.TargetPath) {
			continue
		}
		out = append(out, receiptRef{Receipt: receiptsDir + "/" + d.ID + ".md", ID: d.ID,
			TargetID: d.TargetID, Path: d.TargetPath, At: d.At, Status: d.Status,
			Residual: len(d.Verification.Residual), Hidden: d.Verification.Hidden})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// GET /api/memory/receipts lists the forget receipts the caller can read.
func (s *Server) listReceiptsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.listReceipts(r))
}
