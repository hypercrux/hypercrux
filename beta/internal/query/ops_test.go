// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Each operator against a fake store.Reader: empty tables, one row, ties,
// NULLs, values of every kind and limits at the edges. ops_sqlite_test.go
// holds sorts and aggregates to 0.x on random rows.

// fakeReader is a store.Reader over tables held in memory: each table's
// shape and its records in key order. It counts the scans begun and the
// records handed out, so a test sees what an operator read. Only Table and
// Scan work.
type fakeReader struct {
	tables  map[string]store.Table
	records map[string][]store.Record
	scans   int
	pulled  int
}

var _ store.Reader = (*fakeReader)(nil)

var errFake = errors.New("fakeReader: not here")

func newFakeReader() *fakeReader {
	return &fakeReader{tables: map[string]store.Table{}, records: map[string][]store.Record{}}
}

// add makes a table with these fields and records, each a key and a value
// for each field, NULL for none. The vector field's value is a vector.
func (r *fakeReader) add(name string, fields []string, recs ...fakeRecord) {
	t := store.Table{Name: name, Fields: fields, Vec: slices.Index(fields, "vec")}
	var out []store.Record
	for _, rec := range recs {
		sr := store.Record{Key: rec.key}
		for i, v := range rec.vals {
			switch {
			case v.IsNull():
			case i == t.Vec:
				sr.Vec = v.Vector()
				t.Size = len(sr.Vec)
			default:
				sr.Fields = append(sr.Fields, store.FieldValue{Index: i, Value: v})
			}
		}
		out = append(out, sr)
	}
	slices.SortFunc(out, func(a, b store.Record) int { return strings.Compare(a.Key, b.Key) })
	r.tables[name], r.records[name] = t, out
}

type fakeRecord struct {
	key  string
	vals []value.Value
}

func (r *fakeReader) Table(name string) (store.Table, bool) {
	t, ok := r.tables[name]
	return t, ok
}

func (r *fakeReader) Scan(prefix, after string) (store.Cursor, error) {
	table, _, ok := strings.Cut(prefix, ":")
	if !ok {
		return nil, fmt.Errorf("fakeReader: scan prefix %q needs a table", prefix)
	}
	r.scans++
	var recs []store.Record
	for _, rec := range r.records[table] {
		if strings.HasPrefix(rec.Key, prefix) && rec.Key > after {
			recs = append(recs, rec)
		}
	}
	return &fakeCursor{r: r, recs: recs}, nil
}

func (r *fakeReader) Get(string) (store.Record, error) { return store.Record{}, errFake }
func (r *fakeReader) Neighbours(string, store.Direction, string) ([]store.Link, error) {
	return nil, errFake
}
func (r *fakeReader) Walk(string, store.Direction, string, int) ([]store.Step, error) {
	return nil, errFake
}
func (r *fakeReader) Nearest(string, []float32, int, store.Filter) ([]store.Hit, error) {
	return nil, errFake
}
func (r *fakeReader) Snapshot() iter.Seq[format.Change] { return func(func(format.Change) bool) {} }

type fakeCursor struct {
	r    *fakeReader
	recs []store.Record
}

func (c *fakeCursor) Next() (store.Record, bool) {
	if len(c.recs) == 0 {
		return store.Record{}, false
	}
	rec := c.recs[0]
	c.recs = c.recs[1:]
	c.r.pulled++
	return rec, true
}

// rowsOf is Rows over fixed rows, counting how many were read, which can
// end with an error.
type rowsOf struct {
	rows   [][]value.Value
	end    error
	at     int
	err    error
	closed bool
}

func (s *rowsOf) Next() bool {
	if s.closed || s.err != nil {
		return false
	}
	if s.at == len(s.rows) {
		s.err = s.end
		return false
	}
	s.at++
	return true
}

func (s *rowsOf) Row() []value.Value { return s.rows[s.at-1] }
func (s *rowsOf) Err() error         { return s.err }
func (s *rowsOf) Close()             { s.closed = true }

// collect reads rows as every caller does, copying each.
func collect(r Rows) ([][]value.Value, error) {
	defer r.Close()
	var out [][]value.Value
	for r.Next() {
		out = append(out, slices.Clone(r.Row()))
	}
	return out, r.Err()
}

// rowsText writes rows as the tests compare them: each row's values with
// their kinds, one row a line.
func rowsText(rows [][]value.Value) string {
	var b strings.Builder
	for _, row := range rows {
		for i, v := range row {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(v.String())
		}
		b.WriteString("\n")
	}
	return b.String()
}

// compileAll parses SELECT exprs FROM t and compiles each result column
// with s.
func compileAll(t *testing.T, s Scope, exprs ...string) []Eval {
	t.Helper()
	st, err := Parse("SELECT " + strings.Join(exprs, ", ") + " FROM t")
	if err != nil {
		t.Fatalf("%v: %v", exprs, err)
	}
	var evs []Eval
	for _, r := range st.(*Select).Results {
		ev, err := Compile(r.Expr, s)
		if err != nil {
			t.Fatalf("%s: %v", r.Expr, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// conditions parses a WHERE over t and compiles each of its top-level AND
// terms with s, as the planner hands them to Filter.
func conditions(t *testing.T, s Scope, where string) []Cond {
	t.Helper()
	st, err := Parse("SELECT 1 FROM t WHERE " + where)
	if err != nil {
		t.Fatalf("%s: %v", where, err)
	}
	var conds []Cond
	for _, term := range whereTerms(st.(*Select).Where) {
		c, err := CompileCondition(term, s)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		conds = append(conds, c)
	}
	return conds
}

func ints(vs ...int64) []value.Value {
	out := make([]value.Value, len(vs))
	for i, v := range vs {
		out[i] = value.Int(v)
	}
	return out
}

// docsReader holds the table docs: the fields n, title and vec, and four
// records put in out of key order, one with no fields at all.
func docsReader() *fakeReader {
	r := newFakeReader()
	r.add("docs", []string{"n", "title", "vec"},
		fakeRecord{"docs:3", []value.Value{value.Int(7), value.Text("third"), value.Vector([]float32{1, 2})}},
		fakeRecord{"docs:1", []value.Value{value.Int(3), value.Text("first"), value.Null()}},
		fakeRecord{"docs:20", []value.Value{value.Null(), value.Null(), value.Null()}},
		fakeRecord{"docs:2", []value.Value{value.Real(1.5), value.Null(), value.Vector([]float32{0, 1})}},
	)
	r.add("empty", []string{"n"})
	return r
}

// scanOf makes a scan of table in r, with a RowScope over it, and compiles
// exprs over its rows.
func scanOf(t *testing.T, r *fakeReader, table string, exprs ...string) (Rows, []Eval, *RowSource) {
	t.Helper()
	tbl, ok := r.Table(table)
	if !ok {
		t.Fatalf("no table %s", table)
	}
	src := TableSource(table, tbl, 0)
	evs := compileAll(t, &RowScope{Sources: []*RowSource{src}}, exprs...)
	return Scan(r, src), evs, src
}

func TestAScanGivesKeyOrderAndTheFieldsUsed(t *testing.T) {
	r := docsReader()
	scan, cols, _ := scanOf(t, r, "docs", "key", "n", "typeof(vec)")
	if r.scans != 0 {
		t.Error("the scan began before its first Next")
	}
	got, err := collect(Project(scan, cols, &Frame{}))
	want := "text \"docs:1\", int 3, text \"null\"\ntext \"docs:2\", real 1.5 (3ff8000000000000), text \"blob\"\n" +
		"text \"docs:20\", null, text \"null\"\ntext \"docs:3\", int 7, text \"blob\"\n"
	if err != nil || rowsText(got) != want {
		t.Errorf("got\n%s%v, want\n%s", rowsText(got), err, want)
	}
	if r.scans != 1 || r.pulled != 4 {
		t.Errorf("%d scans and %d records read", r.scans, r.pulled)
	}

	// The fields the statement doesn't read stay NULL, and the vector is a
	// copy, as a vector.
	scan, _, _ = scanOf(t, r, "docs", "vec")
	rows, err := collect(scan)
	if err != nil || len(rows) != 4 {
		t.Fatalf("got %v, %v", rows, err)
	}
	if rows[2][1] != value.Null() || rows[2][2] != value.Null() {
		t.Errorf("unread fields hold %s and %s", rows[2][1], rows[2][2])
	}
	if v := rows[3][3]; v.Kind() != value.KindVector || !slices.Equal(v.Vector(), []float32{1, 2}) {
		t.Errorf("docs:3's vector reads as %s", v)
	}
	r.records["docs"][3].Vec[0] = 9 // the store writes over a slot
	if rows[3][3].Vector()[0] != 1 {
		t.Error("the row's vector shares the store's memory")
	}
}

func TestAScanOfNothing(t *testing.T) {
	r := docsReader()
	for _, table := range []string{"empty"} {
		scan, cols, _ := scanOf(t, r, table, "n")
		got, err := collect(Project(scan, cols, &Frame{}))
		if err != nil || len(got) != 0 {
			t.Errorf("%s: got %v, %v", table, got, err)
		}
	}
	// A table the store hasn't got gives no records, as Scan has it; the
	// planner refuses it first.
	src := TableSource("gone", store.Table{Name: "gone", Vec: -1}, 0)
	if got, err := collect(Scan(r, src)); err != nil || len(got) != 0 {
		t.Errorf("a table that isn't there gave %v, %v", got, err)
	}
	// One record, read once.
	r.add("one", []string{"n"}, fakeRecord{"one:a", ints(5)})
	scan, cols, _ := scanOf(t, r, "one", "n + 1")
	got, err := collect(Project(scan, cols, &Frame{}))
	if err != nil || rowsText(got) != "int 6\n" {
		t.Errorf("one record gave %s%v", rowsText(got), err)
	}
}

func TestARowScopeFindsNames(t *testing.T) {
	docs := TableSource("d", store.Table{Name: "docs", Fields: []string{"n", "Title"}, Vec: -1}, 0)
	walk := &RowSource{Name: "walk", Columns: []string{"key", "depth"}, Base: 3}
	s := &RowScope{Sources: []*RowSource{docs, walk}}
	row := []value.Value{value.Text("docs:1"), value.Int(5), value.Text("Q3"), value.Text("docs:9"), value.Int(2)}
	cases := []struct{ expr, want string }{
		{"n", "int 5"}, {"N", "int 5"}, {"d.n", "int 5"}, {"D.TITLE", "text \"Q3\""}, {"title", "text \"Q3\""},
		{"depth", "int 2"}, {"walk.key", "text \"docs:9\""}, {"d.key", "text \"docs:1\""},
		{"key", "ambiguous column name: key"}, {"docs.n", "no such column: docs.n"}, {"rowid", "no such column: rowid"},
		{"walk.n", "no such column: walk.n"}, {"zz", "no such column: zz"},
		{"(SELECT n FROM docs WHERE key = 'docs:1')", "no such table: docs"},
		{"'a' IN (SELECT key FROM walk('docs:1', 1))", "no such table: walk"},
		{"walk('docs:1', 2)", "walk() needs the statement's planner"},
	}
	for _, cs := range cases {
		st, err := Parse("SELECT " + cs.expr + " FROM t")
		if err != nil {
			t.Fatal(err)
		}
		ev, err := Compile(st.(*Select).Results[0].Expr, s)
		got := ""
		if err == nil {
			var v value.Value
			v, err = ev(&Frame{Row: &row})
			got = v.String()
		}
		if err != nil {
			got = err.Error()
		}
		if got != cs.want {
			t.Errorf("%s: got %s, want %s", cs.expr, got, cs.want)
		}
	}
	if !slices.Equal(docs.reads, []int{0, 1, 2}) || !slices.Equal(walk.reads, []int{0, 1}) {
		t.Errorf("the columns found are %v and %v", docs.reads, walk.reads)
	}

	// The rest goes to Next.
	s.Next = &fakeScope{}
	ev := compileAll(t, s, "walk('docs:1', 2) || count(*)")[0]
	if v, err := ev(&Frame{Row: &row}); err != nil || v != value.Text("walk()count()") {
		t.Errorf("Next gave %s, %v", v, err)
	}

	// A frame without a row is an error.
	if _, err := compileAll(t, s, "n")[0](&Frame{}); err == nil {
		t.Error("a field read without a row")
	}
}

func TestAFilterWorksOutItsTermsInTurn(t *testing.T) {
	src := &RowSource{Name: "t", Columns: []string{"key", "n"}}
	s := &RowScope{Sources: []*RowSource{src}}
	rows := [][]value.Value{
		{value.Text("t:1"), value.Int(1)}, {value.Text("t:2"), value.Null()}, {value.Text("t:3"), value.Int(3)},
		{value.Text("t:4"), value.Text("4")}, {value.Text("t:5"), value.Int(math.MinInt64)},
	}
	cases := []struct {
		where string
		want  string
	}{
		{"n > 1", "t:3 t:4"},
		{"n IS NULL", "t:2"},
		{"n", "t:1 t:3 t:4 t:5"},
		{"NOT n", ""},
		{"n > 3 AND key < 't:5'", "t:4"},
		// The first term rules t:5 out before abs() is worked out, and a
		// NULL rules t:2 out as false would.
		{"n > 0 AND abs(n) > 0", "t:1 t:3 t:4"},
		{"n < 0 AND abs(n) > 0", "an error"},
		{"n < 5 AND abs(n) > 0", "an error"},
		{"n BETWEEN 2 AND 3", "t:3"},
		{"1 = 0", ""},
	}
	for _, cs := range cases {
		got, err := collect(Filter(&rowsOf{rows: rows}, conditions(t, s, cs.where), &Frame{}))
		var keys []string
		for _, row := range got {
			keys = append(keys, row[0].Raw())
		}
		text := strings.Join(keys, " ")
		if err != nil {
			text = "an error"
			var pe *Error
			if !errors.As(err, &pe) {
				text = err.Error()
			}
		}
		if text != cs.want {
			t.Errorf("WHERE %s: got %q, want %q", cs.where, text, cs.want)
		}
	}
	// No rows in, none out, and an error from below comes through.
	if got, err := collect(Filter(&rowsOf{}, conditions(t, s, "n"), &Frame{})); err != nil || got != nil {
		t.Errorf("no rows gave %v, %v", got, err)
	}
	boom := errors.New("boom")
	if _, err := collect(Filter(&rowsOf{rows: rows, end: boom}, conditions(t, s, "n"), &Frame{})); !errors.Is(err, boom) {
		t.Errorf("an error below gave %v", err)
	}
}

func TestAProjectionWorksOutEachColumn(t *testing.T) {
	src := &RowSource{Name: "t", Columns: []string{"key", "n"}}
	s := &RowScope{Sources: []*RowSource{src}}
	rows := [][]value.Value{{value.Text("t:1"), value.Int(2)}, {value.Text("t:2"), value.Text("x")}}
	cols := compileAll(t, s, "n * ?", "key || n", "typeof(n)", "?")
	f := &Frame{Args: []value.Value{value.Int(10), value.Bytes("b")}}
	got, err := collect(Project(&rowsOf{rows: rows}, cols, f))
	want := "int 20, text \"t:12\", text \"integer\", bytes 62\nint 0, text \"t:2x\", text \"text\", bytes 62\n"
	if err != nil || rowsText(got) != want {
		t.Errorf("got\n%s%v, want\n%s", rowsText(got), err, want)
	}
	// A column that fails stops the rows.
	rows = append(rows, []value.Value{value.Text("t:3"), value.Int(math.MinInt64)})
	got, err = collect(Project(&rowsOf{rows: rows}, compileAll(t, s, "abs(n)"), f))
	var pe *Error
	if len(got) != 2 || !errors.As(err, &pe) || pe.Msg != "integer overflow" {
		t.Errorf("got %v, %v", got, err)
	}
}

// sortRows runs Sort, or TopK when k isn't -1, over rows whose first value
// names them, by keys over their other values, and gives the names in
// order.
func sortRows(t *testing.T, rows [][]value.Value, k int64, keys ...SortKey) (string, error) {
	t.Helper()
	cols := make([]Eval, len(rows[0]))
	for i := range cols {
		cols[i] = func(f *Frame) (value.Value, error) {
			row, _ := rowOf(f)
			return row[i], nil
		}
	}
	var got [][]value.Value
	var err error
	if k < 0 {
		got, err = collect(Sort(&rowsOf{rows: rows}, cols, keys, &Frame{}))
	} else {
		got, err = collect(TopK(&rowsOf{rows: rows}, cols, keys, k, &Frame{}))
	}
	var names []string
	for _, row := range got {
		names = append(names, row[0].Raw())
	}
	return strings.Join(names, " "), err
}

// by sorts by the value at place col, going up, or down with desc.
func by(col int, desc bool) SortKey { return SortKey{Col: col, Desc: desc} }

func named(name string, vs ...value.Value) []value.Value {
	return append([]value.Value{value.Text(name)}, vs...)
}

func TestSortsInSQLsOrderOfValues(t *testing.T) {
	// Every kind, with ties between kinds of number and between a vector
	// and its bytes.
	rows := [][]value.Value{
		named("text-a", value.Text("a")), named("null-1", value.Null()), named("bytes", value.Bytes("\x00\x00\x80\x3f")),
		named("int-1", value.Int(1)), named("real-1", value.Real(1)), named("text-A", value.Text("A")),
		named("minus-0", value.Real(math.Copysign(0, -1))), named("vector", value.Vector([]float32{1})),
		named("zero", value.Int(0)), named("null-2", value.Null()), named("big", value.Int(math.MaxInt64)),
		named("bigger", value.Real(9223372036854775808.0)), named("text-ab", value.Text("ab")),
		named("bytes-short", value.Bytes("\x00")), named("minus-inf", value.Real(math.Inf(-1))), named("text-é", value.Text("é")),
	}
	up := "null-1 null-2 minus-inf minus-0 zero int-1 real-1 big bigger text-A text-a text-ab text-é bytes-short bytes vector"
	down := "bytes vector bytes-short text-é text-ab text-a text-A bigger big int-1 real-1 minus-0 zero minus-inf null-1 null-2"
	if got, err := sortRows(t, rows, -1, by(1, false)); err != nil || got != up {
		t.Errorf("going up:\n  got  %s\n  want %s", got, up)
	}
	if got, err := sortRows(t, rows, -1, by(1, true)); err != nil || got != down {
		t.Errorf("going down:\n  got  %s\n  want %s", got, down)
	}

	// Several keys, each its own way.
	rows = [][]value.Value{
		named("a", value.Int(1), value.Text("x")), named("b", value.Int(2), value.Text("x")),
		named("c", value.Int(1), value.Text("y")), named("d", value.Null(), value.Text("x")),
		named("e", value.Int(2), value.Null()), named("f", value.Int(1), value.Text("x")),
	}
	for _, cs := range []struct {
		keys []SortKey
		want string
	}{
		{[]SortKey{by(1, false), by(2, false)}, "d a f c e b"},
		{[]SortKey{by(1, false), by(2, true)}, "d c a f b e"},
		{[]SortKey{by(1, true), by(2, false)}, "e b a f c d"},
		{[]SortKey{by(2, true), by(1, true)}, "c b a f d e"},
		{[]SortKey{by(0, true)}, "f e d c b a"},
	} {
		if got, err := sortRows(t, rows, -1, cs.keys...); err != nil || got != cs.want {
			t.Errorf("%v: got %s, %v, want %s", cs.keys, got, err, cs.want)
		}
	}
	// One row, and none.
	if got, err := sortRows(t, rows[:1], -1, by(1, false)); err != nil || got != "a" {
		t.Errorf("one row: %s, %v", got, err)
	}
	first := func(f *Frame) (value.Value, error) {
		row, _ := rowOf(f)
		return row[0], nil
	}
	if got, err := collect(Sort(&rowsOf{}, []Eval{first}, []SortKey{by(0, false)}, &Frame{})); err != nil || got != nil {
		t.Errorf("no rows: %v, %v", got, err)
	}
}

// tiedRows are n rows named by number, in order, whose second value takes
// only a few values, NULL among them, so most rows tie with many others.
func tiedRows(r *rand.Rand, n int) [][]value.Value {
	vals := []value.Value{value.Null(), value.Int(1), value.Real(1), value.Text("1"), value.Int(2), value.Bytes("")}
	rows := make([][]value.Value, n)
	for i := range rows {
		rows[i] = named(fmt.Sprintf("%04d", i), vals[r.IntN(len(vals))], value.Int(int64(r.IntN(3))))
	}
	return rows
}

// stableOrder is the order a stable sort gives, worked out apart: rows by
// their keys, and then by the order they came in.
func stableOrder(rows [][]value.Value, keys []SortKey) string {
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int {
		for _, k := range keys {
			c := compare(rows[a][k.Col], rows[b][k.Col])
			if k.Desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return a - b
	})
	var names []string
	for _, i := range idx {
		names = append(names, rows[i][0].Raw())
	}
	return strings.Join(names, " ")
}

func TestSortIsStable(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 0x5134))
	rows := tiedRows(r, 300)
	for _, keys := range [][]SortKey{{by(1, false)}, {by(1, true)}, {by(2, false), by(1, true)}, {by(2, true)}} {
		want := stableOrder(rows, keys)
		if got, err := sortRows(t, rows, -1, keys...); err != nil || got != want {
			t.Errorf("%v: rows that tie lost their order:\n  got  %s\n  want %s", keys, got, want)
		}
	}
}

func TestTopKIsSortThenLimit(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 0x5134))
	for round := 0; round < 200; round++ {
		rows := tiedRows(r, 1+r.IntN(40))
		keys := []SortKey{by(1+r.IntN(2), r.IntN(2) == 0)}
		if r.IntN(2) == 0 {
			keys = append(keys, by(1+r.IntN(2), r.IntN(2) == 0))
		}
		full := strings.Fields(stableOrder(rows, keys))
		for k := 0; k <= len(rows)+1; k++ {
			want := strings.Join(full[:min(k, len(full))], " ")
			got, err := sortRows(t, rows, int64(k), keys...)
			if err != nil || got != want {
				t.Fatalf("top %d by %v of %d rows:\n  got  %s\n  want %s", k, keys, len(rows), got, want)
			}
		}
	}
}

// statusRows are the corpus fixture's docs in key order, by their status,
// as 0.x's sorter reads them.
var statusRows = [][]value.Value{
	named("docs:1", value.Text("open")), named("docs:2", value.Text("done")), named("docs:3", value.Text("open")),
	named("docs:4", value.Text("archived")), named("docs:5", value.Text("open")), named("docs:6", value.Text("done")),
	named("docs:7", value.Text("OPEN")), named("docs:8", value.Null()),
}

// workedOut runs ORDER BY status with the given LIMIT, through TopK with a
// result column that notes the rows whose result columns are worked out.
func workedOut(t *testing.T, k int64, desc bool) string {
	var seen []string
	cols := []Eval{func(f *Frame) (value.Value, error) {
		row, _ := rowOf(f)
		seen = append(seen, row[0].Raw())
		return row[0], nil
	}}
	keys := []SortKey{{Col: -1, Desc: desc, Eval: func(f *Frame) (value.Value, error) {
		row, _ := rowOf(f)
		return row[1], nil
	}}}
	var rows Rows
	if k < 0 {
		rows = Sort(&rowsOf{rows: statusRows}, cols, keys, &Frame{})
	} else {
		rows = TopK(&rowsOf{rows: statusRows}, cols, keys, k, &Frame{})
	}
	if _, err := collect(rows); err != nil {
		t.Fatal(err)
	}
	return strings.Join(seen, " ")
}

// TestTopKWorksOutWhatSQLitesSorterDoes holds TopK to the rows whose result
// columns 0.x works out under ORDER BY status LIMIT k, which these SELECTs
// on the corpus's fixture showed, with a result column that fails for one
// row: abs(-9223372036854775807 - (key = 'docs:5')) fails at docs:5 only.
// A row's result columns are worked out when the row comes before the last
// of the best k so far, or fewer than k have come.
func TestTopKWorksOutWhatSQLitesSorterDoes(t *testing.T) {
	cases := []struct {
		k    int64
		desc bool
		want string
	}{
		{1, false, "docs:1 docs:2 docs:4 docs:7 docs:8"},
		{2, false, "docs:1 docs:2 docs:4 docs:7 docs:8"},
		{3, false, "docs:1 docs:2 docs:3 docs:4 docs:6 docs:7 docs:8"},
		{1, true, "docs:1"},
		{2, true, "docs:1 docs:2 docs:3"},
		{8, false, "docs:1 docs:2 docs:3 docs:4 docs:5 docs:6 docs:7 docs:8"},
		{-1, false, "docs:1 docs:2 docs:3 docs:4 docs:5 docs:6 docs:7 docs:8"},
	}
	for _, cs := range cases {
		if got := workedOut(t, cs.k, cs.desc); got != cs.want {
			t.Errorf("top %d, desc %v: worked out\n  %s\n  want %s", cs.k, cs.desc, got, cs.want)
		}
	}
}

// countingEval counts the rows it's worked out for.
func countingEval(n *int, ev Eval) Eval {
	return func(f *Frame) (value.Value, error) {
		*n++
		return ev(f)
	}
}

func TestSortWorksOutEveryRow(t *testing.T) {
	src := &RowSource{Name: "t", Columns: []string{"key", "n"}}
	s := &RowScope{Sources: []*RowSource{src}}
	rows := [][]value.Value{
		{value.Text("t:1"), value.Int(3)}, {value.Text("t:2"), value.Int(math.MinInt64)}, {value.Text("t:3"), value.Int(1)},
	}
	// The second row's result column fails, though it sorts last.
	cols := compileAll(t, s, "key", "abs(n)")
	keys := []SortKey{{Col: -1, Eval: compileAll(t, s, "-n")[0]}}
	if _, err := collect(Sort(&rowsOf{rows: rows}, cols, keys, &Frame{})); err == nil {
		t.Error("Sort skipped a row's result columns")
	}
	// With a LIMIT, TopK doesn't work out a row that comes after the best k.
	got, err := collect(TopK(&rowsOf{rows: rows}, cols, keys, 1, &Frame{}))
	if err != nil || rowsText(got) != "text \"t:1\", int 3\n" {
		t.Errorf("TopK gave %s%v", rowsText(got), err)
	}
	// A term that's a result column is worked out once a row, as a term.
	n := 0
	cols = []Eval{countingEval(&n, cols[0])}
	keys = []SortKey{{Col: 0, Desc: true}}
	got, err = collect(TopK(&rowsOf{rows: rows}, cols, keys, 2, &Frame{}))
	if err != nil || rowsText(got) != "text \"t:3\"\ntext \"t:2\"\n" || n != 3 {
		t.Errorf("TopK by its result column gave %s%v, working it out %d times", rowsText(got), err, n)
	}
	// A failing term fails every sort, whatever the LIMIT.
	keys = []SortKey{{Col: -1, Eval: compileAll(t, s, "abs(n)")[0]}}
	for _, k := range []int64{-1, 1, 5} {
		if _, err := collect(TopK(&rowsOf{rows: rows}, compileAll(t, s, "key"), keys, k, &Frame{})); err == nil {
			t.Errorf("top %d skipped a term", k)
		}
	}
	// TopK of 0 reads nothing.
	in := &rowsOf{rows: rows}
	if got, err := collect(TopK(in, cols, keys, 0, &Frame{})); err != nil || got != nil || in.at != 0 {
		t.Errorf("top 0 gave %v, %v, reading %d rows", got, err, in.at)
	}
}

func TestLimitAndOffsetAtTheEdges(t *testing.T) {
	rows := make([][]value.Value, 5)
	for i := range rows {
		rows[i] = named(fmt.Sprint(i + 1))
	}
	cases := []struct {
		n, skip int64
		want    string
		read    int
	}{
		{-1, 0, "1 2 3 4 5", 5}, {math.MinInt64, math.MinInt64, "1 2 3 4 5", 5}, {0, 0, "", 0}, {0, 3, "", 0},
		{1, 0, "1", 1}, {2, 1, "2 3", 3}, {2, -7, "1 2", 2}, {5, 0, "1 2 3 4 5", 5}, {6, 0, "1 2 3 4 5", 5},
		{math.MaxInt64, 0, "1 2 3 4 5", 5}, {-1, 4, "5", 5}, {-1, 5, "", 5}, {3, 5, "", 5}, {1, math.MaxInt64, "", 5},
		{math.MaxInt64, math.MaxInt64, "", 5}, {1, 4, "5", 5},
	}
	for _, cs := range cases {
		in := &rowsOf{rows: rows}
		got, err := collect(Limit(in, cs.n, cs.skip))
		var names []string
		for _, row := range got {
			names = append(names, row[0].Raw())
		}
		if err != nil || strings.Join(names, " ") != cs.want || in.at != cs.read {
			t.Errorf("LIMIT %d OFFSET %d: got %q, %v, reading %d rows; want %q, reading %d", cs.n, cs.skip, strings.Join(names, " "), err, in.at, cs.want, cs.read)
		}
	}
	// An error below comes through, from among the rows skipped too.
	boom := errors.New("boom")
	for _, skip := range []int64{0, 9} {
		if _, err := collect(Limit(&rowsOf{rows: rows, end: boom}, -1, skip)); !errors.Is(err, boom) {
			t.Errorf("OFFSET %d: an error below gave %v", skip, err)
		}
	}
}

// TestALimitStopsTheScan checks that once LIMIT's rows are out nothing more
// is read, and that the rows OFFSET skips get as far as WHERE, without
// their result columns worked out.
func TestALimitStopsTheScan(t *testing.T) {
	r := newFakeReader()
	var recs []fakeRecord
	for i := 1; i <= 9; i++ {
		n := value.Int(int64(i))
		if i == 2 {
			n = value.Int(math.MinInt64)
		}
		recs = append(recs, fakeRecord{fmt.Sprintf("t:%d", i), []value.Value{n}})
	}
	r.add("t", []string{"n"}, recs...)
	scan, cols, src := scanOf(t, r, "t", "abs(n)")
	where := conditions(t, &RowScope{Sources: []*RowSource{src}}, "n <> 4")
	got, err := collect(Project(Limit(Filter(scan, where, &Frame{}), 2, 2), cols, &Frame{}))
	if err != nil || rowsText(got) != "int 3\nint 5\n" || r.pulled != 5 {
		t.Errorf("got %s%v, reading %d records", rowsText(got), err, r.pulled)
	}
}

func TestBoundsAsSQLiteTakesThem(t *testing.T) {
	cases := []struct {
		limit, offset string
		n, skip       int64
		bad           bool
	}{
		{"", "", -1, 0, false}, {"0", "", 0, 0, false}, {"1", "", 1, 0, false}, {"-1", "-5", -1, -5, false},
		{"'1'", "", 1, 0, false}, {"' 1 '", "", 1, 0, false}, {"1.0", "", 1, 0, false}, {"'1e0'", "", 1, 0, false},
		{"'1.0'", "", 1, 0, false}, {"'-1'", "", -1, 0, false}, {"-0.0", "", 0, 0, false}, {"1e18", "", 1e18, 0, false},
		{"-9223372036854775808", "", math.MinInt64, 0, false}, {"9223372036854775807", "9223372036854775807", math.MaxInt64, math.MaxInt64, false},
		{"1.5", "", 0, 0, true}, {"NULL", "", 0, 0, true}, {"x'31'", "", 0, 0, true}, {"'0x1'", "", 0, 0, true},
		{"1e19", "", 0, 0, true}, {"'1.5'", "", 0, 0, true}, {"'1x'", "", 0, 0, true}, {"''", "", 0, 0, true},
		{"9223372036854775808", "", 0, 0, true}, {"'9223372036854775808'", "", 0, 0, true},
		{"9223372036854775807.0", "", 0, 0, true}, {"-9223372036854775808.0", "", 0, 0, true},
		{"1", "NULL", 0, 0, true}, {"1", "'a'", 0, 0, true}, {"2", "1 + 1", 2, 2, false},
		// A LIMIT of 0 leaves OFFSET unworked.
		{"0", "NULL", 0, 0, false}, {"1 - 1", "x'00'", 0, 0, false}, {"?", "abs(-9223372036854775808)", 0, 0, false},
		{"abs(-9223372036854775808)", "", 0, 0, true}, {"1", "abs(-9223372036854775808)", 0, 0, true},
	}
	for _, cs := range cases {
		q := "SELECT 1"
		if cs.limit != "" {
			q += " LIMIT " + cs.limit
			if cs.offset != "" {
				q += " OFFSET " + cs.offset
			}
		}
		st, err := Parse(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		sel := st.(*Select)
		n, skip, err := Bounds(sel.Limit, sel.Offset, nil, &Frame{Args: ints(0)})
		var pe *Error
		switch {
		case cs.bad && !errors.As(err, &pe):
			t.Errorf("%s: got %d, %d, %v, and wants an error", q, n, skip, err)
		case !cs.bad && (err != nil || n != cs.n || skip != cs.skip):
			t.Errorf("%s: got %d, %d, %v, want %d, %d", q, n, skip, err, cs.n, cs.skip)
		}
	}
}

func TestOperatorsClose(t *testing.T) {
	src := &RowSource{Name: "t", Columns: []string{"key"}}
	s := &RowScope{Sources: []*RowSource{src}}
	cols := compileAll(t, s, "key")
	rows := [][]value.Value{named("a"), named("b")}
	ops := map[string]func(Rows) Rows{
		"Filter":  func(in Rows) Rows { return Filter(in, conditions(t, s, "key <> ''"), &Frame{}) },
		"Project": func(in Rows) Rows { return Project(in, cols, &Frame{}) },
		"Sort":    func(in Rows) Rows { return Sort(in, cols, []SortKey{by(0, false)}, &Frame{}) },
		"TopK":    func(in Rows) Rows { return TopK(in, cols, []SortKey{by(0, false)}, 1, &Frame{}) },
		"Limit":   func(in Rows) Rows { return Limit(in, -1, 0) },
		"Aggregate": func(in Rows) Rows {
			aggs, err := NewAggregates([]Expr{&Call{Name: Ident{Name: "count"}, Star: true}}, s)
			if err != nil {
				t.Fatal(err)
			}
			return Aggregate(in, aggs, &Frame{})
		},
	}
	for name, op := range ops {
		in := &rowsOf{rows: rows}
		x := op(in)
		if !x.Next() {
			t.Errorf("%s gave no row", name)
		}
		x.Close()
		x.Close()
		if x.Next() || x.Err() != nil || !in.closed {
			t.Errorf("%s went on after Close, or didn't close what it reads", name)
		}
	}
	scan := Scan(docsReader(), TableSource("docs", store.Table{Name: "docs", Vec: -1}, 0))
	scan.Close()
	if scan.Next() || scan.Err() != nil {
		t.Error("a closed scan went on")
	}
}

// TestRowsFillOnSeveralGoroutines fills rows of their own from one source
// on several goroutines at once and works out one compiled condition over
// each, as a nearest search's filter may (Q5), and each gets what it gets
// alone. -race checks that Fill and the Evals only read what they share.
func TestRowsFillOnSeveralGoroutines(t *testing.T) {
	r := docsReader()
	tbl, _ := r.Table("docs")
	src := TableSource("docs", tbl, 0)
	scope := &RowScope{Sources: []*RowSource{src}}
	conds := conditions(t, scope, "n > ? AND length(vec) = 8")
	recs := r.records["docs"]
	keep := func(f *Frame, row *[]value.Value, rec store.Record) (bool, error) {
		src.Fill(*row, rec)
		for _, c := range conds {
			if ok, err := c(f); err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	}
	want := make([]bool, len(recs))
	for i, rec := range recs {
		row := make([]value.Value, len(src.Columns))
		var err error
		if want[i], err = keep(&Frame{Args: ints(2), Row: &row}, &row, rec); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(want) != "[false false false true]" {
		t.Fatalf("alone, the records give %v", want)
	}
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		go func() {
			row := make([]value.Value, len(src.Columns))
			f := &Frame{Args: ints(2), Row: &row}
			for round := 0; round < 200; round++ {
				for i, rec := range recs {
					if ok, err := keep(f, &row, rec); err != nil || ok != want[i] {
						errs <- fmt.Errorf("record %s: got %v, %v", rec.Key, ok, err)
						return
					}
				}
			}
			errs <- nil
		}()
	}
	for g := 0; g < 8; g++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
