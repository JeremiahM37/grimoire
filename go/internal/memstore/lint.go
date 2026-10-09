package memstore

import (
	"regexp"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/dream/secscan"
)

// Warning is one gentle complaint about a memory. It never blocks a write.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var (
	bulletRE   = regexp.MustCompile(`(?m)^\s*(?:[-*]|\d+[.)])\s+\S`)
	whyRE      = regexp.MustCompile(`(?i)\b(?:why|because|so that|since|the reason|otherwise|after|incident|broke|burned|caused)\b`)
	howRE      = regexp.MustCompile(`(?i)\b(?:how to apply|apply (?:this )?when|when |whenever|before |instead of|if you|in (?:any|every) )`)
	commitRE   = regexp.MustCompile(`(?i)\b(?:commit|merged in|fixed in|introduced in|git log|sha)\s+[0-9a-f]{7,40}\b`)
	srcPathRE  = regexp.MustCompile(`\b[\w.-]+(?:/[\w.-]+)*\.(?:go|py|ts|tsx|js|rs|java|c|h|cpp|rb)\b`)
	repoSayRE  = regexp.MustCompile(`(?i)\b(?:the (?:repo|codebase|code|source) (?:has|contains|uses|is organi[sz]ed)|(?:is|are) (?:defined|implemented|located) in)\b`)
	sentenceRE = regexp.MustCompile(`[.!?](?:\s|$)`)
)

// Lint checks one memory against the writing guidelines (docs/MEMORY_WRITING.md).
// kind may be "" when unknown; it is inferred from text then.
func Lint(kind, text string) []Warning {
	text = strings.TrimSpace(text)
	if kind = Normalize(kind); kind == "" {
		kind = InferText(text)
	}
	var out []Warning
	add := func(code, msg string) { out = append(out, Warning{code, msg}) }

	if kind != KindProcedure {
		bullets := len(bulletRE.FindAllString(text, -1))
		sentences := len(sentenceRE.FindAllString(text, -1))
		if bullets >= 3 || (len(text) > 700 && sentences >= 6) {
			add("multiple_facts", "this looks like several facts; remember one fact per memory so each can be recalled, corrected and expired on its own")
		}
	}
	if kind == KindRule {
		if !whyRE.MatchString(text) {
			add("rule_missing_why", "a rule should say Why (the incident or reason), so an agent can judge edge cases")
		}
		if !howRE.MatchString(text) {
			add("rule_missing_how", "a rule should say How to apply (when it applies, what to do instead)")
		}
	}
	if secretLike(text) {
		add("looks_like_secret", "this looks like it contains a credential; store the secret in the credential vault and write where to find it instead")
	}
	if commitRE.MatchString(text) || (len(srcPathRE.FindAllString(text, -1)) >= 2 && repoSayRE.MatchString(text)) {
		add("repo_info", "this looks like something the repo or git history already records (commit ids, code layout); memory should hold what the repo cannot say")
	}
	return out
}

func secretLike(text string) bool {
	doc := dream.Doc{Path: "memory/lint.md", Body: text, Kind: dream.KindMemoryNote, Mtime: time.Now()}
	for _, f := range secscan.Scan([]dream.Doc{doc}, nil) {
		if f.Check == "secret" || f.Check == "secret_value" {
			return true
		}
	}
	return false
}
