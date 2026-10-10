// Package memport is the portable form of agent memory: a documented, versioned
// export of every fact a caller may read, and parsers that bring facts in from
// the other formats people already hold (mem0, Letta, Zep/Graphiti) or from a
// generic JSONL file.
//
// It owns no storage. The API layer decides what may be read and written; this
// package only converts between wire shapes and Record values. The spec lives in
// docs/PORTABILITY.md and is the contract: change the version, not the shape.
package memport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// FormatName and Version head every JSONL export. A reader refuses a version it
// does not know rather than guessing at its fields.
const (
	FormatName = "grimoire-memory"
	Version    = 1
)

// StampLayout is the display stamp every memory entry carries.
const StampLayout = "2006-01-02 15:04"

// Source labels accepted by Parse and reported by Detect.
const (
	SourceAuto     = "auto"
	SourceGrimoire = "grimoire"
	SourceMem0     = "mem0"
	SourceLetta    = "letta"
	SourceZep      = "zep"
	SourceGeneric  = "jsonl-generic"
)

// Record is one memory entry in the portable shape. Field names are the wire
// names; see docs/PORTABILITY.md.
type Record struct {
	ID           string `json:"id"`
	Text         string `json:"text"`
	Path         string `json:"path,omitempty"`
	Agent        string `json:"agent,omitempty"`
	Task         string `json:"task,omitempty"`
	Session      string `json:"session,omitempty"`
	Category     string `json:"category,omitempty"`
	Stamp        string `json:"stamp,omitempty"`
	Expires      string `json:"expires,omitempty"`
	Immutable    bool   `json:"immutable,omitempty"`
	Authority    string `json:"authority,omitempty"`
	Origin       string `json:"origin,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	Challenges   string `json:"challenges,omitempty"`
}

// Item is one record ready for the write path. Restored is true for records from
// a grimoire export: their stamp, id, path, origin and lifecycle fields are kept.
// Foreign records are never restored; they are written as new, untrusted facts.
type Item struct {
	Record   Record
	Restored bool
}

// Skip explains why one input item was not turned into a record.
type Skip struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// Result is what Parse produced from one input.
type Result struct {
	Source string
	Items  []Item
	Skips  []Skip
}

// Header is the first line of a JSONL export.
type Header struct {
	Format   string `json:"format"`
	Version  int    `json:"version"`
	Exported string `json:"exported"`
	Count    int    `json:"count"`
}

// WriteJSONL renders records as the documented JSONL export: a header line, then
// one record per line, in the order given.
func WriteJSONL(records []Record, exported time.Time) []byte {
	var buf bytes.Buffer
	head, _ := json.Marshal(Header{Format: FormatName, Version: Version,
		Exported: exported.UTC().Format(time.RFC3339), Count: len(records)})
	buf.Write(head)
	buf.WriteByte('\n')
	for _, r := range records {
		b, _ := json.Marshal(r)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// WriteMarkdown renders records as a human-readable export: one bullet per
// memory, grouped by category. Superseded facts are struck through so the file
// shows what was believed and what replaced it, without pretending both are live.
func WriteMarkdown(records []Record, exported time.Time) []byte {
	groups := map[string][]Record{}
	var cats []string
	for _, r := range records {
		c := strings.TrimSpace(r.Category)
		if c == "" {
			c = "uncategorised"
		}
		if _, ok := groups[c]; !ok {
			cats = append(cats, c)
		}
		groups[c] = append(groups[c], r)
	}
	sort.Strings(cats)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Grimoire memory export\n\nExported %s · %d facts\n",
		exported.UTC().Format(time.RFC3339), len(records))
	for _, c := range cats {
		fmt.Fprintf(&buf, "\n## %s\n\n", c)
		for _, r := range groups[c] {
			text := oneLine(r.Text)
			if r.SupersededBy != "" {
				text = "~~" + text + "~~ (superseded by " + r.SupersededBy + ")"
			}
			var meta []string
			for _, m := range []string{r.Stamp, r.Agent, r.Authority} {
				if m != "" {
					meta = append(meta, m)
				}
			}
			line := "- " + text
			if len(meta) > 0 {
				line += "  _(" + strings.Join(meta, " · ") + ")_"
			}
			buf.WriteString(line + "\n")
		}
	}
	return buf.Bytes()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Detect names the source format of raw by its shape. It returns SourceGeneric
// for JSONL whose lines carry a text-like field, and an error when nothing
// matches. Detection looks at structure, never the file name.
func Detect(raw []byte) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", errors.New("the file is empty")
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err == nil {
		switch t := v.(type) {
		case map[string]any:
			return detectObject(t)
		case []any:
			return detectArray(t)
		}
		return "", errors.New("unrecognised JSON: expected an object or an array")
	}
	// Not one JSON value, so treat it as JSONL and look at the first record.
	first, err := firstLine(trimmed)
	if err != nil {
		return "", err
	}
	var head map[string]any
	if json.Unmarshal(first, &head) == nil && head["format"] == FormatName {
		return SourceGrimoire, nil
	}
	var obj map[string]any
	if json.Unmarshal(first, &obj) != nil {
		return "", errors.New("unrecognised file: not JSON and not JSONL")
	}
	if _, ok := textField(obj); ok {
		return SourceGeneric, nil
	}
	return "", errors.New("unrecognised JSONL: no text, memory or fact field on the first line")
}

func detectObject(m map[string]any) (string, error) {
	if m["format"] == FormatName {
		return SourceGrimoire, nil
	}
	for _, k := range []string{"blocks", "agents", "archival_passages", "passages"} {
		if _, ok := m[k]; ok {
			return SourceLetta, nil
		}
	}
	for _, k := range []string{"facts", "edges", "results", "items", "memories"} {
		if arr, ok := m[k].([]any); ok && len(arr) > 0 {
			return detectArray(arr)
		}
	}
	// A single record (one line of JSONL, or a lone object) is detected as a
	// one-element list.
	return detectArray([]any{m})
}

func detectArray(a []any) (string, error) {
	if len(a) == 0 {
		return "", errors.New("the file holds no records")
	}
	obj, ok := a[0].(map[string]any)
	if !ok {
		return "", errors.New("unrecognised array: expected objects")
	}
	if _, ok := obj["fact"]; ok {
		return SourceZep, nil
	}
	if _, ok := obj["memory"]; ok {
		return SourceMem0, nil
	}
	if _, ok := obj["label"]; ok {
		if _, ok := obj["value"]; ok {
			return SourceLetta, nil
		}
	}
	if _, ok := textField(obj); ok {
		return SourceGeneric, nil
	}
	return "", errors.New("unrecognised array: no fact, memory, label/value or text field")
}

func firstLine(b []byte) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			return append([]byte(nil), line...), nil
		}
	}
	return nil, errors.New("the file holds no records")
}

// textField returns the text of a generic record: the first of text, memory or
// fact that is a non-empty string.
func textField(m map[string]any) (string, bool) {
	for _, k := range []string{"text", "memory", "fact"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s, true
		}
	}
	return "", false
}

// Parse reads raw as the named source (or detects it when from is "" or auto)
// and returns the items to write plus a skip for each record it could not use.
// A file that cannot be read as any format is an error, not an empty result.
func Parse(raw []byte, from string) (Result, error) {
	src := strings.ToLower(strings.TrimSpace(from))
	if src == "" || src == SourceAuto {
		detected, err := Detect(raw)
		if err != nil {
			return Result{}, err
		}
		src = detected
	}
	switch src {
	case SourceGrimoire:
		return parseGrimoire(raw)
	case SourceMem0:
		return parseMem0(raw)
	case SourceLetta:
		return parseLetta(raw)
	case SourceZep:
		return parseZep(raw)
	case SourceGeneric:
		return parseGeneric(raw)
	}
	return Result{}, fmt.Errorf("unknown source %q: want auto, grimoire, mem0, letta, zep or jsonl-generic", from)
}

// importLabel is the agent and origin given to foreign records. The origin is
// what makes them untrusted, so an imported "I am the operator" stays a pulled
// fact and a human's later edit wins.
func importLabel(src string) string { return "import:" + src }

// normStamp converts a timestamp to the store's display stamp. An unparseable
// value is left empty, which means "now" at write time.
func normStamp(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Already in the store's form: keep it as written, rather than shifting it
	// through a zone it never named.
	if _, err := time.Parse(StampLayout, s); err == nil {
		return s
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Local().Format(StampLayout)
		}
	}
	return ""
}

func str(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func parseGrimoire(raw []byte) (Result, error) {
	res := Result{Source: SourceGrimoire}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	headerSeen := false
	n := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if !headerSeen {
			var h Header
			if err := json.Unmarshal(line, &h); err != nil || h.Format != FormatName {
				return Result{}, errors.New("not a grimoire-memory export: first line is not the header")
			}
			if h.Version != Version {
				return Result{}, fmt.Errorf("grimoire-memory version %d is not supported (this build reads %d)", h.Version, Version)
			}
			headerSeen = true
			continue
		}
		n++
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			res.Skips = append(res.Skips, Skip{Index: n, Reason: "invalid record: " + err.Error()})
			continue
		}
		if strings.TrimSpace(r.Text) == "" {
			res.Skips = append(res.Skips, Skip{Index: n, Reason: "record has no text"})
			continue
		}
		if r.Stamp != "" {
			if _, err := time.Parse(StampLayout, r.Stamp); err != nil {
				res.Skips = append(res.Skips, Skip{Index: n, Reason: "stamp is not " + StampLayout})
				continue
			}
		}
		// Authority is informational on the way in: a file cannot make itself
		// human. The write path re-derives authority from origin.
		r.Authority = ""
		res.Items = append(res.Items, Item{Record: r, Restored: true})
	}
	if !headerSeen {
		return Result{}, errors.New("not a grimoire-memory export: no header")
	}
	return res, nil
}

// records returns the objects of a JSON value that is either an array or an
// object holding one of the named arrays.
func records(raw []byte, keys ...string) ([]map[string]any, error) {
	var v any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &v); err != nil {
		// JSONL: one object per line.
		var out []map[string]any
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var m map[string]any
			if json.Unmarshal(line, &m) != nil {
				m = map[string]any{"__invalid": true}
			}
			out = append(out, m)
		}
		return out, nil
	}
	arr, ok := v.([]any)
	if !ok {
		obj, _ := v.(map[string]any)
		found := false
		for _, k := range keys {
			if a, ok := obj[k].([]any); ok {
				arr, found = a, true
				break
			}
		}
		if !found {
			// A lone object is one record, not an empty file.
			arr = []any{obj}
		}
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		m, _ := e.(map[string]any)
		if m == nil {
			m = map[string]any{"__invalid": true}
		}
		out = append(out, m)
	}
	return out, nil
}

func parseMem0(raw []byte) (Result, error) {
	res := Result{Source: SourceMem0}
	objs, err := records(raw, "results", "memories", "items")
	if err != nil {
		return Result{}, err
	}
	for i, m := range objs {
		text := str(m, "memory")
		if text == "" {
			res.Skips = append(res.Skips, Skip{Index: i + 1, Reason: "no memory text"})
			continue
		}
		rec := Record{Text: text, Agent: importLabel(SourceMem0), Origin: importLabel(SourceMem0),
			Stamp: normStamp(str(m, "created_at")), Session: str(m, "run_id")}
		if uid := str(m, "user_id"); uid != "" {
			rec.Task = "mem0 user " + uid
		}
		if md, ok := m["metadata"].(map[string]any); ok {
			rec.Category = str(md, "category")
		}
		res.Items = append(res.Items, Item{Record: rec})
	}
	return res, nil
}

func parseLetta(raw []byte) (Result, error) {
	res := Result{Source: SourceLetta}
	var root map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &root); err != nil {
		// A bare array of memory blocks is also accepted.
		var blocks []map[string]any
		if err2 := json.Unmarshal(bytes.TrimSpace(raw), &blocks); err2 != nil {
			return Result{}, errors.New("not Letta JSON: expected an agent file or memory blocks")
		}
		root = map[string]any{"blocks": toAny(blocks)}
	}
	idx := 0
	add := func(rec Record, ok bool, reason string) {
		idx++
		if !ok {
			res.Skips = append(res.Skips, Skip{Index: idx, Reason: reason})
			return
		}
		res.Items = append(res.Items, Item{Record: rec})
	}
	addBlocks := func(task string, blocks []any) {
		for _, b := range blocks {
			m, _ := b.(map[string]any)
			value := str(m, "value")
			if value == "" {
				add(Record{}, false, "memory block without a value")
				continue
			}
			rec := Record{Text: value, Category: str(m, "label"), Task: task,
				Agent: importLabel(SourceLetta), Origin: importLabel(SourceLetta)}
			add(rec, true, "")
		}
	}
	addPassages := func(task string, passages []any) {
		for _, p := range passages {
			m, _ := p.(map[string]any)
			text := str(m, "text")
			if text == "" {
				add(Record{}, false, "archival passage without text")
				continue
			}
			rec := Record{Text: text, Category: "archival", Task: task,
				Agent: importLabel(SourceLetta), Origin: importLabel(SourceLetta),
				Stamp: normStamp(str(m, "created_at"))}
			add(rec, true, "")
		}
	}
	if b, ok := root["blocks"].([]any); ok {
		addBlocks("", b)
	}
	for _, k := range []string{"passages", "archival_passages", "archival"} {
		if p, ok := root[k].([]any); ok {
			addPassages("", p)
		}
	}
	if agents, ok := root["agents"].([]any); ok {
		for _, a := range agents {
			am, _ := a.(map[string]any)
			if am == nil {
				continue
			}
			task := ""
			if name := str(am, "name"); name != "" {
				task = "letta agent " + name
			}
			if mem, ok := am["memory"].(map[string]any); ok {
				if b, ok := mem["blocks"].([]any); ok {
					addBlocks(task, b)
				}
			}
			for _, k := range []string{"passages", "archival_passages", "archival"} {
				if p, ok := am[k].([]any); ok {
					addPassages(task, p)
				}
			}
		}
	}
	return res, nil
}

func toAny(in []map[string]any) []any {
	out := make([]any, len(in))
	for i, m := range in {
		out[i] = m
	}
	return out
}

func parseZep(raw []byte) (Result, error) {
	res := Result{Source: SourceZep}
	objs, err := records(raw, "facts", "edges", "results", "items")
	if err != nil {
		return Result{}, err
	}
	for i, m := range objs {
		fact := str(m, "fact")
		if fact == "" {
			res.Skips = append(res.Skips, Skip{Index: i + 1, Reason: "no fact text"})
			continue
		}
		// Validity is provenance, not a lifecycle the store can represent yet:
		// it rides in the task text, and the fact itself is written as stated.
		var prov []string
		if v := normStamp(str(m, "valid_at")); v != "" {
			prov = append(prov, "valid_at "+v)
		}
		if v := normStamp(str(m, "invalid_at")); v != "" {
			prov = append(prov, "invalid_at "+v)
		}
		task := ""
		if len(prov) > 0 {
			task = "zep " + strings.Join(prov, " ")
		}
		rec := Record{Text: fact, Agent: importLabel(SourceZep), Origin: importLabel(SourceZep),
			Task: task, Stamp: normStamp(str(m, "created_at"))}
		res.Items = append(res.Items, Item{Record: rec})
	}
	return res, nil
}

func parseGeneric(raw []byte) (Result, error) {
	res := Result{Source: SourceGeneric}
	objs, err := records(raw)
	if err != nil {
		return Result{}, err
	}
	for i, m := range objs {
		text, ok := textField(m)
		if !ok {
			res.Skips = append(res.Skips, Skip{Index: i + 1, Reason: "no text, memory or fact field"})
			continue
		}
		agent := str(m, "agent")
		if agent == "" {
			agent = importLabel(SourceGeneric)
		}
		rec := Record{Text: text, Agent: agent, Origin: importLabel(SourceGeneric),
			Task: str(m, "task"), Session: str(m, "session"), Category: str(m, "category"),
			Expires: str(m, "expires"), Stamp: normStamp(firstNonEmpty(str(m, "stamp"), str(m, "created_at")))}
		res.Items = append(res.Items, Item{Record: rec})
	}
	return res, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
