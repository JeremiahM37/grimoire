package bank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// A session digest is the "where we left off" note for one coding-agent
// session: what was asked, what was learned, what was done and what comes
// next. It is a markdown file in the bank, banks/<bank>/sessions/<id>.md.
//
// The generated text sits between two marker comments and nowhere else. A
// person may write above or below the markers and it is never touched. If the
// person edits inside the markers, the digest is theirs from then on: a
// fingerprint of the generated text is kept in the frontmatter, and a mismatch
// pins the note, so regeneration leaves it alone.
const (
	digestStart = "<!-- grimoire:generated digest -->"
	digestEnd   = "<!-- /grimoire:generated -->"

	sessionsDir = "sessions/"
	// digestInputChars bounds the transcript handed to a model.
	digestInputChars = 14000
	digestMaxTurns   = 80
)

// SessionTurn is one spoken turn of a session.
type SessionTurn struct {
	Speaker   string `json:"speaker"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

// CommandRun is one command the agent ran.
type CommandRun struct {
	Command string `json:"command"`
	Exit    *int   `json:"exit,omitempty"`
}

// SessionActivity is what a session did to the machine, extracted by rules.
type SessionActivity struct {
	Files    []string     `json:"files,omitempty"`
	Commands []CommandRun `json:"commands,omitempty"`
}

// DigestInput is what a digest is written from.
type DigestInput struct {
	SessionID string          `json:"session_id"`
	Turns     []SessionTurn   `json:"turns"`
	Activity  SessionActivity `json:"activity"`
	// UseModel asks for a model-written digest when one is configured.
	// Without it, or without a model, the rule-based digest is written.
	UseModel bool `json:"use_model"`
}

// Digest is the four parts of a digest.
type Digest struct {
	Request string   `json:"request"`
	Learned []string `json:"learned"`
	Done    []string `json:"done"`
	Next    []string `json:"next"`
}

// DigestResult reports what writing a digest did.
type DigestResult struct {
	Path   string `json:"path"`
	Method string `json:"method"` // rules | model
	// Pinned is true when a person's edit to the generated text was kept.
	Pinned  bool   `json:"pinned,omitempty"`
	Written bool   `json:"written"`
	Skipped string `json:"skipped,omitempty"`
}

// DigestPath is where a session's digest lives.
func DigestPath(bankID, session string) string {
	return Prefix(bankID) + sessionsDir + fileOf(session) + ".md"
}

func shortSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

var cueLearned = regexp.MustCompile(`(?i)\b(because|root cause|turns out|the cause|the problem|found that|learned|discovered|the reason|note that|gotcha|caveat|instead of|is keyed|must|never|always)\b`)
var cueNext = regexp.MustCompile(`(?i)\b(next|todo|to-do|remaining|still need|follow[- ]?up|not yet|left to|should also|later|pending)\b`)
var cueDone = regexp.MustCompile(`(?i)\b(fixed|added|implemented|updated|created|removed|renamed|refactored|committed|merged|passes|passing|done|completed|wrote|changed)\b`)
var headingDown = regexp.MustCompile(`(?m)^## `)
var sentenceSplit = regexp.MustCompile(`(?s).+?[.!?](?:\s+|$)|.+$`)

func clip(s string, n int) string {
	s = oneLineText(s)
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

func sentences(text string) []string {
	var out []string
	for _, m := range sentenceSplit.FindAllString(text, -1) {
		if s := strings.TrimSpace(m); len([]rune(s)) >= 12 {
			out = append(out, s)
		}
	}
	return out
}

// ruleDigest writes a digest with no model: the first thing the user asked,
// assistant sentences that carry a cue, and the files and commands the session
// touched.
func ruleDigest(in DigestInput) Digest {
	var d Digest
	var lastUser string
	for _, t := range in.Turns {
		if t.Speaker == "user" {
			if d.Request == "" {
				d.Request = clip(t.Text, 400)
			}
			lastUser = t.Text
		}
	}
	seen := map[string]bool{}
	add := func(list *[]string, s string, max int) {
		s = clip(s, 240)
		if s == "" || seen[strings.ToLower(s)] || len(*list) >= max {
			return
		}
		seen[strings.ToLower(s)] = true
		*list = append(*list, s)
	}
	var assistant []string
	for _, t := range in.Turns {
		if t.Speaker == "assistant" {
			assistant = append(assistant, t.Text)
		}
	}
	for _, text := range assistant {
		for _, s := range sentences(text) {
			if cueLearned.MatchString(s) && !cueNext.MatchString(s) {
				add(&d.Learned, s, 5)
			}
		}
	}
	if len(in.Activity.Files) > 0 {
		files := in.Activity.Files
		if len(files) > 12 {
			files = append(append([]string{}, files[:12]...), fmt.Sprintf("and %d more files", len(in.Activity.Files)-12))
		}
		add(&d.Done, "Touched "+strings.Join(files, ", "), 8)
	}
	failed := 0
	for _, c := range in.Activity.Commands {
		if c.Exit != nil && *c.Exit != 0 {
			failed++
			add(&d.Done, fmt.Sprintf("Command failed (exit %d): %s", *c.Exit, clip(c.Command, 120)), 8)
		}
	}
	if n := len(in.Activity.Commands); n > 0 {
		add(&d.Done, fmt.Sprintf("Ran %d commands, %d failed", n, failed), 8)
	}
	if len(assistant) > 0 {
		for _, s := range sentences(assistant[len(assistant)-1]) {
			if cueDone.MatchString(s) {
				add(&d.Done, s, 8)
			}
		}
		if len(d.Done) == 0 {
			if ss := sentences(assistant[len(assistant)-1]); len(ss) > 0 {
				add(&d.Done, ss[0], 8)
			}
		}
	}
	for i := len(assistant) - 1; i >= 0 && i >= len(assistant)-3; i-- {
		for _, s := range sentences(assistant[i]) {
			if cueNext.MatchString(s) {
				add(&d.Next, s, 4)
			}
		}
	}
	if len(d.Next) == 0 && lastUser != "" && len(in.Turns) > 0 && in.Turns[len(in.Turns)-1].Speaker == "user" {
		add(&d.Next, "Unanswered last request: "+lastUser, 4)
	}
	return d
}

func digestPrompt(in DigestInput) (system, user string) {
	system = "You write a short hand-off note for a coding-agent session so the next session can pick up where " +
		"this one left off. Reply with JSON only: {\"request\": string, \"learned\": [string], \"done\": [string], " +
		"\"next\": [string]}. request is what the user wanted, in one sentence. learned holds decisions, causes and " +
		"gotchas worth remembering (at most 5). done holds what was changed or finished (at most 6). next holds what " +
		"is unfinished or should happen next (at most 4). Each item is one short sentence. Use only what the " +
		"transcript says; leave a list empty rather than guess."
	var b strings.Builder
	turns := in.Turns
	if len(turns) > digestMaxTurns {
		turns = turns[len(turns)-digestMaxTurns:]
	}
	for _, t := range turns {
		fmt.Fprintf(&b, "%s: %s\n", t.Speaker, clip(t.Text, 1200))
	}
	text := b.String()
	if r := []rune(text); len(r) > digestInputChars {
		text = string(r[len(r)-digestInputChars:])
	}
	user = "Session transcript (newest last):\n" + text
	if len(in.Activity.Files) > 0 {
		user += "\nFiles touched: " + strings.Join(in.Activity.Files, ", ") + "\n"
	}
	for i, c := range in.Activity.Commands {
		if i >= 30 {
			break
		}
		exit := ""
		if c.Exit != nil {
			exit = fmt.Sprintf(" (exit %d)", *c.Exit)
		}
		user += "Command: " + clip(c.Command, 160) + exit + "\n"
	}
	return system, user
}

func cleanList(xs []string, max int) []string {
	var out []string
	for _, x := range xs {
		if x = clip(x, 300); x != "" && len(out) < max {
			out = append(out, x)
		}
	}
	return out
}

func (e *Engine) modelDigest(ctx context.Context, in DigestInput) (Digest, error) {
	system, user := digestPrompt(in)
	client := e.AI.WithSurface("bank.digest", "")
	comp, err := client.CompleteWith(ctx, user, ai.CompleteOpts{System: system, Temperature: ai.Temp(0.2), MaxTokens: 700, JSON: true})
	if err != nil {
		return Digest{}, err
	}
	var d Digest
	if err := json.Unmarshal([]byte(extractJSONObject(comp.Text)), &d); err != nil {
		return Digest{}, err
	}
	d.Request = clip(d.Request, 400)
	d.Learned, d.Done, d.Next = cleanList(d.Learned, 5), cleanList(d.Done, 6), cleanList(d.Next, 4)
	if d.Request == "" && len(d.Learned)+len(d.Done)+len(d.Next) == 0 {
		return Digest{}, fmt.Errorf("empty digest")
	}
	return d, nil
}

func extractJSONObject(s string) string {
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// renderRegion is the generated text, markers excluded.
func (d Digest) renderRegion() string {
	var b strings.Builder
	b.WriteString("## Request\n\n" + orNone(d.Request) + "\n")
	for _, sec := range []struct {
		title string
		items []string
	}{{"Learned", d.Learned}, {"Done", d.Done}, {"Next", d.Next}} {
		b.WriteString("\n## " + sec.title + "\n\n")
		if len(sec.items) == 0 {
			b.WriteString("- (nothing recorded)\n")
		}
		for _, it := range sec.items {
			b.WriteString("- " + it + "\n")
		}
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(not recorded)"
	}
	return s
}

// splitRegion separates a digest note body into what precedes the generated
// region, the region, and what follows it. ok is false when the markers are
// missing.
func splitRegion(body string) (before, region, after string, ok bool) {
	i := strings.Index(body, digestStart)
	j := strings.Index(body, digestEnd)
	if i < 0 || j < i {
		return "", "", "", false
	}
	return body[:i], strings.Trim(body[i+len(digestStart):j], "\n"), body[j+len(digestEnd):], true
}

func sanitizeTurns(turns []SessionTurn) []SessionTurn {
	out := make([]SessionTurn, 0, len(turns))
	for _, t := range turns {
		t.Text, _ = StripPrivate(t.Text)
		t.Text, _ = secrets.RedactText(t.Text)
		if strings.TrimSpace(t.Text) != "" {
			out = append(out, t)
		}
	}
	return out
}

func sanitizeActivity(a SessionActivity) SessionActivity {
	var out SessionActivity
	for _, f := range a.Files {
		f, _ = secrets.RedactText(f)
		if f = strings.TrimSpace(f); f != "" && len(out.Files) < 200 {
			out.Files = append(out.Files, clip(f, 200))
		}
	}
	for _, c := range a.Commands {
		cmd, _ := StripPrivate(c.Command)
		cmd, _ = secrets.RedactText(cmd)
		if cmd = strings.TrimSpace(cmd); cmd != "" && len(out.Commands) < 200 {
			out.Commands = append(out.Commands, CommandRun{Command: clip(cmd, 300), Exit: c.Exit})
		}
	}
	return out
}

// WriteDigest writes or refreshes a session's digest note. It never fails for
// want of a model: any model error falls back to the rule digest.
func (e *Engine) WriteDigest(ctx context.Context, bankID string, in DigestInput) (*DigestResult, error) {
	if !ValidID(bankID) {
		return nil, invalid("invalid bank id")
	}
	in.SessionID = strings.TrimSpace(in.SessionID)
	if in.SessionID == "" || len(in.SessionID) > 200 {
		return nil, invalid("session id must be 1..200 characters")
	}
	in.Turns, in.Activity = sanitizeTurns(in.Turns), sanitizeActivity(in.Activity)
	rel := DigestPath(bankID, in.SessionID)
	res := &DigestResult{Path: rel, Method: "rules"}
	if len(in.Turns) == 0 && len(in.Activity.Files)+len(in.Activity.Commands) == 0 {
		res.Skipped = "nothing to summarize"
		return res, nil
	}
	if _, err := e.Profile(bankID); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if err := e.writeProfile(NewProfile(bankID), false); err != nil {
			return nil, err
		}
	}
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()

	inputHash := shortSum(fmt.Sprint(in.Turns, in.Activity))
	var existing *markdown.Frontmatter
	var before, after, oldRegion string
	haveRegion := false
	if n, err := e.Vault.Read(rel); err == nil {
		existing = n.Frontmatter
		before, oldRegion, after, haveRegion = splitRegion(n.Body)
		if !haveRegion {
			res.Pinned, res.Skipped = true, "a person removed the generated markers"
			return res, nil
		}
		if h := existing.StringVal("generated_hash"); h != "" && h != shortSum(oldRegion) {
			res.Pinned, res.Skipped = true, "a person edited the generated text"
			return res, nil
		}
		if existing.StringVal("input_hash") == inputHash && (existing.StringVal("method") == "model" || !in.UseModel) {
			res.Method, res.Skipped = existing.StringVal("method"), "unchanged"
			return res, nil
		}
	}

	var d Digest
	if in.UseModel && e.AI.Available() {
		if md, err := e.modelDigest(ctx, in); err == nil {
			d, res.Method = md, "model"
		}
	}
	if res.Method != "model" {
		d = ruleDigest(in)
	}
	region := d.renderRegion()

	started, ended := "", ""
	for _, t := range in.Turns {
		if t.Timestamp != "" {
			if started == "" {
				started = t.Timestamp
			}
			ended = t.Timestamp
		}
	}
	now := e.Now().UTC().Format(time.RFC3339)
	if ended == "" {
		ended = now
	}
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Where we left off: "+clip(in.SessionID, 40))
	fm.Set("type", "session-digest")
	fm.Set("session_id", in.SessionID)
	fm.Set("bank", bankID)
	if started != "" {
		fm.Set("started", started)
	}
	fm.Set("ended", ended)
	fm.Set("method", res.Method)
	fm.Set("input_hash", inputHash)
	fm.Set("generated_hash", shortSum(strings.Trim(region, "\n")))
	if existing != nil {
		fm = mergeFM(existing, fm)
	}
	generated := digestStart + "\n" + region + digestEnd
	body := before + generated + after
	if !haveRegion {
		body = "# Where we left off\n\n" + generated + "\n\n## Notes\n\nYour own notes go here; they are never overwritten.\n"
	}
	if _, err := e.Vault.Write(rel, body, fm); err != nil {
		return nil, err
	}
	res.Written = true
	return res, nil
}

// mergeFM lays our keys over a person's frontmatter, keeping any key they added.
func mergeFM(existing, ours *markdown.Frontmatter) *markdown.Frontmatter {
	merged := existing.Clone()
	for _, k := range ours.Keys() {
		v, _ := ours.Get(k)
		merged.Set(k, v)
	}
	return merged
}

// DigestSummary is one stored digest.
type DigestSummary struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	Ended     string `json:"ended"`
	Method    string `json:"method"`
	Body      string `json:"-"`
}

// ListDigests returns a bank's session digests, newest first.
func (e *Engine) ListDigests(bankID string, limit int) ([]DigestSummary, error) {
	rels, err := e.Vault.WalkDir(strings.TrimSuffix(Prefix(bankID), "/") + "/" + strings.TrimSuffix(sessionsDir, "/"))
	if err != nil {
		return nil, err
	}
	var out []DigestSummary
	for _, rel := range rels {
		n, err := e.Vault.Read(rel)
		if err != nil || n.Frontmatter.StringVal("type") != "session-digest" {
			continue
		}
		out = append(out, DigestSummary{SessionID: n.Frontmatter.StringVal("session_id"), Path: rel,
			Ended: n.Frontmatter.StringVal("ended"), Method: n.Frontmatter.StringVal("method"), Body: n.Body})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ended > out[j].Ended })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// latestDigestItem is the most recent session digest as a context item, or nil.
// A person's words around or inside the generated text come along with it.
func (e *Engine) latestDigestItem(bankID string) *ContextItem {
	ds, err := e.ListDigests(bankID, 1)
	if err != nil || len(ds) == 0 {
		return nil
	}
	d := ds[0]
	_, region, after, ok := splitRegion(d.Body)
	if !ok {
		region = d.Body
	}
	text := strings.TrimSpace(region)
	if extra := strings.TrimSpace(strings.ReplaceAll(after, "Your own notes go here; they are never overwritten.", "")); extra != "" && extra != "## Notes" {
		text += "\n\n" + extra
	}
	when := d.Ended
	if len(when) >= 10 {
		when = when[:10]
	}
	return &ContextItem{Kind: KindDigest, ID: d.SessionID,
		Text: "Last session (" + when + "):\n" + headingDown.ReplaceAllString(text, "### ")}
}
