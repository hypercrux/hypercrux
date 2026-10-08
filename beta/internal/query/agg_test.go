// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"math"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The aggregates over rows of one column x: a table of columns and what
// each aggregate gives over them, which ops_sqlite_test.go holds to 0.x's
// answers too, as SELECT agg(x) over the same values.

// aggFuncs are the aggregates each aggCase gives an answer for, in order.
var aggFuncs = []string{"count(*)", "count(x)", "sum(x)", "total(x)", "avg(x)", "min(x)", "max(x)"}

// An aggCase is a column of values, row by row: an int64, a float64, a
// string for text, a []byte for bytes, a []float32 for a vector, or nil
// for NULL. want holds what each of aggFuncs gives over them, as a
// ruleCase wants it.
type aggCase struct {
	name string
	xs   []any
	want [7]any
}

const (
	two63  = 9223372036854775808.0
	two63n = -9223372036854775808.0
)

var aggCases = []aggCase{
	{"no rows", nil, [7]any{0, 0, nil, 0.0, nil, nil, nil}},
	{"only NULLs", []any{nil, nil}, [7]any{2, 0, nil, 0.0, nil, nil, nil}},
	{"one row", []any{int64(4)}, [7]any{1, 1, 4, 4.0, 4.0, 4, 4}},
	{"whole numbers", []any{int64(3), int64(1), nil, int64(7), int64(-2)}, [7]any{5, 4, 9, 9.0, 2.25, -2, 7}},
	{"reals", []any{1.5, -0.25, 2.0}, [7]any{3, 3, 3.25, 3.25, 3.25 / 3, -0.25, 2.0}},
	{"whole numbers and reals", []any{int64(1), 0.5, int64(2)}, [7]any{3, 3, 3.5, 3.5, 3.5 / 3, 0.5, 2}},
	// Text that all reads as a whole number counts as one, and the rest of
	// the text that reads as a number as a real.
	{"text that reads as numbers", []any{"5", " 5 ", "5.0", "1e1"}, [7]any{4, 4, 25.0, 25.0, 6.25, " 5 ", "5.0"}},
	{"text that reads as whole numbers", []any{"5", "-0", " 2"}, [7]any{3, 3, 7, 7.0, 7.0 / 3, " 2", "5"}},
	{"text that doesn't read", []any{"abc", "12abc", "0x10"}, [7]any{3, 3, 12.0, 12.0, 4.0, "0x10", "abc"}},
	{"text too big for 64 bits", []any{"9223372036854775808"}, [7]any{1, 1, two63, two63, two63, "9223372036854775808", "9223372036854775808"}},
	// Bytes count as the real their text reads as.
	{"bytes", []any{[]byte("5"), []byte("x"), []byte{}}, [7]any{3, 3, 5.0, 5.0, 5.0 / 3, []byte{}, []byte("x")}},
	{"a vector counts as bytes", []any{[]float32{1}, int64(2)}, [7]any{2, 2, 2.0, 2.0, 1.0, 2, []byte("\x00\x00\x80\x3f")}},
	{"mixed kinds", []any{nil, "a", int64(2), []byte{0}, 1.5, "B"}, [7]any{6, 5, 3.5, 3.5, 0.7, 1.5, []byte{0}}},
	// Past 64 bits, sum() fails, and total() and avg() go on as reals.
	{"overflow", []any{int64(math.MaxInt64), int64(1)}, [7]any{2, 2, anError, two63, two63 / 2, 1, int64(math.MaxInt64)}},
	{"overflow, then a real", []any{int64(math.MaxInt64), int64(1), 0.5}, [7]any{3, 3, two63, two63, two63 / 3, 0.5, int64(math.MaxInt64)}},
	{"overflow and back", []any{int64(math.MaxInt64), int64(1), int64(-1)}, [7]any{3, 3, anError, two63, two63 / 3, -1, int64(math.MaxInt64)}},
	{"a real, then big whole numbers", []any{0.5, int64(math.MaxInt64), int64(math.MaxInt64)}, [7]any{3, 3, 2 * two63, 2 * two63, 2 * two63 / 3, 0.5, int64(math.MaxInt64)}},
	{"the smallest integer", []any{int64(math.MinInt64)}, [7]any{1, 1, int64(math.MinInt64), two63n, two63n, int64(math.MinInt64), int64(math.MinInt64)}},
	{"the smallest integer twice", []any{int64(math.MinInt64), int64(math.MinInt64)}, [7]any{2, 2, anError, 2 * two63n, two63n, int64(math.MinInt64), int64(math.MinInt64)}},
	{"whole numbers beyond 2^53", []any{0.5, int64(9007199254740993), int64(9007199254740993)}, [7]any{3, 3, 18014398509481988.0, 18014398509481988.0, 18014398509481988.0 / 3, 0.5, int64(9007199254740993)}},
	// Kahan-Babuska-Neumaier summation keeps what plain sums of reals lose.
	{"ten tenths", []any{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1}, [7]any{10, 10, 1.0, 1.0, 0.1, 0.1, 0.1}},
	{"a big real and small ones", []any{1e16, 1.0, 1.0, -1e16}, [7]any{4, 4, 2.0, 2.0, 0.5, -1e16, 1e16}},
	{"cancelling", []any{1.0, 1e100, 1.0, -1e100}, [7]any{4, 4, 2.0, 2.0, 0.5, -1e100, 1e100}},
	// The order of the rows shows past the largest real.
	{"past the largest real", []any{1e308, 1e308, -1e308}, [7]any{3, 3, math.Inf(1), math.Inf(1), math.Inf(1), -1e308, 1e308}},
	{"past the largest real, the other way", []any{-1e308, 1e308, 1e308}, [7]any{3, 3, 1e308, 1e308, 1e308 / 3, -1e308, 1e308}},
	{"infinities both ways", []any{"1e999", "-1e999"}, [7]any{2, 2, nil, nil, nil, "-1e999", "1e999"}},
	{"one infinity", []any{"1e999", 1.0}, [7]any{2, 2, math.Inf(1), math.Inf(1), math.Inf(1), 1.0, "1e999"}},
	// -0 adds up as 0, and comes back as it is from min() and max().
	{"negative zero", []any{negZero}, [7]any{1, 1, 0.0, 0.0, 0.0, negZero, negZero}},
	// On a tie, min() and max() keep the earliest row's value.
	{"tied numbers", []any{int64(1), 1.0}, [7]any{2, 2, 2.0, 2.0, 1.0, 1, 1}},
	{"tied numbers the other way", []any{1.0, int64(1)}, [7]any{2, 2, 2.0, 2.0, 1.0, 1.0, 1.0}},
	{"tied zeros", []any{0.0, negZero}, [7]any{2, 2, 0.0, 0.0, 0.0, 0.0, 0.0}},
	{"tied zeros the other way", []any{negZero, int64(0)}, [7]any{2, 2, 0.0, 0.0, 0.0, negZero, negZero}},
}

// aggValue makes an aggCase's value a value.
func aggValue(x any) value.Value {
	switch x := x.(type) {
	case int64:
		return value.Int(x)
	case float64:
		return value.Real(x)
	case string:
		return value.Text(x)
	case []byte:
		return value.Bytes(string(x))
	case []float32:
		return value.Vector(x)
	}
	return value.Null()
}

// aggregateOver works out the result column expr of an aggregate query
// over rows of one column x.
func aggregateOver(t *testing.T, expr string, xs []value.Value, args ...value.Value) (value.Value, error) {
	t.Helper()
	src := &RowSource{Name: "t", Columns: []string{"x"}}
	scope := &RowScope{Sources: []*RowSource{src}}
	st, err := Parse("SELECT " + expr + " FROM t")
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	e := st.(*Select).Results[0].Expr
	aggs, err := NewAggregates([]Expr{e}, scope)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	col, err := Compile(e, aggs)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	rows := make([][]value.Value, len(xs))
	for i, x := range xs {
		rows[i] = []value.Value{x}
	}
	f := &Frame{Args: args}
	got, err := collect(Project(Aggregate(&rowsOf{rows: rows}, aggs, f), []Eval{col}, f))
	if err != nil {
		return value.Value{}, err
	}
	if len(got) != 1 {
		t.Fatalf("%s gave %d rows", expr, len(got))
	}
	return got[0][0], nil
}

func TestAggregatesOverRows(t *testing.T) {
	for _, cs := range aggCases {
		xs := make([]value.Value, len(cs.xs))
		for i, x := range cs.xs {
			xs[i] = aggValue(x)
		}
		for i, fn := range aggFuncs {
			v, err := aggregateOver(t, fn, xs)
			if ok, got := sameAs(v, err, cs.want[i]); !ok {
				t.Errorf("%s: %s gives %s, want %#v", cs.name, fn, got, cs.want[i])
			}
		}
	}
}

// aggExprCases are expressions over aggregates, over the rows aggExprRows,
// each call stepped once a row and its result read where the expression
// needs it.
var (
	aggExprRows  = []any{int64(3), nil, int64(5)}
	aggExprCases = []ruleCase{
		r("max(x) + 1", 6), r("count(*) * 2", 6), r("sum(x) - min(x)", 5), r("coalesce(sum(x), 0)", 8),
		r("typeof(sum(x))", "integer"), r("sum(x) / count(x)", 4), r("count(x) || ' of ' || count(*)", "2 of 3"),
		r("max(x) - max(x)", 0), r("count(*) = 3 AND sum(x) > 7", 1), r("total(x) + ?", 18.0, int64(10)),
		r("min(x * -1)", -5), r("sum(x + ?)", 28, int64(10)), r("abs(min(x - 3))", 0),
		r("sum(abs(x))", 8), r("min(x) IN (3, 4)", 1), r("avg(x) BETWEEN 3 AND 5", 1),
	}
)

// aggSkippedCases are aggregates the evaluator skips, over the rows
// aggSkippedRows, which are worked out all the same, as SQLite works out
// every aggregate a query names, so their errors show.
var (
	aggSkippedRows  = []any{int64(1), int64(math.MinInt64)}
	aggSkippedCases = []ruleCase{
		r("0 AND sum(abs(x))", anError), r("sum(abs(x)) IN ()", anError), r("sum(abs(x)) AND 0", anError),
		r("1 OR max(abs(x))", anError), r("0 AND count(*)", 0), r("count(*) IN ()", 0), r("count(x) NOT IN ()", 1),
		r("0 AND sum(x)", 0), r("coalesce(1, sum(abs(x)))", anError), r("0 AND sum(9223372036854775807)", anError),
	}
)

func checkAggregateRules(t *testing.T, rows []any, cases []ruleCase) {
	t.Helper()
	xs := make([]value.Value, len(rows))
	for i, x := range rows {
		xs[i] = aggValue(x)
	}
	for _, cs := range cases {
		args := make([]value.Value, len(cs.args))
		for i, a := range cs.args {
			args[i] = argValue(a)
		}
		v, err := aggregateOver(t, cs.expr, xs, args...)
		if ok, got := sameAs(v, err, cs.want); !ok {
			t.Errorf("%s: got %s, want %#v", cs.expr, got, cs.want)
		}
	}
}

func TestAggregatesInExpressions(t *testing.T) { checkAggregateRules(t, aggExprRows, aggExprCases) }

func TestEveryAggregateIsWorkedOut(t *testing.T) {
	checkAggregateRules(t, aggSkippedRows, aggSkippedCases)
}

// TestAggregatesTakeTheirArgumentsScope checks what the Aggregates hand on
// when result columns compile over them: aggregates read the row, other
// calls and subqueries go to the arguments' Scope, and a field outside an
// aggregate is refused.
func TestAggregatesTakeTheirArgumentsScope(t *testing.T) {
	inner := &fakeScope{}
	expr := func(sql string) Expr {
		st, err := Parse("SELECT " + sql + " FROM t")
		if err != nil {
			t.Fatal(err)
		}
		return st.(*Select).Results[0].Expr
	}
	e := expr("date('now') || count(*)")
	aggs, err := NewAggregates([]Expr{e}, inner)
	if err != nil || aggs.Len() != 1 {
		t.Fatalf("got %v aggregates, %v", aggs, err)
	}
	ev, err := Compile(e, aggs)
	if err != nil {
		t.Fatal(err)
	}
	row := []value.Value{value.Int(7)}
	if v, err := ev(&Frame{Row: &row}); err != nil || v != value.Text("date()7") {
		t.Errorf("got %s, %v", v, err)
	}
	// The parser refuses a field beside an aggregate in the result columns,
	// and the planner in ORDER BY. Should one come through, it fails.
	ev, err = Compile(expr("n"), aggs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ev(&Frame{Row: &row}); err == nil {
		t.Error("a field outside an aggregate was read")
	}
	// An aggregate NewAggregates wasn't given is refused.
	if _, err := Compile(expr("sum(n)"), aggs); err == nil {
		t.Error("an aggregate NewAggregates never saw compiled")
	}
}
