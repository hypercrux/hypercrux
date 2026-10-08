// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package value

import (
	"math"
	"testing"
)

// SQLite's numbers as text and text as numbers, on fixed cases.
// sqlnum_sqlite_test.go, built with cgo, holds the same cases to SQLite
// itself, and random ones besides.

var (
	negZero  = math.Copysign(0, -1)
	pointOne = 0.1
)

// realTexts are reals and their text, as CAST(r AS TEXT) gives it.
var realTexts = []struct {
	r    float64
	text string
}{
	{0, "0.0"}, {negZero, "0.0"}, {math.Inf(1), "Inf"}, {math.Inf(-1), "-Inf"},
	{5e-324, "4.9406564584124654e-324"}, {math.MaxFloat64, "1.7976931348623157e+308"},
	{-math.MaxFloat64, "-1.7976931348623157e+308"}, {1e21, "1.0e+21"}, {1e-7, "1.0e-07"}, {1e-5, "1.0e-05"},
	{1e-4, "0.0001"}, {0.1, "0.1"}, {pointOne + 0.2, "0.30000000000000004"}, {100, "100.0"},
	{1e16, "10000000000000000.0"}, {1e17, "1.0e+17"}, {49.47, "49.47"},
	{2.2250738585072014e-308, "2.2250738585072014e-308"}, {9007199254740993, "9007199254740992.0"},
	{123456789012345678, "1.2345678901234568e+17"}, {1.5, "1.5"}, {-2.25, "-2.25"}, {1e100, "1.0e+100"},
	{123456789.125, "123456789.125"}, {3.0000000000000004, "3.0000000000000004"},
}

func TestRealText(t *testing.T) {
	for _, c := range realTexts {
		if got := RealText(c.r); got != c.text {
			t.Errorf("RealText(%v) = %q, want %q", c.r, got, c.text)
		}
		if math.IsInf(c.r, 0) {
			continue
		}
		if back, _ := ParseReal(RealText(c.r)); back != c.r {
			t.Errorf("ParseReal(RealText(%v)) = %v", c.r, back)
		}
	}
}

// realPlaces are reals, places and the text "%!.*f" writes, which round()
// reads back.
var realPlaces = []struct {
	r      float64
	places int
	text   string
}{
	{2.675, 2, "2.67"}, {2.5, 0, "3.0"}, {2.5, 3, "2.5"}, {1.15, 1, "1.1"}, {0.000001234, 7, "0.0000012"},
	{123.456, 30, "123.4560000000000031"}, {-1.5, 0, "-2.0"}, {1e20, 2, "100000000000000000000.0"},
	{0.5, 0, "1.0"}, {1.005, 2, "1.0"}, {negZero, 1, "0.0"}, {9.995, 2, "9.99"},
}

func TestRealPlaces(t *testing.T) {
	for _, c := range realPlaces {
		if got := RealPlaces(c.r, c.places); got != c.text {
			t.Errorf("RealPlaces(%v, %d) = %q, want %q", c.r, c.places, got, c.text)
		}
	}
}

// parsedReals are texts, the real SQLite reads from them, and whether all
// of the text reads.
var parsedReals = []struct {
	text string
	r    float64
	all  bool
}{
	{"1.5", 1.5, true}, {"  1.5  ", 1.5, true}, {"1.5x", 1.5, false}, {"12", 12, true}, {"-0", negZero, true},
	{"0.0", 0, true}, {"1e999", math.Inf(1), true}, {"1e-400", 0, true}, {"", 0, false}, {"  ", 0, false},
	{"x", 0, false}, {".", 0, false}, {"-", 0, false}, {"+.5", 0.5, true}, {"5.", 5, true}, {"1e", 1, false},
	{"1e+", 1, false}, {"3500000000000000.2500001", 3500000000000000, true},
	{"123456789012345678901234567890", 1.2345678901234568e+29, true}, {"0.000", 0, true},
	{"1.5\x009", 1.5, true}, {"\t-2.5e-3\n", -0.0025, true}, {"0x10", 0, false}, {"1e5e5", 100000, false},
	{"00000000000000000000001.5", 1.5, true},
}

func TestParseReal(t *testing.T) {
	for _, c := range parsedReals {
		r, rc := ParseReal(c.text)
		if math.Float64bits(r) != math.Float64bits(c.r) || (rc > 0) != c.all {
			t.Errorf("ParseReal(%q) = %v, %d; want %v, all of it read %v", c.text, r, rc, c.r, c.all)
		}
	}
	// The bits of the result.
	for _, c := range []struct {
		text string
		rc   int
	}{
		{"12", RealPrefix}, {"1.5", RealPrefix | RealPoint}, {"0.0", RealPrefix | RealPoint | RealZero},
		{"-0", RealPrefix | RealZero}, {"1e-400", RealPrefix | RealPoint},
		{"3500000000000000.2500001", RealPrefix | RealPoint | RealLong}, {"x", 0},
	} {
		if _, rc := ParseReal(c.text); rc != c.rc {
			t.Errorf("ParseReal(%q) gives %d, want %d", c.text, rc, c.rc)
		}
	}
}

// parsedInts are texts, the whole number SQLite reads from them, and
// sqlite3Atoi64's result.
var parsedInts = []struct {
	text string
	i    int64
	rc   int
}{
	{"12", 12, 0}, {" 12 ", 12, 0}, {"12x", 12, 1}, {"-12", -12, 0}, {"+12", 12, 0},
	{"9223372036854775807", math.MaxInt64, 0}, {"9223372036854775808", math.MaxInt64, 3},
	{"-9223372036854775808", math.MinInt64, 0}, {"-9223372036854775809", math.MinInt64, 2},
	{"99999999999999999999", math.MaxInt64, 2}, {"", 0, -1}, {"x", 0, -1}, {"-", 0, -1}, {"1e3", 1, 1},
	{"0012", 12, 0}, {"12\x00", 12, 1}, {"  -0", 0, 0},
}

func TestParseInt(t *testing.T) {
	for _, c := range parsedInts {
		if i, rc := ParseInt(c.text); i != c.i || rc != c.rc {
			t.Errorf("ParseInt(%q) = %d, %d; want %d, %d", c.text, i, rc, c.i, c.rc)
		}
	}
}
