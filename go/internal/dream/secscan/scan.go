// Package secscan is the security pass of a dream. It reads the documents an
// agent will later recall and reports what would be dangerous to recall.
//
// The question here is not "is this text bad" but "what happens when an agent
// believes it". A memory is read back as a fact, as a rule to follow, or as a
// command to run, and a sentence copied from a web page or a Jira comment can
// arrive in any of those roles. So the checks look for four things: credentials
// that should not be sitting in a note that syncs, instruction-shaped text aimed
// at the model rather than at a person, characters a reader cannot see, and
// commands an agent could replay without thinking.
//
// Every check is a pure function over documents, in keeping with the rest of
// the dream package. Nothing here writes, and no finding carries a Fix: whether
// a credential should be removed or an instruction trusted is a judgement for
// the person who owns the vault.
package secscan

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// Check names. Reports group by these, so they are stable.
const (
	checkSecret        = "secret"
	checkSecretValue   = "secret_value"
	checkInjection     = "injection"
	checkHiddenUnicode = "hidden_unicode"
	checkDangerous     = "dangerous_command"
	checkUntrusted     = "untrusted_instruction"
	checkPII           = "pii"
)

// minSecretLen is the shortest known secret that is searched for verbatim.
// Shorter values are ordinary words and numbers, and matching them would flag
// half the vault.
const minSecretLen = 8

// excerptLimit bounds every excerpt, so one long line cannot make a report
// unreadable or carry a whole paragraph of someone else's text.
const excerptLimit = 120

// Scan reports security findings across docs. knownSecrets are credential
// values the caller already holds, typically from the secret store; any of them
// found verbatim in a document is reported, masked. Values that are too short or
// blank are ignored rather than matched.
//
// Findings come back sorted by severity (high first), then path, then line.
func Scan(docs []dream.Doc, knownSecrets []string) []dream.Finding {
	known := usableSecrets(knownSecrets)
	var out []dream.Finding
	for _, d := range docs {
		out = append(out, scanDoc(d, known)...)
	}
	out = collapse(out)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := rank(a.Severity), rank(b.Severity); ra != rb {
			return ra > rb
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Check < b.Check
	})
	return out
}

// usableSecrets drops values too short to be a credential, blank values, and
// duplicates. It returns the rest longest first. The order is load-bearing when
// redacting: a short value that is a substring of a longer one would otherwise
// be masked first and leave fragments of the longer secret visible.
func usableSecrets(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if len(v) < minSecretLen || strings.TrimSpace(v) == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// scanDoc runs every per-document check over one document.
func scanDoc(doc dream.Doc, known []string) []dream.Finding {
	var out []dream.Finding
	lines := strings.Split(doc.Body, "\n")

	// flagged records lines already reported as a credential. Those lines are
	// skipped by the PII check, which would otherwise repeat the same report at
	// lower severity about the same text.
	flagged := map[int]bool{}

	for _, f := range secrets.ScanText(doc.Path, doc.Body) {
		sev := dream.Medium
		if f.Confidence == secrets.ConfidenceHigh {
			sev = dream.High
		}
		flagged[f.Line] = true
		out = append(out, finding(checkSecret, sev, doc.Path, f.Line,
			fmt.Sprintf("credential (%s) pasted into a note; it syncs and is recalled", f.Kind),
			f.Masked))
	}

	for i, line := range lines {
		var masks []string
		for _, v := range known {
			if strings.Contains(line, v) {
				masks = append(masks, secrets.Mask(v))
			}
		}
		if len(masks) == 0 {
			continue
		}
		flagged[i+1] = true
		out = append(out, finding(checkSecretValue, dream.High, doc.Path, i+1,
			"a known secret value appears verbatim in this note",
			strings.Join(masks, ", ")))
	}

	// Only memory notes carry trust information, so only they are parsed for
	// entries. Other documents have no notion of an untrusted line.
	var entries []memory.Entry
	if doc.Kind == dream.KindMemoryNote {
		entries = memory.Parse(doc.Body)
	}
	untrusted := map[int]bool{}
	for _, e := range entries {
		if e.Untrusted() {
			untrusted[e.Line+1] = true
		}
	}

	for i, line := range lines {
		n := i + 1

		if injectionLine(line) {
			sev := dream.Medium
			msg := "instruction-shaped text aimed at a model; an agent may act on it"
			if untrusted[n] {
				sev = dream.High
				msg = "instruction-shaped text from an untrusted source, stored as a memory; an agent would recall it as a fact"
			}
			out = append(out, finding(checkInjection, sev, doc.Path, n, msg, excerpt(line, known)))
		}

		if counts, dangerous := hiddenRunes(line, i == 0); len(counts) > 0 {
			sev := dream.Medium
			if dangerous {
				sev = dream.High
			}
			// The excerpt names the code points rather than quoting the line, so
			// the invisible characters are not reproduced in the report.
			out = append(out, finding(checkHiddenUnicode, sev, doc.Path, n,
				"invisible or direction-changing characters can hide text from a reader while a model still reads it",
				hiddenNames(counts)))
		}

		for _, d := range dangerousCommands {
			if d.re.MatchString(line) {
				out = append(out, finding(checkDangerous, dream.Low, doc.Path, n,
					"records a command that "+d.reason+"; an agent replaying this memory could run it unreviewed",
					excerpt(line, known)))
				break
			}
		}
	}

	for _, e := range entries {
		if !e.Untrusted() {
			continue
		}
		category := strings.ToLower(strings.TrimSpace(e.Category))
		if category != "procedure" && category != "preference" && !imperativeRE.MatchString(e.Text) {
			continue
		}
		out = append(out, finding(checkUntrusted, dream.Medium, doc.Path, e.Line+1,
			"an instruction learned from text other people can write; an agent would follow it as a rule",
			excerpt(e.Text, known)))
	}

	// Personal data is summarised once per document at info severity. A
	// personal memory store is SUPPOSED to hold its owner's email address;
	// a finding per line buries the credential findings that matter under
	// forty that do not. What is worth knowing is which documents carry it.
	piiCount := map[string]int{}
	firstPII := 0
	for i, line := range lines {
		n := i + 1
		if flagged[n] {
			continue
		}
		_, found := bank.ScreenPII(line)
		for k, c := range found {
			piiCount[k] += c
		}
		if len(found) > 0 && firstPII == 0 {
			firstPII = n
		}
	}
	if len(piiCount) > 0 {
		kinds := make([]string, 0, len(piiCount))
		for k, c := range piiCount {
			kinds = append(kinds, fmt.Sprintf("%s ×%d", k, c))
		}
		sort.Strings(kinds)
		// The excerpt is kinds and counts only. Quoting the line would copy
		// the personal data into the report, which is the thing being reported.
		out = append(out, finding(checkPII, dream.Info, doc.Path, firstPII,
			"personal data in this document; it is recalled to agents verbatim",
			strings.Join(kinds, ", ")))
	}

	return out
}

// injectionPatterns are instruction-shaped phrases aimed at a model. They are
// matched case-insensitively. Each one is a phrase no ordinary note needs, so
// the list stays narrow on purpose: a noisy check gets switched off.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget)\s+(?:all\s+|any\s+)?(?:previous|prior|above|earlier)\s+(?:instructions|rules|context)\b`),
	regexp.MustCompile(`(?i)\byou are now\b`),
	regexp.MustCompile(`(?i)\bnew (?:system )?instructions\b`),
	// "system prompt" alone is how people TALK about prompts; only an
	// attempt to reveal or replace one is aimed at a model.
	regexp.MustCompile(`(?i)\b(?:reveal|print|repeat|show|ignore|override|replace)\s+(?:your|the)\s+system prompt\b`),
	// Chat-template tokens. A model reads these as role boundaries, so text
	// carrying them can pose as a message from the system or the operator.
	regexp.MustCompile(`(?i)<\|im_start\|>|<\|system\|>|\[INST\]|<</SYS>>`),
	regexp.MustCompile(`(?i)</?system>`),
	regexp.MustCompile(`(?i)\bdo not (?:tell|inform|mention to) the user\b`),
	regexp.MustCompile(`(?i)\bwithout (?:telling|informing) the user\b`),
	// Exfiltration: an instruction to send something to a URL.
	regexp.MustCompile(`(?i)\b(?:send|post|upload|forward) (?:it|them|this|the [a-z ]+) to https?://`),
	// A markdown image whose URL carries a query string. Rendering it makes the
	// client fetch the URL, which leaks whatever the query string was built from.
	regexp.MustCompile(`!\[[^\]]*\]\(https?://[^)]*\?[^)]*\)`),
}

// injectionLine reports whether a line matches any injection pattern.
func injectionLine(line string) bool {
	for _, re := range injectionPatterns {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// imperativeRE recognises a fact phrased as a rule. Such a line is trusted or
// not according to its origin, but when it is untrusted it is worth a second
// look even if its category does not say it is a procedure.
var imperativeRE = regexp.MustCompile(`(?i)^\s*(?:always|never|must|do not)\b`)

// dangerousCommands are shell fragments an agent might replay from memory. The
// reasons are phrased so the message can complete "records a command that ...".
var dangerousCommands = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)(curl|wget)[^\n|]*\|\s*(sudo\s+)?(ba)?sh\b`), "pipes a download straight into a shell"},
	// Only the root and home directories. "rm -rf build/" is routine, and
	// flagging it would teach people to ignore this check.
	{regexp.MustCompile(`rm\s+-rf\s+(/|~)(\s|$)`), "recursively deletes the root or home directory"},
	{regexp.MustCompile(`chmod\s+(-R\s+)?777`), "makes files world-writable"},
	{regexp.MustCompile(`--no-verify`), "skips git hooks"},
	// A force push is only a danger to a shared branch. The two orderings cover
	// "git push --force origin main" and "git push origin main --force".
	{regexp.MustCompile(`(?i)git\s+push\b[^\n]*(?:--force\b|\s-f\b)[^\n]*\b(?:main|master)\b`), "force-pushes to the default branch"},
	{regexp.MustCompile(`(?i)git\s+push\b[^\n]*\b(?:main|master)\b[^\n]*(?:--force\b|\s-f\b)`), "force-pushes to the default branch"},
}

// DangerousCommand reports whether a command matches one of the patterns the
// sweep flags, and why. Writes use it to refuse a stored check that would be
// dangerous for another agent to run.
func DangerousCommand(s string) (reason string, bad bool) {
	for _, d := range dangerousCommands {
		if d.re.MatchString(s) {
			return d.reason, true
		}
	}
	return "", false
}

// zero-width, bidi-control and tag code points. Each one can hide or reorder
// text in a way a reader cannot see.
func isZeroWidth(r rune) bool {
	return (r >= 0x200B && r <= 0x200F) || (r >= 0x2060 && r <= 0x2064) || r == 0xFEFF
}

func isBidi(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

func isTag(r rune) bool {
	return r >= 0xE0000 && r <= 0xE007F
}

// hiddenRunes counts the invisible code points in a line. dangerous reports
// whether any of them reorders text or is a tag character, which is the case
// that can make a reader see something other than what a model is given.
//
// A byte order mark at the very start of the body is how many editors save
// UTF-8, so it is not treated as hidden text there.
func hiddenRunes(line string, firstLine bool) (counts map[rune]int, dangerous bool) {
	counts = map[rune]int{}
	pos := 0
	for _, r := range line {
		hidden := isZeroWidth(r) || isBidi(r) || isTag(r)
		if r == 0xFEFF && firstLine && pos == 0 {
			hidden = false
		}
		if hidden {
			counts[r]++
			if isBidi(r) || isTag(r) {
				dangerous = true
			}
		}
		pos++
	}
	return counts, dangerous
}

// hiddenNames renders code points as "U+200B ×2", sorted by code point.
func hiddenNames(counts map[rune]int) string {
	runes := make([]rune, 0, len(counts))
	for r := range counts {
		runes = append(runes, r)
	}
	sort.Slice(runes, func(i, j int) bool { return runes[i] < runes[j] })
	parts := make([]string, 0, len(runes))
	for _, r := range runes {
		parts = append(parts, fmt.Sprintf("U+%04X ×%d", r, counts[r]))
	}
	return strings.Join(parts, ", ")
}

// excerpt is a SAFE quote of a line: known secrets are masked, anything that
// looks like a credential is redacted, and the result is bounded. Redaction runs
// before truncation, because cutting first could leave a credential's prefix
// behind as a fragment that no pattern would recognise.
func excerpt(line string, known []string) string {
	s := line
	for _, v := range known {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, secrets.Mask(v))
		}
	}
	s, _ = secrets.RedactText(s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > excerptLimit {
		s = string(r[:excerptLimit])
	}
	return s
}

func finding(check string, sev dream.Severity, path string, line int, msg, excerpt string) dream.Finding {
	return dream.Finding{
		Check:    check,
		Category: dream.Security,
		Severity: sev,
		Path:     path,
		Line:     line,
		Message:  msg,
		Excerpt:  excerpt,
	}
}

// findingKey identifies findings that say the same thing about the same place.
type findingKey struct {
	check string
	path  string
	line  int
}

// collapse merges findings that share a check, path and line into one. A line
// that matches two injection patterns is one problem to fix, not two, so the
// report does not count it twice. Where the pieces differ, as with two different
// known secrets on one line, they are joined rather than dropped, so no
// credential goes unmentioned. The most severe rating wins.
func collapse(fs []dream.Finding) []dream.Finding {
	index := map[findingKey]int{}
	var out []dream.Finding
	for _, f := range fs {
		k := findingKey{f.Check, f.Path, f.Line}
		i, ok := index[k]
		if !ok {
			index[k] = len(out)
			out = append(out, f)
			continue
		}
		m := &out[i]
		if rank(f.Severity) > rank(m.Severity) {
			m.Severity = f.Severity
		}
		m.Message = joinUnique(m.Message, f.Message, "; ")
		m.Excerpt = joinUnique(m.Excerpt, f.Excerpt, ", ")
	}
	return out
}

// joinUnique appends b to a unless a already contains it.
func joinUnique(a, b, sep string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b || strings.Contains(a, b) {
		return a
	}
	return a + sep + b
}

// rank orders severities for sorting and merging. Unknown values rank lowest.
func rank(s dream.Severity) int {
	switch s {
	case dream.High:
		return 3
	case dream.Medium:
		return 2
	case dream.Low:
		return 1
	}
	return 0
}
