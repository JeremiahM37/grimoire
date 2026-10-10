package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"github.com/JeremiahM37/grimoire/go/internal/rerank"
)

// The memory-bank surfaces that build on retain and recall: reflect,
// observations and consolidation, mental models, directives, operations,
// webhooks and templates. Access is the bank folder's, as for the rest of
// /api/banks (see banks.go). Every route is listed in docs/MEMORY_BANKS.md.

func (s *Server) bankReasoningRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/banks/{bank}/stats", s.bankStats)
	mux.HandleFunc("POST /api/banks/{bank}/reflect", s.reflectBank)

	mux.HandleFunc("GET /api/banks/{bank}/observations", s.listObservations)
	mux.HandleFunc("DELETE /api/banks/{bank}/observations", s.clearObservations)
	mux.HandleFunc("GET /api/banks/{bank}/observations/{id}", s.getObservation)
	mux.HandleFunc("PATCH /api/banks/{bank}/observations/{id}", s.updateObservation)
	mux.HandleFunc("DELETE /api/banks/{bank}/observations/{id}", s.deleteObservation)
	mux.HandleFunc("POST /api/banks/{bank}/consolidate", s.consolidateBank)

	mux.HandleFunc("GET /api/banks/{bank}/mental-models", s.listModels)
	mux.HandleFunc("POST /api/banks/{bank}/mental-models", s.createModel)
	mux.HandleFunc("GET /api/banks/{bank}/mental-models-tree", s.modelTree)
	mux.HandleFunc("GET /api/banks/{bank}/mental-models-export", s.exportModels)
	mux.HandleFunc("GET /api/banks/{bank}/mental-models/{id}", s.getModel)
	mux.HandleFunc("PATCH /api/banks/{bank}/mental-models/{id}", s.updateModel)
	mux.HandleFunc("DELETE /api/banks/{bank}/mental-models/{id}", s.deleteModel)
	mux.HandleFunc("POST /api/banks/{bank}/mental-models/{id}/refresh", s.refreshModel)
	mux.HandleFunc("POST /api/banks/{bank}/mental-models/{id}/proposal/accept", s.acceptProposal)
	mux.HandleFunc("POST /api/banks/{bank}/mental-models/{id}/proposal/reject", s.rejectProposal)
	mux.HandleFunc("GET /api/banks/{bank}/mental-models/{id}/history", s.modelHistory)
	mux.HandleFunc("GET /api/banks/{bank}/mental-models/{id}/history/{version}", s.modelVersion)

	mux.HandleFunc("GET /api/banks/{bank}/directives", s.listDirectives)
	mux.HandleFunc("POST /api/banks/{bank}/directives", s.createDirective)
	mux.HandleFunc("PATCH /api/banks/{bank}/directives/{id}", s.updateDirective)
	mux.HandleFunc("DELETE /api/banks/{bank}/directives/{id}", s.deleteDirective)

	mux.HandleFunc("GET /api/banks/{bank}/operations", s.listOperations)
	mux.HandleFunc("GET /api/banks/{bank}/operations/{id}", s.getOperation)
	mux.HandleFunc("DELETE /api/banks/{bank}/operations/{id}", s.cancelOperation)

	mux.HandleFunc("GET /api/banks/{bank}/webhooks", s.listBankWebhooks)
	mux.HandleFunc("POST /api/banks/{bank}/webhooks", s.createBankWebhook)
	mux.HandleFunc("PATCH /api/banks/{bank}/webhooks/{id}", s.updateBankWebhook)
	mux.HandleFunc("DELETE /api/banks/{bank}/webhooks/{id}", s.deleteBankWebhook)
	mux.HandleFunc("GET /api/banks/{bank}/webhooks/{id}/deliveries", s.bankWebhookDeliveries)
	// Webhooks for every bank are an administrator's.
	mux.HandleFunc("GET /api/webhooks", s.adminOnly(s.listGlobalWebhooks))
	mux.HandleFunc("POST /api/webhooks", s.adminOnly(s.createGlobalWebhook))
	mux.HandleFunc("PATCH /api/webhooks/{id}", s.adminOnly(s.updateGlobalWebhook))
	mux.HandleFunc("DELETE /api/webhooks/{id}", s.adminOnly(s.deleteGlobalWebhook))
	mux.HandleFunc("GET /api/webhooks/{id}/deliveries", s.adminOnly(s.globalWebhookDeliveries))

	mux.HandleFunc("GET /api/bank-templates", s.listBankTemplates)
	mux.HandleFunc("GET /api/bank-templates/{id}", s.getBankTemplate)
	mux.HandleFunc("GET /api/banks/{bank}/export", s.exportBank)
	mux.HandleFunc("POST /api/banks/{bank}/import", s.importBank)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

// bankExists answers 404 (JSON) for a bank that is readable but absent.
func (s *Server) bankExists(w http.ResponseWriter, id string) bool {
	if _, err := s.Banks.Profile(id); err != nil {
		writeBankErr(w, err)
		return false
	}
	return true
}

func (s *Server) bankStats(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	st, err := s.Banks.Stats(id)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ------------------------------------------------------------ reflect

func (s *Server) reflectBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	var in struct {
		Query              string                     `json:"query"`
		Budget             string                     `json:"budget"`
		MaxTokens          *int                       `json:"max_tokens"`
		Context            string                     `json:"context"`
		ResponseSchema     map[string]any             `json:"response_schema"`
		Tags               []string                   `json:"tags"`
		TagsMatch          string                     `json:"tags_match"`
		TagGroups          []bank.TagGroup            `json:"tag_groups"`
		FactTypes          []string                   `json:"fact_types"`
		ApplyAllDirectives bool                       `json:"apply_all_directives"`
		ExcludeModels      bool                       `json:"exclude_mental_models"`
		ExcludeModelIDs    []string                   `json:"exclude_mental_model_ids"`
		QueryTimestamp     string                     `json:"query_timestamp"`
		Include            map[string]json.RawMessage `json:"include"`
		Trace              bool                       `json:"trace"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	req := bank.ReflectRequest{Query: in.Query, Budget: in.Budget, MaxTokens: in.MaxTokens, Context: in.Context,
		ResponseSchema: in.ResponseSchema, Tags: in.Tags, TagsMatch: in.TagsMatch, TagGroups: in.TagGroups,
		FactTypes: in.FactTypes, ApplyAllDirectives: in.ApplyAllDirectives, ExcludeModels: in.ExcludeModels,
		ExcludeModelIDs: in.ExcludeModelIDs, Agent: agentFor(r)}
	if in.QueryTimestamp != "" {
		t, err := parseISO(in.QueryTimestamp)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "query_timestamp is not ISO-8601")
			return
		}
		req.QueryTimestamp = &t
	}
	if req.MaxTokens == nil {
		if p, err := s.Banks.Profile(id); err == nil {
			if n, err := strconv.Atoi(p.Setting("reflect_max_tokens", "")); err == nil {
				req.MaxTokens = &n
			}
		}
	}
	res, err := s.Banks.Reflect(r.Context(), id, req)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	// The trace is returned when asked for; include.tool_calls.output=false
	// keeps the tool calls but drops what they returned.
	raw, wantTrace := in.Include["tool_calls"]
	wantTrace = (wantTrace && string(raw) != "null") || in.Trace
	if !wantTrace {
		res.Trace = nil
	} else if wantTrace && strings.Contains(strings.ReplaceAll(string(raw), " ", ""), `"output":false`) {
		for i := range res.Trace.ToolCalls {
			res.Trace.ToolCalls[i].Output = nil
		}
	}
	s.Banks.FireEvent(id, bank.EventReflectCompleted, "", "completed", map[string]any{"query": in.Query, "mode": res.Mode,
		"memory_count": len(res.BasedOn.Memories), "observation_count": len(res.BasedOn.Observations)})
	writeJSON(w, http.StatusOK, res)
}

// ------------------------------------------------------------ observations

func (s *Server) listObservations(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	oq := bank.ObservationQuery{Query: q.Get("q"), Limit: clampLimit(q.Get("limit"), 100, 1000),
		TagsMatch: q.Get("tags_match"), IncludeHistory: boolParam(r, "include_history")}
	oq.Offset, _ = strconv.Atoi(q.Get("offset"))
	if v := q.Get("authority"); v != "" {
		h := v == "human"
		oq.Human = &h
	}
	for _, t := range q["tags"] {
		oq.Tags = append(oq.Tags, splitCSV(t)...)
	}
	items, hist, total, err := s.Banks.ListObservations(id, oq)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	out := map[string]any{"items": items, "total": total}
	if oq.IncludeHistory {
		out["history"] = hist
	}
	writeJSON(w, http.StatusOK, out)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) getObservation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	o, hist, err := s.Banks.GetObservation(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": o, "history": hist})
}

// updateObservation edits an observation's text; the edit makes it a person's.
func (s *Server) updateObservation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	o, err := s.Banks.UpdateObservation(id, r.PathValue("id"), in.Text)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"observation": o})
}

func (s *Server) deleteObservation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	if err := s.Banks.DeleteObservation(id, r.PathValue("id"), boolParam(r, "force")); err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

func (s *Server) clearObservations(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok || !s.bankExists(w, id) {
		return
	}
	n, err := s.Banks.ClearObservations(id)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retired": n})
}

func (s *Server) consolidateBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok || !s.bankExists(w, id) {
		return
	}
	opID, dedup, err := s.Banks.EnqueueConsolidation(id)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation_id": opID, "deduplicated": dedup})
}

// ------------------------------------------------------------ mental models

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	mq := bank.ModelQuery{Folder: q.Get("folder"), TagsMatch: q.Get("tags_match"),
		Detail: q.Get("detail") == "full" || q.Get("detail") == "content" || boolParam(r, "detail")}
	for _, t := range q["tags"] {
		mq.Tags = append(mq.Tags, splitCSV(t)...)
	}
	items, err := s.Banks.ListModels(id, mq)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (s *Server) modelTree(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	tree, err := s.Banks.ModelTree(id, bank.ModelQuery{Folder: r.URL.Query().Get("folder")})
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": tree})
}

func (s *Server) exportModels(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	files, err := s.Banks.ExportModels(id)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	if r.URL.Query().Get("format") == "markdown" {
		// One document: every page in tree order after the index.
		var b strings.Builder
		for i, f := range files {
			if i > 0 {
				b.WriteString("\n\n<!-- " + f.Path + " -->\n\n")
			}
			b.WriteString(f.Content)
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	m, err := s.Banks.GetModel(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// modelSpecIn accepts the model fields, with "source_query" and "content"
// as synonyms of question and body.
type modelSpecIn struct {
	bank.ModelSpec
	SourceQuery *string `json:"source_query"`
	Content     *string `json:"content"`
}

func (in modelSpecIn) spec() bank.ModelSpec {
	sp := in.ModelSpec
	if sp.Question == nil {
		sp.Question = in.SourceQuery
	}
	if sp.Body == nil {
		sp.Body = in.Content
	}
	return sp
}

func (s *Server) createModel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok || !s.bankExists(w, id) {
		return
	}
	var in modelSpecIn
	if !decodeJSON(w, r, &in) {
		return
	}
	m, err := s.Banks.CreateModel(id, in.spec())
	if err != nil {
		writeBankErr(w, err)
		return
	}
	// The first answer is written in the background — unless a person gave
	// one, or there is no model to write it.
	var opID any
	if s.AI.Available() && m.Body == "" {
		if op, _, err := s.Banks.EnqueueRefresh(id, m.ID); err == nil {
			opID = op
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"mental_model": m, "mental_model_id": m.ID, "operation_id": opID})
}

func (s *Server) updateModel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var in modelSpecIn
	if !decodeJSON(w, r, &in) {
		return
	}
	m, err := s.Banks.UpdateModel(id, r.PathValue("id"), in.spec())
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	if err := s.Banks.DeleteModel(id, r.PathValue("id")); err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

func (s *Server) refreshModel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	mid := r.PathValue("id")
	cur, err := s.Banks.GetModel(id, mid)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	}
	if req.Mode != "" && req.Mode != "full" && req.Mode != "delta" {
		writeErr(w, http.StatusBadRequest, "mode must be full or delta")
		return
	}
	if mode := firstNonEmpty(req.Mode, cur.RefreshMode); mode != "delta" && !s.AI.Available() {
		writeBankErr(w, bank.ErrModelRequired)
		return
	}
	opID, dedup, err := s.Banks.EnqueueRefreshMode(id, mid, req.Mode)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation_id": opID, "status": "queued", "deduplicated": dedup})
}

func (s *Server) acceptProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	m, err := s.Banks.AcceptProposal(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) rejectProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	if err := s.Banks.RejectProposal(id, r.PathValue("id")); err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rejected": r.PathValue("id")})
}

func (s *Server) modelHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	m, err := s.Banks.GetModel(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	versions := []any{}
	if s.History != nil {
		for _, v := range s.History.ListVersions(m.Path) {
			versions = append(versions, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mental_model_id": m.ID, "path": m.Path, "version": m.Version, "versions": versions})
}

func (s *Server) modelVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	m, err := s.Banks.GetModel(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	if s.History == nil {
		writeErr(w, http.StatusNotFound, "no version history")
		return
	}
	body, found := s.History.GetVersion(m.Path, r.PathValue("version"))
	if !found {
		writeErr(w, http.StatusNotFound, "no such version")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mental_model_id": m.ID, "version": r.PathValue("version"), "content": body})
}

// ------------------------------------------------------------ directives

func (s *Server) listDirectives(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	var tags []string
	for _, t := range r.URL.Query()["tags"] {
		tags = append(tags, splitCSV(t)...)
	}
	activeOnly := r.URL.Query().Get("active_only") != "false"
	ds, err := s.Banks.ListDirectives(id, tags, activeOnly)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ds, "total": len(ds)})
}

func (s *Server) createDirective(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var in bank.DirectiveSpec
	if !decodeJSON(w, r, &in) {
		return
	}
	d, err := s.Banks.CreateDirective(id, in)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) updateDirective(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var in bank.DirectiveSpec
	if !decodeJSON(w, r, &in) {
		return
	}
	d, err := s.Banks.UpdateDirective(id, r.PathValue("id"), in)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) deleteDirective(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	if err := s.Banks.DeleteDirective(id, r.PathValue("id")); err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

// ------------------------------------------------------------ operations

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok || !s.bankExists(w, id) {
		return
	}
	q := r.URL.Query()
	kind := q.Get("type")
	if kind == "" {
		kind = q.Get("kind")
	}
	oq := bank.OperationQuery{Status: q.Get("status"), Kind: kind, Limit: clampLimit(q.Get("limit"), 50, 500)}
	oq.Offset, _ = strconv.Atoi(q.Get("offset"))
	ops, total, err := s.Banks.ListOperations(id, oq)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bank_id": id, "operations": ops, "total": total})
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	op, err := s.Banks.GetOperation(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	op, err := s.Banks.CancelOperation(id, r.PathValue("id"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "operation_id": op.ID, "status": op.Status})
}

// ------------------------------------------------------------ webhooks

func (s *Server) listBankWebhooks(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r) // registrations carry URLs; managing them is a write
	if !ok {
		return
	}
	// A name in the commons passes the write check whether or not the bank
	// exists, so the list must confirm existence itself. Otherwise an absent
	// bank answered 200 with an empty list while a hidden one answered 404.
	if !s.bankExists(w, id) {
		return
	}
	s.listWebhooksFor(w, id)
}

func (s *Server) listWebhooksFor(w http.ResponseWriter, bankID string) {
	hooks, err := s.Banks.ListWebhooks(bankID)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": hooks, "total": len(hooks)})
}

func (s *Server) createBankWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok || !s.bankExists(w, id) {
		return
	}
	s.createWebhookFor(w, r, id)
}

func (s *Server) createWebhookFor(w http.ResponseWriter, r *http.Request, bankID string) {
	var in struct {
		bank.WebhookSpec
		EventTypes *[]string `json:"event_types"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Events == nil {
		in.Events = in.EventTypes
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	wh, err := s.Banks.CreateWebhook(ctx, bankID, in.WebhookSpec)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, wh)
}

func (s *Server) updateBankWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	s.updateWebhookFor(w, r, id)
}

func (s *Server) updateWebhookFor(w http.ResponseWriter, r *http.Request, bankID string) {
	var in struct {
		bank.WebhookSpec
		EventTypes *[]string `json:"event_types"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Events == nil {
		in.Events = in.EventTypes
	}
	wh, err := s.Banks.UpdateWebhook(r.Context(), bankID, r.PathValue("id"), in.WebhookSpec)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wh)
}

func (s *Server) deleteBankWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	s.deleteWebhookFor(w, r, id)
}

func (s *Server) deleteWebhookFor(w http.ResponseWriter, r *http.Request, bankID string) {
	if err := s.Banks.DeleteWebhook(bankID, r.PathValue("id")); err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

func (s *Server) bankWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	s.deliveriesFor(w, r, id)
}

func (s *Server) deliveriesFor(w http.ResponseWriter, r *http.Request, bankID string) {
	d, err := s.Banks.ListDeliveries(bankID, r.PathValue("id"), clampLimit(r.URL.Query().Get("limit"), 50, 200))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": d})
}

func (s *Server) listGlobalWebhooks(w http.ResponseWriter, r *http.Request) { s.listWebhooksFor(w, "") }
func (s *Server) createGlobalWebhook(w http.ResponseWriter, r *http.Request) {
	s.createWebhookFor(w, r, "")
}
func (s *Server) updateGlobalWebhook(w http.ResponseWriter, r *http.Request) {
	s.updateWebhookFor(w, r, "")
}
func (s *Server) deleteGlobalWebhook(w http.ResponseWriter, r *http.Request) {
	s.deleteWebhookFor(w, r, "")
}
func (s *Server) globalWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	s.deliveriesFor(w, r, "")
}

// ------------------------------------------------------------ templates

func (s *Server) listBankTemplates(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": bank.BuiltinTemplates})
}

func (s *Server) getBankTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	t, ok := bank.BuiltinTemplate(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such template")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) exportBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankReadable(w, r)
	if !ok {
		return
	}
	m, err := s.Banks.ExportTemplate(id)
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// importBank applies a manifest — sent as the body, as {"manifest": …}, or
// named as {"template": "<built-in id>"} — creating the bank if needed.
func (s *Server) importBank(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	var raw map[string]json.RawMessage
	if !decodeJSON(w, r, &raw) {
		return
	}
	var m bank.Manifest
	switch {
	case raw["template"] != nil:
		var name string
		_ = json.Unmarshal(raw["template"], &name)
		t, found := bank.BuiltinTemplate(name)
		if !found {
			writeErr(w, http.StatusBadRequest, "no built-in template "+strconv.Quote(name))
			return
		}
		m = t.Manifest
	case raw["manifest"] != nil:
		if err := json.Unmarshal(raw["manifest"], &m); err != nil {
			writeErr(w, http.StatusBadRequest, "manifest is not valid")
			return
		}
	default:
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &m); err != nil {
			writeErr(w, http.StatusBadRequest, "manifest is not valid")
			return
		}
	}
	res, err := s.Banks.ImportTemplate(id, m, boolParam(r, "dry_run"))
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ------------------------------------------------------------ reranker

// settingsReranker builds the reranker the settings select, and rebuilds it
// when they change, so switching rerank modes in the console needs no
// restart. "auto" uses a local model only when it is already on disk: a
// recall never waits on a 90 MB download it was not asked for. rerank=local
// is the explicit ask, and fetches the model on first use.
type settingsReranker struct {
	s       *Server
	dataDir string

	mu  chan struct{}
	sig string
	cur rerank.Reranker
	err error
}

func newSettingsReranker(s *Server, dataDir string) *settingsReranker {
	return &settingsReranker{s: s, dataDir: dataDir, mu: make(chan struct{}, 1)}
}

func (sr *settingsReranker) current() (rerank.Reranker, error) {
	st := sr.s.Settings
	if st == nil {
		return nil, nil
	}
	keys := []string{rerank.SettingMode, rerank.SettingModel, rerank.SettingURL, rerank.SettingAPIKey, rerank.SettingMaxLen}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = st.Get(k)
	}
	sig := strings.Join(parts, "\x00")
	sr.mu <- struct{}{}
	defer func() { <-sr.mu }()
	if sig != sr.sig {
		mode := strings.ToLower(strings.TrimSpace(st.Get(rerank.SettingMode)))
		sr.cur, sr.err = rerank.New(st, rerank.Options{DataDir: sr.dataDir, AllowDownload: mode == "local"})
		sr.sig = sig
	}
	return sr.cur, sr.err
}

// Name reports the reranker in use, for recall traces.
func (sr *settingsReranker) Name() string {
	r, _ := sr.current()
	if r == nil {
		return "none"
	}
	return r.Name()
}

// Score scores with the configured reranker; with none it declines (nil,
// nil) and recall keeps its fused order.
func (sr *settingsReranker) Score(ctx context.Context, q string, docs []string) ([]float32, error) {
	r, err := sr.current()
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	return r.Score(ctx, q, docs)
}

var _ bank.Reranker = (*settingsReranker)(nil)

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
