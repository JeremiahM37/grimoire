package bank

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// EntityHint is an entity the caller already knows a content item is about.
type EntityHint struct {
	Text string `json:"text"`
	Type string `json:"type,omitempty"`
}

// Item is one piece of content to retain.
type Item struct {
	Content string
	// Timestamp is when the content was written or said. Nil means now;
	// Unset means the content has no time and its facts get none.
	Timestamp *time.Time
	Unset     bool
	Context   string
	Metadata  map[string]string
	// DocumentID names the document this content belongs to. Retaining the
	// same id again replaces it (or extends it, with UpdateMode "append"),
	// re-extracting only the chunks that changed.
	DocumentID string
	Entities   []EntityHint
	// ResolveEntities=false takes the caller's entity names literally.
	ResolveEntities *bool
	Tags            []string
	UpdateMode      string // "replace" (default) or "append"
}

// RetainOptions apply to a whole retain call.
type RetainOptions struct {
	Agent        string
	DocumentTags []string
	// Mode overrides the bank's extraction mode for this call.
	Mode string
}

// DocResult is what retain did to one document.
type DocResult struct {
	DocumentID      string   `json:"document_id"`
	Chunks          int      `json:"chunks"`
	ChunksExtracted int      `json:"chunks_extracted"`
	ChunksReused    int      `json:"chunks_reused"`
	Facts           int      `json:"facts"`
	FactsAdded      int      `json:"facts_added"`
	HumanKept       int      `json:"human_facts_kept"`
	FactIDs         []string `json:"fact_ids"`
	Unchanged       bool     `json:"unchanged,omitempty"`
}

// RetainResult is the outcome of one retain call.
type RetainResult struct {
	BankID     string       `json:"bank_id"`
	ItemsCount int          `json:"items_count"`
	Mode       string       `json:"mode"`
	Documents  []DocResult  `json:"documents"`
	Usage      usage.Tokens `json:"-"`
	BankCreate bool         `json:"bank_created,omitempty"`
}

// Errors retain and the CRUD calls report. The API maps them to statuses.
var (
	ErrNotFound       = errors.New("not found")
	ErrExists         = errors.New("already exists")
	ErrInvalid        = errors.New("invalid request")
	ErrHumanProtected = errors.New("written by a person")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func newDocID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "doc-" + hex.EncodeToString(b)
}

// Retain extracts and stores facts from items. It is synchronous: the call
// returns when the files are written and indexed.
//
// It is also the unit of work an asynchronous operation runs: an operations
// queue calls exactly this with the stored request, so nothing here may
// depend on an HTTP request still being open beyond ctx.
func (e *Engine) Retain(ctx context.Context, bankID string, items []Item, opts RetainOptions) (*RetainResult, error) {
	if !ValidID(bankID) {
		return nil, invalid("invalid bank id")
	}
	if len(items) == 0 {
		return nil, invalid("items must not be empty")
	}
	for i, it := range items {
		if strings.TrimSpace(it.Content) == "" {
			return nil, invalid("item %d has no content", i)
		}
		switch it.UpdateMode {
		case "", "replace":
		case "append":
			if strings.TrimSpace(it.DocumentID) == "" {
				return nil, invalid("item %d: update_mode append needs a document_id", i)
			}
		default:
			return nil, invalid("item %d: update_mode must be replace or append", i)
		}
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()

	res := &RetainResult{BankID: bankID, ItemsCount: len(items)}
	prof, err := e.Profile(bankID)
	if errors.Is(err, ErrNotFound) {
		// Banks are created on first use, like a folder you write a note into.
		prof = NewProfile(bankID)
		if err := e.writeProfile(prof, false); err != nil {
			return nil, err
		}
		res.BankCreate = true
	} else if err != nil {
		return nil, err
	}
	mode := opts.Mode
	if mode == "" {
		mode = prof.Setting("retain_extraction_mode", ModeConcise)
	}
	switch mode {
	case ModeConcise, ModeVerbatim, ModeChunks:
	default:
		return nil, invalid("unknown extraction mode %q", mode)
	}
	if !e.AI.Available() && mode == ModeVerbatim {
		mode = ModeChunks
	}
	res.Mode = mode
	if mode == ModeConcise && !e.AI.Available() {
		res.Mode = "rules"
	}

	// Group by document. No ids at all: one generated document for the whole
	// batch. Some ids: each id-less item becomes its own document.
	type group struct {
		id    string
		items []Item
	}
	var groups []*group
	byID := map[string]*group{}
	anyID := false
	for _, it := range items {
		if strings.TrimSpace(it.DocumentID) != "" {
			anyID = true
		}
	}
	shared := ""
	for _, it := range items {
		id := strings.TrimSpace(it.DocumentID)
		if id == "" {
			if anyID {
				id = newDocID()
			} else {
				if shared == "" {
					shared = newDocID()
				}
				id = shared
			}
		}
		g, ok := byID[id]
		if !ok {
			g = &group{id: id}
			byID[id] = g
			groups = append(groups, g)
		}
		g.items = append(g.items, it)
	}
	for _, g := range groups {
		dr, tok, err := e.retainDocument(ctx, prof, g.id, g.items, opts, mode)
		res.Usage.Input += tok.Input
		res.Usage.Output += tok.Output
		if err != nil {
			return res, err
		}
		res.Documents = append(res.Documents, *dr)
	}
	return res, nil
}

// segment is one item's span of a document body, for mapping chunks back to
// the item they came from.
type segment struct {
	item  int
	start int // byte offset in the joined body, or turn index for conversations
}

// joinItems builds a document body. Conversations — every item a JSON array
// of turns — are merged into one array, so they keep chunking on turns.
func joinItems(items []Item) (body string, segs []segment, turns bool) {
	allConv := true
	var parsed [][]json.RawMessage
	for _, it := range items {
		var t []json.RawMessage
		c := strings.TrimSpace(it.Content)
		if !strings.HasPrefix(c, "[") || json.Unmarshal([]byte(c), &t) != nil || !allObjects(t) || len(t) == 0 {
			allConv = false
			break
		}
		parsed = append(parsed, t)
	}
	if allConv && len(items) > 1 {
		var parts []string
		for i, t := range parsed {
			segs = append(segs, segment{item: i, start: len(parts)})
			for _, raw := range t {
				parts = append(parts, compact(raw))
			}
		}
		return "[" + strings.Join(parts, ",") + "]", segs, true
	}
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n\n")
		}
		segs = append(segs, segment{item: i, start: b.Len()})
		b.WriteString(strings.TrimSpace(it.Content))
	}
	return b.String(), segs, allConv
}

// chunkItems maps each chunk to the item it starts in.
func chunkItems(body string, chunks []Chunk, segs []segment, turns bool) []int {
	out := make([]int, len(chunks))
	owner := func(pos int) int {
		o := 0
		for _, s := range segs {
			if s.start <= pos {
				o = s.item
			}
		}
		return o
	}
	if turns {
		turn := 0
		for i, c := range chunks {
			out[i] = owner(turn)
			var t []json.RawMessage
			if json.Unmarshal([]byte(c.Text), &t) == nil {
				turn += len(t)
			}
		}
		return out
	}
	cursor := 0
	for i, c := range chunks {
		probe := c.Text
		if len(probe) > 64 {
			probe = probe[:64]
		}
		if j := strings.Index(body[cursor:], probe); j >= 0 {
			cursor += j
		}
		out[i] = owner(cursor)
	}
	return out
}

func (e *Engine) retainDocument(ctx context.Context, prof *Profile, docID string, items []Item,
	opts RetainOptions, mode string) (*DocResult, usage.Tokens, error) {
	var spent usage.Tokens
	bankID := prof.ID
	docRel, factsRel := DocumentPath(bankID, docID), FactsPath(bankID, docID)

	var old *Document
	if n, err := e.Vault.Read(docRel); err == nil {
		old = ParseDocument(n.Frontmatter, n.Body, docID)
	}
	var oldFacts FactsFile
	var oldFactsBody string
	if n, err := e.Vault.Read(factsRel); err == nil {
		oldFactsBody = n.Body
		oldFacts = ParseFacts(n.Body, bankID, docID)
	}

	appendMode := false
	for _, it := range items {
		if it.UpdateMode == "append" {
			appendMode = true
		}
	}
	if appendMode && old != nil && old.Content != "" {
		base := Item{Content: old.Content, Context: old.Context, Metadata: old.Metadata,
			Tags: old.Tags, Unset: old.Unset}
		if !old.Timestamp.IsZero() {
			ts := old.Timestamp
			base.Timestamp = &ts
		}
		items = append([]Item{base}, items...)
	}

	size, _ := strconv.Atoi(prof.Setting("retain_chunk_size", strconv.Itoa(DefaultChunkSize)))
	if size <= 0 {
		size = DefaultChunkSize
	}
	body, segs, turns := joinItems(items)
	chunks := Chunks(body, size)
	owners := chunkItems(body, chunks, segs, turns)

	now := e.now()
	stamp := func(it Item) time.Time {
		if it.Unset {
			return time.Time{}
		}
		if it.Timestamp != nil {
			return it.Timestamp.UTC()
		}
		return now
	}
	tagsOf := func(it Item) []string { return unionTags(it.Tags, opts.DocumentTags) }

	// Which chunks survive unchanged. A chunk is reused when the same index
	// holds the same text; anything else is extracted again.
	unchanged := map[int]bool{}
	newHashes := map[string]bool{}
	for _, c := range chunks {
		newHashes[c.Hash] = true
	}
	oldHashAt := map[int]string{}
	if old != nil {
		for i, h := range old.ChunkHashes {
			oldHashAt[i] = h
			if i < len(chunks) && chunks[i].Hash == h {
				unchanged[i] = true
			}
		}
	}

	dr := &DocResult{DocumentID: docID, Chunks: len(chunks)}

	// Facts carried over: a model's facts from unchanged chunks, and every
	// fact a person wrote or edited, whatever happened to its chunk.
	var kept []Fact
	for _, f := range oldFacts.Facts {
		if f.IsHuman() {
			if f.Chunk >= 0 && !unchanged[f.Chunk] {
				if h, ok := oldHashAt[f.Chunk]; !ok || !newHashes[h] {
					f.DocRemoved = true
				}
			}
			kept = append(kept, f)
			dr.HumanKept++
			continue
		}
		if f.Chunk >= 0 && unchanged[f.Chunk] {
			it := items[owners[f.Chunk]]
			f.Tags = tagsOf(it)
			kept = append(kept, f)
		}
	}

	var todo []int
	for i := range chunks {
		if unchanged[i] {
			dr.ChunksReused++
		} else {
			todo = append(todo, i)
		}
	}
	dr.ChunksExtracted = len(todo)

	// Extract the chunks that need it, concurrently.
	results := make([][]extracted, len(chunks))
	errs := make([]error, len(chunks))
	toks := make([]usage.Tokens, len(chunks))
	causal := prof.Setting("retain_extract_causal", "true") == "true"
	client := e.AI.WithSurface("bank.retain", opts.Agent)
	sem := make(chan struct{}, max(1, e.ExtractConcurrency))
	var wg sync.WaitGroup
	for _, i := range todo {
		it := items[owners[i]]
		in := extractInput{Chunk: chunks[i].Text, Index: i, Total: len(chunks), EventDate: stamp(it),
			Context: it.Context, Metadata: it.Metadata, Mission: prof.RetainMission, Mode: mode, Causal: causal}
		switch {
		case mode == ModeChunks:
			results[i] = []extracted{{Text: normFactText(chunks[i].Text), Type: "world", Kind: "conversation"}}
			continue
		case !e.AI.Available():
			results[i] = ruleExtract(in)
			continue
		}
		wg.Add(1)
		go func(i int, in extractInput) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], toks[i], errs[i] = e.extractChunk(ctx, client, in)
		}(i, in)
	}
	wg.Wait()
	for i := range chunks {
		spent.Input += toks[i].Input
		spent.Output += toks[i].Output
		if errs[i] != nil {
			return nil, spent, errs[i]
		}
	}

	// Entities: the model's names plus the caller's hints, resolved against
	// what the bank already knows.
	c, err := e.cache(bankID)
	if err != nil {
		return nil, spent, err
	}
	res := newResolver(c.knownEntities())
	type pending struct {
		chunk, idx int
		ex         extracted
		names      []string
		literal    []bool
	}
	var fresh []pending
	var mentions []Mention
	for _, i := range todo {
		it := items[owners[i]]
		for j, ex := range results[i] {
			names, literal := mergeHints(ex.Entities, it.Entities, it.ResolveEntities)
			p := pending{chunk: i, idx: j, ex: ex, names: names, literal: literal}
			for k, n := range names {
				mentions = append(mentions, Mention{Text: n, Nearby: names, Event: firstNonZero(ex.OccStart, stamp(it)), Literal: literal[k]})
			}
			fresh = append(fresh, p)
		}
	}
	resolved := res.Resolve(mentions)
	mi := 0
	var added []Fact
	idsByChunk := map[int][]string{}
	for _, p := range fresh {
		it := items[owners[p.chunk]]
		var ents []string
		seen := map[string]bool{}
		for range p.names {
			name := resolved[mi]
			mi++
			if name != "" && !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				ents = append(ents, name)
			}
		}
		f := Fact{
			ID:   FactID(bankID, docID, p.chunk, p.idx, p.ex.Text),
			Text: p.ex.Text, Type: p.ex.Type, Kind: p.ex.Kind, Chunk: p.chunk,
			OccStart: p.ex.OccStart, OccEnd: p.ex.OccEnd, Mentioned: stamp(it),
			Entities: ents, Tags: tagsOf(it),
		}
		idsByChunk[p.chunk] = append(idsByChunk[p.chunk], f.ID)
		added = append(added, f)
	}
	// Causal links point at facts of the same chunk, by their position.
	for k, p := range fresh {
		for _, ci := range p.ex.Causes {
			ids := idsByChunk[p.chunk]
			if ci >= 0 && ci < len(ids) && ids[ci] != added[k].ID {
				added[k].Causes = append(added[k].Causes, ids[ci])
			}
		}
	}

	// A fact the model extracts over a person's correction is recorded as a
	// challenge to it, in the file, so the disagreement survives a rebuild
	// and recall can rank the person first without guessing.
	for k := range added {
		for _, h := range kept {
			if h.IsHuman() && sameSourceRewrite(h.ID, docID, h.Chunk, h.Text, added[k].ID, docID, added[k].Chunk, added[k].Text) {
				added[k].Challenges = h.ID
				break
			}
		}
	}
	all := append(kept, added...)
	dedupIDs(all)
	// File order: by chunk, keeping each chunk's facts in extraction order;
	// facts with no chunk (typed by hand, or orphaned) last.
	sort.SliceStable(all, func(a, b int) bool {
		ca, cb := all[a].Chunk, all[b].Chunk
		if all[a].DocRemoved {
			ca = 1 << 30
		}
		if all[b].DocRemoved {
			cb = 1 << 30
		}
		if ca < 0 {
			ca = 1<<30 + 1
		}
		if cb < 0 {
			cb = 1<<30 + 1
		}
		return ca < cb
	})
	dr.Facts = len(all)
	dr.FactsAdded = len(added)
	for _, f := range all {
		dr.FactIDs = append(dr.FactIDs, f.ID)
	}

	// The document.
	// In append mode items[0] is the stored base, so the document keeps the
	// original's time and context.
	first := items[0]
	doc := &Document{ID: docID, Context: first.Context, Metadata: first.Metadata,
		Tags: unionTags(allTags(items), opts.DocumentTags), ChunkSize: size, Content: body, Unset: first.Unset}
	if !first.Unset {
		doc.Timestamp = stamp(first)
	}
	for _, ch := range chunks {
		doc.ChunkHashes = append(doc.ChunkHashes, ch.Hash)
	}
	factsBody := FormatFacts(docID, FactsFile{Facts: all, Prose: oldFacts.Prose})

	docSame := old != nil && old.Content == doc.Content && equalStrings(old.ChunkHashes, doc.ChunkHashes) &&
		old.Context == doc.Context && equalStrings(old.Tags, doc.Tags)
	factsSame := strings.TrimSpace(oldFactsBody) == strings.TrimSpace(factsBody)
	if docSame && factsSame {
		dr.Unchanged = true
		return dr, spent, nil
	}
	if !docSame {
		if e.History != nil && old != nil {
			if n, err := e.Vault.Read(docRel); err == nil {
				e.History.Snapshot(docRel, n.Body)
			}
		}
		if _, err := e.Vault.Write(docRel, doc.Content, doc.Frontmatter()); err != nil {
			return nil, spent, err
		}
		if _, err := e.Index.Upsert(docRel); err != nil {
			return nil, spent, err
		}
	}
	if !factsSame {
		if e.History != nil && oldFactsBody != "" {
			e.History.Snapshot(factsRel, oldFactsBody)
		}
		if _, err := e.Vault.Write(factsRel, factsBody, factsFrontmatter(bankID, docID)); err != nil {
			return nil, spent, err
		}
		if _, err := e.Index.Upsert(factsRel); err != nil {
			return nil, spent, err
		}
	}
	return dr, spent, nil
}

func firstNonZero(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// mergeHints adds the caller's entity hints to a fact's own names. A hint the
// caller marked literal stays literal even when the model also named it.
func mergeHints(names []string, hints []EntityHint, resolve *bool) ([]string, []bool) {
	literalHints := resolve != nil && !*resolve
	out := append([]string(nil), names...)
	lit := make([]bool, len(out))
	pos := map[string]int{}
	for i, n := range out {
		pos[strings.ToLower(n)] = i
	}
	for _, h := range hints {
		n := NormalizeEntity(h.Text)
		if n == "" {
			continue
		}
		if i, ok := pos[strings.ToLower(n)]; ok {
			lit[i] = lit[i] || literalHints
			continue
		}
		pos[strings.ToLower(n)] = len(out)
		out = append(out, n)
		lit = append(lit, literalHints)
	}
	return out, lit
}

func unionTags(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lists {
		for _, t := range l {
			t = strings.TrimSpace(t)
			if t != "" && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

func allTags(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Tags...)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dedupIDs suffixes repeated ids, which two identical facts extracted from
// the same position would otherwise share.
func dedupIDs(fs []Fact) {
	seen := map[string]int{}
	for i := range fs {
		if n := seen[fs[i].ID]; n > 0 {
			fs[i].ID = fs[i].ID + "-" + strconv.Itoa(n)
		}
		seen[fs[i].ID]++
	}
}
