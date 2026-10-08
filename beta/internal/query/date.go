// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"strings"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Dates (Q2): date('now' [, m ...]) and datetime('now' [, m ...]), as
// SQL.md's "Dates" has them. The arithmetic is SQLite 3.53.4's, from the
// date code in go-sqlite3's sqlite3-binding.c, ported step for step:
// setDateTimeToCurrent for 'now', the case of parseModifier that reads a
// number of days, months or years, computeJD, computeYMD and computeHMS,
// the end of isDate, and dateFunc and datetimeFunc for the text.
//
// 'now' is Frame.Now, the statement's moment, which whoever runs the
// statement reads once before its first row. A first argument other than
// 'now', and a modifier outside SQL.md's pattern, are refused as the
// expression compiles, wherever they are in it, so the statement is refused
// before anything runs. Below the refusal the arithmetic reads a modifier as
// SQLite does, which lets the tests run the corpus's dates on fixed days
// through it, odd modifiers included.

// dateTime is SQLite's DateTime, with the parts the subset uses: the moment,
// the date and the time of day, each valid or not, and each worked out from
// another when it's wanted. It has no zone, which 'now' never has, no raw
// number, which only a number as the first argument gives, and no nFloor,
// which only the modifier 'floor' reads.
type dateTime struct {
	iJD     int64 // the moment, in milliseconds of the Julian day
	Y, M, D int
	h, m    int
	s       float64 // the seconds, with their fraction

	validJD, validYMD, validHMS bool
	// isError is datetimeError's mark: a year moved outside -4713 to 9999,
	// or a moment outside SQLite's range when its date was wanted. The
	// answer is then NULL.
	isError bool
}

const (
	// unixEpochJD is 1970-01-01 00:00:00 in milliseconds of the Julian day,
	// as unixCurrentTimeInt64 has it.
	unixEpochJD = 24405875 * 8640000
	// maxJD is 9999-12-31 23:59:59.999, the last moment SQLite takes. The
	// first is 0, noon on 24 November 4714 BC.
	maxJD = 464269060799999
)

// validJulianDay is validJulianDay: whether the moment is inside SQLite's
// range.
func validJulianDay(iJD int64) bool { return iJD >= 0 && iJD <= maxJD }

// fail is datetimeError.
func (p *dateTime) fail() { *p = dateTime{isError: true} }

// dateNow is setDateTimeToCurrent, with the statement's moment in place of
// SQLite's clock, which counts whole milliseconds, as unixCurrentTimeInt64
// reads it. The moment is the same in every zone, so the zone of now doesn't
// count. SQLite gives NULL for a clock at or before the start of its range.
func dateNow(now time.Time) dateTime {
	ms := now.UnixMilli()
	if plant == "query/date-now-in-its-zone" {
		_, offset := now.Zone()
		ms += int64(offset) * 1000
	}
	if unixEpochJD+ms <= 0 {
		return dateTime{isError: true}
	}
	return dateTime{iJD: unixEpochJD + ms, validJD: true}
}

// computeJD is computeJD: the moment from the date and the time of day, with
// 2000-01-01 when the date isn't valid, by Meeus's rule for the Gregorian
// calendar. The day isn't held to its month, so a day past the end of its
// month runs on into the next. A year outside -4713 to 9999 is an error.
func (p *dateTime) computeJD() {
	if p.validJD {
		return
	}
	y, mo, d := 2000, 1, 1
	if p.validYMD {
		y, mo, d = p.Y, p.M, p.D
	}
	if y < -4713 || y > 9999 {
		p.fail()
		return
	}
	if mo <= 2 {
		y--
		mo += 12
	}
	a := (y + 4800) / 100
	b := 38 - a + a/4
	x1 := 36525 * (y + 4716) / 100
	x2 := 306001 * (mo + 1) / 10000
	p.iJD = int64((float64(x1+x2+d+b) - 1524.5) * 86400000)
	p.validJD = true
	if p.validHMS {
		// float64() rounds the product, as C does, so it can't fuse with the
		// sum.
		p.iJD += int64(p.h*3600000+p.m*60000) + int64(float64(p.s*1000)+0.5)
	}
}

// computeYMD is computeYMD: the date from the moment, or 2000-01-01 when
// neither is valid. A moment outside SQLite's range is an error.
func (p *dateTime) computeYMD() {
	if p.validYMD {
		return
	}
	switch {
	case !p.validJD:
		p.Y, p.M, p.D = 2000, 1, 1
	case !validJulianDay(p.iJD):
		p.fail()
		return
	default:
		z := int((p.iJD + 43200000) / 86400000)
		alpha := int((float64(z)+32044.75)/36524.25) - 52
		a := z + 1 + alpha - (alpha+100)/4 + 25
		b := a + 1524
		c := int((float64(b) - 122.1) / 365.25)
		d := 36525 * (c & 32767) / 100
		e := int(float64(b-d) / 30.6001)
		x1 := int(30.6001 * float64(e))
		p.D = b - d - x1
		p.M = e - 1
		if e >= 14 {
			p.M = e - 13
		}
		p.Y = c - 4715
		if p.M > 2 {
			p.Y = c - 4716
		}
	}
	p.validYMD = true
}

// computeHMS is computeHMS: the time of day from the moment.
func (p *dateTime) computeHMS() {
	if p.validHMS {
		return
	}
	p.computeJD()
	dayMs := int((p.iJD + 43200000) % 86400000)
	p.s = float64(dayMs%60000) / 1000.0
	dayMin := dayMs / 60000
	p.m = dayMin % 60
	p.h = dayMin / 60
	p.validHMS = true
}

// computeYMDHMS is computeYMD_HMS.
func (p *dateTime) computeYMDHMS() {
	p.computeYMD()
	p.computeHMS()
}

// clearYMDHMS is clearYMD_HMS_TZ: only the moment stays valid.
func (p *dateTime) clearYMDHMS() { p.validYMD, p.validHMS = false, false }

// dateUnits is SQLite's aXformType: the units of a modifier "NNN units",
// each with the size of NNN it takes, which SQLite keeps as a float, and
// its length in seconds. Months and years move the month or the year, and
// only a fraction of one counts in seconds.
var dateUnits = [...]struct {
	name    string
	limit   float32
	seconds float32
}{
	{"second", 4.6427e+14, 1.0},
	{"minute", 7.7379e+12, 60.0},
	{"hour", 1.2897e+11, 3600.0},
	{"day", 5373485.0, 86400.0},
	{"month", 176546.0, 2592000.0},
	{"year", 14713.0, 31536000.0},
}

const (
	unitMonth = 4
	unitYear  = 5
)

// dateModifier is a modifier as parseModifier reads it: the number and its
// unit, an index into dateUnits, or bad where SQLite gives NULL.
type dateModifier struct {
	r    float64
	unit int
	bad  bool
}

// parseModifier reads a modifier as parseModifier reads the form "NNN
// units". NNN runs to the first space or colon, and reads as sqlite3AtoF
// reads a number, so it may have a fraction and needn't have a sign. Then
// come any spaces and a unit of dateUnits, in any case and with or without an
// s, and NNN must be inside the unit's limit. Where SQLite gives NULL for the
// modifier, it's bad.
//
// SQLite's other forms of modifier, such as '+0001-02-03', '+01:30' and
// 'start of month', aren't ported, and are bad here too. The subset leaves
// them out and the refusal keeps them from reaching this, so only the
// corpus's odd modifiers in the tests can be outside the subset's pattern,
// and each of those is of this form.
func parseModifier(z string) dateModifier {
	bad := dateModifier{bad: true}
	if i := strings.IndexByte(z, 0); i >= 0 {
		z = z[:i] // SQLite reads the modifier as a C string
	}
	if z == "" || z[0] != '+' && z[0] != '-' && !isDigit(z[0]) {
		return bad
	}
	n := 1
	for ; n < len(z); n++ {
		if z[n] == ':' || isSpace(z[n]) {
			break
		}
		if z[n] == '-' && (n == 5 && yearDigits(z[1:], 4) || n == 6 && yearDigits(z[1:], 5)) {
			break
		}
	}
	r, res := value.ParseReal(z[:n])
	if res <= 0 || n < len(z) && (z[n] == '-' || z[n] == ':') {
		return bad
	}
	u := z[n:]
	for len(u) > 0 && isSpace(u[0]) {
		u = u[1:]
	}
	if len(u) < 3 || len(u) > 10 {
		return bad
	}
	if c := u[len(u)-1]; c == 's' || c == 'S' {
		u = u[:len(u)-1]
	}
	u = fold(u)
	for i, d := range dateUnits {
		limit := float64(d.limit)
		inside := r > -limit && r < limit
		if plant == "query/date-limit-inclusive" {
			inside = r >= -limit && r <= limit
		}
		if d.name == u && inside {
			return dateModifier{r: r, unit: i}
		}
	}
	return bad
}

// yearDigits is getDigits with the format "40f" or "50f": the first n bytes
// of s are digits, and make at most 14712.
func yearDigits(s string, n int) bool {
	if len(s) < n {
		return false
	}
	v := 0
	for i := 0; i < n; i++ {
		if !isDigit(s[i]) {
			return false
		}
		v = v*10 + int(s[i]-'0')
	}
	return v <= 14712
}

// modify moves p by m, as parseModifier does once it has read "NNN units".
// Days and the units below them add NNN of their length, rounded to the
// nearest millisecond, halves away from zero. Months and years move the
// month or the year and keep the day and the time, and computeJD lets a day
// past the end of its month run on into the next. A fraction of a month or
// a year adds its length in seconds on top.
func (p *dateTime) modify(m dateModifier) {
	p.computeJD()
	rounder := 0.5
	if m.r < 0 {
		rounder = -0.5
	}
	r := m.r
	switch m.unit {
	case unitMonth:
		p.computeYMDHMS()
		p.M += int(r)
		x := (p.M - 12) / 12
		if p.M > 0 {
			x = (p.M - 1) / 12
		}
		p.Y += x
		p.M -= x * 12
		p.validJD = false
		r -= float64(int(r))
	case unitYear:
		y := int(r)
		p.computeYMDHMS()
		p.Y += y
		p.validJD = false
		r -= float64(int(r))
	}
	if plant == "query/date-month-end-clamps" && (m.unit == unitMonth || m.unit == unitYear) && p.M >= 1 && p.M <= 12 {
		p.D = min(p.D, monthDays(p.Y, p.M))
	}
	p.computeJD()
	p.iJD += int64(float64(r*1000*float64(dateUnits[m.unit].seconds)) + rounder)
	p.clearYMDHMS()
}

// monthDays is the length of a month, for the planted bug
// query/date-month-end-clamps.
func monthDays(y, m int) int {
	switch m {
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// dateValue is isDate from its modifiers on, then dateFunc, or datetimeFunc
// when withTime is set: p moved by each modifier in turn, and then its date,
// or its date and time, as text. It's NULL where SQLite gives NULL: for a
// bad modifier, a year moved outside -4713 to 9999, and a moment outside
// SQLite's range at the end. A run of days may leave the range and come
// back, since only months and years look at the date on the way.
func dateValue(p dateTime, mods []dateModifier, withTime bool) value.Value {
	order := mods
	if plant == "query/date-modifiers-reversed" {
		order = make([]dateModifier, len(mods))
		for i, m := range mods {
			order[len(mods)-1-i] = m
		}
	}
	for _, m := range order {
		if m.bad || p.isError {
			return null
		}
		p.modify(m)
	}
	p.computeJD()
	if p.isError || !validJulianDay(p.iJD) {
		return null
	}
	if len(mods) == 0 && p.validYMD && p.D > 28 {
		// isDate's normalising of a date of its own with no modifiers, such
		// as 2023-02-31, which 'now' never needs.
		p.validYMD = false
	}
	if withTime {
		return value.Text(p.datetimeText())
	}
	return value.Text(p.dateText())
}

// dateText is dateFunc's text: YYYY-MM-DD, with a minus sign in front of a
// year below 0.
func (p *dateTime) dateText() string {
	p.computeYMD()
	return string(p.appendYMD(make([]byte, 0, 11)))
}

// datetimeText is datetimeFunc's text: YYYY-MM-DD HH:MM:SS, with the
// fraction of the seconds cut off.
func (p *dateTime) datetimeText() string {
	p.computeYMDHMS()
	b := p.appendYMD(make([]byte, 0, 20))
	s := int(p.s)
	if plant == "query/date-seconds-rounded" {
		s = int(p.s + 0.5)
	}
	return string(append(b, ' ', digit(p.h/10), digit(p.h), ':', digit(p.m/10), digit(p.m), ':', digit(s/10), digit(s)))
}

func (p *dateTime) appendYMD(b []byte) []byte {
	y := p.Y
	if y < 0 {
		b = append(b, '-')
		y = -y
	}
	return append(b, digit(y/1000), digit(y/100), digit(y/10), digit(y), '-', digit(p.M/10), digit(p.M), '-',
		digit(p.D/10), digit(p.D))
}

// digit is the last decimal digit of n, which isn't negative.
func digit(n int) byte { return '0' + byte(n%10) }

// isDateCall reports whether c is date() or datetime().
func isDateCall(c *Call) bool {
	f := c.Func()
	return f == "date" || f == "datetime"
}

// isNow reports whether s is 'now' in any case, as sqlite3StrICmp compares
// it: the three letters and nothing around them.
func isNow(s string) bool { return len(s) == 3 && fold(s) == "now" }

// subsetModifier reports whether z is a modifier SQL.md's "Dates" takes: a
// plus or minus sign, a whole number, one or more spaces, and day, days,
// month, months, year or years, in any case. A space is any byte
// sqlite3Isspace takes, as parseModifier reads one: the space, tab, line
// feed, vertical tab, form feed and carriage return.
func subsetModifier(z string) bool {
	if z == "" || z[0] != '+' && z[0] != '-' {
		return false
	}
	i := 1
	for i < len(z) && isDigit(z[i]) {
		i++
	}
	j := i
	for j < len(z) && isSpace(z[j]) {
		j++
	}
	if i == 1 || j == i {
		return false
	}
	switch fold(z[j:]) {
	case "day", "days", "month", "months", "year", "years":
		return true
	}
	return false
}

// dateModifiers checks a call of date() or datetime() against SQL.md's
// "Dates" and reads its modifiers. The arguments are text literals, the
// first is 'now', and each modifier is in the subset's pattern, or the call
// is refused as outside the subset, at the argument it can't take. The
// parser has checked the literals already, and checks them again here only
// for a tree it didn't make.
func dateModifiers(e *Call) ([]dateModifier, error) {
	name := e.Name.Name
	const takes = "it takes 'now' and modifiers such as '+1 day'"
	if len(e.Args) == 0 {
		return nil, fail(e, "%s() without an argument is outside the SQL subset: write %s('now')", name, e.Func())
	}
	mods := make([]dateModifier, 0, len(e.Args)-1)
	for i, a := range e.Args {
		l, ok := a.(*Literal)
		switch {
		case !ok || l.Kind != LitText:
			return nil, fail(a, "%s() on anything but text literals is outside the SQL subset: %s", name, takes)
		case i == 0 && !isNow(l.Data):
			return nil, fail(a, "%s() on %s is outside the SQL subset: %s", name, l.Text, takes)
		case i > 0 && !subsetModifier(l.Data):
			return nil, fail(a, "the modifier %s in %s() is outside the SQL subset: a modifier is a sign, "+
				"a whole number, a space and days, months or years, such as '-3 months'", l.Text, name)
		case i > 0:
			mods = append(mods, parseModifier(l.Data))
		}
	}
	return mods, nil
}

// checkDates refuses each call of date() or datetime() in e that the subset
// doesn't take, inside subqueries too. Compile runs it first, since the
// compiler leaves out parts that can't change the answer, such as x in
// 0 AND x, and a date outside the subset there would otherwise give an
// answer.
func checkDates(e Expr) error {
	if e == nil || plant == "query/date-checked-as-compiled" {
		return nil
	}
	var parts []Expr
	switch e := e.(type) {
	case *Call:
		if isDateCall(e) {
			if _, err := dateModifiers(e); err != nil {
				return err
			}
		}
		parts = e.Args
	case *Cast:
		parts = []Expr{e.X}
	case *Unary:
		parts = []Expr{e.X}
	case *Binary:
		parts = []Expr{e.L, e.R}
	case *Between:
		parts = []Expr{e.X, e.Low, e.High}
	case *Like:
		parts = []Expr{e.X, e.Pattern, e.Escape}
	case *In:
		parts = append([]Expr{e.X}, e.List...)
		if e.Walk != nil {
			parts = append(parts, e.Walk.Args...)
		}
	case *Record:
		parts = []Expr{e.Key}
	}
	for _, x := range parts {
		if err := checkDates(x); err != nil {
			return err
		}
	}
	return nil
}

// compileDate compiles date() or datetime(): the statement's moment, moved
// by each modifier in turn. A Frame without a moment is an error, so a
// statement run without one can't give the first day of year 1.
func compileDate(e *Call) (Eval, error) {
	mods, err := dateModifiers(e)
	if err != nil {
		return nil, err
	}
	withTime := e.Func() == "datetime"
	return func(f *Frame) (value.Value, error) {
		now := f.Now
		if plant == "query/date-now-per-call" {
			now = time.Now()
		}
		if now.IsZero() {
			return null, fail(e, "%s('now') has no moment to read: the statement's Frame.Now isn't set", e.Name.Name)
		}
		return dateValue(dateNow(now), mods, withTime), nil
	}, nil
}
