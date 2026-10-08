package bank

import (
	"testing"
	"time"
)

// ref is Wednesday 24 May 2023, mid-afternoon.
var ref = time.Date(2023, 5, 24, 15, 30, 0, 0, time.UTC)

func TestParseWindow(t *testing.T) {
	cases := []struct {
		q          string
		start, end string // "" means no window expected
	}{
		// absolute
		{"what happened on 2023-05-07?", "2023-05-07", "2023-05-07"},
		{"meeting at 2023-05-07T14:00", "2023-05-07", "2023-05-07"},
		{"in 2022-11 we moved", "2022-11-01", "2022-11-30"},
		{"on 5/7/2023", "2023-05-07", "2023-05-07"},
		{"on 25/12/2022", "2022-12-25", "2022-12-25"},
		{"May 7, 2023", "2023-05-07", "2023-05-07"},
		{"on May 7th 2023", "2023-05-07", "2023-05-07"},
		{"the 7th of May, 2023", "2023-05-07", "2023-05-07"},
		{"13 July 2022", "2022-07-13", "2022-07-13"},
		{"what did I do on March 3rd", "2023-03-03", "2023-03-03"},
		{"plans for June 2nd", "2022-06-02", "2022-06-02"}, // yearless and in the future → last year
		{"in May 2023", "2023-05-01", "2023-05-31"},
		{"during February of 2020", "2020-02-01", "2020-02-29"},
		{"Sept 2021", "2021-09-01", "2021-09-30"},
		{"what did we do in 2019", "2019-01-01", "2019-12-31"},
		{"since 2021", "2021-01-01", "2023-05-24"},
		{"in March", "2023-03-01", "2023-03-31"},
		{"during August", "2022-08-01", "2022-08-31"},
		{"in december", "2022-12-01", "2022-12-31"},
		{"last March", "2023-03-01", "2023-03-31"},
		{"last June", "2022-06-01", "2022-06-30"},
		{"next July", "2023-07-01", "2023-07-31"},
		{"this June", "2023-06-01", "2023-06-30"},
		// parts of periods
		{"early May 2023", "2023-05-01", "2023-05-10"},
		{"late June", "2022-06-21", "2022-06-30"},
		{"at the end of 2022", "2022-08-31", "2022-12-31"},
		{"in mid-2021", "2021-05-02", "2021-08-30"},
		// seasons
		{"last summer", "2022-06-01", "2022-08-31"},
		{"this summer", "2023-06-01", "2023-08-31"},
		{"summer 2019", "2019-06-01", "2019-08-31"},
		{"the winter of 2021", "2021-12-01", "2022-02-28"},
		{"last winter", "2022-12-01", "2023-02-28"},
		{"in the spring", "2023-03-01", "2023-05-31"}, // current season, bare
		{"last fall", "2022-09-01", "2022-11-30"},
		{"in the fall", "2022-09-01", "2022-11-30"},
		{"next spring", "2024-03-01", "2024-05-31"},
		{"early summer 2020", "2020-06-01", "2020-06-30"},
		// quarters and decades
		{"Q3 2022", "2022-07-01", "2022-09-30"},
		{"the second quarter of 2021", "2021-04-01", "2021-06-30"},
		{"last quarter", "2023-01-01", "2023-03-31"},
		{"back in the 1990s", "1990-01-01", "1999-12-31"},
		{"the 80s", "1980-01-01", "1989-12-31"},
		// relative days
		{"yesterday", "2023-05-23", "2023-05-23"},
		{"what did I eat today", "2023-05-24", "2023-05-24"},
		{"tomorrow", "2023-05-25", "2023-05-25"},
		{"the day before yesterday", "2023-05-22", "2023-05-22"},
		{"last night", "2023-05-23", "2023-05-23"},
		{"3 days ago", "2023-05-21", "2023-05-21"},
		{"two weeks ago", "2023-05-07", "2023-05-13"},
		{"a month ago", "2023-04-01", "2023-04-30"},
		{"a year ago", "2022-01-01", "2022-12-31"},
		{"a couple of days ago", "2023-05-21", "2023-05-23"},
		{"a few weeks ago", "2023-04-19", "2023-05-10"},
		{"several months ago", "2022-12-25", "2023-03-25"},
		{"in the last 3 days", "2023-05-21", "2023-05-24"},
		{"over the past two weeks", "2023-05-10", "2023-05-24"},
		{"the past few months", "2023-01-24", "2023-05-24"},
		// periods
		{"last week", "2023-05-15", "2023-05-21"},
		{"this week", "2023-05-22", "2023-05-28"},
		{"next week", "2023-05-29", "2023-06-04"},
		{"the past week", "2023-05-17", "2023-05-24"},
		{"last month", "2023-04-01", "2023-04-30"},
		{"this month", "2023-05-01", "2023-05-31"},
		{"last year", "2022-01-01", "2022-12-31"},
		{"this year", "2023-01-01", "2023-12-31"},
		{"earlier this year", "2023-01-01", "2023-05-24"},
		{"later this month", "2023-05-24", "2023-05-31"},
		{"last weekend", "2023-05-20", "2023-05-21"},
		{"this weekend", "2023-05-27", "2023-05-28"},
		// weekdays
		{"last Tuesday", "2023-05-23", "2023-05-23"},
		{"last Wednesday", "2023-05-17", "2023-05-17"},
		{"on Friday", "2023-05-19", "2023-05-19"},
		{"next Monday", "2023-05-29", "2023-05-29"},
		{"this Friday", "2023-05-26", "2023-05-26"},
		// ranges
		{"from May to July 2022", "2022-05-01", "2022-07-31"},
		{"between March 3 and March 10", "2023-03-03", "2023-03-10"},
		{"from 2019 to 2021", "2019-01-01", "2021-12-31"},
		{"between 2023-01-05 and 2023-02-01", "2023-01-05", "2023-02-01"},
		// not dates
		{"what port does the server use", "", ""},
		{"error 8080 on port 2019", "", ""},
		{"in 8080", "", ""},
		{"I may go", "", ""},
		{"we watched the sun set", "", ""},
		{"don't fall over", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		w, ok := ParseWindow(c.q, ref)
		if c.start == "" {
			if ok {
				t.Errorf("%q: got window %s..%s, want none", c.q, w.Start.Format("2006-01-02"), w.End.Format("2006-01-02"))
			}
			continue
		}
		if !ok {
			t.Errorf("%q: no window, want %s..%s", c.q, c.start, c.end)
			continue
		}
		gs, ge := w.Start.Format("2006-01-02"), w.End.Format("2006-01-02")
		if gs != c.start || ge != c.end {
			t.Errorf("%q: got %s..%s, want %s..%s", c.q, gs, ge, c.start, c.end)
		}
	}
}

// "last weekend" asked on a Sunday is the weekend before, not the one in
// progress.
func TestLastWeekendOnASunday(t *testing.T) {
	sun := time.Date(2023, 5, 28, 12, 0, 0, 0, time.UTC)
	w, ok := ParseWindow("last weekend", sun)
	if !ok || w.Start.Format("2006-01-02") != "2023-05-20" {
		t.Errorf("got %v %v", w, ok)
	}
}

func TestWindowEndsAtEndOfDay(t *testing.T) {
	w, _ := ParseWindow("yesterday", ref)
	if w.End.Hour() != 23 || w.End.Minute() != 59 {
		t.Errorf("end = %v", w.End)
	}
	if !w.Contains(time.Date(2023, 5, 23, 22, 0, 0, 0, time.UTC)) {
		t.Error("an evening event yesterday is in yesterday")
	}
}

func TestLooseDatesWidenCoarseValues(t *testing.T) {
	for in, want := range map[string][2]string{
		"2023-05":              {"2023-05-01", "2023-05-31"},
		"2015":                 {"2015-01-01", "2015-12-31"},
		"2023-05-07":           {"2023-05-07", "2023-05-07"},
		"2023-05-07T10:00:00Z": {"2023-05-07", "2023-05-07"},
		"last week":            {"2023-05-15", "2023-05-21"},
	} {
		w, ok := parseLooseDate(in, ref)
		if !ok || w.Start.Format("2006-01-02") != want[0] || w.End.Format("2006-01-02") != want[1] {
			t.Errorf("%q: got %v..%v %v", in, w.Start, w.End, ok)
		}
	}
	for _, in := range []string{"", "N/A", "null", "soon-ish"} {
		if _, ok := parseLooseDate(in, ref); ok {
			t.Errorf("%q should not parse", in)
		}
	}
}
