package normalize

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
)

// minRun is the shortest base64 or hex run that is decoded. Shorter runs are
// ordinary words and identifiers; decoding them would produce noise that the
// printable check rarely rejects on its own.
const minRun = 16

var (
	base64Run = regexp.MustCompile(`[A-Za-z0-9+/_-]{16,}={0,2}`)
	hexRun    = regexp.MustCompile(`\b(?:0x)?(?:[0-9a-fA-F]{2}){8,}\b`)

	htmlComment = regexp.MustCompile(`(?s)<!--(.*?)-->`)
	// A markdown link title: [text](url "title"), with either quote style.
	linkTitle = regexp.MustCompile(`\]\([^)\s]*\s+(?:"([^"]*)"|'([^']*)')\)`)
	// A reference-style definition: [id]: https://example.com "title".
	refTitle = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]:\s*\S+\s+"([^"]*)"`)
	altText  = regexp.MustCompile(`!\[([^\]]*)\]\(`)
	dataURL  = regexp.MustCompile(`data:([a-zA-Z0-9.+/-]*)((?:;[a-zA-Z0-9.+=_-]+)*),([^\s"'<>)]*)`)
)

// decodeBase64Runs replaces each base64 run that decodes to readable text with
// that text. Standard and URL alphabets, padded and unpadded, are all tried.
func decodeBase64Runs(s string) (string, bool) {
	if len(s) > MaxInput {
		return "", false
	}
	changed := false
	out := base64Run.ReplaceAllStringFunc(s, func(run string) string {
		if len(run) < minRun {
			return run
		}
		if d, ok := decodeB64(run); ok {
			changed = true
			return d
		}
		return run
	})
	return out, changed && out != s
}

func decodeB64(run string) (string, bool) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		b, err := enc.DecodeString(run)
		if err != nil {
			continue
		}
		if len(b) > MaxDecoded {
			return "", false
		}
		if d := string(b); printable(d) {
			return d, true
		}
	}
	return "", false
}

// decodeHexRuns replaces each hex run that decodes to readable text. A commit
// hash or a digest is hex too; it almost never decodes to printable text, and
// when it does the reading is still judged by the same patterns, so the cost
// of a false decode is one extra variant rather than a finding.
func decodeHexRuns(s string) (string, bool) {
	if len(s) > MaxInput {
		return "", false
	}
	changed := false
	out := hexRun.ReplaceAllStringFunc(s, func(run string) string {
		digits := strings.TrimPrefix(strings.ToLower(run), "0x")
		b, err := hex.DecodeString(digits)
		if err != nil || len(b) > MaxDecoded || !printable(string(b)) {
			return run
		}
		changed = true
		return string(b)
	})
	return out, changed && out != s
}

// revealDataURLs returns the payload of each data: URL that decodes to text,
// base64 or percent-encoded. A rendered data URL is fetched by the client, so
// the payload is content the reader is shown as much as any text is.
func revealDataURLs(s string) (string, bool) {
	var parts []string
	for _, m := range dataURL.FindAllStringSubmatch(s, -1) {
		params, payload := m[2], m[3]
		var d string
		if strings.Contains(params, ";base64") {
			b, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				b, err = base64.RawStdEncoding.DecodeString(payload)
			}
			if err != nil || len(b) > MaxDecoded {
				continue
			}
			d = string(b)
		} else {
			u, err := url.PathUnescape(payload)
			if err != nil {
				continue
			}
			d = u
		}
		if printable(d) {
			parts = append(parts, d)
		}
	}
	return joinIfAny(parts)
}

// revealHTMLComments returns the bodies of HTML comments. Markdown renderers
// hide these, so a reader of the rendered page never sees the text a model is
// given. Comments split across lines are not joined here; see the doc.
func revealHTMLComments(s string) (string, bool) {
	var parts []string
	for _, m := range htmlComment.FindAllStringSubmatch(s, -1) {
		parts = append(parts, m[1])
	}
	return joinIfAny(parts)
}

// revealLinkTitles returns the titles of markdown links. A title is shown only
// on hover, so it is text a reader is unlikely to read and a model will.
func revealLinkTitles(s string) (string, bool) {
	var parts []string
	for _, m := range linkTitle.FindAllStringSubmatch(s, -1) {
		if m[1] != "" {
			parts = append(parts, m[1])
		} else {
			parts = append(parts, m[2])
		}
	}
	for _, m := range refTitle.FindAllStringSubmatch(s, -1) {
		parts = append(parts, m[1])
	}
	return joinIfAny(parts)
}

// revealAltText returns markdown image alt text. It is the one place a renderer
// shows text to a screen reader and not to a sighted reader's eye.
func revealAltText(s string) (string, bool) {
	var parts []string
	for _, m := range altText.FindAllStringSubmatch(s, -1) {
		parts = append(parts, m[1])
	}
	return joinIfAny(parts)
}

func joinIfAny(parts []string) (string, bool) {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		return "", false
	}
	return strings.Join(keep, " "), true
}
