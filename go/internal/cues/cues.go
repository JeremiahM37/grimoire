// Package cues indexes memories by the situations in which they apply.
//
// A memory is stored as what is true ("deploys go through release.conf"); an
// agent arrives with what it is doing ("bump the grimoire build"). Those share
// few words and often little meaning, so retrieval keyed on the memory's text
// misses it. A cue is a short description of a situation that should bring a
// memory to mind: a request a user might type, a command or path the agent may
// be about to run or touch, or a set of keywords. Matching the agent's moment
// against cues rather than against the memory itself closes that gap.
//
// Cues come from three sources, recorded on each one:
//
//	generated  written from the memory's text by a model, ahead of need
//	agent      supplied by the agent that wrote the memory, which knows the
//	           situation it was in
//	learned    the situation an agent was in when it was told something the
//	           store already held — the moment the memory should have fired
//
// Cues are derived data about memories, not memories: they are kept in the
// data directory, never in the notes, and a missing file means no cues.
package cues

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kinds of cue.
const (
	Request  = "request"
	Action   = "action"
	Keywords = "keywords"
)

// Sources of cue.
const (
	Generated = "generated"
	Agent     = "agent"
	Learned   = "learned"
)

// MaxText bounds one cue.
const MaxText = 300

// Cue is one situation attached to one memory. Target names the memory:
// "fact:<id>" for a stored fact, "note:<path>" for a whole note.
type Cue struct {
	Target string    `json:"target"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
	Source string    `json:"source"`
	Added  time.Time `json:"added"`
}

// FactTarget and NoteTarget build cue targets.
func FactTarget(id string) string   { return "fact:" + id }
func NoteTarget(path string) string { return "note:" + path }

// Embedder is the subset of the index's embedder this package needs.
type Embedder interface {
	Embed(texts []string) [][]float32
	Signature() string
}

// Store holds every cue and its vector.
type Store struct {
	mu   sync.RWMutex
	path string
	all  []Cue
	vecs [][]float32
	seen map[string]bool
	emb  Embedder
	vc   map[string][]float32 // vector cache by text digest, persisted beside the cues
}

// Open loads cues from dir/cues.jsonl and embeds any not already cached.
// A missing file is an empty store.
func Open(dir string, emb Embedder) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "cues.jsonl"), seen: map[string]bool{}, emb: emb, vc: map[string][]float32{}}
	s.loadVectors()
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var batch []Cue
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var c Cue
		if json.Unmarshal(sc.Bytes(), &c) != nil || c.Target == "" || strings.TrimSpace(c.Text) == "" {
			continue
		}
		if k := key(c); !s.seen[k] {
			s.seen[k] = true
			batch = append(batch, c)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	s.index(batch)
	s.saveVectors()
	return s, nil
}

func key(c Cue) string { return c.Target + "\x00" + strings.ToLower(strings.TrimSpace(c.Text)) }

func digest(sig, text string) string {
	h := sha256.Sum256([]byte(sig + "\x00" + text))
	return hex.EncodeToString(h[:12])
}

// index embeds cues (cached by text) and appends them. Caller holds no lock
// or the write lock.
func (s *Store) index(batch []Cue) {
	if len(batch) == 0 {
		return
	}
	sig := ""
	if s.emb != nil {
		sig = s.emb.Signature()
	}
	var missing []string
	for _, c := range batch {
		if _, ok := s.vc[digest(sig, c.Text)]; !ok {
			missing = append(missing, c.Text)
		}
	}
	if s.emb != nil {
		for start := 0; start < len(missing); start += 64 {
			end := min(start+64, len(missing))
			vs := s.emb.Embed(missing[start:end])
			for i, v := range vs {
				if len(v) > 0 {
					s.vc[digest(sig, missing[start+i])] = v
				}
			}
		}
	}
	for _, c := range batch {
		s.all = append(s.all, c)
		s.vecs = append(s.vecs, s.vc[digest(sig, c.Text)])
	}
}

// Add records cues, skipping exact duplicates, and appends them to the file.
// It returns how many were new.
func (s *Store) Add(cs []Cue) (int, error) {
	var fresh []Cue
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range cs {
		c.Text = strings.TrimSpace(c.Text)
		if r := []rune(c.Text); len(r) > MaxText {
			c.Text = string(r[:MaxText])
		}
		if c.Target == "" || c.Text == "" || s.seen[key(c)] {
			continue
		}
		if c.Added.IsZero() {
			c.Added = time.Now().UTC()
		}
		s.seen[key(c)] = true
		fresh = append(fresh, c)
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	for _, c := range fresh {
		raw, _ := json.Marshal(c)
		if _, err = f.Write(append(raw, '\n')); err != nil {
			break
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	s.index(fresh)
	s.saveVectors()
	return len(fresh), err
}

// For returns the cues attached to a target.
func (s *Store) For(target string) []Cue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Cue
	for _, c := range s.all {
		if c.Target == target {
			out = append(out, c)
		}
	}
	return out
}

// Len is the number of cues.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.all)
}

// Match is one target's best cue for a query.
type Match struct {
	Target string
	Cue    Cue
	Cosine float64
}

// Best returns, per target, the cue most similar to the query vector, best
// targets first, at most k. Kinds, when given, restricts which cues count.
func (s *Store) Best(qv []float32, k int, kinds ...string) []Match {
	if len(qv) == 0 {
		return nil
	}
	allow := map[string]bool{}
	for _, kd := range kinds {
		allow[kd] = true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	best := map[string]Match{}
	qn := norm(qv)
	for i, v := range s.vecs {
		if len(v) != len(qv) || (len(allow) > 0 && !allow[s.all[i].Kind]) {
			continue
		}
		cos := dot(qv, v) / (qn * norm(v))
		t := s.all[i].Target
		if m, ok := best[t]; !ok || cos > m.Cosine {
			best[t] = Match{Target: t, Cue: s.all[i], Cosine: cos}
		}
	}
	out := make([]Match, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cosine != out[j].Cosine {
			return out[i].Cosine > out[j].Cosine
		}
		return out[i].Target < out[j].Target
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func norm(a []float32) float64 {
	n := math.Sqrt(dot(a, a))
	if n == 0 {
		return 1
	}
	return n
}

// The vector cache is a flat binary file: per entry a 24-hex-char digest, a
// uint32 length, then the floats. It only saves re-embedding at start.
func (s *Store) vecPath() string { return strings.TrimSuffix(s.path, ".jsonl") + ".vec" }

func (s *Store) loadVectors() {
	raw, err := os.ReadFile(s.vecPath())
	if err != nil {
		return
	}
	for len(raw) >= 28 {
		d := string(raw[:24])
		n := int(binary.LittleEndian.Uint32(raw[24:28]))
		raw = raw[28:]
		if n < 0 || len(raw) < 4*n {
			return
		}
		v := make([]float32, n)
		for i := range v {
			v[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
		}
		raw = raw[4*n:]
		s.vc[d] = v
	}
}

func (s *Store) saveVectors() {
	var buf []byte
	for d, v := range s.vc {
		buf = append(buf, d...)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(v)))
		for _, x := range v {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(x))
		}
	}
	tmp := s.vecPath() + ".tmp"
	if os.WriteFile(tmp, buf, 0o600) == nil {
		_ = os.Rename(tmp, s.vecPath())
	}
}

// OfKind returns a copy of every cue of one kind.
func (s *Store) OfKind(kind string) []Cue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Cue
	for _, c := range s.all {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// PathTokens are the parts of a cue that name something exactly: paths,
// file names, units. Only these trigger on their own; a command verb like
// "restart" appears in too many actions to mean anything by itself.
func PathTokens(text string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '"' || r == '\'' || r == '`' || r == ',' || r == ';' || r == '(' || r == ')' || r == '='
	}) {
		f = strings.TrimRight(strings.TrimPrefix(f, "~"), ".:")
		if len(f) < 6 || !(strings.Contains(f, "/") || strings.Contains(f, ".")) {
			continue
		}
		if strings.Trim(f, "/.") == "" || strings.HasPrefix(f, "http") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Triggers reports whether an action cue names something the pending action
// touches: one of the cue's path tokens appears in the action, compared on
// the path's last two segments so ~/x/y and /home/me/x/y agree.
func Triggers(cue, action string) bool {
	return len(triggerTails(cue, action)) > 0
}

func pathTail(t string) string {
	segs := strings.Split(strings.Trim(t, "/"), "/")
	if len(segs) > 2 {
		segs = segs[len(segs)-2:]
	}
	return strings.Join(segs, "/")
}

func triggerTails(cue, action string) []string {
	action = strings.ToLower(action)
	var out []string
	for _, t := range PathTokens(strings.ToLower(cue)) {
		if tail := pathTail(t); len(tail) >= 6 && strings.Contains(action, tail) {
			out = append(out, tail)
		}
	}
	return out
}

// MaxTriggerTargets is how many memories may name a path before it stops
// being specific enough to fire on its own. "docker-compose.yml" is named by
// cues of dozens of memories; "grimoire.service.d/release.conf" by one.
const MaxTriggerTargets = 3

// Triggered returns the targets whose action cues name a specific path the
// action touches, where "specific" means at most MaxTriggerTargets memories'
// cues name that path.
func (s *Store) Triggered(action string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byTail := map[string]map[string]bool{}
	for _, c := range s.all {
		if c.Kind != Action {
			continue
		}
		for _, tail := range triggerTails(c.Text, action) {
			if byTail[tail] == nil {
				byTail[tail] = map[string]bool{}
			}
			byTail[tail][c.Target] = true
		}
	}
	if len(byTail) == 0 {
		return nil
	}
	// A tail the action matches is specific only if few memories name it
	// anywhere in their cues, not just among the ones that matched.
	df := map[string]int{}
	for _, c := range s.all {
		if c.Kind != Action {
			continue
		}
		low := strings.ToLower(c.Text)
		for tail := range byTail {
			if strings.Contains(low, tail) {
				df[tail]++
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for tail, targets := range byTail {
		if df[tail] > MaxTriggerTargets {
			continue
		}
		for t := range targets {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}
