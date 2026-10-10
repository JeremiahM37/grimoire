package grounded

import (
	"context"
	"strings"
	"testing"
	"time"
)

func d(y int, m time.Month, dd int) time.Time { return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC) }

func TestAnnotateRelative(t *testing.T) {
	tue := d(2023, 5, 23) // Tuesday
	cases := []struct{ in, want string }{
		{"I went hiking last Saturday.", "last Saturday [= Sat 20 May 2023]"},
		{"We met yesterday.", "yesterday [= Mon 22 May 2023]"},
		{"See you tomorrow", "tomorrow [= Wed 24 May 2023]"},
		{"two weeks ago I started", "two weeks ago [≈ Tue 9 May 2023]"},
		{"3 days ago it rained", "3 days ago [= Sat 20 May 2023]"},
		{"a couple of days ago", "a couple of days ago [≈ Sun 21 May 2023]"},
		{"last week was busy", "last week [= Mon 15 – Sun 21 May 2023]"},
		{"last month we moved", "last month [= April 2023]"},
		{"last year I graduated", "last year [= 2022]"},
		{"next Friday is the party", "next Friday [= Fri 26 May 2023]"},
		{"this Friday is the party", "this Friday [= Fri 26 May 2023]"},
		{"last weekend was fun", "last weekend [= Sat 20 – Sun 21 May 2023]"},
		{"two weekends ago", "two weekends ago [= Sat 13 – Sun 14 May 2023]"},
		{"last summer we travelled", "last summer [= Wed 1 Jun 2022 – Wed 31 Aug 2022]"},
		{"in 3 months I will", "in 3 months [≈ August 2023]"},
		{"5 years ago", "5 years ago [≈ 2018]"},
	}
	for _, c := range cases {
		got := AnnotateText(c.in, tue)
		if !strings.Contains(got, c.want) {
			t.Errorf("%q -> %q, want contain %q", c.in, got, c.want)
		}
	}
}

func TestOriginalTextPreserved(t *testing.T) {
	in := "Last Saturday and yesterday were great, two weeks ago too."
	anns := Annotate(in, d(2023, 5, 23))
	out := Render(in, anns)
	// removing every bracketed annotation gives back the original
	for _, a := range anns {
		out = strings.Replace(out, " ["+a.Text+"]", "", 1)
	}
	if out != in {
		t.Fatalf("original not recoverable: %q", out)
	}
	for _, a := range anns {
		if in[a.Start:a.End] != a.Phrase {
			t.Errorf("offsets wrong for %q", a.Phrase)
		}
	}
}

// The weekend of a Monday reference is the one just gone, not the one before.
func TestLastWeekendFromMonday(t *testing.T) {
	got := AnnotateText("last weekend", d(2023, 7, 17))
	if !strings.Contains(got, "Sat 15 – Sun 16 Jul 2023") {
		t.Fatal(got)
	}
	// from a Sunday the weekend in progress is not "last weekend"
	got = AnnotateText("last weekend", d(2023, 7, 16))
	if !strings.Contains(got, "Sat 8 – Sun 9 Jul 2023") {
		t.Fatal(got)
	}
}

func TestLastWeekdayNeverToday(t *testing.T) {
	got := AnnotateText("last Tuesday", d(2023, 5, 23))
	if !strings.Contains(got, "Tue 16 May 2023") {
		t.Fatal(got)
	}
}

func TestBareWeekdayUsesTense(t *testing.T) {
	ref := d(2023, 5, 23)
	if got := AnnotateText("We went to the lake on Saturday.", ref); !strings.Contains(got, "Sat 20 May 2023") {
		t.Fatal(got)
	}
	if got := AnnotateText("We are going to the lake on Saturday.", ref); !strings.Contains(got, "Sat 27 May 2023") {
		t.Fatal(got)
	}
	// no tense cue at all: left alone without a resolver
	if got := AnnotateText("Lake on Saturday", ref); strings.Contains(got, "[") {
		t.Fatal(got)
	}
}

type fixedResolver struct{ d time.Time }

func (f fixedResolver) Resolve(_ context.Context, _, _ string, _ time.Time) (time.Time, time.Time, bool) {
	return f.d, f.d, true
}

func TestResolverOnlyForAmbiguous(t *testing.T) {
	anns, asked := AnnotateWith(context.Background(), "Lake on Saturday", d(2023, 5, 23), fixedResolver{d(2023, 5, 27)})
	if asked != 1 || len(anns) != 1 || !anns[0].Model {
		t.Fatalf("%v %d", anns, asked)
	}
	_, asked = AnnotateWith(context.Background(), "We went on Saturday, last Friday too", d(2023, 5, 23), fixedResolver{d(2023, 5, 27)})
	if asked != 0 {
		t.Fatal("model asked for a clear case")
	}
}

func TestNoRefNoAnnotation(t *testing.T) {
	if got := Annotate("yesterday", time.Time{}); len(got) != 0 {
		t.Fatal(got)
	}
}
