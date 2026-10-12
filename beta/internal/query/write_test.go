// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// Writes on the corpus's fixture, each with the answer 0.x gives, which
// write_sqlite_test.go holds them to: the order in which INSERT and UPDATE
// work out their values, which shows in which error comes first, the
// checks of each row, the counts, and the queries after.

// writeCase is a write on the fixture, run inside a transaction that's then
// rolled back: its answer, "changed" and the count of changed rows, or the
// error's kind, and the rows of a query run after it in the transaction, as
// answerText gives them. Where 0.x's answer differs from the Beta's, as
// SQL.md and Q6.md have it, zeroWant and zeroRows are 0.x's, and why says
// why.
type writeCase struct {
	sql      string
	args     []any
	want     string
	after    string
	rows     string
	zeroWant string
	zeroRows string
	why      string
}

var writeCases = []writeCase{
	// INSERT works out every row's values before the first row goes in: one
	// row's in the order of the table's columns, which in the fixture's docs
	// are key, n, score, status, title, vec, data and tags, and several
	// rows' row by row, each as written.
	{sql: "INSERT INTO docs (key, title, n) VALUES ('docs:30', abs(-9223372036854775808), vector('x'))", want: "invalid"},
	{sql: "INSERT INTO docs (title, key, n) VALUES (abs(-9223372036854775808), 'docs:30', vector('x'))", want: "invalid"},
	{sql: "INSERT INTO docs (key, title, n) VALUES ('docs:30', abs(-9223372036854775808), vector('x')), ('docs:31', 1, 2)", want: "error"},
	{sql: "INSERT INTO docs (n, key) VALUES (vector('x'), 'docs:30'), (abs(-9223372036854775808), 'docs:31')", want: "invalid"},
	{sql: "INSERT INTO docs (n, key) VALUES (abs(-9223372036854775808), 'docs:30'), (vector('x'), 'docs:31')", want: "error"},
	{sql: "INSERT INTO docs (key, n) VALUES ('docs:1', 1), ('docs:30', vector('x'))", want: "invalid"},
	{sql: "INSERT INTO docs (title) VALUES (vector('x'))", want: "invalid"},
	{sql: "INSERT INTO docs (title) VALUES ('a'), (vector('x'))", want: "invalid"},
	{sql: "INSERT INTO docs (title) VALUES ('no key')", want: "error"},
	{sql: "INSERT INTO docs (title, n) VALUES ('no key', 1), ('nor here', 2)", want: "error"},
	// Then each row: NULL, then UNIQUE, then 0.x's checks after INSERT.
	{sql: "INSERT INTO docs (key) VALUES (NULL)", want: "error"},
	{sql: "INSERT INTO docs (key) VALUES (?)", args: []any{nil}, want: "error"},
	{sql: "INSERT INTO docs (key) VALUES ('docs:30'), ('docs:30')", want: "error"},
	{sql: "INSERT INTO docs (key) VALUES ('people:30'), ('docs:1')", want: "invalid"},
	{sql: "INSERT INTO docs (key) VALUES ('docs:1'), ('people:30')", want: "error"},
	{sql: "INSERT INTO docs (key, vec) VALUES ('docs:1', x'00')", want: "error"},
	{sql: "INSERT INTO docs (key) VALUES ('people:1')", want: "invalid"},
	{sql: "INSERT INTO people (key, name) VALUES ('docs:30', 'x')", want: "invalid"},
	{sql: "INSERT INTO docs (key) VALUES (7)", want: "invalid"},
	{sql: "INSERT INTO docs (key) VALUES (x'646f63733a3330')", want: "invalid"},
	{sql: "INSERT INTO docs (key) VALUES ((SELECT vec FROM docs WHERE key = 'docs:1'))", want: "invalid"},
	{sql: "INSERT INTO docs (key, vec) VALUES ('docs:30', '[1, 2, 3]')", want: "invalid"},
	{sql: "INSERT INTO docs (key, vec) VALUES ('docs:30', x'0000803f0000803f0000803f00')", want: "invalid"},
	{sql: "INSERT INTO docs (key, vec) VALUES ('docs:30', 3)", want: "invalid"},
	{sql: "INSERT INTO docs (key, nothere) VALUES ('docs:30', vector('x'))", want: "error"},
	{sql: "INSERT INTO nothere (key) VALUES (vector('x'))", want: "error"},
	// distance(), vector() and walk() refusing their arguments are invalid
	// in a write.
	{sql: "INSERT INTO docs (key, n) VALUES ('docs:30', distance(x'00000000', x'0000000000000000'))", want: "invalid"},
	{sql: "INSERT INTO docs (key, n) VALUES ('docs:30', walk('people:1', 33))", want: "invalid"},
	{sql: "INSERT INTO docs (key, n) VALUES ('docs:30', abs(-9223372036854775808))", want: "error"},
	// What goes in.
	{sql: "INSERT INTO docs (key, vec) VALUES ('docs:30', x'0000803f0000803f0000803f')", want: "changed 1",
		after: "SELECT key, vec, title FROM docs WHERE key = 'docs:30'", rows: `text "docs:30", bytes 0000803f0000803f0000803f, null`},
	{sql: "INSERT INTO docs (key, vec) VALUES ('docs:30', vector('[0, 0.5, 2]'))", want: "changed 1",
		after: "SELECT vec FROM docs WHERE key = 'docs:30'", rows: `bytes 000000000000003f00000040`},
	{sql: "INSERT INTO docs (key, vec) VALUES ('docs:30', (SELECT vec FROM docs WHERE key = 'docs:5'))", want: "changed 1",
		after: "SELECT key FROM docs WHERE vec = (SELECT vec FROM docs WHERE key = 'docs:5') ORDER BY key", rows: lines(`text "docs:30"`, `text "docs:5"`)},
	{sql: "INSERT INTO docs (key, title) VALUES ('docs:30', (SELECT vec FROM docs WHERE key = 'docs:1'))", want: "changed 1",
		after: "SELECT typeof(title), length(title) FROM docs WHERE key = 'docs:30'", rows: `text "blob", int 12`},
	{sql: "INSERT INTO docs (key, n) VALUES ('docs:30', 1), ('docs:31', (SELECT n FROM docs WHERE key = 'docs:30'))", want: "changed 2",
		after: "SELECT key, n FROM docs WHERE key > 'docs:3' AND key < 'docs:4' ORDER BY key", rows: lines(`text "docs:30", int 1`, `text "docs:31", null`)},
	{sql: "INSERT INTO DOCS (KEY, Title, \"N\") VALUES (?, ?, ?), (?, NULL, ?)", args: []any{"docs:30", "x", 1.5, "docs:31", []byte{7}}, want: "changed 2",
		after: "SELECT * FROM docs WHERE key > 'docs:3' AND key < 'docs:4' ORDER BY key",
		rows:  lines(`text "docs:30", real 1.5 (3ff8000000000000), null, null, text "x", null, null, null`, `text "docs:31", bytes 07, null, null, null, null, null, null`)},
	{sql: "INSERT INTO t_1 (key, vec) VALUES ('t_1:x', x'0000803f00000000'), ('t_1:y', x'0000803f')", want: "invalid"},

	// UPDATE works out its SET values in the order of the table's columns,
	// key first.
	{sql: "UPDATE docs SET title = abs(-9223372036854775808), n = vector('x') WHERE key = 'docs:1'", want: "invalid"},
	{sql: "UPDATE docs SET n = abs(-9223372036854775808), key = 'zz' WHERE key = 'docs:1'", want: "error"},
	{sql: "UPDATE docs SET n = vector('x'), key = 'zz' WHERE key = 'docs:1'", want: "invalid"},
	// One pass: each record's values as the WHERE passes it. The WHERE
	// fails at docs:3, and the SET at docs:1 first.
	{sql: "UPDATE docs SET n = vector('x') WHERE abs(-9223372036854775808 + (n < 5))", want: "invalid"},
	{sql: "UPDATE docs SET n = vector('x') WHERE abs(-9223372036854775808 + (n < 5)) AND walk('people:1', 1) IS NOT NULL", want: "invalid"},
	{sql: "UPDATE docs SET n = vector('x') WHERE key = 'docs:1' OR abs(-9223372036854775808 + (n < 5))", want: "invalid"},
	// Two passes, when the SET names key or vec or the WHERE holds a
	// subquery: the WHERE over every record first.
	{sql: "UPDATE docs SET vec = vector('x') WHERE abs(-9223372036854775808 + (n < 5))", want: "error"},
	{sql: "UPDATE docs SET key = key, n = vector('x') WHERE abs(-9223372036854775808 + (n < 5))", want: "error"},
	{sql: "UPDATE docs SET n = vector('x') WHERE abs(-9223372036854775808 + (n < 5)) AND (SELECT key FROM docs WHERE key = 'docs:1') IS NOT NULL", want: "error"},
	{sql: "UPDATE docs SET n = vector('x') WHERE abs(-9223372036854775808 + (n < 5)) AND key NOT IN (SELECT value FROM json_each(walk('people:9', 1)))", want: "error"},
	// SQLite answers this OR from the key's index once for each branch, and
	// then takes two passes, so 0.x gives the WHERE's error at docs:3. The
	// Beta takes one pass, and gives the SET's at docs:1.
	{sql: "UPDATE docs SET n = vector('x') WHERE key = 'docs:1' OR key > 'docs:2' AND abs(-9223372036854775808 + (n < 5))", want: "invalid",
		zeroWant: "error", why: "SQLite reads the key's index for each branch of the OR, and takes two passes"},
	// The subquery folded away with x AND 0 leaves one pass.
	{sql: "UPDATE docs SET n = vector('x') WHERE abs(-9223372036854775808 + (n < 5)) OR (SELECT key FROM docs WHERE key = 'docs:1') AND 0", want: "invalid"},
	// In the second pass each record is checked and put before the next
	// one's values: vec refuses docs:1's integer before docs:5's abs()
	// fails.
	{sql: "UPDATE docs SET vec = coalesce(data, abs(-9223372036854775808 + (n < 5)))", want: "invalid"},
	{sql: "UPDATE docs SET vec = coalesce(data, vector('[1, 0, 0]')) WHERE n < 100", want: "invalid"},
	// A statement reads the data as it was before it changed anything.
	// 0.x's SQLite reads docs:1 as the UPDATE has changed it.
	{sql: "UPDATE docs SET n = coalesce(n * 2, (SELECT n FROM docs WHERE key = 'docs:1'))", want: "changed 8",
		after: "SELECT n FROM docs WHERE key IN ('docs:1', 'docs:4') ORDER BY key", rows: lines("int 6", "int 3"),
		zeroRows: lines("int 6", "int 6"), why: "0.x's subquery reads docs:1 as the UPDATE changed it"},
	{sql: "UPDATE docs SET n = coalesce(n * 2, (SELECT n FROM docs WHERE key = 'docs:1')), vec = vec", want: "changed 8",
		after: "SELECT n FROM docs WHERE key IN ('docs:1', 'docs:4') ORDER BY key", rows: lines("int 6", "int 3"),
		zeroRows: lines("int 6", "int 6"), why: "0.x's subquery reads docs:1 as the UPDATE changed it"},
	{sql: "UPDATE docs SET vec = coalesce((SELECT vec FROM docs WHERE key = 'docs:2'), vec), title = 'x' WHERE key >= 'docs:2'", want: "changed 7",
		after: "SELECT key FROM docs WHERE vec = x'cdcccc3d6666663fcdcccc3d' ORDER BY key", rows: lines(`text "docs:2"`, `text "docs:3"`, `text "docs:4"`, `text "docs:5"`, `text "docs:6"`, `text "docs:7"`, `text "docs:8"`)},
	{sql: "UPDATE docs SET title = n, n = title WHERE key IN ('docs:1', 'docs:8')", want: "changed 2",
		after: "SELECT key, title, n FROM docs WHERE key IN ('docs:1', 'docs:8') ORDER BY key", rows: lines(`text "docs:1", int 3, text "Q3 plan"`, `text "docs:8", text "5", text "12"`)},
	// A key never changes.
	{sql: "UPDATE docs SET key = key", want: "changed 8", after: "SELECT count(*) FROM docs", rows: "int 8"},
	{sql: "UPDATE docs SET key = 'docs:1' WHERE key = 'docs:1'", want: "changed 1"},
	{sql: "UPDATE docs SET key = NULL WHERE key = 'docs:1'", want: "invalid"},
	{sql: "UPDATE docs SET key = CAST(key AS BLOB) WHERE key = 'docs:1'", want: "invalid"},
	{sql: "UPDATE docs SET key = upper(key) WHERE key = 'docs:1'", want: "invalid"},
	{sql: "UPDATE docs SET key = 'docs:99' WHERE key = 'docs:404'", want: "changed 0"},
	// vec, and a vector anywhere else.
	{sql: "UPDATE docs SET title = vec WHERE key = 'docs:1'", want: "changed 1",
		after: "SELECT typeof(title), length(title) FROM docs WHERE key = 'docs:1'", rows: `text "blob", int 12`},
	{sql: "UPDATE docs SET vec = title WHERE key = 'docs:8'", want: "invalid"},
	{sql: "UPDATE docs SET vec = x'0000803f0000803f0000803f' WHERE key = 'docs:4'", want: "changed 1",
		after: "SELECT vec FROM docs WHERE key = 'docs:4'", rows: "bytes 0000803f0000803f0000803f"},
	{sql: "UPDATE docs SET vec = x'0000803f0000803f0000803f00' WHERE key = 'docs:4'", want: "invalid"},
	{sql: "UPDATE docs SET vec = NULL WHERE vec IS NOT NULL", want: "changed 6", after: "SELECT count(vec) FROM docs", rows: "int 0"},
	{sql: "UPDATE t_1 SET vec = x'0000803f' WHERE key = 't_1:a'", want: "invalid"},
	{sql: "UPDATE t_1 SET vec = (SELECT vec FROM docs WHERE key = 'docs:1') WHERE key = 't_1:é'", want: "invalid"},
	{sql: "UPDATE t_1 SET y = vec WHERE key = 't_1:é'", want: "changed 1", after: "SELECT y FROM t_1 WHERE key = 't_1:é'", rows: "null"},
	// Functions refusing their arguments.
	{sql: "UPDATE docs SET n = walk('people:1', 0)", want: "invalid"},
	{sql: "UPDATE docs SET n = 1 WHERE key IN (SELECT value FROM json_each(walk('people:1', 0)))", want: "invalid"},
	{sql: "UPDATE docs SET n = 1 WHERE distance(vec, '[1, 0]') > 0", want: "invalid"},
	{sql: "UPDATE docs SET n = 1 WHERE abs(-9223372036854775808)", want: "error"},
	{sql: "UPDATE docs SET n = 1 WHERE n LIKE 'a' ESCAPE 'ab'", want: "error"},
	// Counts: the records that matched.
	{sql: "UPDATE docs SET n = n + 1 WHERE key IN ('docs:3', 'docs:1')", want: "changed 2",
		after: "SELECT key, n FROM docs WHERE n > 3 ORDER BY key", rows: lines(`text "docs:1", int 4`, `text "docs:3", int 8`, `text "docs:5", int 10`, `text "docs:7", int 42`, `text "docs:8", text "5"`)},
	{sql: "UPDATE docs SET status = upper(status), n = -n WHERE status = 'open'", want: "changed 3",
		after: "SELECT key, status, n FROM docs WHERE status = 'OPEN' ORDER BY key",
		rows:  lines(`text "docs:1", text "OPEN", int -3`, `text "docs:3", text "OPEN", int -7`, `text "docs:5", text "OPEN", int -10`, `text "docs:7", text "OPEN", int 42`)},
	{sql: "UPDATE docs SET n = 1 WHERE n > 100", want: "changed 1", after: "SELECT key FROM docs WHERE n = 1 ORDER BY key", rows: lines(`text "docs:2"`, `text "docs:8"`)},
	{sql: "UPDATE docs SET n = date('now', '+1 day') WHERE key = 'docs:2'", want: "changed 1"},

	// DELETE reads every key its WHERE matches before it deletes any.
	{sql: "DELETE FROM docs WHERE walk('docs:2', 1) <> '[]'", want: "changed 8", after: "SELECT count(*) FROM docs", rows: "int 0"},
	{sql: "DELETE FROM docs WHERE key IN (SELECT value FROM json_each(walk('docs:2', 3)))", want: "changed 3",
		after: "SELECT key FROM docs ORDER BY key", rows: lines(`text "docs:2"`, `text "docs:4"`, `text "docs:6"`, `text "docs:7"`, `text "docs:8"`)},
	{sql: "DELETE FROM docs WHERE abs(-9223372036854775808 + (n < 5))", want: "error"},
	{sql: "DELETE FROM docs WHERE key IN (SELECT value FROM json_each(walk('people:1', 0)))", want: "invalid"},
	{sql: "DELETE FROM docs WHERE key = 'docs:1' AND n = 3", want: "changed 1", after: "SELECT count(*) FROM docs", rows: "int 7"},
	{sql: "DELETE FROM people WHERE key = 'people:1'", want: "changed 1",
		after: "SELECT value FROM json_each(walk('people:3', 2, NULL, 'both')) ORDER BY value", rows: lines(`text "docs:3"`, `text "docs:5"`, `text "docs:7"`)},
	{sql: "DELETE FROM nothere WHERE vector('x')", want: "error"},
	{sql: "DELETE FROM docs WHERE nothere = 1", want: "error"},
}

// writeResult is a write's answer as writeCase.want has it.
func writeResult(a *sqlcorpus.Answer) string {
	switch {
	case a.Error != "":
		return a.Error
	case a.Changed != nil:
		return fmt.Sprintf("changed %d", *a.Changed)
	}
	return "no count"
}

// runWriteCaseOn runs a write case on s, inside a transaction that's then
// rolled back, and gives its answer and the rows of its query after.
func runWriteCaseOn(t testing.TB, s *store.Store, wc writeCase) (string, string) {
	t.Helper()
	args := make([]value.Value, len(wc.args))
	for i, a := range wc.args {
		args[i] = argValue(a)
	}
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ans := execWrite(tx, wc.sql, args, corpusNow)
	rows := ""
	if ans.Error == "" && wc.after != "" {
		after := planQuery(tx, wc.after, nil, corpusNow)
		if after.Error != "" {
			rows = "error: " + after.Message
		} else {
			rows = answerText(after)
		}
	}
	return writeResult(ans), rows
}

// TestWriteCases runs writeCases on the fixture in a real store.
func TestWriteCases(t *testing.T) {
	s := fixtureStore(t)
	for _, wc := range writeCases {
		got, rows := runWriteCaseOn(t, s, wc)
		if got != wc.want || rows != wc.rows {
			t.Errorf("%s %v\n  gives %s, then %s\n  want  %s, then %s", wc.sql, wc.args, got, strings.ReplaceAll(rows, "\n", " / "),
				wc.want, strings.ReplaceAll(wc.rows, "\n", " / "))
		}
	}
	if got, want := storeText(t, s), storeText(t, fixtureStore(t)); got != want {
		t.Errorf("the store after the writes and their rollbacks:\n%s\nthe fixture:\n%s", got, want)
	}
}

// storeText is the whole store as text, its Snapshot a change a line, which
// shows each table with its fields in order and its vector size, every
// record and every link.
func storeText(t testing.TB, r store.Reader) string {
	t.Helper()
	var b strings.Builder
	for ch := range r.Snapshot() {
		b.WriteString(ch.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// execOn parses q and runs it with Exec inside tx, with args.
func execOn(tx *store.Tx, q string, args ...any) (int64, error) {
	st, err := Parse(q)
	if err != nil {
		return 0, err
	}
	vals := make([]value.Value, len(args))
	for i, a := range args {
		vals[i] = argValue(a)
	}
	return Exec(tx, st, &Frame{Args: vals, Now: corpusNow})
}

// commitChanges commits tx and gives its change list.
func commitChanges(t *testing.T, tx *store.Tx) []format.Change {
	t.Helper()
	var got []format.Change
	err := tx.Commit(func(changes []format.Change) error {
		got = slices.Clone(changes)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAFailedStatementTakesBackItsOwnChanges runs statements inside one
// transaction, some of which fail part of the way through, after putting
// records and setting a table's vector size. Each failed statement leaves
// the transaction as it was before it, and the transaction carries on,
// with the changes of the statements that went through in its change list
// and none of the others'.
func TestAFailedStatementTakesBackItsOwnChanges(t *testing.T) {
	s := store.New()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"notes:1", "notes:2", "notes:3"} {
		if err := tx.Put(key, []format.Field{{Name: "n", Value: value.Int(1)}, {Name: "vec", Value: value.Value{}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, st := range []struct{ sql, want string }{
		{"INSERT INTO notes (key, n) VALUES ('notes:4', 4)", "changed 1"},
		// Puts notes:5, then fails at its second row.
		{"INSERT INTO notes (key, n) VALUES ('notes:5', 5), ('notes:4', 6)", "error"},
		{"UPDATE notes SET n = n + 10 WHERE key <> 'notes:2'", "changed 3"},
		// Puts notes:1 and notes:2 with vectors of two values, the table's
		// first, so they set its size, then fails at notes:3, whose vector
		// has three.
		{"UPDATE notes SET vec = substr(x'0000803f0000803f0000803f', 1, 8 + 4 * (key = 'notes:3')) WHERE n < 100", "invalid"},
		// So the table's size is free again, for three values.
		{"UPDATE notes SET vec = x'0000803f0000803f0000803f' WHERE key = 'notes:3'", "changed 1"},
		// Fails in its WHERE at notes:4, before it deletes anything.
		{"DELETE FROM notes WHERE key = 'notes:1' OR abs(-9223372036854775807 - (key = 'notes:4'))", "error"},
		{"DELETE FROM notes WHERE key = 'notes:2'", "changed 1"},
		{"UPDATE notes SET n = vector('x')", "invalid"},
		{"UPDATE notes SET n = n + 1 WHERE key = 'notes:404'", "changed 0"},
	} {
		n, err := execOn(tx, st.sql)
		got := fmt.Sprintf("changed %d", n)
		if err != nil {
			got = writeKind(err)
		}
		if got != st.want {
			t.Fatalf("%s gives %s (%v), want %s", st.sql, got, err, st.want)
		}
	}
	want := lines(
		`create table notes, vector size 3, fields ["n" "vec"]`,
		"put notes:1 {n: int 11}",
		"put notes:3 {n: int 11, vec: vector [1 (3f800000), 1 (3f800000), 1 (3f800000)]}",
		"put notes:4 {n: int 14}",
		"",
	)
	if got := storeText(t, tx); got != want {
		t.Errorf("the transaction holds\n%s\nwant\n%s", got, want)
	}
	// The change list holds the statements that went through, after the
	// three puts and the table they created, and nothing of the others.
	changes := commitChanges(t, tx)
	var texts []string
	for _, ch := range changes[4:] {
		texts = append(texts, ch.String())
	}
	wantChanges := []string{
		"put notes:4 {n: int 4}",
		"put notes:1 {n: int 11}",
		"put notes:3 {n: int 11}",
		"put notes:4 {n: int 14}",
		"put notes:3 {vec: vector [1 (3f800000), 1 (3f800000), 1 (3f800000)]}",
		"delete notes:2",
	}
	if !slices.Equal(texts, wantChanges) {
		t.Errorf("the change list after the puts:\n%s\nwant\n%s", strings.Join(texts, "\n"), strings.Join(wantChanges, "\n"))
	}
}

// TestWritesOverManyRecordsGoInKeyOrder puts records in a random order,
// then updates and deletes many of them through SQL: the changes go in key
// order, so the batch doesn't depend on the order the records came in, and
// the counts are the records matched. An INSERT's rows go in as given.
func TestWritesOverManyRecordsGoInKeyOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(6, 0x5136))
	s := store.New()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := 0; i < 600; i++ {
		keys = append(keys, fmt.Sprintf("docs:%d", r.IntN(100000)))
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	shuffled := slices.Clone(keys)
	r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	for i, k := range shuffled {
		if err := tx.Put(k, []format.Field{{Name: "n", Value: value.Int(int64(i % 7))}}); err != nil {
			t.Fatal(err)
		}
	}
	commitChanges(t, tx)

	check := func(q string, op format.Op, want []string) {
		t.Helper()
		tx, err := s.Begin()
		if err != nil {
			t.Fatal(err)
		}
		n, err := execOn(tx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		changes := commitChanges(t, tx)
		var got []string
		for _, ch := range changes {
			if ch.Op != op {
				t.Fatalf("%s gives a change %v", q, ch)
			}
			got = append(got, ch.Key)
		}
		if int(n) != len(want) || !slices.Equal(got, want) {
			t.Errorf("%s changed %d records in this order:\n%v\nwant %d in key order:\n%v", q, n, got, len(want), want)
		}
	}
	var odd []string
	for _, k := range keys {
		if rec, err := s.Get(k); err == nil && rec.Field(0).Int()%2 == 1 {
			odd = append(odd, k)
		}
	}
	check("UPDATE docs SET n = n * 10 WHERE n % 2 = 1", format.Put, odd)
	check("UPDATE docs SET n = n + 1", format.Put, keys)
	var many []string
	for _, k := range keys {
		if rec, err := s.Get(k); err == nil && rec.Field(0).Int() > 20 {
			many = append(many, k)
		}
	}
	check("DELETE FROM docs WHERE n > 20", format.Delete, many)
	var list []string
	var in []string
	for i := len(keys) - 1; i >= 0; i -= 3 {
		if _, err := s.Get(keys[i]); err == nil {
			in = append(in, "'"+keys[i]+"'")
			list = append(list, keys[i])
		}
	}
	slices.Sort(list)
	check("DELETE FROM docs WHERE key IN ("+strings.Join(in, ", ")+")", format.Delete, list)

	var rows, given []string
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("docs:x%02d", (i*37)%50)
		rows = append(rows, "('"+k+"', "+fmt.Sprint(i)+")")
		given = append(given, k)
	}
	check("INSERT INTO docs (key, n) VALUES "+strings.Join(rows, ", "), format.Put, given)
}

// TestWriteMessages holds the three refusals whose messages 0.x's tests look
// for to 0.x's words, and the other errors to their kinds.
func TestWriteMessages(t *testing.T) {
	s := fixtureStore(t)
	for _, wc := range []struct{ sql, msg string }{
		{"INSERT INTO docs (key) VALUES ('people:9')", "hypercrux: invalid: keys in table docs are text that starts with docs:, up to 1024 bytes"},
		{"INSERT INTO t_1 (key) VALUES ('t_1:" + strings.Repeat("x", 1021) + "')", "hypercrux: invalid: keys in table t_1 are text that starts with t_1:, up to 1024 bytes"},
		{"UPDATE docs SET key = 'docs:99' WHERE key = 'docs:1'", "hypercrux: invalid: a key never changes; delete the record and put it again"},
		{"UPDATE docs SET vec = x'00000000' WHERE key = 'docs:1'", "hypercrux: invalid: vec must be a blob of float32 values, the same size in the whole table, and not all zero"},
		{"INSERT INTO docs (key) VALUES ('docs:1')", "UNIQUE constraint failed: docs.key"},
		{"INSERT INTO DOCS (title) VALUES ('no key')", "NOT NULL constraint failed: docs.key"},
		{"INSERT INTO docs (key, nothere) VALUES ('docs:26', 1)", "table docs has no column named nothere"},
		{"UPDATE docs SET nothere = 1", "no such column: nothere"},
		{"UPDATE nothere SET n = 1", "no such table: nothere"},
		{"UPDATE docs SET n = walk('people:1', 0)", "hypercrux: invalid: invalid: depth is from 1 to 32, not 0"},
		{"UPDATE docs SET n = 1 WHERE key = ?", "the statement has 1 ? marks and 0 arguments: it takes one argument for each"},
		{"SELECT 1", "Exec runs an INSERT, UPDATE or DELETE, and a SELECT goes through Query"},
	} {
		tx, err := s.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_, err = execOn(tx, wc.sql)
		tx.Rollback()
		if err == nil || err.Error() != wc.msg {
			t.Errorf("%s gives %v\n  want %s", wc.sql, err, wc.msg)
		}
	}
	// A transaction that has ended gives the store's error, as it is.
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if _, err := execOn(tx, "UPDATE docs SET n = 1"); !errors.Is(err, errs.ErrClosed) {
		t.Errorf("an UPDATE in a transaction that has ended gives %v, want ErrClosed", err)
	}
	if _, err := execOn(tx, "INSERT INTO docs (key) VALUES ('docs:30')"); !errors.Is(err, errs.ErrClosed) {
		t.Errorf("an INSERT in a transaction that has ended gives %v, want ErrClosed", err)
	}
}

// TestWritesLeaveOtherRecordsAlone checks that an UPDATE leaves the fields
// its SET doesn't name, and the records its WHERE doesn't match, as they
// were, and that a vector a write puts reads back with the same bits, -0
// apart from 0 included.
func TestWritesLeaveOtherRecordsAlone(t *testing.T) {
	s := fixtureStore(t)
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	before := storeText(t, tx)
	if _, err := execOn(tx, "UPDATE docs SET score = ? WHERE key = 'docs:3'", c.Vector{float32(math.Copysign(0, -1)), 1, float32(1e-45)}); err != nil {
		t.Fatal(err)
	}
	after := storeText(t, tx)
	var changed []string
	bl, al := strings.Split(before, "\n"), strings.Split(after, "\n")
	for i := range bl {
		if bl[i] != al[i] {
			changed = append(changed, al[i])
		}
	}
	if len(bl) != len(al) || len(changed) != 1 || !strings.HasPrefix(changed[0], "put docs:3 {data: bytes 000102, n: int 7, score: bytes 000000800000803f01000000, status") {
		t.Errorf("the UPDATE changed\n%s", strings.Join(changed, "\n"))
	}
	if _, err := execOn(tx, "UPDATE docs SET vec = ? WHERE key = 'docs:3'", c.Vector{float32(math.Copysign(0, -1)), 1, float32(1e-45)}); err != nil {
		t.Fatal(err)
	}
	got := planQuery(tx, "SELECT vec FROM docs WHERE key = 'docs:3'", nil, corpusNow)
	if want := "bytes 000000800000803f01000000"; answerText(got) != want {
		t.Errorf("the vector reads back as %s, want %s", answerText(got), want)
	}
}

// TestPlainSQLFollowsTheRulesThroughExec runs the statements of the named
// test PlainSQLFollowsTheRules through Exec, each in a transaction of its
// own that commits, as the public package's Exec will run them (G4): each
// broken rule is invalid, and what's allowed works and keeps the links in
// step.
func TestPlainSQLFollowsTheRulesThroughExec(t *testing.T) {
	s := store.New()
	run := func(q string, args ...any) error {
		t.Helper()
		tx, err := s.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := execOn(tx, q, args...); err != nil {
			return err
		}
		return tx.Commit(nil)
	}
	for _, key := range []string{"docs:1", "docs:2"} {
		fields := []format.Field{{Name: "title", Value: value.Text(key)}}
		if key == "docs:1" {
			fields = append(fields, format.Field{Name: "vec", Value: value.Vector([]float32{1, 0})})
		}
		if _, err := s.Put(nil, key, fields); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Link(nil, "docs:1", "next", "docs:2"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		stmt string
		args []any
	}{
		{`INSERT INTO docs (key, title) VALUES ('notes:1', 'wrong table')`, nil},
		{`INSERT INTO docs (key, title) VALUES ('docs:', 'no id')`, nil},
		{`INSERT INTO docs (key, title) VALUES (7, 'number')`, nil},
		{`UPDATE docs SET key = 'docs:9' WHERE key = 'docs:1'`, nil},
		{`UPDATE docs SET vec = ? WHERE key = 'docs:2'`, []any{[]byte{0, 0, 0x80, 0x3f}}},
		{`UPDATE docs SET vec = 'text' WHERE key = 'docs:2'`, nil},
		{`UPDATE docs SET vec = ? WHERE key = 'docs:2'`, []any{make([]byte, 8)}},
		{`INSERT INTO docs (key, vec) VALUES ('docs:3', ?)`, []any{[]byte{0, 0, 0x80, 0x3f, 0}}},
	} {
		if err := run(f.stmt, f.args...); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("%s: got %v, want one of HyperCrux's rules to refuse it", f.stmt, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO docs (key, title, vec) VALUES ('docs:3', 'c', vector('[0.5, 0.5]'))`,
		`UPDATE docs SET title = 'z' WHERE key = 'docs:2'`,
	} {
		if err := run(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := s.Link(nil, "docs:3", "next", "docs:1"); err != nil {
		t.Fatal(err)
	}
	if rec, err := s.Get("docs:2"); err != nil || rec.Field(0).Text() != "z" {
		t.Fatalf("docs:2 after the UPDATE: %v, %v", rec, err)
	}
	if err := run(`DELETE FROM docs WHERE key = 'docs:1'`); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"docs:2", "docs:3"} {
		if links, err := s.Neighbours(key, store.Both, ""); err != nil || len(links) != 0 {
			t.Fatalf("the DELETE left %s with the links %v, %v", key, links, err)
		}
	}
	if _, err := s.Get("docs:1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("docs:1 after the DELETE: %v", err)
	}
	if _, err := s.Link(nil, "docs:2", "next", "docs:3"); err != nil {
		t.Fatal(err)
	}
	if err := run(`DELETE FROM docs`); err != nil {
		t.Fatal(err)
	}
	if got := storeText(t, s); got != `create table docs, vector size 2, fields ["title" "vec"]`+"\n" {
		t.Errorf("after deleting every doc the store holds\n%s", got)
	}
}
