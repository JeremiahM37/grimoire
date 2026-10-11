package skillgate

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/skillmine"
)

func candidate() skillmine.Candidate {
	return skillmine.Candidate{ID: "abcd1234", Units: pattern, Sessions: 4,
		Examples: []string{"go test ./...", "git push origin main"},
		Title:    "Release kestrel: build and push", Description: "Use when releasing kestrel.",
		Projects: []string{"kestrel"}}
}

func TestSpecRoundTripsThroughTheNoteFormat(t *testing.T) {
	c := candidate()
	c.Gate = &skillmine.Gate{Pass: true, Trigger: "release kestrel", Sources: []string{"claude-code/src-0", "claude-code/src-1"}}
	spec := FromCandidate(c)
	draft := skillmine.Draft(c)
	stamped, err := Stamp(draft, spec, c.Gate)
	if err != nil {
		t.Fatal(err)
	}
	n := memstore.ParseNote(skillmine.DraftFile(c), stamped, time.Now())
	got, ok := SpecOf(n)
	if !ok {
		t.Fatalf("spec not read back:\n%s", stamped)
	}
	if got.Trigger != "release kestrel" {
		t.Errorf("trigger %q", got.Trigger)
	}
	if !reflect.DeepEqual(got.Preconditions, []string{"command:go", "command:git"}) {
		t.Errorf("pre %v", got.Preconditions)
	}
	if !reflect.DeepEqual(got.Postconditions, []string{"ran:go test", "ran:git push"}) {
		t.Errorf("post %v", got.Postconditions)
	}
	if !reflect.DeepEqual(got.Sessions, []string{"claude-code/src-0", "claude-code/src-1"}) {
		t.Errorf("provenance %v", got.Sessions)
	}
	if !reflect.DeepEqual(got.Steps, []string{"`go test ./...`", "`git push origin main`"}) {
		t.Errorf("steps %q", got.Steps)
	}
	if n.Fields["metadata.skill_gate"] != "pass" {
		t.Errorf("gate state %q", n.Fields["metadata.skill_gate"])
	}
	// The existing reader still sees an ordinary procedure with its own name.
	if n.Kind != "procedure" || n.Title != c.Title {
		t.Errorf("kind %q title %q", n.Kind, n.Title)
	}
}

func TestNoteWithoutSpecLoadsAsBefore(t *testing.T) {
	raw := "---\nname: Rotate logs\ndescription: how to rotate logs\nmetadata:\n  kind: procedure\n---\n\n1. stop it\n2. rotate\n"
	n := memstore.ParseNote("p.md", raw, time.Now())
	if _, ok := SpecOf(n); ok {
		t.Error("a note with no skill_ keys has no spec")
	}
	if n.Kind != "procedure" || n.Title != "Rotate logs" {
		t.Errorf("kind %q title %q", n.Kind, n.Title)
	}
}

func TestOverrideIsRecordedWithItsReason(t *testing.T) {
	c := candidate()
	c.Gate = &skillmine.Gate{Pass: false, Reasons: []string{"support"}, Override: "reviewed by hand, the sessions are a real release"}
	stamped, err := Stamp(skillmine.Draft(c), FromCandidate(c), c.Gate)
	if err != nil {
		t.Fatal(err)
	}
	n := memstore.ParseNote("p.md", stamped, time.Now())
	if n.Fields["metadata.skill_gate"] != "override" || n.Fields["metadata.skill_gate_override"] != "reviewed by hand, the sessions are a real release" {
		t.Errorf("override not recorded: %v", n.Fields)
	}
}

func TestSeparatorsThatWouldCorruptTheListAreRefused(t *testing.T) {
	bad := Spec{Trigger: "x", Preconditions: []string{"file:a;b"}}
	if _, err := Lines(bad, nil); err == nil {
		t.Error("a ';' inside a predicate would split it on read-back; must refuse")
	}
	if _, err := Lines(Spec{Trigger: "x\ny"}, nil); err == nil {
		t.Error("a multi-line trigger cannot live in frontmatter; must refuse")
	}
}

func TestStampRejectsDraftWithoutFrontmatter(t *testing.T) {
	if _, err := Stamp("# just a heading\n", Spec{Trigger: "x"}, nil); err == nil {
		t.Error("expected an error")
	}
	if _, err := Stamp("---\nname: x\n", Spec{Trigger: "x"}, nil); err == nil || !strings.Contains(err.Error(), "frontmatter") {
		t.Errorf("unclosed frontmatter: %v", err)
	}
}

func TestFromCandidateScriptsAreFilePreconditions(t *testing.T) {
	c := skillmine.Candidate{Units: []string{"deploy.sh", "docker compose"}}
	s := FromCandidate(c)
	want := []string{"file:deploy.sh", "command:docker"}
	if !reflect.DeepEqual(s.Preconditions, want) {
		t.Errorf("pre %v, want %v", s.Preconditions, want)
	}
}
