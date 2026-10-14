package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
	"github.com/JeremiahM37/grimoire/go/internal/anchor/anchorlog"
	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/dream/secscan"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/skillexport"
	"github.com/JeremiahM37/grimoire/go/internal/skillgate"
)

// Written procedures: POST /api/memory/procedure.
//
// An agent that has just finished a multi-step task that worked writes it down
// with the checks that make it reusable: `pre` anchors that must hold before the
// steps, and `post` anchors that hold once they have worked. The same gate as
// anchors applies: a loopback caller that is not remote. The store runs every
// pre anchor and every slot-free post anchor once. A failing pre anchor stores
// nothing, so the writer can fix it. The procedure is a skill note, written the
// way `skills accept` writes one, so `skills check` and `skills export` read it.

const (
	procMaxBody     = 64 << 10
	procMaxSteps    = 20
	procMaxStepLen  = 400
	procMinName     = 3
	procMaxName     = 80
	procMaxWhen     = 400
	procMaxField    = 200
	procMaxAnchors  = 16
	procFilePrefix  = "procedure_"
	procSecretCheck = "secret"
)

// procCredentialRE are the narrow built-in patterns a step must not match. They
// catch a key or token written out as a value, nothing more.
var procCredentialRE = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(password|passwd|token|secret|api[_-]?key|authorization)\s*[=:]\s*\S{8,}`),
	regexp.MustCompile(`X-[A-Za-z-]*(Admin|Token|Key)[^\n]{0,4}:\s*\S{8,}`),
}

// procedureIn is the request body.
type procedureIn struct {
	Name         string           `json:"name"`
	When         string           `json:"when"`
	Steps        []string         `json:"steps"`
	Pre          []map[string]any `json:"pre"`
	Post         []map[string]any `json:"post"`
	Host         string           `json:"host"`
	Topic        string           `json:"topic"`
	Agent        string           `json:"agent"`
	Session      string           `json:"session"`
	CallerRemote bool             `json:"caller_remote"`
}

// procResult is one anchor's outcome in the response.
type procResult struct {
	Phase   string            `json:"phase"`
	Type    string            `json:"type"`
	Args    map[string]string `json:"args,omitempty"`
	Result  string            `json:"result"`
	Message string            `json:"message,omitempty"`
}

// writeProcedure handles POST /api/memory/procedure.
func (s *Server) writeProcedure(w http.ResponseWriter, r *http.Request) {
	var in procedureIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, procMaxBody)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.CallerRemote || !loopbackPeer(r) {
		writeErr(w, http.StatusForbidden, "procedures are written by the local agent only")
		return
	}
	p, pre, post, err := s.validateProcedure(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := s.canonicalMemoryDir()
	if dir == "" {
		writeErr(w, http.StatusServiceUnavailable, "no memory store: set memory_canonical_dir")
		return
	}
	if err := skillgate.CheckStore(dir); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "memory store unavailable: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	results := []procResult{}
	preOK := true
	procID := "procedure:" + skillexport.Slug(p.Name)
	for _, a := range pre {
		res := anchor.Run(ctx, a)
		recordObs(s.anchorLog(), procID, anchorlog.OriginWrite, []anchorObs{{a: a, res: res}})
		results = append(results, procResult{Phase: "pre", Type: a.Type, Args: a.Args,
			Result: string(res.Status), Message: res.Message})
		if res.Status != anchor.Pass && res.Status != anchor.Skipped {
			preOK = false
		}
	}
	if !preOK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"detail": "a pre check does not hold here, so nothing was stored; fix it and write again",
			"checks": results,
			"stored": false,
		})
		return
	}
	for _, a := range post {
		if skillgate.HasSlot(a) {
			results = append(results, procResult{Phase: "post", Type: a.Type, Args: a.Args,
				Result: "needs values", Message: "a slot is filled in when the procedure is used"})
			continue
		}
		res := anchor.Run(ctx, a)
		recordObs(s.anchorLog(), procID, anchorlog.OriginWrite, []anchorObs{{a: a, res: res}})
		results = append(results, procResult{Phase: "post", Type: a.Type, Args: a.Args,
			Result: string(res.Status), Message: res.Message})
	}

	file, replace, err := s.procedureFile(dir, p.Name)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	text, err := p.Note()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := skillgate.SaveNote(dir, file, text, replace)
	if err != nil {
		// Without replace the only refusal is a file already there.
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"stored":       true,
		"replaced":     replace,
		"file":         filepath.Base(path),
		"skill":        skillexport.Slug(p.Name),
		"checks":       results,
		"needs_values": skillgate.NeedsValues(p.Spec()),
	})
}

// validateProcedure checks the whole request before anything runs or is written.
func (s *Server) validateProcedure(in procedureIn) (skillgate.Procedure, []anchor.Anchor, []anchor.Anchor, error) {
	var none skillgate.Procedure
	name := strings.TrimSpace(in.Name)
	if n := len([]rune(name)); n < procMinName || n > procMaxName {
		return none, nil, nil, fmt.Errorf("name must be %d to %d characters", procMinName, procMaxName)
	}
	when := strings.TrimSpace(in.When)
	if when == "" {
		return none, nil, nil, fmt.Errorf("when must say when the procedure applies")
	}
	if len([]rune(when)) > procMaxWhen {
		return none, nil, nil, fmt.Errorf("when must be at most %d characters", procMaxWhen)
	}
	if n := len(in.Steps); n < 2 || n > procMaxSteps {
		return none, nil, nil, fmt.Errorf("steps must hold 2 to %d entries", procMaxSteps)
	}
	steps := make([]string, 0, len(in.Steps))
	for i, st := range in.Steps {
		st = strings.TrimSpace(st)
		switch {
		case st == "":
			return none, nil, nil, fmt.Errorf("step %d is empty", i+1)
		case len([]rune(st)) > procMaxStepLen:
			return none, nil, nil, fmt.Errorf("step %d is longer than %d characters", i+1, procMaxStepLen)
		case strings.ContainsAny(st, "\n\r"):
			return none, nil, nil, fmt.Errorf("step %d spans lines; one step is one line", i+1)
		case looksLikeCredential(st):
			return none, nil, nil, fmt.Errorf("step %d looks like it holds a credential; keep secrets out of procedures and use the credential vault", i+1)
		}
		steps = append(steps, st)
	}
	if looksLikeCredential(when) {
		return none, nil, nil, fmt.Errorf("when looks like it holds a credential; keep secrets out of procedures")
	}
	if len(in.Pre) == 0 || len(in.Post) == 0 {
		return none, nil, nil, fmt.Errorf("a procedure needs at least one pre and one post anchor")
	}
	if len(in.Pre)+len(in.Post) > procMaxAnchors {
		return none, nil, nil, fmt.Errorf("at most %d anchors per procedure", procMaxAnchors)
	}
	host := strings.ToLower(strings.TrimSpace(in.Host))
	if host != "" && !hostRE.MatchString(host) {
		return none, nil, nil, fmt.Errorf("invalid host name %q", host)
	}
	for label, v := range map[string]string{"topic": in.Topic, "agent": in.Agent, "session": in.Session} {
		if len(v) > procMaxField || strings.ContainsAny(v, "\n\r;") {
			return none, nil, nil, fmt.Errorf("%s must be one line of at most %d characters without ';'", label, procMaxField)
		}
	}
	pre, err := parseProcAnchors(in.Pre, "pre", false, host)
	if err != nil {
		return none, nil, nil, err
	}
	post, err := parseProcAnchors(in.Post, "post", true, host)
	if err != nil {
		return none, nil, nil, err
	}
	p := skillgate.Procedure{Name: name, When: when, Steps: steps, Pre: pre, Post: post,
		Host: host, Topic: strings.TrimSpace(in.Topic), Agent: strings.TrimSpace(in.Agent),
		Session: strings.TrimSpace(in.Session)}
	return p, pre, post, nil
}

// parseProcAnchors reads one list of anchor objects. Slots are allowed only in
// post. An anchor with no host of its own takes the procedure's host.
func parseProcAnchors(raws []map[string]any, phase string, allowSlots bool, host string) ([]anchor.Anchor, error) {
	var out []anchor.Anchor
	for i, raw := range raws {
		a, err := anchor.FromMap(raw)
		if err != nil {
			return nil, fmt.Errorf("%s anchor %d: %v", phase, i+1, err)
		}
		if !anchor.Storable(a.Type) || a.Type == anchor.Remote {
			// remote is never checked from here, so a procedure could not verify it.
			if a.Type == anchor.Remote {
				return nil, fmt.Errorf("%s anchor %d: \"remote\" cannot be checked; use a real check with \"host\" set to the machine it runs on", phase, i+1)
			}
			return nil, fmt.Errorf("%s anchor %d: %q is not a check this store can run", phase, i+1, a.Type)
		}
		if err := skillgate.ValidateShape(a, allowSlots); err != nil {
			return nil, fmt.Errorf("%s anchor %d: %v", phase, i+1, err)
		}
		out = append(out, a)
	}
	if host != "" {
		out = anchor.Inherit(out, host)
	}
	return out, nil
}

var procSlotRE = regexp.MustCompile(`<[a-z0-9_-]+>`)

// looksLikeCredential is the repo's secret scanner, plus the narrow patterns
// above. It reports only whether a credential-shaped value is present, never
// what matched.
func looksLikeCredential(text string) bool {
	// A slot such as <admin-token> stands for a value, it is not one.
	text = procSlotRE.ReplaceAllString(text, "")
	for _, re := range procCredentialRE {
		if re.MatchString(text) {
			return true
		}
	}
	doc := dream.Doc{Path: "memory/procedure.md", Body: text, Kind: dream.KindMemoryNote, Mtime: time.Now()}
	for _, f := range secscan.Scan([]dream.Doc{doc}, nil) {
		if f.Check == procSecretCheck || f.Check == "secret_value" {
			return true
		}
	}
	return false
}

// procedureFile picks the file a procedure is stored in. A note with the same
// skill name is replaced only when the route wrote it; any other one is a
// conflict. Otherwise the procedure gets a new file.
func (s *Server) procedureFile(dir, name string) (file string, replace bool, err error) {
	slug := skillexport.Slug(name)
	notes, err := memstore.LoadDir(dir)
	if err != nil {
		return "", false, fmt.Errorf("cannot read the memory store: %v", err)
	}
	for _, n := range notes {
		title := n.Title
		for _, k := range []string{"name", "title"} {
			if v := strings.TrimSpace(n.Fields[k]); v != "" {
				title = v
				break
			}
		}
		if skillexport.Slug(title) != slug {
			continue
		}
		if n.Fields["metadata.skill_source"] == skillgate.WrittenSource {
			return n.File, true, nil
		}
		return "", false, fmt.Errorf("a skill named %q already exists and was not written by this route", name)
	}
	return procFilePrefix + slug + ".md", false, nil
}

// procedureTally is the procedures part of the anchors report: how many written
// or mined procedures the store holds, how many carry a spec, and how their
// preconditions fare when run here now. A procedure whose pre checks are all
// skipped (not checkable from here) is in neither pre_pass nor pre_fail.
type procedureTally struct {
	Total    int `json:"total"`
	WithSpec int `json:"with_spec"`
	PrePass  int `json:"pre_pass"`
	PreFail  int `json:"pre_fail"`
}

// procedureTallyOf runs the pre checks of every procedure in the store that has
// a spec. An empty or missing store is an empty tally, not an error.
func (s *Server) procedureTallyOf(ctx context.Context) procedureTally {
	var t procedureTally
	dir := s.canonicalMemoryDir()
	if dir == "" || skillgate.CheckStore(dir) != nil {
		return t
	}
	notes, err := memstore.LoadDir(dir)
	if err != nil {
		return t
	}
	for _, n := range notes {
		if n.Kind != memstore.KindProcedure {
			continue
		}
		t.Total++
		spec, ok := skillgate.SpecOf(n)
		if !ok {
			continue
		}
		t.WithSpec++
		allPass, anyFail, ran := true, false, false
		for _, p := range spec.Preconditions {
			kind, arg, _ := strings.Cut(p, ":")
			if kind != "anchor" {
				continue
			}
			a, err := skillgate.ParseAnchorPredicate(arg)
			if err != nil {
				anyFail = true
				continue
			}
			res := anchor.Run(ctx, a)
			recordObs(s.anchorLog(), "procedure:"+skillexport.Slug(n.Title), anchorlog.OriginCLI, []anchorObs{{a: a, res: res}})
			switch res.Status {
			case anchor.Pass:
				ran = true
			case anchor.Skipped:
				allPass = false
			default:
				anyFail = true
			}
		}
		switch {
		case anyFail:
			t.PreFail++
		case ran && allPass:
			t.PrePass++
		}
	}
	return t
}
