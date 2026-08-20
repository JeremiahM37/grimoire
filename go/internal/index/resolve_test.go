package index

import (
	"fmt"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Link resolution has to stay CORRECT while getting cheaper — the targeted path
// must reach every link the whole-vault pass would have.

func linkState(t *testing.T, ix *Index, src string) (dst string, resolved int) {
	t.Helper()
	var d any
	if err := ix.DB.QueryRow("SELECT dst, resolved FROM links WHERE src=?", src).
		Scan(&d, &resolved); err != nil {
		t.Fatalf("no link row for %s: %v", src, err)
	}
	if s, ok := d.(string); ok {
		dst = s
	}
	return dst, resolved
}

func TestALinkResolvesWhenItsTargetAppearsLater(t *testing.T) {
	ix := testIndex(t)
	// The link is written before the note it points at exists.
	write(t, ix, "source.md", "# Source\n\nsee [[Target Note]]\n")
	if _, err := ix.Upsert("source.md"); err != nil {
		t.Fatal(err)
	}
	if dst, resolved := linkState(t, ix, "source.md"); resolved != 0 || dst != "" {
		t.Fatalf("a link to a missing note resolved to %q (%d)", dst, resolved)
	}

	write(t, ix, "target-note.md", "# Target Note\n\nhere now\n")
	if _, err := ix.Upsert("target-note.md"); err != nil {
		t.Fatal(err)
	}
	dst, resolved := linkState(t, ix, "source.md")
	if resolved != 1 || dst != "target-note.md" {
		t.Fatalf("link did not resolve when its target appeared: dst=%q resolved=%d", dst, resolved)
	}
}

func TestResolutionFollowsTitleAliasStemAndPath(t *testing.T) {
	ix := testIndex(t)
	// Frontmatter goes through the frontmatter API: a --- block typed into the
	// body is body text, which is a thing worth knowing when writing fixtures.
	fm := markdown.NewFrontmatter()
	fm.Set("title", "United States")
	fm.Set("aliases", []markdown.Value{"USA", "America"})
	if _, err := ix.Vault.Write("places/united-states.md", "# United States\n", fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert("places/united-states.md"); err != nil {
		t.Fatal(err)
	}
	for i, target := range []string{
		"United States",        // title
		"USA",                  // alias
		"united-states",        // filename stem
		"places/united-states", // folder-qualified path
	} {
		src := fmt.Sprintf("src%d.md", i)
		write(t, ix, src, "# S\n\nlinks to [["+target+"]]\n")
		if _, err := ix.Upsert(src); err != nil {
			t.Fatal(err)
		}
		dst, resolved := linkState(t, ix, src)
		if resolved != 1 || dst != "places/united-states.md" {
			t.Errorf("[[%s]] resolved to %q (%d)", target, dst, resolved)
		}
	}
}

// A deleted note must stop resolving, and a retitled one must stop answering to
// the name it used to have — the cached lookup maps make both easy to get wrong.
func TestDeletingAndRetitlingUpdateResolution(t *testing.T) {
	ix := testIndex(t)
	write(t, ix, "target.md", "# Old Title\n\nbody\n")
	write(t, ix, "source.md", "# Source\n\nsee [[Old Title]]\n")
	for _, p := range []string{"target.md", "source.md"} {
		if _, err := ix.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, resolved := linkState(t, ix, "source.md"); resolved != 1 {
		t.Fatal("the link did not resolve to begin with")
	}

	// Retitled: the old name must stop resolving.
	write(t, ix, "target.md", "# Brand New Title\n\nbody\n")
	if _, err := ix.Upsert("target.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert("source.md"); err != nil { // re-resolve the source's links
		t.Fatal(err)
	}
	if dst, resolved := linkState(t, ix, "source.md"); resolved == 1 {
		t.Errorf("a link to the OLD title still resolves to %q", dst)
	}

	// Deleted: links pointing at it go dangling.
	write(t, ix, "source2.md", "# S2\n\nsee [[Brand New Title]]\n")
	if _, err := ix.Upsert("source2.md"); err != nil {
		t.Fatal(err)
	}
	if _, resolved := linkState(t, ix, "source2.md"); resolved != 1 {
		t.Fatal("the link to the new title did not resolve")
	}
	if err := ix.Remove("target.md"); err != nil {
		t.Fatal(err)
	}
	if dst, resolved := linkState(t, ix, "source2.md"); resolved != 0 || dst != "" {
		t.Errorf("a link to a DELETED note still resolves to %q (%d)", dst, resolved)
	}
}

// The targeted path and the whole-vault pass must agree — otherwise writes and
// rebuilds would produce different graphs.
func TestTargetedResolutionAgreesWithAFullPass(t *testing.T) {
	ix := testIndex(t)
	for i := 0; i < 12; i++ {
		write(t, ix, fmt.Sprintf("n%02d.md", i),
			fmt.Sprintf("# Note %d\n\nlinks to [[Note %d]] and [[Nothing %d]]\n", i, (i+1)%12, i))
		if _, err := ix.Upsert(fmt.Sprintf("n%02d.md", i)); err != nil {
			t.Fatal(err)
		}
	}
	incremental := map[string]string{}
	rows, err := ix.DB.Query("SELECT src, target, COALESCE(dst,''), resolved FROM links ORDER BY src, target")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var src, target, dst string
		var resolved int
		if err := rows.Scan(&src, &target, &dst, &resolved); err != nil {
			t.Fatal(err)
		}
		incremental[src+"|"+target] = fmt.Sprintf("%s|%d", dst, resolved)
	}
	rows.Close()

	if _, err := ix.Reindex(); err != nil { // the whole-vault pass
		t.Fatal(err)
	}
	rows, err = ix.DB.Query("SELECT src, target, COALESCE(dst,''), resolved FROM links ORDER BY src, target")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	full := map[string]string{}
	for rows.Next() {
		var src, target, dst string
		var resolved int
		if err := rows.Scan(&src, &target, &dst, &resolved); err != nil {
			t.Fatal(err)
		}
		full[src+"|"+target] = fmt.Sprintf("%s|%d", dst, resolved)
	}
	if len(incremental) != len(full) {
		t.Fatalf("%d links after incremental writes, %d after a rebuild", len(incremental), len(full))
	}
	for k, v := range full {
		if incremental[k] != v {
			t.Errorf("%s: incremental %q, rebuild %q", k, incremental[k], v)
		}
	}
}
