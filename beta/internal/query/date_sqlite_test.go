// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && cgo

package query

import (
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The dates against SQLite itself, through 0.x: random dates and modifiers,
// many more than the corpus has, the table of dateCases, and the cases on
// 'now' at the time. These tests need cgo, as 0.x does.

// dateGen makes random moments, dates of their own and modifiers.
type dateGen struct{ r *rand.Rand }

func (g *dateGen) pick(xs ...string) string { return xs[g.r.IntN(len(xs))] }

// year is a year of SQLite's range: any of them, one where the calendar
// turns, or one near ours.
func (g *dateGen) year() int {
	switch g.r.IntN(6) {
	case 0:
		return -4713 + g.r.IntN(14713)
	case 1:
		years := []int{-4713, -4712, -4709, -401, -400, -100, -4, -1, 0, 1, 4, 100, 400, 1600, 1700, 1800, 1900,
			2000, 2100, 2200, 2400, 9900, 9996, 9998, 9999}
		return years[g.r.IntN(len(years))]
	}
	return 1900 + g.r.IntN(250)
}

// clock is the day y-mo-d at a random time.
func (g *dateGen) clock(y, mo, d int) time.Time {
	return time.Date(y, time.Month(mo), d, g.r.IntN(24), g.r.IntN(60), g.r.IntN(60), g.r.IntN(1000)*1e6, time.UTC)
}

// moment is a moment for 'now' inside SQLite's range and after its first
// millisecond, where SQLite's clock would give NULL: anywhere, at the ends
// of the range, on one of the last days of a month, around a leap day, or
// in a year near ours. It's never the first moment of year 1, the zero
// time, which a Frame takes for no moment.
func (g *dateGen) moment() time.Time {
	r := g.r
	for {
		var t time.Time
		switch r.IntN(10) {
		case 0:
			t = time.UnixMilli(1 + r.Int64N(maxJD) - unixEpochJD)
		case 1:
			ends := []int64{1, 2, 43200000, 86400000, 86400001, maxJD, maxJD - 1, maxJD - 86400000,
				unixEpochJD + at(0, 1, 1, 0, 0, 0, 0).UnixMilli(), unixEpochJD + at(1, 1, 1, 0, 0, 0, 0).UnixMilli(),
				unixEpochJD + at(9999, 1, 1, 0, 0, 0, 0).UnixMilli(), unixEpochJD + at(-4712, 1, 1, 0, 0, 0, 0).UnixMilli()}
			t = time.UnixMilli(ends[r.IntN(len(ends))] + r.Int64N(3) - 1 - unixEpochJD)
		case 2, 3, 4, 5:
			y, mo := g.year(), 1+r.IntN(12)
			t = g.clock(y, mo, monthDays(y, mo)-r.IntN(4))
		case 6, 7:
			years := []int{2024, 2000, 1600, 400, 0, -4, -400, 9996, 2028, 1900, 2100, 1700, -100, 9900, 2023, 2026}
			days := [][2]int{{2, 27}, {2, 28}, {2, 29}, {3, 1}, {1, 29}, {1, 30}, {1, 31}}
			md := days[r.IntN(len(days))]
			t = g.clock(years[r.IntN(len(years))], md[0], md[1])
		default:
			t = g.clock(1900+r.IntN(250), 1+r.IntN(12), 1+r.IntN(28))
		}
		if jd := unixEpochJD + t.UnixMilli(); jd >= 1 && jd <= maxJD && !t.IsZero() {
			return t
		}
	}
}

func pad(n, width int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", max(0, width-len(s))) + s
}

// fixedText is a date of its own, written out as SQLite reads one, from a
// year before SQLite's range to its end, often with a day its month hasn't
// got, and with a time or without one.
func (g *dateGen) fixedText() string {
	r := g.r
	y := g.year()
	if r.IntN(40) == 0 {
		y = -4714
	}
	sign := ""
	if y < 0 {
		sign, y = "-", -y
	}
	d := 1 + r.IntN(31)
	if r.IntN(2) == 0 {
		d = 26 + r.IntN(6)
	}
	s := sign + pad(y, 4) + "-" + pad(1+r.IntN(12), 2) + "-" + pad(d, 2)
	hh, mm, ss := pad(r.IntN(24), 2), pad(r.IntN(60), 2), pad(r.IntN(60), 2)
	switch r.IntN(4) {
	case 1:
		s += " " + hh + ":" + mm
	case 2:
		s += "T" + hh + ":" + mm + ":" + ss
	case 3:
		s += " " + hh + ":" + mm + ":" + ss + "." + pad(r.IntN(1000), 3)
	}
	return s
}

// number is a whole number of a unit, day, month or year: small, anywhere
// up to the unit's limit, right at it, past 64 bits, or one that often
// moves a month end, sometimes with zeros in front.
func (g *dateGen) number(unit string) string {
	r := g.r
	limit := map[string]int64{"day": 5373485, "month": 176546, "year": 14713}[unit]
	var n int64
	switch r.IntN(20) {
	case 0, 1, 2, 3, 4, 5:
		n = r.Int64N(32)
	case 6, 7, 8, 9:
		n = r.Int64N(400)
	case 10:
		n = r.Int64N(limit)
	case 11:
		n = limit - 2 + r.Int64N(3)
	case 12:
		return strings.Repeat("9", 19+r.IntN(10))
	default:
		ns := []int64{1, 11, 12, 13, 24, 25, 48, 100, 400, 1200, 4800}
		n = ns[r.IntN(len(ns))]
	}
	s := strconv.FormatInt(n, 10)
	if r.IntN(10) == 0 {
		s = "00" + s
	}
	return s
}

// modifier is a modifier of the subset: a sign, a whole number, one or more
// spaces and a unit, in any case.
func (g *dateGen) modifier() string {
	r := g.r
	unit := g.pick("day", "month", "year")
	name := []byte(unit + g.pick("", "s"))
	for i := range name {
		if r.IntN(4) == 0 {
			name[i] -= 'a' - 'A'
		}
	}
	spaces := " "
	if r.IntN(8) == 0 {
		spaces = ""
		for k := 1 + r.IntN(3); k > 0; k-- {
			spaces += g.pick(" ", "\t", "\n", "\v", "\f", "\r")
		}
	}
	return g.pick("+", "-") + g.number(unit) + spaces + string(name)
}

// oddModifier is a modifier outside the subset's pattern that SQLite reads
// as "NNN units", for the arithmetic below the refusal: without a sign,
// with a fraction, of hours, minutes or seconds, of a unit SQLite hasn't
// got, or written wrong. The fractions are eighths, which SQLite and the
// port work out exactly, so no rounding of a fused multiply and add can
// tell them apart.
func (g *dateGen) oddModifier() string {
	r := g.r
	small := strconv.Itoa(r.IntN(100))
	switch r.IntN(6) {
	case 0:
		unit := g.pick("day", "month", "year")
		return g.number(unit) + " " + unit + g.pick("", "s")
	case 1:
		return g.pick("+", "-", "") + small + g.pick(".5", ".25", ".125", ".75", ".375", ".0", ".") + " " +
			g.pick("day", "days", "month", "months", "year", "years", "hour", "hours", "minute", "minutes", "second", "seconds")
	case 2:
		return g.pick("+", "-") + strconv.Itoa(r.IntN(100000)) + " " + g.pick("hour", "hours", "minute", "minutes", "second", "seconds")
	case 3:
		return g.pick("+", "-") + small + " " + g.pick("week", "weeks", "fortnight", "dayz", "mon", "yr", "sec", "d")
	case 4:
		return g.pick("+ 1 day", "+1day", "+1 day ", "1e1 days", "+1e1 days", "+.5 days", "-0.0 days", "+1. days",
			"-1e-1 years", "+0x10 days", "--1 day", "+1 da", "", "+")
	}
	return g.modifier()
}

func quoteSQL(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// TestRandomDatesGive0xsAnswers runs date() and datetime() on random dates,
// with one to three modifiers, through 0.x and through the arithmetic, and
// the answers must be the same. Three cases in four go through 'now', as
// the subset has it: the date written out for 0.x is the moment in
// Frame.Now. The fourth runs below the refusal, as the closing test does:
// a date of its own, through parseFixedDate, often with a day its month
// hasn't got, and some modifiers outside the subset's pattern.
func TestRandomDatesGive0xsAnswers(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	g := &dateGen{r: rand.New(rand.NewPCG(2, 0x5132))}
	n := 40000
	if testing.Short() {
		n = 4000
	}
	counts := map[string]int{}
	bad := 0
	for i := 0; i < n; i++ {
		fn := g.pick("date", "datetime")
		fixed := i%4 == 3
		var mods []string
		var ms []dateModifier
		for k := 1 + g.r.IntN(3); k > 0; k-- {
			m := g.modifier()
			if fixed && g.r.IntN(3) == 0 {
				m = g.oddModifier()
			}
			mods = append(mods, quoteSQL(m))
			ms = append(ms, parseModifier(m))
		}
		var start string
		var day int
		var got *sqlcorpus.Answer
		if fixed {
			start = g.fixedText()
			p := parseFixedDate(start)
			v := dateValue(p, ms, fn == "datetime")
			got = &sqlcorpus.Answer{Columns: []string{"v"}, Rows: [][]difftest.Value{{{V: goValue(v)}}}}
			day = p.D
		} else {
			now := g.moment()
			start = momentText(now)
			got = runAt(t, "SELECT "+fn+"('now', "+strings.Join(mods, ", ")+") AS v", now, nil)
			day = now.UTC().Day()
		}
		q := "SELECT " + fn + "(" + quoteSQL(start) + ", " + strings.Join(mods, ", ") + ") AS v"
		want := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: q}, false)
		switch {
		case want.Error != "":
			counts["error"]++
		case want.Rows[0][0].V == nil:
			counts["NULL"]++
		case day > 28 && monthOrYear(ms):
			counts["text, from the 29th to the 31st with a month or a year"]++
		default:
			counts["text"]++
		}
		if !sqlcorpus.Same(want, got, false) {
			bad++
			if bad <= 30 {
				t.Errorf("%s (fixed %v)\n  0.x  %+v\n  Beta %+v", q, fixed, want, got)
			}
		}
	}
	t.Logf("%v", counts)
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, n)
	}
}

// TestTheDateCasesAre0xs holds dateCases to 0.x, each on its moment
// written out, apart from the moments only SQLite's clock could give.
func TestTheDateCasesAre0xs(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	for _, cs := range dateCases {
		if cs.nowOnly {
			continue
		}
		q := "SELECT " + fixedAt(cs.expr, cs.now) + " AS v"
		got := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: q}, false)
		want := &sqlcorpus.Answer{Columns: []string{"v"}, Rows: [][]difftest.Value{{{V: cs.want}}}}
		if !sqlcorpus.Same(want, got, false) {
			t.Errorf("%s: 0.x gives %+v, and the table wants %v", q, got, cs.want)
		}
	}
}

// moreNowCases are cases on 'now' beside the corpus's, for
// TestTheNowCasesGive0xsAnswersAtTheTime.
var moreNowCases = []string{
	"SELECT date('NOW'), datetime('Now', '+1 DAY')",
	"SELECT date('now', '+5373484 days', '-5373484 days'), date('now', '+5373485 days', '-5373485 days')",
	"SELECT date('now', '-1000000 days'), datetime('now', '-2461000 days')",
	"SELECT date('now', '+0 days', '-0 months', '+0 years'), date('now', '+0001 month', '-00 year')",
	"SELECT date('now', '+12 months') = date('now', '+1 year'), date('now', '-1 day') < date('now')",
	"SELECT date('now', '+7973 years'), date('now', '+7974 years'), date('now', '-6738 years')",
	"SELECT date('now', '-176545 months'), date('now', '+176545 months'), date('now', '+99999999999999999999 days')",
	"SELECT datetime('now', '+1 month', '-1 month', '+1 day', '-1 day', '+1 year')",
	"SELECT date('now', '+1\tday', '-2\nmonths', '+3\v\f\ryears')",
	"SELECT length(datetime('now')), typeof(date('now')), coalesce(date('now', '+5373485 days'), 'none')",
	"SELECT key FROM people WHERE joined > date('now', '-1 year') ORDER BY key",
	"SELECT key FROM people WHERE date('now', '-32 months') < joined AND joined <= date('now') ORDER BY key",
	"SELECT key, date('now') FROM people WHERE joined IN (date('now'), '2024-02-29') ORDER BY key",
}

// TestTheNowCasesGive0xsAnswersAtTheTime runs the corpus's cases on 'now',
// and moreNowCases, on 0.x and through the evaluator at the same moment.
// 0.x reads its clock as the case runs, between two readings taken before
// and after it, so its answer must be the evaluator's at one of the two:
// they're a few milliseconds apart, and a date changes at most once
// between them.
func TestTheNowCasesGive0xsAnswersAtTheTime(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	people := fixtureRows(t, db, "people")
	all, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", "dates.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []sqlcorpus.Case
	for _, cs := range all {
		if cs.Kind == "now" {
			cases = append(cases, cs)
		}
	}
	for _, q := range moreNowCases {
		cases = append(cases, sqlcorpus.Case{ID: "more", SQL: q, In: true})
	}
	for _, cs := range cases {
		before := time.Now()
		want := sqlcorpus.Run(e, db, cs, false)
		after := time.Now()
		first, last := runAt(t, cs.SQL, before, people), runAt(t, cs.SQL, after, people)
		if !sqlcorpus.Same(want, first, false) && !sqlcorpus.Same(want, last, false) {
			t.Errorf("%s: %s\n  0.x  %+v\n  Beta %+v at %s\n  and  %+v at %s", cs.ID, cs.SQL, want, first, before, last, after)
		}
	}
}
