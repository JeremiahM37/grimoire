package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/cues"
	"github.com/JeremiahM37/grimoire/go/internal/decide"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// cues returns the cue store, opening it on first use. Cues live in the data
// directory beside the index: they are derived from memories and from how
// agents used them, not part of anyone's notes. A store that fails to open
// leaves retrieval exactly as it was without cues.
func (s *Server) cues() *cues.Store {
	s.cueOnce.Do(func() {
		if s.Vault == nil || s.Index == nil {
			return
		}
		st, err := cues.Open(filepath.Join(s.Vault.Root, ".grimoire"), s.Index.Emb)
		if err != nil {
			log.Printf("cues: %v", err)
			return
		}
		s.cueStore = st
	})
	return s.cueStore
}

// recentQueries remembers the last context query each agent made, so a
// remember that turns out to be a re-tell can be tied to the moment that
// should have surfaced the memory even when the agent did not say what it was
// doing. One entry per agent; it is a hint, not a record.
type recentQueries struct {
	mu        sync.Mutex
	by        map[string]recentQuery
	bySession map[string]recentQuery
}

// maxSessions bounds the per-session memory of the last prompt.
const maxSessions = 512

// swapSession records a session's newest prompt and returns the one before
// it, if that one is recent. Sessions are opaque keys (the hook sends a hash).
func (q *recentQueries) swapSession(session, text string) string {
	if session == "" || strings.TrimSpace(text) == "" {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.bySession == nil {
		q.bySession = map[string]recentQuery{}
	}
	prev, ok := q.bySession[session]
	if len(q.bySession) >= maxSessions {
		for k, v := range q.bySession {
			if time.Since(v.at) > retellWindow {
				delete(q.bySession, k)
			}
		}
		if len(q.bySession) >= maxSessions {
			q.bySession = map[string]recentQuery{}
		}
	}
	q.bySession[session] = recentQuery{text: text, at: time.Now()}
	if !ok || time.Since(prev.at) > retellWindow || prev.text == text {
		return ""
	}
	return prev.text
}

type recentQuery struct {
	text string
	at   time.Time
}

// retellWindow is how long after a context query a re-tell is still taken to
// belong to it.
const retellWindow = 30 * time.Minute

func (q *recentQueries) note(agent, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.by == nil {
		q.by = map[string]recentQuery{}
	}
	rq := recentQuery{text: text, at: time.Now()}
	q.by[agent] = rq
	q.by[anyAgent] = rq
}

// anyAgent keys the newest query from anyone. The prompt hook usually carries
// no agent name while the MCP server does, so on a single-user deployment the
// newest prompt overall is the right fallback.
const anyAgent = "\x00any"

func (q *recentQueries) last(agent string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, k := range []string{agent, anyAgent} {
		if rq, ok := q.by[k]; ok && time.Since(rq.at) <= retellWindow {
			return rq.text
		}
	}
	return ""
}

// learnFromRetell records a learned cue on a fact an agent tried to record
// again. Being told something the store already held means the memory did not
// reach the agent when it needed it; the situation it was in is the cue that
// would have brought it. Only trusted writers teach cues, for the same reason
// only they may confirm a fact.
func (s *Server) learnFromRetell(r *http.Request, factID, agent, context string) {
	st := s.cues()
	if st == nil || factID == "" {
		return
	}
	if context = strings.TrimSpace(context); context == "" {
		context = s.recent.last(agent)
	}
	if context == "" {
		return
	}
	if _, err := st.Add([]cues.Cue{{Target: cues.FactTarget(factID), Kind: cues.Request,
		Text: context, Source: cues.Learned}}); err != nil {
		log.Printf("cues: learn: %v", err)
	}
	s.adherenceRetold(cues.FactTarget(factID))
}

// addAgentCues attaches the cues a writer supplied to the fact it added.
func (s *Server) addAgentCues(factID string, m memoryIn) {
	st := s.cues()
	if st == nil || factID == "" || len(m.Cues) == 0 {
		return
	}
	if o := strings.TrimSpace(m.Origin); o != "" && (memory.Entry{Origin: o}).Untrusted() {
		return
	}
	var cs []cues.Cue
	for i, c := range m.Cues {
		if i == 8 {
			break
		}
		kind := cues.Request
		if looksLikeAction(c) {
			kind = cues.Action
		}
		cs = append(cs, cues.Cue{Target: cues.FactTarget(factID), Kind: kind, Text: c, Source: cues.Agent})
	}
	if _, err := st.Add(cs); err != nil {
		log.Printf("cues: add: %v", err)
	}
}

// looksLikeAction tells a command or path from a request by its shape.
func looksLikeAction(c string) bool {
	c = strings.TrimSpace(c)
	return strings.HasPrefix(c, "/") || strings.HasPrefix(c, "~/") || strings.HasPrefix(c, "./") ||
		(!strings.Contains(c, " ") && strings.Contains(c, ".")) ||
		strings.ContainsAny(c, "|$`") || strings.Contains(c, " --")
}

type cuesIn struct {
	Target string   `json:"target"` // fact:<id> or note:<path>
	Cues   []string `json:"cues"`
	Kind   string   `json:"kind"`   // request | action | keywords; default by shape
	Source string   `json:"source"` // agent | learned | generated; default agent
}

// addCues attaches cues to a fact or note. It is how a hook reports a
// re-tell it detected, how a dream pass stores cues it generated, and how a
// person adds one by hand.
func (s *Server) addCues(w http.ResponseWriter, r *http.Request) {
	var in cuesIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON {target, cues}")
		return
	}
	if !strings.HasPrefix(in.Target, "fact:") && !strings.HasPrefix(in.Target, "note:") {
		writeErr(w, http.StatusBadRequest, "target must be fact:<id> or note:<path>")
		return
	}
	if len(in.Cues) == 0 || len(in.Cues) > 16 {
		writeErr(w, http.StatusBadRequest, "cues must hold 1..16 entries")
		return
	}
	switch in.Source {
	case "":
		in.Source = cues.Agent
	case cues.Agent, cues.Learned, cues.Generated:
	default:
		writeErr(w, http.StatusBadRequest, "source must be agent, learned or generated")
		return
	}
	if in.Kind != "" && in.Kind != cues.Request && in.Kind != cues.Action && in.Kind != cues.Keywords {
		writeErr(w, http.StatusBadRequest, "kind must be request, action or keywords")
		return
	}
	if _, ok := s.cueTargetItem(r, in.Target, nil); !ok {
		writeErr(w, http.StatusNotFound, "no current fact or readable note by that target")
		return
	}
	st := s.cues()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "cue store unavailable")
		return
	}
	var cs []cues.Cue
	for _, c := range in.Cues {
		kind := in.Kind
		if kind == "" {
			kind = cues.Request
			if looksLikeAction(c) {
				kind = cues.Action
			}
		}
		cs = append(cs, cues.Cue{Target: in.Target, Kind: kind, Text: c, Source: in.Source})
	}
	n, err := st.Add(cs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": in.Target, "added": n})
}

// listCues shows a target's cues, so a person can see why a memory fires.
func (s *Server) listCues(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if _, ok := s.cueTargetItem(r, target, nil); !ok {
		writeErr(w, http.StatusNotFound, "no current fact or readable note by that target")
		return
	}
	out := []cues.Cue{}
	if st := s.cues(); st != nil {
		out = append(out, st.For(target)...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": target, "cues": out})
}

// retellRE matches explicit restatement: the user saying they have said
// this before. Broader correction words ("don't", "never", "again") are far
// too common in ordinary prompts: replayed over 2,004 real prompts, they
// taught 92 cues and nearly all were wrong (docs/MEMORY_USE.md).
var retellRE = regexp.MustCompile(`(?i)\b(i (already |just )?(told|said|asked|mentioned)( (to )?you)?|like i said|as i (said|mentioned)|remember( that| when| i)|we (already|talked|discussed|agreed)|i'?ve (told|said|asked)|how many times|keep (forgetting|doing))\b`)

// retellMinRel and retellMinOverlap are how strongly a prompt must match a
// memory, by meaning and by the memory's own words, before a restatement is
// taken as restating that memory rather than mentioning its topic.
const (
	retellMinRel     = 0.85
	retellMinOverlap = 0.35
)

// learnFromPrompt spots a re-tell in the prompt stream: a prompt that
// restates a memory on file. The session's previous prompt is the situation
// where that memory should have reached the agent, so it becomes a learned
// cue. This covers every kind of memory, including notes the agent's own
// harness wrote, which a remember NOOP never sees.
//
// Real re-tells are paraphrases ("use opus to coordinate and sonnet to
// implement, we're burning tokens"), so no word rule finds them without also
// firing on ordinary prompts. With a re-tell judge configured, any prompt
// that matches a memory reasonably well is put to the judge in the
// background; without one, only an explicit restatement that carries the
// memory's own words counts.
func (s *Server) learnFromPrompt(prev, query string, items []contextItem) {
	if prev == "" || s.cues() == nil {
		return
	}
	if judge := s.retellClient(); judge != nil {
		best := bestItem(items, retellCandidateRel, "", 0)
		if best < 0 {
			return
		}
		it := items[best]
		go s.judgeRetell(judge, prev, query, it)
		return
	}
	if !retellRE.MatchString(query) {
		return
	}
	if best := bestItem(items, retellMinRel, query, retellMinOverlap); best >= 0 {
		s.learnCue(items[best], prev)
	}
}

// retellCandidateRel is how well a prompt must match a memory before the
// judge is asked about it.
const retellCandidateRel = 0.6

// bestItem returns the highest-scoring item at or above minRel whose own
// terms the query carries at least minOverlap of (when query is given).
func bestItem(items []contextItem, minRel float64, query string, minOverlap float64) int {
	best := -1
	for i, it := range items {
		if it.score < minRel || (best >= 0 && it.score <= items[best].score) {
			continue
		}
		if query != "" && contextOverlap(contextTerms(it.Text), query) < minOverlap {
			continue
		}
		best = i
	}
	return best
}

func (s *Server) learnCue(it contextItem, prev string) {
	target := cues.NoteTarget(it.Path)
	if it.ID != "" {
		target = cues.FactTarget(it.ID)
	}
	if _, err := s.cues().Add([]cues.Cue{{Target: target, Kind: cues.Request, Text: prev, Source: cues.Learned}}); err != nil {
		log.Printf("cues: learn from prompt: %v", err)
	}
	s.adherenceRetold(target)
}

var retellQuestion = map[string]decide.Question{"retell": {
	Type: decide.Noul,
	Instructions: "Is the user's message restating, reminding the agent of, or correcting it with something " +
		"this stored memory already says (so the agent should have known it)? A message that only shares " +
		"the memory's topic, asks about it, or gives a new instruction is not.",
	Criteria: map[string]string{
		"true":  "the message restates or re-asserts what the memory says",
		"false": "the message is new information, a question, or only on the same topic",
	},
}}

// judgeRetell asks the re-tell judge about one prompt and memory, and learns
// the previous prompt as a cue on a confident yes. Runs off the request path.
func (s *Server) judgeRetell(c *decide.Client, prev, query string, it contextItem) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*decide.DefaultTimeout)
	defer cancel()
	clip := func(t string) string {
		if r := []rune(t); len(r) > 1200 {
			return string(r[:1200])
		}
		return t
	}
	state := "Stored memory:\n" + clip(it.Text) + "\n\nUser's message to the agent:\n" + clip(query)
	started := time.Now()
	res, err := c.Ask(ctx, state, retellQuestion)
	s.bookDecision(c, res, started, err)
	if err != nil {
		return
	}
	a, ok := res.Answers["retell"]
	if !ok || a.Type != decide.Noul {
		return
	}
	threshold := 0.9
	if v, err := strconv.ParseFloat(s.setting("retell_threshold"), 64); err == nil && v > 0 && v < 1 {
		threshold = v
	}
	if a.Noul >= threshold {
		s.learnCue(it, prev)
	}
}

// retellClient builds the re-tell judge from settings, or nil when none is
// configured. The key comes from retell_api_key, the vault's
// 'retell-api-key', or failing those the decision model's key.
func (s *Server) retellClient() *decide.Client {
	url := strings.TrimSpace(s.setting("retell_url"))
	if url == "" {
		return nil
	}
	key := strings.TrimSpace(s.setting("retell_api_key"))
	for _, name := range []string{"retell-api-key", "decision-api-key"} {
		if key != "" || s.Secrets == nil {
			break
		}
		if v, err := s.Secrets.Get(name); err == nil {
			key = v
		}
	}
	if key == "" {
		key = strings.TrimSpace(s.setting("decision_api_key"))
	}
	return &decide.Client{BaseURL: url, Model: strings.TrimSpace(s.setting("retell_model")),
		APIKey: key, HTTP: &http.Client{Timeout: 2 * decide.DefaultTimeout}}
}

// WarmCues opens the cue store ahead of the first request.
func (s *Server) WarmCues() { s.cues() }
