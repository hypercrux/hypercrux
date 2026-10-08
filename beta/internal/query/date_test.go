// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The tests of Q2, dates. TestTheCorpusDatesGive0xsAnswers is the closing
// test. date_sqlite_test.go, built only with cgo, holds the arithmetic to
// 0.x on many more dates than the corpus has, and the cases on 'now' to 0.x
// at the time.

// getDigits is SQLite's getDigits, for parseFixedDate: it reads the numbers
// the format describes, four characters a number, and gives the ones it read
// before the first that doesn't fit.
func getDigits(z, format string) []int {
	most := [...]int{12, 14, 24, 31, 59, 14712}
	var vals []int
	for {
		n, least, max := int(format[0]-'0'), int(format[1]-'0'), most[format[2]-'a']
		var next byte
		if len(format) > 3 {
			next = format[3]
		}
		v := 0
		for ; n > 0; n-- {
			if z == "" || !isDigit(z[0]) {
				return vals
			}
			v = v*10 + int(z[0]-'0')
			z = z[1:]
		}
		if v < least || v > max || next != 0 && (z == "" || z[0] != next) {
			return vals
		}
		vals = append(vals, v)
		if next == 0 {
			return vals
		}
		z, format = z[1:], format[4:]
	}
}

// parseFixedDate reads a date of its own as SQLite's parseYyyyMmDd and
// parseHhMmSs read one, which is the form of every date the corpus starts
// from: YYYY-MM-DD, perhaps after a minus sign, then perhaps a time after
// spaces or a T, HH:MM with :SS and a fraction if it has them. Anything
// else, such as 'not a date', gives no date, as in SQLite, whose other forms
// no case uses. Zones aren't read, since no date here has one.
func parseFixedDate(s string) dateTime {
	none := dateTime{isError: true}
	z := s
	neg := strings.HasPrefix(z, "-")
	if neg {
		z = z[1:]
	}
	ymd := getDigits(z, "40f-21a-21d")
	if len(ymd) != 3 {
		return none
	}
	p := dateTime{Y: ymd[0], M: ymd[1], D: ymd[2], validYMD: true}
	if neg {
		p.Y = -p.Y
	}
	z = z[10:]
	for z != "" && (isSpace(z[0]) || z[0] == 'T') {
		z = z[1:]
	}
	if z == "" {
		return p
	}
	hm := getDigits(z, "20c:20e")
	if len(hm) != 2 {
		return none
	}
	z = z[5:]
	sec, frac := 0, 0.0
	if strings.HasPrefix(z, ":") {
		ss := getDigits(z[1:], "20e")
		if len(ss) != 1 {
			return none
		}
		sec, z = ss[0], z[3:]
		if len(z) > 1 && z[0] == '.' && isDigit(z[1]) {
			scale := 1.0
			for z = z[1:]; z != "" && isDigit(z[0]); z = z[1:] {
				frac = frac*10 + float64(z[0]-'0')
				scale *= 10
			}
			frac = min(frac/scale, 0.999)
		}
	}
	for z != "" && isSpace(z[0]) {
		z = z[1:]
	}
	if z != "" {
		return none
	}
	p.h, p.m, p.s, p.validHMS = hm[0], hm[1], float64(sec)+frac, true
	return p
}

// errAnswer is an error as the corpus has it.
func errAnswer(err error) *sqlcorpus.Answer {
	return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
}

// runAt runs a SELECT at the moment now, as the planner will run it: over
// rows, a table's records in key order, when it has a FROM, and else over
// one row with no fields, with its WHERE, and with an ORDER BY of the key
// at most, which the order of the rows already gives. It gives the answer
// as the corpus has it, the columns named by alias, by field or by text.
func runAt(t testing.TB, q string, now time.Time, rows []fakeRow) *sqlcorpus.Answer {
	t.Helper()
	s, err := Parse(q)
	if err != nil {
		return errAnswer(err)
	}
	sel := s.(*Select)
	if sel.From == nil {
		rows = []fakeRow{{}}
	}
	for _, term := range sel.OrderBy {
		if c, ok := term.Expr.(*Column); !ok || c.Name.Folded() != "key" || term.Desc {
			t.Fatalf("%s: runAt orders by the key alone", q)
		}
	}
	scope := &fakeScope{}
	var cond Cond
	if sel.Where != nil {
		if cond, err = CompileCondition(sel.Where, scope); err != nil {
			return errAnswer(err)
		}
	}
	ans := &sqlcorpus.Answer{}
	evs := make([]Eval, len(sel.Results))
	for i, r := range sel.Results {
		name := r.Text
		if c, ok := r.Expr.(*Column); ok {
			name = c.Name.Folded()
		}
		if r.Alias != nil {
			name = r.Alias.Name
		}
		ans.Columns = append(ans.Columns, name)
		if evs[i], err = Compile(r.Expr, scope); err != nil {
			return errAnswer(err)
		}
	}
	for _, row := range rows {
		f := &Frame{Row: row, Now: now}
		if cond != nil {
			ok, err := cond(f)
			if err != nil {
				return errAnswer(err)
			}
			if !ok {
				continue
			}
		}
		var out []difftest.Value
		for _, ev := range evs {
			v, err := ev(f)
			if err != nil {
				return errAnswer(err)
			}
			out = append(out, difftest.Value{V: goValue(v)})
		}
		ans.Rows = append(ans.Rows, out)
	}
	return ans
}

// outsideTheSubset reports whether err refuses SQL as outside the subset.
func outsideTheSubset(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && strings.Contains(pe.Msg, "is outside the SQL subset")
}

// TestTheCorpusDatesGive0xsAnswers is the closing test: every case in the
// corpus on a fixed date gives 0.x's answer exactly, through the arithmetic
// on its fixed date. The subset takes dates on 'now' only, so each call is
// refused as it compiles, and the test runs the arithmetic below the
// refusal, from the date parseFixedDate reads. date(NULL, '+1 day') is
// refused by the parser, as Q3.md says.
func TestTheCorpusDatesGive0xsAnswers(t *testing.T) {
	cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", "dates.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, cs := range cases {
		if cs.Kind != "date" {
			continue
		}
		s, err := Parse(cs.SQL)
		if err != nil {
			counts["refused by the parser"]++
			if !outsideTheSubset(err) || !strings.Contains(cs.SQL, "date(NULL") {
				t.Errorf("%s: %s: the parser gives %v", cs.ID, cs.SQL, err)
			}
			continue
		}
		r := s.(*Select).Results[0]
		call := r.Expr.(*Call)
		if _, err := Compile(call, nil); !outsideTheSubset(err) {
			t.Errorf("%s: %s compiles, with %v, and a date on %s is outside the subset", cs.ID, cs.SQL, err, call.Args[0])
		}
		start := parseFixedDate(call.Args[0].(*Literal).Data)
		var mods []dateModifier
		for _, a := range call.Args[1:] {
			text := a.(*Literal).Data
			mods = append(mods, parseModifier(text))
			if !subsetModifier(text) {
				counts["with a modifier outside the subset"]++
			}
		}
		v := dateValue(start, mods, call.Func() == "datetime")
		got := &sqlcorpus.Answer{Columns: []string{r.Text}, Rows: [][]difftest.Value{{{V: goValue(v)}}}}
		if !sqlcorpus.Same(cs.Answer, got, false) {
			t.Errorf("%s: %s\n  0.x  %+v\n  Beta %+v", cs.ID, cs.SQL, cs.Answer.Rows, got.Rows)
		}
		counts["worked out"]++
		switch {
		case v.IsNull():
			counts["NULL"]++
		case start.validYMD && start.D > 28 && monthOrYear(mods):
			counts["a month or a year moved from the 29th to the 31st"]++
		case start.validYMD && start.M == 2 && start.D >= 28:
			counts["from 28 or 29 February"]++
		}
	}
	t.Logf("%v", counts)
	if counts["worked out"] != 607 || counts["refused by the parser"] != 1 {
		t.Errorf("%d cases worked out and %d refused by the parser, where the corpus has 607 and 1",
			counts["worked out"], counts["refused by the parser"])
	}
}

func monthOrYear(mods []dateModifier) bool {
	for _, m := range mods {
		if !m.bad && (m.unit == unitMonth || m.unit == unitYear) {
			return true
		}
	}
	return false
}

func at(y int, mo time.Month, d, h, mi, s, ms int) time.Time {
	return time.Date(y, mo, d, h, mi, s, ms*1e6, time.UTC)
}

// dateCases are dates on 'now' at set moments, with the answers SQLite
// gives, which TestTheDateCasesAre0xs holds to 0.x through the same dates
// written out. nowOnly marks a moment a date of its own can't stand for:
// SQLite's clock gives NULL at or before the start of its range, where a
// date written out there is a date, and a year past 9999 can't be written.
var dateCases = []struct {
	now     time.Time
	expr    string
	want    any // the text, or nil for NULL
	nowOnly bool
}{
	{now: at(2026, 10, 8, 12, 34, 56, 789), expr: "date('now')", want: "2026-10-08"},
	{now: at(2026, 10, 8, 12, 34, 56, 789), expr: "datetime('now')", want: "2026-10-08 12:34:56"},
	{now: at(2026, 10, 8, 12, 34, 56, 789), expr: "date('now', '+1 day')", want: "2026-10-09"},
	{now: at(2026, 10, 8, 12, 34, 56, 789), expr: "datetime('now', '-1 day')", want: "2026-10-07 12:34:56"},
	{now: at(2026, 10, 8, 12, 34, 56, 789), expr: "date('now', '+1 month')", want: "2026-11-08"},
	{now: at(2026, 10, 8, 12, 34, 56, 789), expr: "date('now', '+13 months')", want: "2027-11-08"},
	{now: at(2026, 10, 8, 12, 34, 56, 789), expr: "date('now', '-4 years')", want: "2022-10-08"},
	{now: at(2026, 10, 8, 12, 34, 56, 789), expr: "datetime('now', '+1 year')", want: "2027-10-08 12:34:56"},
	// A day past the end of its month runs on into the next.
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '+1 month')", want: "2026-03-03"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "datetime('now', '+1 month')", want: "2026-03-03 10:00:00"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '+1 month', '-1 month')", want: "2026-02-03"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '-1 month')", want: "2025-12-31"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '+3 months')", want: "2026-05-01"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '-11 months')", want: "2025-03-03"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '+12 months')", want: "2027-01-31"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '-12 months')", want: "2025-01-31"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '-13 months')", want: "2024-12-31"},
	// The modifiers apply in turn.
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '+1 month', '+1 day')", want: "2026-03-04"},
	{now: at(2026, 1, 31, 10, 0, 0, 0), expr: "date('now', '+1 day', '+1 month')", want: "2026-03-01"},
	{now: at(2026, 3, 31, 0, 0, 0, 0), expr: "date('now', '-1 month')", want: "2026-03-03"},
	{now: at(2026, 3, 31, 0, 0, 0, 0), expr: "date('now', '+1 month', '-1 day')", want: "2026-04-30"},
	{now: at(2026, 5, 31, 0, 0, 0, 0), expr: "date('now', '+1 month')", want: "2026-07-01"},
	{now: at(2026, 12, 31, 0, 0, 0, 0), expr: "date('now', '+2 months')", want: "2027-03-03"},
	{now: at(2026, 12, 31, 0, 0, 0, 0), expr: "date('now', '-10 months')", want: "2026-03-03"},
	// Leap days.
	{now: at(2024, 1, 31, 0, 0, 0, 0), expr: "date('now', '+1 month')", want: "2024-03-02"},
	{now: at(2024, 1, 31, 0, 0, 0, 0), expr: "date('now', '+1 month', '+1 day')", want: "2024-03-03"},
	{now: at(2024, 1, 31, 0, 0, 0, 0), expr: "date('now', '+1 day', '+1 month')", want: "2024-03-01"},
	{now: at(2024, 3, 31, 0, 0, 0, 0), expr: "date('now', '-1 month')", want: "2024-03-02"},
	{now: at(2023, 3, 31, 0, 0, 0, 0), expr: "date('now', '-1 month')", want: "2023-03-03"},
	{now: at(2024, 2, 29, 8, 0, 0, 0), expr: "date('now', '+1 year')", want: "2025-03-01"},
	{now: at(2024, 2, 29, 8, 0, 0, 0), expr: "date('now', '+4 years')", want: "2028-02-29"},
	{now: at(2024, 2, 29, 8, 0, 0, 0), expr: "date('now', '-1 year')", want: "2023-03-01"},
	{now: at(2024, 2, 29, 8, 0, 0, 0), expr: "date('now', '+76 years')", want: "2100-03-01"},
	{now: at(2024, 2, 29, 8, 0, 0, 0), expr: "date('now', '+100 years')", want: "2124-02-29"},
	{now: at(2024, 2, 29, 8, 0, 0, 0), expr: "date('now', '+12 months')", want: "2025-03-01"},
	{now: at(2024, 2, 29, 8, 0, 0, 0), expr: "date('now', '+1 day')", want: "2024-03-01"},
	{now: at(2024, 2, 29, 8, 0, 0, 0), expr: "date('now', '-1 day', '+1 year')", want: "2025-02-28"},
	{now: at(2000, 2, 29, 0, 0, 0, 0), expr: "date('now', '+400 years')", want: "2400-02-29"},
	{now: at(2000, 2, 29, 0, 0, 0, 0), expr: "date('now', '+100 years')", want: "2100-03-01"},
	{now: at(2000, 2, 29, 0, 0, 0, 0), expr: "date('now', '-100 years')", want: "1900-03-01"},
	{now: at(1900, 2, 28, 0, 0, 0, 0), expr: "date('now', '+1 day')", want: "1900-03-01"},
	// The ends of SQLite's range. A run of days can leave it and come back,
	// and a month or a year can't, since they look at the date.
	{now: at(9999, 12, 31, 23, 59, 59, 999), expr: "datetime('now')", want: "9999-12-31 23:59:59"},
	{now: at(9999, 12, 31, 23, 59, 59, 999), expr: "date('now', '+1 day')", want: nil},
	{now: at(9999, 12, 31, 23, 59, 59, 999), expr: "date('now', '+1 day', '-1 day')", want: "9999-12-31"},
	{now: at(9999, 12, 31, 23, 59, 59, 999), expr: "date('now', '+1 day', '+1 month', '-1 month', '-1 day')", want: nil},
	{now: at(9999, 12, 31, 23, 59, 59, 999), expr: "date('now', '+1 year', '-1 year')", want: nil},
	{now: at(9999, 12, 31, 23, 59, 59, 999), expr: "date('now', '-1 month')", want: "9999-12-01"},
	{now: at(9999, 12, 31, 23, 59, 59, 999), expr: "datetime('now', '+1 month')", want: nil},
	{now: at(0, 1, 1, 0, 0, 0, 0), expr: "date('now')", want: "0000-01-01"},
	{now: at(0, 1, 1, 0, 0, 0, 0), expr: "date('now', '-1 day')", want: "-0001-12-31"},
	{now: at(0, 1, 1, 0, 0, 0, 0), expr: "date('now', '-1 year')", want: "-0001-01-01"},
	{now: at(0, 1, 1, 0, 0, 0, 0), expr: "datetime('now', '-1 month')", want: "-0001-12-01 00:00:00"},
	{now: at(-4713, 11, 24, 12, 0, 0, 1), expr: "datetime('now')", want: "-4713-11-24 12:00:00"},
	{now: at(-4713, 11, 24, 12, 0, 0, 1), expr: "date('now', '-1 day')", want: nil},
	{now: at(-4713, 11, 24, 12, 0, 0, 1), expr: "date('now', '-1 day', '+1 day')", want: "-4713-11-24"},
	{now: at(-4713, 11, 24, 12, 0, 0, 1), expr: "date('now', '+1 month')", want: "-4713-12-24"},
	{now: at(-4713, 11, 24, 12, 0, 0, 1), expr: "date('now', '-1 month')", want: nil},
	{now: at(-4713, 11, 24, 12, 0, 0, 1), expr: "date('now', '-1 year')", want: nil},
	{now: at(-4713, 11, 24, 12, 0, 0, 0), expr: "date('now')", want: nil, nowOnly: true},
	{now: at(-4713, 11, 24, 12, 0, 0, 0), expr: "date('now', '+1 day')", want: nil, nowOnly: true},
	{now: at(-4713, 11, 24, 11, 59, 59, 999), expr: "date('now')", want: nil, nowOnly: true},
	{now: at(-4800, 1, 1, 0, 0, 0, 0), expr: "date('now', '+1000 days')", want: nil, nowOnly: true},
	{now: at(10000, 1, 1, 0, 0, 0, 0), expr: "date('now')", want: nil, nowOnly: true},
	{now: at(10000, 1, 1, 0, 0, 0, 0), expr: "date('now', '-1 day')", want: "9999-12-31", nowOnly: true},
	{now: at(10000, 1, 1, 0, 0, 0, 0), expr: "date('now', '-1 month')", want: nil, nowOnly: true},
	// Years before year 1, and the sizes of the numbers.
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '-1000000 days')", want: "-0712-11-09"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+5373484 days', '-5373484 days')", want: "2026-10-07"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+5373485 days', '-5373485 days')", want: nil},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+176546 months')", want: nil},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+99999999999999999999 days')", want: nil},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+7973 years')", want: "9999-10-07"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+7974 years')", want: nil},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '-6738 years')", want: "-4712-10-07"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '-6739 years')", want: nil},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '-0 days')", want: "2026-10-07"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+0 months')", want: "2026-10-07"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+0001 MONTH')", want: "2026-11-07"},
	// 'now' and the units in any case, and SQLite's spaces.
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('NOW')", want: "2026-10-07"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('Now', '-0 Years')", want: "2026-10-07"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+1   DAYS')", want: "2026-10-08"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '+1\tday')", want: "2026-10-08"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "date('now', '-2\r\n\v\fMonths')", want: "2026-08-07"},
	{now: at(2026, 10, 7, 0, 0, 0, 0), expr: "DateTime('now', '+1 yEaR')", want: "2027-10-07 00:00:00"},
	// The moment is in UTC, whatever its zone, and counts whole
	// milliseconds, which datetime() cuts off.
	{now: time.Date(2026, 10, 8, 2, 30, 0, 0, time.FixedZone("UTC+3", 3*3600)), expr: "date('now')", want: "2026-10-07"},
	{now: time.Date(2026, 10, 8, 2, 30, 0, 0, time.FixedZone("UTC+3", 3*3600)), expr: "datetime('now')", want: "2026-10-07 23:30:00"},
	{now: time.Date(2026, 10, 8, 12, 34, 56, 999999999, time.UTC), expr: "datetime('now')", want: "2026-10-08 12:34:56"},
	{now: time.Date(2026, 10, 8, 12, 34, 56, 999999999, time.UTC), expr: "datetime('now', '+1 month')", want: "2026-11-08 12:34:56"},
	{now: at(2026, 10, 8, 23, 59, 59, 500), expr: "datetime('now', '+1 day')", want: "2026-10-09 23:59:59"},
}

func TestDateCases(t *testing.T) {
	for _, cs := range dateCases {
		ans := runAt(t, "SELECT "+cs.expr+" AS v", cs.now, nil)
		want := &sqlcorpus.Answer{Columns: []string{"v"}, Rows: [][]difftest.Value{{{V: cs.want}}}}
		if !sqlcorpus.Same(want, ans, false) {
			t.Errorf("%s at %s: got %+v, want %v", cs.expr, cs.now, ans, cs.want)
		}
	}
}

// TestTheNowCasesAreTaken runs the corpus's cases on 'now' at a set moment.
// They're in the subset, so none is refused, and each gives the answer
// SQLite gives at that moment, here worked out by hand.
// date_sqlite_test.go holds them to 0.x at the time.
func TestTheNowCasesAreTaken(t *testing.T) {
	cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", "dates.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]any{
		"now-01": {"2026-10-08"}, "now-02": {"2026-10-08 09:05:07"}, "now-03": {"2026-10-09"},
		"now-04": {"2026-10-07"}, "now-05": {"2026-11-07"}, "now-06": {"2026-11-08"}, "now-07": {"2026-09-08"},
		"now-08": {"2027-11-08"}, "now-09": {"2027-10-08"}, "now-10": {"2022-10-08"},
		"now-11": {"2026-09-09 09:05:07"}, "now-12": {"2028-10-05 09:05:07"}, "now-13": {int64(1)},
		"now-14": {"people:1", "people:2", "people:3", "people:4"},
	}
	people := []fakeRow{
		{"key": value.Text("people:1"), "joined": value.Text("2026-01-15")},
		{"key": value.Text("people:2"), "joined": value.Text("2025-12-31")},
		{"key": value.Text("people:3"), "joined": value.Text("2024-02-29")},
		{"key": value.Text("people:4"), "joined": value.Text("2026-10-07")},
	}
	now := at(2026, 10, 8, 9, 5, 7, 250)
	n := 0
	for _, cs := range cases {
		if cs.Kind != "now" {
			continue
		}
		n++
		ans := runAt(t, cs.SQL, now, people)
		var got []any
		for _, row := range ans.Rows {
			got = append(got, row[0].V)
		}
		if ans.Error != "" || fmt.Sprint(got) != fmt.Sprint(want[cs.ID]) {
			t.Errorf("%s: %s gives %+v, want %v", cs.ID, cs.SQL, ans, want[cs.ID])
		}
	}
	if n != 14 {
		t.Errorf("%d cases on 'now', and the corpus has 14", n)
	}
	// Earlier in the year, fewer people had joined.
	ans := runAt(t, "SELECT key FROM people WHERE joined <= date('now') ORDER BY key", at(2026, 1, 14, 0, 0, 0, 0), people)
	if len(ans.Rows) != 2 || ans.Rows[0][0].V != "people:2" || ans.Rows[1][0].V != "people:3" {
		t.Errorf("on 14 January 2026: %+v", ans)
	}
}

// TestDatesOutsideTheSubsetAreRefused checks the refusals: a first argument
// other than 'now', and a modifier outside SQL.md's pattern, refused as the
// expression compiles, with a message that says what isn't taken, at the
// argument it can't take. A date the compiler would leave out, such as one
// that can't change an AND, is refused all the same.
func TestDatesOutsideTheSubsetAreRefused(t *testing.T) {
	cases := []struct{ expr, msg, at string }{
		{"date('2026-10-07')", "date() on '2026-10-07' is outside the SQL subset: it takes 'now' and modifiers such as '+1 day'", "'2026-10-07'"},
		{"DateTime('2026-10-07 12:00', '+1 day')", "DateTime() on '2026-10-07 12:00' is outside the SQL subset", "'2026-10-07 12:00'"},
		{"date('now ')", "date() on 'now ' is outside", "'now '"},
		{"date(' now')", "date() on ' now' is outside", "' now'"},
		{"date('not now')", "date() on 'not now' is outside", "'not now'"},
		{"date('')", "date() on '' is outside", "''"},
		{"date('subsec')", "date() on 'subsec' is outside", "'subsec'"},
		{"date('now', '+1 fortnight')", "the modifier '+1 fortnight' in date() is outside the SQL subset: a modifier is a sign, a whole number, a space and days, months or years, such as '-3 months'", "'+1 fortnight'"},
		{"date('now', '+1.5 days')", "the modifier '+1.5 days' in date() is outside", "'+1.5 days'"},
		{"date('now', '1 day')", "the modifier '1 day' in date() is outside", "'1 day'"},
		{"date('now', '+ 1 day')", "the modifier '+ 1 day' in date() is outside", "'+ 1 day'"},
		{"date('now', '+1day')", "the modifier '+1day' in date() is outside", "'+1day'"},
		{"date('now', '+1 day ')", "the modifier '+1 day ' in date() is outside", "'+1 day '"},
		{"date('now', ' +1 day')", "the modifier ' +1 day' in date() is outside", "' +1 day'"},
		{"date('now', '+1 dayss')", "the modifier '+1 dayss' in date() is outside", "'+1 dayss'"},
		{"date('now', '+1 hour')", "the modifier '+1 hour' in date() is outside", "'+1 hour'"},
		{"date('now', '+1e2 days')", "the modifier '+1e2 days' in date() is outside", "'+1e2 days'"},
		{"date('now', '+01:00')", "the modifier '+01:00' in date() is outside", "'+01:00'"},
		{"date('now', '+0001-02-03')", "the modifier '+0001-02-03' in date() is outside", "'+0001-02-03'"},
		{"date('now', 'start of month')", "the modifier 'start of month' in date() is outside", "'start of month'"},
		{"date('now', 'localtime')", "the modifier 'localtime' in date() is outside", "'localtime'"},
		{"date('now', '')", "the modifier '' in date() is outside", "''"},
		{"datetime('now', '+1 day', 'weekday 0')", "the modifier 'weekday 0' in datetime() is outside", "'weekday 0'"},
		{"date('now', '+1 day', '+1 day', 'utc')", "the modifier 'utc' in date() is outside", "'utc'"},
		// Where the compiler would leave the date out.
		{"0 AND date('2026-10-07')", "date() on '2026-10-07' is outside", "'2026-10-07'"},
		{"date('2026-10-07') AND 0", "date() on '2026-10-07' is outside", "'2026-10-07'"},
		{"1 OR datetime('now', '+1 week')", "the modifier '+1 week' in datetime() is outside", "'+1 week'"},
		{"date('2026-10-07') IN ()", "date() on '2026-10-07' is outside", "'2026-10-07'"},
		{"1 IS (date('2026-10-07') NOT IN ())", "date() on '2026-10-07' is outside", "'2026-10-07'"},
		{"(SELECT n FROM docs WHERE key = date('2026-10-07'))", "date() on '2026-10-07' is outside", "'2026-10-07'"},
		{"'docs:1' IN (SELECT key FROM walk(date('2026-10-07'), 1))", "date() on '2026-10-07' is outside", "'2026-10-07'"},
		{"coalesce(1, date('now', '+1 week'))", "the modifier '+1 week' in date() is outside", "'+1 week'"},
		{"abs(date('2026-10-07', '+1 week'))", "date() on '2026-10-07' is outside", "'2026-10-07'"},
	}
	for _, cs := range cases {
		q := "SELECT " + cs.expr + " FROM docs"
		st, err := Parse(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		e := st.(*Select).Results[0].Expr
		_, err = Compile(e, &fakeScope{})
		var pe *Error
		if !errors.As(err, &pe) || !strings.HasPrefix(pe.Msg, cs.msg) || !outsideTheSubset(err) {
			t.Errorf("%s: got %v, want %q", cs.expr, err, cs.msg)
			continue
		}
		if where := strings.Index(q, cs.at); pe.Pos != where {
			t.Errorf("%s: the error is at %d, and %s at %d", cs.expr, pe.Pos, cs.at, where)
		}
		// A condition is refused the same way.
		if _, err := CompileCondition(e, &fakeScope{}); err == nil || err.Error() != pe.Msg {
			t.Errorf("%s as a condition: got %v, want %q", cs.expr, err, pe.Msg)
		}
	}
	// The parser refuses a date() on anything but text literals already, and
	// so does the compiler, given a tree it didn't make.
	call := &Call{Name: Ident{Name: "date"}, Args: []Expr{&Literal{Kind: LitInt, Text: "1"}}}
	if _, err := Compile(call, nil); !outsideTheSubset(err) {
		t.Errorf("date(1) from a tree made by hand: got %v", err)
	}
	if _, err := Compile(&Call{Name: Ident{Name: "datetime"}}, nil); !outsideTheSubset(err) {
		t.Errorf("datetime() from a tree made by hand: got %v", err)
	}
}

// TestTheModifiersOfTheSubset holds subsetModifier to SQL.md's pattern, and
// parseModifier to SQLite's reading of what it takes.
func TestTheModifiersOfTheSubset(t *testing.T) {
	const second, minute, hour, day, month, year = 0, 1, 2, 3, 4, 5
	bad := dateModifier{bad: true}
	cases := []struct {
		text string
		in   bool
		want dateModifier // as parseModifier reads it
	}{
		{"+1 day", true, dateModifier{r: 1, unit: day}}, {"-1 days", true, dateModifier{r: -1, unit: day}},
		{"+31 DAYS", true, dateModifier{r: 31, unit: day}}, {"+0 day", true, dateModifier{r: 0, unit: day}},
		{"+13 months", true, dateModifier{r: 13, unit: month}}, {"-1 Month", true, dateModifier{r: -1, unit: month}},
		{"+4 years", true, dateModifier{r: 4, unit: year}}, {"-400 YEAR", true, dateModifier{r: -400, unit: year}},
		{"+007 days", true, dateModifier{r: 7, unit: day}}, {"+1  day", true, dateModifier{r: 1, unit: day}},
		{"+1\t\n\v\f\rday", true, dateModifier{r: 1, unit: day}},
		{"+5373484 days", true, dateModifier{r: 5373484, unit: day}},
		{"+176545 months", true, dateModifier{r: 176545, unit: month}},
		{"-14712 years", true, dateModifier{r: -14712, unit: year}},
		// In the pattern, past SQLite's limits, and so NULL.
		{"+5373485 days", true, bad}, {"-176546 months", true, bad}, {"+14713 years", true, bad},
		{"+99999999999999999999999 days", true, bad},
		// Outside the pattern, as SQLite reads them below the refusal.
		{"1 day", false, dateModifier{r: 1, unit: day}}, {"+1.5 days", false, dateModifier{r: 1.5, unit: day}},
		{"-.25 months", false, dateModifier{r: -0.25, unit: month}}, {"+1e1 days", false, dateModifier{r: 10, unit: day}},
		{"+1 hour", false, dateModifier{r: 1, unit: hour}}, {"-90 minutes", false, dateModifier{r: -90, unit: minute}},
		{"+3600 seconds", false, dateModifier{r: 3600, unit: second}},
		{"+1 day\x00", false, dateModifier{r: 1, unit: day}},
		{"+1 fortnight", false, bad}, {"+ 1 day", false, bad}, {"+1day", false, bad}, {"+1 day ", false, bad},
		{" +1 day", false, bad}, {"+1 dayss", false, bad}, {"+1 da", false, bad}, {"+1", false, bad}, {"", false, bad},
		{"+", false, bad}, {"day", false, bad}, {"+1 dаy", false, bad}, {"+1 Kday", false, bad},
		{"+1 day", false, bad},
		// SQLite's other forms, which aren't ported.
		{"+01:00", false, bad}, {"+0001-02-03", false, bad}, {"start of month", false, bad},
	}
	for _, cs := range cases {
		if got := subsetModifier(cs.text); got != cs.in {
			t.Errorf("%q: in the subset %v, want %v", cs.text, got, cs.in)
		}
		if got := parseModifier(cs.text); got != cs.want {
			t.Errorf("%q reads as %+v, want %+v", cs.text, got, cs.want)
		}
	}
}

// TestTheCalendarIsGos holds the port's calendar to Go's time package, whose
// calendar is the same proleptic Gregorian one: computeYMD and computeHMS at
// moments across SQLite's whole range, and computeJD back from them.
func TestTheCalendarIsGos(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 0x5132))
	moments := []int64{0, 1, 43199999, 43200000, 86400000, maxJD, maxJD - 1, unixEpochJD, unixEpochJD - 1}
	for y := -4713; y <= 9999; y += 1 + r.IntN(40) {
		for _, d := range []time.Time{at(y, 1, 1, 0, 0, 0, 0), at(y, 2, 28, 23, 59, 59, 999), at(y, 3, 1, 0, 0, 0, 0), at(y, 12, 31, 12, 0, 0, 0)} {
			moments = append(moments, unixEpochJD+d.UnixMilli())
		}
	}
	n := 200000
	if testing.Short() {
		n = 20000
	}
	for i := 0; i < n; i++ {
		moments = append(moments, r.Int64N(maxJD+1))
	}
	bad := 0
	for _, jd := range moments {
		if !validJulianDay(jd) {
			continue
		}
		want := time.UnixMilli(jd - unixEpochJD).UTC()
		p := dateTime{iJD: jd, validJD: true}
		p.computeYMDHMS()
		ms := int(p.s*1000 + 0.5)
		q := dateTime{Y: p.Y, M: p.M, D: p.D, h: p.h, m: p.m, s: p.s, validYMD: true, validHMS: true}
		q.computeJD()
		if p.Y != want.Year() || p.M != int(want.Month()) || p.D != want.Day() || p.h != want.Hour() || p.m != want.Minute() ||
			ms != want.Second()*1000+want.Nanosecond()/1e6 || q.iJD != jd || p.isError || q.isError {
			bad++
			if bad <= 10 {
				t.Errorf("%d: %d-%d-%d %d:%d:%v and back %d, and Go has %s", jd, p.Y, p.M, p.D, p.h, p.m, p.s, q.iJD, want)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d moments differ", bad, len(moments))
	}
}

// TestNowComesFromTheFrame checks that a date reads its moment from the
// Frame alone, so one Eval gives each Frame's own date, on several
// goroutines at once, and that a Frame without a moment is an error. The
// zero time stands for no moment, and a millisecond later is a moment.
func TestNowComesFromTheFrame(t *testing.T) {
	st, err := Parse("SELECT datetime('now', '+1 month')")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Compile(st.(*Select).Results[0].Expr, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*Frame{{}, {Now: at(1, 1, 1, 0, 0, 0, 0)}} {
		_, err = ev(f)
		var pe *Error
		if !errors.As(err, &pe) || pe.Msg != "datetime('now') has no moment to read: the statement's Frame.Now isn't set" {
			t.Errorf("without a moment, at %s: got %v", f.Now, err)
		}
	}
	if v, err := ev(&Frame{Now: at(1, 1, 1, 0, 0, 0, 1)}); err != nil || v != value.Text("0001-02-01 00:00:00") {
		t.Errorf("a millisecond into year 1: got %s, %v", v, err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// 31 January and a month is 31 February, which runs on into March.
			now := at(2001+g, 1, 31, 6, 7, 8, 0)
			want := value.Text(fmt.Sprintf("%d-03-%02d 06:07:08", 2001+g, 31-monthDays(2001+g, 2)))
			for i := 0; i < 200; i++ {
				if v, err := ev(&Frame{Now: now}); err != nil || v != want {
					t.Errorf("at %s: got %s, %v, want %s", now, v, err, want)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// momentText writes a moment as a date of its own that SQLite reads back to
// the same moment, to the millisecond.
func momentText(t time.Time) string {
	t = t.UTC()
	y, sign := t.Year(), ""
	if y < 0 {
		y, sign = -y, "-"
	}
	return fmt.Sprintf("%s%04d-%02d-%02d %02d:%02d:%02d.%03d", sign, y, t.Month(), t.Day(), t.Hour(), t.Minute(),
		t.Second(), t.Nanosecond()/1e6)
}

var nowArgument = regexp.MustCompile(`(?i)\('now'`)

// fixedAt writes a date on 'now' as the same date on the moment now written
// out, for 0.x.
func fixedAt(expr string, now time.Time) string {
	return nowArgument.ReplaceAllLiteralString(expr, "('"+momentText(now)+"'")
}

func TestMomentTextReadsBack(t *testing.T) {
	for _, now := range []time.Time{at(2026, 10, 8, 9, 5, 7, 250), at(-4713, 11, 24, 12, 0, 0, 1), at(0, 2, 29, 0, 0, 0, 0),
		at(9999, 12, 31, 23, 59, 59, 999), time.Date(2026, 10, 8, 2, 30, 0, 999999999, time.FixedZone("UTC+3", 3*3600))} {
		p := parseFixedDate(momentText(now))
		p.computeJD()
		if want := dateNow(now); p.iJD != want.iJD || p.isError {
			t.Errorf("%s writes as %s, which reads as %d, want %d", now, momentText(now), p.iJD, want.iJD)
		}
	}
	if got := fixedAt("datetime('NOW', '+1 day')", at(2026, 1, 2, 3, 4, 5, 6)); got != "datetime('2026-01-02 03:04:05.006', '+1 day')" {
		t.Errorf("fixedAt gives %s", got)
	}
}
