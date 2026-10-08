// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The evaluator with a Scope, as the planner will give it one: fields, the
// one-record subquery, IN over a walk and the calls it leaves to others.
// eval_sqlite_test.go holds the same against 0.x on the corpus's fixture.

// fakeScope is a Scope over records held in memory. Frame.Row is the
// current record, and the one-record subquery and walks read the records.
// It logs the subqueries and calls it works out, in order.
type fakeScope struct {
	records map[string]fakeRow // by key
	walk    func(args []value.Value) ([]value.Value, error)
	log     []string
}

// A fakeRow is a record's fields by their folded names, key among them.
type fakeRow map[string]value.Value

func (s *fakeScope) Column(c *Column) (Eval, error) {
	name := c.Name.Folded()
	return func(f *Frame) (value.Value, error) { return f.Row.(fakeRow)[name], nil }, nil
}

func (s *fakeScope) Record(r *Record, key Eval) (RecordEval, error) {
	field, table := r.Field.Folded(), r.Table.Folded()
	return func(f *Frame) (value.Value, bool, error) {
		k, err := key(f)
		if err != nil {
			return value.Value{}, false, err
		}
		s.log = append(s.log, "record "+k.String())
		if k.Kind() != value.KindText || !strings.HasPrefix(k.Raw(), table+":") {
			return value.Value{}, false, nil
		}
		row, found := s.records[k.Raw()]
		return row[field], found, nil
	}, nil
}

func (s *fakeScope) Walk(w *Walk, args []Eval) (func(f *Frame) ([]value.Value, error), error) {
	return func(f *Frame) ([]value.Value, error) {
		vs := make([]value.Value, len(args))
		for i, a := range args {
			var err error
			if vs[i], err = a(f); err != nil {
				return nil, err
			}
		}
		s.log = append(s.log, "walk")
		return s.walk(vs)
	}, nil
}

func (s *fakeScope) Call(c *Call, args []Eval) (Eval, error) {
	name := c.Func()
	return func(f *Frame) (value.Value, error) {
		s.log = append(s.log, name)
		return value.Text(name + "()"), nil
	}, nil
}

// fakeWalk reaches docs:2 and docs:3 from docs:1, and nothing from
// anywhere else, and refuses a depth that isn't a whole number.
func fakeWalk(args []value.Value) ([]value.Value, error) {
	if args[1].Kind() != value.KindInt {
		return nil, &FuncError{Func: "walk", Msg: "walk() needs a whole number of links"}
	}
	if args[0].Kind() == value.KindText && args[0].Raw() == "docs:1" {
		return []value.Value{value.Text("docs:2"), value.Text("docs:3")}, nil
	}
	return nil, nil
}

// compileWith parses SELECT expr FROM docs and compiles its one result
// column with s.
func compileWith(t *testing.T, expr string, s Scope) Eval {
	t.Helper()
	st, err := Parse("SELECT " + expr + " FROM docs")
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	ev, err := Compile(st.(*Select).Results[0].Expr, s)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return ev
}

func TestFieldsCompareAsTheyAre(t *testing.T) {
	// n holds the text '5', the integer 5, the real 5.0, the bytes '5' and
	// NULL in turn.
	rows := []fakeRow{{"n": value.Text("5")}, {"n": value.Int(5)}, {"n": value.Real(5)}, {"n": value.Bytes("5")}, {}}
	cases := []struct {
		expr string
		want []any
	}{
		{"n = '5'", []any{1, 0, 0, 0, nil}},
		{"n = 5", []any{0, 1, 1, 0, nil}},
		{"n < 6", []any{0, 1, 1, 0, nil}},
		{"CAST(5 AS INTEGER) = n", []any{1, 1, 1, 0, nil}},
		{"n = CAST(5.0 AS REAL)", []any{1, 1, 1, 0, nil}},
		{"n = CAST('5' AS TEXT)", []any{1, 0, 0, 0, nil}},
		{"CAST(n AS TEXT) = 5", []any{1, 1, 0, 1, nil}},
		{"(n) = '5'", []any{1, 0, 0, 0, nil}},
		{"n IN ('5', 6)", []any{1, 0, 0, 0, nil}},
		{"CAST(5 AS INTEGER) IN (n, 6)", []any{1, 1, 1, 0, nil}},
		{"n BETWEEN '5' AND '5'", []any{1, 0, 0, 0, nil}},
		{"n IS 5", []any{0, 1, 1, 0, 0}},
		{"n = n", []any{1, 1, 1, 1, nil}},
		{"n + 0", []any{5, 5, 5.0, 5, nil}},
		{"nullif(n, '5')", []any{nil, 5, 5.0, []byte("5"), nil}},
	}
	s := &fakeScope{}
	for _, cs := range cases {
		ev := compileWith(t, cs.expr, s)
		for i, row := range rows {
			v, err := ev(&Frame{Row: row})
			if ok, got := sameAs(v, err, cs.want[i]); !ok {
				t.Errorf("%s with n %s: got %s, want %#v", cs.expr, row["n"], got, cs.want[i])
			}
		}
	}
}

func TestAVectorFieldReadsAsBytes(t *testing.T) {
	row := fakeRow{"vec": value.Vector([]float32{3, 4})}
	bits := "\x00\x00\x40\x40\x00\x00\x80\x40"
	cases := []struct {
		expr string
		want any
	}{
		{"typeof(vec)", "blob"}, {"length(vec)", 8}, {"vec = CAST(? AS BLOB)", 1}, {"vec || ''", bits},
		{"substr(vec, 1, 4)", []byte(bits[:4])}, {"distance(vec, '[3, 4]')", 0.0}, {"vector(vec)", []byte(bits)},
		{"vec IS NULL", 0}, {"CAST(vec AS TEXT)", bits}, {"instr(vec, CAST(? AS BLOB))", 1}, {"max(vec, 'z')", []byte(bits)},
		{"vec > 'z'", 1}, {"vec LIKE ?", 1},
	}
	s := &fakeScope{}
	for _, cs := range cases {
		ev := compileWith(t, cs.expr, s)
		v, err := ev(&Frame{Row: row, Args: []value.Value{value.Text(bits)}})
		if ok, got := sameAs(v, err, cs.want); !ok {
			t.Errorf("%s: got %s, want %#v", cs.expr, got, cs.want)
		}
	}
	// A vector given back as it is stays a vector, for the driver to hand
	// out as bytes.
	v, err := compileWith(t, "coalesce(vec, 1)", s)(&Frame{Row: row})
	if err != nil || v != row["vec"] {
		t.Errorf("coalesce(vec, 1): got %s, %v", v, err)
	}
}

func TestWithoutAScope(t *testing.T) {
	cases := []struct{ expr, msg string }{
		{"n + 1", "no such column: n"},
		{"docs.n", "no such column: docs.n"},
		{"(SELECT n FROM docs WHERE key = 'docs:1')", "no such table: docs"},
		{"'docs:1' IN (SELECT key FROM walk('docs:1', 1))", "no such table: walk"},
		{"walk('docs:1', 1)", "walk() needs the statement's planner"},
		{"count(*)", "count() needs the statement's planner"},
		{"max(n)", "no such column: n"},
		{"max(1)", "max() needs the statement's planner"},
	}
	for _, cs := range cases {
		st, err := Parse("SELECT " + cs.expr + " FROM docs")
		if err != nil {
			t.Fatalf("%s: %v", cs.expr, err)
		}
		_, err = Compile(st.(*Select).Results[0].Expr, nil)
		var pe *Error
		if !errors.As(err, &pe) || pe.Msg != cs.msg {
			t.Errorf("%s: got %v, want %q", cs.expr, err, cs.msg)
		}
	}
}

func TestCallsGoToTheScope(t *testing.T) {
	s := &fakeScope{}
	ev := compileWith(t, "date('now') || walk('docs:1', 2) || count(*) || sum(n) || min(n) || datetime('now')", s)
	// The evaluator works out the dates itself (Q2), from the Frame's moment.
	v, err := ev(&Frame{Row: fakeRow{}, Now: time.Date(2026, 10, 8, 9, 5, 7, 0, time.UTC)})
	if err != nil || v != value.Text("2026-10-08walk()count()sum()min()2026-10-08 09:05:07") {
		t.Errorf("got %s, %v", v, err)
	}
	if got := strings.Join(s.log, " "); got != "walk count sum min" {
		t.Errorf("worked out %s", got)
	}
}

// TestWhatASubqueryWaitsFor checks SQLite's order around the one-record
// subquery and IN over a walk (exprComputeOperands, exprCodeTargetAndOr and
// sqlite3ExprCodeIN): the side without one goes first, and a subquery isn't
// worked out when that side settles the answer.
func TestWhatASubqueryWaitsFor(t *testing.T) {
	const sub = "(SELECT n FROM docs WHERE key = 'docs:1')"
	const walk = "(SELECT key FROM walk('docs:1', 1))"
	cases := []struct {
		expr string
		want any
		log  string
	}{
		{sub + " + 1", 4, "record text \"docs:1\""},
		{sub + " + NULL", nil, ""},
		{"NULL + " + sub, nil, ""},
		{"n + " + sub, nil, ""},
		{sub + " IS NULL", 0, "record text \"docs:1\""},
		{"NULL IS " + sub, 0, "record text \"docs:1\""},
		{sub + " = -NULL", nil, ""},
		{sub + " = -5", 0, "record text \"docs:1\""},
		{sub + " AND (1 = 0)", 0, ""},
		{sub + " OR (1 = 1)", 1, ""},
		{"(1 = 1) AND " + sub, 1, "record text \"docs:1\""},
		{sub + " AND 0", 0, ""},
		{"walk('docs:1', 2) = " + sub, 0, "walk record text \"docs:1\""},
		{sub + " = walk('docs:1', 2)", 0, "walk record text \"docs:1\""},
		{"'docs:2' IN " + walk, 1, "walk"},
		{"'docs:9' NOT IN " + walk, 1, "walk"},
		{"NULL IN " + walk, nil, "walk"},
		{"NULL IN (SELECT key FROM walk('docs:9', 1))", 0, "walk"},
		{"2 IN " + walk, 0, "walk"},
		{"CAST('docs:2' AS BLOB) IN " + walk, 0, "walk"},
		{"walk('docs:1', 2) IN " + walk, 0, "walk walk"},
		{"NULL + ('docs:2' IN " + walk + ")", nil, ""},
		{sub + " IN (1, 3, 5)", 1, "record text \"docs:1\""},
		{"3 IN (" + sub + ", walk('docs:1', 2))", 1, "record text \"docs:1\""},
		// SQLite's parser makes x IN ((SELECT ...)) IN over the subquery:
		// the subquery first, and no record is an empty set.
		{"walk('docs:1', 2) IN (" + sub + ")", 0, "record text \"docs:1\" walk"},
		{"NULL IN ((SELECT n FROM docs WHERE key = 'docs:9'))", 0, "record text \"docs:9\""},
		{"NULL NOT IN ((SELECT n FROM docs WHERE key = 'docs:9'))", 1, "record text \"docs:9\""},
		{"NULL IN (" + sub + ")", nil, "record text \"docs:1\""},
		{"3 IN ((SELECT data FROM docs WHERE key = 'docs:1'))", nil, "record text \"docs:1\""},
		{"'3' IN (" + sub + ")", 0, "record text \"docs:1\""},
		{"CAST('3' AS INTEGER) IN (" + sub + ")", 1, "record text \"docs:1\""},
		{"CAST(3 AS TEXT) IN (" + sub + ")", 0, "record text \"docs:1\""},
		{"CAST(3 AS TEXT) IN (" + sub + ", 0)", 1, "record text \"docs:1\""},
		{"3 IN (+" + sub + ")", 1, "record text \"docs:1\""},
	}
	for _, cs := range cases {
		s := &fakeScope{records: map[string]fakeRow{"docs:1": {"key": value.Text("docs:1"), "n": value.Int(3)}}, walk: fakeWalk}
		ev := compileWith(t, cs.expr, s)
		v, err := ev(&Frame{Row: fakeRow{}})
		if ok, got := sameAs(v, err, cs.want); !ok {
			t.Errorf("%s: got %s, want %#v", cs.expr, got, cs.want)
		}
		if got := strings.Join(s.log, " "); got != cs.log {
			t.Errorf("%s: worked out %q, want %q", cs.expr, got, cs.log)
		}
	}
}

func TestInOverAWalkWorksOutTheWalkFirst(t *testing.T) {
	s := &fakeScope{walk: fakeWalk}
	ev := compileWith(t, "abs(-9223372036854775808) IN (SELECT key FROM walk('docs:1', 2.0))", s)
	_, err := ev(&Frame{Row: fakeRow{}})
	var fe *FuncError
	if !errors.As(err, &fe) {
		t.Errorf("got %v, want the walk's error", err)
	}
}
