package bank

import (
	"strings"
	"testing"
)

func TestScreenPIIDetectors(t *testing.T) {
	in := "Mail jane.doe@example.com, call (415) 555-0134 or +44 20 7946 0958. Card 4111 1111 1111 1111, " +
		"not 4111 1111 1111 1112. SSN 123-45-6789 (not 000-12-3456). IBAN GB82 WEST 1234 5698 7654 32. " +
		"Lives at 221 Baker Street. Ordered 2023-05-24 for 1234567."
	out, found := ScreenPII(in)
	for _, leak := range []string{"jane.doe@", "555-0134", "7946 0958", "4111 1111 1111 1111", "123-45-6789", "WEST 1234", "221 Baker"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q survived: %s", leak, out)
		}
	}
	for _, keep := range []string{"4111 1111 1111 1112", "000-12-3456", "2023-05-24", "1234567"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q was wrongly redacted: %s", keep, out)
		}
	}
	for _, k := range []string{"email", "phone", "card", "ssn", "iban", "address"} {
		if found[k] == 0 {
			t.Errorf("no %s found: %v", k, found)
		}
	}
	if !strings.Contains(out, "[EMAIL]") || !strings.Contains(out, "[CARD]") {
		t.Errorf("placeholders = %s", out)
	}
}

func setPII(t *testing.T, h *harness, mode string) {
	t.Helper()
	if _, err := h.e.Profile("b"); err != nil {
		h.retain(t, "b", Item{Content: "seed.", DocumentID: "seed"})
	}
	if _, err := h.e.UpdateProfile("b", func(p *Profile) error { p.Config["pii_screening"] = mode; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestPIIScreeningModes(t *testing.T) {
	text := "Priya's email is priya@shopify.com and she works at Shopify. <private>hidden words</private>"
	h := newHarness(t, false)
	h.retain(t, "b", Item{Content: text, DocumentID: "off"})
	if !strings.Contains(vaultText(t, h.root), "priya@shopify.com") {
		t.Fatal("off must keep the text")
	}
	h = newHarness(t, false)
	setPII(t, h, "redact")
	res := h.retain(t, "b", Item{Content: text, DocumentID: "d1"})
	all := vaultText(t, h.root)
	if strings.Contains(all, "priya@shopify.com") || strings.Contains(all, "hidden words") || !strings.Contains(all, "[EMAIL]") ||
		!strings.Contains(all, "Shopify") || res.PIIFindings != 1 {
		t.Fatalf("redact: findings=%d\n%s", res.PIIFindings, all)
	}
	h = newHarness(t, false)
	setPII(t, h, "flag")
	h.retain(t, "b", Item{Content: text, DocumentID: "d1"})
	all = vaultText(t, h.root)
	if !strings.Contains(all, "priya@shopify.com") || !strings.Contains(all, "pii") || strings.Contains(all, "hidden words") {
		t.Fatalf("flag keeps text and notes the kind, still strips private:\n%s", all)
	}
	// The queued payload is screened too.
	h = newHarness(t, false)
	setPII(t, h, "redact")
	if _, err := h.e.EnqueueRetain("b", []Item{{Content: "reach me at a.b@c.org", DocumentID: "q"}}, RetainOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(vaultText(t, h.root), "a.b@c.org") {
		t.Fatal("queued payload kept the address")
	}
}

func TestPIIScreeningNeverRewritesAPersonsFact(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = livesIn("Lyon")
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon last year.", DocumentID: "conv"})
	rel := FactsPath("b", "conv")
	h.editFile(t, rel, "Alice lives in Lyon <!--f", "Alice lives in Lyon, reach her at alice@home.fr <!--f")
	setPII(t, h, "redact")
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon last year. Mail alice@home.fr.", DocumentID: "conv"})
	file := h.read(t, rel)
	if !strings.Contains(file, "reach her at alice@home.fr") || !strings.Contains(file, "by=human") {
		t.Fatalf("the person's fact was screened or lost:\n%s", file)
	}
}
