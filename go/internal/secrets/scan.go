package secrets

import (
	"bufio"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Finding credentials that were pasted into notes instead of stored.
//
// A dedicated secrets manager scans git history, because that is where its
// users leak. Here the substrate is a markdown vault somebody types into — and
// in a normal deployment, syncs to a phone. A key pasted into a note during
// debugging is the likeliest way a credential escapes this system, and the
// index already reads every note, so Grimoire is the only thing positioned to
// notice.
//
// Two rules shape everything below.
//
// A scanner that cries wolf gets turned off, and then it is worse than nothing
// because its silence means "not run" rather than "clean". So the patterns are
// the ones with an issuer-defined shape — a prefix and a length a provider
// controls — and the generic "looks like an api key" case has to clear an
// entropy bar before it counts.
//
// And a report about a leaked secret must not contain the secret. Writing the
// value into a scan result puts it in a terminal, a log, an HTTP response and
// probably a ticket — reproducing the leak in more places, on purpose. Findings
// carry a masked fragment, enough to locate the line and nothing more.

// Finding is one suspected credential.
type Finding struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	// Kind names the issuer where the shape identifies one.
	Kind string `json:"kind"`
	// Masked shows the first and last few characters only, so a person can
	// tell which key it is without the report becoming a copy of it.
	Masked string `json:"masked"`
	// Confidence is high for issuer-shaped matches, medium for entropy.
	Confidence string `json:"confidence"`
	// Advice is what to do, since a finding nobody can act on is noise.
	Advice string `json:"advice"`
}

// Confidence levels.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
)

// pattern is one credential shape.
type pattern struct {
	kind string
	re   *regexp.Regexp
	// group is the submatch holding the credential; 0 means the whole match.
	group int
}

// patterns are issuer-defined shapes.
//
// Each is anchored on a prefix the issuer chose and a length it fixes, which
// is what makes these worth alerting on: a string matching one of these is
// almost never anything else.
var patterns = []pattern{
	{kind: "AWS access key", re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{kind: "GitHub token", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{kind: "GitHub fine-grained token", re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{50,}\b`)},
	{kind: "Slack token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{kind: "Slack webhook", re: regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/+]{20,}`)},
	{kind: "Stripe key", re: regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{20,}\b`)},
	// Anthropic before OpenAI: "sk-ant-…" also satisfies the OpenAI shape, and
	// RE2 has no lookahead to exclude it. First match wins per value, so the
	// more specific rule has to be tried first or every Anthropic key is
	// reported as somebody else's.
	{kind: "Anthropic key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{32,}\b`)},
	{kind: "OpenAI key", re: regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{32,}\b`)},
	{kind: "Google API key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{kind: "SendGrid key", re: regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\b`)},
	{kind: "Twilio SID", re: regexp.MustCompile(`\bAC[0-9a-fA-F]{32}\b`)},
	{kind: "npm token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{kind: "PyPI token", re: regexp.MustCompile(`\bpypi-[A-Za-z0-9_-]{50,}\b`)},
	{kind: "private key", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----`)},
	{kind: "JSON Web Token", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{kind: "Mullvad account", re: regexp.MustCompile(`\bmullvad[_ -]?account[^\n]{0,20}?\b(\d{16})\b`), group: 1},
}

// assignment matches a secret-shaped variable being set, for the generic case
// no issuer prefix covers.
//
// This one is deliberately gated behind an entropy test: the pattern alone
// matches `api_key = "TODO"` and `password: changeme`, and a scanner that
// reported those would be ignored within a week.
var assignment = regexp.MustCompile(
	`(?i)\b(api[_-]?key|secret|token|password|passwd|access[_-]?key|auth)\b\s*[:=]\s*["']?([A-Za-z0-9_\-./+=]{16,})["']?`)

// placeholders are the values people actually write when they mean "not a real
// one". Reporting these is the fastest way to teach somebody to ignore a tool.
var placeholders = map[string]bool{
	"changeme": true, "your_api_key_here": true, "todo": true, "xxx": true,
	"placeholder": true, "example": true, "redacted": true, "none": true,
	"null": true, "undefined": true, "test": true, "secret": true,
	"password": true, "hunter2": true, "notarealkey": true,
}

// minEntropy is the Shannon bits-per-character a generic value must clear.
//
// 3.2 sits above English prose and repeated characters, and below essentially
// every random key. Tuned to miss real secrets rather than to catch prose: a
// scanner people trust is one whose findings are all worth reading.
const minEntropy = 3.2

// ScanText reports suspected credentials in one document.
//
// path is used only to label findings; the text is never stored or logged.
func ScanText(path, text string) []Finding {
	var out []Finding
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Text()
		// Grimoire's own sealed bodies are ciphertext, which is high-entropy by
		// definition. Reporting the vault's own encryption as a leak would be
		// both wrong and very loud.
		if strings.Contains(raw, EncPrefix) {
			continue
		}
		for _, p := range patterns {
			for _, m := range p.re.FindAllStringSubmatch(raw, -1) {
				val := m[0]
				if p.group > 0 && p.group < len(m) {
					val = m[p.group]
				}
				key := fmt.Sprintf("%d:%s", line, val)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, Finding{
					Path: path, Line: line, Kind: p.kind, Masked: Mask(val),
					Confidence: ConfidenceHigh,
					Advice: "Store it with `grimoire secret add`, rotate it at the issuer, " +
						"and remove it from this note.",
				})
			}
		}
		for _, m := range assignment.FindAllStringSubmatch(raw, -1) {
			val := m[2]
			if placeholders[strings.ToLower(val)] || !looksRandom(val) {
				continue
			}
			key := fmt.Sprintf("%d:%s", line, val)
			if seen[key] {
				continue // an issuer pattern already reported this exact value
			}
			seen[key] = true
			out = append(out, Finding{
				Path: path, Line: line, Kind: "possible " + strings.ToLower(m[1]),
				Masked: Mask(val), Confidence: ConfidenceMedium,
				Advice: "High-entropy value assigned to a credential-shaped name. " +
					"If it is real, store it and rotate it.",
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// Mask reduces a credential to something identifiable but not usable.
//
// Short values are replaced entirely: showing four of twelve characters is a
// meaningful fraction of a weak secret.
func Mask(s string) string {
	const keep = 4
	if len(s) < 12 {
		return strings.Repeat("•", len(s))
	}
	return s[:keep] + strings.Repeat("•", 6) + s[len(s)-keep:]
}

// looksRandom reports whether a string has the character distribution of a
// generated credential rather than of a word.
func looksRandom(s string) bool {
	if len(s) < 16 {
		return false
	}
	// A value that is one repeated run ("aaaaaaaa...", "--------") has low
	// entropy anyway, but checking cheaply first keeps prose out of the log.
	if entropy(s) < minEntropy {
		return false
	}
	// Real keys mix classes. A long lowercase-only string is far more often a
	// sentence fragment, a path, or a hash of something public.
	var upper, lower, digit, other int
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= 'a' && r <= 'z':
			lower++
		case r >= '0' && r <= '9':
			digit++
		default:
			other++
		}
	}
	classes := 0
	for _, n := range []int{upper, lower, digit, other} {
		if n > 0 {
			classes++
		}
	}
	return classes >= 2
}

// entropy is Shannon entropy in bits per character.
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	total := float64(len([]rune(s)))
	var h float64
	for _, n := range counts {
		p := float64(n) / total
		h -= p * math.Log2(p)
	}
	return h
}
