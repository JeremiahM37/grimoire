package bank

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Mental models are standing answers to stored questions: a question ("what
// are Dana's working preferences?") whose answer the bank keeps written down
// and rewrites when what it knows changes. Each is a note under
// banks/<bank>/models/, and the folders under models/ are the knowledge-page
// tree: a model in models/people/dana.md is the page "dana" in the folder
// "people".
//
// The answer is the note's body, written by reflect over the bank. A person
// may edit it like any note — and then it is theirs: the model writes the
// hash of the body it last produced into the frontmatter (body_sum), so a
// body that no longer matches was changed by someone else, and a refresh
// that finds one does not overwrite it. It files the new answer as a
// proposal (banks/<bank>/proposals/<id>.md) that the person accepts or
// rejects.

// ErrModelRequired is returned by features that need a language model when
// none is configured.
var ErrModelRequired = errors.New("model_required")

// A model id is its path under models/ without ".md": "dana", or
// "people/dana" for the page "dana" in the folder "people". Each segment is
// lowercase letters, digits and ._- so an id is safe in a URL and a path.
var modelIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}(/[a-z0-9][a-z0-9._-]{0,63}){0,7}$`)
var folderSegRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidModelID reports whether id may name a mental model.
func ValidModelID(id string) bool { return modelIDRE.MatchString(id) && !strings.Contains(id, "..") }

func splitModelID(id string) (folder, stem string) {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[:i], id[i+1:]
	}
	return "", id
}

func joinModelID(folder, stem string) string {
	if folder == "" {
		return stem
	}
	return folder + "/" + stem
}

func cleanFolder(f string) (string, error) {
	f = strings.Trim(strings.TrimSpace(f), "/")
	if f == "" {
		return "", nil
	}
	parts := strings.Split(f, "/")
	if len(parts) > 8 {
		return "", invalid("folder is nested too deep")
	}
	for _, p := range parts {
		if !folderSegRE.MatchString(p) || p == "." || p == ".." {
			return "", invalid("folder segment %q is not allowed", p)
		}
	}
	return strings.Join(parts, "/"), nil
}

// ModelPath is where a mental model lives.
func ModelPath(bankID, id string) string { return Prefix(bankID) + "models/" + id + ".md" }

// ProposalPath is where a refresh files an answer it may not write.
func ProposalPath(bankID, id string) string { return Prefix(bankID) + "proposals/" + id + ".md" }

// MentalModel is a model as the API shows it.
type MentalModel struct {
	ID        string   `json:"id"`
	BankID    string   `json:"bank_id"`
	Name      string   `json:"name"`
	Question  string   `json:"question"`
	Folder    string   `json:"folder"`
	Path      string   `json:"path"`
	Tags      []string `json:"tags"`
	Refresh   string   `json:"refresh"` // auto | manual
	MaxTokens int      `json:"max_tokens"`
	Budget    string   `json:"budget"`
	FactTypes []string `json:"fact_types,omitempty"`
	Body      string   `json:"body,omitempty"`
	Version   int      `json:"version"`
	// LastRefreshed is when a refresh last wrote, confirmed or proposed an
	// answer.
	LastRefreshed string   `json:"last_refreshed,omitempty"`
	BasedOn       []string `json:"based_on"`
	// Authority is "human" when a person wrote or edited the body since the
	// model last did, "agent" otherwise.
	Authority string `json:"authority"`
	IsStale   bool   `json:"is_stale"`
	// StaleReason explains IsStale: never_refreshed or memories_changed.
	StaleReason string    `json:"stale_reason,omitempty"`
	Proposal    *Proposal `json:"pending_proposal,omitempty"`
	Updated     string    `json:"updated,omitempty"`

	scopeSig string
	bodySum  string
}

// Proposal is an answer a refresh could not write over a person's text.
type Proposal struct {
	Body        string   `json:"content"`
	BasedOn     []string `json:"based_on"`
	ProposedAt  string   `json:"created_at"`
	BaseVersion int      `json:"base_version"`
	scopeSig    string
}

// ModelSpec creates or patches a model. Nil pointers are "not sent".
type ModelSpec struct {
	ID        string    `json:"id"`
	Name      *string   `json:"name"`
	Question  *string   `json:"question"`
	Folder    *string   `json:"folder"`
	Tags      *[]string `json:"tags"`
	Refresh   *string   `json:"refresh"`
	MaxTokens *int      `json:"max_tokens"`
	Budget    *string   `json:"budget"`
	FactTypes *[]string `json:"fact_types"`
	// Body is a person's own answer. Sent on create it makes the model a
	// person's from the start; sent on update it is an edit, and refresh
	// will only propose over it.
	Body *string `json:"body"`
}

func bodySum(body string) string {
	b := strings.ReplaceAll(strings.TrimSpace(body), "\r\n", "\n")
	if b == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(b))
	return hex.EncodeToString(sum[:])[:16]
}

func intOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func listOf(fm *markdown.Frontmatter, key string) []string {
	v, ok := fm.Get(key)
	if !ok {
		return []string{}
	}
	return valueList(v)
}

// parseModel reads a model note.
func parseModel(bankID, rel string, fm *markdown.Frontmatter, body string) *MentalModel {
	id := strings.TrimSuffix(strings.TrimPrefix(rel, Prefix(bankID)+"models/"), ".md")
	folder, stem := splitModelID(id)
	m := &MentalModel{ID: id, BankID: bankID, Folder: folder, Path: rel,
		Name: strings.TrimSpace(fm.StringVal("name")), Question: strings.TrimSpace(fm.StringVal("question")),
		Tags: listOf(fm, "model_tags"), Refresh: fm.StringVal("refresh"), Budget: fm.StringVal("budget"),
		MaxTokens: intOr(fm.StringVal("max_tokens"), DefaultModelMaxTokens), Version: intOr(fm.StringVal("version"), 0),
		LastRefreshed: fm.StringVal("last_refreshed"), BasedOn: listOf(fm, "based_on"),
		Body: strings.TrimSpace(body), Updated: fm.StringVal("updated"),
		scopeSig: fm.StringVal("scope_sig"), bodySum: fm.StringVal("body_sum")}
	if v, ok := fm.Get("fact_types"); ok {
		m.FactTypes = valueList(v)
	}
	if m.Name == "" {
		m.Name = stem
	}
	if m.Refresh != "manual" {
		m.Refresh = "auto"
	}
	if m.Budget == "" {
		m.Budget = "mid"
	}
	m.Authority = "agent"
	if m.edited() {
		m.Authority = "human"
	}
	return m
}

// edited reports whether a person wrote or changed the body since the model
// last wrote it: there is a body, and it is not the one the model recorded.
func (m *MentalModel) edited() bool {
	return m.Body != "" && bodySum(m.Body) != m.bodySum
}

// DefaultModelMaxTokens is a model answer's length target.
const DefaultModelMaxTokens = 2048

func (m *MentalModel) frontmatter(base *markdown.Frontmatter) *markdown.Frontmatter {
	fm := markdown.NewFrontmatter()
	if base != nil {
		fm = base.Clone()
	}
	fm.Set("title", oneLine(m.Name))
	fm.Set("bank", m.BankID)
	fm.Set("name", oneLine(m.Name))
	fm.Set("question", oneLine(m.Question))
	fm.Set("model_tags", listValue(m.Tags))
	fm.Set("refresh", m.Refresh)
	fm.Set("max_tokens", strconv.Itoa(m.MaxTokens))
	fm.Set("budget", m.Budget)
	if len(m.FactTypes) > 0 {
		fm.Set("fact_types", listValue(m.FactTypes))
	} else {
		fm.Delete("fact_types")
	}
	fm.Set("version", strconv.Itoa(m.Version))
	if m.LastRefreshed != "" {
		fm.Set("last_refreshed", m.LastRefreshed)
	}
	fm.Set("based_on", listValue(m.BasedOn))
	if m.scopeSig != "" {
		fm.Set("scope_sig", m.scopeSig)
	}
	if m.bodySum != "" {
		fm.Set("body_sum", m.bodySum)
	} else {
		fm.Delete("body_sum")
	}
	return fm
}

func validateModel(m *MentalModel) error {
	if strings.TrimSpace(m.Question) == "" {
		return invalid("question must not be empty")
	}
	switch m.Refresh {
	case "auto", "manual":
	default:
		return invalid("refresh must be auto or manual")
	}
	if m.MaxTokens < 256 || m.MaxTokens > 8192 {
		return invalid("max_tokens must be 256..8192")
	}
	if _, err := ThinkingBudget(m.Budget); err != nil {
		return err
	}
	for _, t := range m.FactTypes {
		switch t {
		case "world", "experience", "observation":
		default:
			return invalid("unknown fact type %q", t)
		}
	}
	return nil
}

func slugID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 48 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "model"
	}
	return s
}

// modelPathByID finds a model's file: ids are unique within a bank whatever
// folder they are in.
func (e *Engine) modelPathByID(bankID, id string) (string, error) {
	if !ValidModelID(id) {
		return "", ErrNotFound
	}
	rel := ModelPath(bankID, id)
	if _, err := e.Vault.Read(rel); err != nil {
		return "", ErrNotFound
	}
	return rel, nil
}

func (e *Engine) readModel(bankID, id string) (*MentalModel, *vault.Note, error) {
	rel, err := e.modelPathByID(bankID, id)
	if err != nil {
		return nil, nil, err
	}
	n, err := e.Vault.Read(rel)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	return parseModel(bankID, rel, n.Frontmatter, n.Body), n, nil
}

func (e *Engine) readProposal(bankID, id string) *Proposal {
	n, err := e.Vault.Read(ProposalPath(bankID, id))
	if err != nil {
		return nil
	}
	return &Proposal{Body: strings.TrimSpace(n.Body), BasedOn: listOf(n.Frontmatter, "based_on"),
		ProposedAt: n.Frontmatter.StringVal("proposed_at"), BaseVersion: intOr(n.Frontmatter.StringVal("base_version"), 0),
		scopeSig: n.Frontmatter.StringVal("scope_sig")}
}

// indexModel caches one model note for listing and for reflect's search.
func (e *Engine) indexModel(bankID string, note *vault.Note) error {
	m := parseModel(bankID, note.Path, note.Frontmatter, note.Body)
	if !ValidModelID(m.ID) {
		return nil // a stray note under models/ is just a note
	}
	text := m.Name + "\n" + m.Question + "\n" + runeCut(m.Body, 4000)
	vecs, err := e.embedTexts([]string{text})
	if err != nil {
		return err
	}
	var blob []byte
	if len(vecs) == 1 && len(vecs[0]) > 0 {
		blob = index.Pack(vecs[0])
	}
	db := e.Index.DB
	db.Lock()
	defer db.Unlock()
	return withTx(db.Conn(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM bank_models WHERE path=?", note.Path); err != nil {
			return err
		}
		res, err := tx.Exec("INSERT OR IGNORE INTO bank_models(bank,id,path,folder,name,question,tags,body,embedding)"+
			" VALUES(?,?,?,?,?,?,?,?,?)", bankID, m.ID, note.Path, m.Folder, m.Name, m.Question, joinList(m.Tags), m.Body, blob)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("mental model id %q already indexed", m.ID)
		}
		return nil
	})
}

// modelScope is the set of units a model's answer is drawn from.
func (m *MentalModel) inScope(u *unit) bool {
	if len(m.FactTypes) > 0 {
		ok := false
		for _, t := range m.FactTypes {
			if u.Type == t {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(m.Tags) > 0 {
		return tagsAllow(u.Tags, m.Tags, "all_strict")
	}
	return true
}

// scopeSignature fingerprints what a model can draw on: every in-scope
// unit's id and text. A refresh records it; when it no longer matches, the
// bank has learned or lost something the answer could depend on. Computed
// from the files' content alone, so a rebuild gives the same answer.
func (c *bankCache) scopeSignature(m *MentalModel) string {
	h := sha256.New()
	for i := range c.units {
		u := &c.units[i]
		if !m.inScope(u) {
			continue
		}
		h.Write([]byte(u.ID))
		h.Write([]byte{0})
		h.Write([]byte(u.Text))
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (e *Engine) markStale(c *bankCache, m *MentalModel) {
	switch {
	case m.LastRefreshed == "" || m.scopeSig == "":
		m.IsStale, m.StaleReason = true, "never_refreshed"
	case c != nil && c.scopeSignature(m) != m.scopeSig:
		m.IsStale, m.StaleReason = true, "memories_changed"
	}
}

// ModelQuery filters a model listing.
type ModelQuery struct {
	Tags      []string
	TagsMatch string
	Folder    string
	// Detail includes each model's body.
	Detail bool
}

// ListModels lists a bank's mental models, by folder then id.
func (e *Engine) ListModels(bankID string, q ModelQuery) ([]MentalModel, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	rows, err := e.Index.DB.Query("SELECT path FROM bank_models WHERE bank=? ORDER BY folder, id", bankID)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			paths = append(paths, p)
		}
	}
	rows.Close()
	c, _ := e.cache(bankID)
	match := q.TagsMatch
	if match == "" {
		match = "any"
	}
	out := []MentalModel{}
	for _, p := range paths {
		n, err := e.Vault.Read(p)
		if err != nil {
			continue
		}
		m := parseModel(bankID, p, n.Frontmatter, n.Body)
		if !tagsAllow(m.Tags, q.Tags, match) {
			continue
		}
		if q.Folder != "" && m.Folder != q.Folder && !strings.HasPrefix(m.Folder, q.Folder+"/") {
			continue
		}
		e.markStale(c, m)
		m.Proposal = e.readProposal(bankID, m.ID)
		if !q.Detail {
			m.Body = ""
			if m.Proposal != nil {
				m.Proposal = &Proposal{ProposedAt: m.Proposal.ProposedAt, BaseVersion: m.Proposal.BaseVersion, BasedOn: m.Proposal.BasedOn}
			}
		}
		out = append(out, *m)
	}
	return out, nil
}

// GetModel returns one model with its body, staleness and any proposal.
func (e *Engine) GetModel(bankID, id string) (*MentalModel, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	m, _, err := e.readModel(bankID, id)
	if err != nil {
		return nil, err
	}
	c, _ := e.cache(bankID)
	e.markStale(c, m)
	m.Proposal = e.readProposal(bankID, id)
	return m, nil
}

func (spec ModelSpec) apply(m *MentalModel) error {
	if spec.Name != nil {
		m.Name = oneLine(*spec.Name)
	}
	if spec.Question != nil {
		m.Question = oneLine(*spec.Question)
	}
	if spec.Tags != nil {
		m.Tags = unionTags(*spec.Tags)
	}
	if spec.Refresh != nil {
		m.Refresh = *spec.Refresh
	}
	if spec.MaxTokens != nil {
		m.MaxTokens = *spec.MaxTokens
	}
	if spec.Budget != nil {
		m.Budget = *spec.Budget
	}
	if spec.FactTypes != nil {
		m.FactTypes = append([]string{}, (*spec.FactTypes)...)
	}
	if spec.Folder != nil {
		f, err := cleanFolder(*spec.Folder)
		if err != nil {
			return err
		}
		m.Folder = f
	}
	return nil
}

// CreateModel creates a mental model. Its answer is written by a refresh;
// a body sent with the create is a person's own and is kept as theirs.
func (e *Engine) CreateModel(bankID string, spec ModelSpec) (*MentalModel, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	m := &MentalModel{BankID: bankID, Refresh: "auto", Budget: "mid", MaxTokens: DefaultModelMaxTokens,
		Tags: []string{}, BasedOn: []string{}}
	if err := spec.apply(m); err != nil {
		return nil, err
	}
	if m.Name == "" {
		m.Name = m.Question
	}
	if err := validateModel(m); err != nil {
		return nil, err
	}
	id := strings.Trim(strings.TrimSpace(spec.ID), "/")
	if id == "" {
		base := joinModelID(m.Folder, slugID(m.Name))
		id = base
		for i := 2; ; i++ {
			if _, err := e.modelPathByID(bankID, id); errors.Is(err, ErrNotFound) {
				break
			}
			id = fmt.Sprintf("%s-%d", base, i)
		}
	} else if !ValidModelID(id) {
		return nil, invalid("model id must be lowercase path segments of [a-z0-9._-], e.g. people/dana")
	} else if _, err := e.modelPathByID(bankID, id); err == nil {
		return nil, ErrExists
	}
	m.ID = id
	m.Folder, _ = splitModelID(id)
	body := ""
	if spec.Body != nil {
		body = strings.TrimSpace(*spec.Body)
	}
	rel := ModelPath(bankID, id)
	if _, err := e.Vault.Read(rel); err == nil {
		return nil, ErrExists
	}
	if _, err := e.Vault.Write(rel, body, m.frontmatter(nil)); err != nil {
		return nil, err
	}
	if _, err := e.Index.Upsert(rel); err != nil {
		return nil, err
	}
	m.Body = body
	m.Authority = "agent"
	if body != "" {
		m.Authority = "human"
	}
	m.Path = rel
	return m, nil
}

// UpdateModel patches a model's settings, moves it between folders, or
// replaces its body. A body sent here is a person's edit.
func (e *Engine) UpdateModel(bankID, id string, spec ModelSpec) (*MentalModel, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	m, n, err := e.readModel(bankID, id)
	if err != nil {
		return nil, err
	}
	oldRel := m.Path
	if err := spec.apply(m); err != nil {
		return nil, err
	}
	if err := validateModel(m); err != nil {
		return nil, err
	}
	body := m.Body
	if spec.Body != nil {
		body = strings.TrimSpace(*spec.Body)
	}
	_, stem := splitModelID(id)
	newID := joinModelID(m.Folder, stem)
	newRel := ModelPath(bankID, newID)
	if e.History != nil && body != m.Body {
		e.History.Snapshot(oldRel, n.Body)
	}
	if newRel != oldRel {
		if _, err := e.Vault.Read(newRel); err == nil {
			return nil, ErrExists
		}
		if _, err := e.Vault.Rename(oldRel, newRel); err != nil {
			return nil, err
		}
		if err := e.Index.Remove(oldRel); err != nil {
			return nil, err
		}
		// A pending proposal follows its model.
		if p, err := e.Vault.Read(ProposalPath(bankID, id)); err == nil {
			if _, err := e.Vault.Write(ProposalPath(bankID, newID), p.Body, p.Frontmatter); err != nil {
				return nil, err
			}
			if err := e.dropProposal(bankID, id); err != nil {
				return nil, err
			}
			if _, err := e.Index.Upsert(ProposalPath(bankID, newID)); err != nil {
				return nil, err
			}
		}
	}
	if _, err := e.Vault.Write(newRel, body, m.frontmatter(n.Frontmatter)); err != nil {
		return nil, err
	}
	if _, err := e.Index.Upsert(newRel); err != nil {
		return nil, err
	}
	return e.getModelLocked(bankID, newID)
}

func (e *Engine) getModelLocked(bankID, id string) (*MentalModel, error) {
	m, _, err := e.readModel(bankID, id)
	if err != nil {
		return nil, err
	}
	c, _ := e.cache(bankID)
	e.markStale(c, m)
	m.Proposal = e.readProposal(bankID, id)
	return m, nil
}

// DeleteModel removes a model and any pending proposal.
func (e *Engine) DeleteModel(bankID, id string) error {
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	_, n, err := e.readModel(bankID, id)
	if err != nil {
		return err
	}
	if e.History != nil {
		e.History.Snapshot(n.Path, n.Body)
	}
	if err := e.Vault.Delete(n.Path); err != nil {
		return err
	}
	if err := e.Index.Remove(n.Path); err != nil {
		return err
	}
	return e.dropProposal(bankID, id)
}

func (e *Engine) dropProposal(bankID, id string) error {
	rel := ProposalPath(bankID, id)
	if _, err := e.Vault.Read(rel); err != nil {
		return nil
	}
	if err := e.Vault.Delete(rel); err != nil {
		return err
	}
	return e.Index.Remove(rel)
}

// RefreshOutcome is what a refresh did.
type RefreshOutcome struct {
	ModelID string `json:"mental_model_id"`
	// Outcome is written (the answer changed and was written), unchanged
	// (the answer came out the same), proposed (a person had edited the
	// body, so the new answer waits as a proposal) or no_sources (nothing in
	// the bank is in the model's scope yet).
	Outcome string   `json:"outcome"`
	Version int      `json:"version"`
	BasedOn []string `json:"based_on"`
	Usage   Usage    `json:"usage"`
}

// RefreshModel rewrites a model's answer by reflecting on its question. A
// body a person edited is never overwritten: the new answer becomes a
// pending proposal instead.
func (e *Engine) RefreshModel(ctx context.Context, bankID, id string) (*RefreshOutcome, error) {
	if !e.AI.Available() {
		return nil, ErrModelRequired
	}
	m, _, err := e.readModel(bankID, id)
	if err != nil {
		return nil, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	sig := c.scopeSignature(m)
	out := &RefreshOutcome{ModelID: id, Version: m.Version, BasedOn: []string{}}
	inScope := 0
	for i := range c.units {
		if m.inScope(&c.units[i]) {
			inScope++
		}
	}
	if inScope == 0 {
		out.Outcome = "no_sources"
		return out, nil
	}
	match := "any"
	if len(m.Tags) > 0 {
		match = "all_strict"
	}
	maxTok := m.MaxTokens
	res, err := e.Reflect(ctx, bankID, ReflectRequest{
		Query: m.Question, Budget: m.Budget, MaxTokens: &maxTok, Tags: m.Tags, TagsMatch: match,
		FactTypes: m.FactTypes, ExcludeModelIDs: []string{id}, Agent: "mental-model-refresh",
		Context: fmt.Sprintf("You are writing the standing answer stored as %q. Include only what answers its "+
			"question; leave out retrieved material that is merely nearby. Keep concrete examples and dates, "+
			"organise it around the topic rather than around the sources, and when something changed over time "+
			"say since when the current state holds.", m.Name),
		RequireModel: true,
	})
	if err != nil {
		return nil, err
	}
	out.Usage = res.Usage
	text := strings.TrimSpace(res.Text)
	based := res.BasedOn.ids()
	out.BasedOn = based
	if text == "" {
		return nil, fmt.Errorf("refresh produced an empty answer")
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	// Re-read under the lock: a person may have edited the body while the
	// model was thinking, and that edit must win.
	m, n, err := e.readModel(bankID, id)
	if err != nil {
		return nil, err
	}
	now := e.now().Format(time.RFC3339)
	if m.edited() {
		fm := markdown.NewFrontmatter()
		fm.Set("title", "Proposal: "+oneLine(m.Name))
		fm.Set("bank", bankID)
		fm.Set("model_id", id)
		fm.Set("proposed_at", now)
		fm.Set("base_version", strconv.Itoa(m.Version))
		fm.Set("based_on", listValue(based))
		fm.Set("scope_sig", sig)
		rel := ProposalPath(bankID, id)
		if old, err := e.Vault.Read(rel); err == nil && e.History != nil {
			e.History.Snapshot(rel, old.Body)
		}
		if _, err := e.Vault.Write(rel, text, e.keepFrontmatter(rel, fm)); err != nil {
			return nil, err
		}
		if _, err := e.Index.Upsert(rel); err != nil {
			return nil, err
		}
		out.Outcome = "proposed"
		return out, nil
	}
	m.LastRefreshed, m.scopeSig, m.BasedOn = now, sig, based
	body := m.Body
	if strings.TrimSpace(body) == text {
		out.Outcome = "unchanged"
	} else {
		if e.History != nil && strings.TrimSpace(n.Body) != "" {
			e.History.Snapshot(n.Path, n.Body)
		}
		body = text
		m.Version++
		out.Outcome = "written"
	}
	m.bodySum = bodySum(body)
	if _, err := e.Vault.Write(n.Path, body, m.frontmatter(n.Frontmatter)); err != nil {
		return nil, err
	}
	if _, err := e.Index.Upsert(n.Path); err != nil {
		return nil, err
	}
	// A proposal superseded by an answer that could be written is moot.
	if err := e.dropProposal(bankID, id); err != nil {
		return nil, err
	}
	out.Version = m.Version
	return out, nil
}

// AcceptProposal makes a pending proposal the model's answer. The person who
// accepts it hands the body back to the model: later refreshes may rewrite
// it again.
func (e *Engine) AcceptProposal(bankID, id string) (*MentalModel, error) {
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	m, n, err := e.readModel(bankID, id)
	if err != nil {
		return nil, err
	}
	p := e.readProposal(bankID, id)
	if p == nil {
		return nil, ErrNotFound
	}
	if e.History != nil && strings.TrimSpace(n.Body) != "" {
		e.History.Snapshot(n.Path, n.Body)
	}
	m.Version++
	m.LastRefreshed = e.now().Format(time.RFC3339)
	m.BasedOn, m.scopeSig, m.bodySum = p.BasedOn, p.scopeSig, bodySum(p.Body)
	if _, err := e.Vault.Write(n.Path, p.Body, m.frontmatter(n.Frontmatter)); err != nil {
		return nil, err
	}
	if _, err := e.Index.Upsert(n.Path); err != nil {
		return nil, err
	}
	if err := e.dropProposal(bankID, id); err != nil {
		return nil, err
	}
	return e.getModelLocked(bankID, id)
}

// RejectProposal discards a pending proposal; the person's body stays.
func (e *Engine) RejectProposal(bankID, id string) error {
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	if _, _, err := e.readModel(bankID, id); err != nil {
		return err
	}
	if e.readProposal(bankID, id) == nil {
		return ErrNotFound
	}
	return e.dropProposal(bankID, id)
}

// ModelNode is one node of the knowledge-page tree.
type ModelNode struct {
	Kind     string       `json:"kind"` // folder | page
	Name     string       `json:"name"`
	Path     string       `json:"path"` // folder path, or folder/id for a page
	Model    *MentalModel `json:"model,omitempty"`
	Children []*ModelNode `json:"children,omitempty"`
}

// ModelTree arranges a bank's models by folder.
func (e *Engine) ModelTree(bankID string, q ModelQuery) ([]*ModelNode, error) {
	models, err := e.ListModels(bankID, q)
	if err != nil {
		return nil, err
	}
	root := &ModelNode{Kind: "folder"}
	folders := map[string]*ModelNode{"": root}
	var folderOf func(string) *ModelNode
	folderOf = func(p string) *ModelNode {
		if f, ok := folders[p]; ok {
			return f
		}
		parent := root
		if d := path.Dir(p); d != "." {
			parent = folderOf(d)
		}
		f := &ModelNode{Kind: "folder", Name: path.Base(p), Path: p}
		parent.Children = append(parent.Children, f)
		folders[p] = f
		return f
	}
	for i := range models {
		m := &models[i]
		f := folderOf(m.Folder)
		f.Children = append(f.Children, &ModelNode{Kind: "page", Name: m.Name, Path: m.ID, Model: m})
	}
	var sortTree func(n *ModelNode)
	sortTree = func(n *ModelNode) {
		sort.SliceStable(n.Children, func(a, b int) bool {
			if n.Children[a].Kind != n.Children[b].Kind {
				return n.Children[a].Kind == "folder"
			}
			return n.Children[a].Name < n.Children[b].Name
		})
		for _, c := range n.Children {
			sortTree(c)
		}
	}
	sortTree(root)
	if root.Children == nil {
		root.Children = []*ModelNode{}
	}
	return root.Children, nil
}

// ExportFile is one file of a markdown export.
type ExportFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ExportModels renders the bank's models as a self-contained markdown
// bundle: an index.md with the tree, and one file per model.
func (e *Engine) ExportModels(bankID string) ([]ExportFile, error) {
	tree, err := e.ModelTree(bankID, ModelQuery{Detail: true})
	if err != nil {
		return nil, err
	}
	var files []ExportFile
	var idx strings.Builder
	idx.WriteString("# Knowledge pages\n\n")
	var walk func(nodes []*ModelNode, depth int)
	walk = func(nodes []*ModelNode, depth int) {
		for _, n := range nodes {
			ind := strings.Repeat("  ", depth)
			if n.Kind == "folder" {
				idx.WriteString(ind + "- **" + n.Name + "/**\n")
				walk(n.Children, depth+1)
				continue
			}
			m := n.Model
			file := n.Path + ".md"
			idx.WriteString(fmt.Sprintf("%s- [%s](./%s) — %s\n", ind, m.Name, file, m.Question))
			var b strings.Builder
			b.WriteString("---\n")
			b.WriteString("id: " + m.ID + "\n")
			b.WriteString("title: " + quoteYAML(m.Name) + "\n")
			b.WriteString("question: " + quoteYAML(m.Question) + "\n")
			b.WriteString("tags: [" + strings.Join(m.Tags, ", ") + "]\n")
			b.WriteString(fmt.Sprintf("version: %d\n", m.Version))
			if m.LastRefreshed != "" {
				b.WriteString("refreshed: " + m.LastRefreshed + "\n")
			}
			b.WriteString("authority: " + m.Authority + "\n")
			b.WriteString("---\n\n")
			body := m.Body
			if body == "" {
				body = "_No content yet._"
			}
			b.WriteString(body + "\n")
			files = append(files, ExportFile{Path: file, Content: b.String()})
		}
	}
	walk(tree, 0)
	if len(files) == 0 {
		idx.WriteString("_No knowledge pages yet._\n")
	}
	return append([]ExportFile{{Path: "index.md", Content: idx.String()}}, files...), nil
}

func quoteYAML(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(oneLine(s)) + `"`
}

// staleAutoModels lists the auto-refreshing models a change could affect:
// those whose scope signature no longer matches.
func (e *Engine) staleAutoModels(bankID string) ([]string, error) {
	models, err := e.ListModels(bankID, ModelQuery{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range models {
		if m.Refresh == "auto" && m.IsStale {
			out = append(out, m.ID)
		}
	}
	return out, nil
}
