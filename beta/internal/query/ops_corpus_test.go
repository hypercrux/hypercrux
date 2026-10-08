// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The closing test of Q4: the corpus's statements with aggregates, on the
// fixture sqlcorpus.Fixture builds, loaded into a real store, each run
// through operators put together by hand, as the planner will put them
// together (Q5), give 0.x's answers exactly.

// storeDB lets sqlcorpus.Fixture fill a store: it puts its records and its
// links through Update, Put and Link, which go straight to the store. The
// fixture uses nothing else, and anything else panics.
type storeDB struct {
	c.DB
	s *store.Store
}

type storeHandle struct {
	c.Handle
	s *store.Store
}

func (d storeDB) Update(fn func(tx c.Handle) error) error { return fn(storeHandle{s: d.s}) }

func (h storeHandle) Put(key string, f c.Fields) error {
	fields, err := storeFields(f)
	if err != nil {
		return err
	}
	_, err = h.s.Put(nil, key, fields)
	return err
}

func (h storeHandle) Link(from, typ, to string) error {
	_, err := h.s.Link(nil, from, typ, to)
	return err
}

// storeFields makes a put's fields of 0.x's, through value.FromGo, as the
// public package does. The conformance suite's Vector reaches FromGo as a
// []float32.
func storeFields(f c.Fields) ([]format.Field, error) {
	var fields []format.Field
	for name, v := range f {
		if vec, ok := v.(c.Vector); ok {
			v = []float32(vec)
		}
		x, err := value.FromGo(name, v)
		if err != nil {
			return nil, err
		}
		fields = append(fields, format.Field{Name: name, Value: x})
	}
	slices.SortFunc(fields, func(a, b format.Field) int { return strings.Compare(a.Name, b.Name) })
	return fields, nil
}

// fixtureStore is a store holding the corpus's fixture.
func fixtureStore(t testing.TB) *store.Store {
	t.Helper()
	s := store.New()
	if err := sqlcorpus.Fixture(storeDB{s: s}); err != nil {
		t.Fatal(err)
	}
	return s
}

// errUnplanned marks a statement runSelect doesn't put together, since it
// needs more than a scan of one table: a walk, a join, no FROM, the
// one-record subquery or IN over a walk.
var errUnplanned = fmt.Errorf("needs more than a scan")

// runSelect runs sel over r through the operators, put together by hand as
// the planner will put them together (Q5), and gives the answer as the
// corpus has it. It takes a SELECT over one table, with WHERE split at its
// top-level ANDs, result columns or *, aggregates, ORDER BY by alias,
// column number or expression, LIMIT and OFFSET. Anything else gives
// errUnplanned.
func runSelect(r store.Reader, sel *Select, args []value.Value) (*sqlcorpus.Answer, error) {
	if sel.From == nil || len(sel.From.Sources) != 1 || sel.From.Sources[0].Table == nil || usesSubqueries(sel) {
		return nil, errUnplanned
	}
	failed := func(err error) (*sqlcorpus.Answer, error) {
		return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}, nil
	}
	from := sel.From.Sources[0]
	t, ok := r.Table(from.Table.Folded())
	if !ok {
		return failed(&Error{Pos: from.At, Msg: "no such table: " + from.Table.Name})
	}
	src := TableSource(from.Name(), t, 0)
	scope := &RowScope{Sources: []*RowSource{src}}
	frame := &Frame{Args: args}

	// What SQLite prepares, and so its errors, come first: the names, the
	// result columns, the ORDER BY terms and the aggregates.
	results := sel.Results
	if sel.Star {
		results = nil
		for _, name := range src.Columns {
			results = append(results, Result{Expr: &Column{Name: Ident{Name: name}}})
		}
	}
	var conds []Cond
	if sel.Where != nil {
		for _, term := range whereTerms(sel.Where) {
			cond, err := CompileCondition(term, scope)
			if err != nil {
				return failed(err)
			}
			conds = append(conds, cond)
		}
	}
	ans := &sqlcorpus.Answer{}
	exprs := make([]Expr, len(results))
	for i, res := range results {
		exprs[i] = res.Expr
		name := res.Text
		if res.Alias != nil {
			name = res.Alias.Name
		} else if col, ok := res.Expr.(*Column); ok {
			if at := src.find(col.Name.Name); at >= 0 {
				name = src.Columns[at]
			}
		}
		ans.Columns = append(ans.Columns, name)
	}
	keys, orderExprs, err := orderKeys(sel, results)
	if err != nil {
		return failed(err)
	}
	var colScope Scope = scope
	var aggs *Aggregates
	if sel.Aggregate {
		// An aggregate query's ORDER BY sorts one row: only the aggregates
		// in its expressions are worked out.
		all := exprs
		for _, e := range orderExprs {
			if e != nil {
				all = append(all, e)
			}
		}
		if aggs, err = NewAggregates(all, scope); err != nil {
			return failed(err)
		}
		colScope = aggs
	}
	cols := make([]Eval, len(exprs))
	for i, e := range exprs {
		if cols[i], err = Compile(e, colScope); err != nil {
			return failed(err)
		}
	}
	if !sel.Aggregate {
		for i, k := range keys {
			if k.Col < 0 {
				if keys[i].Eval, err = Compile(orderExprs[i], scope); err != nil {
					return failed(err)
				}
			}
		}
	}

	n, skip, err := Bounds(sel.Limit, sel.Offset, scope, frame)
	if err != nil {
		return failed(err)
	}
	if n == 0 {
		return ans, nil
	}
	rows := Scan(r, src)
	if conds != nil {
		rows = Filter(rows, conds, frame)
	}
	switch {
	case sel.Aggregate:
		rows = Project(Limit(Aggregate(rows, aggs, frame), n, skip), cols, frame)
	case len(keys) > 0:
		k := n + max(skip, 0)
		if n < 0 || k < 0 {
			rows = Sort(rows, cols, keys, frame)
		} else {
			rows = TopK(rows, cols, keys, k, frame)
		}
		rows = Limit(rows, n, skip)
	default:
		rows = Project(Limit(rows, n, skip), cols, frame)
	}
	defer rows.Close()
	for rows.Next() {
		row := make([]difftest.Value, len(rows.Row()))
		for i, v := range rows.Row() {
			row[i] = difftest.Value{V: goValue(v)}
		}
		ans.Rows = append(ans.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return failed(err)
	}
	return ans, nil
}

// usesSubqueries reports whether sel holds the one-record subquery or IN
// over a walk anywhere, or calls walk(), date() or datetime(), which need
// the planner's Scope.
func usesSubqueries(sel *Select) bool {
	var exprs []Expr
	for _, r := range sel.Results {
		exprs = append(exprs, r.Expr)
	}
	for _, t := range sel.OrderBy {
		exprs = append(exprs, t.Expr)
	}
	exprs = append(exprs, sel.Where, sel.Limit, sel.Offset)
	for _, e := range exprs {
		if e == nil {
			continue
		}
		found := hasSubquery(e)
		walkExpr(e, func(e Expr) bool {
			if call, ok := e.(*Call); ok && (call.Func() == "walk" || call.Func() == "date" || call.Func() == "datetime") {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// orderKeys settles each ORDER BY term by SQL.md's three rules: the alias
// of a result column, a column number, or an expression of its own, which
// it gives at the same place in exprs for the caller to compile, and nil
// for the others. The expressions here use fields only, without the result
// columns' aliases.
func orderKeys(sel *Select, results []Result) (keys []SortKey, exprs []Expr, err error) {
	for i, term := range sel.OrderBy {
		key := SortKey{Col: -1, Desc: term.Desc}
		if col, ok := term.Expr.(*Column); ok && col.Table == nil {
			for j, res := range results {
				if res.Alias != nil && res.Alias.Folded() == col.Name.Folded() {
					key.Col = j
					break
				}
			}
		}
		if key.Col < 0 {
			if n, ok := columnNumber(term.Expr); ok {
				if n < 1 || n > int64(len(results)) {
					return nil, nil, &Error{Pos: term.Expr.Pos(), Msg: fmt.Sprintf("%s ORDER BY term out of range - should be between 1 and %d", ordinal(i+1), len(results))}
				}
				key.Col = int(n - 1)
			}
		}
		keys = append(keys, key)
		if key.Col < 0 {
			exprs = append(exprs, term.Expr)
		} else {
			exprs = append(exprs, nil)
		}
	}
	return keys, exprs, nil
}

// columnNumber reads ORDER BY's column number: an integer literal that fits
// in 32 bits, perhaps with signs in front of it.
func columnNumber(e Expr) (int64, bool) {
	neg := false
	for {
		u, ok := e.(*Unary)
		if !ok || u.Op == OpNot {
			break
		}
		if u.Op == OpNeg {
			neg = !neg
		}
		e = u.X
	}
	l, ok := e.(*Literal)
	if !ok || l.Kind != LitInt || len(l.Text) > 10 {
		return 0, false
	}
	v := literal(l, false)
	if v.Kind() != value.KindInt || v.Int() > 1<<31-1 {
		return 0, false
	}
	if neg {
		return -v.Int(), true
	}
	return v.Int(), true
}

func ordinal(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}

// aggregateCases are the corpus's statements that use an aggregate, with
// what each needs. Those that need more than a scan are left for later
// tasks, with the reason.
func aggregateCases(t testing.TB) (run []sqlcorpus.Case, later map[string]string) {
	t.Helper()
	cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", "statements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	later = map[string]string{}
	for _, cs := range cases {
		if !cs.In {
			continue
		}
		st, err := Parse(cs.SQL)
		if err != nil {
			continue
		}
		if sel, ok := st.(*Select); ok && sel.Aggregate {
			run = append(run, cs)
			continue
		}
		if cs.After == "" {
			continue
		}
		after, err := Parse(cs.After)
		if err != nil {
			continue
		}
		if sel, ok := after.(*Select); ok && sel.Aggregate {
			later[cs.ID] = "an aggregate query after a write, which needs Q6's " + strings.Fields(cs.SQL)[0]
			if usesSubqueries(sel) {
				later[cs.ID] += ", and walk() from Q5's Scope"
			}
		}
	}
	return run, later
}

func TestTheCorpusAggregatesGive0xsAnswers(t *testing.T) {
	s := fixtureStore(t)
	cases, later := aggregateCases(t)
	bad := 0
	for _, cs := range cases {
		sel, err := Parse(cs.SQL)
		if err != nil {
			t.Fatalf("%s: %v", cs.ID, err)
		}
		got, err := runSelect(s, sel.(*Select), argValues(cs.Args))
		if err != nil {
			t.Errorf("%s: %s: %v", cs.ID, cs.SQL, err)
			continue
		}
		if !sqlcorpus.Same(cs.Answer, got, cs.Close) {
			bad++
			t.Errorf("%s: %s\n  want %+v\n  got  %+v", cs.ID, cs.SQL, cs.Answer, got)
		}
	}
	// The corpus has 16 aggregate statements marked in, each over one
	// table, and two aggregate queries after a write.
	if len(cases) != 16 {
		t.Errorf("%d aggregate statements, and the corpus has 16", len(cases))
	}
	wantLater := map[string]string{
		"statement-0216": "an aggregate query after a write, which needs Q6's UPDATE",
		"statement-0224": "an aggregate query after a write, which needs Q6's DELETE, and walk() from Q5's Scope",
	}
	if fmt.Sprint(later) != fmt.Sprint(wantLater) {
		t.Errorf("left for later: %v, want %v", later, wantLater)
	}
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, len(cases))
	}
}

// answerText writes an answer as fixtureQueries has it: its rows, one a
// line, each value as value.String writes it, or "error".
func answerText(a *sqlcorpus.Answer) string {
	if a.Error != "" {
		return "error"
	}
	var lines []string
	for _, row := range a.Rows {
		vals := make([]string, len(row))
		for i, v := range row {
			vals[i] = argValue(v.V).String()
		}
		lines = append(lines, strings.Join(vals, ", "))
	}
	return strings.Join(lines, "\n")
}

// overflowAt fails at one record of the fixture's docs and gives the
// largest integer at every other: abs() of the smallest integer there.
func overflowAt(key string) string {
	return "abs(-9223372036854775807 - (key = '" + key + "'))"
}

const most = "int 9223372036854775807"

// fixtureQueries are queries over the corpus's fixture that show how the
// operators work together, as runSelect puts them together, and what each
// gives, as answerText writes it. ops_sqlite_test.go holds them to 0.x's
// answers. They show what SQLite works out and what it leaves: nothing at
// all after a LIMIT of 0, every aggregate a query names, the aggregates'
// errors behind an OFFSET that skips their row, only the WHERE of the rows
// OFFSET skips without ORDER BY, and under ORDER BY with a LIMIT, every
// row's terms and the result columns of the rows the sorter keeps as they
// come.
var fixtureQueries = []struct{ sql, want string }{
	{"SELECT count(*) FROM docs LIMIT 0", ""},
	{"SELECT count(*) FROM docs LIMIT 1 OFFSET 1", ""},
	{"SELECT count(*) FROM docs LIMIT -1 OFFSET -5", "int 8"},
	{"SELECT count(*) FROM docs LIMIT 0 OFFSET NULL", ""},
	{"SELECT count(*) FROM docs LIMIT NULL", "error"},
	{"SELECT count(*) FROM docs LIMIT '2' OFFSET ' 0 '", "int 8"},
	{"SELECT count(*) FROM docs ORDER BY 2", "error"},
	{"SELECT sum(abs(-9223372036854775808)) FROM docs LIMIT 0", ""},
	{"SELECT sum(abs(-9223372036854775808)) FROM docs WHERE 0", "null"},
	{"SELECT count(*), sum(n), total(n), avg(n), min(n), max(n) FROM docs WHERE n > 1000 AND n < 0", "int 0, null, real 0 (0000000000000000), null, null, null"},
	{"SELECT count(*) FROM docs ORDER BY abs(-9223372036854775808)", "int 8"},
	{"SELECT count(*) FROM docs ORDER BY sum(abs(-9223372036854775808))", "error"},
	{"SELECT 0 AND sum(abs(-9223372036854775808)) FROM docs", "error"},
	{"SELECT 0 AND count(*) FROM docs", "int 0"},
	{"SELECT count(*) IN () FROM docs", "int 0"},
	{"SELECT sum(abs(-9223372036854775808)) IN () FROM docs", "error"},
	{"SELECT sum(9223372036854775807) FROM docs", "error"},
	{"SELECT sum(9223372036854775807) FROM docs LIMIT 1 OFFSET 1", "error"},
	{"SELECT sum(9223372036854775807) FROM docs LIMIT 0", ""},
	{"SELECT sum(9223372036854775807) - 1 FROM docs WHERE key = 'docs:1'", "int 9223372036854775806"},
	{"SELECT total(9223372036854775807), avg(9223372036854775807) FROM docs", "real 7.378697629483821e+19 (4410000000000000), real 9.223372036854776e+18 (43e0000000000000)"},
	{"SELECT abs(-9223372036854775808) + count(*) FROM docs LIMIT 1 OFFSET 1", ""},
	{"SELECT abs(-9223372036854775808) + count(*) FROM docs", "error"},
	{"SELECT abs(min(n)), max(n) + 1, count(*) * 2 FROM docs", "int 2, int 6, int 16"},
	{"SELECT min(title), max(title), count(title) FROM docs WHERE title LIKE 'q3%'", "text \"Q3 budget\", text \"Q3 plan\", int 2"},
	{"SELECT count(vec), typeof(min(vec)), sum(vec) FROM docs", "int 6, text \"blob\", real 0 (0000000000000000)"},
	{"SELECT sum(score), total(score), avg(score) FROM docs WHERE key <= 'docs:2'", "real 1.5 (3ff8000000000000), real 1.5 (3ff8000000000000), real 0.75 (3fe8000000000000)"},
	{"SELECT min(score), max(score), sum(score) FROM docs WHERE key = 'docs:2'", "real -0 (8000000000000000), real -0 (8000000000000000), real 0 (0000000000000000)"},
	{"SELECT key FROM docs ORDER BY status LIMIT 3", "text \"docs:8\"\ntext \"docs:7\"\ntext \"docs:4\""},
	{"SELECT key FROM docs ORDER BY status DESC LIMIT 3", "text \"docs:1\"\ntext \"docs:3\"\ntext \"docs:5\""},
	{"SELECT key, n FROM docs ORDER BY n DESC", "text \"docs:8\", text \"5\"\ntext \"docs:7\", int 42\ntext \"docs:5\", int 10\ntext \"docs:3\", int 7\n" +
		"text \"docs:1\", int 3\ntext \"docs:2\", int 1\ntext \"docs:6\", int -2\ntext \"docs:4\", null"},
	{"SELECT key, score FROM docs ORDER BY score", "text \"docs:6\", null\ntext \"docs:8\", null\ntext \"docs:2\", real -0 (8000000000000000)\n" +
		"text \"docs:4\", real 0.1 (3fb999999999999a)\ntext \"docs:1\", real 1.5 (3ff8000000000000)\ntext \"docs:3\", real 2.25 (4002000000000000)\n" +
		"text \"docs:7\", int 3\ntext \"docs:5\", real 1e+21 (444b1ae4d6e2ef50)"},
	{"SELECT key, data FROM docs ORDER BY data DESC LIMIT 4", "text \"docs:3\", bytes 000102\ntext \"docs:6\", bytes \ntext \"docs:1\", null\ntext \"docs:2\", null"},
	{"SELECT key, title FROM docs ORDER BY title DESC LIMIT 2 OFFSET 1", "text \"docs:7\", text \"a%b_c\"\ntext \"docs:4\", text \"Retro\""},
	{"SELECT key FROM docs ORDER BY n IS NULL, n LIMIT 3", "text \"docs:6\"\ntext \"docs:2\"\ntext \"docs:1\""},
	{"SELECT key, 1 AS c FROM docs ORDER BY c LIMIT 3 OFFSET 2", "text \"docs:3\", int 1\ntext \"docs:4\", int 1\ntext \"docs:5\", int 1"},
	{"SELECT key AS k FROM docs ORDER BY k DESC LIMIT 2", "text \"docs:8\"\ntext \"docs:7\""},
	{"SELECT key, x FROM t_1 ORDER BY x % 2, x DESC", "text \"t_1:d\", int 4\ntext \"t_1:b\", int 2\ntext \"t_1:é\", int 5\ntext \"t_1:c\", int 3\ntext \"t_1:a\", int 1"},
	{"SELECT key FROM docs ORDER BY -1", "error"},
	{"SELECT key FROM docs ORDER BY 0", "error"},
	{"SELECT " + overflowAt("docs:5") + " FROM docs ORDER BY status LIMIT 1", most},
	{"SELECT " + overflowAt("docs:7") + " FROM docs ORDER BY status LIMIT 1", "error"},
	{"SELECT " + overflowAt("docs:6") + " FROM docs ORDER BY status LIMIT 2", most + "\n" + most},
	{"SELECT " + overflowAt("docs:5") + " FROM docs ORDER BY status LIMIT 3", most + "\n" + most + "\n" + most},
	{"SELECT " + overflowAt("docs:6") + " FROM docs ORDER BY status LIMIT 2 OFFSET 1", "error"},
	{"SELECT " + overflowAt("docs:5") + " FROM docs ORDER BY status LIMIT -1", "error"},
	{"SELECT " + overflowAt("docs:3") + " FROM docs ORDER BY status DESC LIMIT 1", most},
	{"SELECT " + overflowAt("docs:3") + " FROM docs ORDER BY status DESC LIMIT 2", "error"},
	{"SELECT " + overflowAt("docs:5") + " AS v FROM docs ORDER BY status, v LIMIT 1", "error"},
	{"SELECT key FROM docs ORDER BY " + overflowAt("docs:5") + " LIMIT 1", "error"},
	{"SELECT key FROM docs ORDER BY abs(-9223372036854775808) LIMIT 0", ""},
	{"SELECT " + overflowAt("docs:5") + " FROM docs LIMIT 2", most + "\n" + most},
	{"SELECT " + overflowAt("docs:5") + " FROM docs LIMIT 2 OFFSET 4", "error"},
	{"SELECT " + overflowAt("docs:5") + " FROM docs LIMIT 2 OFFSET 5", most + "\n" + most},
	{"SELECT key FROM docs WHERE " + overflowAt("docs:5") + " LIMIT 2", "text \"docs:1\"\ntext \"docs:2\""},
	{"SELECT key FROM docs WHERE " + overflowAt("docs:5") + " ORDER BY n LIMIT 2", "error"},
}

func TestQueriesOverTheFixture(t *testing.T) {
	s := fixtureStore(t)
	for _, q := range fixtureQueries {
		st, err := Parse(q.sql)
		if err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
		ans, err := runSelect(s, st.(*Select), nil)
		if err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
		if got := answerText(ans); got != q.want {
			t.Errorf("%s:\n  got  %s\n  want %s", q.sql, strings.ReplaceAll(got, "\n", " / "), strings.ReplaceAll(q.want, "\n", " / "))
		}
	}
}

// unplannedWhy says what a SELECT runSelect leaves needs: the planner's
// one row without a FROM, a walk as a table, a join, or a subquery, walk()
// or a date, which need the planner's Scope (Q5).
func unplannedWhy(sel *Select) string {
	switch {
	case sel.From == nil:
		return "no FROM"
	case len(sel.From.Sources) != 1:
		return "a join"
	case sel.From.Sources[0].Table == nil:
		return "a walk as a table"
	}
	return "the planner's Scope"
}

// TestTheCorpusQueriesOverOneTableGive0xsAnswers runs every statement of
// the corpus that's a SELECT over one table, marked in, through runSelect,
// and each gives 0.x's answer exactly: its columns' names, its rows in
// order, and its error's kind. Those that need more than a scan are left
// for the planner (Q5), and counted by what they need.
func TestTheCorpusQueriesOverOneTableGive0xsAnswers(t *testing.T) {
	s := fixtureStore(t)
	cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", "statements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ran, left := 0, map[string]int{}
	for _, cs := range cases {
		if !cs.In {
			continue
		}
		st, err := Parse(cs.SQL)
		if err != nil {
			continue
		}
		sel, ok := st.(*Select)
		if !ok {
			continue
		}
		got, err := runSelect(s, sel, argValues(cs.Args))
		if err != nil {
			left[unplannedWhy(sel)]++
			continue
		}
		ran++
		if !sqlcorpus.Same(cs.Answer, got, cs.Close) {
			t.Errorf("%s: %s %v\n  want %+v\n  got  %+v", cs.ID, cs.SQL, cs.Args, cs.Answer, got)
		}
	}
	want := map[string]int{"no FROM": 24, "a join": 4, "a walk as a table": 4, "the planner's Scope": 9}
	if ran != 157 || fmt.Sprint(left) != fmt.Sprint(want) {
		t.Errorf("ran %d of the corpus's SELECTs, and left %v; want 157, and %v", ran, left, want)
	}
}
