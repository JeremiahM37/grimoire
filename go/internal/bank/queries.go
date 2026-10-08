package bank

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// FactQuery filters a fact listing.
type FactQuery struct {
	Type     string
	Document string
	Query    string // case-insensitive substring of the text
	Human    *bool
	Limit    int
	Offset   int
}

// ListFacts lists a bank's facts in file order.
func (e *Engine) ListFacts(bankID string, q FactQuery) ([]RecallFact, int, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, 0, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, 0, err
	}
	needle := strings.ToLower(strings.TrimSpace(q.Query))
	var out []RecallFact
	total := 0
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	for i := range c.units {
		u := &c.units[i]
		if q.Type != "" && u.Type != q.Type || q.Document != "" && u.Doc != q.Document {
			continue
		}
		if q.Human != nil && u.Human != *q.Human {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(u.Text), needle) {
			continue
		}
		total++
		if total <= q.Offset || len(out) >= limit {
			continue
		}
		out = append(out, c.factOut(bankID, int32(i)))
	}
	if out == nil {
		out = []RecallFact{}
	}
	return out, total, nil
}

// GetFact returns one fact.
func (e *Engine) GetFact(bankID, id string) (*RecallFact, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	p, ok := c.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	f := c.factOut(bankID, p)
	return &f, nil
}

// DeleteFact removes one fact from its facts file. A fact a person wrote or
// edited is removed only with force — retracting someone's own correction
// must be a deliberate act, not a side effect of a cleanup.
func (e *Engine) DeleteFact(bankID, id string, force bool) error {
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	c, err := e.cache(bankID)
	if err != nil {
		return err
	}
	p, ok := c.byID[id]
	if !ok {
		return ErrNotFound
	}
	u := c.units[p]
	n, err := e.Vault.Read(u.Path)
	if err != nil {
		return ErrNotFound
	}
	ff := ParseFacts(n.Body, bankID, u.Doc)
	var keep []Fact
	found := false
	for _, f := range ff.Facts {
		if f.ID == id {
			if f.IsHuman() && !force {
				return ErrHumanProtected
			}
			found = true
			continue
		}
		keep = append(keep, f)
	}
	if !found {
		return ErrNotFound
	}
	ff.Facts = keep
	return e.rewriteFacts(bankID, u.Doc, u.Path, n.Body, ff)
}

func (e *Engine) rewriteFacts(bankID, doc, rel, oldBody string, ff FactsFile) error {
	if e.History != nil {
		e.History.Snapshot(rel, oldBody)
	}
	if _, err := e.Vault.Write(rel, FormatFacts(doc, ff), e.keepFrontmatter(rel, factsFrontmatter(bankID, doc))); err != nil {
		return err
	}
	_, err := e.Index.Upsert(rel)
	return err
}

// EntitySummary is one entity in a listing.
type EntitySummary struct {
	ID        string   `json:"entity_id"`
	Name      string   `json:"canonical_name"`
	Mentions  int      `json:"mention_count"`
	Aliases   []string `json:"aliases,omitempty"`
	FirstSeen string   `json:"first_seen,omitempty"`
	LastSeen  string   `json:"last_seen,omitempty"`
}

func (c *bankCache) entitySummary(ei int32) EntitySummary {
	en := c.entities[ei]
	s := EntitySummary{ID: en.ID, Name: en.Name, Mentions: len(en.units), Aliases: en.Aliases}
	if en.first != 0 {
		s.FirstSeen = fromMS(en.first).Format("2006-01-02T15:04:05Z")
	}
	if en.last != 0 {
		s.LastSeen = fromMS(en.last).Format("2006-01-02T15:04:05Z")
	}
	return s
}

// ListEntities lists a bank's entities, most mentioned first.
func (e *Engine) ListEntities(bankID, query string, limit int) ([]EntitySummary, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	var out []EntitySummary
	for i := range c.entities {
		if needle != "" && !strings.Contains(strings.ToLower(c.entities[i].Name), needle) {
			continue
		}
		out = append(out, c.entitySummary(int32(i)))
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Mentions != out[b].Mentions {
			return out[a].Mentions > out[b].Mentions
		}
		return out[a].Name < out[b].Name
	})
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []EntitySummary{}
	}
	return out, nil
}

// EntityDetail is an entity with the facts that mention it.
type EntityDetail struct {
	EntitySummary
	Related []EntitySummary `json:"related"`
	Facts   []RecallFact    `json:"facts"`
}

// GetEntity returns one entity by id or by name.
func (e *Engine) GetEntity(bankID, idOrName string, limit int) (*EntityDetail, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	ei, ok := c.entByLow[strings.ToLower(idOrName)]
	if !ok {
		ei = -1
		for i, en := range c.entities {
			if en.ID == idOrName {
				ei = int32(i)
				break
			}
		}
		if ei < 0 {
			return nil, ErrNotFound
		}
	}
	d := &EntityDetail{EntitySummary: c.entitySummary(ei), Related: []EntitySummary{}, Facts: []RecallFact{}}
	if limit <= 0 {
		limit = 50
	}
	co := map[int32]int{}
	us := c.entities[ei].units
	for i := len(us) - 1; i >= 0; i-- {
		if len(d.Facts) < limit {
			d.Facts = append(d.Facts, c.factOut(bankID, us[i]))
		}
		for _, o := range c.units[us[i]].ents {
			if o != ei {
				co[o]++
			}
		}
	}
	var rel []int32
	for o := range co {
		rel = append(rel, o)
	}
	sort.Slice(rel, func(a, b int) bool {
		if co[rel[a]] != co[rel[b]] {
			return co[rel[a]] > co[rel[b]]
		}
		return c.entities[rel[a]].Name < c.entities[rel[b]].Name
	})
	for _, o := range rel[:min(len(rel), 20)] {
		d.Related = append(d.Related, c.entitySummary(o))
	}
	return d, nil
}

// DocumentSummary is one document in a listing.
type DocumentSummary struct {
	ID        string   `json:"document_id"`
	Path      string   `json:"path"`
	Timestamp string   `json:"timestamp,omitempty"`
	Context   string   `json:"context,omitempty"`
	Tags      []string `json:"tags"`
	Chunks    int      `json:"chunks"`
	Chars     int      `json:"chars"`
	Facts     int      `json:"facts"`
	Updated   string   `json:"updated,omitempty"`
}

// ListDocuments lists a bank's documents.
func (e *Engine) ListDocuments(bankID string, limit, offset int) ([]DocumentSummary, int, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 100
	}
	total, err := e.Index.DB.Count("SELECT COUNT(*) FROM bank_documents WHERE bank=?", bankID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := e.Index.DB.Query("SELECT doc,path,timestamp,context,tags,chunks,chars,updated FROM bank_documents"+
		" WHERE bank=? ORDER BY updated DESC, doc LIMIT ? OFFSET ?", bankID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	var out []DocumentSummary
	for rows.Next() {
		var d DocumentSummary
		var tags string
		if err := rows.Scan(&d.ID, &d.Path, &d.Timestamp, &d.Context, &tags, &d.Chunks, &d.Chars, &d.Updated); err != nil {
			rows.Close()
			return nil, 0, err
		}
		d.Tags = splitList(tags)
		if d.Tags == nil {
			d.Tags = []string{}
		}
		out = append(out, d)
	}
	rows.Close()
	c, err := e.cache(bankID)
	if err == nil {
		counts := map[string]int{}
		for i := range c.units {
			counts[c.units[i].Doc]++
		}
		for i := range out {
			out[i].Facts = counts[out[i].ID]
		}
	}
	if out == nil {
		out = []DocumentSummary{}
	}
	return out, total, nil
}

// DocumentDetail is a document with its content and facts.
type DocumentDetail struct {
	Document
	Path      string       `json:"path"`
	FactsPath string       `json:"facts_path"`
	Facts     []RecallFact `json:"facts"`
}

// GetDocument returns one document.
func (e *Engine) GetDocument(bankID, doc string) (*DocumentDetail, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, err
	}
	rel := DocumentPath(bankID, doc)
	n, err := e.Vault.Read(rel)
	if err != nil {
		return nil, ErrNotFound
	}
	d := &DocumentDetail{Document: *ParseDocument(n.Frontmatter, n.Body, doc), Path: rel, FactsPath: FactsPath(bankID, doc)}
	facts, _, err := e.ListFacts(bankID, FactQuery{Document: doc, Limit: 100000})
	if err != nil {
		return nil, err
	}
	d.Facts = facts
	return d, nil
}

// DeleteDocument removes a document and the facts a model extracted from it.
// Facts a person wrote or edited stay, marked doc_removed, unless force: the
// document was the model's source, not the person's.
func (e *Engine) DeleteDocument(bankID, doc string, force bool) (keptHuman int, err error) {
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	docRel, factsRel := DocumentPath(bankID, doc), FactsPath(bankID, doc)
	n, err := e.Vault.Read(docRel)
	if err != nil {
		return 0, ErrNotFound
	}
	if e.History != nil {
		e.History.Snapshot(docRel, n.Body)
	}
	if err := e.Vault.Delete(docRel); err != nil {
		return 0, err
	}
	if err := e.Index.Remove(docRel); err != nil {
		return 0, err
	}
	fn, err := e.Vault.Read(factsRel)
	if err != nil {
		return 0, nil
	}
	ff := ParseFacts(fn.Body, bankID, doc)
	var keep []Fact
	if !force {
		for _, f := range ff.Facts {
			if f.IsHuman() {
				f.DocRemoved = true
				keep = append(keep, f)
			}
		}
	}
	if len(keep) == 0 && len(ff.Prose) == 0 {
		if e.History != nil {
			e.History.Snapshot(factsRel, fn.Body)
		}
		if err := e.Vault.Delete(factsRel); err != nil {
			return 0, err
		}
		return 0, e.Index.Remove(factsRel)
	}
	ff.Facts = keep
	return len(keep), e.rewriteFacts(bankID, doc, factsRel, fn.Body, ff)
}

// GetChunk returns one chunk by its id.
func (e *Engine) GetChunk(bankID, chunkID string) (*ChunkOut, error) {
	b, doc, idx, ok := ParseChunkID(chunkID)
	if !ok || b != bankID {
		return nil, ErrNotFound
	}
	var text string
	err := e.Index.DB.QueryRow("SELECT text FROM bank_chunks WHERE bank=? AND doc=? AND idx=?", bankID, doc, idx).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ChunkOut{ID: chunkID, Text: text, Index: idx, Document: doc}, nil
}
