package queries

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Properties, formulas and grouping — the database-view half of a query block.
//
// A note's frontmatter is where people put the fields they later want to sort
// and filter by: status, priority, owner, due. Without this, a query block
// could find notes by tag and path and nothing else, so anyone keeping
// structured data in a vault had to keep it somewhere else too.
//
// Filtering happens in SQL through json_extract rather than by fetching
// everything and sifting in Go, so the LIMIT still means something. The
// property NAME reaches SQL as a bound path parameter and is checked against a
// conservative pattern first — a query block is user-authored text, and this
// package's whole design rule is that nothing user-authored becomes SQL.

// propertyKeyRE is what may name a frontmatter field. Deliberately narrow:
// the name travels into a json_extract path, and a pattern that admits quotes
// or dots would let a block reach into a value it was not given.
var propertyKeyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// Comparison operators a filter may use.
const (
	opEq       = "="
	opNe       = "!="
	opGt       = ">"
	opGe       = ">="
	opLt       = "<"
	opLe       = "<="
	opContains = "contains"
	opExists   = "exists"
	opMissing  = "missing"
)

var operators = []string{opGe, opLe, opNe, opEq, opGt, opLt}

// PropertyFilter is one `where:` line.
type PropertyFilter struct {
	Key   string
	Op    string
	Value string
}

// parseWhere reads "status = active", "priority >= 2", "owner contains ana",
// "due exists".
func parseWhere(val string) (PropertyFilter, error) {
	text := strings.TrimSpace(val)
	for _, word := range []string{opContains, opExists, opMissing} {
		if key, rest, ok := cutWord(text, word); ok {
			if !propertyKeyRE.MatchString(key) {
				return PropertyFilter{}, fmt.Errorf("'%s' is not a property name", key)
			}
			if word == opContains && strings.TrimSpace(rest) == "" {
				return PropertyFilter{}, fmt.Errorf("'contains' needs something to look for")
			}
			return PropertyFilter{Key: key, Op: word, Value: unquote(rest)}, nil
		}
	}
	for _, op := range operators {
		if i := strings.Index(text, op); i > 0 {
			key := strings.TrimSpace(text[:i])
			value := unquote(text[i+len(op):])
			if !propertyKeyRE.MatchString(key) {
				return PropertyFilter{}, fmt.Errorf("'%s' is not a property name", key)
			}
			if value == "" {
				return PropertyFilter{}, fmt.Errorf("'%s' needs a value to compare with", key)
			}
			return PropertyFilter{Key: key, Op: op, Value: value}, nil
		}
	}
	return PropertyFilter{}, fmt.Errorf(
		"where needs 'property = value' (or !=, >, <, >=, <=, contains, exists)")
}

// cutWord splits on a bare word operator, so "owner contains ana" is a
// contains filter but "container = docker" is not.
func cutWord(text, word string) (before, after string, ok bool) {
	fields := strings.Fields(text)
	for i, f := range fields {
		if !strings.EqualFold(f, word) || i == 0 {
			continue
		}
		return strings.Join(fields[:i], " "), strings.Join(fields[i+1:], " "), true
	}
	return "", "", false
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// jsonPath renders a property name as a json_extract path. The name has
// already been matched against propertyKeyRE, which admits no quote and no
// dot, so the path cannot be broken out of.
func jsonPath(key string) string { return "$." + key }

// sql renders one filter as a condition plus its bound parameters.
//
// A numeric comparison casts BOTH sides, because frontmatter is text and
// "10" < "9" is true as a string — which is the kind of wrong that looks like
// the query working.
func (f PropertyFilter) sql(column string) (string, []any) {
	extract := fmt.Sprintf("json_extract(%s, ?)", column)
	switch f.Op {
	case opExists:
		return "(" + extract + " IS NOT NULL AND " + extract + " != '')",
			[]any{jsonPath(f.Key), jsonPath(f.Key)}
	case opMissing:
		return "(" + extract + " IS NULL OR " + extract + " = '')",
			[]any{jsonPath(f.Key), jsonPath(f.Key)}
	case opContains:
		return "(" + extract + " LIKE ? ESCAPE '\\' COLLATE NOCASE)",
			[]any{jsonPath(f.Key), "%" + likeEscape(f.Value) + "%"}
	case opEq:
		return "(CAST(" + extract + " AS TEXT) = ? COLLATE NOCASE)",
			[]any{jsonPath(f.Key), f.Value}
	case opNe:
		return "(CAST(" + extract + " AS TEXT) != ? COLLATE NOCASE OR " + extract + " IS NULL)",
			[]any{jsonPath(f.Key), f.Value, jsonPath(f.Key)}
	}
	if _, err := strconv.ParseFloat(f.Value, 64); err == nil {
		return fmt.Sprintf("(CAST(%s AS REAL) %s CAST(? AS REAL))", extract, f.Op),
			[]any{jsonPath(f.Key), f.Value}
	}
	return fmt.Sprintf("(CAST(%s AS TEXT) %s ?)", extract, f.Op),
		[]any{jsonPath(f.Key), f.Value}
}

// Formula is a computed column: `formula: age = days_since(updated)`.
type Formula struct {
	Name string
	Fn   string
	Args []string
}

// formulaFns are the only computations a block may run. A whitelist rather
// than an expression language: a query block is authored by whoever can write
// a note, and "arbitrary evaluation over your vault" is a much larger promise
// than "show me a table".
var formulaFns = map[string]int{
	"days_since": 1, "days_until": 1, "year": 1, "month": 1, "date": 1,
	"upper": 1, "lower": 1, "length": 1, "default": 2, "concat": 2,
}

var formulaCallRE = regexp.MustCompile(`^([a-z_]+)\((.*)\)$`)

func parseFormula(val string) (Formula, error) {
	name, expr, ok := strings.Cut(val, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return Formula{}, fmt.Errorf("formula needs 'name = function(field)'")
	}
	if !propertyKeyRE.MatchString(name) {
		return Formula{}, fmt.Errorf("'%s' is not a usable column name", name)
	}
	m := formulaCallRE.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		return Formula{}, fmt.Errorf("'%s' is not a function call", strings.TrimSpace(expr))
	}
	want, known := formulaFns[m[1]]
	if !known {
		return Formula{}, fmt.Errorf("unknown function '%s' (have: %s)",
			m[1], strings.Join(formulaNames(), ", "))
	}
	var args []string
	for _, a := range splitArgs(m[2]) {
		if a != "" {
			args = append(args, a)
		}
	}
	if len(args) != want {
		return Formula{}, fmt.Errorf("'%s' takes %d argument(s), got %d",
			m[1], want, len(args))
	}
	return Formula{Name: name, Fn: m[1], Args: args}, nil
}

func formulaNames() []string {
	out := make([]string, 0, len(formulaFns))
	for name := range formulaFns {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

func splitArgs(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// Apply computes the formula for one row. A formula that cannot be computed —
// a field that is not there, a date that will not parse — yields an empty
// cell rather than an error, because one unparseable note should not blank a
// whole table.
func (f Formula) Apply(row map[string]any, now time.Time) any {
	arg := func(i int) string {
		if i >= len(f.Args) {
			return ""
		}
		if lit := unquoteLiteral(f.Args[i]); lit != nil {
			return *lit
		}
		return asText(row[f.Args[i]])
	}
	switch f.Fn {
	case "days_since", "days_until", "year", "month", "date":
		t, ok := parseDate(arg(0))
		if !ok {
			return ""
		}
		switch f.Fn {
		case "days_since":
			return int(now.Sub(t).Hours() / 24)
		case "days_until":
			return int(t.Sub(now).Hours() / 24)
		case "year":
			return t.Year()
		case "month":
			return int(t.Month())
		default:
			return t.Format("2006-01-02")
		}
	case "upper":
		return strings.ToUpper(arg(0))
	case "lower":
		return strings.ToLower(arg(0))
	case "length":
		return len([]rune(arg(0)))
	case "default":
		if v := arg(0); strings.TrimSpace(v) != "" {
			return v
		}
		return arg(1)
	case "concat":
		return arg(0) + arg(1)
	}
	return ""
}

func unquoteLiteral(s string) *string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		out := s[1 : len(s)-1]
		return &out
	}
	return nil
}

func asText(v any) string {
	switch typed := v.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case []string:
		return strings.Join(typed, ", ")
	default:
		return fmt.Sprint(typed)
	}
}

// parseDate accepts the shapes frontmatter dates actually come in.
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05",
		"2006-01-02 15:04", "2006-01-02", "2006/01/02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
