// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hypercrux/hypercrux/beta/internal/rules"
	val "github.com/hypercrux/hypercrux/beta/internal/value"
)

// The codec checks names and values in rules.go, with its own code and its
// own words, by FORMAT.md. The store and the public package check them with
// beta/internal/rules, by 0.x's rules, with 0.x's words. FORMAT.md's rules
// are 0.x's, so the two must refuse exactly the same things: if the store
// let through something the codec refuses, a commit would fail at
// AppendBatch with the codec's words; if the codec refused something the
// store takes, a file that the store wrote would read as damage.
// TestTheCodecRefusesWhatTheRulesRefuse holds them together. A change to
// either that makes them part fails it, and then FORMAT.md and its version
// have to change with the codec, since what a file may hold has changed.
//
// It goes through AppendBatch, which checks each change as DecodeBatch does
// (F1's tests hold the two to refusing the same changes).

// refuses reports whether AppendBatch refuses a batch holding c alone.
func refuses(c Change) bool {
	_, _, err := AppendBatch(nil, 1, 1, []Change{c})
	return err != nil
}

// awkward gives strings near the rules' edges: names that are nearly
// table or field names, reserved words in mixed case, the prefixes 0.x
// keeps for itself, zero bytes, bytes that aren't UTF-8, letters outside
// ASCII, the Kelvin sign that folds to k elsewhere, and lengths at each
// limit and either side of it.
type awkward struct{ r *rand.Rand }

var pieces = []string{
	"a", "z", "A", "Z", "m", "0", "9", "_", ":", "-", " ", ".", "/", "\x00", "\xff", "\xc0\x80", "\xed\xa0\x80",
	"é", "ü", "😀", "\u212a", "\U0010ffff", "\xf4\x90\x80\x80", "\ufffd", "hc_", "sqlite_", "key", "KEY", "Key",
	"rowid", "ROWID", "RowId", "oid", "OID", "_rowid_", "_ROWID_", "vec", "VEC", "Vec", "docs", "t_1",
}

// piece returns a short string built from pieces.
func (g awkward) piece() string {
	var b strings.Builder
	for n := 1 + g.r.IntN(4); n > 0; n-- {
		b.WriteString(pieces[g.r.IntN(len(pieces))])
	}
	return b.String()
}

// sized returns a string of about n bytes: a piece, padded with one
// letter, or cut.
func (g awkward) sized(n int) string {
	s := g.piece()
	pad := []string{"a", "Z", "_", "9", "ü"}[g.r.IntN(5)]
	for len(s) < n {
		s += pad
	}
	if len(s) > n && g.r.IntN(2) == 0 {
		s = s[:n]
	}
	return s
}

// name returns a string that could be a table or field name, or nearly.
func (g awkward) name() string {
	switch g.r.IntN(6) {
	case 0:
		return g.sized([]int{0, 1, 2, 62, 63, 64, 65, 66}[g.r.IntN(8)])
	case 1:
		return ""
	}
	s := g.piece()
	if g.r.IntN(3) == 0 {
		s = strings.ToLower(s)
	}
	return s
}

// key returns a string that could be a key, or nearly.
func (g awkward) key() string {
	switch g.r.IntN(8) {
	case 0:
		return g.name() + ":" + g.sized([]int{1, 900, 1020, 1023, 1024, 1025, 1030}[g.r.IntN(7)])
	case 1:
		return g.name()
	case 2:
		return g.name() + ":"
	case 3:
		return ":" + g.piece()
	}
	tbl := []string{"docs", "t_1", "a", "hc", strings.ToLower(g.piece())}[g.r.IntN(5)]
	return tbl + ":" + g.piece()
}

// linkType returns a string that could be a link type, or nearly.
func (g awkward) linkType() string {
	if g.r.IntN(3) == 0 {
		n := []int{0, 1, 199, 200, 201, 250}[g.r.IntN(6)]
		r := []string{"a", "ü", "😀", "\u212a", "\x00"}[g.r.IntN(5)]
		s := strings.Repeat(r, n)
		if g.r.IntN(4) == 0 {
			s = g.piece() + s
		}
		return s
	}
	return g.piece()
}

// float32s returns a vector near the rules' edges: empty, at and past the
// largest size, all zeros with -0 among them, holding NaN, infinities,
// subnormals and the extremes.
func (g awkward) float32s() []float32 {
	n := []int{0, 1, 2, 3, 8}[g.r.IntN(5)]
	if g.r.IntN(400) == 0 {
		n = []int{rules.MaxDims, rules.MaxDims + 1}[g.r.IntN(2)]
	}
	special := []float32{0, float32(math.Copysign(0, -1)), float32(math.NaN()), math.Float32frombits(0x7fc00001),
		math.Float32frombits(0xffffffff), float32(math.Inf(1)), float32(math.Inf(-1)), math.SmallestNonzeroFloat32,
		-math.SmallestNonzeroFloat32, math.MaxFloat32, -math.MaxFloat32, 1, -0.5}
	v := make([]float32, n)
	zeros := g.r.IntN(3) == 0
	for i := range v {
		switch {
		case zeros:
			v[i] = special[g.r.IntN(2)]
		case g.r.IntN(4) == 0:
			v[i] = special[g.r.IntN(len(special))]
		default:
			v[i] = float32(g.r.NormFloat64())
		}
	}
	return v
}

// float64s returns a real near the rules' edges.
func (g awkward) float64s() float64 {
	special := []float64{0, math.Copysign(0, -1), math.NaN(), math.Float64frombits(0x7ff8000000000001),
		math.Float64frombits(0xfff0000000000001), math.Inf(1), math.Inf(-1), math.SmallestNonzeroFloat64,
		math.MaxFloat64, -math.MaxFloat64, 1}
	if g.r.IntN(2) == 0 {
		return special[g.r.IntN(len(special))]
	}
	return g.r.NormFloat64() * math.Pow(10, float64(g.r.IntN(601)-300))
}

// text returns text that is valid UTF-8 or nearly.
func (g awkward) text() string {
	if g.r.IntN(4) == 0 {
		b := make([]byte, g.r.IntN(6))
		for i := range b {
			b[i] = byte(g.r.Uint32())
		}
		return string(b)
	}
	return g.piece()
}

func TestTheCodecRefusesWhatTheRulesRefuse(t *testing.T) {
	n := 20000
	if testing.Short() {
		n = 4000
	}
	g := awkward{rand.New(rand.NewPCG(7, 1))}
	// taken and refused count how often each rule takes and refuses, so a
	// generator that stopped reaching one side would show.
	taken, refused := map[string]int{}, map[string]int{}
	agree := func(rule string, what any, ruleRefuses bool, codec ...Change) {
		t.Helper()
		if ruleRefuses {
			refused[rule]++
		} else {
			taken[rule]++
		}
		for _, c := range codec {
			if refuses(c) != ruleRefuses {
				t.Errorf("%s: %q: rules refuses it: %v; the codec refuses %v: %v", rule, what, ruleRefuses, c, !ruleRefuses)
			}
		}
	}
	for range n {
		name := g.name()
		agree("table name", name, rules.Table(name) != nil,
			Change{Op: CreateTable, Table: name}, Change{Op: Drop, Table: name})

		key := g.key()
		_, err := rules.TableOf(key)
		agree("key", key, err != nil,
			Change{Op: Delete, Key: key}, Change{Op: Put, Key: key},
			Change{Op: Link, Key: key, Type: "t", To: "a:1"}, Change{Op: Unlink, Key: "a:1", Type: "t", To: key})

		field := g.name()
		agree("field name", field, rules.Field(field) != nil,
			Change{Op: CreateTable, Table: "t", Names: []string{field}},
			Change{Op: Put, Key: "t:1", Fields: []Field{{Name: field}}})

		typ := g.linkType()
		agree("link type", typ, rules.LinkType(typ) != nil,
			Change{Op: Link, Key: "a:1", Type: typ, To: "b:2"}, Change{Op: Unlink, Key: "a:1", Type: typ, To: "b:2"})

		// Two names that both keep the rules clash when they match
		// regardless of case, in a create change and in a put alike.
		a, b := g.name(), g.name()
		if g.r.IntN(2) == 0 {
			b = flipCase(g.r, a)
		}
		if rules.Field(a) == nil && rules.Field(b) == nil && a != b {
			lo, hi := min(a, b), max(a, b)
			agree("two field names", lo+" "+hi, rules.SameName(a, b),
				Change{Op: CreateTable, Table: "t", Names: []string{a, b}},
				Change{Op: Put, Key: "t:1", Fields: []Field{{Name: lo}, {Name: hi}}})
		}

		v := g.float32s()
		agree("vector", v, rules.Vector(v) != nil,
			Change{Op: Put, Key: "t:1", Fields: []Field{{Name: "vec", Value: val.Vector(v)}}})

		f := g.float64s()
		agree("real", f, rules.Real("x", f) != nil, Change{Op: Put, Key: "t:1", Fields: []Field{{Name: "x", Value: val.Real(f)}}})

		s := g.text()
		agree("text", s, rules.Text("x", s) != nil, Change{Op: Put, Key: "t:1", Fields: []Field{{Name: "x", Value: val.Text(s)}}})

		// The vector field holds a vector or null, and no other field
		// holds a vector: the store's rule, from rules.IsVec and
		// rules.VectorElsewhere.
		vf := []string{"vec", "Vec", "VEC", "vex", "vec_", "x"}[g.r.IntN(6)]
		vv := []val.Value{val.Null(), val.Int(1), val.Real(0.5), val.Text("[1]"), val.Bytes("\x00\x00\x80\x3f"), val.Vector([]float32{1})}[g.r.IntN(6)]
		isVec, kind := rules.IsVec(vf), vv.Kind()
		agree("the vector field", vf+" "+kind.String(), isVec && kind != val.KindNull && kind != val.KindVector || !isVec && kind == val.KindVector,
			Change{Op: Put, Key: "t:1", Fields: []Field{{Name: vf, Value: vv}}})
	}
	for _, rule := range []string{"table name", "key", "field name", "link type", "two field names", "vector", "real", "text", "the vector field"} {
		if taken[rule] < n/100 || refused[rule] < n/100 {
			t.Errorf("%s: the rules took %d and refused %d, too few of one to compare", rule, taken[rule], refused[rule])
		}
	}
	if testing.Verbose() {
		t.Logf("taken %v\nrefused %v", taken, refused)
	}
}

// flipCase returns s with the case of some of its ASCII letters changed.
func flipCase(r *rand.Rand, s string) string {
	b := []byte(s)
	for i, c := range b {
		if r.IntN(2) == 0 {
			switch {
			case 'a' <= c && c <= 'z':
				b[i] = c - 'a' + 'A'
			case 'A' <= c && c <= 'Z':
				b[i] = c - 'A' + 'a'
			}
		}
	}
	return string(b)
}

// TestTheRulesEdges pins the edges both sets of rules share, one case
// each, so that a generator that stopped reaching one would still be
// caught here.
func TestTheRulesEdges(t *testing.T) {
	long := func(n int, s string) string { return strings.Repeat(s, n) }
	cases := []struct {
		rule string
		s    string
		bad  bool
	}{
		{"table", long(63, "a"), false}, {"table", long(64, "a"), true}, {"table", "hc_x", true}, {"table", "hc", false},
		{"table", "sqlite_x", true}, {"table", "a9_", false}, {"table", "9a", true}, {"table", "_a", true}, {"table", "aB", true},
		{"key", "a:" + long(1022, "x"), false}, {"key", "a:" + long(1023, "x"), true}, {"key", "a:\x00", true},
		{"key", "a:\xff", true}, {"key", "a:é", false}, {"key", "a:b:c", false}, {"key", "a:", true}, {"key", ":a", true},
		{"field", long(64, "a"), false}, {"field", long(65, "a"), true}, {"field", "_", false}, {"field", "_rowid_", true},
		{"field", "OiD", true}, {"field", "keys", false}, {"field", "1a", true}, {"field", "a\u212a", true},
		{"type", long(200, "😀"), false}, {"type", long(201, "a"), true}, {"type", "\x00a", true}, {"type", "a\x00", false},
		{"type", "", true}, {"type", "\xff", true},
	}
	for _, c := range cases {
		var ruleBad, codecBad bool
		switch c.rule {
		case "table":
			ruleBad, codecBad = rules.Table(c.s) != nil, refuses(Change{Op: Drop, Table: c.s})
		case "key":
			_, err := rules.TableOf(c.s)
			ruleBad, codecBad = err != nil, refuses(Change{Op: Delete, Key: c.s})
		case "field":
			ruleBad, codecBad = rules.Field(c.s) != nil, refuses(Change{Op: CreateTable, Table: "t", Names: []string{c.s}})
		case "type":
			ruleBad, codecBad = rules.LinkType(c.s) != nil, refuses(Change{Op: Link, Key: "a:1", Type: c.s, To: "b:2"})
		}
		if ruleBad != c.bad || codecBad != c.bad {
			t.Errorf("%s %.50q (%d bytes, %d characters): rules refuses it: %v; the codec: %v; want %v",
				c.rule, c.s, len(c.s), utf8.RuneCountInString(c.s), ruleBad, codecBad, c.bad)
		}
	}
}

// TestTheVectorCheck holds vectorProblem, which reads two values at a time,
// to the words of a check one value at a time: the first value that's NaN or
// infinite is the one named, wherever it falls, at an even place or an odd
// one, the last of a vector of an odd length included. A vector of zeros and
// -0 is all zero, and one value that isn't zero, anywhere, keeps it from
// being so.
func TestTheVectorCheck(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 2))
	bad := []float32{float32(math.NaN()), math.Float32frombits(0x7fc00001), math.Float32frombits(0xffffffff),
		float32(math.Inf(1)), float32(math.Inf(-1)), math.Float32frombits(0x7f800001)}
	zero := []float32{0, float32(math.Copysign(0, -1))}
	for range 5000 {
		n := 1 + r.IntN(9)
		if r.IntN(50) == 0 {
			n = []int{384, 385, 1536, rules.MaxDims}[r.IntN(4)]
		}
		v := make([]float32, n)
		for i := range v {
			switch k := r.IntN(10); {
			case k < 1 && r.IntN(n) < 2:
				v[i] = bad[r.IntN(len(bad))]
			case k < 6:
				v[i] = zero[r.IntN(2)]
			default:
				v[i] = math.Float32frombits(r.Uint32() &^ 0x40000000) // never NaN or infinite
			}
		}
		// The words of a check one value at a time.
		want := ""
		allZero := true
		for i, x := range v {
			if math.Float32bits(x)&0x7f800000 == 0x7f800000 {
				want = fmt.Sprintf("a vector whose value %d is %v", i, x)
				break
			}
			allZero = allZero && math.Float32bits(x)&0x7fffffff == 0
		}
		if want == "" && allZero {
			want = "a vector whose values are all zero"
		}
		if got := vectorProblem(val.Vector(v).Raw()); got != want {
			t.Fatalf("a vector of %d values starting %v: %q, where %q is wanted", n, v[:min(n, 9)], got, want)
		}
	}
}
