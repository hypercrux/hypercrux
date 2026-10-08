// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && cgo

package query

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The parser against SQLite itself, as 0.x runs it, which is what the Beta
// has to agree with. These tests need cgo, as 0.x does, and the rest of
// the parser's tests don't.
//
// A strict printer puts every operator's operands in parentheses, which
// SQLite drops, so SQLite reads exactly the parser's tree. Where the
// parser's tree differs from SQLite's, the strict SQL gives another answer.

func withoutNames(a *sqlcorpus.Answer) *sqlcorpus.Answer {
	if a == nil {
		return nil
	}
	b := *a
	b.Columns = nil
	return &b
}

// TestTreesAreSQLitesOnTheCorpus runs every case the parser takes, printed
// back strictly and as String prints it, through 0.x on the corpus's data,
// and each must give 0.x's recorded answer, apart from the names of
// columns, which come from the text.
func TestTreesAreSQLitesOnTheCorpus(t *testing.T) {
	e := zerox.Engine{}
	db, err := sqlcorpus.OpenFixture(e, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n := 0
	for _, cs := range loadCorpus(t) {
		if cs.Answer == nil || cs.Via != "" {
			continue // a "now" case, or a form 0.x hasn't got
		}
		s, err := Parse(cs.SQL)
		if err != nil {
			continue
		}
		for _, strict := range []bool{true, false} {
			again := cs
			again.SQL = (&printer{strict: strict}).statement(s)
			got := sqlcorpus.Run(e, db, again, false)
			if !sqlcorpus.Same(withoutNames(cs.Answer), withoutNames(got), cs.Close) {
				t.Errorf("%s: %s, printed as %s, gives another answer in 0.x:\n  want %+v\n  got  %+v", cs.ID, cs.SQL, again.SQL, cs.Answer, got)
			}
		}
		n++
	}
	if n < 3500 {
		t.Errorf("only %d cases run", n)
	}
	t.Logf("%d cases give 0.x's answers printed back", n)
}

// exprGen makes expressions as text, without regard for how they group,
// so their operators meet in every order.
type exprGen struct{ r *rand.Rand }

func (g *exprGen) pick(xs ...string) string { return xs[g.r.IntN(len(xs))] }

func (g *exprGen) leaf() string {
	return g.pick("0", "1", "2", "-1", "7", "9223372036854775807", "9223372036854775808", "0.5", "-0.0", "1e999",
		"'a'", "'A'", "''", "'1'", "'%'", "'a_'", "x'00'", "x''", "NULL", "null", "'x'")
}

func (g *exprGen) expr(depth int) string {
	if depth == 0 || g.r.IntN(5) == 0 {
		return g.leaf()
	}
	sub := func() string { return g.expr(depth - 1) }
	switch g.r.IntN(13) {
	case 0, 1, 2, 3:
		return sub() + " " + g.pick("OR", "AND", "=", "==", "!=", "<>", "<", "<=", ">", ">=", "+", "-", "*", "/", "%", "||", "IS", "IS NOT") + " " + sub()
	case 4:
		return g.pick("NOT ", "-", "+", "- ", "NOT NOT ", "-+", "+-") + sub()
	case 5:
		return sub() + g.pick(" BETWEEN ", " NOT BETWEEN ") + sub() + " AND " + sub()
	case 6:
		s := sub() + g.pick(" LIKE ", " NOT LIKE ") + sub()
		if g.r.IntN(3) == 0 {
			s += " ESCAPE " + sub()
		}
		return s
	case 7:
		items := make([]string, g.r.IntN(3))
		for i := range items {
			items[i] = sub()
		}
		return sub() + g.pick(" IN (", " NOT IN (") + strings.Join(items, ", ") + ")"
	case 8:
		return sub() + g.pick(" IS NULL", " IS NOT NULL")
	case 9:
		return "(" + sub() + ")"
	case 10:
		return "CAST(" + sub() + " AS " + g.pick("INTEGER", "REAL", "TEXT", "BLOB", "NUMERIC") + ")"
	case 11:
		return g.pick("abs", "typeof", "length", "lower") + "(" + sub() + ")"
	}
	return g.pick("coalesce", "nullif", "max", "min", "instr") + "(" + sub() + ", " + sub() + ")"
}

// chain makes a run of operands and operators without parentheses, over
// small values, where grouping them another way almost always changes the
// answer.
func (g *exprGen) chain(n int) string {
	term := func() string {
		s := g.pick("0", "1", "2", "3", "5", "-1", "'1'", "'a'", "NULL", "2.5")
		switch g.r.IntN(10) {
		case 0:
			s = g.pick("NOT ", "-", "+", "- ", "NOT -") + s
		case 1:
			s = "(" + g.chain(2) + ")"
		}
		return s
	}
	s := term()
	for i := 0; i < n; i++ {
		switch g.r.IntN(12) {
		case 0:
			s += g.pick(" BETWEEN ", " NOT BETWEEN ") + term() + " AND " + term()
		case 1:
			s += g.pick(" LIKE ", " NOT LIKE ") + term()
		case 2:
			s += g.pick(" IN (", " NOT IN (") + term() + ", " + term() + ")"
		case 3:
			s += g.pick(" IS NULL", " IS NOT NULL", " IS ", " IS NOT ")
			if !strings.HasSuffix(s, "NULL") {
				s += term()
			}
		default:
			s += " " + g.pick("OR", "AND", "=", "!=", "<", "<=", ">", ">=", "+", "-", "*", "/", "%", "||") + " " + term()
		}
	}
	return s
}

// TestTreesAreSQLitesOnRandomExpressions runs random expressions through
// 0.x as written and printed strictly, and both must give the same answer.
// Where the parser refuses one with a syntax error, so must 0.x.
func TestTreesAreSQLitesOnRandomExpressions(t *testing.T) {
	e := zerox.Engine{}
	db, err := sqlcorpus.OpenFixture(e, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	g := &exprGen{r: rand.New(rand.NewPCG(3, 0x5133))}
	n := 20000
	if testing.Short() {
		n = 4000
	}
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		var q string
		if i%2 == 0 {
			q = "SELECT " + g.expr(1+g.r.IntN(4)) + " AS v"
		} else {
			q = "SELECT " + g.chain(2+g.r.IntN(5)) + " AS v"
		}
		want := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: q}, false)
		s, err := Parse(q)
		if err != nil {
			counts["refused"]++
			if syntaxKind(err.Error()) != "" && syntaxKind(want.Message) == "" {
				t.Errorf("%s: the parser gives %v, and 0.x gives %+v", q, err, want)
			}
			continue
		}
		counts["taken"]++
		strict := (&printer{strict: true}).statement(s)
		got := sqlcorpus.Run(e, db, sqlcorpus.Case{SQL: strict}, false)
		if !sqlcorpus.Same(withoutNames(want), withoutNames(got), false) {
			t.Errorf("%s gives %+v in 0.x, and the parser's tree, %s, gives %+v", q, want, strict, got)
		}
	}
	t.Logf("%v", counts)
}

// keywordsOfSQLite are SQLite 3.53.4's keywords, as its keyword table has
// them, and a few names SQLite reads specially.
const keywordsOfSQLite = `ABORT ACTION ADD AFTER ALL ALTER ALWAYS ANALYZE AND AS ASC ATTACH
AUTOINCREMENT BEFORE BEGIN BETWEEN BY CASCADE CASE CAST CHECK COLLATE COLUMN COMMIT CONFLICT
CONSTRAINT CREATE CROSS CURRENT CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP DATABASE DEFAULT
DEFERRABLE DEFERRED DELETE DESC DETACH DISTINCT DO DROP EACH ELSE END ESCAPE EXCEPT EXCLUDE
EXCLUSIVE EXISTS EXPLAIN FAIL FILTER FIRST FOLLOWING FOR FOREIGN FROM FULL GENERATED GLOB GROUP
GROUPS HAVING IF IGNORE IMMEDIATE IN INDEX INDEXED INITIALLY INNER INSERT INSTEAD INTERSECT INTO
IS ISNULL JOIN KEY LAST LEFT LIKE LIMIT MATCH MATERIALIZED NATURAL NO NOT NOTHING NOTNULL NULL
NULLS OF OFFSET ON OR ORDER OTHERS OUTER OVER PARTITION PLAN PRAGMA PRECEDING PRIMARY QUERY RAISE
RANGE RECURSIVE REFERENCES REGEXP REINDEX RELEASE RENAME REPLACE RESTRICT RETURNING RIGHT ROLLBACK
ROW ROWS SAVEPOINT SELECT SET TABLE TEMP TEMPORARY THEN TIES TO TRANSACTION TRIGGER UNBOUNDED
UNION UNIQUE UPDATE USING VACUUM VALUES VIEW VIRTUAL WHEN WHERE WINDOW WITH WITHOUT
TRUE FALSE ROWID OID _ROWID_ WALK JSON_EACH`

// TestKeywordsReadAsSQLiteReadsThem puts each of SQLite's keywords in each
// place where the parser takes a bare name. Wherever the parser takes the
// statement, SQLite must run it on tables and fields named after the
// keywords, and give the columns the names the parser's rules give.
func TestKeywordsReadAsSQLiteReadsThem(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	words := strings.Fields(keywordsOfSQLite)
	cols := []string{`x`, `"key"`}
	for _, w := range words {
		lw := strings.ToLower(w)
		if lw != "key" {
			cols = append(cols, `"`+lw+`"`)
		}
		if _, err := db.Exec(`CREATE TABLE "` + lw + `" (x, "key")`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE t (` + strings.Join(cols, ", ") + `)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t (x, "key") VALUES (1, 't:1')`); err != nil {
		t.Fatal(err)
	}
	places := []string{
		"SELECT § FROM t", "SELECT §, x FROM t", "SELECT x § FROM t", "SELECT x §, 2 FROM t", "SELECT 1 §",
		"SELECT (x) § FROM t", "SELECT max(x) § FROM t", "SELECT x AS § FROM t", "SELECT x FROM §",
		"SELECT §.x FROM t §", "SELECT x FROM t §", "SELECT x FROM t § WHERE 1", "SELECT x FROM t § ORDER BY x",
		"SELECT x FROM t § LIMIT 1", "SELECT §.x FROM t AS §", "SELECT (§) FROM t", "SELECT x FROM t WHERE x IN (§)",
		"SELECT x FROM t WHERE x IN (1, §)", "SELECT t.§ FROM t", "SELECT x FROM t ORDER BY §",
		"SELECT x FROM t ORDER BY § DESC", "SELECT x FROM t ORDER BY 1, §", "SELECT x FROM t WHERE x BETWEEN § AND 1",
		"SELECT x FROM t WHERE x BETWEEN 0 AND §", "SELECT x FROM t WHERE NOT §", "SELECT -§ FROM t",
		"SELECT 1 + § FROM t", "SELECT x FROM t WHERE x = §", "SELECT x FROM t WHERE § = 1", "SELECT x FROM t WHERE § IS NULL",
		"SELECT x FROM t WHERE (§)", "SELECT x FROM t WHERE x = 1 AND §", "SELECT x FROM t WHERE x LIKE §",
		"SELECT x FROM t WHERE x IS §", "SELECT count(§) FROM t", "SELECT abs(§) FROM t", "SELECT x || § FROM t",
		"SELECT CAST(§ AS TEXT) FROM t", "SELECT (SELECT § FROM t WHERE key = 't:1')", "SELECT (SELECT x FROM § WHERE key = 1)",
		"INSERT INTO t (key, §) VALUES (1, 1)", "INSERT INTO § (key, x) VALUES (1, 1)", "UPDATE t SET § = 1",
		"UPDATE § SET x = 1", "DELETE FROM §", "UPDATE t SET x = § WHERE §", "DELETE FROM t WHERE § = §",
	}
	taken := 0
	for _, w := range words {
		for _, place := range places {
			for _, word := range []string{w, strings.ToLower(w)} {
				q := strings.ReplaceAll(place, "§", word)
				s, err := Parse(q)
				if err != nil {
					continue
				}
				taken++
				if _, isSelect := s.(*Select); !isSelect {
					if _, err := db.Exec(q); err != nil {
						t.Errorf("%s: the parser takes it, and SQLite gives %v", q, err)
					}
					continue
				}
				rows, err := db.Query(q)
				if err != nil {
					t.Errorf("%s: the parser takes it, and SQLite gives %v", q, err)
					continue
				}
				names, _ := rows.Columns()
				for rows.Next() {
				}
				if err := rows.Err(); err != nil {
					t.Errorf("%s: the parser takes it, and SQLite gives %v", q, err)
				}
				rows.Close()
				if want := columnNames(s.(*Select)); want != nil && fmt.Sprint(want) != fmt.Sprint(names) {
					t.Errorf("%s: the parser calls the columns %q, and SQLite %q", q, want, names)
				}
			}
		}
	}
	t.Logf("%d statements taken, of %d", taken, 2*len(words)*len(places))
}

// columnNames gives a SELECT's columns' names by SQL.md's rules, for the
// tables in TestKeywordsReadAsSQLiteReadsThem, whose fields' names are in
// lower case.
func columnNames(s *Select) []string {
	if s.Star {
		return nil
	}
	var names []string
	for _, r := range s.Results {
		switch c, ok := r.Expr.(*Column); {
		case r.Alias != nil:
			names = append(names, r.Alias.Name)
		case ok:
			names = append(names, c.Name.Folded())
		default:
			names = append(names, r.Text)
		}
	}
	return names
}
