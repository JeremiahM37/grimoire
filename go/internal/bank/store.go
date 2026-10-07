package bank

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Snapshotter keeps a version of a file before it is rewritten.
type Snapshotter interface {
	Snapshot(rel, body string)
}

// Engine is the memory-bank service. It is safe for concurrent use.
type Engine struct {
	Index   *index.Index
	Vault   *vault.Vault
	AI      *ai.Client
	History Snapshotter
	// Reranker scores recall candidates against the query. Nil means none:
	// fused ranks are used as they are.
	Reranker Reranker
	// Now is indirected for tests.
	Now func() time.Time
	// ExtractConcurrency bounds parallel extraction calls per retain.
	ExtractConcurrency int
	// AllowPrivateWebhooks lets webhooks reach loopback and private
	// networks; read on every registration and delivery.
	AllowPrivateWebhooks func() bool
	// WebhookClient overrides the guarded client deliveries use (tests).
	WebhookClient *http.Client
	// WebhookDelays overrides the retry schedule (tests).
	WebhookDelays []time.Duration

	q           *ops
	deliverWake chan struct{}

	mu     sync.Mutex
	rev    map[string]int64
	caches map[string]*bankCache
	locks  map[string]*sync.Mutex
	build  map[string]*sync.Mutex
}

// New builds an engine over an index.
func New(ix *index.Index, v *vault.Vault, client *ai.Client, hist Snapshotter) *Engine {
	return &Engine{Index: ix, Vault: v, AI: client, History: hist, Now: time.Now,
		ExtractConcurrency: 8,
		rev:                map[string]int64{}, caches: map[string]*bankCache{},
		locks: map[string]*sync.Mutex{}, build: map[string]*sync.Mutex{}}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// bankLock serializes writes to one bank. Retain reads the bank's entities
// and existing facts, asks a model, and writes; two retains interleaving
// would each resolve entities against a state the other is changing.
func (e *Engine) bankLock(id string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.locks[id]
	if !ok {
		l = &sync.Mutex{}
		e.locks[id] = l
	}
	return l
}

func (e *Engine) bump(id string) {
	e.mu.Lock()
	e.rev[id]++
	e.mu.Unlock()
}

func (e *Engine) bumpAll() {
	e.mu.Lock()
	for k := range e.rev {
		e.rev[k]++
	}
	e.caches = map[string]*bankCache{}
	e.mu.Unlock()
}

const listSep = "\x1f"

func joinList(xs []string) string { return strings.Join(xs, listSep) }

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, listSep)
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// bkToken confines a full-text MATCH to one bank: a digits-only token that no
// stemmer alters and no fact text contains.
func bkToken(id string) string {
	h := fnv.New64a()
	h.Write([]byte(id))
	return fmt.Sprintf("bk%d", h.Sum64())
}

// ------------------------------------------------------------ index hooks

// ResetBanks drops every bank cache row ahead of a full rebuild. The
// embedding cache is kept: it is keyed by content, so it stays correct.
func (e *Engine) ResetBanks() error {
	for _, tbl := range []string{"bank_banks", "bank_documents", "bank_chunks", "bank_units",
		"bank_units_fts", "bank_entities", "bank_unit_entities", "bank_links", "bank_models"} {
		if err := e.Index.DB.Exec("DELETE FROM " + tbl); err != nil {
			return err
		}
	}
	e.bumpAll()
	return nil
}

// RemoveBankFile drops the rows a file produced.
func (e *Engine) RemoveBankFile(rel string) error {
	id, kind := ParsePath(rel)
	switch kind {
	case ProfileKind:
		if err := e.Index.DB.Exec("DELETE FROM bank_banks WHERE id=?", id); err != nil {
			return err
		}
	case DocumentKind:
		if err := e.Index.DB.Exec("DELETE FROM bank_documents WHERE path=?", rel); err != nil {
			return err
		}
		if err := e.Index.DB.Exec("DELETE FROM bank_chunks WHERE path=?", rel); err != nil {
			return err
		}
	case FactsKind, ObservationsKind:
		db := e.Index.DB
		db.Lock()
		err := withTx(db.Conn(), func(tx *sql.Tx) error { return deleteUnitsByPath(tx, rel) })
		db.Unlock()
		if err != nil {
			return err
		}
	case ModelKind:
		if err := e.Index.DB.Exec("DELETE FROM bank_models WHERE path=?", rel); err != nil {
			return err
		}
	default:
		return nil
	}
	e.bump(id)
	return nil
}

// IndexBankFile (re)builds the rows for one bank file. A file that does not
// parse is logged and skipped rather than failing the write or the rebuild
// that carried it: a person mid-edit must not be able to break indexing.
func (e *Engine) IndexBankFile(note *vault.Note) error {
	id, kind := ParsePath(note.Path)
	var err error
	switch kind {
	case ProfileKind:
		p := ParseProfile(id, note.Frontmatter, note.Body)
		err = e.Index.DB.Exec("INSERT OR REPLACE INTO bank_banks(id,path,name,updated) VALUES(?,?,?,?)",
			id, note.Path, p.Name, note.Frontmatter.StringVal("updated"))
	case DocumentKind:
		err = e.indexDocument(id, note)
	case FactsKind:
		err = e.indexFacts(id, note)
	case ObservationsKind:
		err = e.indexObservations(id, note)
	case ModelKind:
		err = e.indexModel(id, note)
	default:
		return nil
	}
	if err != nil {
		log.Printf("bank: indexing %s: %v", note.Path, err)
		return err
	}
	e.bump(id)
	return nil
}

// keepFrontmatter layers the keys this package owns over a file's existing
// frontmatter. A rewrite must not drop what a person added — a `readers:`
// list restricting who may open the bank, an alias, a tag — and the vault's
// patching writer removes any flat key the new frontmatter does not carry.
func (e *Engine) keepFrontmatter(rel string, ours *markdown.Frontmatter) *markdown.Frontmatter {
	n, err := e.Vault.Read(rel)
	if err != nil {
		return ours
	}
	merged := n.Frontmatter.Clone()
	for _, k := range ours.Keys() {
		v, _ := ours.Get(k)
		merged.Set(k, v)
	}
	return merged
}

func stemOf(rel string) string { return strings.TrimSuffix(path.Base(rel), ".md") }

func (e *Engine) indexDocument(bankID string, note *vault.Note) error {
	d := ParseDocument(note.Frontmatter, note.Body, stemOf(note.Path))
	size := d.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}
	chunks := Chunks(d.Content, size)
	meta, _ := json.Marshal(d.Metadata)
	ts := ""
	if !d.Timestamp.IsZero() {
		ts = d.Timestamp.Format(time.RFC3339)
	}
	db := e.Index.DB
	db.Lock()
	defer db.Unlock()
	return withTx(db.Conn(), func(tx *sql.Tx) error {
		for _, q := range []string{"DELETE FROM bank_documents WHERE path=?", "DELETE FROM bank_chunks WHERE path=?"} {
			if _, err := tx.Exec(q, note.Path); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM bank_documents WHERE bank=? AND doc=?", bankID, d.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM bank_chunks WHERE bank=? AND doc=?", bankID, d.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO bank_documents(bank,doc,path,timestamp,context,tags,metadata,chunks,chars,created,updated)"+
			" VALUES(?,?,?,?,?,?,?,?,?,?,?)", bankID, d.ID, note.Path, ts, d.Context, joinList(d.Tags),
			string(meta), len(chunks), runeLen(d.Content), d.Created, d.Updated); err != nil {
			return err
		}
		for _, c := range chunks {
			if _, err := tx.Exec("INSERT INTO bank_chunks(bank,doc,idx,hash,text,path) VALUES(?,?,?,?,?,?)",
				bankID, d.ID, c.Index, c.Hash, c.Text, note.Path); err != nil {
				return err
			}
		}
		return nil
	})
}

func withTx(conn *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func deleteUnitsByPath(tx *sql.Tx, rel string) error {
	for _, q := range []string{
		"DELETE FROM bank_units_fts WHERE rowid IN (SELECT rid FROM bank_units WHERE path=?)",
		"DELETE FROM bank_unit_entities WHERE unit IN (SELECT rid FROM bank_units WHERE path=?)",
		"DELETE FROM bank_links WHERE path=?",
		"DELETE FROM bank_units WHERE path=?",
	} {
		if _, err := tx.Exec(q, rel); err != nil {
			return err
		}
	}
	return nil
}

func vecKey(sig, text string) string {
	sum := sha256.Sum256([]byte(text))
	return sig + ":" + hex.EncodeToString(sum[:16])
}

// embedTexts embeds through the bank embedding cache: a text this embedder
// has seen before is read back rather than embedded again, which is what
// makes a rebuild of a large bank cost a table scan instead of a model run.
func (e *Engine) embedTexts(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	if len(texts) == 0 || e.Index.Emb == nil {
		return out, nil
	}
	sig := e.Index.Emb.Signature()
	keys := make([]string, len(texts))
	for i, t := range texts {
		keys[i] = vecKey(sig, t)
	}
	found := map[string][]float32{}
	for start := 0; start < len(keys); start += 400 {
		batch := keys[start:min(start+400, len(keys))]
		args := make([]any, len(batch))
		for i, k := range batch {
			args[i] = k
		}
		rows, err := e.Index.DB.Query("SELECT key, embedding FROM bank_vec_cache WHERE key IN (?"+
			strings.Repeat(",?", len(batch)-1)+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			var blob []byte
			if err := rows.Scan(&k, &blob); err != nil {
				rows.Close()
				return nil, err
			}
			found[k] = index.Unpack(blob)
		}
		rows.Close()
	}
	var missIdx []int
	var missText []string
	for i, k := range keys {
		if v, ok := found[k]; ok {
			out[i] = v
		} else {
			missIdx = append(missIdx, i)
			missText = append(missText, texts[i])
		}
	}
	if len(missText) == 0 {
		return out, nil
	}
	var fresh [][]float32
	for start := 0; start < len(missText); start += 256 {
		fresh = append(fresh, e.Index.Emb.Embed(missText[start:min(start+256, len(missText))])...)
	}
	if len(fresh) != len(missText) {
		return nil, fmt.Errorf("embedder returned %d vectors for %d texts", len(fresh), len(missText))
	}
	db := e.Index.DB
	db.Lock()
	defer db.Unlock()
	err := withTx(db.Conn(), func(tx *sql.Tx) error {
		for j, i := range missIdx {
			out[i] = fresh[j]
			if len(fresh[j]) == 0 {
				continue
			}
			if _, err := tx.Exec("INSERT OR REPLACE INTO bank_vec_cache(key, embedding) VALUES(?,?)",
				keys[i], index.Pack(fresh[j])); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// docInfo is what a facts file inherits from its document.
type docInfo struct {
	Context  string
	Metadata map[string]string
}

func (e *Engine) docInfo(bankID, doc string) docInfo {
	n, err := e.Vault.Read(DocumentPath(bankID, doc))
	if err != nil {
		return docInfo{}
	}
	d := ParseDocument(n.Frontmatter, n.Body, doc)
	return docInfo{Context: d.Context, Metadata: d.Metadata}
}

func dateSignals(f Fact) string {
	var parts []string
	if !f.OccStart.IsZero() {
		parts = append(parts, f.OccStart.Format("January 2 2006"))
		if !f.OccEnd.IsZero() && !f.OccEnd.Equal(f.OccStart) {
			parts = append(parts, f.OccEnd.Format("January 2 2006"))
		}
	}
	return strings.Join(parts, " ")
}

func (e *Engine) indexFacts(bankID string, note *vault.Note) error {
	doc := strings.TrimSpace(note.Frontmatter.StringVal("document_id"))
	if doc == "" {
		doc = stemOf(note.Path)
	}
	ff := ParseFacts(note.Body, bankID, doc)
	info := e.docInfo(bankID, doc)
	texts := make([]string, len(ff.Facts))
	for i, f := range ff.Facts {
		texts[i] = f.EmbedText()
	}
	vecs, err := e.embedTexts(texts)
	if err != nil {
		return err
	}
	meta := ""
	if len(info.Metadata) > 0 {
		raw, _ := json.Marshal(info.Metadata)
		meta = string(raw)
	}
	bk := bkToken(bankID)
	db := e.Index.DB
	db.Lock()
	defer db.Unlock()
	return withTx(db.Conn(), func(tx *sql.Tx) error {
		if err := deleteUnitsByPath(tx, note.Path); err != nil {
			return err
		}
		for i, f := range ff.Facts {
			human := 0
			if f.IsHuman() {
				human = 1
			}
			removed := 0
			if f.DocRemoved {
				removed = 1
			}
			var blob []byte
			if len(vecs[i]) > 0 {
				blob = index.Pack(vecs[i])
			}
			res, err := tx.Exec("INSERT OR IGNORE INTO bank_units(bank,id,doc,chunk,type,kind,text,context,"+
				"occ_start,occ_end,mentioned,tags,entities,causes,proof,human,doc_removed,challenges,metadata,path,line,embedding)"+
				" VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
				bankID, f.ID, doc, f.Chunk, f.Type, f.Kind, f.Text, info.Context,
				ms(f.OccStart), ms(f.OccEnd), ms(f.Mentioned), joinList(f.Tags), joinList(f.Entities),
				joinList(f.Causes), f.Proof, human, removed, f.Challenges, meta, note.Path, f.Line, blob)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				log.Printf("bank %s: fact id %s in %s already indexed from another file; skipped", bankID, f.ID, note.Path)
				continue
			}
			rid, err := res.LastInsertId()
			if err != nil {
				return err
			}
			body := strings.Join([]string{f.Text, strings.Join(f.Entities, " "), info.Context, dateSignals(f)}, " ")
			if _, err := tx.Exec("INSERT INTO bank_units_fts(rowid, bk, body) VALUES(?,?,?)", rid, bk, body); err != nil {
				return err
			}
			for _, name := range f.Entities {
				eid := EntityID(bankID, name)
				if _, err := tx.Exec("INSERT OR IGNORE INTO bank_entities(bank,id,canonical) VALUES(?,?,?)",
					bankID, eid, name); err != nil {
					return err
				}
				if _, err := tx.Exec("INSERT INTO bank_unit_entities(bank,unit,entity) VALUES(?,?,?)",
					bankID, rid, eid); err != nil {
					return err
				}
			}
			for _, c := range f.Causes {
				if _, err := tx.Exec("INSERT INTO bank_links(bank,src,dst,kind,weight,path) VALUES(?,?,?,?,?,?)",
					bankID, f.ID, c, "caused_by", 1.0, note.Path); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// indexObservations caches the current observations as recall units of type
// "observation". History entries are not recalled: they are what the bank no
// longer believes.
func (e *Engine) indexObservations(bankID string, note *vault.Note) error {
	of := ParseObservations(note.Body, bankID)
	texts := make([]string, len(of.Current))
	for i, o := range of.Current {
		texts[i] = Fact{Text: o.Text, OccStart: o.OccStart, OccEnd: o.OccEnd, Mentioned: o.Mentioned}.EmbedText()
	}
	vecs, err := e.embedTexts(texts)
	if err != nil {
		return err
	}
	bk := bkToken(bankID)
	db := e.Index.DB
	db.Lock()
	defer db.Unlock()
	return withTx(db.Conn(), func(tx *sql.Tx) error {
		if err := deleteUnitsByPath(tx, note.Path); err != nil {
			return err
		}
		for i, o := range of.Current {
			human := 0
			if o.IsHuman() {
				human = 1
			}
			var blob []byte
			if len(vecs[i]) > 0 {
				blob = index.Pack(vecs[i])
			}
			res, err := tx.Exec("INSERT OR IGNORE INTO bank_units(bank,id,doc,chunk,type,kind,text,context,"+
				"occ_start,occ_end,mentioned,tags,entities,causes,proof,human,doc_removed,challenges,metadata,path,line,embedding,sources)"+
				" VALUES(?,?,'',-1,'observation','',?,'',?,?,?,?,'','',?,?,0,?,'',?,?,?,?)",
				bankID, o.ID, o.Text, ms(o.OccStart), ms(o.OccEnd), ms(o.Mentioned), joinList(o.Tags),
				len(o.Sources), human, o.Challenges, note.Path, o.Line, blob, joinList(o.Sources))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				log.Printf("bank %s: observation id %s collides with another unit; skipped", bankID, o.ID)
				continue
			}
			rid, err := res.LastInsertId()
			if err != nil {
				return err
			}
			body := strings.Join([]string{o.Text, dateSignals(Fact{OccStart: o.OccStart, OccEnd: o.OccEnd})}, " ")
			if _, err := tx.Exec("INSERT INTO bank_units_fts(rowid, bk, body) VALUES(?,?,?)", rid, bk, body); err != nil {
				return err
			}
		}
		return nil
	})
}

// ------------------------------------------------------------ the cache

// unit is one fact as recall sees it.
type unit struct {
	rid        int64
	ID, Doc    string
	Chunk      int
	Type, Kind string
	Text       string
	Context    string
	OccStart   int64
	OccEnd     int64
	Mentioned  int64
	Tags       []string
	Entities   []string
	ents       []int32
	Causes     []int32 // positions of the facts this was caused by
	Proof      int
	Human      bool
	DocRemoved bool
	Challenges string
	Metadata   string
	Path       string
	// Sources are an observation's evidence facts.
	Sources []string
}

func (u *unit) eventMS() int64 {
	if u.OccStart != 0 {
		return u.OccStart
	}
	return u.Mentioned
}

type entityInfo struct {
	ID, Name string
	// Aliases are the other spellings folded into this entity (a lone first
	// name standing for the bank's one full name that starts with it).
	Aliases []string
	units   []int32
	first   int64
	last    int64
}

// bankCache is one bank held in memory for recall: every fact, its unit
// vector in one contiguous slice, and the entity and time indexes the graph
// and temporal arms walk. Built once per bank revision; any write to the bank
// bumps the revision and the next reader rebuilds.
type bankCache struct {
	rev      int64
	units    []unit
	byID     map[string]int32
	byRID    map[int64]int32
	dim      int
	vecs     []float32 // len(units) * dim, unit-normalised; a zero row has no vector
	entities []entityInfo
	entByLow map[string]int32
	// byTime holds, per fact type, the positions of facts with a time,
	// sorted by it.
	byTime  map[string][]int32
	timePos []int32
	humans  []int32
	// bodyLen is each fact's keyword-searchable length in tokens.
	bodyLen []int32

	semMu   sync.Mutex
	semMemo map[int32][]neighbor

	postMu   sync.Mutex
	postings map[string][]int32
}

type neighbor struct {
	pos    int32
	weight float64
}

func (e *Engine) cache(bankID string) (*bankCache, error) {
	e.mu.Lock()
	rev := e.rev[bankID]
	c := e.caches[bankID]
	bl, ok := e.build[bankID]
	if !ok {
		bl = &sync.Mutex{}
		e.build[bankID] = bl
	}
	e.mu.Unlock()
	if c != nil && c.rev == rev {
		return c, nil
	}
	bl.Lock()
	defer bl.Unlock()
	e.mu.Lock()
	rev = e.rev[bankID]
	c = e.caches[bankID]
	e.mu.Unlock()
	if c != nil && c.rev == rev {
		return c, nil
	}
	c, err := e.loadCache(bankID, rev)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.rev[bankID] == rev {
		e.caches[bankID] = c
	}
	e.mu.Unlock()
	return c, nil
}

func (e *Engine) loadCache(bankID string, rev int64) (*bankCache, error) {
	rows, err := e.Index.DB.Query("SELECT rid,id,doc,chunk,type,kind,text,context,occ_start,occ_end,mentioned,"+
		"tags,entities,causes,proof,human,doc_removed,challenges,metadata,path,embedding,sources"+
		" FROM bank_units WHERE bank=? ORDER BY path, line, rid", bankID)
	if err != nil {
		return nil, err
	}
	c := &bankCache{rev: rev, byID: map[string]int32{}, byRID: map[int64]int32{}, entByLow: map[string]int32{},
		byTime: map[string][]int32{}, semMemo: map[int32][]neighbor{}, postings: map[string][]int32{}}
	var blobs [][]byte
	var causes [][]string
	for rows.Next() {
		var u unit
		var tags, ents, cs, srcs string
		var human, removed int
		var blob []byte
		if err := rows.Scan(&u.rid, &u.ID, &u.Doc, &u.Chunk, &u.Type, &u.Kind, &u.Text, &u.Context,
			&u.OccStart, &u.OccEnd, &u.Mentioned, &tags, &ents, &cs, &u.Proof, &human, &removed,
			&u.Challenges, &u.Metadata, &u.Path, &blob, &srcs); err != nil {
			rows.Close()
			return nil, err
		}
		u.Tags, u.Entities, u.Sources = splitList(tags), splitList(ents), splitList(srcs)
		u.Human, u.DocRemoved = human == 1, removed == 1
		c.byID[u.ID] = int32(len(c.units))
		c.byRID[u.rid] = int32(len(c.units))
		c.units = append(c.units, u)
		blobs = append(blobs, blob)
		causes = append(causes, splitList(cs))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, b := range blobs {
		if len(b) > 0 {
			c.dim = len(b) / 4
			break
		}
	}
	// An observation is about whatever its evidence is about: it inherits the
	// entities of its source facts, which is what lets the graph arm reach it.
	for i := range c.units {
		u := &c.units[i]
		if u.Type != "observation" || len(u.Entities) > 0 {
			continue
		}
		seen := map[string]bool{}
		for _, sid := range u.Sources {
			if p, ok := c.byID[sid]; ok {
				for _, n := range c.units[p].Entities {
					if !seen[strings.ToLower(n)] {
						seen[strings.ToLower(n)] = true
						u.Entities = append(u.Entities, n)
					}
				}
			}
		}
	}
	c.vecs = make([]float32, len(c.units)*c.dim)
	for i, b := range blobs {
		if len(b) != c.dim*4 || c.dim == 0 {
			continue
		}
		row := c.vecs[i*c.dim : (i+1)*c.dim]
		copy(row, index.Unpack(b))
		normalize(row)
	}
	c.timePos = make([]int32, len(c.units))
	c.bodyLen = make([]int32, len(c.units))
	var allNames []string
	for i := range c.units {
		allNames = append(allNames, c.units[i].Entities...)
	}
	aliases := entityAliases(allNames)
	for i := range c.units {
		u := &c.units[i]
		c.timePos[i] = -1
		c.bodyLen[i] = int32(len(strings.Fields(u.Text)) + len(u.Entities) + len(strings.Fields(u.Context)))
		for _, cid := range causes[i] {
			if p, ok := c.byID[cid]; ok {
				u.Causes = append(u.Causes, p)
			}
		}
		if u.Human {
			c.humans = append(c.humans, int32(i))
		}
		for _, name := range u.Entities {
			low := strings.ToLower(name)
			spelled := ""
			if full, ok := aliases[low]; ok {
				spelled, name, low = name, full, strings.ToLower(full)
			}
			ei, ok := c.entByLow[low]
			if !ok {
				ei = int32(len(c.entities))
				c.entByLow[low] = ei
				c.entities = append(c.entities, entityInfo{ID: EntityID(bankID, name), Name: name})
			}
			en := &c.entities[ei]
			if spelled != "" {
				if _, seen := c.entByLow[strings.ToLower(spelled)]; !seen {
					c.entByLow[strings.ToLower(spelled)] = ei
					en.Aliases = append(en.Aliases, spelled)
				}
			}
			if slices.Contains(u.ents, ei) {
				continue // "Dana" and "Dana Kim" on one fact are one mention
			}
			en.units = append(en.units, int32(i))
			if t := u.eventMS(); t != 0 {
				if en.first == 0 || t < en.first {
					en.first = t
				}
				if t > en.last {
					en.last = t
				}
			}
			u.ents = append(u.ents, ei)
		}
		if u.eventMS() != 0 {
			c.byTime[u.Type] = append(c.byTime[u.Type], int32(i))
		}
	}
	for _, list := range c.byTime {
		sort.SliceStable(list, func(a, b int) bool {
			return c.units[list[a]].eventMS() < c.units[list[b]].eventMS()
		})
		for p, pos := range list {
			c.timePos[pos] = int32(p)
		}
	}
	return c, nil
}

func (c *bankCache) avgBodyLen() float64 {
	if len(c.bodyLen) == 0 {
		return 1
	}
	var sum int64
	for _, n := range c.bodyLen {
		sum += int64(n)
	}
	return math.Max(1, float64(sum)/float64(len(c.bodyLen)))
}

func normalize(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
}

func (c *bankCache) vec(i int32) []float32 {
	if c.dim == 0 {
		return nil
	}
	return c.vecs[int(i)*c.dim : (int(i)+1)*c.dim]
}

func dot(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	n := len(a)
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}

// knownEntities is the cache's entities in the shape resolution needs.
func (c *bankCache) knownEntities() []knownEntity {
	out := make([]knownEntity, len(c.entities))
	for i, en := range c.entities {
		cooc := map[string]bool{}
		for _, u := range en.units {
			for _, other := range c.units[u].ents {
				if other != int32(i) {
					cooc[strings.ToLower(c.entities[other].Name)] = true
				}
			}
		}
		out[i] = knownEntity{Name: en.Name, LastSeen: fromMS(en.last), Cooc: cooc, Mentions: len(en.units)}
	}
	// An alias stays a name of its own to resolution, so a new "Dana" is
	// written as "Dana" and the files keep what was said; the fold into the
	// full name happens when the cache is built, where it can be undone if a
	// second "Dana …" arrives.
	for _, en := range c.entities {
		for _, a := range en.Aliases {
			out = append(out, knownEntity{Name: a, LastSeen: fromMS(en.last), Mentions: len(en.units)})
		}
	}
	return out
}
