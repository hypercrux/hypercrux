// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sqlcorpus

import (
	"fmt"
	"math/rand/v2"
	"strings"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/difftest"
)

// Build makes every case in the corpus, without answers, in a fixed order.
// The same code always gives the same cases, which TestCorpusMatchesItsSource
// holds the saved corpus to.
func Build() []Case {
	var cases []Case
	for i, s := range statements() {
		s.ID, s.Kind = fmt.Sprintf("statement-%04d", i+1), "statement"
		cases = append(cases, s)
	}
	for i, e := range expressions(3000) {
		cases = append(cases, Case{ID: fmt.Sprintf("expression-%04d", i+1), Kind: "expression",
			SQL: "SELECT " + e.sql + " AS v", Args: e.args, In: true})
	}
	for i, d := range dates() {
		d.ID, d.Kind = fmt.Sprintf("date-%04d", i+1), "date"
		cases = append(cases, d)
	}
	for i, d := range nowCases() {
		d.ID, d.Kind, d.In = fmt.Sprintf("now-%02d", i+1), "now", true
		cases = append(cases, d)
	}
	return cases
}

func v(x any) difftest.Value { return difftest.Value{V: x} }

func in(sql string, args ...any) Case  { return Case{SQL: sql, In: true, Args: values(args)} }
func out(sql string, args ...any) Case { return Case{SQL: sql, Args: values(args)} }

func values(args []any) []difftest.Value {
	var out []difftest.Value
	for _, a := range args {
		out = append(out, v(a))
	}
	return out
}

func (cs Case) with(note string) Case { cs.Note = note; return cs }
func (cs Case) via(sql string) Case   { cs.Via = sql; return cs }
func (cs Case) after(sql string) Case { cs.After = sql; return cs }
func (cs Case) close() Case           { cs.Close = true; return cs }
func each(sql string, preds []string) []Case {
	var out []Case
	for _, p := range preds {
		out = append(out, in(strings.ReplaceAll(sql, "{}", p)))
	}
	return out
}

// statements are written by hand, from BETA.md's "Queries and SQL". Which
// are inside the Beta's subset is settled in beta/SQL.md, and each one
// outside it says why in its note.
func statements() []Case {
	var s []Case
	add := func(cs ...Case) { s = append(s, cs...) }

	// Plain reads: columns, aliases, expressions, order, limits.
	add(
		in(`SELECT * FROM docs ORDER BY key`).with("SELECT * gives the fields in the table's order"),
		in(`SELECT * FROM people ORDER BY key`),
		in(`SELECT * FROM t_1 ORDER BY key`),
		in(`SELECT key FROM docs ORDER BY key`),
		in(`SELECT key, title FROM docs ORDER BY key`),
		in(`SELECT title, key FROM docs ORDER BY key`),
		in(`SELECT key AS k, title AS t FROM docs ORDER BY k`),
		in(`SELECT key, n * 2 AS twice, n + 0.5 AS half FROM docs ORDER BY key`),
		in(`SELECT key, title || ' (' || status || ')' AS label FROM docs ORDER BY key`),
		in(`SELECT key, typeof(n), typeof(score), typeof(data), typeof(vec), typeof(tags) FROM docs ORDER BY key`),
		in(`SELECT 1, 'a', NULL, 2.5, x'01'`).with("column names of literals"),
		in(`SELECT key, n, score FROM docs ORDER BY key`),
		in(`SELECT key FROM docs ORDER BY key DESC`),
		in(`SELECT key, n FROM docs ORDER BY n, key`).with("NULL first, then numbers, then text"),
		in(`SELECT key, n FROM docs ORDER BY n DESC, key`),
		in(`SELECT key, score FROM docs ORDER BY score, key`).with("-0 and 0 compare equal"),
		in(`SELECT key, title FROM docs ORDER BY title, key`),
		in(`SELECT key, status FROM docs ORDER BY status DESC, key ASC`),
		in(`SELECT key, n FROM docs ORDER BY 2, 1`).with("ORDER BY a column's number"),
		in(`SELECT key, n * -1 AS m FROM docs ORDER BY m, key`).with("ORDER BY an alias"),
		in(`SELECT key FROM docs ORDER BY length(title), key`).with("ORDER BY an expression"),
		in(`SELECT key FROM docs ORDER BY key LIMIT 3`),
		in(`SELECT key FROM docs ORDER BY key LIMIT 3 OFFSET 2`),
		in(`SELECT key FROM docs ORDER BY key LIMIT 0`),
		in(`SELECT key FROM docs ORDER BY key LIMIT -1`).with("a negative limit means no limit"),
		in(`SELECT key FROM docs ORDER BY key LIMIT 100 OFFSET 7`),
		in(`SELECT key FROM docs ORDER BY key LIMIT ? OFFSET ?`, int64(2), int64(1)),
		out(`SELECT key FROM docs ORDER BY key LIMIT 2, 3`).with("SQLite's LIMIT offset, count; the subset has LIMIT and OFFSET"),
		in(`SELECT name, age FROM people ORDER BY age, key`),
		in(`SELECT name FROM people WHERE email IS NULL ORDER BY key`),
		in(`SELECT * FROM docs WHERE key = 'docs:3'`),
		in(`SELECT * FROM docs WHERE key = ?`, "docs:1"),
		in(`SELECT title FROM docs WHERE key = 'docs:404'`).with("no rows"),
		in(`SELECT x, y, x * y, x / 2, x / 2.0, x % 2, -y FROM t_1 ORDER BY key`),
		in(`SELECT key, joined FROM people WHERE joined > '2025' ORDER BY joined`),
	)

	// WHERE, one condition at a time.
	add(each(`SELECT key FROM docs WHERE {} ORDER BY key`, []string{
		"n > 2", "n >= 3", "n < 3", "n <= 1", "n = 3", "n == 3", "n != 3", "n <> 3",
		"n = '5'", "n = 5", "n > '4'", "n = 3.0", "n > 2.5",
		"title = 'Q3 plan'", "title = 'q3 plan'", "title > 'Q'", "title < 'R'", "title = ''",
		"title IS NULL", "title IS NOT NULL", "status IS NULL", "n IS NULL", "n IS NOT NULL",
		"n IS 3", "n IS NOT 3", "score > 1", "score = 3", "score = 3.0", "score < 0", "score = 0", "score = -0.0",
		"data IS NOT NULL", "data = x'000102'", "data = x''", "vec IS NOT NULL", "vec IS NULL",
		"n > 2 AND status = 'open'", "n > 5 OR status = 'done'", "NOT (n > 2)", "NOT n > 2",
		"n > 2 AND (status = 'open' OR status = 'OPEN')", "n > 2 AND status = 'open' OR n < 0",
		"n IN (1, 3, 7)", "n NOT IN (1, 3, 7)", "n IN ()", "n NOT IN ()", "status IN ('open', 'done')",
		"n IN (1, NULL)", "n NOT IN (1, NULL)",
		"n BETWEEN 2 AND 7", "n NOT BETWEEN 2 AND 7", "score BETWEEN 0 AND 2", "title BETWEEN 'H' AND 'R'",
		"title LIKE 'Q3%'", "title LIKE 'q3%'", "title LIKE '%o%'", "title LIKE '_etro'",
		`title LIKE 'a\%b\_c' ESCAPE '\'`, "title LIKE 'a%b%'", "title NOT LIKE 'Q%'", "status LIKE 'open'",
		"title LIKE 'É%'", "title LIKE 'é%'", "title LIKE '%'", "title LIKE ''",
		"length(title) > 5", "lower(status) = 'open'", "upper(title) = 'RETRO'", "substr(title, 1, 2) = 'Q3'",
		"n % 2 = 1", "n * 2 > 10", "n + score > 5", "coalesce(n, 0) = 0", "ifnull(status, 'none') = 'none'",
		"key > 'docs:4'", "key LIKE 'docs:%'", "1", "0", "NULL", "'a'", "'1'", "(n > 2) = 1", "n > 2 IS 1",
		"abs(n) = 2", "round(score) = 2", "instr(title, '3') > 0", "replace(title, 'Q3', 'Q4') = 'Q4 plan'",
		"trim(title) = title", "typeof(n) = 'text'", "typeof(score) = 'integer'",
	})...)
	add(
		in(`SELECT key FROM docs WHERE n > ? ORDER BY key`, int64(2)),
		in(`SELECT key FROM docs WHERE title = ? ORDER BY key`, "Retro"),
		in(`SELECT key FROM docs WHERE n IN (?, ?) ORDER BY key`, int64(1), int64(7)),
		in(`SELECT key FROM docs WHERE score > ? ORDER BY key`, 1.0),
		in(`SELECT key FROM docs WHERE data = ? ORDER BY key`, []byte{0, 1, 2}),
		in(`SELECT key FROM docs WHERE n = ? ORDER BY key`, "5").with("a text argument against a text value"),
		in(`SELECT key FROM docs WHERE n IS ? ORDER BY key`, nil),
	)

	// Aggregates over the whole result.
	add(
		in(`SELECT count(*) FROM docs`),
		in(`SELECT count(n), count(score), count(vec), count(data) FROM docs`),
		in(`SELECT sum(n), total(n), avg(n), min(n), max(n) FROM docs`).with("n holds text in one record"),
		in(`SELECT min(title), max(title) FROM docs`),
		in(`SELECT sum(score), avg(score), min(score), max(score) FROM docs`),
		in(`SELECT count(*) FROM docs WHERE status = 'open'`),
		in(`SELECT sum(n), total(n), avg(n), min(n), count(n) FROM docs WHERE n > 100`).with("aggregates of nothing"),
		in(`SELECT count(*) FROM people WHERE age > 30`),
		in(`SELECT min(joined), max(joined) FROM people`),
		in(`SELECT count(*), sum(n), avg(n) FROM docs WHERE vec IS NOT NULL`),
		in(`SELECT max(n) + 1, count(*) * 2 AS c FROM docs`),
		in(`SELECT avg(age), sum(age) FROM people`),
		in(`SELECT sum(x), min(y), max(y), avg(y) FROM t_1`),
		in(`SELECT count(*) FROM docs LIMIT 1`),
		in(`SELECT sum(n) FROM docs WHERE status IS NULL`),
		in(`SELECT count(*) AS c FROM t_1 WHERE vec IS NULL`),
		out(`SELECT group_concat(key) FROM docs`).with("group_concat(), whose order isn't set"),
		out(`SELECT count(DISTINCT status) FROM docs`).with("DISTINCT in an aggregate"),
		out(`SELECT key, count(*) FROM docs`).with("a bare column next to an aggregate"),
	)

	// Walks, in 0.x's forms and the Beta's.
	walk2 := `SELECT value AS key FROM json_each(walk('people:1', 2)) ORDER BY key`
	add(
		in(`SELECT walk('people:1', 1)`).with("the function form, which gives JSON text"),
		in(`SELECT walk('people:1', 2)`),
		in(`SELECT walk('people:1', 3, 'owns')`),
		in(`SELECT walk('docs:3', 1, NULL, 'in')`),
		in(`SELECT walk('docs:1', 2, 'cites', 'both')`),
		in(`SELECT walk('t_1:a', 5, 'next')`).with("a cycle"),
		in(`SELECT walk('docs:404', 1)`),
		in(`SELECT walk('people:1', 0)`),
		in(`SELECT walk('people:1', 1, 'owns', 'sideways')`),
		in(`SELECT value FROM json_each(walk('people:1', 2)) ORDER BY value`),
		in(`SELECT value FROM json_each(walk(?, ?)) ORDER BY value`, "people:3", int64(3)),
		in(`SELECT d.key, d.title FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value ORDER BY d.key`),
		in(`SELECT d.key, d.title FROM json_each(walk('people:1', 3)) w JOIN docs d ON d.key = w.value WHERE d.status = 'open' ORDER BY d.key`),
		in(`SELECT d.key, d.title FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value WHERE d.status = 'open' AND d.vec IS NOT NULL ORDER BY distance(d.vec, '[1, 0, 0]') LIMIT 10`).
			with("the crux query from the README"),
		in(`SELECT key FROM docs WHERE key IN (SELECT value FROM json_each(walk('people:1', 2))) ORDER BY key`),
		in(`SELECT key FROM docs WHERE key NOT IN (SELECT value FROM json_each(walk('people:1', 2))) ORDER BY key`),
		in(`SELECT key FROM people WHERE key IN (SELECT value FROM json_each(walk('people:3', 2, 'knows'))) ORDER BY key`),
		out(`SELECT key, value FROM json_each(walk('people:1', 2)) ORDER BY key`).with("json_each's columns other than value"),
		in(`SELECT key FROM walk('people:1', 2) ORDER BY key`).via(walk2).
			with("walk as a table, the Beta's form, with the columns key and depth"),
		in(`SELECT d.key, d.title FROM walk('people:1', 2) w JOIN docs d ON d.key = w.key ORDER BY d.key`).
			via(`SELECT d.key, d.title FROM json_each(walk('people:1', 2)) w JOIN docs d ON d.key = w.value ORDER BY d.key`),
		in(`SELECT key FROM docs WHERE key IN (SELECT key FROM walk('people:1', 2)) ORDER BY key`).
			via(`SELECT key FROM docs WHERE key IN (SELECT value FROM json_each(walk('people:1', 2))) ORDER BY key`),
		in(`SELECT key FROM docs WHERE key NOT IN (SELECT key FROM walk('people:1', 2)) ORDER BY key`).
			via(`SELECT key FROM docs WHERE key NOT IN (SELECT value FROM json_each(walk('people:1', 2))) ORDER BY key`),
		in(`SELECT key FROM walk('docs:3', 1, NULL, 'in') ORDER BY key`).
			via(`SELECT value AS key FROM json_each(walk('docs:3', 1, NULL, 'in')) ORDER BY key`),
	)

	// The one-record subquery, distance and vector.
	add(
		in(`SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM docs WHERE key = 'docs:2')), key LIMIT 3`),
		in(`SELECT key, distance(vec, (SELECT vec FROM t_1 WHERE key = 't_1:a')) AS d FROM t_1 WHERE vec IS NOT NULL ORDER BY d, key`).close(),
		in(`SELECT (SELECT title FROM docs WHERE key = 'docs:1')`),
		in(`SELECT key FROM docs WHERE n > (SELECT n FROM docs WHERE key = 'docs:1') ORDER BY key`),
		in(`SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, (SELECT vec FROM docs WHERE key = ?)), key LIMIT 2`, "docs:404").
			with("a missing record's vector is NULL"),
		in(`SELECT key, distance(vec, '[1, 0, 0]') AS d FROM docs WHERE vec IS NOT NULL ORDER BY d, key`).close(),
		in(`SELECT key FROM docs ORDER BY distance(vec, '[1, 0, 0]'), key`).with("rows without a vector come first"),
		in(`SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, ?), key LIMIT 2`, c.Vector{0, 1, 0}),
		in(`SELECT key FROM t_1 WHERE vec IS NOT NULL ORDER BY distance(vec, '[1, 0]') DESC, key`),
		in(`SELECT key FROM docs WHERE status = 'open' AND vec IS NOT NULL ORDER BY distance(vec, '[0, 1, 0]'), key LIMIT 2`),
		in(`SELECT key FROM docs WHERE vec IS NOT NULL AND distance(vec, '[1, 0, 0]') < 0.05 ORDER BY key`),
		in(`SELECT distance('[1, 0]', '[0, 1]'), distance('[1, 0]', '[1, 0]'), distance('[1, 0]', '[-1, 0]')`).close(),
		in(`SELECT distance(vector('[1, 2]'), '[2, 4]'), distance('[3, 4]', '[4, 3]')`).close(),
		in(`SELECT distance(NULL, '[1]'), distance('[1]', NULL)`),
		in(`SELECT distance('[1, 0]', '[1, 0, 0]')`),
		in(`SELECT distance('[0, 0]', '[1, 0]')`),
		in(`SELECT distance('[1, 0]', 'oops')`),
		in(`SELECT distance(?, ?)`, c.Vector{1, 2, 3}, c.Vector{3, 2, 1}).close(),
		in(`SELECT vector('[1, 0.5, -2]'), vector('[0.1]')`),
		in(`SELECT vector('[]')`),
		in(`SELECT vector('oops')`),
		in(`SELECT typeof(vector('[1]')), length(vector('[1, 2]'))`),
		in(`SELECT key, vec FROM t_1 ORDER BY key`),
	)

	// Writes, each read back inside its own transaction and then undone.
	allDocs := `SELECT key, title, n, score FROM docs ORDER BY key`
	add(
		in(`INSERT INTO docs (key, title) VALUES ('docs:20', 'new')`).after(allDocs),
		in(`INSERT INTO docs (key, title, n) VALUES ('docs:21', 'a', 1), ('docs:22', 'b', 2)`).after(allDocs),
		in(`INSERT INTO docs (key, vec) VALUES ('docs:23', ?)`, c.Vector{1, 1, 1}).after(`SELECT key, vec FROM docs WHERE key = 'docs:23'`),
		in(`INSERT INTO docs (key, vec) VALUES ('docs:24', ?)`, c.Vector{1, 1}).with("the wrong size"),
		in(`INSERT INTO docs (key, vec) VALUES ('docs:25', ?)`, c.Vector{0, 0, 0}).with("all zeros"),
		in(`INSERT INTO docs (key) VALUES ('people:9')`).with("a key from another table"),
		in(`INSERT INTO docs (key) VALUES ('docs:1')`).with("a key that's taken"),
		in(`INSERT INTO docs (title) VALUES ('no key')`),
		in(`INSERT INTO docs (key) VALUES ('docs:')`),
		in(`INSERT INTO people (key, name, age) VALUES ('people:5', 'Yael', 45)`).after(`SELECT * FROM people ORDER BY key`),
		in(`INSERT INTO t_1 (key, x, y) VALUES (?, ?, ?)`, "t_1:z", int64(9), 0.25).after(`SELECT * FROM t_1 ORDER BY key`),
		in(`INSERT INTO docs (key, nothere) VALUES ('docs:26', 1)`).with("a field the table hasn't got"),
		in(`UPDATE docs SET n = n + 1 WHERE status = 'open'`).after(allDocs),
		in(`UPDATE docs SET title = upper(title) WHERE key = 'docs:2'`).after(allDocs),
		in(`UPDATE docs SET n = NULL WHERE n > 5`).after(allDocs),
		in(`UPDATE docs SET score = score * 2`).after(allDocs),
		in(`UPDATE docs SET vec = ? WHERE key = 'docs:1'`, c.Vector{0, 0, 1}).after(`SELECT key, vec FROM docs WHERE key = 'docs:1'`),
		in(`UPDATE docs SET vec = x'00000000' WHERE key = 'docs:1'`),
		in(`UPDATE docs SET vec = NULL WHERE key = 'docs:1'`).after(`SELECT count(vec) FROM docs`),
		in(`UPDATE docs SET key = 'docs:99' WHERE key = 'docs:1'`).with("a key never changes"),
		in(`UPDATE docs SET n = 0 WHERE key = 'docs:404'`),
		in(`UPDATE people SET age = age + 1 WHERE age IS NOT NULL`).after(`SELECT key, age FROM people ORDER BY key`),
		in(`UPDATE t_1 SET y = ? WHERE key = ?`, 0.0, "t_1:a").after(`SELECT key, y FROM t_1 ORDER BY key`),
		in(`UPDATE docs SET title = 'x', status = 'y' WHERE n = 3`).after(allDocs),
		in(`DELETE FROM docs WHERE key = 'docs:2'`).after(`SELECT value FROM json_each(walk('people:1', 1)) ORDER BY value`).
			with("the record's links go with it"),
		in(`DELETE FROM docs WHERE n > 5`).after(allDocs),
		in(`DELETE FROM docs`).after(`SELECT count(*), walk('people:1', 2) FROM docs`),
		in(`DELETE FROM people WHERE key = 'people:404'`),
		in(`DELETE FROM people WHERE age < ?`, int64(30)).after(`SELECT key FROM people ORDER BY key`),
		out(`INSERT INTO docs (key, title) SELECT 'docs:30', title FROM docs WHERE key = 'docs:1'`).after(allDocs).
			with("INSERT ... SELECT"),
		out(`INSERT OR REPLACE INTO docs (key, title) VALUES ('docs:1', 'replaced')`).after(allDocs).
			with("a conflict clause"),
		out(`INSERT INTO docs (key, n) VALUES ('docs:1', 9) ON CONFLICT (key) DO UPDATE SET n = excluded.n`).after(allDocs).
			with("an upsert"),
		out(`REPLACE INTO docs (key) VALUES ('docs:1')`).after(allDocs).with("REPLACE"),
		out(`UPDATE docs SET n = (SELECT max(n) FROM docs)`).after(allDocs).
			with("a subquery other than the one-record one"),
		out(`DELETE FROM docs WHERE key IN (SELECT key FROM docs WHERE n > 5)`).after(allDocs).
			with("IN over a subquery other than a walk"),
		out(`UPDATE docs SET n = 1 WHERE key = 'docs:1' RETURNING key`).with("RETURNING"),
	)

	// Outside the subset.
	add(
		out(`SELECT status, count(*) FROM docs GROUP BY status ORDER BY status`).with("GROUP BY"),
		out(`SELECT status, count(*) FROM docs GROUP BY status HAVING count(*) > 1 ORDER BY status`).
			with("GROUP BY and HAVING"),
		out(`SELECT d.key, p.name FROM docs d JOIN people p ON p.key = 'people:1' ORDER BY d.key`).
			with("a join of two tables"),
		out(`SELECT d.key FROM docs d LEFT JOIN people p ON p.name = d.title ORDER BY d.key`).with("a left join"),
		out(`SELECT d.key, p.key FROM docs d, people p WHERE d.n = p.age ORDER BY 1`).with("a join of two tables"),
		out(`SELECT key FROM docs NATURAL JOIN people`).with("a natural join"),
		out(`SELECT DISTINCT status FROM docs ORDER BY status`).with("DISTINCT"),
		out(`SELECT key, CASE WHEN n > 5 THEN 'big' ELSE 'small' END FROM docs ORDER BY key`).with("CASE"),
		out(`WITH x AS (SELECT key FROM docs) SELECT key FROM x ORDER BY key`).with("WITH"),
		out(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 3) SELECT i FROM c`).
			with("a recursive WITH; walks follow links"),
		out(`SELECT key, row_number() OVER (ORDER BY key) FROM docs ORDER BY key`).with("a window function"),
		out(`SELECT key FROM docs UNION SELECT key FROM people ORDER BY 1`).with("UNION"),
		out(`SELECT key FROM docs EXCEPT SELECT value FROM json_each(walk('people:1', 1)) ORDER BY 1`).with("EXCEPT"),
		out(`SELECT key FROM docs INTERSECT SELECT key FROM docs WHERE n > 2 ORDER BY 1`).with("INTERSECT"),
		out(`SELECT key FROM docs WHERE n > (SELECT avg(n) FROM docs) ORDER BY key`).
			with("a subquery other than the one-record one"),
		out(`SELECT key FROM docs d WHERE EXISTS (SELECT 1 FROM people p WHERE p.name = d.title) ORDER BY key`).
			with("EXISTS"),
		out(`SELECT key FROM docs WHERE key IN (SELECT key FROM people) ORDER BY key`).
			with("IN over a subquery other than a walk"),
		out(`SELECT (SELECT count(*) FROM people)`).with("a subquery other than the one-record one"),
		out(`SELECT * FROM (SELECT key FROM docs) ORDER BY key`).with("a subquery in FROM"),
		out(`SELECT date(joined, '+1 day') FROM people ORDER BY key`).with("date on a stored date"),
		out(`SELECT strftime('%Y', '2026-10-07')`).with("strftime()"),
		out(`SELECT json_extract(tags, '$[0]') FROM docs WHERE key = 'docs:4'`).with("JSON functions over fields"),
		out(`SELECT printf('%05d', n) FROM docs ORDER BY key`).with("printf()"),
		out(`SELECT key, n & 1, n | 2, n << 1, ~n FROM docs ORDER BY key`).with("bit operators"),
		out(`SELECT key FROM docs WHERE title GLOB 'Q*' ORDER BY key`).with("GLOB"),
		out(`SELECT key FROM docs ORDER BY key COLLATE NOCASE`).with("COLLATE"),
		out(`SELECT key FROM docs WHERE title = 'retro' COLLATE NOCASE`).with("COLLATE"),
		out(`SELECT key, title FROM docs ORDER BY title NULLS LAST, key`).with("NULLS LAST"),
		out(`SELECT sqlite_version() > '3'`).with("SQLite's own functions"),
		out(`SELECT name FROM sqlite_schema WHERE name = 'docs'`).with("SQLite's own tables"),
		out(`SELECT src, type, dst FROM hc_links ORDER BY src, type, dst`).with("0.x's own tables"),
		out(`SELECT key FROM docs WHERE rowid = 1`).with("rowid, which records haven't got"),
		out(`VALUES (1, 2)`).with("VALUES on its own"),
		out(`EXPLAIN SELECT key FROM docs`).with("EXPLAIN, whose answer depends on SQLite's version"),
		out(`PRAGMA table_info(docs)`).with("PRAGMA"),
		out(`CREATE TABLE extra (key TEXT PRIMARY KEY)`).after(`SELECT count(*) FROM sqlite_schema WHERE name = 'extra'`).
			with("CREATE TABLE; tables come from Put"),
		out(`DROP TABLE people`).with("DROP TABLE; Drop does it"),
		out(`ALTER TABLE docs ADD COLUMN extra`).with("ALTER TABLE; fields come from Put"),
		out(`CREATE INDEX docs_n ON docs (n)`).with("CREATE INDEX"),
		out(`VACUUM`).with("VACUUM; Compact does it"),
		out(`BEGIN`).with("BEGIN; transactions go through Update"),
		out(`SELECT 1; SELECT 2`).with("two statements"),
	)

	// Mistakes.
	add(
		in(`SELECT * FROM nothere`),
		in(`SELECT nothere FROM docs`),
		in(`SELEC 1`),
		in(`SELECT`),
		in(`SELECT 1 +`),
		in(`SELECT abs(1, 2)`),
		in(`SELECT unknownfunc(1)`),
		in(`SELECT key FROM docs ORDER BY 9`),
		in(`SELECT key FROM docs LIMIT 'x'`),
		in(`SELECT ?`).with("an argument that isn't there"),
		in(`SELECT ?, ?`, int64(1)),
		in(`SELECT key FROM docs WHERE`),
		in(`SELECT 'unterminated`),
		in(`SELECT key FROM docs ORDER BY`),
	)
	return s
}

// expr is a generated expression and its arguments.
type expr struct {
	sql  string
	args []difftest.Value
}

// exprGen makes random expressions from literals of every type, the
// operators, and the text and number functions beta/SQL.md takes, CAST and
// nullif among them. About a third of the operators go without parentheses,
// so the corpus holds SQLite's precedence too.
type exprGen struct {
	r    *rand.Rand
	args []difftest.Value
}

var (
	intLits  = []string{"0", "1", "-1", "2", "3", "7", "10", "42", "100", "1000000", "9223372036854775807", "-9223372036854775808", "9223372036854775808"}
	realLits = []string{"0.0", "-0.0", "0.5", "1.5", "-2.25", "3.0", "0.1", "0.2", "1e10", "1e308", "1.7976931348623157e308", "5e-324", "1e-7", "2.5e-3", "1e999"}
	textLits = []string{"''", "'a'", "'abc'", "'ABC'", "'Abc'", "'É'", "'é'", "'😀'", "'12'", "' 12 '", "'1e3'", "'0x10'", "'abc%'", "'a_c'", "'%'", "'_'", "'it''s'", "'-5'", "'3.0'", "'  pad  '", "'x y'"}
	blobLits = []string{"x''", "x'00'", "x'41'", "x'ff00'", "x'3132'"}
	binOps   = []string{"+", "-", "*", "/", "%", "=", "==", "!=", "<>", "<", "<=", ">", ">=", "AND", "OR", "||", "IS", "IS NOT"}
	funcs    = []struct {
		name     string
		min, max int
	}{
		{"abs", 1, 1}, {"length", 1, 1}, {"lower", 1, 1}, {"upper", 1, 1}, {"typeof", 1, 1},
		{"substr", 2, 3}, {"trim", 1, 2}, {"round", 1, 2}, {"coalesce", 2, 3}, {"ifnull", 2, 2},
		{"nullif", 2, 2}, {"instr", 2, 2}, {"replace", 3, 3}, {"min", 2, 3}, {"max", 2, 3},
	}
	castTypes = []string{"INTEGER", "REAL", "TEXT", "BLOB", "NUMERIC"}
)

func expressions(n int) []expr {
	g := &exprGen{r: rand.New(rand.NewPCG(4, 0x5143))}
	out := make([]expr, 0, n)
	seen := map[string]bool{}
	for len(out) < n {
		g.args = nil
		s := g.expr(1 + g.r.IntN(3))
		key := s + fmt.Sprint(g.args)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, expr{sql: s, args: g.args})
	}
	return out
}

func (g *exprGen) pick(list []string) string { return list[g.r.IntN(len(list))] }

func (g *exprGen) literal() string {
	switch g.r.IntN(14) {
	case 0, 1, 2:
		return g.pick(intLits)
	case 3, 4, 5:
		return g.pick(realLits)
	case 6, 7, 8:
		return g.pick(textLits)
	case 9:
		return g.pick(blobLits)
	case 10:
		return "NULL"
	case 11: // an argument instead of a literal
		vals := []any{int64(5), int64(-1), 2.5, 0.0, "abc", "12", []byte{0x41}, nil}
		g.args = append(g.args, v(vals[g.r.IntN(len(vals))]))
		return "?"
	}
	return g.pick(intLits)
}

func (g *exprGen) wrap(s string) string {
	if g.r.IntN(3) == 0 {
		return s
	}
	return "(" + s + ")"
}

func (g *exprGen) expr(depth int) string {
	if depth == 0 || g.r.IntN(5) == 0 {
		return g.literal()
	}
	sub := func() string { return g.expr(depth - 1) }
	switch g.r.IntN(12) {
	case 0, 1, 2:
		return g.wrap(sub() + " " + g.pick(binOps) + " " + sub())
	case 3:
		return g.wrap(g.pick([]string{"-", "+", "NOT "}) + sub())
	case 4:
		not := g.pick([]string{"", "NOT "})
		return g.wrap(sub() + " " + not + "BETWEEN " + sub() + " AND " + sub())
	case 5:
		not := g.pick([]string{"", "NOT "})
		items := make([]string, 1+g.r.IntN(3))
		for i := range items {
			items[i] = sub()
		}
		return g.wrap(sub() + " " + not + "IN (" + strings.Join(items, ", ") + ")")
	case 6:
		not := g.pick([]string{"", "NOT "})
		pattern := g.pick([]string{"'a%'", "'%c'", "'_b_'", "'%'", "'A%'", "'é%'", "'1%'", "'%.%'"})
		if g.r.IntN(3) == 0 {
			pattern = sub()
		}
		return g.wrap(sub() + " " + not + "LIKE " + pattern)
	case 7:
		return g.wrap(sub() + " IS " + g.pick([]string{"NULL", "NOT NULL"}))
	case 8:
		return "CAST(" + sub() + " AS " + g.pick(castTypes) + ")"
	}
	f := funcs[g.r.IntN(len(funcs))]
	args := make([]string, f.min+g.r.IntN(f.max-f.min+1))
	for i := range args {
		args[i] = sub()
	}
	return f.name + "(" + strings.Join(args, ", ") + ")"
}

// dates are date and datetime from fixed dates, plus or minus days, months
// and years, with the month ends and leap days that catch arithmetic out.
// The subset only has them on 'now'; these are for Q2's own arithmetic.
func dates() []Case {
	r := rand.New(rand.NewPCG(5, 0x4454))
	starts := []string{"2024-01-31", "2024-02-29", "2023-02-28", "2023-03-31", "2026-01-31", "2026-03-31", "2026-05-31",
		"2026-08-31", "2026-12-31", "2000-02-29", "1900-02-28", "2100-02-28", "1999-12-31 23:59:59",
		"2026-10-07 12:34:56", "2026-10-07", "0001-01-01", "9999-12-31", "2026-02-28 00:00:00"}
	units := []string{"days", "day", "months", "month", "years", "year"}
	amounts := map[string][]int{
		"day": {1, -1, 28, -29, 30, 31, 59, 60, 365, 366, -365, 1000, -400},
		"mon": {1, -1, 2, 3, 6, 11, 12, 13, 24, 25, -11, -12, -13, 120},
		"yea": {1, -1, 4, -4, 100, -100, 400, 8000},
	}
	modifier := func() string {
		u := units[r.IntN(len(units))]
		a := amounts[u[:3]][r.IntN(len(amounts[u[:3]]))]
		if u == "day" || u == "month" || u == "year" {
			a = []int{1, -1}[r.IntN(2)]
		}
		return fmt.Sprintf("'%+d %s'", a, u)
	}
	var out []Case
	seen := map[string]bool{}
	for len(out) < 600 {
		fn := []string{"date", "datetime"}[r.IntN(2)]
		parts := []string{"'" + starts[r.IntN(len(starts))] + "'"}
		for n := 1 + r.IntN(2); n > 0; n-- {
			parts = append(parts, modifier())
		}
		sql := "SELECT " + fn + "(" + strings.Join(parts, ", ") + ")"
		if seen[sql] {
			continue
		}
		seen[sql] = true
		out = append(out, Case{SQL: sql})
	}
	for _, s := range []string{`SELECT date('2026-10-07', '+1 fortnight')`, `SELECT date('not a date')`,
		`SELECT date('2026-02-30')`, `SELECT datetime('2026-10-07 25:00:00')`, `SELECT date('2026-10-07', '+1.5 days')`,
		`SELECT date('2026-10-07', '+ 1 day')`, `SELECT date('2026-10-07', '1 day')`, `SELECT date(NULL, '+1 day')`} {
		out = append(out, Case{SQL: s, Note: "an odd date or modifier"})
	}
	return out
}

// nowCases are in the subset. Their answers change with the date, so a
// replay compares them with a reference engine at the time.
func nowCases() []Case {
	var out []Case
	for _, s := range []string{
		`SELECT date('now')`, `SELECT datetime('now')`,
		`SELECT date('now', '+1 day')`, `SELECT date('now', '-1 day')`, `SELECT date('now', '+30 days')`,
		`SELECT date('now', '+1 month')`, `SELECT date('now', '-1 month')`, `SELECT date('now', '+13 months')`,
		`SELECT date('now', '+1 year')`, `SELECT date('now', '-4 years')`,
		`SELECT datetime('now', '+1 day', '-1 month')`, `SELECT datetime('now', '+2 years', '-3 days')`,
		`SELECT date('now') > '2026-01-01'`, `SELECT key FROM people WHERE joined <= date('now') ORDER BY key`,
	} {
		out = append(out, Case{SQL: s})
	}
	return out
}
