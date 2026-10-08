// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && cgo

package query

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The evaluator against SQLite itself, through 0.x, on many more random
// expressions than the corpus holds, with the awkward values: the ends of
// 64 bits, -0, infinity, subnormals, reals with more digits than SQLite
// keeps, text that reads as a number or partly as one, text with NUL
// bytes or that isn't UTF-8, bytes, Unicode, and arguments of every kind.
// These tests need cgo, as 0.x does.

// valueGen makes random expressions of the subset without fields, as text,
// with their arguments.
type valueGen struct {
	r    *rand.Rand
	args []difftest.Value
	// fields sets the generator to use docs's fields, the one-record
	// subquery and IN over a walk, for a SELECT over the corpus's fixture.
	fields bool
}

func (g *valueGen) pick(xs ...string) string { return xs[g.r.IntN(len(xs))] }

var (
	awkwardInts = []string{"0", "1", "2", "3", "7", "10", "42", "100", "255", "1000000", "2147483647",
		"2147483648", "4294967296", "9007199254740992", "9007199254740993", "4503599627370496",
		"9223372036854775806", "9223372036854775807", "9223372036854775808", "9223372036854775809",
		"18446744073709551615", "18446744073709551616", "99999999999999999999999", "00012",
		"0009223372036854775808", "00000000000000000000000000000000000001"}
	awkwardReals = []string{"0.0", "0.5", "1.5", "2.25", "0.1", "0.2", "0.3", "1e10", "1e15", "1e16",
		"1e17", "1e21", "1e22", "1e-4", "1e-5", "1e-7", "1e308", "1.7976931348623157e308",
		"1.7976931348623159e308", "5e-324", "4.9e-324", "2.4703282292062327e-324", "2.2250738585072014e-308",
		"2.225073858507201e-308", "1e999", "1e-400", "3500000000000000.2500001", "0.30000000000000004",
		"49.47", "2.675", "4503599627370496.5", "4503599627370497.5", "9007199254740993.0",
		"9223372036854775807.0", "9223372036854775808.0", "9223372036854774784.0", "123456789012345678901234567890.0",
		"0.123456789012345678901234567890", "1.0000000000000002", "0.49999999999999994", "2.5", "3.5",
		".5", "5.", "1E3", "1e+3", "1.5e-3", "100.0", "12345.678", "7.0e-310"}
	awkwardTexts = []string{"''", "'a'", "'A'", "'abc'", "'ABC'", "'aBc'", "'É'", "'é'", "'😀'", "'ʼAmir'",
		"'12'", "' 12 '", "'12abc'", "'1e3'", "'1e'", "'1e+'", "'e5'", "'0x10'", "'1.5'", "'-5'", "'+5'",
		"'-0'", "'-0.0'", "'3.0'", "'1e999'", "'-1e999'", "'Inf'", "'NaN'", "'abc%'", "'a_c'", "'%'", "'_'",
		"'it''s'", "'  pad  '", "'x y'", "'9223372036854775807'", "'9223372036854775808'",
		"'-9223372036854775808'", "'-9223372036854775809'", "'18446744073709551616'", "'.5'", "'5.'",
		"'   '", "'0000000000000000000012'", "'123456789012345678901234567890'", "'3500000000000000.2500001'",
		"'1.0'", "'100'", "' 1 2'", "'\t7\n'", "'+'", "'-'", "'.'", "'1.5abc'", "'2251799813685248'",
		"'2251799813685248.0'", "'4503599627370496.5'", "'1e15'", "'1e16'", "'[1, 2]'", "'[0.5]'"}
	awkwardBlobs = []string{"x''", "x'00'", "x'41'", "x'61'", "x'ff00'", "x'3132'", "x'313200'", "x'c3a9'",
		"x'c389'", "x'0000803f'", "x'00000080'", "x'0000807f'", "x'0000c07f'", "x'000000000000803f'",
		"x'0000803f0000803f'", "x'2031203f'", "x'312e35'", "x'2d30'", "x'ff'", "x'80'", "x'c0'", "x'e282ac'"}
	awkwardOther = []string{"NULL", "CAST(x'610062' AS TEXT)", "CAST(x'313200' AS TEXT)", "CAST(x'00' AS TEXT)",
		"CAST(x'003132' AS TEXT)", "CAST(x'ff' AS TEXT)", "CAST(x'c3' AS TEXT)", "CAST(x'3132003334' AS TEXT)",
		"CAST(x'c3a9c3' AS TEXT)", "CAST(x'eda080' AS TEXT)", "CAST(x'efbfbf' AS TEXT)", "CAST(x'f4908080' AS TEXT)",
		"CAST(x'80c3a9' AS TEXT)", "CAST(x'f88080808080' AS TEXT)"}
	vectorTexts = []string{"'[1, 0.5, -2]'", "'[0.1]'", "'[3, 4]'", "'[4, 3]'", "'[0, 0]'", "'[0, 1]'",
		"'[1, 0]'", "'[-1, 0]'", "'[]'", "'oops'", "'[1e39]'", "'[1e-46]'", "'[null, 1]'", "'[1,2,3]'",
		"'[1e400]'", "'[-0, 0]'", "'[1, 2] '", "' [1]'", "'[\"1\"]'", "'[1, 2, 3, 4]'", "'[2,4]'", "'[1,2]'"}
	vectorBlobs = []string{"x'0000803f'", "x'0000803f00000000'", "x'000000000000803f'", "x'00000000'",
		"x'0000000000000080'", "x'0000c07f'", "x'0000807f'", "x'0000803f000000400000404000'",
		"x'0000803f0000004000004040'", "x'cdcccc3d'", "x''", "x'00'"}
	likePatterns = []string{"'a%'", "'%c'", "'_b_'", "'%'", "'A%'", "'é%'", "'É%'", "'1%'", "'%.%'", "'%_'",
		"'_%'", "'%%'", "'a\\%'", "'%a%b%'", "'__'", "''", "'%1'", "'1_'", "'%e%'", "'%😀%'", "'_😀'",
		"'a!%'", "'!_%'", "'%!%'", "'%x y'"}
	escapes = []string{"'!'", "'\\'", "'%'", "'_'", "'a'", "'é'", "''", "'ab'", "NULL", "x'21'", "1", "'😀'"}
)

func (g *valueGen) arg() string {
	vals := []any{int64(0), int64(1), int64(-1), int64(5), int64(math.MaxInt64), int64(math.MinInt64),
		int64(1 << 53), int64(1<<53 + 1), 0.0, math.Copysign(0, -1), 2.5, -1.5, 0.1, math.Inf(1), math.Inf(-1),
		math.NaN(), 5e-324, math.MaxFloat64, 1e21, 1e-7, 4503599627370496.5, "", "abc", "12", " 12 ", "1e3",
		"-0", "a\x00b", "12\x00", "\xff\xfe", "É", "%", "[1, 2]", "[1]", []byte{}, []byte{0x41}, []byte{0, 0x80, 0x3f},
		[]byte{0, 0, 0x80, 0x3f}, nil, true, false, c.Vector{1, 2}, c.Vector{0, 0}, c.Vector{0.5}}
	g.args = append(g.args, difftest.Value{V: vals[g.r.IntN(len(vals))]})
	return "?"
}

// docsFields are the fields of the fixture's docs table, as a query may
// name them.
var docsFields = []string{"n", "title", "score", "vec", "data", "key", "tags", "status", "docs.n", "N", "Title"}

func (g *valueGen) leaf() string {
	if g.fields && g.r.IntN(4) == 0 {
		return g.pick(docsFields...)
	}
	switch g.r.IntN(16) {
	case 0, 1, 2:
		return g.pick(awkwardInts...)
	case 3, 4, 5:
		return g.pick(awkwardReals...)
	case 6, 7, 8:
		return g.pick(awkwardTexts...)
	case 9, 10:
		return g.pick(awkwardBlobs...)
	case 11:
		return g.pick(awkwardOther...)
	case 12, 13:
		return g.arg()
	case 14:
		return "-" + g.pick(append(awkwardInts, awkwardReals...)...)
	}
	return "NULL"
}

// wrap puts an operand in parentheses two times in three, so the tree
// usually is what the generator meant, and sometimes SQLite's precedence
// decides.
func (g *valueGen) wrap(s string) string {
	if g.r.IntN(3) == 0 {
		return s
	}
	return "(" + s + ")"
}

func (g *valueGen) expr(depth int) string {
	if depth == 0 || g.r.IntN(6) == 0 {
		return g.leaf()
	}
	sub := func() string { return g.wrap(g.expr(depth - 1)) }
	switch g.r.IntN(24) {
	case 0, 1, 2, 3:
		return sub() + " " + g.pick("+", "-", "*", "/", "%", "||", "=", "==", "!=", "<>", "<", "<=", ">", ">=",
			"AND", "OR", "IS", "IS NOT") + " " + sub()
	case 4:
		return g.pick("- ", "+", "NOT ", "-") + sub()
	case 5:
		return sub() + g.pick(" BETWEEN ", " NOT BETWEEN ") + sub() + " AND " + sub()
	case 6:
		items := make([]string, g.r.IntN(4))
		for i := range items {
			items[i] = g.expr(depth - 1)
		}
		return sub() + g.pick(" IN (", " NOT IN (") + strings.Join(items, ", ") + ")"
	case 7:
		p := g.pick(likePatterns...)
		if g.r.IntN(3) == 0 {
			p = sub()
		}
		s := sub() + g.pick(" LIKE ", " NOT LIKE ") + p
		if g.r.IntN(3) == 0 {
			s += " ESCAPE " + g.pick(escapes...)
		}
		return s
	case 8:
		return sub() + g.pick(" IS NULL", " IS NOT NULL")
	case 9, 10:
		return "CAST(" + g.expr(depth-1) + " AS " + g.pick("INTEGER", "REAL", "TEXT", "BLOB", "NUMERIC") + ")"
	case 11:
		return g.pick("abs", "length", "lower", "upper", "typeof") + "(" + g.expr(depth-1) + ")"
	case 12:
		if g.r.IntN(2) == 0 {
			return "substr(" + g.expr(depth-1) + ", " + g.pos() + ")"
		}
		return "substr(" + g.expr(depth-1) + ", " + g.pos() + ", " + g.pos() + ")"
	case 13:
		if g.r.IntN(2) == 0 {
			return "trim(" + g.expr(depth-1) + ")"
		}
		return "trim(" + g.expr(depth-1) + ", " + g.pick(append(awkwardTexts, "NULL", "x'61'", "1")...) + ")"
	case 14:
		if g.r.IntN(3) == 0 {
			return "round(" + g.expr(depth-1) + ")"
		}
		return "round(" + g.expr(depth-1) + ", " + g.pick("0", "1", "2", "3", "5", "10", "15", "17", "20", "30", "31",
			"-1", "2.7", "'2'", "NULL", "9223372036854775807") + ")"
	case 15:
		return g.pick("coalesce", "ifnull", "nullif", "instr") + "(" + g.expr(depth-1) + ", " + g.expr(depth-1) + ")"
	case 16:
		return "replace(" + g.expr(depth-1) + ", " + g.expr(depth-1) + ", " + g.expr(depth-1) + ")"
	case 17:
		n := 2 + g.r.IntN(2)
		items := make([]string, n)
		for i := range items {
			items[i] = g.expr(depth - 1)
		}
		return g.pick("min", "max", "coalesce") + "(" + strings.Join(items, ", ") + ")"
	case 18:
		return "distance(" + g.vec() + ", " + g.vec() + ")"
	case 19:
		return "vector(" + g.vec() + ")"
	case 20:
		return "instr(" + g.expr(depth-1) + ", " + g.pick("'a'", "'é'", "''", "x'00'", "'1'", "x'41'", "'.'", "'😀'") + ")"
	case 21, 22:
		if g.fields {
			return g.record(depth)
		}
	case 23:
		if g.fields {
			return sub() + g.pick(" IN ", " NOT IN ") + g.walk(depth)
		}
	}
	return g.leaf()
}

// record is the one-record subquery, with a key that uses no fields.
func (g *valueGen) record(depth int) string {
	table, field := "docs", g.pick("n", "title", "score", "vec", "data", "key", "tags", "status")
	if g.r.IntN(4) == 0 {
		table, field = "people", g.pick("name", "age", "email", "joined", "key")
	}
	key := g.pick("'docs:1'", "'docs:8'", "'docs:4'", "'docs:9'", "'people:1'", "'people:3'", "NULL", "1",
		"abs(-9223372036854775808)", "vector('oops')", "'docs:' || 2")
	if g.r.IntN(4) == 0 {
		g.fields = false
		key = g.expr(depth - 1)
		g.fields = true
	}
	return "(SELECT " + field + " FROM " + table + " WHERE key = " + key + ")"
}

// walk is the subquery of IN over a walk, in either of its forms, with
// arguments that use no fields.
func (g *valueGen) walk(depth int) string {
	args := []string{g.pick("'docs:1'", "'docs:2'", "'people:1'", "'people:3'", "'docs:4'", "'t_1:a'", "'docs:9'", "NULL", "1", "x'00'"),
		g.pick("1", "2", "3", "32", "2.0", "'2'", "0", "33", "NULL", "-1", "abs(-9223372036854775808)")}
	if g.r.IntN(2) == 0 {
		args = append(args, g.pick("'cites'", "'owns'", "'knows'", "NULL", "''", "1", "'nope'"))
		if g.r.IntN(2) == 0 {
			args = append(args, g.pick("'out'", "'in'", "'both'", "'IN'", "''", "NULL", "'up'", "2"))
		}
	}
	if g.r.IntN(5) == 0 {
		g.fields = false
		args[0] = g.expr(depth - 1)
		g.fields = true
	}
	w := "walk(" + strings.Join(args, ", ") + ")"
	if g.r.IntN(3) == 0 {
		return "(SELECT value FROM json_each(" + w + "))"
	}
	return "(SELECT key FROM " + w + ")"
}

// pos is a position or a length for substr().
func (g *valueGen) pos() string {
	if g.r.IntN(18) == 0 {
		return g.arg()
	}
	return g.pick("0", "1", "2", "3", "-1", "-2", "-3", "5", "-5", "100", "-100", "NULL", "2.9", "'2'", "-0.5",
		"9223372036854775807", "-9223372036854775808")
}

// vec is a side of distance() or vector(): mostly a vector of up to three
// values, so the dot products give 0.x's bits exactly.
func (g *valueGen) vec() string {
	switch g.r.IntN(8) {
	case 0, 1, 2:
		return g.pick(vectorTexts...)
	case 3, 4:
		return g.pick(vectorBlobs...)
	case 5:
		return "vector(" + g.pick(vectorTexts...) + ")"
	case 6:
		return g.arg()
	}
	return g.leaf()
}

// short3 reports whether every vector in q has at most three values, as
// far as the generator's vectors go, which makes distances exact.
func short3(q string) bool {
	return !strings.Contains(q, "[1, 2, 3, 4]") && !strings.Contains(q, "x'0000803f000000400000404000'")
}

// sameAnswer compares two answers exactly, and else within
// conformance.DistanceBound when close is set, for distances worked out
// with other sums. sqlcorpus.Same with close takes two equal infinities
// as different, since their difference is NaN.
func sameAnswer(want, got *sqlcorpus.Answer, close bool) bool {
	return sqlcorpus.Same(want, got, false) || close && sqlcorpus.Same(want, got, true)
}

func openZeroX(t *testing.T) (zerox.Engine, c.DB) {
	t.Helper()
	e := zerox.Engine{}
	db, err := sqlcorpus.OpenFixture(e, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return e, db
}

// TestRandomExpressionsGive0xsAnswers runs random expressions through 0.x
// and through Parse and the evaluator, and the answers must be the same,
// reals to the bit and errors by kind.
func TestRandomExpressionsGive0xsAnswers(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	g := &valueGen{r: rand.New(rand.NewPCG(1, 0x5131))}
	n := 60000
	if testing.Short() {
		n = 8000
	}
	counts := map[string]int{}
	bad := 0
	for i := 0; i < n; i++ {
		g.args = nil
		q := "SELECT " + g.expr(1+g.r.IntN(4)) + " AS v"
		cs := sqlcorpus.Case{SQL: q, Args: g.args}
		if _, err := Parse(q); err != nil {
			counts["refused"]++
			continue
		}
		want := sqlcorpus.Run(e, db, cs, false)
		got := evalSelect(t, q, argValues(cs.Args))
		switch {
		case want.Error != "":
			counts["error"]++
		default:
			counts[typeName(want.Rows[0][0].V)]++
		}
		if !sameAnswer(want, got, !short3(q)) {
			bad++
			if bad <= 30 {
				t.Errorf("%s %v\n  0.x  %+v\n  Beta %+v", q, cs.Args, want, got)
			}
		}
	}
	t.Logf("%v", counts)
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, n)
	}
}

// numberText makes text that reads as a number, or partly as one, with the
// cases SQLite's readers treat apart: signs, spaces, points and exponents,
// more digits than 64 bits hold, and something after the number.
func numberText(r *rand.Rand) string {
	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + r.IntN(10))
		}
		return string(b)
	}
	sign := func() string { return []string{"", "", "+", "-"}[r.IntN(4)] }
	var s string
	switch r.IntN(7) {
	case 0:
		s = digits(1 + r.IntN(22))
	case 1:
		s = digits(r.IntN(20)) + "." + digits(r.IntN(24))
	case 2:
		s = digits(1+r.IntN(20)) + "eE"[r.IntN(2):][:1] + sign() + digits(1+r.IntN(4))
	case 3:
		s = digits(r.IntN(4)) + "." + digits(1+r.IntN(30)) + "e" + sign() + digits(1+r.IntN(3))
	case 4:
		s = strconv.FormatFloat(randomReal(r), 'g', -1, 64)
	case 5:
		s = strconv.FormatInt(r.Int64()>>r.IntN(64), 10)
	default:
		s = []string{"", ".", "-", "+", "e5", "1e", "1e+", ".e1", "0x10", "Inf", "--1", "1..2", "1e5e5"}[r.IntN(13)]
	}
	s = sign() + s
	switch r.IntN(8) {
	case 0:
		s = "  " + s
	case 1:
		s += "  "
	case 2:
		s = "\t" + s + "\n"
	case 3:
		s += "x"
	case 4:
		s += "\x009"
	case 5:
		s += " 1"
	}
	return s
}

// randomReal makes a real of any kind: any bits, a decimal of a few
// digits, a subnormal, or a whole number scaled by a power of two.
func randomReal(r *rand.Rand) float64 {
	switch r.IntN(6) {
	case 0:
		return math.Float64frombits(r.Uint64())
	case 1:
		return r.Float64() * math.Pow(10, float64(r.IntN(44)-22))
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

// TestNumbersAndTextAs0xReadsThem gives 0.x and the evaluator random text
// that reads as a number, or partly, and random reals, as arguments, in
// each place SQL.md's "Text as numbers" and "Numbers as text" name: the
// text of a real, arithmetic, comparing under a numeric CAST, CAST to each
// type, truth, and the functions' number arguments.
func TestNumbersAndTextAs0xReadsThem(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	r := rand.New(rand.NewPCG(2, 0x5131))
	forms := []string{
		"CAST(? AS TEXT)", "CAST(? AS REAL)", "CAST(? AS INTEGER)", "CAST(? AS NUMERIC)", "? + 0", "? - 0.0",
		"? * 1", "? / 1", "? % 7", "-?", "? || ''", "typeof(? + 0)", "? = CAST(? AS INTEGER)",
		"CAST(? AS NUMERIC) = ?", "? < CAST(? AS REAL)", "NOT ?", "? AND 1", "abs(?)", "round(?)", "round(?, 2)",
		"round(?, 10)", "substr('abcdef', ?)", "length(?)", "typeof(CAST(? AS NUMERIC))", "? IN (CAST(? AS INTEGER))",
		"CAST(? AS INTEGER) IN (?, 1)", "CAST(? AS TEXT) = ?", "? BETWEEN CAST(? AS REAL) AND 1e300",
		"upper(?)", "CAST(CAST(? AS REAL) AS TEXT)", "CAST(? AS REAL) = CAST(CAST(CAST(? AS REAL) AS TEXT) AS REAL)",
	}
	n := 40000
	if testing.Short() {
		n = 6000
	}
	bad := 0
	for i := 0; i < n; i++ {
		form := forms[r.IntN(len(forms))]
		var args []difftest.Value
		for range strings.Count(form, "?") {
			var v any
			if r.IntN(3) == 0 {
				v = randomReal(r)
			} else {
				v = numberText(r)
			}
			args = append(args, difftest.Value{V: v})
		}
		q := "SELECT " + form + " AS v"
		cs := sqlcorpus.Case{SQL: q, Args: args}
		want := sqlcorpus.Run(e, db, cs, false)
		got := evalSelect(t, q, argValues(args))
		if !sqlcorpus.Same(want, got, false) {
			bad++
			if bad <= 30 {
				t.Errorf("%s %v\n  0.x  %+v\n  Beta %+v", q, args, want, got)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, n)
	}
}

// TestTheRuleTablesAre0xs holds every table in eval_test.go to 0.x's
// answers, so each rule's cases are SQLite's behaviour as well as the
// evaluator's.
func TestTheRuleTablesAre0xs(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	for name, cases := range ruleTables {
		for _, cs := range cases {
			args := make([]difftest.Value, len(cs.args))
			for i, a := range cs.args {
				args[i] = difftest.Value{V: a}
			}
			ans := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: "SELECT " + cs.expr + " AS v", Args: args}, false)
			if ok, got := same0x(ans, cs.want); !ok {
				t.Errorf("%s: %s %v: 0.x gives %s, and the table wants %#v", name, cs.expr, cs.args, got, cs.want)
			}
		}
	}
}

// same0x reports whether 0.x's answer is what a ruleCase wants, and says
// what it is. 0.x's own functions refuse with messages that start
// "hypercrux: ", or give a blob's length, and SQLite's errors are the rest.
func same0x(ans *sqlcorpus.Answer, want any) (bool, string) {
	if ans.Error != "" {
		f := anError
		if strings.HasPrefix(ans.Message, "hypercrux: ") || strings.HasPrefix(ans.Message, "a vector blob of") {
			f = aFuncError
		}
		return want == f, string(f) + ": " + ans.Message
	}
	v := ans.Rows[0][0].V
	got := fmt.Sprintf("%T %#v", v, v)
	switch w := want.(type) {
	case nil:
		return v == nil, got
	case int:
		x, ok := v.(int64)
		return ok && x == int64(w), got
	case int64:
		x, ok := v.(int64)
		return ok && x == w, got
	case float64:
		x, ok := v.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(w), got
	case string:
		x, ok := v.(string)
		return ok && x == w, got
	case []byte:
		x, ok := v.([]byte)
		return ok && string(x) == string(w), got
	}
	return false, got
}

// TestArgumentsAs0xBindsThem holds Arg to what 0.x makes of each Go value
// in argCases, through database/sql and go-sqlite3, as SELECT ? gives it
// back, or the error.
func TestArgumentsAs0xBindsThem(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	for _, cs := range argCases {
		q := sqlcorpus.Case{SQL: "SELECT ? AS v", Args: []difftest.Value{{V: cs.v}}}
		want := sqlcorpus.Run(e, db, q, false)
		v, err := Arg(cs.v)
		got := &sqlcorpus.Answer{Error: "error"}
		if err == nil {
			got = evalSelect(t, q.SQL, []value.Value{v})
		}
		if !sqlcorpus.Same(want, got, false) {
			t.Errorf("%#v: 0.x gives %+v, and Arg %+v", cs.v, want, got)
		}
	}
}

// fixtureRows reads a table of the corpus's fixture from 0.x as SQL sees
// it, in key order, as the rows the planner would hand the evaluator: each
// field by its folded name, and vec as a vector.
func fixtureRows(t *testing.T, db c.DB, table string) []fakeRow {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table + " ORDER BY key")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []fakeRow
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := fakeRow{}
		for i, col := range cols {
			v := argValue(vals[i])
			if strings.EqualFold(col, "vec") && v.Kind() == value.KindBytes {
				v = value.VectorBits(v.Raw())
			}
			row[strings.ToLower(col)] = v
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// fixtureScope is a fakeScope over the corpus's fixture as 0.x holds it,
// whose walks are 0.x's, run as SELECT key FROM walk(...), with 0.x's
// refusals as FuncErrors.
func fixtureScope(t *testing.T, db c.DB) (*fakeScope, []fakeRow) {
	s := &fakeScope{records: map[string]fakeRow{}}
	var docs []fakeRow
	for _, table := range []string{"docs", "people", "t_1"} {
		rows := fixtureRows(t, db, table)
		for _, row := range rows {
			s.records[row["key"].Raw()] = row
		}
		if table == "docs" {
			docs = rows
		}
	}
	s.walk = func(args []value.Value) ([]value.Value, error) {
		as := make([]any, len(args))
		for i, a := range args {
			as[i] = goValue(a)
		}
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		rows, err := db.Query("SELECT value FROM json_each(walk("+marks+"))", as...)
		if err != nil {
			return nil, &FuncError{Func: "walk", Msg: strings.TrimPrefix(err.Error(), "hypercrux: ")}
		}
		defer rows.Close()
		var keys []value.Value
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return nil, err
			}
			keys = append(keys, value.Text(k))
		}
		if err := rows.Err(); err != nil {
			return nil, &FuncError{Func: "walk", Msg: strings.TrimPrefix(err.Error(), "hypercrux: ")}
		}
		return keys, nil
	}
	return s, docs
}

// evalOverRows works out SELECT expr AS v FROM docs for each of rows, with
// s, as the corpus has a query's answer.
func evalOverRows(t *testing.T, q string, s *fakeScope, rows []fakeRow, args []value.Value) *sqlcorpus.Answer {
	st, err := Parse(q)
	if err != nil {
		return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
	}
	ev, err := Compile(st.(*Select).Results[0].Expr, s)
	if err != nil {
		return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
	}
	ans := &sqlcorpus.Answer{Columns: []string{"v"}}
	for _, row := range rows {
		v, err := ev(&Frame{Args: args, Row: row})
		if err != nil {
			return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
		}
		ans.Rows = append(ans.Rows, []difftest.Value{{V: goValue(v)}})
	}
	return ans
}

// fieldExprs are expressions over the fixture's docs table for
// TestFieldsAs0xHasThem, beside its random ones.
var fieldExprs = []string{
	"n = '5'", "n = 5", "n > 4", "n IN ('5', 3)", "n IN ('5', 3, 7)", "CAST(n AS INTEGER) = 5", "n = CAST('5' AS TEXT)",
	"CAST(5 AS INTEGER) = n", "n = CAST(5 AS INTEGER)", "CAST(5 AS INTEGER) IN (n, 6)", "n BETWEEN CAST('1' AS INTEGER) AND '9'",
	"typeof(vec)", "length(vec)", "vec = vector('[0.9, 0.1, 0]')", "distance(vec, '[1, 0, 0]')", "vec IS NULL",
	"typeof(data)", "data = x''", "title LIKE 'q3%'", "upper(title)", "title || n", "score * 2", "-score", "score = 0",
	"+n = '5'", "(n) = '5'", "n IS '5'", "nullif(n, '5')", "max(n, score)", "min(title, n)", "CAST(n AS TEXT) = n",
	"n = title", "key = 'docs:1'", "key > 'docs:4'", "key < 5", "key = CAST(1 AS INTEGER)", "substr(key, 6)", "tags",
	"typeof(tags)", "status IN ('open', 'done')", "coalesce(score, n, 'none')", "ifnull(data, vec)", "vector(vec)",
	"n + score", "n / 0", "instr(title, 'Q')", "round(score, 1)", "abs(n)", "CAST(title AS NUMERIC) = 12", "title = 12",
	"CAST(12 AS REAL) = title", "title IN (12, '12')", "CAST(title AS INTEGER) IN ('12')", "score IN (1.5, 2.25)",
	"score = -0.0", "1 / score", "vec || ''", "distance(vec, vec)", "distance(vec, '[1, 2]')",
	"(SELECT n FROM docs WHERE key = 'docs:8') = 5", "(SELECT n FROM docs WHERE key = 'docs:8') = CAST(5 AS INTEGER)",
	"n = (SELECT n FROM docs WHERE key = 'docs:8')", "(SELECT title FROM docs WHERE key = 'docs:8') = 12",
	"(SELECT vec FROM docs WHERE key = 'docs:1') = vec", "distance(vec, (SELECT vec FROM docs WHERE key = 'docs:1'))",
	"(SELECT key FROM docs WHERE key = 'docs:2')", "(SELECT n FROM docs WHERE key = 'people:1')",
	"(SELECT age FROM people WHERE key = 'people:1') + n", "NULL + (SELECT n FROM docs WHERE key = abs(-9223372036854775808))",
	"(SELECT n FROM docs WHERE key = abs(-9223372036854775808)) AND (n = 0)", "n + (SELECT n FROM docs WHERE key = vector('x'))",
	"key IN (SELECT key FROM walk('docs:1', 2))", "key NOT IN (SELECT key FROM walk('docs:1', 2, 'cites'))",
	"key IN (SELECT value FROM json_each(walk('people:1', 2)))", "n IN (SELECT key FROM walk('docs:1', 1))",
	"NULL IN (SELECT key FROM walk('docs:1', 1))", "NULL IN (SELECT key FROM walk('docs:4', 1))",
	"(SELECT key FROM docs WHERE key = 'docs:3') IN (SELECT key FROM walk('docs:1', 1))",
	"abs(-9223372036854775808) IN (SELECT key FROM walk('docs:1', 2.0))",
	"n IS NULL OR key IN (SELECT key FROM walk('docs:2', 3, NULL, 'both'))",
	"n IN ((SELECT n FROM docs WHERE key = 'docs:9'))", "NULL IN ((SELECT n FROM docs WHERE key = 'docs:9'))",
	"NULL NOT IN ((SELECT n FROM docs WHERE key = 'docs:9'))", "n IN ((SELECT n FROM docs WHERE key = 'docs:4'))",
	"CAST(n AS TEXT) IN ((SELECT n FROM docs WHERE key = 'docs:1'))", "CAST(n AS TEXT) IN ((SELECT n FROM docs WHERE key = 'docs:1'), 0)",
	"CAST(n AS INTEGER) IN ((SELECT n FROM docs WHERE key = 'docs:8'))", "n IN ((SELECT n FROM docs WHERE key = 'docs:8'))",
	"n IN (+(SELECT n FROM docs WHERE key = 'docs:8'))", "key IN ((SELECT key FROM docs WHERE key = 'docs:3'))",
	"CAST(key AS TEXT) IN ((SELECT key FROM docs WHERE key = 'docs:3'))", "vec IN ((SELECT vec FROM docs WHERE key = 'docs:1'))",
}

// TestFieldsAs0xHasThem runs expressions over the fixture's docs table on
// 0.x and through the evaluator with a Scope over the same records: fields
// as they compare and convert, the vector field as bytes, the one-record
// subquery and IN over a walk, and the order SQLite works them out in.
func TestFieldsAs0xHasThem(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	s, docs := fixtureScope(t, db)
	g := &valueGen{r: rand.New(rand.NewPCG(3, 0x5131)), fields: true}
	n := 12000
	if testing.Short() {
		n = 2000
	}
	bad := 0
	for i := 0; i < len(fieldExprs)+n; i++ {
		g.args = nil
		var q string
		if i < len(fieldExprs) {
			q = "SELECT " + fieldExprs[i] + " AS v FROM docs ORDER BY key"
		} else {
			q = "SELECT " + g.expr(1+g.r.IntN(3)) + " AS v FROM docs ORDER BY key"
			if _, err := Parse(q); err != nil {
				continue
			}
		}
		cs := sqlcorpus.Case{SQL: q, Via: via0x(q), Args: g.args}
		want := sqlcorpus.Run(e, db, cs, false)
		got := evalOverRows(t, q, s, docs, argValues(cs.Args))
		if !sameAnswer(want, got, !short3(q)) {
			bad++
			if bad <= 30 {
				t.Errorf("%s %v\n  0.x  %+v\n  Beta %+v", q, cs.Args, want, got)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d differ", bad)
	}
}

// via0x writes the Beta's IN over a walk, (SELECT key FROM walk(...)), as
// 0.x has it, (SELECT value FROM json_each(walk(...))), for 0.x has walk()
// only as a function.
func via0x(q string) string {
	const beta, zerox = "(SELECT key FROM walk(", "(SELECT value FROM json_each(walk("
	for {
		i := strings.Index(q, beta)
		if i < 0 {
			return q
		}
		depth, j := 1, i+len(beta)
		for ; j < len(q) && depth > 0; j++ {
			switch q[j] {
			case '(':
				depth++
			case ')':
				depth--
			case '\'':
				for j++; j < len(q) && q[j] != '\''; j++ {
				}
			}
		}
		q = q[:i] + zerox + q[i+len(beta):j] + ")" + q[j:]
	}
}

// TestWhereConditionsAs0xHasThem holds condCases to 0.x, then runs random
// conditions, some of them several joined by AND, as SELECT 1 AS v WHERE
// cond on 0.x and through CompileCondition, with the WHERE split as
// whereSplit splits it.
func TestWhereConditionsAs0xHasThem(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	for _, cs := range condCases {
		q := "SELECT 1 AS v WHERE " + loneE.ReplaceAllLiteralString(cs.cond, overflow)
		want := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: q}, false)
		if !sqlcorpus.Same(want, condAnswer(cs.want), false) {
			t.Errorf("%s: 0.x gives %+v, and the table wants %v", cs.cond, want, cs.want)
		}
	}
	g := &valueGen{r: rand.New(rand.NewPCG(4, 0x5131))}
	n := 30000
	if testing.Short() {
		n = 4000
	}
	bad := 0
	for i := 0; i < n; i++ {
		g.args = nil
		terms := make([]string, 1+g.r.IntN(3))
		for j := range terms {
			terms[j] = g.wrap(g.expr(1 + g.r.IntN(4)))
		}
		q := "SELECT 1 AS v WHERE " + strings.Join(terms, " AND ")
		if _, err := Parse(q); err != nil {
			continue
		}
		cs := sqlcorpus.Case{SQL: q, Args: g.args}
		want := sqlcorpus.Run(e, db, cs, false)
		got := evalWhere(q, argValues(cs.Args))
		if !sameAnswer(want, got, false) {
			bad++
			if bad <= 30 {
				t.Errorf("%s %v\n  0.x  %+v\n  Beta %+v", q, cs.Args, want, got)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, n)
	}
}

// randomVector makes a vector of dims values: mostly normal ones, with
// zeros, -0 and any bits at all now and then, NaN, infinities and
// subnormals among them.
func randomVector(r *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		switch r.IntN(24) {
		case 0:
			v[i] = 0
		case 1:
			v[i] = float32(math.Copysign(0, -1))
		case 2:
			v[i] = math.Float32frombits(r.Uint32())
		default:
			v[i] = float32(r.NormFloat64() * math.Pow(10, float64(r.IntN(9)-4)))
		}
	}
	return v
}

// vectorArg hands a vector over as bytes, or as text holding a JSON array.
func vectorArg(r *rand.Rand, v []float32) any {
	if r.IntN(2) == 0 {
		return []byte(vectorBits(v))
	}
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(float64(x), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// TestDistancesAs0xHasThem gives distance() and vector() random vectors of
// up to 300 values, as text and as bytes, on 0.x and through the
// evaluator. distance() adds up the dot products in 8 running sums, as the
// store's Nearest does, where 0.x adds them up in one, so the two agree to
// the bit up to 3 values and within conformance.DistanceBound beyond.
func TestDistancesAs0xHasThem(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	r := rand.New(rand.NewPCG(5, 0x5131))
	n := 4000
	if testing.Short() {
		n = 600
	}
	bad := 0
	for i := 0; i < n; i++ {
		dims := 1 + r.IntN(3)
		if r.IntN(2) == 0 {
			dims = 1 + r.IntN(300)
		}
		a, b := randomVector(r, dims), randomVector(r, dims)
		if r.IntN(20) == 0 {
			b = randomVector(r, 1+r.IntN(300))
		}
		args := []difftest.Value{{V: vectorArg(r, a)}, {V: vectorArg(r, b)}}
		for _, q := range []string{"SELECT distance(?, ?) AS v", "SELECT vector(?) AS v"} {
			cs := sqlcorpus.Case{SQL: q, Args: args[:strings.Count(q, "?")]}
			want := sqlcorpus.Run(e, db, cs, false)
			got := evalSelect(t, q, argValues(cs.Args))
			if !sameAnswer(want, got, dims > 3) {
				bad++
				if bad <= 10 {
					t.Errorf("%s %v\n  0.x  %+v\n  Beta %+v", q, cs.Args, want, got)
				}
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d differ", bad)
	}
}
