// Package rulecheck compiles standing rules ("never X", "always Y", "before X
// do Y") into checks over an agent's tool calls, measures each check's
// precision against real transcript history, and decides which checks are
// precise enough to act on. Nothing here calls a hosted model except the
// optional labelling pass; candidates come from the text itself and from a
// local model.
package rulecheck

import (
	"fmt"
	"regexp"
	"strings"
)

// Shapes a rule can compile to.
const (
	ShapeForbid        = "forbid"         // the action pattern must never occur
	ShapeRequireBefore = "require_before" // the action must be preceded, in the session, by Before
	ShapeRequireWith   = "require_with"   // the action must carry With in the same call
)

// Limits that keep a compiled check cheap and reviewable.
const (
	MaxPattern    = 200
	MaxTargetScan = 4096 // bytes of a tool target a pattern is run against
	MaxHistory    = 400  // earlier calls kept per session for require_before
)

// Scope narrows where a check applies. Empty fields mean anywhere.
type Scope struct {
	Cwd   string `json:"cwd,omitempty"`   // RE2 over the session's working directory (repo or path)
	Agent string `json:"agent,omitempty"` // an agent name, e.g. claude-code
}

// Spec is the machine-checkable form of one rule.
type Spec struct {
	Shape  string   `json:"shape"`
	Tools  []string `json:"tools,omitempty"` // tool names it applies to; empty = Bash
	Action string   `json:"action"`          // RE2 over the tool target
	Before string   `json:"before,omitempty"`
	With   string   `json:"with,omitempty"`
	Scope  Scope    `json:"scope,omitempty"`
}

// DefaultTools is where a check looks when the spec names none.
var DefaultTools = []string{"Bash"}

// vague patterns that match nearly everything.
var vague = regexp.MustCompile(`^(\.\*|\.\+|\^|\$|\\s\*?|\\w\+?|\\b|\.)$`)

// Compiled is a Spec with its regexes built.
type Compiled struct {
	Spec
	action, before, with, cwd *regexp.Regexp
}

func compileOne(name, p string, required bool) (*regexp.Regexp, error) {
	if p == "" {
		if required {
			return nil, fmt.Errorf("%s is required", name)
		}
		return nil, nil
	}
	if len(p) > MaxPattern {
		return nil, fmt.Errorf("%s longer than %d bytes", name, MaxPattern)
	}
	if vague.MatchString(strings.TrimSpace(p)) {
		return nil, fmt.Errorf("%s %q matches nearly everything", name, p)
	}
	re, err := regexp.Compile(p) // RE2: linear time, no backtracking
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	if re.MatchString("") {
		return nil, fmt.Errorf("%s matches the empty string", name)
	}
	return re, nil
}

// Compile validates and builds a Spec.
func (s Spec) Compile() (*Compiled, error) {
	c := &Compiled{Spec: s}
	var err error
	switch s.Shape {
	case ShapeForbid:
		if s.Before != "" || s.With != "" {
			return nil, fmt.Errorf("forbid takes only an action pattern")
		}
	case ShapeRequireBefore:
		if s.Before == "" || s.With != "" {
			return nil, fmt.Errorf("require_before needs before (and no with)")
		}
	case ShapeRequireWith:
		if s.With == "" || s.Before != "" {
			return nil, fmt.Errorf("require_with needs with (and no before)")
		}
	default:
		return nil, fmt.Errorf("unknown shape %q", s.Shape)
	}
	if c.action, err = compileOne("action", s.Action, true); err != nil {
		return nil, err
	}
	if c.before, err = compileOne("before", s.Before, false); err != nil {
		return nil, err
	}
	if c.with, err = compileOne("with", s.With, false); err != nil {
		return nil, err
	}
	if c.cwd, err = compileOne("scope.cwd", s.Scope.Cwd, false); err != nil {
		return nil, err
	}
	if len(c.Tools) > 8 {
		return nil, fmt.Errorf("at most 8 tools")
	}
	return c, nil
}

func clipTarget(t string) string {
	if len(t) > MaxTargetScan {
		return t[:MaxTargetScan]
	}
	return t
}

// AppliesTo reports whether a call is in the check's tool and scope.
func (c *Compiled) AppliesTo(tool, cwd, agent string) bool {
	tools := c.Tools
	if len(tools) == 0 {
		tools = DefaultTools
	}
	ok := false
	for _, t := range tools {
		if strings.EqualFold(t, tool) {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	if c.Scope.Agent != "" && !strings.EqualFold(c.Scope.Agent, agent) {
		return false
	}
	if c.cwd != nil && !c.cwd.MatchString(cwd) {
		return false
	}
	return true
}

// IsAction reports whether the call is the thing the rule is about.
func (c *Compiled) IsAction(target string) bool { return c.action.MatchString(clipTarget(target)) }

// Violates reports whether the call breaks the rule. earlier are the targets
// of the session's earlier calls in the same tool family (oldest first); only
// require_before reads them.
func (c *Compiled) Violates(target string, earlier []string) bool {
	t := clipTarget(target)
	if !c.action.MatchString(t) {
		return false
	}
	switch c.Shape {
	case ShapeForbid:
		return true
	case ShapeRequireWith:
		return !c.with.MatchString(t)
	case ShapeRequireBefore:
		// The call that is itself the prerequisite satisfies it (a compound
		// "prep && action" line), as does any earlier call.
		if c.before.MatchString(t) {
			return false
		}
		for _, e := range earlier {
			if c.before.MatchString(clipTarget(e)) {
				return false
			}
		}
		return true
	}
	return false
}

// Describe is a one-line human reading of the check.
func (s Spec) Describe() string {
	scope := ""
	if s.Scope.Cwd != "" {
		scope += " in cwd~" + s.Scope.Cwd
	}
	if s.Scope.Agent != "" {
		scope += " for " + s.Scope.Agent
	}
	switch s.Shape {
	case ShapeForbid:
		return fmt.Sprintf("forbid /%s/%s", s.Action, scope)
	case ShapeRequireBefore:
		return fmt.Sprintf("/%s/ needs earlier /%s/%s", s.Action, s.Before, scope)
	case ShapeRequireWith:
		return fmt.Sprintf("/%s/ must carry /%s/%s", s.Action, s.With, scope)
	}
	return s.Shape
}
