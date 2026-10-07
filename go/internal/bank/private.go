package bank

import (
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// A person marks text they do not want remembered by wrapping it in
// <private>...</private>. The span is removed before anything is extracted,
// queued, written to the vault or sent to a model, so it never exists in a
// file, an index or a webhook. An opening tag with no closing one hides the
// rest of the content: failing closed is the safe reading of a typo.
var (
	privateSpan = regexp.MustCompile(`(?is)<private\b[^>]*>.*?</private\s*>`)
	privateOpen = regexp.MustCompile(`(?is)<private\b[^>]*>.*$`)
)

// StripPrivate removes private spans from text and reports whether any existed.
func StripPrivate(text string) (string, bool) {
	if !strings.Contains(strings.ToLower(text), "<private") {
		return text, false
	}
	out := privateSpan.ReplaceAllString(text, "")
	out = privateOpen.ReplaceAllString(out, "")
	return out, out != text
}

// sanitizeItems applies the privacy rules to a retain request: private spans
// are stripped from every free-text field, secrets are redacted from items
// that ask for it, and an item with nothing left is dropped.
func sanitizeItems(items []Item) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		it.Content, _ = StripPrivate(it.Content)
		it.Context, _ = StripPrivate(it.Context)
		if it.ScanSecrets {
			it.Content, _ = secrets.RedactText(it.Content)
			it.Context, _ = secrets.RedactText(it.Context)
		}
		if strings.TrimSpace(it.Content) == "" {
			continue
		}
		out = append(out, it)
	}
	return out
}
