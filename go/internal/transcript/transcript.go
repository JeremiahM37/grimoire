// Package transcript reads the session transcripts of coding agents into one
// shape, so memory can learn from sessions of any agent: what was asked, which
// tools ran against what, which of them failed, how long it took.
//
// A transcript is something an agent's user wrote and an agent's tools
// printed, so it is full of secrets. Every reader here returns text that has
// been through the same redaction the vault uses for notes (secrets.RedactText)
// and bounded in length; nothing un-redacted leaves ReadFile.
//
// The formats follow what the agents write today. Claude Code, Codex, Pi,
// OpenCode and Cursor are readers; a new agent that writes JSON lines is
// described by a field map (generic-jsonl) instead of new code. The shapes of
// the Codex, Pi, OpenCode and Cursor stores were learned from the open-source
// watchmen project (MIT, github.com/mikeyobrien/watchmen's adapters); the code
// here is independent.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// Formats the package can read. internal/agentprofile validates against its own
// copy; a test keeps the two equal.
var Formats = []string{"claude-jsonl", "codex-rollout", "opencode", "pi", "cursor", "generic-jsonl"}

// Roles of a Turn.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool" // one tool call; Tool and ToolTarget say which
)

// Session is one conversation with an agent.
type Session struct {
	Agent string
	ID    string
	Cwd   string
	Start time.Time
	End   time.Time
	Model string
	Path  string // the file (or database) it came from
	Turns []Turn
}

// Turn is one message or one tool call.
type Turn struct {
	Role       string
	Text       string // what was said (empty for a tool call)
	Tool       string // tool name, for RoleTool
	ToolTarget string // the command, path or query the tool acted on
	ToolError  bool   // the tool reported failure
	At         time.Time
}

// Duration is End minus Start, zero when either is unknown.
func (s *Session) Duration() time.Duration {
	if s.Start.IsZero() || s.End.IsZero() || s.End.Before(s.Start) {
		return 0
	}
	return s.End.Sub(s.Start)
}

// Prompts counts user turns.
func (s *Session) Prompts() int { return s.count(func(t Turn) bool { return t.Role == RoleUser }) }

// ToolCalls counts tool turns.
func (s *Session) ToolCalls() int { return s.count(func(t Turn) bool { return t.Role == RoleTool }) }

// ToolErrors counts tool turns that failed.
func (s *Session) ToolErrors() int {
	return s.count(func(t Turn) bool { return t.Role == RoleTool && t.ToolError })
}

func (s *Session) count(f func(Turn) bool) int {
	n := 0
	for _, t := range s.Turns {
		if f(t) {
			n++
		}
	}
	return n
}

// Spec says what to read and how; it is the transcripts section of an agent
// profile plus the agent's name.
type Spec struct {
	Agent  string
	Format string
	Glob   string
	Map    map[string]string // generic-jsonl field map
}

// Options narrows ReadAll.
type Options struct {
	Since time.Time // skip files not modified since, and sessions that ended before
	Limit int       // at most this many sessions (newest first); 0 = all
	// MaxFiles reads only the newest N files (0 = all). Transcript folders get
	// very large; a consumer that wants recent behaviour can bound the work.
	MaxFiles int
	// NoAssistantText drops what the assistant said, keeping its turns' tool
	// calls. Mining and friction metrics do not need prose, which is most of
	// the memory a large read would take.
	NoAssistantText bool
}

const (
	maxTextRunes   = 4000
	maxTargetRunes = 600
)

// ReadFile reads every session in one file or database, redacted.
func ReadFile(spec Spec, path string) ([]Session, error) {
	var (
		out []Session
		err error
	)
	switch spec.Format {
	case "claude-jsonl":
		out, err = readClaude(path)
	case "codex-rollout":
		out, err = readCodex(path)
	case "pi":
		out, err = readPi(path)
	case "opencode":
		out, err = readOpenCode(path)
	case "cursor":
		out, err = readCursor(path)
	case "generic-jsonl":
		out, err = readGeneric(path, spec.Map)
	default:
		return nil, fmt.Errorf("unknown transcript format %q (want one of %s)", spec.Format, strings.Join(Formats, ", "))
	}
	if err != nil {
		return nil, err
	}
	kept := out[:0]
	for i := range out {
		s := &out[i]
		if len(s.Turns) == 0 {
			continue
		}
		s.Agent = spec.Agent
		s.Path = path
		finish(s)
		kept = append(kept, *s)
	}
	return kept, nil
}

// ReadAll reads every file the spec's glob matches. A file that cannot be read
// is skipped and reported in errs, not fatal: transcripts are written by other
// programs and a half-written one is normal.
func ReadAll(spec Spec, opt Options) (sessions []Session, errs []error) {
	files, err := Files(spec.Glob)
	if err != nil {
		return nil, []error{err}
	}
	type fm struct {
		path string
		mod  time.Time
	}
	var fl []fm
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || fi.IsDir() {
			continue
		}
		if !opt.Since.IsZero() && fi.ModTime().Before(opt.Since) {
			continue
		}
		fl = append(fl, fm{f, fi.ModTime()})
	}
	sort.Slice(fl, func(i, j int) bool { return fl[i].mod.After(fl[j].mod) })
	if opt.MaxFiles > 0 && len(fl) > opt.MaxFiles {
		fl = fl[:opt.MaxFiles]
	}
	type result struct {
		ss  []Session
		err error
	}
	results := make([]result, len(fl))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < min(8, max(1, runtime.NumCPU())); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				ss, err := ReadFile(spec, fl[i].path)
				if opt.NoAssistantText {
					for si := range ss {
						for ti := range ss[si].Turns {
							if ss[si].Turns[ti].Role == RoleAssistant {
								ss[si].Turns[ti].Text = ""
							}
						}
					}
				}
				results[i] = result{ss, err}
			}
		}()
	}
	for i := range fl {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", fl[i].path, r.err))
			continue
		}
		for _, s := range r.ss {
			if !opt.Since.IsZero() && !s.End.IsZero() && s.End.Before(opt.Since) {
				continue
			}
			sessions = append(sessions, s)
		}
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Start.After(sessions[j].Start) })
	if opt.Limit > 0 && len(sessions) > opt.Limit {
		sessions = sessions[:opt.Limit]
	}
	return sessions, errs
}

// finish redacts and bounds a session's text, and fills Start/End/Model.
func finish(s *Session) {
	for i := range s.Turns {
		t := &s.Turns[i]
		t.Text = clean(t.Text, maxTextRunes)
		t.ToolTarget = clean(t.ToolTarget, maxTargetRunes)
		if !t.At.IsZero() {
			if s.Start.IsZero() || t.At.Before(s.Start) {
				s.Start = t.At
			}
			if t.At.After(s.End) {
				s.End = t.At
			}
		}
	}
	s.Cwd = clean(s.Cwd, 300)
}

func clean(text string, maxRunes int) string {
	text = strings.ToValidUTF8(strings.TrimSpace(text), "")
	if text == "" {
		return ""
	}
	text, _ = secrets.RedactText(text)
	if r := []rune(text); len(r) > maxRunes {
		text = string(r[:maxRunes]) + "…"
	}
	return text
}

// ---- files ----------------------------------------------------------------

// Files expands a path pattern. `*` matches within a path segment, `**`
// across segments, `?` one character.
func Files(pattern string) ([]string, error) {
	if pattern == "" {
		return nil, errors.New("no transcripts.glob")
	}
	if !strings.Contains(pattern, "**") {
		m, err := filepath.Glob(pattern)
		sort.Strings(m)
		return m, err
	}
	i := strings.Index(pattern, "**")
	base := pattern[:strings.LastIndex(pattern[:i], "/")+1]
	if base == "" {
		base = "."
	}
	re, err := regexp.Compile("^" + globRegexp(pattern) + "$")
	if err != nil {
		return nil, err
	}
	var out []string
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && re.MatchString(p) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, nil
}

func globRegexp(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			i++
			if i+1 < len(p) && p[i+1] == '/' { // "**/" also matches no directory at all
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// ---- reading helpers --------------------------------------------------------

type obj = map[string]any

// eachLine calls fn with every JSON object line of a file. Lines that do not
// parse are skipped. Lines may be megabytes long.
func eachLine(path string, fn func(obj)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 && line[0] == '{' {
			var o obj
			if json.Unmarshal(line, &o) == nil {
				fn(o)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func asObj(v any) obj {
	o, _ := v.(map[string]any)
	return o
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(o obj, k string) string {
	s, _ := o[k].(string)
	return s
}

// dig follows a dotted path ("message.content"); "a|b" tries a then b and the
// first non-empty result wins. Numeric segments index lists.
func dig(v any, path string) any {
	for _, alt := range strings.Split(path, "|") {
		cur := v
		for _, seg := range strings.Split(strings.TrimSpace(alt), ".") {
			switch c := cur.(type) {
			case map[string]any:
				cur = c[seg]
			case []any:
				n, err := strconv.Atoi(seg)
				if err != nil || n < 0 || n >= len(c) {
					cur = nil
				} else {
					cur = c[n]
				}
			default:
				cur = nil
			}
			if cur == nil {
				break
			}
		}
		if cur != nil && cur != "" {
			return cur
		}
	}
	return nil
}

func toTime(v any) time.Time {
	switch t := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts
			}
		}
	case float64:
		return epoch(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return epoch(f)
		}
	}
	return time.Time{}
}

// epoch accepts seconds or milliseconds.
func epoch(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	if f > 1e12 {
		return time.UnixMilli(int64(f)).UTC()
	}
	return time.Unix(int64(f), 0).UTC()
}

// textOf flattens message content: a string, or blocks with a "text" field.
func textOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			switch bb := b.(type) {
			case string:
				parts = append(parts, bb)
			case map[string]any:
				switch str(bb, "type") {
				case "", "text", "input_text", "output_text":
					if t := str(bb, "text"); t != "" {
						parts = append(parts, t)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// targetKeys are tried in order to find what a tool acted on.
var targetKeys = []string{"command", "cmd", "file_path", "filePath", "path", "notebook_path", "pattern", "url", "query", "prompt", "description"}

// targetOf picks the command, path or query out of a tool's input.
func targetOf(input any) string {
	in := asObj(input)
	if in == nil {
		if s, ok := input.(string); ok {
			return s
		}
		return ""
	}
	for _, k := range targetKeys {
		switch v := in[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case []any:
			return joinCommand(v)
		}
	}
	return ""
}

// joinCommand renders an argv, dropping a leading `bash -lc` wrapper.
func joinCommand(argv []any) string {
	var parts []string
	for _, a := range argv {
		if s, ok := a.(string); ok {
			parts = append(parts, s)
		}
	}
	if len(parts) >= 3 && (parts[0] == "bash" || parts[0] == "sh" || parts[0] == "zsh") && strings.HasPrefix(parts[1], "-") && strings.Contains(parts[1], "c") {
		return parts[2]
	}
	return strings.Join(parts, " ")
}

// builder assembles a session turn by turn and pairs tool results with calls.
type builder struct {
	s    Session
	byID map[string]int
}

func newBuilder() *builder { return &builder{byID: map[string]int{}} }

func (b *builder) say(role, text string, at time.Time) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.s.Turns = append(b.s.Turns, Turn{Role: role, Text: text, At: at})
}

func (b *builder) tool(id, name, target string, at time.Time) {
	b.s.Turns = append(b.s.Turns, Turn{Role: RoleTool, Tool: name, ToolTarget: target, At: at})
	if id != "" {
		b.byID[id] = len(b.s.Turns) - 1
	}
}

func (b *builder) result(id string, failed bool) {
	if i, ok := b.byID[id]; ok && failed {
		b.s.Turns[i].ToolError = true
	}
}

func (b *builder) session() []Session { return []Session{b.s} }
