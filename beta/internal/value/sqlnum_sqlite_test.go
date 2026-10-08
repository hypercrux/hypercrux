// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && cgo

package value

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// The routines of sqlnum.go against SQLite itself, through go-sqlite3: the
// fixed cases of sqlnum_test.go, then random reals and random text that
// reads as a number, or partly. These tests need cgo.

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// sqliteReal runs a query that gives a real.
func sqliteReal(t *testing.T, db *sql.DB, q string, args ...any) float64 {
	t.Helper()
	var r float64
	if err := db.QueryRow(q, args...).Scan(&r); err != nil {
		t.Fatalf("%s %v: %v", q, args, err)
	}
	return r
}

func TestFixedCasesAsSQLiteHasThem(t *testing.T) {
	db := openSQLite(t)
	defer db.Close()
	for _, c := range realTexts {
		var want string
		if err := db.QueryRow("SELECT CAST(? AS TEXT)", c.r).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if want != c.text {
			t.Errorf("SQLite writes %v as %q, and the table has %q", c.r, want, c.text)
		}
	}
	for _, c := range realPlaces {
		want := sqliteReal(t, db, "SELECT round(?, ?)", c.r, c.places)
		if got, _ := ParseReal(c.text); math.Float64bits(got) != math.Float64bits(want) {
			t.Errorf("SQLite's round(%v, %d) is %v, and the table's text reads as %v", c.r, c.places, want, got)
		}
	}
	for _, c := range parsedReals {
		if want := sqliteReal(t, db, "SELECT CAST(? AS REAL)", c.text); math.Float64bits(want) != math.Float64bits(c.r) {
			t.Errorf("SQLite reads %q as %v, and the table has %v", c.text, want, c.r)
		}
	}
	for _, c := range parsedInts {
		var want int64
		if err := db.QueryRow("SELECT CAST(? AS INTEGER)", c.text).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if want != c.i {
			t.Errorf("SQLite reads %q as %d, and the table has %d", c.text, want, c.i)
		}
	}
}

// randomReal makes a real of any kind: any bits, a decimal of a few
// digits, a subnormal, or a whole number scaled by a power of two.
func randomReal(r *rand.Rand) float64 {
	switch r.IntN(6) {
	case 0:
		return math.Float64frombits(r.Uint64())
	case 1:
		return r.Float64() * math.Pow(10, float64(r.IntN(40)-20))
	case 2:
		return float64(r.Int64N(1<<53)) / math.Pow(10, float64(r.IntN(20)))
	case 3:
		return math.Float64frombits(r.Uint64() & 0x000fffffffffffff)
	case 4:
		return float64(r.Int64()) * math.Pow(2, float64(r.IntN(200)-100))
	}
	v, _ := strconv.ParseFloat(fmt.Sprintf("%.*g", 1+r.IntN(17), r.NormFloat64()*math.Pow(10, float64(r.IntN(30)-15))), 64)
	return v
}

// numberText makes text that reads as a number, or partly: digits, points
// and exponents, more digits than 64 bits hold, signs, spaces, and
// something after the number, a NUL byte among them.
func numberText(r *rand.Rand) string {
	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + r.IntN(10))
		}
		return string(b)
	}
	sign := func() string { return []string{"", "+", "-"}[r.IntN(3)] }
	var s string
	switch r.IntN(5) {
	case 0:
		s = digits(1 + r.IntN(25))
	case 1:
		s = digits(r.IntN(20)) + "." + digits(r.IntN(25))
	case 2:
		s = digits(1+r.IntN(20)) + "e" + sign() + digits(1+r.IntN(4))
	case 3:
		s = digits(r.IntN(5)) + "." + digits(1+r.IntN(30)) + "E" + sign() + digits(1+r.IntN(3))
	default:
		s = strconv.FormatFloat(randomReal(r), 'g', -1, 64)
	}
	switch r.IntN(8) {
	case 0:
		s = "-" + s
	case 1:
		s = "+" + s
	case 2:
		s = "  " + s + " "
	case 3:
		s += "x"
	case 4:
		s += "\x009"
	}
	return s
}

func TestRealTextAsSQLiteWritesIt(t *testing.T) {
	db := openSQLite(t)
	defer db.Close()
	r := rand.New(rand.NewPCG(1, 2))
	n := 40000
	if testing.Short() {
		n = 5000
	}
	bad := 0
	for i := 0; i < n; i++ {
		f := randomReal(r)
		if math.IsNaN(f) {
			continue
		}
		var want string
		if err := db.QueryRow("SELECT CAST(? AS TEXT)", f).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if got := RealText(f); got != want {
			bad++
			if bad <= 20 {
				t.Errorf("RealText(%v) (%016x) = %q, and SQLite writes %q", f, math.Float64bits(f), got, want)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, n)
	}
}

func TestParseAsSQLiteReads(t *testing.T) {
	db := openSQLite(t)
	defer db.Close()
	r := rand.New(rand.NewPCG(3, 4))
	n := 40000
	if testing.Short() {
		n = 5000
	}
	bad := 0
	for i := 0; i < n; i++ {
		s := numberText(r)
		want := sqliteReal(t, db, "SELECT CAST(? AS REAL)", s)
		if got, _ := ParseReal(s); math.Float64bits(got) != math.Float64bits(want) {
			bad++
			if bad <= 20 {
				t.Errorf("ParseReal(%q) = %v, and SQLite reads %v", s, got, want)
			}
		}
		var wantInt int64
		if err := db.QueryRow("SELECT CAST(? AS INTEGER)", s).Scan(&wantInt); err != nil {
			t.Fatal(err)
		}
		if got, _ := ParseInt(s); got != wantInt {
			bad++
			if bad <= 20 {
				t.Errorf("ParseInt(%q) = %d, and SQLite reads %d", s, got, wantInt)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, 2*n)
	}
}

func TestRealPlacesAsRoundReadsThem(t *testing.T) {
	db := openSQLite(t)
	defer db.Close()
	r := rand.New(rand.NewPCG(5, 6))
	n := 20000
	if testing.Short() {
		n = 3000
	}
	bad := 0
	for i := 0; i < n; i++ {
		f := randomReal(r)
		if math.IsNaN(f) || math.Abs(f) > 4503599627370496.0 {
			continue // round() leaves a real more than 2^52 in size as it is
		}
		places := 1 + r.IntN(30)
		want := sqliteReal(t, db, "SELECT round(?, ?)", f, places)
		if got, _ := ParseReal(RealPlaces(f, places)); math.Float64bits(got) != math.Float64bits(want) {
			bad++
			if bad <= 20 {
				t.Errorf("round(%v, %d): RealPlaces gives %q, and SQLite %v", f, places, RealPlaces(f, places), want)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, n)
	}
}
