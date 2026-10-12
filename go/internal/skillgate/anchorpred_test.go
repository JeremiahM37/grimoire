package skillgate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
)

func fileAnchor(path, text string) anchor.Anchor {
	return anchor.Anchor{Type: anchor.FileContains, Args: map[string]string{"path": path, "text": text}}
}

func TestAnchorPredicateRoundTrips(t *testing.T) {
	a := fileAnchor("/opt/x/config;yes.yml", "port: 9111 | a=b; c%d")
	p := AnchorPredicate(a)
	if strings.Contains(p, ";") {
		t.Fatalf("predicate still holds a list separator: %q", p)
	}
	kind, arg, _ := strings.Cut(p, ":")
	if kind != "anchor" {
		t.Fatalf("kind %q", kind)
	}
	got, err := ParseAnchorPredicate(arg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("round trip changed the anchor:\n in  %#v\n out %#v", a, got)
	}
}

func TestAnchorPredicateSurvivesTheSpecLists(t *testing.T) {
	s := Spec{Trigger: "deploy the thing", Steps: []string{"make", "ship"},
		Preconditions:  []string{AnchorPredicate(fileAnchor("/opt/x/a.txt", "one")), "command:make"},
		Postconditions: []string{AnchorPredicate(anchor.Anchor{Type: anchor.UnitActive, Args: map[string]string{"unit": "x.service"}})}}
	if _, err := Lines(s, nil); err != nil {
		t.Fatalf("Lines refused an anchor predicate: %v", err)
	}
	draft := "---\nname: Deploy\nmetadata:\n  kind: procedure\n---\n\n1. make\n2. ship\n"
	stamped, err := Stamp(draft, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := memstore.ParseNote("deploy.md", stamped, time.Now())
	got, ok := SpecOf(n)
	if !ok {
		t.Fatalf("spec not read back:\n%s", stamped)
	}
	if !reflect.DeepEqual(got.Preconditions, s.Preconditions) || !reflect.DeepEqual(got.Postconditions, s.Postconditions) {
		t.Fatalf("predicates changed in the note:\npre  %q\npost %q", got.Preconditions, got.Postconditions)
	}
}

func TestAnchorPreconditionIsRunAndReported(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	pass := AnchorPredicate(fileAnchor(marker, "ready"))
	fail := AnchorPredicate(fileAnchor(marker, "missing text"))
	s := Spec{Preconditions: []string{pass, fail}}
	ctx := NewContext(dir, nil)
	ok, unmet := Applicable(s, ctx)
	if ok || !reflect.DeepEqual(unmet, []string{fail}) {
		t.Fatalf("applicable %v unmet %v", ok, unmet)
	}
	// Without a runner nothing can be vouched for.
	ctx.Run = nil
	if ok, _ := Applicable(Spec{Preconditions: []string{pass}}, ctx); ok {
		t.Fatal("an anchor with no runner must not hold")
	}
}

func TestSkippedAnchorDoesNotHold(t *testing.T) {
	ctx := Context{Run: func(anchor.Anchor) anchor.Result {
		return anchor.Result{Status: anchor.Skipped, Message: anchor.SkipRemote}
	}}
	if ok, _ := Applicable(Spec{Preconditions: []string{AnchorPredicate(fileAnchor("/x", "y"))}}, ctx); ok {
		t.Fatal("a skipped probe is not a verdict and must not make a skill apply")
	}
}

func TestSlotAnchorIsNeverRun(t *testing.T) {
	calls := 0
	ctx := Context{Run: func(anchor.Anchor) anchor.Result { calls++; return anchor.Result{Status: anchor.Pass} }}
	slot := AnchorPredicate(anchor.Anchor{Type: anchor.BinaryVersion, Args: map[string]string{"bin": "grimoire", "text": "<version>"}})
	if ok, _ := Applicable(Spec{Preconditions: []string{slot}}, ctx); ok || calls != 0 {
		t.Fatalf("slot predicate ran (calls %d) or held", calls)
	}
	s := Spec{Postconditions: []string{slot, "ran:go test", AnchorPredicate(fileAnchor("/x", "y"))}}
	if got := NeedsValues(s); !reflect.DeepEqual(got, []string{slot}) {
		t.Fatalf("needs values %v", got)
	}
}

func TestExistingPredicatesUnchanged(t *testing.T) {
	ctx := Context{Cwd: "/w", Exists: func(p string) bool { return p == "/w/run.sh" },
		Command: func(n string) bool { return n == "go" }, Run: func(anchor.Anchor) anchor.Result {
			t.Fatal("a non-anchor predicate must not run an anchor")
			return anchor.Result{}
		}}
	s := Spec{Preconditions: []string{"file:run.sh", "command:go", "fact:Kestrel"}}
	ctx.Facts = []string{"project kestrel lives here"}
	if ok, unmet := Applicable(s, ctx); !ok {
		t.Fatalf("unmet %v", unmet)
	}
	if got := NeedsValues(Spec{Postconditions: []string{"ran:go"}}); len(got) != 0 {
		t.Fatalf("ran: flagged as needing values: %v", got)
	}
}
