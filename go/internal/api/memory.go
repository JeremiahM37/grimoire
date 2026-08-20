package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/fts"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Agent memory — the substrate's agent-writable namespace.
//
// Agents persist what they learn under memory/ as ordinary markdown notes with
// provenance frontmatter (memory: true, agent, task). Because memories ARE
// notes, everything the console offers applies: read, edit, diff, roll back,
// link, search. That human-auditable loop — your agent's memory is a note you
// can open — is the point of the design.
//
// Writes RECONCILE rather than append. Appending every reported fact is what
// makes long-lived memory useless: "prefers tabs" and "prefers spaces" both
// end up on file, recall returns whichever ranks higher, and the agent acts on
// a belief its owner corrected months ago. Every write is therefore split into
// facts, each fact is checked against what is already known, and the outcome
// is one of ADD / UPDATE / DELETE / NOOP (see internal/memory).
//
// A superseded fact is struck through, never deleted. Deleting it would make
// the note agree with the database and lose the only record of what the agent
// used to believe — which is the question you actually ask when an agent has
// behaved strangely for a month.

// MemoryDir is the namespace agent memories live in.
const MemoryDir = memory.Dir

// agentRE keeps an agent name to something safe to display and store; it lands
// in frontmatter and in a provenance banner.
var agentRE = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}_ .:/-]{0,60}$`)

// scopeRE constrains the free-form scope fields a caller may set. They are
// matched exactly in SQL and rendered into the bullet trailer, so they must
// not contain the trailer's separators or anything that could close it.
var scopeRE = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}_.:/-]{0,60}$`)

// reconcileCandidates is how many existing facts a new one is checked against.
// Every candidate is a line in an LLM prompt on the write path, and the
// ranking that produces them already puts the contradiction first if there is
// one; past a dozen the extra lines cost latency without changing verdicts.
const reconcileCandidates = 12

type memoryIn struct {
	Text  string `json:"text"`
	Topic string `json:"topic"`
	Agent string `json:"agent"`
	Task  string `json:"task"`

	// Session is mem0's run_id: the task or conversation this was learned in,
	// so "what did this agent learn during that run" is answerable.
	Session  string `json:"session"`
	Category string `json:"category"`

	// Expires is an absolute RFC3339 instant; ExpiresIn is a duration from now
	// ("72h"), which is what a caller usually has. Either one makes the fact
	// stop being recalled without anyone having to remember to delete it.
	Expires   string `json:"expires"`
	ExpiresIn string `json:"expires_in"`

	// Immutable pins a fact: reconciliation may never supersede or retract it,
	// and it is never offered to the model as a target.
	Immutable bool `json:"immutable"`

	// Infer defaults to true. Setting it false stores the text verbatim as one
	// fact with no extraction and no reconciliation — the escape hatch for a
	// caller that has already decided what it wants on file.
	Infer *bool `json:"infer"`

	// Scope bounds which existing facts this write may supersede. The default
	// is the whole vault, which is right for one person's memory: a belief
	// contradicted in another note is still contradicted.
	//
	// It is wrong the moment memory is partitioned. A store that hands each
	// user or each agent a namespace needs a write in one namespace to be
	// unable to strike through a fact in another — and without this it could,
	// because reconciliation ranked across everything the caller may read.
	// One of: "" / "vault", "topic", "session", "agent".
	Scope string `json:"scope"`
}

// Reconciliation scopes.
const (
	scopeVault   = "vault"
	scopeTopic   = "topic"
	scopeSession = "session"
	scopeAgent   = "agent"
)

func validScope(s string) bool {
	switch s {
	case "", scopeVault, scopeTopic, scopeSession, scopeAgent:
		return true
	}
	return false
}

func (s *Server) memoryRel(topic string) string {
	slug := vault.Now().Format("2006-01-02")
	if strings.TrimSpace(topic) != "" {
		slug = vault.Slugify(topic)
	}
	return MemoryDir + "/" + slug + ".md"
}

// memoryResult is what one fact did.
type memoryResult struct {
	Op     string `json:"op"`
	ID     string `json:"id,omitempty"`
	Text   string `json:"text,omitempty"`
	Target string `json:"target,omitempty"`
	Path   string `json:"path,omitempty"`
	Why    string `json:"why,omitempty"`
}

// remember records what an agent learned, reconciling each fact against what
// is already on file.
func (s *Server) remember(w http.ResponseWriter, r *http.Request) {
	var m memoryIn
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	s.rememberOne(w, r, m)
}

func (s *Server) rememberOne(w http.ResponseWriter, r *http.Request, m memoryIn) {
	text := strings.TrimSpace(m.Text)
	if text == "" || len(text) > 20000 {
		writeErr(w, http.StatusBadRequest, "text must be 1..20000 characters")
		return
	}
	agent := strings.TrimSpace(m.Agent)
	if agent == "" {
		agent = "agent"
	}
	if !agentRE.MatchString(agent) {
		writeErr(w, http.StatusBadRequest, "invalid agent name")
		return
	}
	for name, v := range map[string]string{"session": m.Session, "category": m.Category} {
		if v != "" && !scopeRE.MatchString(v) {
			writeErr(w, http.StatusBadRequest, "invalid "+name)
			return
		}
	}
	if !validScope(strings.TrimSpace(m.Scope)) {
		writeErr(w, http.StatusBadRequest,
			"scope must be one of vault, topic, session, agent")
		return
	}
	expires, err := resolveExpiry(m.Expires, m.ExpiresIn)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	task := strings.TrimSpace(m.Task)
	rel := s.memoryRel(m.Topic)
	// The topic decides the destination, and the topic comes from the caller.
	if !s.requireWrite(w, r, normPath(rel)) {
		return
	}

	facts := []string{text}
	if m.Infer == nil || *m.Infer {
		facts = s.AI.ExtractFacts(text)
	}

	_, readErr := s.Vault.Read(rel)
	created := readErr != nil

	results := make([]memoryResult, 0, len(facts))
	for _, fact := range facts {
		fact = strings.TrimSpace(fact)
		if fact == "" {
			continue
		}
		res, err := s.reconcileFact(w, r, rel, fact, agent, task, m, expires)
		if err != nil {
			return // reconcileFact already answered
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		writeErr(w, http.StatusBadRequest, "text must be 1..20000 characters")
		return
	}

	// The first result is the headline for callers that want one line back.
	head := results[0]
	writeJSON(w, http.StatusCreated, map[string]any{
		"path": rel, "created": created, "entry": head.Text,
		"op": head.Op, "id": head.ID, "results": results,
	})
}

// reconcileFact decides what one fact does and applies it. A non-nil error
// means the response has already been written.
func (s *Server) reconcileFact(w http.ResponseWriter, r *http.Request, rel, fact,
	agent, task string, m memoryIn, expires string) (memoryResult, error) {

	decision := memory.Decision{Op: memory.OpAdd, Text: fact, Why: "reconciliation disabled"}
	if m.Infer == nil || *m.Infer {
		query := index.MemoryQuery{
			Filter: filterFor(r, true),
			Query:  fact,
			Limit:  reconcileCandidates,
			Now:    vault.Now(),
		}
		switch strings.TrimSpace(m.Scope) {
		case scopeTopic:
			query.Note = normPath(rel)
		case scopeSession:
			// A write with no session under session scope can only supersede
			// other sessionless facts, which is the same rule applied
			// consistently rather than a special case.
			query.Session = strings.TrimSpace(m.Session)
			query.SessionSet = true
		case scopeAgent:
			query.Agent = agent
		}
		hits, err := s.Index.MemoryEntries(query)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return memoryResult{}, err
		}
		candidates := make([]memory.Entry, 0, len(hits))
		byID := make(map[string]index.MemoryHit, len(hits))
		for _, h := range hits {
			candidates = append(candidates, h.Entry)
			byID[h.ID] = h
		}
		decision = s.AI.DecideMemory(fact, candidates)
		if decision.Target != "" {
			target, ok := byID[decision.Target]
			if !ok {
				// A verdict about a fact that is not in the candidate set
				// cannot be applied; keeping the fact is the safe outcome.
				decision = memory.Decision{Op: memory.OpAdd, Text: fact,
					Why: "target not found; kept"}
			} else if decision.Op == memory.OpUpdate || decision.Op == memory.OpDelete {
				res, err := s.applySupersession(w, r, target, decision, fact,
					agent, task, m, expires, rel)
				return res, err
			}
		}
	}
	switch decision.Op {
	case memory.OpNoop:
		return memoryResult{Op: string(memory.OpNoop), Target: decision.Target,
			Text: fact, Why: decision.Why}, nil
	default:
		id, err := s.appendEntry(w, r, rel, fact, agent, task, m, expires, "")
		if err != nil {
			return memoryResult{}, err
		}
		return memoryResult{Op: string(memory.OpAdd), ID: id, Text: fact,
			Path: rel, Why: decision.Why}, nil
	}
}

// applySupersession marks the target struck through and, for an UPDATE, writes
// the replacement. The replacement is written FIRST so that a failure between
// the two writes leaves the old belief standing rather than leaving the agent
// with no belief at all.
func (s *Server) applySupersession(w http.ResponseWriter, r *http.Request,
	target index.MemoryHit, d memory.Decision, fact, agent, task string,
	m memoryIn, expires, rel string) (memoryResult, error) {

	if target.Immutable {
		// Belt and braces: the rule path and the model path both exclude
		// immutable entries, so reaching here means a bug, not a request.
		id, err := s.appendEntry(w, r, rel, fact, agent, task, m, expires, "")
		if err != nil {
			return memoryResult{}, err
		}
		return memoryResult{Op: string(memory.OpAdd), ID: id, Text: fact,
			Path: rel, Why: "target is immutable; kept both"}, nil
	}
	// Superseding is a write to the note the OLD fact lives in, which is not
	// necessarily the note being written to.
	if !s.requireWrite(w, r, normPath(target.Note)) {
		return memoryResult{}, errHandled
	}

	newID := ""
	if d.Op == memory.OpUpdate {
		var err error
		newID, err = s.appendEntry(w, r, rel, fact, agent, task, m, expires, "")
		if err != nil {
			return memoryResult{}, err
		}
	} else {
		// A retraction has no replacement, so the tombstone points at the
		// agent that retracted it rather than at another fact.
		newID = "retracted:" + agent
	}
	if err := s.mutateEntry(target.Note, target.ID, func(e *memory.Entry) {
		e.SupersededBy = newID
		e.SupersededAt = vault.Now().Format(memory.StampFormat)
	}); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return memoryResult{}, err
	}
	op := string(d.Op)
	return memoryResult{Op: op, ID: newID, Text: d.Text, Target: target.ID,
		Path: rel, Why: d.Why}, nil
}

// errHandled marks a path that has already written its own response.
var errHandled = &handledError{}

type handledError struct{}

func (*handledError) Error() string { return "request already answered" }

// appendEntry writes one new fact into a memory note, creating it on first
// use. It returns the new entry's id.
func (s *Server) appendEntry(w http.ResponseWriter, r *http.Request, rel, fact,
	agent, task string, m memoryIn, expires, supersededBy string) (string, error) {

	stamp := vault.Now().Format("2006-01-02 15:04")
	e := memory.Entry{
		ID: memory.DeriveID(stamp, agent, fact), Text: fact, Agent: agent,
		Task: task, Session: strings.TrimSpace(m.Session), Stamp: stamp,
		Category: strings.TrimSpace(m.Category), Expires: expires,
		Immutable: m.Immutable, SupersededBy: supersededBy,
	}

	existing, readErr := s.Vault.Read(rel)
	var fm *markdown.Frontmatter
	var body string
	if readErr == nil {
		// agent writes are rollbackable like any other edit
		s.History.Snapshot(rel, existing.Body)
		fm = existing.Frontmatter.Clone()
		fm.Set("agent", agent) // most recent writer…
		if task != "" {
			fm.Set("task", task) // …and their task, kept together
		}
		// Two facts written in the same minute by the same agent with the same
		// wording would derive the same id; disambiguate rather than letting
		// one shadow the other.
		e.ID = uniqueID(memory.Parse(existing.Body), e.ID)
		body = memory.Append(existing.Body, e)
	} else {
		title := strings.TrimSpace(m.Topic)
		if title == "" {
			title = vault.Now().Format("2006-01-02")
		}
		fm = markdown.NewFrontmatter()
		fm.Set("title", "Memory: "+title)
		fm.Set("memory", true)
		fm.Set("agent", agent)
		if task != "" {
			fm.Set("task", task)
		}
		body = "# Memory: " + title + "\n\n" + e.Format() + "\n"
	}
	if _, err := s.Vault.Write(rel, body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return "", err
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return "", err
	}
	return e.ID, nil
}

func uniqueID(existing []memory.Entry, id string) string {
	taken := make(map[string]bool, len(existing))
	for _, e := range existing {
		taken[e.ID] = true
	}
	out := id
	for i := 1; taken[out]; i++ {
		out = id + "-" + strconv.Itoa(i)
	}
	return out
}

// mutateEntry edits one entry in place, preserving every other line of the
// note. The caller has already checked that the principal may write it.
func (s *Server) mutateEntry(note, id string, mutate func(*memory.Entry)) error {
	existing, err := s.Vault.Read(note)
	if err != nil {
		return err
	}
	entries := memory.Parse(existing.Body)
	var edited []memory.Entry
	for _, e := range entries {
		if e.ID != id {
			continue
		}
		mutate(&e)
		edited = append(edited, e)
	}
	if len(edited) == 0 {
		return sql.ErrNoRows
	}
	s.History.Snapshot(note, existing.Body)
	body := memory.Rewrite(existing.Body, edited)
	if _, err := s.Vault.Write(note, body, existing.Frontmatter.Clone()); err != nil {
		return err
	}
	_, err = s.Index.Upsert(note)
	return err
}

// resolveExpiry turns either form of time-to-live into a stored instant.
func resolveExpiry(absolute, relative string) (string, error) {
	absolute, relative = strings.TrimSpace(absolute), strings.TrimSpace(relative)
	if absolute != "" && relative != "" {
		return "", errBadExpiry("give expires or expires_in, not both")
	}
	if absolute != "" {
		t, err := time.Parse(time.RFC3339, absolute)
		if err != nil {
			return "", errBadExpiry("expires must be RFC3339")
		}
		return t.UTC().Format(time.RFC3339), nil
	}
	if relative == "" {
		return "", nil
	}
	d, err := time.ParseDuration(relative)
	if err != nil {
		return "", errBadExpiry("expires_in must be a duration like 72h")
	}
	if d <= 0 {
		return "", errBadExpiry("expires_in must be positive")
	}
	return vault.Now().Add(d).UTC().Format(time.RFC3339), nil
}

type expiryError string

func (e expiryError) Error() string { return string(e) }

func errBadExpiry(msg string) error { return expiryError(msg) }

type memoryOut struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Updated string `json:"updated"`
	Body    string `json:"body"`
}

// entryOut is one recalled fact.
type entryOut struct {
	ID           string  `json:"id"`
	Text         string  `json:"text"`
	Path         string  `json:"path"`
	Agent        string  `json:"agent,omitempty"`
	Task         string  `json:"task,omitempty"`
	Session      string  `json:"session,omitempty"`
	Category     string  `json:"category,omitempty"`
	Stamp        string  `json:"stamp,omitempty"`
	Expires      string  `json:"expires,omitempty"`
	Immutable    bool    `json:"immutable,omitempty"`
	SupersededBy string  `json:"superseded_by,omitempty"`
	Helpful      int     `json:"helpful,omitempty"`
	Unhelpful    int     `json:"unhelpful,omitempty"`
	Score        float64 `json:"score"`

	// Why this fact was recalled, for the surface that has to justify it.
	Scores *scoreBreakdown `json:"scores,omitempty"`
}

type scoreBreakdown struct {
	Semantic float64 `json:"semantic"`
	Keyword  float64 `json:"keyword"`
	Entity   float64 `json:"entity"`
	Recency  float64 `json:"recency"`
	Useful   float64 `json:"useful"`
}

func entriesOut(hits []index.MemoryHit, explain bool) []entryOut {
	out := make([]entryOut, 0, len(hits))
	for _, h := range hits {
		e := entryOut{
			ID: h.ID, Text: h.Text, Path: h.Note, Agent: h.Agent, Task: h.Task,
			Session: h.Session, Category: h.Category, Stamp: h.Stamp,
			Expires: h.Expires, Immutable: h.Immutable,
			SupersededBy: h.SupersededBy, Helpful: h.Helpful,
			Unhelpful: h.Unhelpful, Score: h.Score,
		}
		if explain {
			e.Scores = &scoreBreakdown{Semantic: h.Semantic, Keyword: h.Keyword,
				Entity: h.Entity, Recency: h.Recency, Useful: h.Useful}
		}
		out = append(out, e)
	}
	return out
}

// recall returns remembered facts, ranked. Fact-level rather than note-level:
// a memory note accumulates dozens of unrelated facts, so returning whole
// notes hands an agent a page to find one line in, and ranks every fact in the
// note as though it were about the query.
//
// shape=notes returns the older note-level view, which is what the console
// reads when it wants to show the file rather than the belief.
func (s *Server) recall(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if r.URL.Query().Get("shape") == "notes" {
		s.recallNotes(w, r, q)
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 20, 200)
	// as_of asks what was believed at an instant, rather than what is believed
	// now. A malformed instant is refused rather than quietly ignored: a
	// historical query silently answered about the present is a wrong answer
	// that looks right.
	var asOf time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("as_of")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "as_of must be RFC3339")
			return
		}
		asOf = t
	}
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            filterFor(r, true),
		Query:             q,
		Agent:             strings.TrimSpace(r.URL.Query().Get("agent")),
		Task:              strings.TrimSpace(r.URL.Query().Get("task")),
		Session:           strings.TrimSpace(r.URL.Query().Get("session")),
		Category:          strings.TrimSpace(r.URL.Query().Get("category")),
		Note:              normPath(r.URL.Query().Get("path")),
		IncludeSuperseded: boolParam(r, "include_superseded"),
		IncludeExpired:    boolParam(r, "include_expired"),
		AsOf:              asOf,
		Now:               vault.Now(),
		Limit:             limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entriesOut(hits, boolParam(r, "explain")))
}

func boolParam(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func clampLimit(raw string, def, max int) int {
	limit := def
	if n, err := strconv.Atoi(raw); err == nil {
		limit = n
	}
	if limit < 1 {
		limit = 1
	}
	if limit > max {
		limit = max
	}
	return limit
}

// recallNotes is the note-level view: whole memory notes, most recently
// touched first, or matching a query.
func (s *Server) recallNotes(w http.ResponseWriter, r *http.Request, q string) {
	limit := clampLimit(r.URL.Query().Get("limit"), 20, 100)
	like := MemoryDir + "/%"

	var out []memoryOut
	if q != "" {
		// Memories are ordinary notes, so they live in spaces like any other
		// and one member must not recall another's.
		where, spaceArgs := s.whereReadable(r, "n.space", "n.acl",
			" WHERE n.path LIKE ? AND n.path IN (SELECT path FROM fts WHERE fts MATCH ?)")
		args := append(append([]any{like, fts.Terms(q)}, spaceArgs...), limit)
		rows, err := s.Index.DB.Query(
			"SELECT n.path, n.title, n.body, n.updated FROM notes n"+where+
				" ORDER BY n.updated DESC, n.path LIMIT ?", args...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, err = scanMemories(rows)
		rows.Close()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		if len(out) == 0 {
			// exact terms missed — fall back to semantic retrieval over the
			// memory namespace so paraphrased recalls still land
			hits, err := s.Index.RetrieveFor(q, limit*3, filterFor(r, true))
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			seen := map[string]bool{}
			for _, h := range hits {
				if !strings.HasPrefix(h.Path, MemoryDir+"/") || seen[h.Path] {
					continue
				}
				seen[h.Path] = true
				if len(out) >= limit {
					break
				}
				var m memoryOut
				if err := s.Index.DB.QueryRow(
					"SELECT path, title, body, updated FROM notes WHERE path=?", h.Path,
				).Scan(&m.Path, &m.Title, &m.Body, &m.Updated); err == nil {
					out = append(out, m)
				}
			}
		}
	} else {
		where, spaceArgs := s.whereReadable(r, "space", "acl", " WHERE path LIKE ?")
		args := append(append([]any{like}, spaceArgs...), limit)
		rows, err := s.Index.DB.Query(
			"SELECT path, title, body, updated FROM notes"+where+
				" ORDER BY updated DESC, path LIMIT ?", args...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, err = scanMemories(rows)
		rows.Close()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if out == nil {
		out = []memoryOut{}
	}
	writeJSON(w, http.StatusOK, out)
}

// scanMemories drains a memory query. It takes the concrete *sql.Rows rather
// than a Next/Scan interface so that Err() — the only way to tell "no more
// rows" apart from "iteration failed" — is reachable.
func scanMemories(rows *sql.Rows) ([]memoryOut, error) {
	var out []memoryOut
	for rows.Next() {
		var m memoryOut
		if err := rows.Scan(&m.Path, &m.Title, &m.Body, &m.Updated); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// briefing is the "read this first" pack for an agent joining a session:
// pinned notes, onboarding-tagged notes, and the most recent memories, in one
// call — standing context arrives unprompted instead of being hunted for.
func (s *Server) briefing(w http.ResponseWriter, r *http.Request) {
	n := 5
	if v := r.URL.Query().Get("memories"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 {
			n = k
		}
	}
	collect := func(q string, args ...any) ([]map[string]string, error) {
		out := []map[string]string{}
		rows, err := s.Index.DB.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var path, title, body, acl string
			if err := rows.Scan(&path, &title, &body, &acl); err != nil {
				return nil, err
			}
			// The briefing is standing context handed to an agent before it
			// asks for anything, so it is the surface most likely to leak a
			// space quietly. Every bucket goes through this one filter.
			// canReadNote, not canRead: this loop holds an open cursor, and
			// canRead reads the note's reader list from the database — which
			// on a single connection waits for the cursor to finish and never
			// returns. The briefing hung on exactly that.
			if !s.canReadNote(r, path, acl) {
				continue
			}
			out = append(out, map[string]string{"path": path, "title": title, "body": body})
		}
		return out, rows.Err()
	}
	// Python stores frontmatter JSON with a space after the colon, so the LIKE
	// pattern must match that shape exactly or no note ever looks pinned.
	// A briefing that silently drops a bucket is worse than one that fails:
	// the agent cannot tell "nothing is pinned" from "the pinned query broke".
	pinned, err := collect(
		"SELECT path, title, body, acl FROM notes WHERE private=0 " +
			"AND frontmatter_json LIKE '%\"pinned\": true%' ORDER BY updated DESC, path LIMIT 10")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	onboarding, err := collect(
		"SELECT n.path, n.title, n.body, n.acl FROM notes n JOIN tags t ON t.note=n.path " +
			"WHERE t.tag='onboarding' AND n.private=0 ORDER BY n.updated DESC, n.path LIMIT 10")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	recent, err := collect(
		"SELECT path, title, body, acl FROM notes WHERE path LIKE ? AND private=0 "+
			"ORDER BY updated DESC, path LIMIT ?", MemoryDir+"/%", n)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// A note appearing in more than one bucket is listed once, in the first
	// bucket that claims it — the briefing is a reading list, not a join.
	seen := map[string]bool{}
	dedupe := func(rows []map[string]string) []map[string]string {
		out := []map[string]string{}
		for _, r := range rows {
			if seen[r["path"]] {
				continue
			}
			seen[r["path"]] = true
			out = append(out, r)
		}
		return out
	}
	// The facts themselves, not just the notes holding them: an agent joining
	// a session needs the beliefs that are current, and a note-shaped bucket
	// hands it every belief the note ever held, including the superseded ones.
	facts, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter: filterFor(r, false), Limit: n, Now: vault.Now()})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pinned":          dedupe(pinned),
		"onboarding":      dedupe(onboarding),
		"recent_memories": dedupe(recent),
		"recent_facts":    entriesOut(facts, false),
	})
}

// Editing one fact, rather than the note it lives in.
//
// The console can already edit a memory note as markdown, which is the human
// path and stays. These endpoints are the agent path: address a fact by id,
// change one property of it, and leave every other line — including the prose
// a person wrote around it — byte-for-byte alone.

type entryPatch struct {
	Path      string  `json:"path"`
	ID        string  `json:"id"`
	Text      *string `json:"text"`
	Category  *string `json:"category"`
	Expires   *string `json:"expires"`
	ExpiresIn *string `json:"expires_in"`
	Immutable *bool   `json:"immutable"`
}

// patchEntry changes one property of one fact.
func (s *Server) patchEntry(w http.ResponseWriter, r *http.Request) {
	var in entryPatch
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	note := normPath(in.Path)
	if note == "" || strings.TrimSpace(in.ID) == "" {
		writeErr(w, http.StatusBadRequest, "path and id are required")
		return
	}
	if !index.IsMemoryPath(note) {
		writeErr(w, http.StatusBadRequest, "not a memory note")
		return
	}
	if !s.requireWrite(w, r, note) {
		return
	}
	if in.Category != nil && *in.Category != "" && !scopeRE.MatchString(*in.Category) {
		writeErr(w, http.StatusBadRequest, "invalid category")
		return
	}
	var expires string
	if in.Expires != nil || in.ExpiresIn != nil {
		var err error
		expires, err = resolveExpiry(deref(in.Expires), deref(in.ExpiresIn))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var out memory.Entry
	err := s.mutateEntry(note, strings.TrimSpace(in.ID), func(e *memory.Entry) {
		// An immutable fact is pinned against reconciliation, not against its
		// owner: a person editing it deliberately is the one case that should
		// go through, so the pin is checked by reconciliation and not here.
		if in.Text != nil && strings.TrimSpace(*in.Text) != "" {
			e.Text = strings.TrimSpace(*in.Text)
		}
		if in.Category != nil {
			e.Category = strings.TrimSpace(*in.Category)
		}
		if in.Expires != nil || in.ExpiresIn != nil {
			e.Expires = expires
		}
		if in.Immutable != nil {
			e.Immutable = *in.Immutable
		}
		out = *e
	})
	if err != nil {
		writeErr(w, statusForEntryErr(err), entryErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": note, "entry": entryOf(out, note)})
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func entryOf(e memory.Entry, note string) entryOut {
	return entryOut{ID: e.ID, Text: e.Text, Path: note, Agent: e.Agent, Task: e.Task,
		Session: e.Session, Category: e.Category, Stamp: e.Stamp, Expires: e.Expires,
		Immutable: e.Immutable, SupersededBy: e.SupersededBy}
}

// forgetEntry retracts one fact. By default it is struck through and kept, the
// same as any belief a later fact replaced — the record of what an agent
// believed survives the belief. hard=1 removes the bullet entirely, for the
// case where the fact itself is the problem and "keep it, but struck through"
// is not an answer; the pre-edit body is snapshotted either way, so even a
// hard forget is one rollback from recoverable until history rotates.
func (s *Server) forgetEntry(w http.ResponseWriter, r *http.Request) {
	note := normPath(r.URL.Query().Get("path"))
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if note == "" || id == "" {
		writeErr(w, http.StatusBadRequest, "path and id are required")
		return
	}
	if !index.IsMemoryPath(note) {
		writeErr(w, http.StatusBadRequest, "not a memory note")
		return
	}
	if !s.requireWrite(w, r, note) {
		return
	}
	if boolParam(r, "hard") {
		if err := s.removeEntry(note, id); err != nil {
			writeErr(w, statusForEntryErr(err), entryErrMsg(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": note, "id": id, "forgotten": true})
		return
	}
	who := strings.TrimSpace(r.URL.Query().Get("agent"))
	if who == "" {
		who = "human"
	}
	if !agentRE.MatchString(who) {
		writeErr(w, http.StatusBadRequest, "invalid agent name")
		return
	}
	if err := s.mutateEntry(note, id, func(e *memory.Entry) {
		e.SupersededBy = "retracted:" + who
		e.SupersededAt = vault.Now().Format(memory.StampFormat)
	}); err != nil {
		writeErr(w, statusForEntryErr(err), entryErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": note, "id": id, "retracted": true})
}

// removeEntry deletes the bullet outright.
func (s *Server) removeEntry(note, id string) error {
	existing, err := s.Vault.Read(note)
	if err != nil {
		return err
	}
	lines := strings.Split(existing.Body, "\n")
	kept := make([]string, 0, len(lines))
	found := false
	for _, ln := range lines {
		if e, ok := memory.ParseLine(ln); ok && e.ID == id {
			found = true
			continue
		}
		kept = append(kept, ln)
	}
	if !found {
		return sql.ErrNoRows
	}
	s.History.Snapshot(note, existing.Body)
	if _, err := s.Vault.Write(note, strings.Join(kept, "\n"),
		existing.Frontmatter.Clone()); err != nil {
		return err
	}
	_, err = s.Index.Upsert(note)
	return err
}

func statusForEntryErr(err error) int {
	if err == sql.ErrNoRows {
		return http.StatusNotFound
	}
	return statusForVaultErr(err)
}

func entryErrMsg(err error) string {
	if err == sql.ErrNoRows {
		return "no such memory entry"
	}
	return err.Error()
}

// feedback records whether a recalled fact earned its place.
//
// The obvious objection to a feedback endpoint is that it is a lever on
// ranking pointed at your own memory. The answer here is that it writes to the
// note the fact lives in, so it takes a WRITE check on that note — a member
// cannot vote down a fact in a space they can only read, and the same reader
// lists and spaces that govern everything else govern this. What is left is a
// person adjusting their own memory, which is the point.
//
// The effect is bounded and saturating (see memory.Entry.Usefulness): feedback
// reorders facts that already rank close together and cannot bury one that is
// the only answer to a question. A signal that could would be a way to lose
// information by clicking a button.
func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path    string `json:"path"`
		ID      string `json:"id"`
		Helpful *bool  `json:"helpful"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	note := normPath(in.Path)
	if note == "" || strings.TrimSpace(in.ID) == "" || in.Helpful == nil {
		writeErr(w, http.StatusBadRequest, "path, id and helpful are required")
		return
	}
	if !index.IsMemoryPath(note) {
		writeErr(w, http.StatusBadRequest, "not a memory note")
		return
	}
	if !s.requireWrite(w, r, note) {
		return
	}
	var out memory.Entry
	err := s.mutateEntry(note, strings.TrimSpace(in.ID), func(e *memory.Entry) {
		if *in.Helpful {
			e.Helpful++
		} else {
			e.Unhelpful++
		}
		out = *e
	})
	if err != nil {
		writeErr(w, statusForEntryErr(err), entryErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": note, "id": out.ID, "helpful": out.Helpful,
		"unhelpful": out.Unhelpful, "usefulness": out.Usefulness(),
	})
}

// rememberBatch records several memories in one call. Each is reconciled in
// turn against everything written before it, so a batch behaves exactly as the
// same writes made one at a time — a batch that contradicts itself ends with
// the last fact standing rather than with both.
func (s *Server) rememberBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Items []memoryIn `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(in.Items) == 0 {
		writeErr(w, http.StatusBadRequest, "items must not be empty")
		return
	}
	if len(in.Items) > 100 {
		writeErr(w, http.StatusBadRequest, "at most 100 items per batch")
		return
	}
	out := make([]map[string]any, 0, len(in.Items))
	worst := 0
	failed := 0
	for _, item := range in.Items {
		rec := &captureWriter{ResponseWriter: w}
		s.rememberOne(rec, r, item)
		var body map[string]any
		if err := json.Unmarshal(rec.buf.Bytes(), &body); err != nil {
			body = map[string]any{"error": rec.buf.String()}
		}
		body["status"] = rec.status
		if rec.status >= 400 {
			failed++
			if rec.status > worst {
				worst = rec.status
			}
		}
		out = append(out, body)
	}
	// A batch where nothing was written is not a success, whatever the
	// per-item detail says: a caller that checks only the HTTP status would
	// otherwise record a write that never happened. A PARTIAL failure stays
	// 200 — some facts did land, and the per-item statuses say which.
	status := http.StatusOK
	if failed == len(in.Items) {
		status = worst
	}
	writeJSON(w, status, map[string]any{
		"results": out, "written": len(in.Items) - failed, "failed": failed})
}

// exportMemory hands back every fact the caller may read, in one document.
// mem0 has this because a memory store you cannot get your data out of is a
// lock-in; here the markdown was always the export, and this is the shape a
// program wants rather than the shape a person reads.
func (s *Server) exportMemory(w http.ResponseWriter, r *http.Request) {
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            filterFor(r, true),
		Agent:             strings.TrimSpace(r.URL.Query().Get("agent")),
		Session:           strings.TrimSpace(r.URL.Query().Get("session")),
		Category:          strings.TrimSpace(r.URL.Query().Get("category")),
		IncludeSuperseded: true,
		IncludeExpired:    true,
		Now:               vault.Now(),
		Limit:             1 << 30,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":    len(hits),
		"exported": vault.Now().UTC().Format(time.RFC3339),
		"entries":  entriesOut(hits, false),
	})
}

// embedText hands back vectors from THIS server's embedding model.
//
// It exists so a framework that owns its embedding step can put its vectors in
// the same space as the stored ones. That is the whole reason a vector search
// is safe to offer: a cosine between vectors from two different models is a
// number with no meaning, and a memory store that accepts foreign vectors
// returns confident nonsense. Embedding here and searching here is one space
// by construction.
func (s *Server) embedText(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Texts []string `json:"texts"`
		Text  string   `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	texts := in.Texts
	if len(texts) == 0 && strings.TrimSpace(in.Text) != "" {
		texts = []string{in.Text}
	}
	if len(texts) == 0 {
		writeErr(w, http.StatusBadRequest, "texts must not be empty")
		return
	}
	if len(texts) > 256 {
		writeErr(w, http.StatusBadRequest, "at most 256 texts per call")
		return
	}
	vecs := s.Index.Emb.Embed(texts)
	out := make([][]float32, len(vecs))
	copy(out, vecs)
	writeJSON(w, http.StatusOK, map[string]any{
		// The signature identifies the space. A caller that caches vectors can
		// tell when the model changed underneath them, which is the failure
		// that otherwise shows up as retrieval quietly getting worse.
		"model":      s.Index.Emb.Signature(),
		"dimensions": s.Index.Emb.Dim(),
		"embeddings": out,
	})
}

// searchMemoryByVector ranks memory against a vector the caller computed.
func (s *Server) searchMemoryByVector(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Embedding         []float32 `json:"embedding"`
		Query             string    `json:"query"`
		Path              string    `json:"path"`
		Agent             string    `json:"agent"`
		Task              string    `json:"task"`
		Session           string    `json:"session"`
		Category          string    `json:"category"`
		Limit             int       `json:"limit"`
		IncludeSuperseded bool      `json:"include_superseded"`
		IncludeExpired    bool      `json:"include_expired"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(in.Embedding) == 0 && strings.TrimSpace(in.Query) == "" {
		writeErr(w, http.StatusBadRequest, "embedding or query is required")
		return
	}
	// The dimension check is what enforces "one embedding space". A vector
	// from another model would otherwise be scored against these, and cosine
	// does not report that it is comparing two unrelated coordinate systems —
	// it reports a number, and retrieval looks like it works.
	if n := len(in.Embedding); n > 0 && n != s.Index.Emb.Dim() {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"embedding has %d dimensions, this server's model produces %d — "+
				"embed the query with POST /api/embed", n, s.Index.Emb.Dim()))
		return
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            filterFor(r, true),
		Query:             strings.TrimSpace(in.Query),
		QueryVector:       in.Embedding,
		Note:              normPath(in.Path),
		Agent:             strings.TrimSpace(in.Agent),
		Task:              strings.TrimSpace(in.Task),
		Session:           strings.TrimSpace(in.Session),
		Category:          strings.TrimSpace(in.Category),
		IncludeSuperseded: in.IncludeSuperseded,
		IncludeExpired:    in.IncludeExpired,
		Now:               vault.Now(),
		Limit:             limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entriesOut(hits, true))
}

// memoryGraph answers what memory knows about a thing, and what that thing is
// connected to. Without a seed it returns the busiest entities, which is the
// "show me the shape of what I know" view.
func (s *Server) memoryGraph(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	graph, err := s.Index.MemoryGraphFor(index.GraphQuery{
		Memory: index.MemoryQuery{
			Filter:   filterFor(r, true),
			Agent:    strings.TrimSpace(q.Get("agent")),
			Session:  strings.TrimSpace(q.Get("session")),
			Category: strings.TrimSpace(q.Get("category")),
			Now:      vault.Now(),
		},
		Seed:  strings.TrimSpace(q.Get("entity")),
		Depth: clampLimit(q.Get("depth"), 1, 4),
		Limit: clampLimit(q.Get("limit"), 50, 500),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"seed": graph.Seed, "nodes": graph.Nodes, "edges": graph.Edges,
		"entries": entriesOut(graph.Entries, false),
	})
}

// memoryFacets lists the scopes present in the caller's memory, so a console
// can offer them instead of asking someone to remember what they named a
// session three weeks ago.
func (s *Server) memoryFacets(w http.ResponseWriter, r *http.Request) {
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter: filterFor(r, true), IncludeSuperseded: true, IncludeExpired: true,
		Now: vault.Now(), Limit: 1 << 30,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	agents, sessions, categories := map[string]int{}, map[string]int{}, map[string]int{}
	live := 0
	for _, h := range hits {
		if h.Agent != "" {
			agents[h.Agent]++
		}
		if h.Session != "" {
			sessions[h.Session]++
		}
		if h.Category != "" {
			categories[h.Category]++
		}
		if h.Live(vault.Now()) {
			live++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": agents, "sessions": sessions, "categories": categories,
		"total": len(hits), "live": live,
	})
}
