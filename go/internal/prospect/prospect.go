// Package prospect holds intentions: facts that should come back at a moment,
// not when they happen to match a question.
//
// Ordinary memory answers "what is true about X". An intention is "when Y is
// about to happen, remind me of Z", such as "when anyone edits deploy/, say the
// staging database is shared" or "after 2026-11-01, check the cert was renewed".
// It is stored as an ordinary fact with a cue: a small conjunction of typed
// predicates evaluated against the situation of a context request.
//
// The cue language is
//
//	path:<glob>     a file being read or edited matches (deploy/**, deploy/*.yml)
//	cmd:<prefix>    a shell command starts with the prefix, on a word boundary
//	project:<name>  the request's project is this name
//	after:<date>    today is on or after this UTC date (YYYY-MM-DD)
//	before:<date>   today is before this UTC date
//	term:<word>     the prompt or action contains this word
//
// Terms are joined with "&", which means AND. A cue is refused at write time
// when it does not parse, and a stored cue that does not parse never fires.
//
// The predicate kinds are not the ones in skillgate. Skill preconditions test
// the machine (file:, command:, env:, fact:); an intention tests the moment
// (what is being touched, typed or run). The parsing discipline is the same:
// an unknown or malformed predicate is unmet, never silently true.
//
// Lifecycle is one of: standing (the default, empty in the trailer), once (it
// retires after a firing has been acknowledged), or until:<date> (it stops
// firing after that UTC day).
package prospect

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Predicate kinds.
const (
	KindPath    = "path"
	KindCmd     = "cmd"
	KindProject = "project"
	KindAfter   = "after"
	KindBefore  = "before"
	KindTerm    = "term"
)

// Lifecycles, as they are stored.
const (
	LifeOnce     = "once"
	LifeUntil    = "until:"
	LifeStanding = ""
)

// Bounds. MaxCue matches the bound on a stored cue's source text elsewhere
// (cues.MaxText) so one situation never costs more than the others to read.
const (
	MaxCue            = 300
	MaxTerms          = 4
	MaxArg            = 200
	MaxPaths          = 32
	MaxFiresPerReq    = 3
	dateLayout        = "2006-01-02"
	maxPathComponents = 32
)

// Term is one predicate of a cue.
type Term struct {
	Kind string
	Arg  string
}

// Cue is a conjunction of terms. A cue with no terms never parses.
type Cue struct {
	Terms []Term
}

// Situation is what a cue is evaluated against: one context request.
type Situation struct {
	Now     time.Time
	Prompt  string   // the prompt, or the tool and its target for an action
	Paths   []string // files being read or edited
	Command string   // a shell command about to run
	Project string   // the project the request belongs to
}

// ParseCue reads a cue from its text form and refuses anything it cannot
// evaluate exactly. It returns the cue, whose String is the canonical form.
func ParseCue(s string) (Cue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Cue{}, errors.New("cue is empty")
	}
	if len(s) > MaxCue {
		return Cue{}, fmt.Errorf("cue is longer than %d characters", MaxCue)
	}
	if strings.ContainsAny(s, "<>\\") || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return Cue{}, errors.New("cue contains a character that is not allowed")
	}
	parts := strings.Split(s, "&")
	if len(parts) > MaxTerms {
		return Cue{}, fmt.Errorf("a cue has at most %d terms", MaxTerms)
	}
	var c Cue
	for _, raw := range parts {
		t, err := parseTerm(strings.TrimSpace(raw))
		if err != nil {
			return Cue{}, err
		}
		c.Terms = append(c.Terms, t)
	}
	return c, nil
}

func parseTerm(s string) (Term, error) {
	kind, arg, ok := strings.Cut(s, ":")
	if !ok {
		return Term{}, fmt.Errorf("predicate %q has no colon", s)
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return Term{}, fmt.Errorf("predicate %q has no argument", s)
	}
	if len(arg) > MaxArg {
		return Term{}, fmt.Errorf("predicate %q argument is longer than %d characters", kind, MaxArg)
	}
	t := Term{Kind: kind, Arg: arg}
	switch kind {
	case KindPath:
		if err := checkPattern(arg); err != nil {
			return Term{}, err
		}
	case KindCmd:
		if strings.Contains(arg, "&") {
			return Term{}, errors.New("cmd: argument cannot contain & (it is the AND separator)")
		}
	case KindProject:
		if strings.ContainsAny(arg, " \t/") {
			return Term{}, errors.New("project: names one project, without spaces or slashes")
		}
	case KindAfter, KindBefore:
		if _, err := time.Parse(dateLayout, arg); err != nil {
			return Term{}, fmt.Errorf("%s: wants a date as YYYY-MM-DD", kind)
		}
	case KindTerm:
		if strings.ContainsFunc(arg, func(r rune) bool { return !isWordRune(r) }) {
			return Term{}, errors.New("term: names one word of letters, digits, '-', '_' or '.'")
		}
	default:
		return Term{}, fmt.Errorf("unknown predicate %q (want path, cmd, project, after, before or term)", kind)
	}
	return t, nil
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.'
}

// checkPattern refuses a path glob that could reach outside the tree it names.
func checkPattern(p string) error {
	if strings.Contains(p, "\\") {
		return errors.New("path: use forward slashes")
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return errors.New("path: a pattern cannot contain a parent (dot-dot) component")
		}
	}
	if _, err := path.Match(strings.TrimSuffix(p, "/**"), ""); err != nil {
		return fmt.Errorf("path: %v", err)
	}
	return nil
}

// String is the canonical text of the cue: terms in the order written, joined
// with "&" and no spaces around it.
func (c Cue) String() string {
	parts := make([]string, len(c.Terms))
	for i, t := range c.Terms {
		parts[i] = t.Kind + ":" + t.Arg
	}
	return strings.Join(parts, "&")
}

// Holds reports whether every term holds in the situation.
func (c Cue) Holds(s Situation) bool {
	if len(c.Terms) == 0 {
		return false
	}
	for _, t := range c.Terms {
		if !t.Holds(s) {
			return false
		}
	}
	return true
}

// Holds reports whether one predicate holds in the situation.
func (t Term) Holds(s Situation) bool {
	switch t.Kind {
	case KindPath:
		for _, p := range s.Paths {
			if pathMatches(t.Arg, p) {
				return true
			}
		}
		return false
	case KindCmd:
		cmd := strings.TrimSpace(s.Command)
		return cmd == t.Arg || strings.HasPrefix(cmd, t.Arg+" ")
	case KindProject:
		return s.Project != "" && strings.EqualFold(s.Project, t.Arg)
	case KindAfter:
		d, err := time.Parse(dateLayout, t.Arg)
		return err == nil && !s.Now.Before(d)
	case KindBefore:
		d, err := time.Parse(dateLayout, t.Arg)
		return err == nil && s.Now.Before(d)
	case KindTerm:
		want := strings.ToLower(t.Arg)
		for _, w := range strings.FieldsFunc(s.Prompt, func(r rune) bool { return !isWordRune(r) }) {
			if strings.ToLower(w) == want {
				return true
			}
		}
		return false
	}
	return false
}

// pathMatches reports whether a glob matches the file, or any suffix of it
// that starts at a component boundary. So deploy/* matches
// /home/me/repo/deploy/a.yml, and deploy/** matches anything below a deploy
// directory at any depth. A glob starting with / must match the whole path.
func pathMatches(glob, file string) bool {
	file = strings.ReplaceAll(file, "\\", "/")
	if file == "" {
		return false
	}
	if strings.HasPrefix(glob, "/") {
		return globMatch(glob, file) || (strings.HasSuffix(glob, "/**") && strings.HasPrefix(file, strings.TrimSuffix(glob, "/**")+"/"))
	}
	comps := strings.Split(file, "/")
	if len(comps) > maxPathComponents {
		comps = comps[len(comps)-maxPathComponents:]
	}
	for i := range comps {
		suffix := strings.Join(comps[i:], "/")
		if globMatch(glob, suffix) {
			return true
		}
	}
	return false
}

func globMatch(glob, name string) bool {
	if strings.HasSuffix(glob, "/**") {
		base := strings.TrimSuffix(glob, "/**")
		return name == base || strings.HasPrefix(name, base+"/")
	}
	ok, err := path.Match(glob, name)
	return err == nil && ok
}

// NormLife returns the stored form of a lifecycle: "" for standing, "once",
// or "until:<date>".
func NormLife(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == "standing":
		return LifeStanding, nil
	case s == LifeOnce:
		return LifeOnce, nil
	case strings.HasPrefix(s, LifeUntil):
		d, err := time.Parse(dateLayout, strings.TrimPrefix(s, LifeUntil))
		if err != nil {
			return "", errors.New("life: until wants a date as until:YYYY-MM-DD")
		}
		return LifeUntil + d.Format(dateLayout), nil
	}
	return "", fmt.Errorf("life must be once, standing or until:YYYY-MM-DD, not %q", s)
}

// Live reports whether an intention may still fire at all: it has not
// retired, and its until-date (inclusive of that UTC day) has not passed.
func Live(life, retired string, now time.Time) bool {
	if retired != "" {
		return false
	}
	if strings.HasPrefix(life, LifeUntil) {
		d, err := time.Parse(dateLayout, strings.TrimPrefix(life, LifeUntil))
		if err != nil {
			return false
		}
		return now.Before(d.AddDate(0, 0, 1))
	}
	return true
}

// Fires reports whether an entry is an intention whose cue holds now. An
// entry that is not an intention, that is superseded, retired, expired, hidden
// from defaults or written from an untrusted origin never fires. Every one of
// those is a refusal, not a filter on output, so a document cannot plant a
// reminder by including a cue line.
func Fires(e memory.Entry, s Situation) bool {
	if e.Cue == "" || e.Superseded() || e.Untrusted() || e.Hidden() || e.Redacted() {
		return false
	}
	if !Live(e.Life, e.Retired, s.Now) {
		return false
	}
	c, err := ParseCue(e.Cue)
	return err == nil && c.Holds(s)
}

// Located is an entry with the note it is stored in.
type Located struct {
	Note  string
	Entry memory.Entry
}

// Select returns the intentions that fire for a situation, most recent first
// and at most max of them. suppressed counts the firing ones that did not fit,
// so the caller can say so instead of dropping them silently.
func Select(notes []Located, s Situation, max int) (picked []Located, suppressed int) {
	var hit []Located
	for _, f := range notes {
		if Fires(f.Entry, s) {
			hit = append(hit, f)
		}
	}
	sort.SliceStable(hit, func(i, j int) bool {
		if hit[i].Entry.Stamp != hit[j].Entry.Stamp {
			return hit[i].Entry.Stamp > hit[j].Entry.Stamp
		}
		return hit[i].Entry.ID < hit[j].Entry.ID
	})
	if max <= 0 || len(hit) <= max {
		return hit, 0
	}
	return hit[:max], len(hit) - max
}

// CheckWritable refuses an intention from an untrusted origin. A cue is a
// standing instruction to surface text later, so a document may not write one.
func CheckWritable(origin string) error {
	if strings.TrimSpace(origin) != "" && (memory.Entry{Origin: strings.TrimSpace(origin)}).Untrusted() {
		return errors.New("an intention cannot be recorded from an untrusted origin")
	}
	return nil
}
