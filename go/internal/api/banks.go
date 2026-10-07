package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
)

// Memory banks over HTTP. A bank is a folder of notes (banks/<bank>/), so
// access is decided exactly as for any note under that path: the caller must
// be able to read the bank's bank.md to see anything in it, and write it to
// change anything. See internal/bank for the engine itself.

func (s *Server) bankRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/banks", s.listBanks)
	mux.HandleFunc("POST /api/banks", s.createBank)
	mux.HandleFunc("GET /api/banks/{bank}", s.getBank)
	mux.HandleFunc("PATCH /api/banks/{bank}", s.updateBank)
	mux.HandleFunc("DELETE /api/banks/{bank}", s.deleteBank)
	mux.HandleFunc("POST /api/banks/{bank}/memories", s.retainBank)
	mux.HandleFunc("GET /api/banks/{bank}/memories", s.listBankMemories)
	mux.HandleFunc("POST /api/banks/{bank}/memories/recall", s.recallBank)
	mux.HandleFunc("GET /api/banks/{bank}/memories/{id}", s.getBankMemory)
	mux.HandleFunc("DELETE /api/banks/{bank}/memories/{id}", s.deleteBankMemory)
	mux.HandleFunc("GET /api/banks/{bank}/entities", s.listBankEntities)
	mux.HandleFunc("GET /api/banks/{bank}/entities/{id}", s.getBankEntity)
	mux.HandleFunc("GET /api/banks/{bank}/documents", s.listBankDocuments)
	mux.HandleFunc("GET /api/banks/{bank}/documents/{id}", s.getBankDocument)
	mux.HandleFunc("DELETE /api/banks/{bank}/documents/{id}", s.deleteBankDocument)
	mux.HandleFunc("GET /api/banks/{bank}/chunks/{id}", s.getBankChunk)
	mux.HandleFunc("GET /api/banks/{bank}/context", s.bankContext)
	mux.HandleFunc("GET /api/banks/{bank}/index", s.bankIndex)
	mux.HandleFunc("GET /api/banks/{bank}/timeline", s.bankTimeline)
	mux.HandleFunc("GET /api/banks/{bank}/file-memory", s.bankFileMemory)
	mux.HandleFunc("GET /api/banks/{bank}/duplicates", s.bankDuplicates)
	mux.HandleFunc("POST /api/banks/{bank}/duplicates/merge", s.bankMergeDuplicates)
	mux.HandleFunc("GET /api/banks/{bank}/lookup", s.bankLookup)
	mux.HandleFunc("GET /api/banks/{bank}/sessions", s.listBankSessions)
	mux.HandleFunc("POST /api/banks/{bank}/sessions/{session}/digest", s.writeSessionDigest)
	s.bankReasoningRoutes(mux)
}

// bankFor reads and checks the {bank} path value.
func bankFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("bank")
	if !bank.ValidID(id) {
		writeErr(w, http.StatusBadRequest, "invalid bank id")
		return "", false
	}
	return id, true
}

// bankReadable answers 404 for a bank the caller may not see — the same
// answer as for one that does not exist, so a bank's name is not confirmed to
// someone outside it.
func (s *Server) bankReadable(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := bankFor(w, r)
	if !ok {
		return "", false
	}
	if !s.canRead(r, bank.ProfilePath(id)) {
		writeErr(w, http.StatusNotFound, "no such bank")
		return "", false
	}
	return id, true
}

func (s *Server) bankWritable(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := bankFor(w, r)
	if !ok {
		return "", false
	}
	if !s.requireUser(w, r) {
		return "", false
	}
	if !s.canWrite(r, bank.ProfilePath(id)) {
		if !s.canRead(r, bank.ProfilePath(id)) {
			writeErr(w, http.StatusNotFound, "no such bank")
		} else {
			writeErr(w, http.StatusForbidden, "you have read-only access to that bank")
		}
		return "", false
	}
	return id, true
}

func writeBankErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bank.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, bank.ErrExists):
		writeErr(w, http.StatusConflict, "already exists") // a bank, a mental model or a directive name
	case errors.Is(err, bank.ErrInvalid):
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), bank.ErrInvalid.Error()+": "))
	case errors.Is(err, bank.ErrHumanProtected):
		writeErr(w, http.StatusConflict, "a person wrote or edited this; pass force=true to remove it anyway")
	case errors.Is(err, bank.ErrModelRequired):
		writeJSON(w, http.StatusConflict, map[string]string{"detail": "model_required: this needs a language model and none is configured",
			"code": "model_required"})
	case errors.Is(err, bank.ErrTerminal):
		writeErr(w, http.StatusConflict, "the operation has already finished")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// profileIn is the editable part of a bank profile. Pointers distinguish
// "not sent" from "set to empty".
type profileIn struct {
	ID            string             `json:"bank_id"`
	Name          *string            `json:"name"`
	Mission       *string            `json:"mission"`
	RetainMission *string            `json:"retain_mission"`
	Disposition   *bank.Disposition  `json:"disposition"`
	Tags          *[]string          `json:"tags"`
	Directives    *[]bank.Directive  `json:"directives"`
	Config        *map[string]string `json:"config"`
}

func (in profileIn) apply(p *bank.Profile) {
	if in.Name != nil {
		p.Name = *in.Name
	}
	if in.Mission != nil {
		p.Mission = *in.Mission
	}
	if in.RetainMission != nil {
		p.RetainMission = *in.RetainMission
	}
	if in.Disposition != nil {
		p.Disposition = *in.Disposition
	}
	if in.Tags != nil {
		p.Tags = *in.Tags
	}
	if in.Directives != nil {
		p.Directives = *in.Directives
	}
	if in.Config != nil {
		// A patch merges; an empty value removes a setting.
		for k, v := range *in.Config {
			if v == "" {
				delete(p.Config, k)
			} else {
				p.Config[k] = v
			}
		}
	}
}

func (s *Server) listBanks(w http.ResponseWriter, r *http.Request) {
	all, err := s.Banks.ListBanks()
	if err != nil {
		writeBankErr(w, err)
		return
	}
	out := []bank.BankSummary{}
	for _, b := range all {
		if s.canRead(r, bank.ProfilePath(b.ID)) {
			out = append(out, b)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"banks": out})
}

func (s *Server) createBank(w http.ResponseWriter, r *http.Request) {
	var in profileIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !bank.ValidID(in.ID) {
		writeErr(w, http.StatusBadRequest, "bank_id must match [a-z0-9][a-z0-9._:-]{0,63}")
		return
	}
	if !s.requireUser(w, r) || !s.requireWrite(w, r, bank.ProfilePath(in.ID)) {
		return
	}
	p := bank.NewProfile(in.ID)
	in.apply(p)
	if err := s.Banks.CreateBank(p); err != nil {
		writeBankErr(w, err)
		return
	}
	got, err := s.Banks.Profile(in.ID)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (s *Server) getBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	p, err := s.Banks.Profile(id)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) updateBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var in profileIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	p, err := s.Banks.UpdateProfile(id, func(p *bank.Profile) error { in.apply(p); return nil })
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deleteBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	if err := s.Banks.DeleteBank(id); err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// itemIn is one retain item as it arrives over the wire. Content may be a
// string or any JSON value — a conversation is usually an array of turns —
// and tags may be a list, a JSON-encoded list, or one string.
type itemIn struct {
	Content         json.RawMessage   `json:"content"`
	Timestamp       json.RawMessage   `json:"timestamp"`
	Context         string            `json:"context"`
	Metadata        map[string]any    `json:"metadata"`
	DocumentID      string            `json:"document_id"`
	Entities        []bank.EntityHint `json:"entities"`
	ResolveEntities *bool             `json:"resolve_entities"`
	Tags            json.RawMessage   `json:"tags"`
	UpdateMode      string            `json:"update_mode"`
	ScanSecrets     bool              `json:"scan_secrets"`
}

func (in itemIn) item() (bank.Item, error) {
	it := bank.Item{Context: in.Context, DocumentID: strings.TrimSpace(in.DocumentID),
		Entities: in.Entities, ResolveEntities: in.ResolveEntities, UpdateMode: in.UpdateMode,
		ScanSecrets: in.ScanSecrets}
	var str string
	if json.Unmarshal(in.Content, &str) == nil {
		it.Content = str
	} else if len(in.Content) > 0 && string(in.Content) != "null" {
		it.Content = string(in.Content)
	}
	if len(in.Timestamp) > 0 && string(in.Timestamp) != "null" {
		var ts string
		if err := json.Unmarshal(in.Timestamp, &ts); err != nil {
			return it, fmt.Errorf("timestamp must be an ISO-8601 string")
		}
		switch strings.TrimSpace(ts) {
		case "":
		case "unset":
			it.Unset = true
		default:
			t, err := parseISO(ts)
			if err != nil {
				return it, fmt.Errorf("timestamp %q is not ISO-8601", ts)
			}
			it.Timestamp = &t
		}
	}
	if len(in.Metadata) > 0 {
		it.Metadata = map[string]string{}
		for k, v := range in.Metadata {
			if v != nil {
				it.Metadata[k] = fmt.Sprint(v)
			}
		}
	}
	tags, err := parseTags(in.Tags)
	if err != nil {
		return it, err
	}
	it.Tags = tags
	return it, nil
}

func parseTags(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list, nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, fmt.Errorf("tags must be a list of strings")
	}
	if strings.HasPrefix(strings.TrimSpace(s), "[") && json.Unmarshal([]byte(s), &list) == nil {
		return list, nil
	}
	return []string{s}, nil
}

// parseISO accepts the timestamp shapes callers send: RFC 3339 with or
// without fractional seconds, with Z or an offset, or naive (read as UTC).
func parseISO(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05",
		"2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("not ISO-8601")
}

// maxRetainItems bounds one synchronous retain.
const maxRetainItems = 500

// maxAsyncRetainItems bounds one asynchronous retain request.
const maxAsyncRetainItems = 10000

func (s *Server) retainBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var req struct {
		Items        []itemIn `json:"items"`
		Async        bool     `json:"async"`
		DocumentTags []string `json:"document_tags"`
		Mode         string   `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	limit := maxRetainItems
	if req.Async {
		limit = maxAsyncRetainItems
	}
	if len(req.Items) == 0 || len(req.Items) > limit {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("items must hold 1..%d entries", limit))
		return
	}
	items := make([]bank.Item, 0, len(req.Items))
	for i, in := range req.Items {
		it, err := in.item()
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("item %d: %v", i, err))
			return
		}
		items = append(items, it)
	}
	opts := bank.RetainOptions{Agent: agentFor(r), DocumentTags: req.DocumentTags, Mode: req.Mode}
	if req.Async {
		// A large batch is split into operations of at most maxRetainItems,
		// so each stays a unit of work the queue can retry and cancel.
		var ids []string
		for start := 0; start < len(items); start += maxRetainItems {
			opID, err := s.Banks.EnqueueRetain(id, items[start:min(start+maxRetainItems, len(items))], opts)
			if err != nil {
				writeBankErr(w, err)
				return
			}
			ids = append(ids, opID)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "bank_id": id, "items_count": len(items),
			"async": true, "operation_id": ids[0], "operation_ids": ids})
		return
	}
	res, err := s.Banks.Retain(r.Context(), id, items, opts)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "bank_id": res.BankID, "items_count": res.ItemsCount, "async": false,
		"mode": res.Mode, "documents": res.Documents, "bank_created": res.BankCreate,
		"usage": map[string]int{"input_tokens": res.Usage.Input, "output_tokens": res.Usage.Output,
			"total_tokens": res.Usage.Input + res.Usage.Output},
	})
}

func (s *Server) recallBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	var in struct {
		Query          string                     `json:"query"`
		Types          []string                   `json:"types"`
		Budget         string                     `json:"budget"`
		MaxTokens      *int                       `json:"max_tokens"`
		QueryTimestamp string                     `json:"query_timestamp"`
		Tags           []string                   `json:"tags"`
		TagsMatch      string                     `json:"tags_match"`
		Include        map[string]json.RawMessage `json:"include"`
		Trace          bool                       `json:"trace"`
		TagGroups      []bank.TagGroup            `json:"tag_groups"`
		TemporalWindow *struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"temporal_window"`
		MinScores          *bank.MinScores `json:"min_scores"`
		PreferObservations bool            `json:"prefer_observations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	req := bank.RecallRequest{Query: in.Query, Types: in.Types, Budget: in.Budget, MaxTokens: in.MaxTokens,
		Tags: in.Tags, TagsMatch: in.TagsMatch, Trace: in.Trace, TagGroups: in.TagGroups, MinScores: in.MinScores,
		PreferObservations: in.PreferObservations}
	if tw := in.TemporalWindow; tw != nil {
		a, err1 := parseISO(tw.Start)
		b, err2 := parseISO(tw.End)
		if err1 != nil || err2 != nil {
			writeErr(w, http.StatusBadRequest, "temporal_window needs ISO-8601 start and end")
			return
		}
		if b.Hour() == 0 && b.Minute() == 0 && b.Second() == 0 {
			b = b.Add(24*time.Hour - time.Nanosecond) // a date end covers its whole day
		}
		req.Window = &bank.Window{Start: a, End: b}
	}
	if in.QueryTimestamp != "" {
		t, err := parseISO(in.QueryTimestamp)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "query_timestamp is not ISO-8601")
			return
		}
		req.QueryTimestamp = &t
	}
	if raw, ok := in.Include["entities"]; ok && string(raw) == "null" {
		off := false
		req.Entities = &off
	}
	if raw, ok := in.Include["chunks"]; ok && string(raw) != "null" {
		var opt struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(raw, &opt)
		req.ChunkTokens = opt.MaxTokens
		if req.ChunkTokens <= 0 {
			req.ChunkTokens = bank.DefaultChunkTokens
		}
	}
	if raw, ok := in.Include["source_facts"]; ok && string(raw) != "null" {
		req.SourceFacts = true
		var opt struct {
			MaxTokens       *int `json:"max_tokens"`
			MaxTokensPerObs *int `json:"max_tokens_per_observation"`
		}
		_ = json.Unmarshal(raw, &opt)
		if opt.MaxTokens != nil {
			req.SourceFactsMaxTokens = *opt.MaxTokens
			if req.SourceFactsMaxTokens == 0 {
				req.SourceFacts = false
			}
		}
		if opt.MaxTokensPerObs != nil {
			req.SourceFactsPerObs = *opt.MaxTokensPerObs
		}
	}
	res, err := s.Banks.Recall(r.Context(), id, req)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) listBankMemories(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	fq := bank.FactQuery{Type: q.Get("type"), Document: q.Get("document_id"), Query: q.Get("q"),
		Limit: clampLimit(q.Get("limit"), 100, 1000)}
	fq.Offset, _ = strconv.Atoi(q.Get("offset"))
	if v := q.Get("authority"); v != "" {
		h := v == "human"
		fq.Human = &h
	}
	facts, total, err := s.Banks.ListFacts(id, fq)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": facts, "total": total})
}

func (s *Server) getBankMemory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	f, err := s.Banks.GetFact(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) deleteBankMemory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	if err := s.Banks.DeleteFact(id, r.PathValue("id"), boolParam(r, "force")); err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

func (s *Server) listBankEntities(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	ents, err := s.Banks.ListEntities(id, r.URL.Query().Get("q"), clampLimit(r.URL.Query().Get("limit"), 100, 1000))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ents})
}

func (s *Server) getBankEntity(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	d, err := s.Banks.GetEntity(id, r.PathValue("id"), clampLimit(r.URL.Query().Get("limit"), 50, 500))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) listBankDocuments(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	docs, total, err := s.Banks.ListDocuments(id, clampLimit(r.URL.Query().Get("limit"), 100, 1000), max(offset, 0))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": docs, "total": total})
}

func (s *Server) getBankDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	d, err := s.Banks.GetDocument(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) deleteBankDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	kept, err := s.Banks.DeleteDocument(id, r.PathValue("id"), boolParam(r, "force"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id"), "human_facts_kept": kept})
}

func (s *Server) getBankChunk(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	c, err := s.Banks.GetChunk(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// bankContext renders what a coding agent is shown when a session starts, held
// under max_chars (default 9000).
func (s *Server) bankContext(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("max_chars"))
	res, err := s.Banks.SessionContext(id, bank.ContextOptions{MaxChars: n, Source: r.URL.Query().Get("source")})
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeSessionDigest writes a session's "where we left off" note into the bank.
func (s *Server) writeSessionDigest(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var in bank.DigestInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.SessionID = r.PathValue("session")
	res, err := s.Banks.WriteDigest(r.Context(), id, in)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// listBankSessions lists a bank's session digests, newest first.
func (s *Server) listBankSessions(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 200 {
		n = 20
	}
	ds, err := s.Banks.ListDigests(id, n)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	if ds == nil {
		ds = []bank.DigestSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ds, "total": len(ds)})
}

// bankIndex is the cheap first step of progressive disclosure: ids, one-line
// titles and dates, ranked by q when given and newest first otherwise.
func (s *Server) bankIndex(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	items, total, err := s.Banks.BankIndex(r.Context(), id, bank.IndexQuery{Query: q.Get("q"),
		Types: splitCSV(q.Get("types")), Since: q.Get("since"), Limit: limit, Offset: offset})
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// bankTimeline returns the entries around an entry (anchor=#ref) or a day.
func (s *Server) bankTimeline(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	before, errB := strconv.Atoi(q.Get("before"))
	after, errA := strconv.Atoi(q.Get("after"))
	if errB != nil {
		before = 5
	}
	if errA != nil {
		after = 5
	}
	res, err := s.Banks.Timeline(id, q.Get("anchor"), before, after)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// bankLookup fetches entries in full by short reference or id (ids=a,b,c).
func (s *Server) bankLookup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	refs := splitCSV(r.URL.Query().Get("ids"))
	if len(refs) == 0 || len(refs) > 50 {
		writeErr(w, http.StatusBadRequest, "ids must hold 1..50 references")
		return
	}
	items, missing, err := s.Banks.GetByIDs(id, refs)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "missing": missing})
}

// bankFileMemory returns what the bank remembers about one file.
func (s *Server) bankFileMemory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	file := strings.TrimSpace(q.Get("path"))
	if file == "" || len(file) > 500 {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	items, err := s.Banks.FileMemory(id, file, limit)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": file, "items": items})
}

// bankDuplicates lists near-duplicate facts and observations for review.
func (s *Server) bankDuplicates(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	min, _ := strconv.ParseFloat(q.Get("min_score"), 64)
	items, err := s.Banks.DuplicateCandidates(id, bank.DuplicateQuery{MinScore: min, Limit: limit, Types: q.Get("type")})
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": items})
}

// bankMergeDuplicates folds one entry into another; the merged text is struck
// through and kept.
func (s *Server) bankMergeDuplicates(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var req struct {
		Keep  string `json:"keep"`
		Merge string `json:"merge"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be {\"keep\": id, \"merge\": id}")
		return
	}
	res, err := s.Banks.MergeDuplicates(id, req.Keep, req.Merge)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
