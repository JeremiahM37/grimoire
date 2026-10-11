package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/prospect"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Intentions (internal/prospect), behind the `prospect` experiment flag.
//
// With the flag off none of this runs: writes that carry a cue are refused,
// the context endpoint and the listing behave as they always did, and the
// routes answer that the experiment is off.
//
// With the flag on, a fact with a cue is held back from ordinary context and
// recall, and is returned as a reminder only while its cue holds. Every
// firing is appended to .grimoire/prospect.jsonl. A once-intention retires
// only after a person or agent acknowledges a firing that was delivered.

// prospectLog is the firing log, beside the other derived state in .grimoire.
// It is not a note: it never syncs and is never read by recall.
const prospectLogName = "prospect.jsonl"

// intentionInput validates a write that carries a cue or a lifecycle, and
// normalises both in place. It runs before anything is stored.
func intentionInput(m *memoryIn) error {
	if !exp.On("prospect") {
		if m.Cue != "" || m.Life != "" {
			return errors.New("intentions are an experiment: start the server with GRIMOIRE_EXP=prospect")
		}
		return nil
	}
	if m.Cue == "" {
		if m.Life != "" {
			return errors.New("life needs a cue: an intention has to say when it fires")
		}
		return nil
	}
	c, err := prospect.ParseCue(m.Cue)
	if err != nil {
		return err
	}
	life, err := prospect.NormLife(m.Life)
	if err != nil {
		return err
	}
	if err := prospect.CheckWritable(m.Origin); err != nil {
		return err
	}
	if m.Visibility != "" {
		// A reminder has to be readable when it fires. Private, sensitive
		// and sealed facts are held back from every default read, so an
		// intention written that way could never fire.
		return errors.New("an intention must be normal visibility, or it never fires")
	}
	m.Cue, m.Life = c.String(), life
	return nil
}

// intentionEntries returns every cued fact in the memory notes, with its note
// and access list. It does not filter on the caller: the gate needs to know
// about every intention, so that a cued fact the caller cannot read is still
// never returned as ordinary recall.
func (s *Server) intentionEntries() ([]intentionLoc, error) {
	rows, err := s.Index.DB.Query("SELECT DISTINCT note, acl FROM memory_entries")
	if err != nil {
		return nil, err
	}
	type noteACL struct{ note, acl string }
	var notes []noteACL
	for rows.Next() {
		var n noteACL
		if err := rows.Scan(&n.note, &n.acl); err != nil {
			rows.Close()
			return nil, err
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []intentionLoc
	for _, n := range notes {
		note, err := s.Vault.Read(n.note)
		if err != nil {
			continue // a row whose file is gone has no intention to fire
		}
		for _, e := range memory.Parse(note.Body) {
			if e.Cue != "" {
				out = append(out, intentionLoc{note: n.note, acl: n.acl, entry: e})
			}
		}
	}
	return out, nil
}

type intentionLoc struct {
	note, acl string
	entry     memory.Entry
}

// situationOf reads the context of a request. The explicit intent_* parameters
// win. For an action (the hook's PreToolUse stage, whose query is the tool
// name and its target), the target of a shell or file tool is the command or
// path, so the existing hook supports cmd: and path: cues without change.
func situationOf(r *http.Request, now time.Time) prospect.Situation {
	q := r.URL.Query()
	sit := prospect.Situation{
		Now:     now,
		Prompt:  q.Get("q"),
		Command: strings.TrimSpace(q.Get("intent_cmd")),
		Project: strings.TrimSpace(q.Get("intent_project")),
	}
	for _, p := range q["intent_path"] {
		if p = strings.TrimSpace(p); p != "" && len(sit.Paths) < prospect.MaxPaths && len(p) <= 512 {
			sit.Paths = append(sit.Paths, p)
		}
	}
	if q.Get("stage") == "action" {
		tool, target, _ := strings.Cut(strings.TrimSpace(q.Get("q")), " ")
		target = strings.TrimSpace(target)
		switch tool {
		case "Bash", "bash", "shell":
			if sit.Command == "" {
				sit.Command = target
			}
		case "Edit", "MultiEdit", "Write", "NotebookEdit":
			if len(sit.Paths) == 0 && target != "" && len(target) <= 512 {
				sit.Paths = []string{target}
			}
		}
	}
	return sit
}

// prospectItems applies intentions to the items a context request would
// write. Cued facts are removed from the ordinary items whether or not their
// cue holds. The ones that do hold come back first, as reminders, at most
// prospect.MaxFiresPerReq of them, and each is logged. An error means the
// response must not be written: a firing that cannot be logged is not
// delivered.
func (s *Server) prospectItems(r *http.Request, items []contextItem) ([]contextItem, error) {
	if !exp.On("prospect") {
		return items, nil
	}
	switch r.URL.Query().Get("scope") {
	case "off", "manual":
		return items, nil
	}
	all, err := s.intentionEntries()
	if err != nil {
		return nil, err
	}
	cued := make(map[string]bool, len(all))
	var readable []prospect.Located
	for _, loc := range all {
		cued[entryKey(loc.note, loc.entry.ID)] = true
		if s.canReadNote(r, loc.note, loc.acl) {
			readable = append(readable, prospect.Located{Note: loc.note, Entry: loc.entry})
		}
	}
	kept := make([]contextItem, 0, len(items))
	for _, item := range items {
		if item.ID != "" && cued[entryKey(item.Path, item.ID)] {
			continue
		}
		kept = append(kept, item)
	}
	sit := situationOf(r, vault.Now())
	due, _ := prospect.Select(readable, sit, prospect.MaxFiresPerReq)
	var reminders []contextItem
	for _, f := range due {
		if err := s.logFirings(f, sit, r.URL.Query().Get("session")); err != nil {
			return nil, err
		}
		e := f.Entry
		authority := "agent"
		if e.Human {
			authority = "human"
		}
		reminders = append(reminders, contextItem{
			Path: f.Note, ID: e.ID, Authority: authority, Trust: "trusted", Kind: "intention",
			Text: fmt.Sprintf("Reminder you set (cue %s, set %s by %s in %s): %s",
				e.Cue, e.Stamp, e.Agent, f.Note, e.Text),
			score: 1,
		})
	}
	return append(reminders, kept...), nil
}

// entryKey names a fact by its note and id. An id is a content hash, so two
// notes can in principle share one; the note keeps them apart.
func entryKey(note, id string) string { return note + "\x00" + id }

// prospectRecallHits applies intentions to an ordinary recall. A cued fact is
// returned only when its cue holds for the query, and then it is logged as a
// firing. Recall has no reminder marker: the fact comes back as any hit would.
func (s *Server) prospectRecallHits(r *http.Request, hits []index.MemoryHit) ([]index.MemoryHit, error) {
	if !exp.On("prospect") {
		return hits, nil
	}
	all, err := s.intentionEntries()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return hits, nil
	}
	byKey := make(map[string]memory.Entry, len(all))
	for _, loc := range all {
		byKey[entryKey(loc.note, loc.entry.ID)] = loc.entry
	}
	sit := situationOf(r, vault.Now())
	kept := make([]index.MemoryHit, 0, len(hits))
	for _, h := range hits {
		e, cued := byKey[entryKey(h.Note, h.ID)]
		if !cued {
			kept = append(kept, h)
			continue
		}
		if !prospect.Fires(e, sit) {
			continue
		}
		if err := s.logFirings(prospect.Located{Note: h.Note, Entry: e}, sit, r.URL.Query().Get("session")); err != nil {
			return nil, err
		}
		kept = append(kept, h)
	}
	return kept, nil
}

// prospectRecord is one line of the firing log.
type prospectRecord struct {
	Event   string `json:"event"` // fired, acknowledged or retired
	At      string `json:"at"`
	ID      string `json:"id"`
	Path    string `json:"path"`
	Cue     string `json:"cue,omitempty"`
	Life    string `json:"life,omitempty"`
	Session string `json:"session,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
}

func (s *Server) prospectLogPath() string {
	return filepath.Join(s.Vault.Root, ".grimoire", prospectLogName)
}

func (s *Server) appendProspectLog(rec prospectRecord) error {
	p := s.prospectLogPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// logFirings records one delivered reminder. The prompt is kept only as far
// as its length, since it may be a whole request.
func (s *Server) logFirings(f prospect.Located, sit prospect.Situation, session string) error {
	prompt := sit.Prompt
	if len(prompt) > 200 {
		prompt = prompt[:200]
	}
	return s.appendProspectLog(prospectRecord{Event: "fired", At: sit.Now.UTC().Format(time.RFC3339),
		ID: f.Entry.ID, Path: f.Note, Cue: f.Entry.Cue, Life: f.Entry.Life,
		Session: session, Prompt: prompt})
}

// wasDelivered reports whether a reminder with this id has ever been logged as
// fired. An acknowledgement is only meaningful after a delivery.
func (s *Server) wasDelivered(id string) (bool, error) {
	f, err := os.Open(s.prospectLogPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var rec prospectRecord
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Event == "fired" && rec.ID == id {
			return true, nil
		}
	}
	return false, sc.Err()
}

// intentionList answers GET /api/memory/intentions: the intentions the caller
// can read, with their status now. Hidden intentions are left out, as they
// are everywhere a default read happens.
func (s *Server) intentionList(w http.ResponseWriter, r *http.Request) {
	if !exp.On("prospect") {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "intentions": []any{}})
		return
	}
	all, err := s.intentionEntries()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := vault.Now()
	out := []map[string]any{}
	for _, loc := range all {
		e := loc.entry
		if e.Hidden() || e.Redacted() || !s.canReadNote(r, loc.note, loc.acl) {
			continue
		}
		// Whether the cue holds depends on the request, so the listing does not
		// claim it: a live intention is "waiting" until a request satisfies it.
		status := "waiting"
		switch {
		case e.Retired != "":
			status = "retired"
		case !prospect.Live(e.Life, e.Retired, now):
			status = "expired"
		}
		out = append(out, map[string]any{"path": loc.note, "id": e.ID, "text": e.Text,
			"cue": e.Cue, "life": e.Life, "retired": e.Retired, "agent": e.Agent,
			"stamp": e.Stamp, "status": status})
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "intentions": out})
}

type intentionAckIn struct {
	Path string `json:"path"`
	ID   string `json:"id"`
}

// intentionAck answers POST /api/memory/intention/ack. A once-intention
// retires when this arrives for a firing that was delivered earlier. Any other
// intention is left as it is, and the acknowledgement is only logged.
func (s *Server) intentionAck(w http.ResponseWriter, r *http.Request) {
	if !exp.On("prospect") {
		writeErr(w, http.StatusNotFound, "intentions are an experiment: start the server with GRIMOIRE_EXP=prospect")
		return
	}
	var in intentionAckIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	note := normPath(in.Path)
	if note == "" || in.ID == "" {
		writeErr(w, http.StatusBadRequest, "path and id are required")
		return
	}
	if !s.requireWrite(w, r, note) {
		return
	}
	existing, err := s.Vault.Read(note)
	if err != nil {
		writeErr(w, http.StatusNotFound, "intention not found")
		return
	}
	var target *memory.Entry
	for _, e := range memory.Parse(existing.Body) {
		if e.ID == in.ID && e.Cue != "" {
			target = &e
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "intention not found")
		return
	}
	delivered, err := s.wasDelivered(in.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !delivered {
		writeErr(w, http.StatusConflict, "nothing has fired for this intention yet, so there is nothing to acknowledge")
		return
	}
	now := vault.Now()
	retire := target.Life == prospect.LifeOnce && target.Retired == ""
	if retire {
		stamp := now.UTC().Format(time.RFC3339)
		if err := s.mutateEntry(note, in.ID, func(e *memory.Entry) { e.Retired = stamp }); err != nil {
			writeErr(w, statusForEntryErr(err), entryErrMsg(err))
			return
		}
	}
	event := "acknowledged"
	if retire {
		event = "retired"
	}
	if err := s.appendProspectLog(prospectRecord{Event: event, At: now.UTC().Format(time.RFC3339),
		ID: in.ID, Path: note, Cue: target.Cue, Life: target.Life}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": note, "id": in.ID, "retired": retire})
}
