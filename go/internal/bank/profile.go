package bank

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Disposition is how a bank's reflective answers lean: how readily it doubts
// (skepticism), how literally it reads (literalism), and how much weight it
// gives feelings (empathy). Each is 1..5 and 3 is neutral. Only explicit calls
// change it — never a model.
type Disposition struct {
	Skepticism int `json:"skepticism"`
	Literalism int `json:"literalism"`
	Empathy    int `json:"empathy"`
}

// Directive is a standing rule the bank's answers must respect.
type Directive struct {
	ID   string   `json:"id"`
	Name string   `json:"name,omitempty"`
	Text string   `json:"text"`
	Tags []string `json:"tags,omitempty"`
	// Priority orders directives in a reflect prompt, highest first.
	Priority int `json:"priority,omitempty"`
	// Inactive keeps a directive on file without applying it.
	Inactive bool `json:"inactive,omitempty"`
}

// Profile is a bank's identity and configuration, read from bank.md.
type Profile struct {
	ID            string            `json:"bank_id"`
	Name          string            `json:"name"`
	Mission       string            `json:"mission"`
	RetainMission string            `json:"retain_mission"`
	Disposition   Disposition       `json:"disposition"`
	Tags          []string          `json:"tags"`
	Directives    []Directive       `json:"directives"`
	Config        map[string]string `json:"config"`
	Created       string            `json:"created,omitempty"`
	Updated       string            `json:"updated,omitempty"`
	// Notes is any body text outside the sections this package owns — what a
	// person wrote in bank.md for themselves. It is kept verbatim on rewrite.
	Notes string `json:"-"`
}

// NewProfile is a profile with the defaults a bank starts from.
func NewProfile(id string) *Profile {
	return &Profile{ID: id, Name: id, Disposition: Disposition{3, 3, 3},
		Tags: []string{}, Directives: []Directive{}, Config: map[string]string{}}
}

// ConfigKeys are the per-bank settings a bank.md may carry, with the values
// each accepts. A key outside this table is refused rather than stored: a
// misspelt setting that is silently kept is a control the operator believes
// they have.
var ConfigKeys = map[string]func(string) error{
	"retain_extraction_mode": oneOf("concise", "verbatim", "chunks"),
	"retain_chunk_size":      intRange(500, 20000),
	"retain_extract_causal":  oneOf("true", "false"),
	"enable_text_search":     oneOf("true", "false"),
	"enable_graph":           oneOf("true", "false"),
	"enable_temporal":        oneOf("true", "false"),
	"enable_reranking":       oneOf("true", "false"),
	// Personal-data screening on retain: off, redact (typed placeholders) or
	// flag (kept, with the kinds noted in the item's metadata).
	"pii_screening": oneOf("off", "redact", "flag"),
	// When consolidation runs: after every retain (auto, the default when a
	// model is configured), only when asked (manual), or never (off).
	"consolidation": oneOf("auto", "manual", "off"),
	// What consolidation should track, in the bank's own words.
	"observations_mission": anyText,
	// Facts per consolidation model call.
	"consolidation_batch_size": intRange(1, 32),
	// The MCP tools an agent may use on this bank, comma separated; empty
	// means all.
	"mcp_tools": toolList,
	// The answer length target of reflect when a call names none.
	"reflect_max_tokens": intRange(256, 16000),
}

func anyText(string) error { return nil }

func toolList(v string) error {
	for _, t := range strings.Split(v, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		for _, r := range t {
			if !(r >= 'a' && r <= 'z' || r == '_') {
				return fmt.Errorf("must be a comma-separated list of tool names")
			}
		}
	}
	return nil
}

func oneOf(vals ...string) func(string) error {
	return func(v string) error {
		for _, x := range vals {
			if v == x {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(vals, ", "))
	}
}

func intRange(lo, hi int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("must be an integer in %d..%d", lo, hi)
		}
		return nil
	}
}

// ValidateConfig checks a config map against ConfigKeys.
func ValidateConfig(cfg map[string]string) error {
	for k, v := range cfg {
		check, ok := ConfigKeys[k]
		if !ok {
			return fmt.Errorf("unknown bank setting %q", k)
		}
		if err := check(v); err != nil {
			return fmt.Errorf("%s %v", k, err)
		}
	}
	return nil
}

// ValidateDisposition checks each trait is 1..5.
func ValidateDisposition(d Disposition) error {
	for name, v := range map[string]int{"skepticism": d.Skepticism, "literalism": d.Literalism, "empathy": d.Empathy} {
		if v < 1 || v > 5 {
			return fmt.Errorf("disposition %s must be 1..5", name)
		}
	}
	return nil
}

// Setting reads a config value, falling back to def.
func (p *Profile) Setting(key, def string) string {
	if p == nil {
		return def
	}
	if v, ok := p.Config[key]; ok && v != "" {
		return v
	}
	return def
}

// The body sections this package owns. Anything else in bank.md is a person's
// own notes and survives every rewrite.
const (
	secMission    = "Mission"
	secRetain     = "Retain mission"
	secDirectives = "Directives"
	secConfig     = "Config"
)

// ParseProfile reads a bank.md.
func ParseProfile(id string, fm *markdown.Frontmatter, body string) *Profile {
	p := NewProfile(id)
	if n := strings.TrimSpace(fm.StringVal("name")); n != "" {
		p.Name = n
	}
	p.Disposition = Disposition{
		Skepticism: traitOr(fm.StringVal("skepticism")),
		Literalism: traitOr(fm.StringVal("literalism")),
		Empathy:    traitOr(fm.StringVal("empathy")),
	}
	if v, ok := fm.Get("bank_tags"); ok {
		p.Tags = valueList(v)
	}
	p.Created = fm.StringVal("created")
	p.Updated = fm.StringVal("updated")

	var notes []string
	for _, sec := range splitSections(body) {
		switch strings.ToLower(sec.heading) {
		case strings.ToLower(secMission):
			p.Mission = strings.TrimSpace(sec.text)
		case strings.ToLower(secRetain):
			p.RetainMission = strings.TrimSpace(sec.text)
		case strings.ToLower(secDirectives):
			p.Directives = parseDirectives(sec.text)
		case strings.ToLower(secConfig):
			p.Config = parseConfigLines(sec.text)
		default:
			notes = append(notes, sec.raw)
		}
	}
	p.Notes = strings.TrimSpace(strings.Join(notes, ""))
	return p
}

func traitOr(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 5 {
		return 3
	}
	return n
}

func valueList(v markdown.Value) []string {
	out := []string{}
	switch t := v.(type) {
	case []markdown.Value:
		for _, x := range t {
			if s := strings.TrimSpace(fmt.Sprint(x)); s != "" {
				out = append(out, s)
			}
		}
	case string:
		for _, x := range strings.Split(t, ",") {
			if s := strings.TrimSpace(x); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

type section struct {
	heading string // "" for text before the first "## "
	text    string
	raw     string
}

// splitSections cuts a body on level-two headings. The level-one title line
// the writer emits is not a section and is dropped from the preamble.
func splitSections(body string) []section {
	var out []section
	cur := section{}
	var buf, raw strings.Builder
	flush := func() {
		cur.text, cur.raw = buf.String(), raw.String()
		if cur.heading != "" || strings.TrimSpace(cur.text) != "" {
			out = append(out, cur)
		}
		buf.Reset()
		raw.Reset()
	}
	for _, ln := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimRight(ln, "\r\n")
		if strings.HasPrefix(trimmed, "## ") {
			flush()
			cur = section{heading: strings.TrimSpace(trimmed[3:])}
			raw.WriteString(ln)
			continue
		}
		if cur.heading == "" && strings.HasPrefix(trimmed, "# ") {
			continue
		}
		buf.WriteString(ln)
		raw.WriteString(ln)
	}
	flush()
	return out
}

func parseDirectives(text string) []Directive {
	out := []Directive{}
	for _, ln := range strings.Split(text, "\n") {
		s := strings.TrimSpace(ln)
		if !strings.HasPrefix(s, "- ") {
			continue
		}
		s = strings.TrimSpace(s[2:])
		d := Directive{}
		if i := strings.Index(s, "<!--d "); i >= 0 {
			tr := s[i:]
			s = strings.TrimSpace(s[:i])
			for k, v := range trailerFields(strings.TrimSuffix(strings.TrimPrefix(tr, "<!--d "), "-->")) {
				switch k {
				case "id":
					d.ID = v
				case "tags":
					d.Tags = splitComma(v)
				case "name":
					d.Name = unescapeField(v)
				case "prio":
					d.Priority, _ = atoi(v)
				case "off":
					d.Inactive = true
				}
			}
		}
		if s == "" {
			continue
		}
		d.Text = s
		if d.ID == "" {
			// A directive a person typed into the file has no id yet; one
			// derived from its text is stable across reindexes.
			d.ID = "d" + shortHash(s, 10)
		}
		out = append(out, d)
	}
	return out
}

func parseConfigLines(text string) map[string]string {
	out := map[string]string{}
	for _, ln := range strings.Split(text, "\n") {
		s := strings.TrimSpace(ln)
		if !strings.HasPrefix(s, "- ") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(s[2:]), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// Frontmatter renders the profile's frontmatter.
func (p *Profile) Frontmatter() *markdown.Frontmatter {
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Bank: "+oneLine(p.Name))
	fm.Set("bank", p.ID)
	fm.Set("name", oneLine(p.Name))
	fm.Set("skepticism", strconv.Itoa(p.Disposition.Skepticism))
	fm.Set("literalism", strconv.Itoa(p.Disposition.Literalism))
	fm.Set("empathy", strconv.Itoa(p.Disposition.Empathy))
	tags := make([]markdown.Value, 0, len(p.Tags))
	for _, t := range p.Tags {
		tags = append(tags, t)
	}
	fm.Set("bank_tags", tags)
	return fm
}

// Body renders the profile's markdown body.
func (p *Profile) Body() string {
	var b strings.Builder
	b.WriteString("# " + oneLine(p.Name) + "\n\n")
	b.WriteString("## " + secMission + "\n\n")
	if p.Mission != "" {
		b.WriteString(strings.TrimSpace(p.Mission) + "\n\n")
	}
	b.WriteString("## " + secRetain + "\n\n")
	if p.RetainMission != "" {
		b.WriteString(strings.TrimSpace(p.RetainMission) + "\n\n")
	}
	b.WriteString("## " + secDirectives + "\n\n")
	for _, d := range p.Directives {
		b.WriteString("- " + oneLine(d.Text) + " <!--d id=" + escapeField(d.ID))
		if d.Name != "" {
			b.WriteString(" name=" + escapeField(oneLine(d.Name)))
		}
		if d.Priority != 0 {
			b.WriteString(" prio=" + itoa(d.Priority))
		}
		if d.Inactive {
			b.WriteString(" off")
		}
		if len(d.Tags) > 0 {
			b.WriteString(" tags=" + joinComma(d.Tags))
		}
		b.WriteString(" -->\n")
	}
	if len(p.Directives) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("## " + secConfig + "\n\n")
	keys := make([]string, 0, len(p.Config))
	for k := range p.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("- " + k + ": " + oneLine(p.Config[k]) + "\n")
	}
	if p.Notes != "" {
		b.WriteString("\n" + strings.TrimSpace(p.Notes) + "\n")
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
