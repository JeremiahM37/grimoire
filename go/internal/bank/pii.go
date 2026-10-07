package bank

import (
	"regexp"
	"sort"
	"strings"
)

// PII screening runs on retain, per bank (config pii_screening): off (the
// default), redact (typed placeholders replace the spans) or flag (the text is
// kept and the item's metadata names the kinds found). The detectors are
// rules, so screening works with no model and never sends text anywhere.
// They favour precision: card numbers must pass a Luhn check and IBANs a
// mod-97 check, so a long ID or a date is left alone.

var (
	piiEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	piiIBAN  = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`)
	piiCard  = regexp.MustCompile(`\b\d(?:[ \-]?\d){12,18}\b`)
	piiSSN   = regexp.MustCompile(`\b(\d{3})-(\d{2})-(\d{4})\b`)
	piiPhone = regexp.MustCompile(`(?:\+\d{1,3}[ .\-]?)?(?:\(\d{3}\)[ .\-]?|\b\d{3}[ .\-])\d{3}[ .\-]\d{4}\b|\+\d{1,3}(?:[ .\-]?\d{2,4}){3,4}\b`)
	piiAddr  = regexp.MustCompile(`\b\d{1,5} (?:[A-Z][a-z]+ ){1,3}(?:Street|St|Avenue|Ave|Road|Rd|Boulevard|Blvd|Lane|Ln|Drive|Dr|Court|Ct|Way|Place|Pl)\b\.?`)
)

func luhn(digits string) bool {
	sum, alt, n := 0, false, 0
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if alt {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
		n++
	}
	return n >= 13 && sum%10 == 0
}

func ibanValid(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	s = s[4:] + s[:4]
	rem := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A'+10)) % 97
		default:
			return false
		}
	}
	return rem == 1
}

func ssnPlausible(m string) bool {
	p := piiSSN.FindStringSubmatch(m)
	return p != nil && p[1] != "000" && p[1] != "666" && p[1][0] != '9' && p[2] != "00" && p[3] != "0000"
}

// ScreenPII replaces personal data in text with typed placeholders such as
// [EMAIL] and reports how many of each kind it found.
func ScreenPII(text string) (string, map[string]int) {
	found := map[string]int{}
	rep := func(re *regexp.Regexp, kind string, ok func(string) bool) {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			if ok != nil && !ok(m) {
				return m
			}
			found[kind]++
			return "[" + strings.ToUpper(kind) + "]"
		})
	}
	rep(piiEmail, "email", nil)
	rep(piiIBAN, "iban", ibanValid)
	rep(piiCard, "card", luhn)
	rep(piiSSN, "ssn", ssnPlausible)
	rep(piiPhone, "phone", nil)
	rep(piiAddr, "address", nil)
	return text, found
}

// piiMode is a bank's pii_screening setting.
func (e *Engine) piiMode(bankID string) string {
	p, err := e.Profile(bankID)
	if err != nil {
		return "off"
	}
	return p.Setting("pii_screening", "off")
}

// screenItemsPII applies a bank's PII mode to the items and returns how many
// spans were found.
func screenItemsPII(items []Item, mode string) int {
	if mode != "redact" && mode != "flag" {
		return 0
	}
	total := 0
	for i := range items {
		it := &items[i]
		kinds := map[string]int{}
		for _, f := range []*string{&it.Content, &it.Context} {
			out, found := ScreenPII(*f)
			for k, n := range found {
				kinds[k] += n
				total += n
			}
			if mode == "redact" {
				*f = out
			}
		}
		if mode == "flag" && len(kinds) > 0 {
			names := make([]string, 0, len(kinds))
			for k := range kinds {
				names = append(names, k)
			}
			sort.Strings(names)
			md := make(map[string]string, len(it.Metadata)+1)
			for k, v := range it.Metadata {
				md[k] = v
			}
			md["pii"] = strings.Join(names, ";")
			it.Metadata = md
		}
	}
	return total
}
