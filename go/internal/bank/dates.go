package bank

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Window is an inclusive time range a query or a fact refers to.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Days is the window's span in days.
func (w Window) Days() float64 { return w.End.Sub(w.Start).Hours() / 24 }

// Contains reports whether t falls inside the window.
func (w Window) Contains(t time.Time) bool { return !t.Before(w.Start) && !t.After(w.End) }

// Overlaps reports whether [a, b] intersects the window.
func (w Window) Overlaps(a, b time.Time) bool { return !a.After(w.End) && !b.Before(w.Start) }

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func endOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, time.UTC)
}

func dayWindow(t time.Time) Window {
	t = day(t.Year(), t.Month(), t.Day())
	return Window{t, endOfDay(t)}
}

func spanDays(a, b time.Time) Window {
	a, b = day(a.Year(), a.Month(), a.Day()), day(b.Year(), b.Month(), b.Day())
	if b.Before(a) {
		a, b = b, a
	}
	return Window{a, endOfDay(b)}
}

func monthWindow(y int, m time.Month) Window {
	start := day(y, m, 1)
	return Window{start, endOfDay(start.AddDate(0, 1, -1))}
}

func yearWindow(y int) Window { return Window{day(y, 1, 1), endOfDay(day(y, 12, 31))} }

func weekdayIndex(t time.Time) int { return (int(t.Weekday()) + 6) % 7 } // Monday = 0

var monthNames = map[string]time.Month{
	"january": 1, "jan": 1, "february": 2, "feb": 2, "march": 3, "mar": 3, "april": 4, "apr": 4,
	"may": 5, "june": 6, "jun": 6, "july": 7, "jul": 7, "august": 8, "aug": 8,
	"september": 9, "sept": 9, "sep": 9, "october": 10, "oct": 10, "november": 11, "nov": 11,
	"december": 12, "dec": 12,
}

var weekdayNames = map[string]int{
	"monday": 0, "mon": 0, "tuesday": 1, "tue": 1, "tues": 1, "wednesday": 2, "wed": 2,
	"thursday": 3, "thu": 3, "thur": 3, "thurs": 3, "friday": 4, "fri": 4,
	"saturday": 5, "sat": 5, "sunday": 6, "sun": 6,
}

var numberWords = map[string]int{
	"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
	"fifteen": 15, "twenty": 20, "thirty": 30,
}

const (
	monthRE   = `(january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sept|sep|oct|nov|dec)`
	weekdayRE = `(monday|tuesday|wednesday|thursday|friday|saturday|sunday|tues|thurs|thur|mon|tue|wed|thu|fri|sat|sun)`
	ordRE     = `(\d{1,2})(?:st|nd|rd|th)?`
	yearRE    = `(\d{4})`
	countRE   = `(\d{1,3}|a|an|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|fifteen|twenty|thirty)`
	seasonRE  = `(spring|summer|autumn|fall|winter)`
)

// a candidate expression found in the text.
type dateMatch struct {
	start, end int
	w          Window
	// rebuild re-resolves a yearless expression in a given year, so a range
	// like "from May to July 2023" puts May in 2023 rather than in whichever
	// year "May" alone resolved to.
	rebuild func(year int) Window
	score   int
}

type dateRule struct {
	re *regexp.Regexp
	// lead, when set, is the submatch where the expression itself begins: a
	// leading preposition helps decide that a number is a year, but it is
	// not part of the date — "from 2019 to 2021" needs "from" and "to" left
	// outside both expressions to be read as a range.
	lead int
	// fn turns submatches into a window; ok=false rejects the match.
	fn func(m []string, ref time.Time) (w Window, rebuild func(int) Window, ok bool)
}

func rx(s string) *regexp.Regexp { return regexp.MustCompile(`\b` + s + `\b`) }

func countOf(s string) int {
	if n, ok := numberWords[s]; ok {
		return n
	}
	n, _ := strconv.Atoi(s)
	return n
}

func plausibleYear(y int, ref time.Time) bool {
	return y >= max(1, ref.Year()-120) && y <= ref.Year()+20
}

// pastMonth resolves a yearless month to its most recent occurrence that has
// started by ref — "in March" asked in October means this March, asked in
// January means last March. Memory questions are about the past.
func pastMonth(m time.Month, ref time.Time) Window {
	y := ref.Year()
	if day(y, m, 1).After(ref) {
		y--
	}
	return monthWindow(y, m)
}

func pastDay(m time.Month, d int, ref time.Time) (Window, bool) {
	y := ref.Year()
	t := day(y, m, d)
	if t.Month() != m {
		return Window{}, false // Feb 30
	}
	if t.After(ref) {
		t = day(y-1, m, d)
	}
	return dayWindow(t), true
}

// seasonWindow is a meteorological season in the northern hemisphere: the
// convention most text assumes and the only one a query gives no way to
// override. Winter belongs to the year it starts in.
func seasonWindow(name string, year int) Window {
	switch name {
	case "spring":
		return Window{day(year, 3, 1), endOfDay(day(year, 5, 31))}
	case "summer":
		return Window{day(year, 6, 1), endOfDay(day(year, 8, 31))}
	case "autumn", "fall":
		return Window{day(year, 9, 1), endOfDay(day(year, 11, 30))}
	}
	return Window{day(year, 12, 1), endOfDay(day(year+1, 3, 1).AddDate(0, 0, -1))}
}

// currentSeasonYear is the season-year of the season named, for "this".
func currentSeasonYear(name string, ref time.Time) int {
	if (name == "winter") && ref.Month() <= 2 {
		return ref.Year() - 1
	}
	return ref.Year()
}

// partOf narrows a window to its early, middle or late third, in whole days.
func partOf(part string, w Window) Window {
	// Whole calendar days, counted on dates: an end at 23:59:59.999999999
	// is a float's rounding away from the next midnight.
	n := int(day(w.End.Year(), w.End.Month(), w.End.Day()).Sub(w.Start).Hours()/24+0.5) + 1
	third := max(n/3, 1)
	at := func(k int) time.Time { return w.Start.AddDate(0, 0, k) }
	switch part {
	case "early", "beginning of", "start of", "the beginning of", "the start of", "earlier in":
		return Window{w.Start, endOfDay(at(third - 1))}
	case "mid", "mid-", "middle of", "the middle of":
		return Window{at(third), endOfDay(at(2*third - 1))}
	case "late", "end of", "the end of", "later in":
		return Window{at(2 * third), w.End}
	}
	return w
}

const partRE = `(early|mid|middle of|the middle of|late|beginning of|the beginning of|start of|the start of|end of|the end of)`

var dateRules = []dateRule{
	// ISO date and datetime.
	{re: rx(`(\d{4})-(\d{1,2})-(\d{1,2})(?:[t ]\d{1,2}:\d{2}(?::\d{2})?)?`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if mo < 1 || mo > 12 || d < 1 || d > 31 || !plausibleYear(y, ref) {
			return Window{}, nil, false
		}
		return dayWindow(day(y, time.Month(mo), d)), nil, true
	}},
	// ISO month, not the start of a full date.
	{re: regexp.MustCompile(`\b(\d{4})-(\d{2})\b(?:[^-\d]|$)`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		if mo < 1 || mo > 12 || !plausibleYear(y, ref) {
			return Window{}, nil, false
		}
		return monthWindow(y, time.Month(mo)), nil, true
	}},
	// Numeric dates: month/day/year, falling back to day/month/year when the
	// first number cannot be a month.
	{re: rx(`(\d{1,2})[/.](\d{1,2})[/.](\d{2,4})`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		if y < 100 {
			y += 2000
			if y > ref.Year()+20 {
				y -= 100
			}
		}
		mo, d := a, b
		if mo > 12 {
			mo, d = b, a
		}
		if mo < 1 || mo > 12 || d < 1 || d > 31 || !plausibleYear(y, ref) {
			return Window{}, nil, false
		}
		t := day(y, time.Month(mo), d)
		if t.Month() != time.Month(mo) {
			return Window{}, nil, false
		}
		return dayWindow(t), nil, true
	}},
	// "May 7th, 2023", "May 7 2023"
	{re: rx(monthRE + `\.? ` + ordRE + `,? ` + yearRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		return dayOf(monthNames[m[1]], m[2], m[3], ref)
	}},
	// "7 May 2023", "7th of May, 2023"
	{re: rx(ordRE + ` (?:of )?` + monthRE + `\.?,? ` + yearRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		return dayOf(monthNames[m[2]], m[1], m[3], ref)
	}},
	// "May 7th", "on the 7th of May" — no year: the most recent one.
	{re: rx(monthRE + `\.? ` + ordRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		return yearlessDay(monthNames[m[1]], m[2], ref)
	}},
	{re: rx(ordRE + ` (?:of )?` + monthRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		return yearlessDay(monthNames[m[2]], m[1], ref)
	}},
	// "early May 2023", "the end of June"
	{re: rx(partRE + `[ -]?` + monthRE + `(?:,? ` + yearRE + `)?`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		mo := monthNames[m[2]]
		if m[3] != "" {
			y, _ := strconv.Atoi(m[3])
			if !plausibleYear(y, ref) {
				return Window{}, nil, false
			}
			return partOf(m[1], monthWindow(y, mo)), nil, true
		}
		return partOf(m[1], pastMonth(mo, ref)), func(y int) Window { return partOf(m[1], monthWindow(y, mo)) }, true
	}},
	// "May 2023", "May of 2023"
	{re: rx(monthRE + `\.?,? (?:of )?` + yearRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		y, _ := strconv.Atoi(m[2])
		if !plausibleYear(y, ref) {
			return Window{}, nil, false
		}
		return monthWindow(y, monthNames[m[1]]), nil, true
	}},
	// "last May", "next March", "this June"
	{re: rx(`(last|this|next|past) ` + monthRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		mo := monthNames[m[2]]
		switch m[1] {
		case "this":
			return monthWindow(ref.Year(), mo), nil, true
		case "next":
			y := ref.Year()
			if mo <= ref.Month() {
				y++
			}
			return monthWindow(y, mo), nil, true
		}
		y := ref.Year()
		if mo >= ref.Month() {
			y--
		}
		return monthWindow(y, mo), nil, true
	}},
	// A bare month after a preposition: "in March", "during June". A month
	// with no preposition is accepted only for the unambiguous long names —
	// "may" and "march" are also a verb and a noun.
	{lead: 2, re: rx(`(?:(in|during|throughout|of|since|until|till|by|from|to|through|around|before|after) )?` + monthRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		name := m[2]
		if m[1] == "" && (len(name) <= 4 || name == "march") {
			return Window{}, nil, false
		}
		mo := monthNames[name]
		return pastMonth(mo, ref), func(y int) Window { return monthWindow(y, mo) }, true
	}},
	// Seasons: "last summer", "summer 2023", "the summer of 2019", "early spring".
	{re: rx(`(?:(?:in|during|over|through) the )?(?:(early|late|mid) )?(?:(last|this|next|past) )?` + seasonRE + `(?:,? (?:of )?` + yearRE + `)?`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		part, rel, name := m[1], m[2], m[3]
		// "fall" alone is usually a verb; it needs a qualifier to be a season.
		if name == "fall" && part == "" && rel == "" && m[4] == "" && !strings.Contains(m[0], "the ") {
			return Window{}, nil, false
		}
		var w Window
		switch {
		case m[4] != "":
			y, _ := strconv.Atoi(m[4])
			if !plausibleYear(y, ref) {
				return Window{}, nil, false
			}
			w = seasonWindow(name, y)
		case rel == "this":
			w = seasonWindow(name, currentSeasonYear(name, ref))
		case rel == "next":
			y := currentSeasonYear(name, ref)
			w = seasonWindow(name, y)
			if !w.Start.After(ref) {
				w = seasonWindow(name, y+1)
			}
		default:
			// "last summer", or a bare "summer": the most recent one that
			// has ended, unless a bare season is happening now.
			y := currentSeasonYear(name, ref)
			w = seasonWindow(name, y)
			if rel == "" && w.Contains(ref) {
				break
			}
			for w.End.After(ref) {
				y--
				w = seasonWindow(name, y)
			}
		}
		if part != "" {
			w = partOf(part, w)
		}
		return w, nil, true
	}},
	// Quarters: "Q3 2023", "the second quarter of 2022", "last quarter".
	{re: rx(`q([1-4])(?: of)? ` + yearRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		q, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		return quarter(y, q), nil, plausibleYear(y, ref)
	}},
	{re: rx(`(first|second|third|fourth|1st|2nd|3rd|4th) quarter(?: of)?(?: ` + yearRE + `)?`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		q := map[string]int{"first": 1, "1st": 1, "second": 2, "2nd": 2, "third": 3, "3rd": 3, "fourth": 4, "4th": 4}[m[1]]
		y := ref.Year()
		if m[2] != "" {
			y, _ = strconv.Atoi(m[2])
		}
		return quarter(y, q), nil, plausibleYear(y, ref)
	}},
	{re: rx(`(last|this|next|past) quarter`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		q := (int(ref.Month())-1)/3 + 1
		y := ref.Year()
		switch m[1] {
		case "last", "past":
			if q--; q == 0 {
				q, y = 4, y-1
			}
		case "next":
			if q++; q == 5 {
				q, y = 1, y+1
			}
		}
		return quarter(y, q), nil, true
	}},
	// Decades: "the 1990s", "the 90s", "the '80s".
	{re: rx(`(?:the )?'?(\d{2}|\d{4})s`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		n, _ := strconv.Atoi(m[1])
		if len(m[1]) == 2 {
			if n%10 != 0 {
				return Window{}, nil, false
			}
			if 2000+n > ref.Year() {
				n += 1900
			} else {
				n += 2000
			}
		} else if n%10 != 0 || !plausibleYear(n, ref) {
			return Window{}, nil, false
		}
		return Window{day(n, 1, 1), endOfDay(day(n+9, 12, 31))}, nil, true
	}},
	// A year after a word that makes it a date. A bare four-digit number is
	// not one: "port 2019" and "error 8080" must not become time windows.
	{re: rx(partRE + `[ -]?(?:of )?` + yearRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		y, _ := strconv.Atoi(m[2])
		if !plausibleYear(y, ref) {
			return Window{}, nil, false
		}
		return partOf(m[1], yearWindow(y)), nil, true
	}},
	{lead: 2, re: regexp.MustCompile(`\b(in|during|throughout|year|of|since|by|from|until|till|before|after|through|to|around|circa) (?:the year )?(\d{4})\b(?:[^-/.\d]|$)`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		y, _ := strconv.Atoi(m[2])
		if !plausibleYear(y, ref) {
			return Window{}, nil, false
		}
		return yearWindow(y), nil, true
	}},
	// Days relative to now.
	{re: rx(`(the day before yesterday|the day after tomorrow|yesterday|today|tonight|tomorrow|this morning|this afternoon|this evening|last night)`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		off := map[string]int{"the day before yesterday": -2, "yesterday": -1, "last night": -1,
			"tomorrow": 1, "the day after tomorrow": 2}[m[1]]
		return dayWindow(ref.AddDate(0, 0, off)), nil, true
	}},
	// Vague counts: "a couple of days ago", "a few weeks ago", "several months ago".
	{re: rx(`(?:a )?(couple|few|several)(?: of)? (day|week|month|year)s? ago`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		lo, hi := 1, 3 // couple
		if m[1] != "couple" {
			lo, hi = 2, 5
		}
		switch m[2] {
		case "day":
			return spanDays(ref.AddDate(0, 0, -hi), ref.AddDate(0, 0, -lo)), nil, true
		case "week":
			return spanDays(ref.AddDate(0, 0, -7*hi), ref.AddDate(0, 0, -7*lo)), nil, true
		case "month":
			return spanDays(ref.AddDate(0, 0, -30*(hi+0)), ref.AddDate(0, 0, -30*lo)), nil, true
		}
		return Window{day(ref.Year()-hi, 1, 1), endOfDay(day(ref.Year()-lo, 12, 31))}, nil, true
	}},
	// Exact counts: "3 days ago", "two weeks ago", "a month ago", "a year ago".
	{re: rx(countRE + ` (day|week|month|year)s? ago`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		n := countOf(m[1])
		if n <= 0 {
			return Window{}, nil, false
		}
		switch m[2] {
		case "day":
			return dayWindow(ref.AddDate(0, 0, -n)), nil, true
		case "week":
			c := ref.AddDate(0, 0, -7*n)
			return spanDays(c.AddDate(0, 0, -3), c.AddDate(0, 0, 3)), nil, true
		case "month":
			c := ref.AddDate(0, -n, 0)
			return monthWindow(c.Year(), c.Month()), nil, true
		}
		return yearWindow(ref.Year() - n), nil, true
	}},
	// Trailing spans: "the last 3 days", "past two weeks", "in the last few months".
	{re: rx(`(?:the )?(?:last|past|previous) (\d{1,3}|two|three|four|five|six|seven|eight|nine|ten|twelve|few|couple of|several) (day|week|month|year)s`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		n := countOf(m[1])
		switch m[1] {
		case "few", "several":
			n = 4
		case "couple of":
			n = 2
		}
		if n <= 0 {
			return Window{}, nil, false
		}
		var from time.Time
		switch m[2] {
		case "day":
			from = ref.AddDate(0, 0, -n)
		case "week":
			from = ref.AddDate(0, 0, -7*n)
		case "month":
			from = ref.AddDate(0, -n, 0)
		default:
			from = ref.AddDate(-n, 0, 0)
		}
		return spanDays(from, ref), nil, true
	}},
	// "earlier this year", "later this month"
	{re: rx(`(earlier|later) this (week|month|year)`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		p := periodWindow("this", m[2], ref)
		if m[1] == "earlier" {
			return Window{p.Start, endOfDay(ref)}, nil, true
		}
		return Window{day(ref.Year(), ref.Month(), ref.Day()), p.End}, nil, true
	}},
	// "last week", "this month", "next year", "past week", "last weekend".
	{re: rx(`(?:the )?(last|this|next|past|previous|coming|this past) (week|month|year|weekend)`), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		return periodWindow(m[1], m[2], ref), nil, true
	}},
	// Weekdays: "last Tuesday", "on Friday", "next Monday".
	{re: rx(`(?:(last|this|next|past|on|this past) )?` + weekdayRE), fn: func(m []string, ref time.Time) (Window, func(int) Window, bool) {
		name := m[2]
		if m[1] == "" && len(name) <= 3 {
			return Window{}, nil, false // "sat", "sun", "wed" are words too
		}
		target := weekdayNames[name]
		cur := weekdayIndex(ref)
		switch m[1] {
		case "next":
			d := (target - cur + 7) % 7
			if d == 0 {
				d = 7
			}
			return dayWindow(ref.AddDate(0, 0, d)), nil, true
		case "this":
			return dayWindow(ref.AddDate(0, 0, target-cur)), nil, true
		case "last", "past", "this past":
			d := (cur - target + 7) % 7
			if d == 0 {
				d = 7
			}
			return dayWindow(ref.AddDate(0, 0, -d)), nil, true
		}
		d := (cur - target + 7) % 7 // most recent, today included
		return dayWindow(ref.AddDate(0, 0, -d)), nil, true
	}},
}

func quarter(y, q int) Window {
	start := day(y, time.Month(3*(q-1)+1), 1)
	return Window{start, endOfDay(start.AddDate(0, 3, -1))}
}

func periodWindow(rel, unit string, ref time.Time) Window {
	ref = day(ref.Year(), ref.Month(), ref.Day())
	monday := ref.AddDate(0, 0, -weekdayIndex(ref))
	switch unit {
	case "week":
		switch rel {
		case "last", "previous":
			return spanDays(monday.AddDate(0, 0, -7), monday.AddDate(0, 0, -1))
		case "next", "coming":
			return spanDays(monday.AddDate(0, 0, 7), monday.AddDate(0, 0, 13))
		case "past", "this past":
			return spanDays(ref.AddDate(0, 0, -7), ref)
		}
		return spanDays(monday, monday.AddDate(0, 0, 6))
	case "month":
		switch rel {
		case "last", "previous":
			p := ref.AddDate(0, 0, -ref.Day())
			return monthWindow(p.Year(), p.Month())
		case "next", "coming":
			n := day(ref.Year(), ref.Month(), 1).AddDate(0, 1, 0)
			return monthWindow(n.Year(), n.Month())
		case "past", "this past":
			return spanDays(ref.AddDate(0, 0, -30), ref)
		}
		return monthWindow(ref.Year(), ref.Month())
	case "year":
		switch rel {
		case "last", "previous":
			return yearWindow(ref.Year() - 1)
		case "next", "coming":
			return yearWindow(ref.Year() + 1)
		case "past", "this past":
			return spanDays(ref.AddDate(-1, 0, 0), ref)
		}
		return yearWindow(ref.Year())
	}
	// weekend
	sat := monday.AddDate(0, 0, 5)
	switch rel {
	case "last", "past", "previous", "this past":
		// The most recent weekend that has finished: asked on a Sunday,
		// "last weekend" is the one before this one.
		prev := sat.AddDate(0, 0, -7)
		return spanDays(prev, prev.AddDate(0, 0, 1))
	case "next", "coming":
		next := sat
		if !sat.After(ref) {
			next = sat.AddDate(0, 0, 7)
		}
		return spanDays(next, next.AddDate(0, 0, 1))
	}
	return spanDays(sat, sat.AddDate(0, 0, 1))
}

func dayOf(m time.Month, d, y string, ref time.Time) (Window, func(int) Window, bool) {
	dd, _ := strconv.Atoi(d)
	yy, _ := strconv.Atoi(y)
	if dd < 1 || dd > 31 || !plausibleYear(yy, ref) {
		return Window{}, nil, false
	}
	t := day(yy, m, dd)
	if t.Month() != m {
		return Window{}, nil, false
	}
	return dayWindow(t), nil, true
}

func yearlessDay(m time.Month, d string, ref time.Time) (Window, func(int) Window, bool) {
	dd, _ := strconv.Atoi(d)
	if dd < 1 || dd > 31 {
		return Window{}, nil, false
	}
	w, ok := pastDay(m, dd, ref)
	if !ok {
		return Window{}, nil, false
	}
	return w, func(y int) Window {
		t := day(y, m, dd)
		return dayWindow(t)
	}, true
}

// scoreMatch ranks a candidate: a number is strong evidence of a date, a
// month name or a relative word next, then weekdays and bare period words.
// Ties go to the longer expression.
func scoreMatch(s string) int {
	score := 0
	if strings.ContainsAny(s, "0123456789") {
		score += 100
	}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '-' || r == '.' || r == '\'' }) {
		switch {
		case monthNames[w] != 0:
			score += 50
		case w == "yesterday" || w == "today" || w == "tomorrow" || w == "tonight" || w == "ago" ||
			w == "last" || w == "next" || w == "this" || w == "past" || w == "previous" || w == "earlier" || w == "later":
			score += 50
		case isWeekday(w):
			score += 30
		case w == "week" || w == "weeks" || w == "month" || w == "months" || w == "year" || w == "years" ||
			w == "weekend" || w == "quarter" || w == "spring" || w == "summer" || w == "autumn" ||
			w == "fall" || w == "winter" || w == "days" || w == "day" || strings.HasSuffix(w, "0s"):
			score += 20
		}
	}
	return score
}

func isWeekday(w string) bool { _, ok := weekdayNames[w]; return ok && len(w) > 3 }

var dateSpaceRE = regexp.MustCompile(`\s+`)
var dateJunkRE = regexp.MustCompile(`[?!;"()\[\]{}]`)

func normalizeDateText(q string) string {
	s := strings.ToLower(q)
	s = strings.NewReplacer("’", "'", "–", "-", "—", " - ", "mid-", "mid ").Replace(s)
	s = dateJunkRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(dateSpaceRE.ReplaceAllString(s, " "))
}

func findMatches(s string, ref time.Time) []dateMatch {
	var all []dateMatch
	for _, rule := range dateRules {
		for _, loc := range rule.re.FindAllStringSubmatchIndex(s, -1) {
			sub := make([]string, len(loc)/2)
			for i := range sub {
				if loc[2*i] >= 0 {
					sub[i] = s[loc[2*i]:loc[2*i+1]]
				}
			}
			w, rebuild, ok := rule.fn(sub, ref)
			if !ok {
				continue
			}
			start := loc[0]
			if rule.lead > 0 && loc[2*rule.lead] >= 0 {
				start = loc[2*rule.lead]
			}
			// Trailing context consumed by a lookahead-style group is not
			// part of the expression.
			text := strings.TrimRight(s[start:loc[1]], " ,.")
			all = append(all, dateMatch{start: start, end: start + len(text), w: w,
				rebuild: rebuild, score: scoreMatch(text)})
		}
	}
	// Strongest first; among overlapping candidates only the best survives.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].end-all[i].start > all[j].end-all[j].start
	})
	var kept []dateMatch
	for _, m := range all {
		overlap := false
		for _, k := range kept {
			if m.start < k.end && k.start < m.end {
				overlap = true
				break
			}
		}
		if !overlap {
			kept = append(kept, m)
		}
	}
	return kept
}

var rangeJoinRE = regexp.MustCompile(`^\s*(?:,\s*)?(?:to|until|till|through|thru|and|-)\s*$`)

// ParseWindow finds the time window a piece of text refers to, relative to
// ref ("now", or the moment the text was written). It recognises absolute
// dates, months, years, seasons, quarters and decades; days, weeks, months and
// years counted back from ref; named weekdays and periods (last week, this
// month, next year, last weekend); parts of a period (early May, the end of
// 2023); and ranges between two of those ("from May to July 2023", "between
// March 3 and March 10", "since June"). No model is involved.
//
// When several expressions appear, the most specific wins — one with a
// number, then one with a month name or a relative word — and ok is false
// when there is none.
func ParseWindow(text string, ref time.Time) (Window, bool) {
	ref = ref.UTC()
	s := normalizeDateText(text)
	if s == "" {
		return Window{}, false
	}
	ms := findMatches(s, ref)
	if len(ms) == 0 {
		return Window{}, false
	}
	// Ranges: two expressions joined by to/until/through/and/-.
	byPos := append([]dateMatch(nil), ms...)
	sort.Slice(byPos, func(i, j int) bool { return byPos[i].start < byPos[j].start })
	for i := 0; i+1 < len(byPos); i++ {
		a, b := byPos[i], byPos[i+1]
		between := s[a.end:b.start]
		if !rangeJoinRE.MatchString(between) {
			continue
		}
		before := strings.TrimSpace(s[:a.start])
		joiner := strings.TrimSpace(strings.Trim(between, " ,"))
		if joiner == "and" && !strings.HasSuffix(before, "between") {
			continue
		}
		start := a.w
		if a.rebuild != nil && b.rebuild == nil {
			start = a.rebuild(b.w.Start.Year())
			if start.Start.After(b.w.Start) {
				start = a.rebuild(b.w.Start.Year() - 1)
			}
		}
		if start.Start.After(b.w.End) {
			continue
		}
		return Window{start.Start, b.w.End}, true
	}
	best := ms[0]
	w := best.w
	// Open-ended prefixes turn a point into a span reaching ref.
	prefix := strings.TrimSpace(s[:best.start])
	switch {
	case strings.HasSuffix(prefix, "since"), strings.HasSuffix(prefix, "after"):
		if w.End.Before(ref) {
			start := w.Start
			if strings.HasSuffix(prefix, "after") {
				start = w.End
			}
			w = Window{start, endOfDay(ref)}
		}
	case strings.HasSuffix(prefix, "before"), strings.HasSuffix(prefix, "until"), strings.HasSuffix(prefix, "prior to"):
		w = Window{w.Start.AddDate(-1, 0, 0), w.Start}
	}
	return w, true
}

// parseLooseDate reads a date a model wrote: an ISO timestamp, an ISO date, a
// year-month or a bare year. A coarse value widens to the period it names, so
// "2023-05" is all of May rather than its first day — collapsing a coarse date
// to its start is how "sometime in 2015" becomes "on 1 January 2015".
func parseLooseDate(s string, ref time.Time) (Window, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "n/a") || strings.EqualFold(s, "null") || strings.EqualFold(s, "none") {
		return Window{}, false
	}
	if t := parseTime(s); !t.IsZero() {
		return Window{t, t}, true
	}
	if len(s) == 7 && s[4] == '-' {
		y, err1 := strconv.Atoi(s[:4])
		m, err2 := strconv.Atoi(s[5:])
		if err1 == nil && err2 == nil && m >= 1 && m <= 12 {
			return monthWindow(y, time.Month(m)), true
		}
	}
	if len(s) == 4 {
		if y, err := strconv.Atoi(s); err == nil && y > 0 {
			return yearWindow(y), true
		}
	}
	if w, ok := ParseWindow(s, ref); ok {
		return w, true
	}
	return Window{}, false
}
