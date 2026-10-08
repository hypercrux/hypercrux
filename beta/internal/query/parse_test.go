// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"strings"
	"testing"
)

// mustParse parses q, which must parse and print back.
func mustParse(t *testing.T, q string) Statement {
	t.Helper()
	s, err := Parse(q)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	checkPrintsBack(t, q, s)
	return s
}

// exprOf parses SELECT e and returns the expression.
func exprOf(t *testing.T, e string) Expr {
	t.Helper()
	return mustParse(t, "SELECT "+e).(*Select).Results[0].Expr
}

// TestPrecedence holds the trees to SQL.md's "Operators and how they group"
// and "A sign before a literal", SQL.md's own examples first.
func TestPrecedence(t *testing.T) {
	for _, c := range []struct{ sql, tree string }{
		{"1 = 2 < 3", "(= 1 (< 2 3))"},
		{"1 = 1 = 1", "(= (= 1 1) 1)"},
		{"1 = NOT 2 = 3", "(= 1 (NOT (= 2 3)))"},
		{"- NOT 1 + 2", "(- (NOT (+ 1 2)))"},
		{"-2 || 3", "(|| (- 2) 3)"},
		{"x BETWEEN y BETWEEN 0 AND 1 AND 2", "(BETWEEN x (BETWEEN y 0 1) 2)"},
		{"-+x", "(- x)"},
		{"+(+x)", "(+ x)"},
		{"-(+x)", "(- x)"},
		{"+-x", "(+ (- x))"},
		{"- -1", "(- (- 1))"},
		{"-(1)", "(- 1)"},
		{"-(-(1))", "(- (- 1))"},

		// Levels 1 to 9.
		{"a OR b AND c", "(OR a (AND b c))"},
		{"a AND b OR c", "(OR (AND a b) c)"},
		{"a OR b OR c", "(OR (OR a b) c)"},
		{"NOT a AND b", "(AND (NOT a) b)"},
		{"NOT a = b", "(NOT (= a b))"},
		{"NOT NOT a", "(NOT (NOT a))"},
		{"NOT a OR b", "(OR (NOT a) b)"},
		{"a = b <> c != d == e", "(= (!= (!= (= a b) c) d) e)"},
		{"a < b = c < d", "(= (< a b) (< c d))"},
		{"a <= b > c >= d", "(>= (> (<= a b) c) d)"},
		{"a + b * c", "(+ a (* b c))"},
		{"a * b + c", "(+ (* a b) c)"},
		{"a - b - c", "(- (- a b) c)"},
		{"a / b % c * d", "(* (% (/ a b) c) d)"},
		{"a || b || c", "(|| (|| a b) c)"},
		{"a * b || c", "(* a (|| b c))"},
		{"a || b * c", "(* (|| a b) c)"},
		{"-a * b", "(* (- a) b)"},
		{"-a || b", "(|| (- a) b)"},
		{"1 + -2", "(+ 1 (- 2))"},
		{"1 - - 2", "(- 1 (- 2))"},
		{"1 + 2 = 3 AND 4 < 5 OR NOT 6", "(OR (AND (= (+ 1 2) 3) (< 4 5)) (NOT 6))"},
		{"(a OR b) AND c", "(AND (OR a b) c)"},
		{"((a))", "a"},

		// IS, IN, LIKE and BETWEEN, all at level 4.
		{"a = b IS c", "(IS (= a b) c)"},
		{"a IS b = c", "(= (IS a b) c)"},
		{"a IS NOT b = c", "(= (IS NOT a b) c)"},
		{"a IS NOT NOT b", "(IS NOT a (NOT b))"},
		{"a IS NULL", "(IS a NULL)"},
		{"a IS NOT NULL", "(IS NOT a NULL)"},
		{"a < b IS NULL", "(IS (< a b) NULL)"},
		{"NOT a IS NULL", "(NOT (IS a NULL))"},
		{"a IN (1, 2) = 1", "(= (IN a [1 2]) 1)"},
		{"a = b IN (1)", "(IN (= a b) [1])"},
		{"a IN ()", "(IN a [])"},
		{"a NOT IN ()", "(NOT IN a [])"},
		{"a NOT IN (b OR c, d)", "(NOT IN a [(OR b c) d])"},
		{"a IN (b) IN (c)", "(IN (IN a [b]) [c])"},
		{"a LIKE b", "(LIKE a b)"},
		{"a NOT LIKE b", "(NOT LIKE a b)"},
		{"a LIKE b ESCAPE c", "(LIKE a b c)"},
		{"a LIKE b < c ESCAPE d < e", "(LIKE a (< b c) (< d e))"},
		{"a LIKE b = c", "(= (LIKE a b) c)"},
		{"a LIKE b ESCAPE c = d", "(= (LIKE a b c) d)"},
		{"a LIKE b || c", "(LIKE a (|| b c))"},
		{"a = b LIKE c", "(LIKE (= a b) c)"},
		{"a LIKE b LIKE c", "(LIKE (LIKE a b) c)"},
		{"a BETWEEN 1 AND 2", "(BETWEEN a 1 2)"},
		{"a NOT BETWEEN 1 AND 2", "(NOT BETWEEN a 1 2)"},
		{"NOT a BETWEEN 1 AND 2", "(NOT (BETWEEN a 1 2))"},
		{"1 BETWEEN 0 AND 5 AND 2", "(AND (BETWEEN 1 0 5) 2)"},
		{"a BETWEEN 1 AND 2 = 3", "(= (BETWEEN a 1 2) 3)"},
		{"a BETWEEN 1 = 1 AND 2", "(BETWEEN a (= 1 1) 2)"},
		{"a BETWEEN 1 + 1 AND 2 * 3", "(BETWEEN a (+ 1 1) (* 2 3))"},
		{"a BETWEEN 1 AND 2 < 3", "(BETWEEN a 1 (< 2 3))"},
		{"a BETWEEN NOT 1 AND 2", "(BETWEEN a (NOT 1) 2)"},
		{"a BETWEEN 1 AND NOT 2 = 3", "(BETWEEN a 1 (NOT (= 2 3)))"},
		{"a BETWEEN b IN (1) AND c", "(BETWEEN a (IN b [1]) c)"},
		{"a BETWEEN (b OR c) AND d", "(BETWEEN a (OR b c) d)"},
		{"a = b BETWEEN c AND d", "(BETWEEN (= a b) c d)"},
		{"a BETWEEN b AND c BETWEEN d AND e", "(BETWEEN (BETWEEN a b c) d e)"},

		// Primaries.
		{"CAST(1 + 2 AS TEXT) || 'a'", "(|| (CAST (+ 1 2) TEXT) 'a')"},
		{"cast(x as integer)", "(CAST x INTEGER)"},
		{"abs(-1) * 2", "(* (abs (- 1)) 2)"},
		{"count(*)", "(count *)"},
		{"coalesce(a, b, c)", "(coalesce a b c)"},
		{"(SELECT n FROM docs WHERE key = 'a') + 1", "(+ (record n docs 'a') 1)"},
		{"(SELECT key FROM docs WHERE KEY = ?)", "(record key docs ?0)"},
		{"(SELECT n FROM docs WHERE \"key\" == 1 + 2)", "(record n docs (+ 1 2))"},
		{"x IN (SELECT key FROM walk('a', 1))", "(IN x (walk 'a' 1))"},
		{"x NOT IN (SELECT value FROM json_each(walk('a', 1, 'owns', 'in')))", "(NOT IN x (each (walk 'a' 1 'owns' 'in')))"},
		{"walk('a', 2)", "(walk 'a' 2)"},
		{"d.title", "d.title"},
		{`"d"."ti""tle"`, `"d"."ti\"tle"`},
		{"x'00ff'", "x'00ff'"},
		{"null", "null"},
		{"?", "?0"},
	} {
		if got := dumpExpr(exprOf(t, c.sql)); got != c.tree {
			t.Errorf("%s gives %s, want %s", c.sql, got, c.tree)
		}
	}
}

// TestStatements holds each statement form of SQL.md's grammar to its tree.
func TestStatements(t *testing.T) {
	for _, c := range []struct{ sql, tree string }{
		{"SELECT 1", "(select 1) params=0"},
		{"select 1;", "(select 1) params=0"},
		{"SeLeCt 1 ; -- done", "(select 1) params=0"},
		{"/* first */ SELECT 1 /* last */", "(select 1) params=0"},
		{"SELECT *", "(select *) params=0"},
		{"SELECT * FROM docs", "(select * (from docs)) params=0"},
		{"SELECT a, b AS c, d e, f \"g\", h AS \"i j\" FROM t", `(select a b as c d as e f as "g" h as "i j" (from t)) params=0`},
		{"SELECT x FROM docs d", "(select x (from docs as d)) params=0"},
		{"SELECT x FROM docs AS d", "(select x (from docs as d)) params=0"},
		{`SELECT x FROM "Docs" "d"`, `(select x (from "Docs" as "d")) params=0`},
		{"SELECT key FROM walk('people:1', 2)", "(select key (from (walk 'people:1' 2))) params=0"},
		{"SELECT key, depth FROM WALK('people:1', 2, NULL, 'both') w", "(select key depth (from (walk 'people:1' 2 NULL 'both') as w)) params=0"},
		{"SELECT value FROM json_each(walk(?, ?)) AS w", "(select value (from (each (walk ?0 ?1)) as w)) params=2"},
		{"SELECT d.key FROM walk('a', 1) w JOIN docs d ON d.key = w.key",
			"(select d.key (from (walk 'a' 1) as w docs as d (on d.key w.key))) params=0"},
		{"SELECT d.key FROM docs d INNER JOIN walk('a', 1) w ON (w.key == d.key)",
			"(select d.key (from docs as d (walk 'a' 1) as w (on w.key d.key))) params=0"},
		{"SELECT d.key FROM json_each(walk('a', 1)) w inner join docs d ON ((d.key = w.value))",
			"(select d.key (from (each (walk 'a' 1)) as w docs as d (on d.key w.value))) params=0"},
		{"SELECT key FROM docs WHERE n > 2 ORDER BY n DESC, key ASC, 2 LIMIT 3 OFFSET 1",
			"(select key (from docs) (where (> n 2)) (order n desc key 2) (limit 3 1)) params=0"},
		{"SELECT key FROM docs LIMIT -1", "(select key (from docs) (limit (- 1))) params=0"},
		{"SELECT count(*), sum(n) FROM docs", "(select (count *) (sum n) (from docs) aggregate) params=0"},
		{"SELECT max(n) + 1, count(*) * 2 AS c FROM docs ORDER BY count(*)",
			"(select (+ (max n) 1) (* (count *) 2) as c (from docs) (order (count *)) aggregate) params=0"},
		{"SELECT count(*), walk('people:1', 2) FROM docs", "(select (count *) (walk 'people:1' 2) (from docs) aggregate) params=0"},
		{"SELECT max(n, 1) FROM docs", "(select (max n 1) (from docs)) params=0"},
		{"SELECT min(n) FROM docs", "(select (min n) (from docs) aggregate) params=0"},
		{"INSERT INTO docs (key, title) VALUES ('docs:1', 'a')", "(insert docs (key title) ('docs:1' 'a')) params=0"},
		{"INSERT INTO docs (KEY, n) VALUES (?, 1), (?, -2)", "(insert docs (KEY n) (?0 1) (?1 (- 2))) params=2"},
		{`INSERT INTO "docs" ("key") VALUES ('docs:1')`, `(insert "docs" ("key") ('docs:1')) params=0`},
		{"UPDATE docs SET n = n + 1", "(update docs (set n (+ n 1))) params=0"},
		{"UPDATE docs SET a = b, b == a WHERE key = ?", "(update docs (set a b) (set b a) (where (= key ?0))) params=1"},
		{"UPDATE docs SET a = b = c", "(update docs (set a (= b c))) params=0"},
		{"DELETE FROM docs", "(delete docs) params=0"},
		{"DELETE FROM docs WHERE key IN (SELECT key FROM walk('a', 1))", "(delete docs (where (IN key (walk 'a' 1)))) params=0"},
		{"SELECT ? FROM docs WHERE n = ? ORDER BY ? LIMIT ? OFFSET ?",
			"(select ?0 (from docs) (where (= n ?1)) (order ?2) (limit ?3 ?4)) params=5"},
		{"SELECT (SELECT vec FROM photo WHERE key = ?), distance(vec, ?) FROM photo",
			"(select (record vec photo ?0) (distance vec ?1) (from photo)) params=2"},
		{"SELECT d.key, d.title FROM json_each(walk('customer:42', 2)) w JOIN docs d ON d.key = w.value WHERE d.status = 'open' AND d.vec IS NOT NULL ORDER BY distance(d.vec, ?) LIMIT 10",
			"(select d.key d.title (from (each (walk 'customer:42' 2)) as w docs as d (on d.key w.value)) (where (AND (= d.status 'open') (IS NOT d.vec NULL))) (order (distance d.vec ?0)) (limit 10)) params=1"},
	} {
		if got := dump(mustParse(t, c.sql)); got != c.tree {
			t.Errorf("%s gives\n  %s, want\n  %s", c.sql, got, c.tree)
		}
	}
}

// TestNamesAndKeywords: reserved words, the six keywords only in place,
// names quoted and bare, and the SQLite keywords that are names in the
// subset.
func TestNamesAndKeywords(t *testing.T) {
	for _, c := range []struct{ sql, tree string }{
		// The six words that are keywords only where the grammar has them.
		{"SELECT asc, desc, by, offset, inner, like FROM t ORDER BY desc DESC, asc",
			"(select asc desc by offset inner like (from t) (order desc desc asc)) params=0"},
		{"SELECT x desc, y asc, z by, w offset FROM t", "(select x as desc y as asc z as by w as offset (from t)) params=0"},
		{"SELECT x FROM t desc", "(select x (from t as desc)) params=0"},
		{"SELECT x FROM t like", "(select x (from t as like)) params=0"},
		{"SELECT like(1) FROM t", ""},
		{"SELECT x FROM desc LIMIT 1 OFFSET 2", "(select x (from desc) (limit 1 2)) params=0"},
		// Function names, walk and json_each are names.
		{"SELECT walk, json_each, abs, count FROM walk", "(select walk json_each abs count (from walk)) params=0"},
		{"SELECT x FROM json_each", "(select x (from json_each)) params=0"},
		// SQLite's other keywords are names in the subset.
		{"SELECT glob, regexp, match, left, natural, indexed, window, over, filter, nulls, first, true.x FROM t",
			"(select glob regexp match left natural indexed window over filter nulls first true.x (from t)) params=0"},
		{"SELECT 1 AS left, 2 AS indexed, 3 AS like, 4 window, 5 over, 6 filter", "(select 1 as left 2 as indexed 3 as like 4 as window 5 as over 6 as filter) params=0"},
		{"SELECT x FROM t AS left", "(select x (from t as left)) params=0"},
		{"SELECT x FROM with", "(select x (from with)) params=0"},
		{"SELECT x AS with FROM t with", "(select x as with (from t as with)) params=0"},
		{"SELECT (SELECT with FROM t WHERE key = 1)", "(select (record with t 1)) params=0"},
		{"INSERT INTO t (key, with, left) VALUES (1, 2, 3)", "(insert t (key with left) (1 2 3)) params=0"},
		// Quoted names may be anything, reserved words included.
		{`SELECT "select", "a""b", "" FROM "from"`, `(select "select" "a\"b" "" (from "from")) params=0`},
		{`SELECT "true", "with" FROM t`, `(select "true" "with" (from t)) params=0`},
		// Bare names hold letters, digits, _, $ and bytes from 0x80 up.
		{"SELECT a$b, _x1, é, ʼAmir, x$ FROM t", "(select a$b _x1 é ʼAmir x$ (from t)) params=0"},
		{"SELECT select1, from_, ordered FROM t", "(select select1 from_ ordered (from t)) params=0"},
	} {
		s, err := Parse(c.sql)
		if c.tree == "" {
			if err == nil {
				t.Errorf("%s parses", c.sql)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		checkPrintsBack(t, c.sql, s)
		if got := dump(s); got != c.tree {
			t.Errorf("%s gives\n  %s, want\n  %s", c.sql, got, c.tree)
		}
	}
	// Keywords match in any case, and every reserved word is refused as a
	// bare name, in an expression and as an alias or a table.
	for w := range reserved {
		for _, q := range []string{"SELECT " + w + " FROM t", "SELECT x AS " + strings.ToLower(w), "SELECT x FROM " + w} {
			if w == "NULL" && strings.HasPrefix(q, "SELECT NULL FROM") {
				continue
			}
			if _, err := Parse(q); err == nil {
				t.Errorf("%s parses, and %s is reserved", q, w)
			}
		}
		if _, err := Parse(`SELECT "` + w + `" FROM "` + w + `"`); err != nil {
			t.Errorf("%s in double quotes: %v", w, err)
		}
	}
}

// TestParams counts the ? marks, numbered in the order they're written.
func TestParams(t *testing.T) {
	s := mustParse(t, "SELECT ?, (SELECT n FROM docs WHERE key = ?) FROM walk(?, ?) WHERE x IN (?, ?) AND y LIKE ? ESCAPE ? ORDER BY ? LIMIT ? OFFSET ?")
	if s.Params() != 11 {
		t.Fatalf("%d marks", s.Params())
	}
	var at []int
	var idx []int
	sql := s.String()
	_ = sql
	var visit func(e Expr)
	visit = func(e Expr) {
		walkExpr(e, func(e Expr) bool {
			if p, ok := e.(*Param); ok {
				at, idx = append(at, p.At), append(idx, p.Index)
			}
			if r, ok := e.(*Record); ok {
				visit(r.Key)
			}
			return true
		})
	}
	sel := s.(*Select)
	visit(sel.Results[0].Expr)
	visit(sel.Results[1].Expr)
	for _, a := range sel.From.Sources[0].Walk.Args {
		visit(a)
	}
	visit(sel.Where)
	visit(sel.OrderBy[0].Expr)
	visit(sel.Limit)
	visit(sel.Offset)
	for i := range idx {
		if idx[i] != i || i > 0 && at[i] <= at[i-1] {
			t.Fatalf("marks numbered %v at %v", idx, at)
		}
	}
	if len(idx) != 11 {
		t.Fatalf("found %d marks", len(idx))
	}
	for _, q := range []string{"INSERT INTO t (key, a) VALUES (?, ?), (?, 1)", "UPDATE t SET a = ?, b = ? WHERE c = ?", "DELETE FROM t WHERE a = ? OR b IN (?, ?)"} {
		if n := mustParse(t, q).Params(); n != 3 {
			t.Errorf("%s has %d marks", q, n)
		}
	}
	// SQLite's limit on marks.
	many := "SELECT 1 IN (" + strings.TrimSuffix(strings.Repeat("?, ", maxParams), ", ") + ")"
	if s := mustParse(t, many); s.Params() != maxParams {
		t.Errorf("%d marks", s.Params())
	}
	if _, err := Parse(many[:len(many)-1] + ", ?)"); err == nil || err.Error() != "too many SQL variables" {
		t.Errorf("%d marks: %v", maxParams+1, err)
	}
}

// TestLiterals keeps each literal as written, with what text and bytes
// stand for.
func TestLiterals(t *testing.T) {
	for _, c := range []struct {
		sql  string
		kind LitKind
		data string
	}{
		{"1", LitInt, ""}, {"9223372036854775808", LitInt, ""}, {"007", LitInt, ""},
		{"1.5", LitReal, ""}, {".5", LitReal, ""}, {"5.", LitReal, ""}, {"1e10", LitReal, ""},
		{"2.5E-3", LitReal, ""}, {"1.e5", LitReal, ""}, {"1E+5", LitReal, ""}, {"1e999", LitReal, ""},
		{"''", LitText, ""}, {"'it''s'", LitText, "it's"}, {"'a\nb'", LitText, "a\nb"}, {"''''", LitText, "'"},
		{"'\xff\x01'", LitText, "\xff\x01"}, {"'é'", LitText, "é"},
		{"x''", LitBytes, ""}, {"x'00ff'", LitBytes, "\x00\xff"}, {"X'0A'", LitBytes, "\n"}, {"x'aBcD'", LitBytes, "\xab\xcd"},
		{"NULL", LitNull, ""}, {"null", LitNull, ""},
	} {
		l, ok := exprOf(t, c.sql).(*Literal)
		if !ok || l.Kind != c.kind || l.Text != c.sql || l.Data != c.data || l.At != len("SELECT ") {
			t.Errorf("%s gives %#v", c.sql, l)
		}
	}
	// A minus sign is never part of a number.
	if u, ok := exprOf(t, "-1").(*Unary); !ok || u.Op != OpNeg || u.X.(*Literal).Text != "1" {
		t.Errorf("-1 gives %s", dumpExpr(u))
	}
}

// TestResultText holds result columns' text to SQL.md's "What a result
// column is called", with SQLite's own examples, which 0.x gives.
func TestResultText(t *testing.T) {
	for _, c := range []struct {
		sql   string
		texts []string
	}{
		{"SELECT 1 + 2 /* x */, n FROM docs", []string{"1 + 2 /* x */", "n"}},
		{"SELECT 1 --x", []string{"1 --x"}},
		{"SELECT 1 --x\n;", []string{"1 --x"}},
		{"SELECT /* c */ 1", []string{"1"}},
		{"SELECT  ( 1 )  ", []string{"( 1 )"}},
		{"SELECT 1 /* x */", []string{"1 /* x */"}},
		{"SELECT 1 /* x", []string{"1 /* x"}},
		{"SELECT 1 /**/", []string{"1 /**/"}},
		{"SELECT 1;  -- c", []string{"1"}},
		{"SELECT 1 \v", []string{"1"}},
		{"SELECT 1 \v+ 2", []string{"1 \v+ 2"}},
		{"SELECT -1, - 1, -(1), +1, + 1", []string{"-1", "- 1", "-(1)", "+1", "+ 1"}},
		{"SELECT count( * ), COUNT(*) FROM docs", []string{"count( * )", "COUNT(*)"}},
		{"SELECT a\n+ b FROM t", []string{"a\n+ b"}},
		{"SELECT 1 x /* c */, 2", []string{"1", "2"}},
		{"SELECT x'01', 'a', NULL, 2.5", []string{"x'01'", "'a'", "NULL", "2.5"}},
		{"SELECT 1\x00garbage", []string{"1"}},
	} {
		sel := mustParse(t, c.sql).(*Select)
		var got []string
		for _, r := range sel.Results {
			got = append(got, r.Text)
		}
		if strings.Join(got, "|") != strings.Join(c.texts, "|") {
			t.Errorf("%q gives %q, want %q", c.sql, got, c.texts)
		}
	}
}

// TestPlaces checks that nodes and errors know where they are in the text.
func TestPlaces(t *testing.T) {
	q := "SELECT a + b, abs(c) FROM docs d WHERE d.x IN (1) AND y BETWEEN 1 AND 2"
	sel := mustParse(t, q).(*Select)
	at := func(e Expr, want string) {
		t.Helper()
		if !strings.HasPrefix(q[e.Pos():], want) {
			t.Errorf("%s is at %d, %q", dumpExpr(e), e.Pos(), q[e.Pos():])
		}
	}
	add := sel.Results[0].Expr.(*Binary)
	at(add, "a + b")
	at(add.R, "b,")
	if !strings.HasPrefix(q[add.OpAt:], "+ b") {
		t.Errorf("the operator is at %d", add.OpAt)
	}
	at(sel.Results[1].Expr, "abs(c)")
	at(sel.Results[1].Expr.(*Call).Args[0], "c)")
	and := sel.Where.(*Binary)
	at(and.L, "d.x IN")
	at(and.R, "y BETWEEN")
	if s := sel.From.Sources[0]; !strings.HasPrefix(q[s.At:], "docs d") || !strings.HasPrefix(q[s.Alias.At:], "d WHERE") {
		t.Errorf("the source is at %d and its alias at %d", s.At, s.Alias.At)
	}
	for _, c := range []struct{ sql, at string }{
		{"SELECT 1 +", ""},
		{"SELEC 1", "SELEC"},
		{"SELECT 1 FROM docs GROUP BY n", "GROUP"},
		{"SELECT nosuchfunc(1)", "nosuchfunc"},
		{"SELECT count(count(*)) FROM docs", "count(*))"},
		{"SELECT key FROM docs LIMIT n", "n"},
		{"SELECT 12abc", "12abc"},
	} {
		_, err := Parse(c.sql)
		var pe *Error
		if !errors.As(err, &pe) || c.sql[pe.Pos:] != c.sql[len(c.sql)-len(c.at):] && !strings.HasPrefix(c.sql[pe.Pos:], c.at) {
			t.Errorf("%s: %v at %d", c.sql, err, pe.Pos)
		}
	}
}

// TestRefusals: each statement outside the subset, and each rule beside
// the grammar, is an error that says what's wrong.
func TestRefusals(t *testing.T) {
	deep := func(open, x, close string, n int) string {
		return strings.Repeat(open, n) + x + strings.Repeat(close, n)
	}
	for _, c := range []struct{ sql, msg string }{
		// No statement, or two.
		{"", "no statement"},
		{"  -- nothing", "no statement"},
		{";", "no statement"},
		{"SELECT 1;;", "second statement"},
		{"SELECT 1; SELECT 2", "second statement"},
		{"SELECT 'a;b'; SELECT 2", "second statement"},
		// Syntax errors, with SQLite's messages.
		{"SELECT", "incomplete input"},
		{"SELECT 1 +", "incomplete input"},
		{"SELECT (1", "incomplete input"},
		{"SELECT 1 FROM", "incomplete input"},
		{"SELECT --1", "incomplete input"},
		{"SELEC 1", `near "SELEC": syntax error`},
		{"SELECT 1 2", `near "2": syntax error`},
		{"SELECT 1 /*", `near "*": syntax error`},
		{"SELECT (1))", `near ")": syntax error`},
		{"SELECT 1 FROM docs ORDER BY x y", `near "y": syntax error`},
		{"SELECT x BETWEEN 1 OR 2 AND 3", `near "OR": syntax error`},
		{"SELECT x LIKE y ESCAPE z ESCAPE w", `near "ESCAPE": syntax error`},
		{"SELECT 1 IS NOT IN (1)", `near "IN": syntax error`},
		{"SELECT x NOT y", `near "NOT": syntax error`},
		{"SELECT 1 left", `near "left": syntax error`},
		{"SELECT 1 indexed", `near "indexed": syntax error`},
		{"SELECT 1 natural, 2", `near "natural": syntax error`},
		{"SELECT 'a'.b", `near ".": syntax error`},
		{"SELECT d.* + 1 FROM docs d", "d.* is outside"},
		{"SELECT 1 + d.* FROM docs d", `near "*": syntax error`},
		{"SELECT 12abc", `unrecognized token: "12abc"`},
		{"SELECT 1e", `unrecognized token: "1e"`},
		{"SELECT 1e+", `unrecognized token: "1e"`},
		{"SELECT 0x", `unrecognized token: "0x"`},
		{"SELECT 0x1g", `unrecognized token: "0x1g"`},
		{"SELECT 1.5.3", `near ".3": syntax error`},
		{"SELECT x'0'", `unrecognized token: "x'0'"`},
		{"SELECT x'0g'", `unrecognized token: "x'0g'"`},
		{"SELECT 'abc", `unrecognized token: "'abc"`},
		{`SELECT "abc`, `unrecognized token: "\"abc"`},
		{"SELECT [abc", `unrecognized token: "[abc"`},
		{"SELECT !1", `unrecognized token: "!"`},
		{"SELECT \\", `unrecognized token: "\\"`},
		{"SELECT 1\v+ 2", `unrecognized token: "\v"`},
		{"SELECT :", `unrecognized token: ":"`},
		{"SELECT 'a\x00b'", `unrecognized token: "'a"`},
		{"SELECT\xef\xbb\xbf1", "syntax error"},
		// Statements outside the subset.
		{"WITH x AS (SELECT 1) SELECT * FROM x", "WITH is outside"},
		{"VALUES (1)", "VALUES on its own is outside"},
		{"EXPLAIN SELECT 1", "EXPLAIN is outside"},
		{"PRAGMA x", "PRAGMA is outside"},
		{"CREATE TABLE t (x)", "CREATE is outside the SQL subset: tables and their fields come from Put"},
		{"drop table t", "DROP is outside"},
		{"ALTER TABLE t ADD x", "ALTER is outside"},
		{"REPLACE INTO t (key) VALUES (1)", "REPLACE is outside"},
		{"BEGIN", "BEGIN is outside the SQL subset: transactions go through Update"},
		{"COMMIT", "COMMIT is outside"},
		{"END", "END is outside"},
		{"ROLLBACK", "ROLLBACK is outside"},
		{"SAVEPOINT a", "SAVEPOINT is outside"},
		{"RELEASE a", "RELEASE is outside"},
		{"ATTACH 'x' AS y", "ATTACH is outside"},
		{"DETACH y", "DETACH is outside"},
		{"VACUUM", "VACUUM is outside the SQL subset: Compact does its job"},
		{"ANALYZE", "ANALYZE is outside"},
		{"REINDEX", "REINDEX is outside"},
		// Words and symbols outside the subset.
		{"SELECT ?1", "?1 is outside the SQL subset: parameters are ?"},
		{"SELECT :a", ":a is outside"},
		{"SELECT @a", "@a is outside"},
		{"SELECT $a", "$a is outside"},
		{"SELECT #a", "#a is outside"},
		{"SELECT $a::b(c)", "$a::b(c) is outside"},
		{"SELECT `a` FROM t", "`a` is outside the SQL subset: names go in double quotes"},
		{"SELECT [a] FROM t", "[a] is outside"},
		{"SELECT 0x10", "0x10 is outside the SQL subset: it has no hex numbers"},
		{"SELECT 1_000", "1_000 is outside the SQL subset: it has no digit separators"},
		{"SELECT 1.5_0", "1.5_0 is outside"},
		{"SELECT true", "TRUE is outside the SQL subset: write 1 or 0"},
		{"SELECT x FROM t WHERE FALSE", "FALSE is outside"},
		{"SELECT CURRENT_DATE", "CURRENT_DATE is outside the SQL subset: write date('now')"},
		{"SELECT CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP is outside"},
		{"SELECT 1 & 2", "the operator & is outside"},
		{"SELECT 1 | 2", "the operator | is outside"},
		{"SELECT ~1", "the operator ~ is outside"},
		{"SELECT 1 << 2", "the operator << is outside"},
		{"SELECT 1 >> 2", "the operator >> is outside"},
		{"SELECT x -> 'a'", "the operator -> is outside"},
		{"SELECT x ->> 'a'", "the operator ->> is outside"},
		{"SELECT x GLOB 'a'", "GLOB is outside"},
		{"SELECT x NOT REGEXP 'a'", "NOT REGEXP is outside"},
		{"SELECT x match 'a'", "MATCH is outside"},
		{"SELECT x ISNULL", "ISNULL is outside the SQL subset: write IS NULL"},
		{"SELECT x NOTNULL", "NOTNULL is outside"},
		{"SELECT x NOT NULL", "NOT NULL after an operand is outside"},
		{"SELECT x IS DISTINCT FROM y", "IS DISTINCT FROM is outside"},
		{"SELECT x IS NOT DISTINCT FROM y", "IS DISTINCT FROM is outside"},
		{"SELECT x COLLATE NOCASE", "COLLATE is outside"},
		{"SELECT CASE WHEN 1 THEN 2 END", "CASE is outside"},
		{"SELECT EXISTS (SELECT 1)", "EXISTS is outside"},
		{"SELECT NOT EXISTS (SELECT 1)", "EXISTS is outside"},
		{"SELECT RAISE(IGNORE)", "RAISE is outside"},
		{"SELECT (1, 2)", "a row value"},
		{"SELECT x IN docs", "IN over a table"},
		{"SELECT with FROM t", `with as a bare name in an expression is outside the SQL subset: write "with"`},
		{"SELECT (with)", "WITH is outside"},
		{"SELECT 1 + (with)", "WITH is outside"},
		{"SELECT x IN (with)", "WITH is outside"},
		{"SELECT NOT with = 1", "with as a bare name"},
		{"SELECT (VALUES (1))", "VALUES is outside"},
		{"SELECT 1 'a'", "text in single quotes as an alias is outside"},
		{"SELECT 1 AS 'a'", "text in single quotes as an alias is outside the SQL subset: names go in double quotes"},
		{"SELECT x FROM 'docs'", "text in single quotes as a table's name is outside"},
		{"SELECT x FROM docs 'd'", "text in single quotes as an alias is outside"},
		{"SELECT d.'x' FROM docs d", "text in single quotes as a field's name is outside"},
		{"INSERT INTO t ('key') VALUES (1)", "text in single quotes as a field's name is outside"},
		{"SELECT a.b.c FROM t", "a name with a schema's name before it is outside"},
		// The SELECT's clauses.
		{"SELECT DISTINCT x FROM t", "DISTINCT is outside"},
		{"SELECT ALL x FROM t", "SELECT ALL is outside"},
		{"SELECT *, key FROM t", "* beside other result columns is outside"},
		{"SELECT key, * FROM t", `near "*": syntax error`},
		{"SELECT d.* FROM docs d", "d.* is outside"},
		{"SELECT x FROM t GROUP BY x", "GROUP BY is outside"},
		{"SELECT x FROM t HAVING x", "HAVING is outside"},
		{"SELECT x FROM t WINDOW w AS (ORDER BY x)", "WINDOW is outside"},
		{"SELECT 1 UNION SELECT 2", "UNION is outside"},
		{"SELECT 1 EXCEPT SELECT 2", "EXCEPT is outside"},
		{"SELECT 1 INTERSECT SELECT 2", "INTERSECT is outside"},
		{"SELECT x FROM t LIMIT 2, 3", "LIMIT m, n is outside the SQL subset: write LIMIT n OFFSET m"},
		{"SELECT x FROM t ORDER BY x NULLS FIRST", "NULLS FIRST and NULLS LAST are outside"},
		{"SELECT x FROM t ORDER BY x DESC NULLS LAST", "NULLS FIRST and NULLS LAST are outside"},
		{"SELECT x FROM t OFFSET 1", `near "1": syntax error`},
		{"SELECT 1 WINDOW w AS (ORDER BY 1)", "WINDOW is outside"},
		// FROM.
		{"SELECT x FROM (SELECT 1)", "a subquery in FROM is outside"},
		{"SELECT x FROM main.docs", "a table's name with a schema's name before it is outside"},
		{"SELECT x FROM docs d, people p", "a join written with a comma is outside the SQL subset: join with JOIN ... ON"},
		{"SELECT x FROM docs d LEFT JOIN walk('a', 1) w ON d.key = w.key", "LEFT JOIN is outside"},
		{"SELECT x FROM docs left outer join people", "LEFT JOIN is outside"},
		{"SELECT x FROM docs RIGHT JOIN people", "RIGHT JOIN is outside"},
		{"SELECT x FROM docs FULL JOIN people", "FULL JOIN is outside"},
		{"SELECT x FROM docs CROSS JOIN people", "CROSS JOIN is outside"},
		{"SELECT x FROM docs NATURAL JOIN people", "NATURAL JOIN is outside"},
		{"SELECT x FROM docs d OUTER JOIN people", "OUTER JOIN is outside"},
		{"SELECT x FROM docs INDEXED BY i", "INDEXED BY is outside"},
		{"SELECT x FROM docs NOT INDEXED", "NOT INDEXED is outside"},
		{"SELECT x FROM docs d JOIN people p ON p.key = d.key", "a join of two tables is outside"},
		{"SELECT x FROM walk('a', 1) v JOIN walk('b', 1) w ON v.key = w.key", "a join of two walks is outside"},
		{"SELECT x FROM walk('a', 1) w JOIN docs d", "a join without ON is outside"},
		{"SELECT x FROM walk('a', 1) w JOIN docs d USING (key)", "USING is outside"},
		{"SELECT x FROM walk('a', 1) w JOIN docs d ON d.key = w.key AND d.n = 1", "a join ON anything but one column = another is outside"},
		{"SELECT x FROM walk('a', 1) w JOIN docs d ON d.key < w.key", "a join ON anything but one column = another is outside"},
		{"SELECT x FROM walk('a', 1) w JOIN docs d ON d.key = w.key JOIN people p ON p.key = w.key", "a FROM with more than two sources is outside"},
		{"SELECT x FROM walk('a', 1) w INNER docs d ON d.key = w.key", `near "docs": syntax error`},
		{"SELECT x FROM generate_series(1, 2)", "generate_series(...) as a table is outside"},
		{"SELECT x FROM json_each('[1, 2]')", "json_each() over anything but walk(...) is outside"},
		{"SELECT x FROM json_each(walk('a', 1), '$')", "json_each() with more than walk(...) in it is outside"},
		{"SELECT x FROM json_tree(walk('a', 1))", "json_tree(...) as a table is outside"},
		{"SELECT * FROM walk('a', 1) w JOIN docs d ON d.key = w.key", "* over a join is outside"},
		{"SELECT * FROM json_each(walk('a', 1))", "* over json_each is outside"},
		{"SELECT x FROM walk('a')", "wrong number of arguments to function walk()"},
		{"SELECT x FROM walk('a', 1, 2, 3, 4)", "wrong number of arguments to function walk()"},
		// Subqueries.
		{"SELECT (SELECT 1)", "a subquery other than the one-record one is outside the SQL subset: it's (SELECT field FROM table WHERE key = value)"},
		{"SELECT (SELECT n FROM docs)", "a subquery other than the one-record one"},
		{"SELECT (SELECT count(*) FROM docs)", "a subquery other than the one-record one"},
		{"SELECT (SELECT * FROM docs WHERE key = 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n, m FROM docs WHERE key = 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n AS m FROM docs WHERE key = 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs d WHERE key = 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs WHERE n = 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs WHERE d.key = 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs WHERE key = 1 OR 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs WHERE key = 1 = 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs WHERE key = 1 ORDER BY n)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs WHERE key = 1 LIMIT 1)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs WHERE 1 = key)", "a subquery other than the one-record one"},
		{"SELECT (SELECT n FROM docs WHERE key = 1", "incomplete input"},
		{"SELECT (SELECT true FROM docs WHERE key = 1)", "TRUE is outside"},
		{"SELECT x FROM docs WHERE key IN (SELECT key FROM docs)", "IN over a subquery other than a walk is outside the SQL subset: IN takes (SELECT key FROM walk(...)) or (SELECT value FROM json_each(walk(...)))"},
		{"SELECT x IN (SELECT depth FROM walk('a', 1))", "IN over a subquery other than a walk"},
		{"SELECT x IN (SELECT key FROM json_each(walk('a', 1)))", "IN over a subquery other than a walk"},
		{"SELECT x IN (SELECT value FROM walk('a', 1))", "IN over a subquery other than a walk"},
		{"SELECT x IN (SELECT key FROM walk('a', 1) w)", "IN over a subquery other than a walk"},
		{"SELECT x IN (SELECT key FROM walk('a', 1) WHERE depth = 1)", "IN over a subquery other than a walk"},
		{"SELECT x IN (SELECT key, depth FROM walk('a', 1))", "IN over a subquery other than a walk"},
		{"SELECT x IN ((SELECT key FROM walk('a', 1)))", "a subquery other than the one-record one"},
		{"SELECT x IN (SELECT key FROM walk('a'))", "wrong number of arguments to function walk()"},
		// Writes.
		{"INSERT OR REPLACE INTO t (key) VALUES (1)", "INSERT OR REPLACE is outside"},
		{"INSERT INTO t VALUES (1)", "an INSERT without a list of fields is outside"},
		{"INSERT INTO t SELECT 1", "INSERT ... SELECT is outside"},
		{"INSERT INTO t (key) SELECT 1", "INSERT ... SELECT is outside"},
		{"INSERT INTO t DEFAULT VALUES", "DEFAULT VALUES is outside"},
		{"INSERT INTO t AS u (key) VALUES (1)", "an alias for the table of an INSERT is outside"},
		{"INSERT INTO main.t (key) VALUES (1)", "a table's name with a schema's name before it is outside"},
		{"INSERT INTO t (key) VALUES (1) ON CONFLICT DO NOTHING", "ON CONFLICT"},
		{"INSERT INTO t (key) VALUES (1) RETURNING key", "RETURNING is outside"},
		{"INSERT INTO t (key, a) VALUES (1)", "1 values for 2 columns"},
		{"INSERT INTO t (key) VALUES (1, 2)", "2 values for 1 columns"},
		{"INSERT INTO t (key) VALUES (1), (1, 2)", "all VALUES must have the same number of terms"},
		{"INSERT INTO t (key, KEY) VALUES (1, 2)", "the field KEY is named twice"},
		{"INSERT INTO t (key, a, \"A\") VALUES (1, 2, 3)", "the field A is named twice"},
		{"INSERT INTO DOCS (title) VALUES ('no key')", "NOT NULL constraint failed: docs.key"},
		{"INSERT INTO t (key) VALUES (title)", "an INSERT's values can't use fields: title"},
		{"INSERT INTO t (key) VALUES (count(*))", "misuse of aggregate function count()"},
		{"UPDATE OR IGNORE t SET a = 1", "UPDATE OR IGNORE is outside"},
		{"UPDATE t AS u SET a = 1", "an alias for the table of a write is outside"},
		{"UPDATE t INDEXED BY i SET a = 1", "INDEXED BY is outside"},
		{"UPDATE t SET (a, b) = (1, 2)", "SET with several fields in parentheses is outside"},
		{"UPDATE t SET a = 1 FROM u", "UPDATE ... FROM is outside"},
		{"UPDATE t SET a = 1 RETURNING a", "RETURNING is outside"},
		{"UPDATE t SET a = 1 ORDER BY a", "ORDER BY on a write is outside"},
		{"UPDATE t SET a = 1 LIMIT 1", "LIMIT on a write is outside"},
		{"UPDATE t SET a = 1, A = 2", "the field A is set twice"},
		{"UPDATE t SET a = max(b)", "misuse of aggregate function max()"},
		{"UPDATE t SET a = 1 WHERE count(*) > 1", "misuse of aggregate function count()"},
		{"UPDATE t SET t.a = 1", `near ".": syntax error`},
		{"DELETE FROM t LIMIT 1", "LIMIT on a write is outside"},
		{"DELETE FROM t WHERE a = 1 ORDER BY a", "ORDER BY on a write is outside"},
		{"DELETE FROM t AS u", "an alias for the table of a write is outside"},
		{"DELETE FROM t WHERE sum(a) > 1", "misuse of aggregate function sum()"},
		{"DELETE t", `near "t": syntax error`},
		// Functions, wherever they're called, checked once the statement has
		// parsed, as SQLite checks them.
		{"SELECT nosuchfunc(1)", "no such function: nosuchfunc"},
		{"SELECT nosuchfunc(1) +", "incomplete input"},
		{"SELECT key FROM walk(nosuchfunc(1), 2)", "no such function: nosuchfunc"},
		{"SELECT key FROM docs WHERE key IN (SELECT key FROM walk('a', abs(1, 2)))", "wrong number of arguments to function abs()"},
		{"SELECT (SELECT n FROM docs WHERE key = printf('%d', 1))", "printf() is outside"},
		{"SELECT key FROM docs ORDER BY key LIMIT abs()", "wrong number of arguments to function abs()"},
		{"INSERT INTO docs (key) VALUES (nosuchfunc(1))", "no such function: nosuchfunc"},
		{"UPDATE docs SET n = nosuchfunc(1)", "no such function: nosuchfunc"},
		{"DELETE FROM docs WHERE abs(1, 2)", "wrong number of arguments to function abs()"},
		{"SELECT printf('%d', 1)", "printf() is outside"},
		{"SELECT group_concat(key) FROM t", "group_concat() is outside"},
		{"SELECT iif(1, 2, 3)", "iif() is outside"},
		{"SELECT like('a', 'b')", "like() is outside"},
		{"SELECT json_each(walk('a', 1))", "json_each() in an expression is outside the SQL subset: it goes in a FROM"},
		{"SELECT abs(1, 2)", "wrong number of arguments to function abs()"},
		{"SELECT ABS()", "wrong number of arguments to function ABS()"},
		{"SELECT coalesce(1)", "wrong number of arguments to function coalesce()"},
		{"SELECT substr('a')", "wrong number of arguments to function substr()"},
		{"SELECT walk('a')", "wrong number of arguments to function walk()"},
		{"SELECT walk('a', 1, 2, 3, 4)", "wrong number of arguments to function walk()"},
		{"SELECT abs(*)", "wrong number of arguments to function abs()"},
		{"SELECT max(*) FROM t", "wrong number of arguments to function max()"},
		{"SELECT count() FROM t", "count() without an argument is outside the SQL subset: write count(*)"},
		{"SELECT count(a, b) FROM t", "wrong number of arguments to function count()"},
		{"SELECT count(DISTINCT a) FROM t", "DISTINCT in a function's arguments is outside"},
		{"SELECT count(ALL a) FROM t", "ALL in a function's arguments is outside"},
		{"SELECT max(a ORDER BY a) FROM t", "ORDER BY in a function's arguments is outside"},
		{"SELECT count(*) FILTER (WHERE a) FROM t", "FILTER is outside"},
		{"SELECT row_number() OVER (ORDER BY a) FROM t", "a window function, with OVER, is outside"},
		{"SELECT max(a) OVER w FROM t", "a window function, with OVER, is outside"},
		{"SELECT date(joined) FROM people", "date() on anything but text literals is outside the SQL subset: it takes 'now' and modifiers such as '+1 day'"},
		{"SELECT date('now', ?)", "date() on anything but text literals is outside"},
		{"SELECT datetime(NULL)", "datetime() on anything but text literals is outside"},
		{"SELECT date(1)", "date() on anything but text literals is outside"},
		{"SELECT date()", "date() without an argument is outside the SQL subset: write date('now')"},
		{"SELECT DateTime()", "DateTime() without an argument is outside the SQL subset: write datetime('now')"},
		{"SELECT abs(" + strings.TrimSuffix(strings.Repeat("1, ", maxArgs+1), ", ") + ")", "too many arguments on function abs"},
		// CAST.
		{"SELECT CAST(1 AS INT)", "CAST to INT is outside the SQL subset: CAST takes INTEGER, REAL, TEXT, BLOB and NUMERIC"},
		{"SELECT CAST(1 AS VARCHAR(10))", "CAST to VARCHAR is outside"},
		{`SELECT CAST(1 AS "INTEGER")`, `CAST to "INTEGER" is outside`},
		{"SELECT CAST(1 AS INTEGER(10))", "CAST to a type with more than its name is outside"},
		{"SELECT CAST(1 AS INTEGER PRIMARY KEY)", `near "PRIMARY": syntax error`},
		{"SELECT CAST(1 AS", "incomplete input"},
		{"SELECT CAST(1)", `near ")": syntax error`},
		// Aggregates.
		{"SELECT key, count(*) FROM docs", "key beside an aggregate is outside the SQL subset: in a query with an aggregate, fields go only inside an aggregate's argument"},
		{"SELECT count(*) + n FROM docs", "n beside an aggregate is outside"},
		{"SELECT count(*), walk(key, 1) FROM docs", "key beside an aggregate is outside"},
		{"SELECT count(count(*)) FROM docs", "misuse of aggregate function count()"},
		{"SELECT max(min(n)) FROM docs", "misuse of aggregate function min()"},
		{"SELECT abs(sum(abs(sum(n)))) FROM docs", "misuse of aggregate function sum()"},
		{"SELECT key FROM docs WHERE count(*) > 1", "misuse of aggregate function count()"},
		{"SELECT key FROM docs ORDER BY count(*)", "misuse of aggregate: count()"},
		{"SELECT key FROM docs LIMIT count(*)", "misuse of aggregate function count()"},
		{"SELECT count(*) FROM docs LIMIT 1 OFFSET max(n)", "misuse of aggregate function max()"},
		{"SELECT count(*) FROM docs LIMIT 1 OFFSET max(1)", "misuse of aggregate function max()"},
		{"SELECT (SELECT n FROM docs WHERE key = max(1)) FROM docs", "misuse of aggregate function max()"},
		{"SELECT key FROM walk(count(*), 1)", "misuse of aggregate function count()"},
		// Fields where they can't go.
		{"SELECT key FROM docs LIMIT n", "LIMIT and OFFSET can't use fields: n"},
		{"SELECT key FROM docs LIMIT 1 OFFSET d.n", "LIMIT and OFFSET can't use fields: d.n"},
		{"SELECT key FROM walk(title, 1)", "a walk's arguments in a FROM or an IN can't use fields: title"},
		{"SELECT d.key FROM docs d JOIN json_each(walk(d.key, 1)) w ON d.key = w.value", "a walk's arguments in a FROM or an IN can't use fields: d.key"},
		{"SELECT key FROM docs WHERE key IN (SELECT key FROM walk(key, 1))", "a walk's arguments in a FROM or an IN can't use fields: key"},
		{"SELECT (SELECT n FROM docs WHERE key = d.key) FROM docs d", "the one-record subquery's key can't use fields: d.key"},
		// SQLite's limits.
		{"SELECT " + strings.TrimSuffix(strings.Repeat("1, ", maxColumns+1), ", "), "too many columns in result set"},
		{"SELECT 1 ORDER BY " + strings.TrimSuffix(strings.Repeat("1, ", maxColumns+1), ", "), "too many terms in ORDER BY clause"},
		{"UPDATE t SET " + strings.TrimSuffix(strings.Repeat("a = 1, ", maxColumns+1), ", "), "too many columns in set list"},
		{"SELECT 1" + strings.Repeat(" + 1", maxHeight), "Expression tree is too large (maximum depth 1000)"},
		{"SELECT " + strings.Repeat("NOT ", 300) + "1", "nested more than 250 deep"},
		{"SELECT " + deep("(", "1", ")", 300), "nested more than 250 deep"},
		{"SELECT " + deep("abs(", "1", ")", 300), "nested more than 250 deep"},
		{"SELECT " + deep("(SELECT n FROM docs WHERE key = ", "1", ")", 30), "nested more than 250 deep"},
		{"SELECT " + deep("1 IN (", "1", ")", 600), "nested more than 250 deep"},
		{"SELECT 1" + strings.Repeat(" IN (1)", 600), "Expression tree is too large"},
	} {
		_, err := Parse(c.sql)
		var pe *Error
		if err == nil || !errors.As(err, &pe) || !strings.Contains(err.Error(), c.msg) {
			sql := c.sql
			if len(sql) > 80 {
				sql = sql[:80] + "..."
			}
			t.Errorf("%q gives %v, want %q", sql, err, c.msg)
		}
	}
}

// TestTheLimitsHoldAtTheEdge: SQLite's limits are reached, and taken, one
// short of the refusals in TestRefusals, and what's taken prints back, as
// deep as it was written and no deeper.
func TestTheLimitsHoldAtTheEdge(t *testing.T) {
	n := maxNesting - 2
	for _, q := range []string{
		"SELECT " + strings.TrimSuffix(strings.Repeat("1, ", maxColumns), ", "),
		"SELECT 1" + strings.Repeat(" + 1", maxHeight-1),
		"SELECT 1" + strings.Repeat(" AND 1 OR 1", (maxHeight-1)/2),
		"SELECT 1" + strings.Repeat(" BETWEEN 0 AND 2", maxHeight-1),
		"SELECT " + strings.Repeat("(", maxNesting-1) + "1" + strings.Repeat(")", maxNesting-1),
		"SELECT coalesce(" + strings.TrimSuffix(strings.Repeat("1, ", maxArgs), ", ") + ")",
		"SELECT " + strings.Repeat("NOT ", n) + "1",
		"SELECT " + strings.Repeat("- ", n) + "1",
		"SELECT " + strings.Repeat("- NOT ", n/2) + "1",
		"SELECT " + strings.Repeat("1 = NOT ", n/2) + "1",
		"SELECT " + strings.Repeat("1 + (", n/2) + "1" + strings.Repeat(")", n/2),
		"SELECT " + strings.Repeat("(SELECT n FROM t WHERE key = ", 20) + "1" + strings.Repeat(")", 20),
		"SELECT " + strings.Repeat("x IS NOT NOT ", n/2) + "1",
		"SELECT " + strings.Repeat("x IS (NOT ", n/3) + "1" + strings.Repeat(")", n/3),
		"SELECT " + strings.Repeat("abs(", n) + "1" + strings.Repeat(")", n),
	} {
		s, err := Parse(q)
		if err != nil {
			t.Errorf("%.40s...: %v", q, err)
			continue
		}
		checkPrintsBack(t, q, s)
	}
}

// TestConditions: Nearest's filter.
func TestConditions(t *testing.T) {
	for _, q := range []string{"", "   ", "\t\n", "\v "} {
		if e, n, err := ParseCondition(q); e != nil || n != 0 || err != nil {
			t.Errorf("%q gives %v, %d, %v", q, e, n, err)
		}
	}
	e, n, err := ParseCondition(`"group" = ?`)
	if err != nil || n != 1 || dumpExpr(e) != `(= "group" ?0)` {
		t.Errorf("gives %s, %d, %v", dumpExpr(e), n, err)
	}
	e, n, err = ParseCondition(`status = ? AND vec IS NOT NULL OR key IN (SELECT key FROM walk(?, 2))`)
	if err != nil || n != 2 || dumpExpr(e) != "(OR (AND (= status ?0) (IS NOT vec NULL)) (IN key (walk ?1 2)))" {
		t.Errorf("gives %s, %d, %v", dumpExpr(e), n, err)
	}
	for _, c := range []struct{ cond, msg string }{
		{"1) OR (1", `near ")": syntax error`},
		{"nosuchfunc(n)", "no such function: nosuchfunc"},
		{"a = 1;", `near ";": syntax error`},
		{"count(*) > 1", "misuse of aggregate function count()"},
		{"-- just a comment", "incomplete input"},
		{"a = ", "incomplete input"},
		{"SELECT 1", `near "SELECT": syntax error`},
	} {
		if _, _, err := ParseCondition(c.cond); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q gives %v, want %q", c.cond, err, c.msg)
		}
	}
}

// TestTheTreeIsWhatTheTestsSay checks the Call, Source and Ident helpers
// the later tasks use.
func TestTheTreeIsWhatTheTestsSay(t *testing.T) {
	sel := mustParse(t, `SELECT COUNT(*), Max(n), "Sum"(x) FROM "Docs" AS "D"`).(*Select)
	c := sel.Results[0].Expr.(*Call)
	if c.Func() != "count" || !c.Aggregate() || c.Name.Name != "COUNT" {
		t.Errorf("COUNT(*): %q %v", c.Func(), c.Aggregate())
	}
	if m := sel.Results[1].Expr.(*Call); !m.Aggregate() || m.Func() != "max" {
		t.Error("max(n) isn't an aggregate")
	}
	if s := sel.Results[2].Expr.(*Call); !s.Aggregate() || !s.Name.Quoted {
		t.Error(`"Sum"(x) isn't an aggregate`)
	}
	if m := exprOf(t, "max(n, 1)").(*Call); m.Aggregate() {
		t.Error("max(n, 1) is an aggregate")
	}
	src := sel.From.Sources[0]
	if src.Name() != "d" || src.Table.Folded() != "docs" || src.Table.Name != "Docs" || !src.Table.Quoted {
		t.Errorf("the source is %q, %+v", src.Name(), src.Table)
	}
	for q, name := range map[string]string{
		"SELECT key FROM walk('a', 1)":              "walk",
		"SELECT value FROM json_each(walk('a', 1))": "json_each",
		"SELECT key FROM Docs":                      "docs",
	} {
		if got := mustParse(t, q).(*Select).From.Sources[0].Name(); got != name {
			t.Errorf("%s: the source is called %q", q, got)
		}
	}
	for op := OpOr; op <= OpNot; op++ {
		if op.String() == "Op(?)" {
			t.Errorf("Op %d has no text", op)
		}
	}
	if Op(0).String() != "Op(?)" || LitKind(9).String() != "LitKind(?)" || LitBytes.String() != "bytes" {
		t.Error("the names of kinds are wrong")
	}
	if fold("ÉtÉ ABC") != "ÉtÉ abc" || fold("abc") != "abc" {
		t.Error("fold folds more than ASCII")
	}
}
