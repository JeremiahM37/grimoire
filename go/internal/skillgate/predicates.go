package skillgate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
	"github.com/JeremiahM37/grimoire/go/internal/skillmine"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// Context is what preconditions are checked against: the working directory,
// the machine, and the facts recorded about the person's setup. The functions
// are fields so a test can answer them without touching the machine.
type Context struct {
	Cwd     string
	Exists  func(path string) bool            // an absolute path
	Command func(name string) bool            // a program on PATH
	Env     func(name string) bool            // an environment variable that is set and non-empty
	Facts   []string                          // recorded facts; `fact:` matches one by substring, case-insensitively
	Run     func(anchor.Anchor) anchor.Result // runs an `anchor:` predicate; nil makes every anchor unmet
}

// NewContext is the real context: the filesystem, PATH and the environment.
func NewContext(cwd string, facts []string) Context {
	return Context{
		Cwd: cwd,
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		Command: func(n string) bool {
			_, err := exec.LookPath(n)
			return err == nil
		},
		Env: func(n string) bool {
			return os.Getenv(n) != ""
		},
		Facts: facts,
		Run:   anchorRunner,
	}
}

// Applicable reports whether a skill's preconditions hold here, and which do
// not. A spec with no preconditions always applies. An unknown predicate is
// unmet: a check that cannot be read must not let a skill inject blindly.
func Applicable(s Spec, ctx Context) (bool, []string) {
	var unmet []string
	for _, p := range s.Preconditions {
		if !holds(p, ctx) {
			unmet = append(unmet, p)
		}
	}
	return len(unmet) == 0, unmet
}

func holds(p string, ctx Context) bool {
	kind, arg, ok := strings.Cut(p, ":")
	if !ok || arg == "" {
		return false
	}
	switch kind {
	case "file":
		path := arg
		if !filepath.IsAbs(path) {
			path = filepath.Join(ctx.Cwd, path)
		}
		return ctx.Exists != nil && ctx.Exists(path)
	case "command":
		return ctx.Command != nil && ctx.Command(arg)
	case "env":
		return ctx.Env != nil && ctx.Env(arg)
	case "fact":
		want := strings.ToLower(arg)
		for _, f := range ctx.Facts {
			if strings.Contains(strings.ToLower(f), want) {
				return true
			}
		}
	case "anchor":
		return runAnchorPredicate(arg, ctx)
	}
	return false
}

// CheckPost evaluates postconditions against a finished session. A `ran:` unit
// holds when the session ran that step without an error; `last-ok` holds when
// its final shell command succeeded. Unknown predicates are unmet.
func CheckPost(s Spec, sess transcript.Session) (bool, []string) {
	units := skillmine.Units(sess)
	var unmet []string
	for _, p := range s.Postconditions {
		kind, arg, _ := strings.Cut(p, ":")
		ok := false
		switch {
		case kind == "ran" && arg != "":
			ok = ranUnit(units, arg) && !erroredUnit(sess, arg)
		case p == "last-ok":
			known, failed := skillmine.Outcome(sess)
			ok = known && !failed
		}
		if !ok {
			unmet = append(unmet, p)
		}
	}
	return len(unmet) == 0, unmet
}

func ranUnit(units []string, u string) bool {
	for _, x := range units {
		if x == u {
			return true
		}
	}
	return false
}

// erroredUnit is true when every run of the unit in the session failed.
func erroredUnit(sess transcript.Session, u string) bool {
	ran, errored := false, false
	for _, t := range sess.Turns {
		if t.Role != transcript.RoleTool {
			continue
		}
		for _, st := range skillmine.Normalize(t.ToolTarget) {
			if st.Unit == u {
				ran = true
				if !t.ToolError {
					return false
				}
				errored = true
			}
		}
	}
	return ran && errored
}
