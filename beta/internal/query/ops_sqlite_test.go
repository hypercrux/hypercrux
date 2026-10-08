// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && cgo

package query

import (
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The operators against 0.x through zerox: the tables of agg_test.go and
// ops_corpus_test.go, then sorts and aggregates on random rows, put into
// 0.x and into a store in key order, so 0.x reads them in key order too.
// These tests need cgo, as 0.x does.

// unionOf is a FROM's subquery giving one row for each of n arguments, in
// order, in the column x, which has no affinity, as a field has none.
func unionOf(n int) string {
	if n == 0 {
		return "(SELECT NULL AS x WHERE 0)"
	}
	return "(SELECT ? AS x" + strings.Repeat(" UNION ALL SELECT ?", n-1) + ")"
}

// zeroxValue is an aggCase's value as 0.x takes it: a vector as the
// conformance suite's Vector, which binds as its bytes.
func zeroxValue(x any) difftest.Value {
	if v, ok := x.([]float32); ok {
		return difftest.Value{V: c.Vector(v)}
	}
	return difftest.Value{V: x}
}

// TestTheAggregateTablesAre0xs holds aggCases, aggExprCases and
// aggSkippedCases to 0.x's answers, as SELECT agg(x) AS v over a subquery
// that gives the same rows in the same order.
func TestTheAggregateTablesAre0xs(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	run := func(name, expr string, rows []any, args []any, want any) {
		var vals []difftest.Value
		for _, a := range args {
			vals = append(vals, difftest.Value{V: a})
		}
		for _, x := range rows {
			vals = append(vals, zeroxValue(x))
		}
		q := "SELECT " + expr + " AS v FROM " + unionOf(len(rows))
		ans := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: q, Args: vals}, false)
		if ok, got := same0x(ans, want); !ok {
			t.Errorf("%s: %s: 0.x gives %s, and the table wants %#v", name, expr, got, want)
		}
	}
	for _, cs := range aggCases {
		for i, fn := range aggFuncs {
			run(cs.name, fn, cs.xs, nil, cs.want[i])
		}
	}
	for _, cs := range aggExprCases {
		run("expressions", cs.expr, aggExprRows, cs.args, cs.want)
	}
	for _, cs := range aggSkippedCases {
		run("aggregates skipped", cs.expr, aggSkippedRows, cs.args, cs.want)
	}
}

func TestTheFixtureQueriesAre0xs(t *testing.T) {
	e, db := openZeroX(t)
	defer db.Close()
	for _, q := range fixtureQueries {
		ans := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: q.sql}, false)
		if got := answerText(ans); got != q.want {
			t.Errorf("%s:\n  0.x gives %s\n  the table %s", q.sql, strings.ReplaceAll(got, "\n", " / "), strings.ReplaceAll(q.want, "\n", " / "))
		}
	}
}

// fieldValues are the values a random record's fields take, every one a
// value 0.x and the Beta both store: whole numbers and reals at their
// edges, text that reads as a number or partly or not at all, bytes, and
// NULL.
var fieldValues = []any{
	nil, nil, nil, int64(0), int64(1), int64(-1), int64(2), int64(3), int64(7), int64(10), int64(42), int64(100),
	int64(math.MaxInt64), int64(math.MinInt64), int64(math.MaxInt64 - 1), int64(1 << 53), int64(1<<53 + 1),
	int64(1 << 62), int64(-1 << 62), 0.0, negZero, 0.1, 0.2, 0.25, 0.5, 1.0, 1.5, -2.25, 3.0, 1e308, -1e308,
	math.MaxFloat64, 5e-324, 1e-300, 1e21, 1e16, 9007199254740993.0, "", "a", "A", "b", "z", "5", " 5 ", "5.0",
	"1e3", "abc", "0x10", "-0", "-0.0", "1e999", "-1e999", "9223372036854775807", "9223372036854775808", "É", "é",
	"12abc", []byte{}, []byte{0}, []byte("5"), []byte{0xff}, []byte("abc"),
}

// randomField is a random field's value: one of fieldValues, or now and
// then a random whole number or real.
func randomField(r *rand.Rand) any {
	switch r.IntN(10) {
	case 0:
		return r.Int64N(2000) - 1000
	case 1:
		return r.Int64() >> r.IntN(64)
	case 2:
		return r.NormFloat64() * math.Pow(10, float64(r.IntN(30)-15))
	}
	return fieldValues[r.IntN(len(fieldValues))]
}

// randomTable puts n records into the table t, on 0.x and in a store, in
// key order: the fields a, b and c with random values, and now and then a
// vector of two values.
func randomTable(t *testing.T, r *rand.Rand, n int) (zerox.Engine, c.DB, *store.Store) {
	t.Helper()
	e := zerox.Engine{}
	db, err := e.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	s := store.New()
	err = db.Update(func(tx c.Handle) error {
		for i := 0; i < n; i++ {
			f := c.Fields{"a": randomField(r), "b": randomField(r), "c": randomField(r), "vec": nil}
			if r.IntN(3) == 0 {
				f["vec"] = c.Vector{float32(r.IntN(5) - 2), float32(1 + r.IntN(3))}
			}
			key := fmt.Sprintf("t:%04d", i)
			if err := tx.Put(key, f); err != nil {
				return err
			}
			fields, err := storeFields(f)
			if err != nil {
				return err
			}
			if _, err := s.Put(nil, key, fields); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return e, db, s
}

// queryGen makes random queries over the table randomTable makes.
type queryGen struct{ r *rand.Rand }

func (g *queryGen) pick(xs ...string) string { return xs[g.r.IntN(len(xs))] }

// where is a WHERE that never fails, of up to three terms, or nothing.
func (g *queryGen) where() string {
	n := g.r.IntN(4)
	if n == 0 {
		return ""
	}
	terms := make([]string, n)
	for i := range terms {
		terms[i] = g.pick("a > 0", "a < b", "b IS NOT NULL", "c IS NULL", "typeof(a) = 'integer'", "typeof(b) = 'real'",
			"key > 't:0040'", "key < 't:0090'", "a BETWEEN -10 AND 10", "length(c) > 2", "b = 5", "a IN (1, 2, 3)",
			"c LIKE 'a%'", "NOT (a > b)", "vec IS NOT NULL", "a IS NOT b", "c >= 'a'", "a = 1", "b IN (1, 3)",
			"typeof(c) = 'text'", "typeof(a) <> 'real'")
	}
	return " WHERE " + strings.Join(terms, " AND ")
}

// limit is a LIMIT, perhaps with an OFFSET, or nothing.
func (g *queryGen) limit() string {
	switch g.r.IntN(4) {
	case 0:
		return ""
	case 1:
		return " LIMIT " + g.pick("-1", "0", "1", "2", "3", "5", "10", "50", "500") + " OFFSET " + g.pick("-1", "0", "1", "2", "5", "30", "500")
	}
	return " LIMIT " + g.pick("-1", "0", "1", "2", "3", "5", "10", "50", "500")
}

// sortQuery is a query with an ORDER BY, for the Beta, and the same with
// key as its last term, for 0.x, whose ties aren't settled: the Beta's
// stable sort over rows in key order has to give 0.x's answer.
func (g *queryGen) sortQuery() (beta, zero string) {
	cols := g.pick("a", "b", "c", "vec", "a + b", "abs(a)", "typeof(b)", "a || c", "-c", "length(b)", "coalesce(a, b, c)", "abs(b) + 1")
	n := 1
	for g.r.IntN(2) == 0 && n < 4 {
		cols += ", " + g.pick("a", "b AS p", "c", "vec", "abs(c) AS q", "a * 2", "b || 'x'", "typeof(a)")
		n++
	}
	terms := make([]string, 1+g.r.IntN(3))
	for i := range terms {
		switch g.r.IntN(4) {
		case 0:
			terms[i] = fmt.Sprint(1 + g.r.IntN(n))
		case 1:
			if strings.Contains(cols, " AS p") {
				terms[i] = "p"
				break
			}
			fallthrough
		default:
			terms[i] = g.pick("a", "b", "c", "vec", "a + b", "-a", "typeof(b)", "length(c)", "abs(c)", "a IS NULL", "c || ''", "b * 0")
		}
		if g.r.IntN(2) == 0 {
			terms[i] += " DESC"
		}
	}
	head := "SELECT " + cols + ", key FROM t" + g.where() + " ORDER BY " + strings.Join(terms, ", ")
	tail := g.limit()
	return head + tail, head + ", key" + tail
}

// aggQuery is an aggregate query.
func (g *queryGen) aggQuery() string {
	n := 1 + g.r.IntN(4)
	exprs := make([]string, n)
	for i := range exprs {
		exprs[i] = g.pick("count(*)", "count(a)", "sum(a)", "total(b)", "avg(c)", "min(a)", "max(b)", "sum(a + b)",
			"sum(abs(a))", "max(a) - min(a)", "coalesce(sum(c), 0)", "typeof(sum(a))", "total(CAST(a AS REAL))",
			"sum(length(b))", "avg(a * 2)", "count(vec)", "min(vec)", "max(c) || 'x'", "0 AND sum(abs(c))", "sum(b) IS NULL",
			"avg(a) + avg(b)", "min(a) = max(a)", "sum(CAST(c AS INTEGER))", "total(a) / count(*)", "max(a, 0) + min(b)",
			"sum(b)", "avg(b)", "min(c)", "max(c)", "sum(c)", "total(c)", "avg(a)", "count(c)", "sum(-a)", "min(-b)")
		// a field's max(), with two arguments, is no aggregate
		if exprs[i] == "max(a, 0) + min(b)" {
			exprs[i] = "max(sum(a), 0) + min(b)"
		}
	}
	q := "SELECT " + strings.Join(exprs, ", ") + " FROM t" + g.where()
	if g.r.IntN(4) == 0 {
		q += " ORDER BY " + g.pick("1", "count(*) DESC", "sum(abs(c))", "max(a)", "min(b) DESC")
	}
	if g.r.IntN(4) == 0 {
		q += " LIMIT " + g.pick("0", "1", "-1") + g.pick("", " OFFSET 1", " OFFSET 0")
	}
	return q
}

// compareOn runs a query through runSelect on s and its 0.x form on db,
// and reports whether the answers are the same.
func compareOn(t *testing.T, e zerox.Engine, db c.DB, s *store.Store, beta, zero string) (bool, string) {
	st, err := Parse(beta)
	if err != nil {
		t.Fatalf("%s: %v", beta, err)
	}
	got, err := runSelect(s, st.(*Select), nil)
	if err != nil {
		t.Fatalf("%s: %v", beta, err)
	}
	want := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: zero}, false)
	if sqlcorpus.Same(want, got, false) {
		return true, want.Error
	}
	return false, fmt.Sprintf("%s\n  0.x  %s\n  Beta %s", beta, strings.ReplaceAll(answerText(want), "\n", " / "), strings.ReplaceAll(answerText(got), "\n", " / "))
}

// TestSortsAs0xHasThem runs random queries with ORDER BY, LIMIT and OFFSET
// over random rows, on 0.x and through Sort, TopK and Limit. The rows tie
// often, so Sort's stability counts, and some result columns fail for some
// rows, so which rows TopK works out counts too.
func TestSortsAs0xHasThem(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 0x5134))
	tables, queries := 6, 400
	if testing.Short() {
		tables, queries = 2, 200
	}
	g := &queryGen{r: r}
	bad, failed, total := 0, 0, 0
	for i := 0; i < tables; i++ {
		e, db, s := randomTable(t, r, 20+r.IntN(150))
		for j := 0; j < queries; j++ {
			beta, zero := g.sortQuery()
			total++
			ok, why := compareOn(t, e, db, s, beta, zero)
			switch {
			case !ok:
				bad++
				if bad <= 20 {
					t.Error(why)
				}
			case why != "":
				failed++
			}
		}
		db.Close()
	}
	t.Logf("%d queries, %d of them errors", total, failed)
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, total)
	}
}

// TestAggregatesAs0xHasThem runs random aggregate queries over random rows,
// on 0.x and through Aggregate: whole numbers past 64 bits, reals to the
// bit, text as numbers, and the errors of their arguments and of sum().
func TestAggregatesAs0xHasThem(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 0x5134))
	tables, queries := 6, 400
	if testing.Short() {
		tables, queries = 2, 200
	}
	g := &queryGen{r: r}
	bad, failed, total := 0, 0, 0
	for i := 0; i < tables; i++ {
		e, db, s := randomTable(t, r, r.IntN(150))
		for j := 0; j < queries; j++ {
			q := g.aggQuery()
			total++
			ok, why := compareOn(t, e, db, s, q, q)
			switch {
			case !ok:
				bad++
				if bad <= 20 {
					t.Error(why)
				}
			case why != "":
				failed++
			}
		}
		db.Close()
	}
	t.Logf("%d queries, %d of them errors", total, failed)
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, total)
	}
}
