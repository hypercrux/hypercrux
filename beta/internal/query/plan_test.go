// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The plan-shape tests of Q5: each case in the contract, as the plan
// Prepare makes, the operators String writes, and the answer Run gives on
// the corpus's fixture. plan_sqlite_test.go holds every answer to 0.x's.

// planStore is the corpus's fixture in a store, with one more table,
// empty, which has held a record with the fields n and vec.
func planStore(t testing.TB) *store.Store {
	t.Helper()
	s := fixtureStore(t)
	fields, err := storeFields(c.Fields{"n": int64(1), "vec": c.Vector{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(nil, "empty:1", fields); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(nil, "empty:1"); err != nil {
		t.Fatal(err)
	}
	return s
}

// planCase is a statement, the plan it gets and its answer, as answerText
// writes it. zero is the statement as 0.x runs it, when that differs: a
// walk as a table or IN over one written with json_each, ties settled by
// key or by the walk's order, or "-" when 0.x can't run it or answers
// otherwise on purpose, as SQL.md's "Where the Beta differs from 0.x" has
// it. plan, when it isn't empty, is the whole of the plan's String.
type planCase struct {
	sql   string
	args  []any
	shape Shape
	plan  string
	want  string
	zero  string
}

// One case's lines, for want: values as value.String writes them.
func lines(ls ...string) string { return strings.Join(ls, "\n") }

const (
	d1 = `text "docs:1"`
	d2 = `text "docs:2"`
	d3 = `text "docs:3"`
	d4 = `text "docs:4"`
	d5 = `text "docs:5"`
	d6 = `text "docs:6"`
	d7 = `text "docs:7"`
	d8 = `text "docs:8"`
	p2 = `text "people:2"`
)

var planCases = []planCase{
	// The nearest search, for ORDER BY distance() with vec IS NOT NULL and a
	// LIMIT. docs:1 and docs:7 hold the same vector, so they tie, and the
	// search breaks ties by key, as a stable sort over the table does.
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]') LIMIT 3", shape: ShapeNearest,
		plan: lines("nearest docs by distance(vec, '[1, 0, 0]')", "limit", "project key"),
		want: lines(d1, d7, d3), zero: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]'), key LIMIT 3"},
	{sql: "SELECT key, title FROM docs WHERE status = 'open' AND vec IS NOT NULL ORDER BY distance(vec, ?) LIMIT 2", args: []any{c.Vector{1, 0, 0}}, shape: ShapeNearest,
		plan: lines("nearest docs by distance(vec, ?) through status = 'open'", "limit", "project key, title"),
		want: lines(`text "docs:1", text "Q3 plan"`, `text "docs:3", text "Q3 budget"`)},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]') LIMIT 2 OFFSET 1", shape: ShapeNearest,
		plan: lines("nearest docs by distance(vec, '[1, 0, 0]')", "limit and offset", "project key"),
		want: lines(d7, d3), zero: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]'), key LIMIT 2 OFFSET 1"},
	{sql: "SELECT key, distance(vec, '[0, 0, 1]') AS d FROM docs WHERE vec IS NOT NULL ORDER BY d LIMIT 2", shape: ShapeNearest, want: lines(`text "docs:6", real 0 (0000000000000000)`, `text "docs:5", real 0.42264973081037416 (3fdb0cb174df99c6)`)},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance('[0, 1, 0]', vec), key LIMIT 1", shape: ShapeNearest, want: d2},
	{sql: "SELECT key, n FROM docs WHERE vec IS NOT NULL ORDER BY 2 LIMIT 1", shape: ShapeScan, want: `text "docs:6", int -2`},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM docs WHERE key = 'docs:6')) LIMIT 2", shape: ShapeNearest,
		plan: lines("nearest docs by distance(vec, (SELECT vec FROM docs WHERE key = 'docs:6'))", "limit", "project key", "once (SELECT vec FROM docs WHERE key = 'docs:6')"),
		want: lines(d6, d5)},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM docs WHERE key = ?)) LIMIT 3", args: []any{"docs:3"}, shape: ShapeNearest,
		want: lines(d3, d1, d7), zero: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM docs WHERE key = ?)), key LIMIT 3"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL AND abs(-9223372036854775807 - (key = 'docs:6')) ORDER BY distance(vec, '[1, 0, 0]') LIMIT 1", shape: ShapeNearest, want: "error"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL AND n IN (SELECT key FROM walk('people:1', 1)) ORDER BY distance(vec, '[1, 0, 0]') LIMIT 1", shape: ShapeNearest, want: "",
		zero: "SELECT key FROM docs WHERE vec IS NOT NULL AND n IN (SELECT value FROM json_each(walk('people:1', 1))) ORDER BY distance(vec, '[1, 0, 0]') LIMIT 1"},
	// The search can't take these query vectors, so it sorts, and gives
	// what 0.x gives: NULL ties every row, a vector of another size or text
	// that isn't one fails at the first row that passes WHERE, and none
	// when no row does, and zeros given as bytes are distance 1 to all.
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM docs WHERE key = 'docs:4')) LIMIT 2", shape: ShapeNearest, want: lines(d1, d2)},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM t_1 WHERE key = 't_1:a')) LIMIT 2", shape: ShapeNearest, want: "error"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0]') LIMIT 2", shape: ShapeNearest, want: "error"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL AND n > 100 ORDER BY distance(vec, '[1, 0]') LIMIT 2", shape: ShapeNearest, want: ""},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, ?) LIMIT 3", args: []any{c.Vector{0, 0, 0}}, shape: ShapeNearest, want: lines(d1, d2, d3)},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[0, 0, 0]') LIMIT 3", shape: ShapeNearest, want: "error"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, 'oops') LIMIT 3", shape: ShapeNearest, want: "error"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL AND key = 'docs:4' ORDER BY distance(vec, 'oops') LIMIT 3", shape: ShapeLookup, want: ""},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL AND status = 'gone' ORDER BY distance(vec, 'oops') LIMIT 3", shape: ShapeNearest, want: ""},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, 5) LIMIT 3", shape: ShapeNearest, want: "error"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]') LIMIT -1", shape: ShapeNearest, want: lines(d1, d7, d3, d5, d2, d6),
		zero: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]'), key LIMIT -1"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]') LIMIT 10001", shape: ShapeNearest, want: lines(d1, d7, d3, d5, d2, d6),
		zero: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]'), key LIMIT 10001"},
	// Not a nearest search: no LIMIT, no vec IS NOT NULL, going down, a
	// second term other than the key, a term before vec IS NOT NULL that
	// can fail, or a result column that can, which the sort works out for
	// the rows it keeps as they come.
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]')", shape: ShapeScan,
		plan: lines("scan docs", "filter vec IS NOT NULL", "sort by distance(vec, '[1, 0, 0]')", "project key"),
		want: lines(d1, d7, d3, d5, d2, d6), zero: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]'), key"},
	{sql: "SELECT key FROM docs ORDER BY distance(vec, '[1, 0, 0]') LIMIT 4", shape: ShapeScan,
		plan: lines("scan docs", "top k by distance(vec, '[1, 0, 0]')", "limit", "project key"),
		want: lines(d4, d8, d1, d7), zero: "SELECT key FROM docs ORDER BY distance(vec, '[1, 0, 0]'), key LIMIT 4"},
	{sql: "SELECT key FROM docs ORDER BY distance(vec, '[1, 0, 0]')", shape: ShapeScan, want: lines(d4, d8, d1, d7, d3, d5, d2, d6),
		zero: "SELECT key FROM docs ORDER BY distance(vec, '[1, 0, 0]'), key"},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]') DESC LIMIT 2", shape: ShapeScan, want: lines(d6, d2)},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]'), n LIMIT 2", shape: ShapeScan, want: lines(d1, d7)},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, vec) LIMIT 2", shape: ShapeScan, want: lines(d1, d2)},
	{sql: "SELECT key FROM docs WHERE abs(-9223372036854775807 - (key = 'docs:4')) AND vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]') LIMIT 2", shape: ShapeScan, want: "error"},
	{sql: "SELECT key, abs(-9223372036854775807 - (key = 'docs:1')) FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[0, 1, 0]') LIMIT 1", shape: ShapeScan, want: "error"},
	{sql: "SELECT key, abs(-9223372036854775807 - (key = 'docs:2')) FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]') LIMIT 1", shape: ShapeScan, want: `text "docs:1", int 9223372036854775807`},

	// Walk joins: the walk, its terms alone, the table's record for each
	// step, the other terms. The crux query sorts the join's rows.
	{sql: "SELECT d.key, d.title FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value WHERE d.status = 'open' AND d.vec IS NOT NULL ORDER BY distance(d.vec, '[1, 0, 0]') LIMIT 10", shape: ShapeWalkJoin,
		plan: lines("walk json_each(walk('people:1', 2)) AS w", "join docs AS d ON d.key = w.value", "filter d.status = 'open' AND d.vec IS NOT NULL",
			"top k by distance(d.vec, '[1, 0, 0]')", "limit", "project key, title"),
		want: lines(`text "docs:1", text "Q3 plan"`, `text "docs:3", text "Q3 budget"`)},
	{sql: "SELECT d.key, d.title FROM walk('people:1', 2) w JOIN docs d ON d.key = w.key ORDER BY d.key", shape: ShapeWalkJoin, want: lines(`text "docs:1", text "Q3 plan"`, `text "docs:2", text "Hiring notes"`, `text "docs:3", text "Q3 budget"`),
		zero: "SELECT d.key, d.title FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value ORDER BY d.key"},
	{sql: "SELECT d.key FROM docs d JOIN json_each(walk('people:3', 2)) w ON w.value = d.key ORDER BY d.key", shape: ShapeWalkJoin, want: lines(d1, d2, d5, d7)},
	{sql: "SELECT d.key FROM docs d JOIN json_each(walk('people:3', 2)) w ON (w.value = d.key) LIMIT 3", shape: ShapeWalkJoin, want: lines(d5, d1, d2),
		zero: "SELECT d.key FROM docs d JOIN json_each(walk('people:3', 2)) w ON (w.value = d.key) ORDER BY w.key LIMIT 3"},
	{sql: "SELECT w.depth, d.key FROM walk('people:3', 2) w JOIN docs d ON d.key = w.key", shape: ShapeWalkJoin, want: lines(`int 1, text "docs:5"`, `int 2, text "docs:1"`, `int 2, text "docs:2"`, `int 2, text "docs:7"`), zero: "-"},
	{sql: "SELECT d.key FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value WHERE abs(-9223372036854775807 - (w.value = 'people:2')) ORDER BY d.key", shape: ShapeWalkJoin,
		plan: lines("walk json_each(walk('people:1', 2)) AS w", "filter abs(-9223372036854775807 - (w.value = 'people:2'))", "join docs AS d ON d.key = w.value",
			"sort by d.key", "project key"),
		want: "error"},
	{sql: "SELECT d.key FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value WHERE abs(-9223372036854775807 - (d.key = 'docs:2')) ORDER BY d.key", shape: ShapeWalkJoin, want: "error"},
	{sql: "SELECT count(*), sum(d.n) FROM json_each(walk('people:1', 3)) w JOIN docs d ON d.key = w.value WHERE d.status = 'open'", shape: ShapeWalkJoin, want: `int 3, int 20`},
	{sql: "SELECT d.key, w.value FROM docs d JOIN json_each(walk('docs:3', 1, NULL, 'both')) w ON d.key = w.value ORDER BY distance(d.vec, ?) LIMIT 1", args: []any{c.Vector{0, 0, 1}}, shape: ShapeWalkJoin, want: `text "docs:5", text "docs:5"`},

	// IN and NOT IN over walks. IN on the key reads the records the walk
	// reached, in key order, as SQLite looks them up in the key's index, so
	// the other terms never meet the rest; NOT IN and IN on anything else
	// are terms of the scan.
	{sql: "SELECT key FROM docs WHERE key IN (SELECT key FROM walk('people:1', 2)) ORDER BY key", shape: ShapeLookup,
		plan: lines("lookup docs by key IN (SELECT key FROM walk('people:1', 2))", "project key", "once walk('people:1', 2)"),
		want: lines(d1, d2, d3), zero: "SELECT key FROM docs WHERE key IN (SELECT value FROM json_each(walk('people:1', 2))) ORDER BY key"},
	{sql: "SELECT key FROM docs WHERE key NOT IN (SELECT value FROM json_each(walk('people:1', 2))) ORDER BY key", shape: ShapeScan,
		plan: lines("scan docs", "filter key NOT IN (SELECT value FROM json_each(walk('people:1', 2)))", "project key", "once json_each(walk('people:1', 2))"),
		want: lines(d4, d5, d6, d7, d8)},
	{sql: "SELECT key FROM docs WHERE abs(-9223372036854775807 - (key = 'docs:8')) AND key IN (SELECT value FROM json_each(walk('people:1', 2))) ORDER BY key", shape: ShapeLookup, want: lines(d1, d2, d3)},
	{sql: "SELECT key FROM docs WHERE vec IS NOT NULL AND key IN (SELECT value FROM json_each(walk('people:1', 2))) ORDER BY key DESC LIMIT 2", shape: ShapeLookup,
		plan: lines("lookup docs by key IN (SELECT value FROM json_each(walk('people:1', 2))), backwards", "filter vec IS NOT NULL", "limit", "project key", "once json_each(walk('people:1', 2))"),
		want: lines(d3, d2)},
	{sql: "SELECT key, title FROM docs WHERE title IN (SELECT value FROM json_each(walk('people:1', 1))) ORDER BY key", shape: ShapeScan, want: ""},
	{sql: "SELECT value FROM json_each(walk('people:1', 2)) WHERE value NOT IN (SELECT value FROM json_each(walk('people:1', 1))) ORDER BY value", shape: ShapeWalk, want: d3},
	{sql: "SELECT key FROM docs WHERE status NOT IN (SELECT key FROM walk('people:1', 1)) ORDER BY key", shape: ShapeScan, want: lines(d1, d2, d3, d4, d5, d6, d7),
		zero: "SELECT key FROM docs WHERE status NOT IN (SELECT value FROM json_each(walk('people:1', 1))) ORDER BY key"},
	{sql: "SELECT key FROM docs WHERE status NOT IN (SELECT key FROM walk('docs:6', 1)) ORDER BY key", shape: ShapeScan, want: lines(d1, d2, d3, d4, d5, d6, d7, d8),
		zero: "SELECT key FROM docs WHERE status NOT IN (SELECT value FROM json_each(walk('docs:6', 1))) ORDER BY key"},
	{sql: "SELECT key FROM empty WHERE key IN (SELECT value FROM json_each(walk('people:1', 0)))", shape: ShapeLookup, want: "error"},
	{sql: "SELECT key FROM empty WHERE key NOT IN (SELECT value FROM json_each(walk('people:1', 0)))", shape: ShapeScan, want: ""},

	// The one-record subquery, worked out once for the statement.
	{sql: "SELECT (SELECT title FROM docs WHERE key = 'docs:1')", shape: ShapeRow,
		plan: lines("row", "project (SELECT title FROM docs WHERE key = 'docs:1')", "once (SELECT title FROM docs WHERE key = 'docs:1')"), want: `text "Q3 plan"`},
	{sql: "SELECT key FROM docs WHERE n > (SELECT n FROM docs WHERE key = 'docs:1') ORDER BY key", shape: ShapeScan,
		plan: lines("scan docs", "filter n > (SELECT n FROM docs WHERE key = 'docs:1')", "project key", "once (SELECT n FROM docs WHERE key = 'docs:1')"),
		want: lines(d3, d5, d7, d8)},
	{sql: "SELECT key FROM docs WHERE key = (SELECT key FROM docs WHERE key = 'docs:2')", shape: ShapeLookup, want: d2},
	{sql: "SELECT key FROM empty WHERE key = (SELECT key FROM docs WHERE key = abs(-9223372036854775808))", shape: ShapeLookup, want: "error"},
	{sql: "SELECT (SELECT key FROM docs WHERE key = 'people:1'), (SELECT name FROM people WHERE key = 'people:1')", shape: ShapeRow, want: `null, text "Dana"`},
	{sql: "SELECT key FROM docs WHERE n IN ((SELECT n FROM docs WHERE key = 'docs:8')) ORDER BY key", shape: ShapeScan, want: d8},
	{sql: "SELECT key FROM docs WHERE key IN ((SELECT key FROM docs WHERE key = 'docs:5'))", shape: ShapeLookup, want: d5},
	{sql: "SELECT key FROM docs WHERE (SELECT n FROM docs WHERE key = abs(-9223372036854775808)) AND 0", shape: ShapeScan, want: ""},
	{sql: "SELECT key FROM empty WHERE (SELECT n FROM docs WHERE key = abs(-9223372036854775808))", shape: ShapeScan, want: ""},
	{sql: "SELECT key FROM docs WHERE (SELECT n FROM docs WHERE key = abs(-9223372036854775808))", shape: ShapeScan, want: "error"},

	// WHERE's terms that read no table are worked out once, before any row.
	{sql: "SELECT key FROM empty WHERE abs(-9223372036854775808)", shape: ShapeScan,
		plan: lines("once abs(-9223372036854775808)", "scan empty", "project key"), want: "error"},
	{sql: "SELECT count(*) FROM docs WHERE 0", shape: ShapeScan, want: "int 0"},
	{sql: "SELECT count(*) FROM empty WHERE abs(-9223372036854775808)", shape: ShapeScan, want: "error"},
	{sql: "SELECT value FROM json_each(walk('people:1', 0)) WHERE 1 = 0", shape: ShapeWalk, want: ""},
	{sql: "SELECT key FROM docs WHERE abs(-9223372036854775808) LIMIT 0", shape: ShapeScan, want: ""},
	{sql: "SELECT key FROM docs WHERE abs(-9223372036854775807 - (key = 'docs:1')) AND 0", shape: ShapeScan,
		plan: lines("once 0", "scan docs", "filter abs(-9223372036854775807 - (key = 'docs:1'))", "project key"), want: ""},
	{sql: "SELECT key FROM docs WHERE abs(-9223372036854775808) AND 0", shape: ShapeScan, want: "error"},
	{sql: "SELECT key FROM empty WHERE walk('people:1', 0) = 'x'", shape: ShapeScan, want: ""},

	// The walk as a table, and walk() as a function.
	{sql: "SELECT key, depth FROM walk('people:3', 2)", shape: ShapeWalk,
		plan: lines("walk walk('people:3', 2)", "project key, depth"),
		want: lines(`text "docs:5", int 1`, `text "people:1", int 1`, `text "docs:1", int 2`, `text "docs:2", int 2`, `text "docs:7", int 2`, `text "people:2", int 2`), zero: "-"},
	{sql: "SELECT * FROM walk('t_1:a', 3, 'next') w", shape: ShapeWalk, want: lines(`text "t_1:b", int 1`, `text "t_1:c", int 2`), zero: "-"},
	{sql: "SELECT key FROM walk('people:3', 2) WHERE depth = 2 ORDER BY key DESC LIMIT 2", shape: ShapeWalk, want: lines(p2, d7), zero: "-"},
	{sql: "SELECT value FROM json_each(walk('people:1', 2)) ORDER BY value DESC LIMIT 2", shape: ShapeWalk, want: lines(p2, d3)},
	{sql: "SELECT abs(-9223372036854775807 - (value = 'docs:3')) FROM json_each(walk('people:1', 2)) ORDER BY value DESC LIMIT 1", shape: ShapeWalk, want: "int 9223372036854775807"},
	{sql: "SELECT count(*) FROM walk('docs:404', 2)", shape: ShapeWalk, want: "int 0", zero: "SELECT count(*) FROM json_each(walk('docs:404', 2))"},
	{sql: "SELECT count(*) FROM walk('people:1', 33)", shape: ShapeWalk, want: "error", zero: "SELECT count(*) FROM json_each(walk('people:1', 33))"},
	{sql: "SELECT count(*) FROM walk(1, 1)", shape: ShapeWalk, want: "error", zero: "SELECT count(*) FROM json_each(walk(1, 1))"},
	{sql: "SELECT key, walk(key, 1, 'cites') FROM docs WHERE key < 'docs:4' ORDER BY key", shape: ShapeScan, want: lines(`text "docs:1", text "[\"docs:3\"]"`, `text "docs:2", text "[\"docs:1\"]"`, `text "docs:3", text "[\"docs:5\"]"`)},
	{sql: "SELECT walk('people:1', 2, NULL, 'OUT'), walk('nocolon', 1), walk('people:1', 1, '')", shape: ShapeRow, want: `text "[\"docs:1\",\"docs:2\",\"people:2\",\"docs:3\"]", text "[]", text "[\"docs:1\",\"docs:2\",\"people:2\"]"`},
	{sql: "SELECT walk('people:1', 0, 5)", shape: ShapeRow, want: "error"},

	// ORDER BY the key is the order the records come in already: forwards,
	// or backwards going down, so it sorts nothing, works out no other term
	// and leaves the result columns of the rows OFFSET skips unworked.
	{sql: "SELECT key, abs(-9223372036854775807 - (key = 'docs:1')) FROM docs ORDER BY key LIMIT 1 OFFSET 1", shape: ShapeScan,
		plan: lines("scan docs", "limit and offset", "project key, abs(-9223372036854775807 - (key = 'docs:1'))"), want: d2 + ", int 9223372036854775807"},
	{sql: "SELECT key, abs(-9223372036854775807 - (key = 'docs:8')) FROM docs ORDER BY key DESC LIMIT 1 OFFSET 1", shape: ShapeScan, want: d7 + ", int 9223372036854775807"},
	{sql: "SELECT key AS k FROM docs ORDER BY k, abs(-9223372036854775808) LIMIT 2", shape: ShapeScan, want: lines(d1, d2)},
	{sql: "SELECT key FROM docs ORDER BY 1 DESC LIMIT 2", shape: ShapeScan, want: lines(d8, d7)},

	// Key lookups for key = e and key IN (...).
	{sql: "SELECT key FROM docs WHERE abs(-9223372036854775807 - (key = 'docs:8')) AND key = 'docs:1'", shape: ShapeLookup,
		plan: lines("lookup docs by key = 'docs:1'", "filter abs(-9223372036854775807 - (key = 'docs:8'))", "project key"), want: d1},
	{sql: "SELECT key FROM docs WHERE key IN ('docs:2', 'docs:1', 5, NULL, 'people:1') AND abs(-9223372036854775807 - (key = 'docs:3'))", shape: ShapeLookup, want: lines(d1, d2)},
	{sql: "SELECT key FROM empty WHERE key = abs(-9223372036854775808)", shape: ShapeLookup, want: "error"},
	{sql: "SELECT key FROM docs WHERE 'docs:404' = key", shape: ShapeLookup, want: ""},
	{sql: "SELECT key FROM docs WHERE key = CAST('docs:1' AS BLOB)", shape: ShapeLookup, want: ""},

	// No FROM: one row, and ORDER BY, which SQLite never works out there.
	{sql: "SELECT 1 ORDER BY abs(-9223372036854775808)", shape: ShapeRow, want: "int 1"},
	{sql: "SELECT count(*) ORDER BY sum(abs(-9223372036854775808))", shape: ShapeRow, want: "error"},
	{sql: "SELECT count(*) WHERE 0", shape: ShapeRow, want: "int 0"},
	{sql: "SELECT abs(-9223372036854775808) LIMIT 1 OFFSET 1", shape: ShapeRow, want: ""},

	// ORDER BY's expressions: a field first, then a result column's alias,
	// which stands for its expression, affinity and all.
	{sql: "SELECT n * 2 AS twice FROM docs WHERE n > 5 ORDER BY twice + 0", shape: ShapeScan, want: lines(`int 10`, `int 14`, `int 20`, `int 84`)},
	{sql: "SELECT n AS title FROM docs WHERE n > 5 ORDER BY title || ''", shape: ShapeScan, want: lines(`text "5"`, `int 7`, `int 42`, `int 10`)},
	{sql: "SELECT key, CAST(n AS INTEGER) AS c FROM docs ORDER BY c = '5' DESC, key LIMIT 1", shape: ShapeScan, want: `text "docs:8", int 5`},
	{sql: "SELECT key FROM docs ORDER BY n AND 0", want: "error"},
	{sql: "SELECT count(*) FROM docs ORDER BY n", want: "error", zero: "-"},

	// Names: json_each's columns clash as in SQLite, and only value may be
	// used; two sources of one name make their columns ambiguous.
	{sql: "SELECT key FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value", want: "error"},
	{sql: "SELECT title FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value ORDER BY title", shape: ShapeWalkJoin, want: lines(`text "Hiring notes"`, `text "Q3 budget"`, `text "Q3 plan"`)},
	{sql: "SELECT d.key FROM json_each(walk('people:1', 2)) d JOIN docs d ON d.key = d.value", want: "error"},
	{sql: "SELECT type FROM json_each(walk('people:1', 1))", want: "error", zero: "-"},
	{sql: "SELECT json FROM json_each(walk('people:1', 1)) w JOIN docs d ON d.key = w.value", want: "error", zero: "-"},
	{sql: "SELECT d.key FROM docs d JOIN json_each(walk('people:1', 2)) w ON d.title = w.value", want: "error", zero: "-"},
	{sql: "SELECT d.key FROM docs d JOIN json_each(walk('people:1', 2)) w ON d.key = w.key", want: "error", zero: "-"},

	// x AND 0 folded as SQLite's parser folds it, so an IN list's items are
	// constant and all worked out.
	{sql: "SELECT 1 IN (1, (n AND 0), abs(-9223372036854775808)) FROM docs WHERE key = 'docs:1'", shape: ShapeLookup, want: "error"},
	{sql: "SELECT 1 IN (1, ((SELECT n FROM docs WHERE key = 'docs:1') AND 0), abs(-9223372036854775808))", shape: ShapeRow, want: "error"},
}

// planArgs makes a case's arguments values, through Arg.
func planArgs(args []any) []value.Value {
	vals := make([]value.Value, len(args))
	for i, a := range args {
		vals[i] = argValue(a)
	}
	return vals
}

var planNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// TestPlanShapes is the plan-shape test: each statement gets the plan its
// case names, and gives its answer.
func TestPlanShapes(t *testing.T) {
	s := planStore(t)
	for _, cs := range planCases {
		st, err := Parse(cs.sql)
		if err != nil {
			t.Fatalf("%s: %v", cs.sql, err)
		}
		p, err := Prepare(s, st.(*Select))
		switch {
		case err != nil && cs.shape != "":
			t.Errorf("%s: %v", cs.sql, err)
		case err == nil && cs.shape != p.Shape():
			t.Errorf("%s: shape %q, want %q\n%s", cs.sql, p.Shape(), cs.shape, p)
		case err == nil && cs.plan != "" && cs.plan != p.String():
			t.Errorf("%s:\n  plan\n%s\n  want\n%s", cs.sql, indent(p.String()), indent(cs.plan))
		}
		if got := answerText(planQuery(s, cs.sql, planArgs(cs.args), planNow)); got != cs.want {
			t.Errorf("%s:\n  got  %s\n  want %s", cs.sql, strings.ReplaceAll(got, "\n", " / "), strings.ReplaceAll(cs.want, "\n", " / "))
		}
	}
}

func indent(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") }

// countingReader counts the calls a statement makes to the store: Get, by
// key, and Walk.
type countingReader struct {
	store.Reader
	gets  map[string]int
	walks int
}

func (r *countingReader) Get(key string) (store.Record, error) {
	r.gets[key]++
	return r.Reader.Get(key)
}

func (r *countingReader) Walk(key string, dir store.Direction, typ string, depth int) ([]store.Step, error) {
	r.walks++
	return r.Reader.Walk(key, dir, typ, depth)
}

// TestSubqueriesAreWorkedOutOnce checks SQL.md's rule that the one-record
// subquery and IN over a walk are worked out once for each statement, the
// first time a row reaches them, where walk() as a function is worked out
// for each row, and a subquery no row reaches isn't worked out at all.
func TestSubqueriesAreWorkedOutOnce(t *testing.T) {
	s := planStore(t)
	cases := []struct {
		sql         string
		key         string // the key whose reads are counted
		gets, walks int
		rows        int
	}{
		{"SELECT key FROM docs WHERE n > (SELECT n FROM docs WHERE key = 'docs:1')", "docs:1", 1, 0, 4},
		{"SELECT key, (SELECT title FROM docs WHERE key = 'docs:1') FROM docs", "docs:1", 1, 0, 8},
		{"SELECT key FROM docs WHERE title = 'none' AND n > (SELECT n FROM docs WHERE key = 'docs:1')", "docs:1", 0, 0, 0},
		{"SELECT key FROM docs WHERE status NOT IN (SELECT key FROM walk('people:1', 2))", "docs:1", 0, 1, 7},
		{"SELECT key FROM docs WHERE 'docs:1' IN (SELECT key FROM walk('people:1', 1)) AND n IS NOT NULL", "docs:1", 0, 1, 7},
		{"SELECT walk('people:1', 1) FROM docs", "docs:1", 0, 8, 8},
		{"SELECT key FROM docs WHERE key IN (SELECT key FROM walk('people:1', 2))", "docs:1", 1, 1, 3},
		{"SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM docs WHERE key = 'docs:5')) LIMIT 1", "docs:5", 2, 0, 1},
		{"SELECT d.key FROM walk('people:1', 2) w JOIN docs d ON d.key = w.key WHERE d.n > (SELECT n FROM docs WHERE key = 'docs:2')", "docs:2", 2, 1, 2},
	}
	for _, cs := range cases {
		r := &countingReader{Reader: s, gets: map[string]int{}}
		ans := planQuery(r, cs.sql, nil, planNow)
		if ans.Error != "" || len(ans.Rows) != cs.rows || r.gets[cs.key] != cs.gets || r.walks != cs.walks {
			t.Errorf("%s: %d rows, %v, with Get of %s %d times and Walk %d; want %d rows, %d and %d", cs.sql, len(ans.Rows), ans.Message, cs.key, r.gets[cs.key], r.walks, cs.rows, cs.gets, cs.walks)
		}
	}
}

// TestPlansReadThroughATransaction runs statements through a store.Tx,
// which sees its own changes, and through the store after the rollback.
func TestPlansReadThroughATransaction(t *testing.T) {
	s := planStore(t)
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	fields, err := storeFields(c.Fields{"title": "new", "status": "open", "vec": c.Vector{1, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Put("docs:0", fields); err != nil {
		t.Fatal(err)
	}
	if err := tx.Link("people:1", "owns", "docs:0"); err != nil {
		t.Fatal(err)
	}
	crux := "SELECT d.key FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value WHERE d.status = 'open' AND d.vec IS NOT NULL ORDER BY distance(d.vec, '[1, 0, 0]') LIMIT 2"
	near := "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0, 0]') LIMIT 2"
	for _, q := range []struct{ sql, inside, after string }{
		{crux, lines(`text "docs:0"`, d1), lines(d1, d3)},
		{near, lines(`text "docs:0"`, d1), lines(d1, d7)},
		{"SELECT count(*) FROM docs WHERE key IN (SELECT key FROM walk('people:1', 1))", "int 3", "int 2"},
	} {
		if got := answerText(planQuery(tx, q.sql, nil, planNow)); got != q.inside {
			t.Errorf("%s inside the transaction: %q, want %q", q.sql, got, q.inside)
		}
		defer func(q string, want string) {
			if got := answerText(planQuery(s, q, nil, planNow)); got != want {
				t.Errorf("%s after the rollback: %q, want %q", q, got, want)
			}
		}(q.sql, q.after)
	}
	tx.Rollback()
	// A transaction that has ended reads nothing, and says so.
	if ans := planQuery(tx, crux, nil, planNow); ans.Error == "" {
		t.Errorf("a plan through an ended transaction gave %+v", ans)
	}
}

func TestRunTakesOneArgumentForEachMark(t *testing.T) {
	s := planStore(t)
	st, err := Parse("SELECT key FROM docs WHERE n > ? AND title = ?")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]value.Value{nil, {value.Int(1)}, {value.Int(1), value.Text("a"), value.Int(3)}} {
		if _, _, err := Query(s, st.(*Select), &Frame{Args: args}); err == nil {
			t.Errorf("%d arguments for 2 marks gave no error", len(args))
		}
	}
	if _, _, err := Query(s, st.(*Select), &Frame{Args: []value.Value{value.Int(1), value.Text("Retro")}}); err != nil {
		t.Errorf("2 arguments for 2 marks: %v", err)
	}
}

// TestDatesTakeTheStatementsMoment runs dates through the planner, on the
// moment in the Frame, in WHERE and in the result columns alike.
func TestDatesTakeTheStatementsMoment(t *testing.T) {
	s := planStore(t)
	q := "SELECT key, date('now', '-1 year') FROM people WHERE joined >= date('now', '-1 year') ORDER BY key"
	if got := answerText(planQuery(s, q, nil, planNow)); got != lines(`text "people:1", text "2025-10-09"`, `text "people:2", text "2025-10-09"`, `text "people:4", text "2025-10-09"`) {
		t.Errorf("%s: %q", q, got)
	}
	if ans := planQuery(s, "SELECT key FROM people WHERE joined < date('now')", nil, time.Time{}); ans.Error == "" {
		t.Errorf("a date without a moment gave %+v", ans)
	}
	if ans := planQuery(s, "SELECT key FROM people WHERE joined < date('2026-01-01')", nil, planNow); ans.Error == "" {
		t.Errorf("a date on a stored date gave %+v", ans)
	}
}

// TestColumnNames checks the names of the result columns: *, fields as the
// table spells them, the walk's and json_each's columns, aliases and text.
func TestColumnNames(t *testing.T) {
	s := planStore(t)
	for _, cs := range []struct{ sql, names string }{
		{"SELECT * FROM docs", "key n score status title vec data tags"},
		{"SELECT * FROM walk('people:1', 1) w", "key depth"},
		{"SELECT KEY, TITLE, d.N FROM docs d", "key title n"},
		{"SELECT w.value, d.key AS k, d.n + 1 FROM json_each(walk('people:1', 1)) w JOIN docs d ON d.key = w.value", "value k d.n + 1"},
		{"SELECT (key), w.depth FROM walk('people:1', 1) w", "key depth"},
	} {
		st, err := Parse(cs.sql)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Prepare(s, st.(*Select))
		if err != nil {
			t.Fatalf("%s: %v", cs.sql, err)
		}
		if got := strings.Join(p.Columns(), " "); got != cs.names {
			t.Errorf("%s: %q, want %q", cs.sql, got, cs.names)
		}
	}
	if ans := planQuery(s, "SELECT *", nil, planNow); ans.Error == "" {
		t.Errorf("SELECT * without a FROM gave %+v", ans)
	}
}

// TestTheSearchFilterIsSafeOnGoroutines calls a nearest search's filter
// from several goroutines at once, as the store may (P3.md), and each
// record gets the answer it gets alone.
func TestTheSearchFilterIsSafeOnGoroutines(t *testing.T) {
	s := planStore(t)
	st, err := Parse("SELECT key FROM docs WHERE vec IS NOT NULL AND n > (SELECT n FROM docs WHERE key = 'docs:1') AND title LIKE '%a%' ORDER BY distance(vec, ?) LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(s, st.(*Select))
	if err != nil || p.Shape() != ShapeNearest {
		t.Fatalf("%v, %v", p, err)
	}
	keep := p.filter(&Frame{Args: []value.Value{value.Text("[1, 0, 0]")}})
	var recs []store.Record
	c, _ := s.Scan("docs:", "")
	for rec, ok := c.Next(); ok; rec, ok = c.Next() {
		recs = append(recs, rec)
	}
	want := make([]bool, len(recs))
	for i, rec := range recs {
		ok, err := keep(rec)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = ok
	}
	done := make(chan string)
	for g := 0; g < 8; g++ {
		go func() {
			for round := 0; round < 200; round++ {
				for i, rec := range recs {
					if ok, err := keep(rec); err != nil || ok != want[i] {
						done <- rec.Key
						return
					}
				}
			}
			done <- ""
		}()
	}
	for g := 0; g < 8; g++ {
		if k := <-done; k != "" {
			t.Errorf("%s got another answer on a goroutine of its own", k)
		}
	}
}

// TestTheSortForASearchWorksOutItsQueryOnce checks that a nearest search
// that sorts instead, since its query vector is NULL, works out the
// subquery that gives it once, for the check and the sort alike.
func TestTheSortForASearchWorksOutItsQueryOnce(t *testing.T) {
	s := planStore(t)
	r := &countingReader{Reader: s, gets: map[string]int{}}
	q := "SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM docs WHERE key = 'docs:4')) LIMIT 2"
	ans := planQuery(r, q, nil, planNow)
	if answerText(ans) != lines(d1, d2) || r.gets["docs:4"] != 1 {
		t.Errorf("%s: %+v, with Get of docs:4 %d times; want docs:1 and docs:2, and 1", q, ans, r.gets["docs:4"])
	}
}

// plannedNearest is Nearest(table, q, k, where, args...) as the public
// package can make it with NearestFilter: 0.x's checks in 0.x's order, as
// the store makes them, then, for a table with a vector size, the filter,
// then the search.
func plannedNearest(r store.Reader, table string, q []float32, k int, where string, args []value.Value, now time.Time) ([]store.Hit, error) {
	if err := rules.Table(table); err != nil {
		return nil, err
	}
	if err := rules.Vector(q); err != nil {
		return nil, err
	}
	if k < 1 || k > store.MaxK {
		return nil, fmt.Errorf("%w: k is from 1 to %d, not %d", errs.ErrInvalid, store.MaxK, k)
	}
	t, ok := r.Table(table)
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: no record table %s", errs.ErrNotFound, table)
	case t.Size == 0:
		return nil, nil
	case t.Size != len(q):
		return nil, fmt.Errorf("%w: table %s holds vectors of %d values, and the query has %d", errs.ErrInvalid, table, t.Size, len(q))
	}
	cond, params, err := ParseCondition(where)
	if err != nil {
		return nil, err
	}
	var keep store.Filter
	if cond != nil {
		var none bool
		if keep, none, err = NearestFilter(r, table, cond, params, &Frame{Args: args, Now: now}); err != nil {
			return nil, err
		}
		if none {
			return []store.Hit{}, nil
		}
	}
	return r.Nearest(table, q, k, keep)
}

// betaKind is difftest.Kind for the Beta's errors.
func betaKind(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errs.ErrNotFound):
		return "not found"
	case errors.Is(err, errs.ErrInvalid):
		return "invalid"
	}
	return "error"
}

// TestNearestFilters runs Nearest's filter, as the public package can make
// it with NearestFilter, on the fixture: WHERE's rules, a filter that rules
// every record out at once, and errors of kind "error", after Nearest's own
// checks, which a table without a vector size never gets past.
func TestNearestFilters(t *testing.T) {
	s := planStore(t)
	q := []float32{1, 0, 0}
	cases := []struct {
		table, where string
		args         []value.Value
		want         string // the hits' keys, or an error's kind
	}{
		{"docs", "", nil, "docs:1 docs:7 docs:3 docs:5 docs:2 docs:6"},
		{"docs", "  ", nil, "docs:1 docs:7 docs:3 docs:5 docs:2 docs:6"},
		{"docs", "status = ?", []value.Value{value.Text("open")}, "docs:1 docs:3 docs:5"},
		{"docs", `"status" = 'open' AND n > 3`, nil, "docs:3 docs:5"},
		{"docs", "n > (SELECT n FROM docs WHERE key = 'docs:3')", nil, "docs:7 docs:5"},
		{"docs", "key IN (SELECT key FROM walk('people:1', 2))", nil, "docs:1 docs:3 docs:2"},
		{"docs", "key = 'docs:2' OR key = 'docs:6'", nil, "docs:2 docs:6"},
		{"docs", "key = 'docs:2'", nil, "docs:2"},
		{"docs", "0", nil, ""},
		{"docs", "1 = 1 AND title LIKE 'q3%'", nil, "docs:1 docs:3"},
		{"docs", "abs(-9223372036854775808)", nil, "error"},
		{"docs", "abs(-9223372036854775807 - (key = 'docs:4'))", nil, "docs:1 docs:7 docs:3 docs:5 docs:2 docs:6"},
		{"docs", "abs(-9223372036854775807 - (key = 'docs:6'))", nil, "error"},
		{"docs", "nosuch = 1", nil, "error"},
		{"docs", "status = ?", nil, "error"},
		{"docs", "status = 'open' AND", nil, "error"},
		{"docs", "count(*) > 1", nil, "error"},
		{"docs", "a\x00b", nil, "error"},
		{"empty", "nosuch = 1", nil, "invalid"},
		{"people", "nosuch = 1", nil, ""},
		{"nosuch", "nosuch = 1", nil, "not found"},
	}
	for _, cs := range cases {
		hits, err := plannedNearest(s, cs.table, q, 10, cs.where, cs.args, planNow)
		got := betaKind(err)
		if err == nil {
			got = strings.Join(keysOfHits(hits), " ")
		}
		if got != cs.want {
			t.Errorf("Nearest(%q, %q, %v): %q, %v; want %q", cs.table, cs.where, cs.args, got, err, cs.want)
		}
	}
}

func keysOfHits(hits []store.Hit) []string {
	keys := make([]string, len(hits))
	for i, h := range hits {
		keys[i] = h.Key
	}
	return keys
}
