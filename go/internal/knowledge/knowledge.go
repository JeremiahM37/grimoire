// Package knowledge provides a bounded, provenance-preserving knowledge graph.
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

const MaxDepth = 3
const MaxNodes = 500
const MaxText = 4000
const maxTotalTriples = 256

type Evidence struct {
	Path   string `json:"path"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	Origin string `json:"origin"`
	Trust  string `json:"trust"`
}
type Node struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
}
type Edge struct {
	ID       string     `json:"id"`
	Source   string     `json:"source"`
	Target   string     `json:"target"`
	Relation string     `json:"relation"`
	Evidence []Evidence `json:"evidence"`
}
type Stats struct {
	Nodes     int `json:"nodes"`
	Edges     int `json:"edges"`
	Documents int `json:"documents"`
	Entities  int `json:"entities"`
	Chunks    int `json:"chunks"`
}
type Graph struct {
	Revision  int64  `json:"revision"`
	Nodes     []Node `json:"nodes"`
	Edges     []Edge `json:"edges"`
	Truncated bool   `json:"truncated"`
	Stats     Stats  `json:"stats"`
}
type Citation struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	Origin string `json:"origin"`
	Trust  string `json:"trust"`
}
type QueryResult struct {
	Answer    string     `json:"answer"`
	Citations []Citation `json:"citations"`
	Graph     Graph      `json:"graph"`
	Revision  int64      `json:"revision"`
}
type Visibility func(path, space, acl string, private, untrusted bool) bool
type GraphOptions struct {
	Seed, Relation, Q                          string
	Depth, Limit                               int
	IncludeDocuments, IncludeChunks, DropNoisy bool
	MinDegree                                  int
	After, Before                              string
}
type row struct {
	path, title, body, origin, space, acl string
	private, untrusted                    int
}
type link struct{ src, target string }

type Store struct {
	ix           *index.Index
	v            *vault.Vault
	mu           sync.Mutex
	rev          int64
	rows         []row
	links        []link
	completer    func(string) (string, error)
	completerKey string
	triples      map[string]cachedTriple
	persistMu    sync.Mutex
}

type cachedTriple struct {
	hash    string
	model   string
	triples []Triple
}
type diskTriple struct {
	Hash    string   `json:"hash"`
	Model   string   `json:"model"`
	Triples []Triple `json:"triples"`
}

func New(ix *index.Index, v *vault.Vault) *Store {
	s := &Store{ix: ix, v: v, triples: map[string]cachedTriple{}}
	if data, err := os.ReadFile(filepath.Join(v.Root, ".grimoire", "knowledge-triples.json")); err == nil {
		var disk map[string]diskTriple
		if json.Unmarshal(data, &disk) == nil {
			for path, c := range disk {
				s.triples[path] = cachedTriple{hash: c.Hash, model: c.Model, triples: c.Triples}
			}
		}
	}
	return s
}

func (s *Store) SetCompleter(complete func(string) (string, error)) {
	s.SetCompleterVersion("", complete)
}

// SetCompleterVersion invalidates semantic triples when the model/configuration
// changes, while preserving per-document hash caching across ordinary reads.
func (s *Store) SetCompleterVersion(key string, complete func(string) (string, error)) {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completerKey != "" && key != "" && s.completerKey != key {
		s.triples = map[string]cachedTriple{}
	}
	s.completerKey = key
	s.completer = complete
}
func stable(kind, value string) string {
	value = strings.TrimSpace(value)
	if kind == "entity" {
		value = strings.ToLower(value)
	}
	h := sha256.Sum256([]byte(kind + ":" + value))
	return kind + ":" + hex.EncodeToString(h[:12])
}
func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
func trustName(u int) string {
	if u != 0 {
		return "untrusted"
	}
	return "trusted"
}
func evidence(r row) Evidence {
	return Evidence{r.path, r.title, trim(r.body, MaxText), r.origin, trustName(r.untrusted)}
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rev := s.ix.Rev()
	if s.rows != nil && s.rev == rev {
		return nil
	}
	r, err := s.ix.DB.Query("SELECT path,title,body,COALESCE(origin,''),space,acl,private,untrusted FROM notes")
	if err != nil {
		return err
	}
	defer r.Close()
	var notes []row
	for r.Next() {
		var n row
		if err := r.Scan(&n.path, &n.title, &n.body, &n.origin, &n.space, &n.acl, &n.private, &n.untrusted); err != nil {
			return err
		}
		notes = append(notes, n)
	}
	if err := r.Err(); err != nil {
		return err
	}
	l, err := s.ix.DB.Query("SELECT src,dst FROM links WHERE resolved=1")
	if err != nil {
		return err
	}
	defer l.Close()
	var links []link
	for l.Next() {
		var x link
		if err := l.Scan(&x.src, &x.target); err != nil {
			return err
		}
		links = append(links, x)
	}
	if err := l.Err(); err != nil {
		return err
	}
	s.rows, s.links, s.rev = notes, links, rev
	return nil
}

func frontmatterEntities(n *vault.Note) []string {
	keys := []string{"entities", "owner", "project", "participants", "people", "author", "team", "organization", "organisations"}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		k := strings.ToLower(v)
		if v != "" && !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	for _, key := range keys {
		v, ok := n.Frontmatter.Get(key)
		if !ok {
			continue
		}
		switch x := v.(type) {
		case string:
			for _, p := range strings.Split(x, ",") {
				add(p)
			}
		case []interface{}:
			for _, p := range x {
				add(fmt.Sprint(p))
			}
		case []markdown.Value:
			for _, p := range x {
				add(fmt.Sprint(p))
			}
		default:
			add(fmt.Sprint(x))
		}
	}
	return out
}

type structuralRelation struct{ relation, value string }

func frontmatterStructuralRelations(n *vault.Note) []structuralRelation {
	relations := map[string]string{"role": "has role", "ruolo": "has role", "responsible": "has responsible", "responsabile": "has responsible", "organization": "belongs to", "organisation": "belongs to", "organizzazione": "belongs to", "project": "participates in", "progetto": "participates in", "status": "has status", "stato": "has status", "license": "has license", "licenza": "has license", "location": "located at", "lieu": "located at", "date": "has date", "data": "has date", "datum": "has date", "fecha": "has date", "budget": "has budget", "duration_months": "has duration", "durata_mesi": "has duration"}
	var out []structuralRelation
	for key, relation := range relations {
		if value, ok := n.Frontmatter.Get(key); ok {
			for _, item := range frontmatterValues(value) {
				out = append(out, structuralRelation{relation, item})
			}
		}
	}
	for _, key := range []string{"tags", "participants", "partecipanti", "teilnehmer", "participantes"} {
		if value, ok := n.Frontmatter.Get(key); ok {
			relation := "has tag"
			if key != "tags" {
				relation = "has participant"
			}
			for _, item := range frontmatterValues(value) {
				out = append(out, structuralRelation{relation, item})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].relation != out[j].relation {
			return out[i].relation < out[j].relation
		}
		return out[i].value < out[j].value
	})
	return out
}

func frontmatterValues(value markdown.Value) []string {
	switch item := value.(type) {
	case string:
		var out []string
		for _, part := range strings.Split(item, ",") {
			if strings.TrimSpace(part) != "" {
				out = append(out, strings.TrimSpace(part))
			}
		}
		return out
	case []markdown.Value:
		var out []string
		for _, part := range item {
			out = append(out, fmt.Sprint(part))
		}
		return out
	case []interface{}:
		var out []string
		for _, part := range item {
			out = append(out, fmt.Sprint(part))
		}
		return out
	default:
		return []string{fmt.Sprint(item)}
	}
}

func (s *Store) Snapshot(visible Visibility, o GraphOptions) (Graph, error) {
	if o.Depth < 0 {
		o.Depth = 0
	}
	if o.Depth > MaxDepth {
		o.Depth = MaxDepth
	}
	if o.Limit <= 0 {
		o.Limit = 200
	}
	if o.Limit > MaxNodes {
		o.Limit = MaxNodes
	}
	if err := s.load(); err != nil {
		return Graph{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	accessible := map[string]row{}
	for _, n := range s.rows {
		allowed := visible == nil || visible(n.path, n.space, n.acl, n.private != 0, n.untrusted != 0)
		if allowed && (o.After != "" || o.Before != "") {
			note, err := s.v.Read(n.path)
			date := ""
			if err == nil {
				date = documentDate(note)
			}
			if o.After != "" && (date == "" || date < o.After) {
				allowed = false
			}
			if o.Before != "" && (date == "" || date > o.Before) {
				allowed = false
			}
		}
		if allowed {
			accessible[n.path] = n
		}
	}
	accessiblePaths := make([]string, 0, len(accessible))
	for path := range accessible {
		accessiblePaths = append(accessiblePaths, path)
	}
	sort.Strings(accessiblePaths)
	nodes := map[string]Node{}
	edges := map[string]Edge{}
	addDoc := func(n row) {
		if o.IncludeDocuments {
			id := stable("document", n.path)
			nodes[id] = Node{id, n.title, "document", n.path}
		}
	}
	for _, path := range accessiblePaths {
		addDoc(accessible[path])
	}
	addEdge := func(a, b, rel string, ev Evidence) {
		if o.Relation != "" && !strings.EqualFold(o.Relation, rel) {
			return
		}
		id := stable("edge", a+"|"+b+"|"+rel)
		e := edges[id]
		e.ID, e.Source, e.Target, e.Relation = id, a, b, rel
		if len(e.Evidence) < 3 {
			e.Evidence = append(e.Evidence, ev)
		}
		edges[id] = e
	}
	links := append([]link(nil), s.links...)
	sort.Slice(links, func(i, j int) bool {
		if links[i].src != links[j].src {
			return links[i].src < links[j].src
		}
		return links[i].target < links[j].target
	})
	for _, l := range links {
		a, oka := accessible[l.src]
		b, okb := accessible[l.target]
		if oka && okb {
			addDoc(a)
			addEdge(stable("document", l.src), stable("document", l.target), "wikilink", evidence(a))
			addEdge(stable("document", l.src), stable("document", l.target), "wikilink", evidence(b))
		}
	}
	for _, path := range accessiblePaths {
		n := accessible[path]
		note, err := s.v.Read(n.path)
		if err != nil || note.Encrypted {
			continue
		}
		for _, ent := range frontmatterEntities(note) {
			id := stable("entity", ent)
			nodes[id] = Node{id, ent, "entity", ""}
			if o.IncludeDocuments {
				addEdge(stable("document", n.path), id, "mentions", evidence(n))
			}
		}
		for _, structural := range frontmatterStructuralRelations(note) {
			entityID := stable("entity", structural.value)
			nodes[entityID] = Node{entityID, structural.value, "entity", ""}
			if o.IncludeDocuments {
				addEdge(stable("document", n.path), entityID, structural.relation, evidence(n))
				addEdge(stable("document", n.path), entityID, "mentions", evidence(n))
			}
		}
		for _, t := range s.triplesForLocked(n.path, note) {
			a, b := stable("entity", t.Subject), stable("entity", t.Object)
			nodes[a] = Node{a, t.Subject, "entity", ""}
			nodes[b] = Node{b, t.Object, "entity", ""}
			if o.IncludeDocuments {
				addEdge(stable("document", n.path), a, "mentions", Evidence{n.path, n.title, trim(t.Quote, MaxText), n.origin, trustName(n.untrusted)})
				addEdge(stable("document", n.path), b, "mentions", Evidence{n.path, n.title, trim(t.Quote, MaxText), n.origin, trustName(n.untrusted)})
			}
			addEdge(a, b, t.Relation, Evidence{n.path, n.title, trim(t.Quote, MaxText), n.origin, trustName(n.untrusted)})
		}
	}
	if o.IncludeChunks {
		for _, path := range accessiblePaths {
			n := accessible[path]
			rs, err := s.ix.DB.Query("SELECT chunk_idx,chunk FROM vectors WHERE note=? ORDER BY chunk_idx", n.path)
			if err != nil {
				continue
			}
			for rs.Next() {
				var i int
				var text string
				if rs.Scan(&i, &text) == nil {
					id := stable("chunk", fmt.Sprintf("%s:%d", n.path, i))
					nodes[id] = Node{id, trim(text, 180), "chunk", n.path}
					if o.IncludeDocuments {
						addEdge(stable("document", n.path), id, "contains", Evidence{n.path, n.title, trim(text, MaxText), n.origin, trustName(n.untrusted)})
					}
				}
			}
			rs.Close()
		}
	}
	if o.Q != "" {
		q := strings.ToLower(strings.TrimSpace(o.Q))
		for id, n := range nodes {
			if !strings.Contains(strings.ToLower(n.Label), q) && !strings.Contains(strings.ToLower(n.Path), q) {
				delete(nodes, id)
			}
		}
	}
	filteredEdges := make(map[string]Edge, len(edges))
	for id, edge := range edges {
		if _, ok := nodes[edge.Source]; !ok {
			continue
		}
		if _, ok := nodes[edge.Target]; !ok {
			continue
		}
		filteredEdges[id] = edge
	}
	edges = filteredEdges
	keep := map[string]bool{}
	seed := strings.TrimSpace(strings.ToLower(o.Seed))
	if seed == "" {
		for id := range nodes {
			keep[id] = true
		}
	} else {
		for id, n := range nodes {
			if strings.Contains(strings.ToLower(n.Label), seed) || strings.EqualFold(id, seed) || strings.EqualFold(n.Path, o.Seed) {
				keep[id] = true
			}
		}
		front := []string{}
		for id := range keep {
			front = append(front, id)
		}
		sort.Strings(front)
		for hop := 0; hop < o.Depth && len(front) > 0; hop++ {
			nextSet := map[string]bool{}
			for _, id := range front {
				for _, e := range edges {
					if e.Source == id {
						nextSet[e.Target] = true
					}
					if e.Target == id {
						nextSet[e.Source] = true
					}
				}
			}
			front = front[:0]
			for id := range nextSet {
				if _, exists := nodes[id]; exists && !keep[id] {
					keep[id] = true
					front = append(front, id)
				}
			}
			sort.Strings(front)
		}
	}
	list := make([]Node, 0, len(keep))
	for id := range keep {
		list = append(list, nodes[id])
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Kind != list[j].Kind {
			return list[i].Kind < list[j].Kind
		}
		if list[i].Label != list[j].Label {
			return list[i].Label < list[j].Label
		}
		return list[i].ID < list[j].ID
	})
	trunc := len(list) > o.Limit
	if trunc {
		list = list[:o.Limit]
	}
	allowed := map[string]bool{}
	for _, n := range list {
		allowed[n.ID] = true
	}
	out := []Edge{}
	for _, e := range edges {
		if allowed[e.Source] && allowed[e.Target] {
			out = append(out, e)
		}
	}
	degree := map[string]int{}
	for _, e := range out {
		degree[e.Source]++
		degree[e.Target]++
	}
	if o.MinDegree > 0 || o.DropNoisy {
		filtered := list[:0]
		for _, n := range list {
			noisy := n.Kind == "entity" && (len([]rune(n.Label)) < 2 || strings.EqualFold(n.Label, "note") || strings.EqualFold(n.Label, "thing"))
			if (o.MinDegree > 0 && degree[n.ID] < o.MinDegree) || (o.DropNoisy && noisy) {
				delete(allowed, n.ID)
				trunc = true
				continue
			}
			filtered = append(filtered, n)
		}
		list = filtered
		out = out[:0]
		for _, e := range edges {
			if allowed[e.Source] && allowed[e.Target] {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	stats := Stats{Nodes: len(list), Edges: len(out)}
	for _, n := range list {
		if n.Kind == "document" {
			stats.Documents++
		} else if n.Path != "" {
			stats.Chunks++
		} else {
			stats.Entities++
		}
	}
	return Graph{s.rev, list, out, trunc || len(nodes) > len(list), stats}, nil
}

func (s *Store) triplesForLocked(path string, n *vault.Note) []Triple {
	if c, ok := s.triples[path]; ok && c.hash == n.Hash && (s.completer == nil || c.model == s.completerKey) {
		return c.triples
	}
	return nil
}

type ExtractResult struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Triples int    `json:"triples"`
	Error   string `json:"error,omitempty"`
}

// Extract explicitly populates the durable semantic cache. Graph reads never
// call the model; callers must opt into this bounded operation.
func (s *Store) Extract(paths []string, force bool, visible Visibility) []ExtractResult {
	if len(paths) > 10 {
		paths = paths[:10]
	}
	s.mu.Lock()
	complete, model := s.completer, s.completerKey
	s.mu.Unlock()
	out := make([]ExtractResult, 0, len(paths))
	for _, path := range paths {
		res := ExtractResult{Path: path}
		if complete == nil {
			res.Status = "error"
			res.Error = "model unavailable"
			out = append(out, res)
			continue
		}
		n, err := s.v.Read(path)
		if err != nil {
			res.Status = "error"
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		var space, acl string
		if err = s.ix.DB.QueryRow("SELECT space,acl FROM notes WHERE path=?", n.Path).Scan(&space, &acl); err != nil || (visible != nil && !visible(n.Path, space, acl, n.Private, n.Untrusted())) {
			res.Status = "error"
			res.Error = "source is not accessible"
			out = append(out, res)
			continue
		}
		s.mu.Lock()
		cached, ok := s.triples[n.Path]
		same := ok && cached.hash == n.Hash && cached.model == model
		s.mu.Unlock()
		if same && !force {
			res.Status = "cached"
			res.Triples = len(cached.triples)
			out = append(out, res)
			continue
		}
		triples, err := extractDocument(n.Raw, complete)
		if err != nil {
			res.Status = "error"
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		if err = s.persistTriple(n.Path, cachedTriple{hash: n.Hash, model: model, triples: triples}); err != nil {
			res.Status = "error"
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		res.Status = "indexed"
		res.Triples = len(triples)
		out = append(out, res)
	}
	return out
}

func extractDocument(text string, complete func(string) (string, error)) ([]Triple, error) {
	const (
		maxDocumentBytes = 1 << 20
		chunkSize        = 8 * 1024
		overlap          = 512
	)
	if len(text) > maxDocumentBytes {
		return nil, fmt.Errorf("document exceeds extraction job limit (%d bytes)", maxDocumentBytes)
	}
	if len(text) <= chunkSize {
		return ExtractTriples(text, complete)
	}
	seen := map[string]bool{}
	var result []Triple
	for start := 0; start < len(text); {
		end := start + chunkSize
		if end > len(text) {
			end = len(text)
		}
		if end < len(text) {
			if boundary := strings.LastIndexByte(text[start:end], '\n'); boundary >= chunkSize/2 {
				end = start + boundary + 1
			}
			for end > start && !utf8.ValidString(text[start:end]) {
				end--
			}
		}
		chunk := text[start:end]
		triples, err := ExtractTriples(chunk, complete)
		if err != nil {
			return nil, err
		}
		for _, triple := range triples {
			key := strings.ToLower(triple.Subject + "\x00" + triple.Relation + "\x00" + triple.Object + "\x00" + triple.Quote)
			if !seen[key] {
				seen[key] = true
				result = append(result, triple)
			}
		}
		if len(result) > maxTotalTriples {
			return nil, fmt.Errorf("semantic extraction exceeded total triple limit (%d)", maxTotalTriples)
		}
		if end == len(text) {
			break
		}
		next := end - overlap
		if next <= start {
			next = end
		}
		start = next
	}
	return result, nil
}

func (s *Store) persistTriple(path string, value cachedTriple) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	next := map[string]cachedTriple{}
	for key, item := range s.triples {
		next[key] = item
	}
	next[path] = value
	disk := map[string]diskTriple{}
	for key, item := range next {
		disk[key] = diskTriple{Hash: item.hash, Model: item.model, Triples: item.triples}
	}
	data, err := json.Marshal(disk)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	dir := filepath.Join(s.v.Root, ".grimoire")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "knowledge-triples-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpPath, filepath.Join(dir, "knowledge-triples.json")); err != nil {
		return err
	}
	s.mu.Lock()
	s.triples = next
	s.mu.Unlock()
	return nil
}

type SourceDocument struct {
	Path      string `json:"path"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	Origin    string `json:"origin"`
	Trust     string `json:"trust"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
}

const maxSourceBytes = 1 << 20

func (s *Store) Source(path string, visible Visibility) (SourceDocument, error) {
	if _, err := s.v.SafePath(path); err != nil {
		return SourceDocument{}, err
	}
	n, err := s.v.Read(path)
	if err != nil {
		return SourceDocument{}, err
	}
	var space, acl string
	if err := s.ix.DB.QueryRow("SELECT space,acl FROM notes WHERE path=?", n.Path).Scan(&space, &acl); err != nil {
		return SourceDocument{}, err
	}
	if visible != nil && !visible(n.Path, space, acl, n.Private, n.Untrusted()) {
		return SourceDocument{}, fmt.Errorf("source is not accessible")
	}
	return SourceDocument{Path: n.Path, Title: n.Title, Text: trim(n.Raw, maxSourceBytes), Origin: n.Origin, Trust: trustName(boolInt(n.Untrusted())), Total: len(n.Raw), Truncated: len(n.Raw) > maxSourceBytes}, nil
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func documentDate(n *vault.Note) string {
	for _, k := range []string{"event_date", "date", "start", "occurred", "created", "updated"} {
		if x := n.Frontmatter.StringVal(k); x != "" {
			if len(x) > 10 {
				return x[:10]
			}
			return x
		}
	}
	return ""
}
func (s *Store) queryCompleter() func(string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completer
}
func (s *Store) Query(question string, k int, f index.Filter, visible Visibility, after, before string, depth int, expand bool, client *ai.Client) (QueryResult, error) {
	if k <= 0 {
		k = 6
	}
	if k > 50 {
		k = 50
	}
	if depth > MaxDepth {
		depth = MaxDepth
	}
	for attempt := 0; attempt < 2; attempt++ {
		rev := s.ix.Rev()
		queries := []string{question}
		if expand {
			qs, err := ExpandQuery(question, s.queryCompleter())
			if err != nil {
				return QueryResult{}, err
			}
			if len(qs) > 0 {
				queries = qs
			}
		}
		var hits []index.Hit
		ranks := map[string]float64{}
		for _, q := range queries {
			hs, err := s.ix.RetrieveFor(q, k*8, f)
			if err != nil {
				return QueryResult{}, err
			}
			for rank, h := range hs {
				key := h.Path + ":" + fmt.Sprint(h.ChunkIdx)
				ranks[key] += 1.0 / float64(rank+1)
				hits = append(hits, h)
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			a := hits[i].Path + ":" + fmt.Sprint(hits[i].ChunkIdx)
			b := hits[j].Path + ":" + fmt.Sprint(hits[j].ChunkIdx)
			if ranks[a] != ranks[b] {
				return ranks[a] > ranks[b]
			}
			return a < b
		})
		seen := map[string]bool{}
		filtered := hits[:0]
		for _, h := range hits {
			key := h.Path + ":" + fmt.Sprint(h.ChunkIdx)
			if seen[key] {
				continue
			}
			seen[key] = true
			n, err := s.v.Read(h.Path)
			if err != nil {
				continue
			}
			d := documentDate(n)
			if after != "" && (d == "" || d < after) {
				continue
			}
			if before != "" && (d == "" || d > before) {
				continue
			}
			filtered = append(filtered, h)
			if len(filtered) >= k {
				break
			}
		}
		if len(filtered) == 0 {
			return QueryResult{Answer: ai.ExtractiveAnswer(question, nil), Citations: []Citation{}, Graph: Graph{Revision: rev, Nodes: []Node{}, Edges: []Edge{}, Stats: Stats{}}, Revision: rev}, nil
		}
		seed := ""
		if len(filtered) > 0 {
			seed = stable("document", filtered[0].Path)
		}
		g, err := s.Snapshot(visible, GraphOptions{Seed: seed, Depth: depth, Limit: 200, IncludeDocuments: true, DropNoisy: true, After: after, Before: before})
		if err != nil {
			return QueryResult{}, err
		}
		// Edge evidence is the graph retrieval leg. It is merged only after its
		// source has passed the same date and access checks as seed passages.
		merged := append([]index.Hit(nil), filtered...)
		seenPaths := map[string]bool{}
		for _, h := range filtered {
			seenPaths[h.Path] = true
		}
		for _, e := range g.Edges {
			for _, ev := range e.Evidence {
				if len(merged) >= k+20 {
					break
				}
				if seenPaths[ev.Path] {
					continue
				}
				n, er := s.v.Read(ev.Path)
				if er != nil {
					continue
				}
				d := documentDate(n)
				if after != "" && (d == "" || d < after) {
					continue
				}
				if before != "" && (d == "" || d > before) {
					continue
				}
				if visible != nil {
					var sp, ac string
					if s.ix.DB.QueryRow("SELECT space,acl FROM notes WHERE path=?", ev.Path).Scan(&sp, &ac) != nil || !visible(ev.Path, sp, ac, n.Private, n.Untrusted()) {
						continue
					}
				}
				merged = append(merged, index.Hit{Path: ev.Path, Title: ev.Title, Chunk: ev.Text, Origin: ev.Origin, Trust: ev.Trust})
				seenPaths[ev.Path] = true
			}
		}
		ctx := make([]ai.Context, 0, len(merged))
		cit := make([]Citation, 0, len(merged))
		for _, h := range merged {
			ctx = append(ctx, ai.Context{Path: h.Path, Title: h.Title, Chunk: h.Chunk, Origin: h.Origin, Untrusted: h.Untrusted()})
			cit = append(cit, Citation{stable("citation", h.Path+":"+fmt.Sprint(h.ChunkIdx)), h.Path, h.Title, trim(h.Chunk, MaxText), h.Origin, h.Trust})
		}
		answer := ai.ExtractiveAnswer(question, ctx)
		if client != nil {
			answer = client.Answer(question, ctx)
		}
		if rev == s.ix.Rev() && rev == g.Revision {
			return QueryResult{answer, cit, g, rev}, nil
		}
	}
	return QueryResult{}, fmt.Errorf("knowledge changed during query")
}
