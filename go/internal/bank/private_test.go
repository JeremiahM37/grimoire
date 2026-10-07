package bank

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripPrivate(t *testing.T) {
	cases := map[string]string{
		"keep <private>hide</private> this":          "keep  this",
		"a <PRIVATE note=x>multi\nline</Private> b":  "a  b",
		"before <private>never closed, hide all":     "before ",
		"no tags here":                               "no tags here",
		"<private>a</private>x<private>b</private>y": "xy",
	}
	for in, want := range cases {
		if got, _ := StripPrivate(in); got != want {
			t.Errorf("StripPrivate(%q) = %q, want %q", in, got, want)
		}
	}
}

// vaultText is every byte under the vault root, including the cache.
func vaultText(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			raw, _ := os.ReadFile(p)
			b.Write(raw)
		}
		return nil
	})
	return b.String()
}

func TestPrivateAndSecretTextNeverReachTheVault(t *testing.T) {
	h := newHarness(t, false)
	key := "ghp_" + strings.Repeat("aB3dE5gH7j", 4)
	h.retain(t, "b", Item{
		Content:     "Priya works at Shopify. <private>Her salary is 123456 dollars.</private> She uses " + key + " daily.",
		DocumentID:  "d1",
		ScanSecrets: true,
	})
	all := vaultText(t, h.root)
	for _, leak := range []string{"123456 dollars", key} {
		if strings.Contains(all, leak) {
			t.Fatalf("%q reached the vault", leak)
		}
	}
	if !strings.Contains(all, "Shopify") {
		t.Fatal("the public text was lost")
	}
}

func TestOnlyPrivateContentStoresNothing(t *testing.T) {
	h := newHarness(t, false)
	res, err := h.e.Retain(context.Background(), "b", []Item{{Content: "<private>all of it</private>"}}, RetainOptions{})
	if err != nil || res.ItemsCount != 0 || len(res.Documents) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := h.e.EnqueueRetain("b", []Item{{Content: "<private>x</private>"}}, RetainOptions{}); err == nil {
		t.Fatal("a queued retain of only private text must be refused, not stored")
	}
}

func TestQueuedPayloadHoldsNoPrivateText(t *testing.T) {
	h := newHarness(t, false)
	id, err := h.e.EnqueueRetain("b", []Item{{Content: "visible fact. <private>hidden words</private>", DocumentID: "q"}}, RetainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = id
	if strings.Contains(vaultText(t, h.root), "hidden words") {
		t.Fatal("private text was queued")
	}
}
