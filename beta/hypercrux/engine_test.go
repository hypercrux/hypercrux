// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	zx "github.com/hypercrux/hypercrux"
	c "github.com/hypercrux/hypercrux/beta/conformance"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
)

// The engine's tests (G4): SQL through the driver on the Beta's engine,
// against 0.x, every way into it, the statement's moment, vectors as
// arguments, Nearest's filter, and an Update whose unlock fails.

// sqlFixture is the data the engine's tests run on, put the same way on
// both engines, so each table's fields come in the same order.
var sqlFixture = []struct {
	key string
	f   goFields
}{
	{"people:1", goFields{"name": "Dana"}},
	{"people:2", goFields{"name": "Eli"}},
	{"docs:1", goFields{"title": "Q3 plan", "status": "open", "n": 3, "group": 1, "vec": vec{0.9, 0.1, 0}}},
	{"docs:2", goFields{"title": "Hiring notes", "status": "done", "n": 1, "group": 2, "vec": vec{0.1, 0.9, 0.1}}},
	{"docs:3", goFields{"title": "Q3 budget", "status": "open", "n": 7, "group": 1, "vec": vec{0.8, 0.2, 0.1}}},
	{"docs:4", goFields{"title": "Retro", "status": "open", "n": 10, "group": 2}},
	{"docs:5", goFields{"title": "Q3 risks", "status": "open", "n": 5, "group": 1, "vec": vec{0.95, 0.05, 0}}},
	{"docs:6", goFields{"title": "Same as 1", "status": "done", "n": 3, "group": 3, "vec": vec{0.9, 0.1, 0}}},
	{"plain:1", goFields{"a": 1}},
}

var sqlFixtureLinks = []struct{ from, typ, to string }{
	{"people:1", "owns", "docs:1"},
	{"people:1", "owns", "docs:2"},
	{"docs:1", "cites", "docs:3"},
	{"people:2", "owns", "docs:5"},
	{"docs:3", "cites", "docs:4"},
	{"docs:5", "cites", "docs:6"},
}

// openPair opens a database on each engine and puts sqlFixture in both.
func openPair(t *testing.T) (*zx.DB, *hc.DB) {
	t.Helper()
	dir := t.TempDir()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	ok(t, err)
	t.Cleanup(func() { z.Close() })
	b := open(t, filepath.Join(dir, "beta.hcx"))
	for _, h := range []handle{zeroOn{z}, betaOn{b}} {
		for _, r := range sqlFixture {
			ok(t, h.put(r.key, r.f))
		}
		for _, l := range sqlFixtureLinks {
			ok(t, h.link(l.from, l.typ, l.to))
		}
	}
	return z, b
}

// argsFor gives a statement's arguments for one engine, with vec, which
// stands for each package's Vector, made the engine's own.
func argsFor(beta bool, args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if v, isVec := a.(vec); isVec {
			if beta {
				a = hc.Vector(v)
			} else {
				a = zx.Vector(v)
			}
		}
		out[i] = a
	}
	return out
}

// outcome is what a statement gave, one way into an engine: its rows, or
// its count of changed rows, as text, or its error's kind and message.
type outcome struct {
	text, kind, msg string
}

func (o outcome) String() string {
	if o.kind != "ok" {
		return o.kind + ": " + o.msg
	}
	return "ok: " + o.text
}

func failedWith(err error) outcome { return outcome{kind: kindOf(err), msg: err.Error()} }

// rowsOf reads rows, with each value's type and bits, as the outcome of a
// Query.
func rowsOf(rows *sql.Rows, err error) outcome {
	if err != nil {
		return failedWith(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return failedWith(err)
	}
	lines := []string{strings.Join(cols, ", ")}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return failedWith(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = describeAny(v)
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	if err := rows.Err(); err != nil {
		return failedWith(err)
	}
	return outcome{text: strings.Join(lines, "; "), kind: "ok"}
}

// changedBy gives an Exec's count of changed rows as its outcome.
func changedBy(res sql.Result, err error) outcome {
	if err != nil {
		return failedWith(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return failedWith(err)
	}
	return outcome{text: fmt.Sprintf("changed %d", n), kind: "ok"}
}

// asSQL is a statement's outcome through a handle from SQL(), or a prepared
// statement on one, run with Query or with Exec.
func asSQL(h *sql.DB, prepared, query bool, text string, args []any) outcome {
	if prepared {
		st, err := h.Prepare(text)
		if err != nil {
			return failedWith(err)
		}
		defer st.Close()
		if query {
			return rowsOf(st.Query(args...))
		}
		return changedBy(st.Exec(args...))
	}
	if query {
		return rowsOf(h.Query(text, args...))
	}
	return changedBy(h.Exec(text, args...))
}

// sqlWays are the ways into the driver the statements test goes through, in
// the order it goes through them: a transaction that rolls back first, so
// that a write that changes the data does so through the ways that commit.
var sqlWays = []string{"tx.Exec", "tx.Query", "db.Exec", "db.Query", "SQL().Exec", "SQL().Query", "prepared Exec", "prepared Query"}

// through runs a statement on one engine one way, with zq or bq the
// engine's own, and gives its outcome.
func through(way string, z *zx.DB, b *hc.DB, text string, args []any) outcome {
	beta := b != nil
	args = argsFor(beta, args)
	switch way {
	case "tx.Exec", "tx.Query":
		var o outcome
		var err error
		if beta {
			err = b.Update(func(tx *hc.Tx) error {
				if way == "tx.Exec" {
					o = changedBy(tx.Exec(text, args...))
				} else {
					o = rowsOf(tx.Query(text, args...))
				}
				return errRollback
			})
		} else {
			err = z.Update(func(tx *zx.Tx) error {
				if way == "tx.Exec" {
					o = changedBy(tx.Exec(text, args...))
				} else {
					o = rowsOf(tx.Query(text, args...))
				}
				return errRollback
			})
		}
		if !errors.Is(err, errRollback) {
			return outcome{kind: "error", msg: fmt.Sprintf("the Update gave %v", err)}
		}
		return o
	case "db.Exec":
		if beta {
			return changedBy(b.Exec(text, args...))
		}
		return changedBy(z.Exec(text, args...))
	case "db.Query":
		if beta {
			return rowsOf(b.Query(text, args...))
		}
		return rowsOf(z.Query(text, args...))
	}
	var h *sql.DB
	if beta {
		h = b.SQL()
	} else {
		h = z.SQL()
	}
	return asSQL(h, strings.HasPrefix(way, "prepared"), strings.HasSuffix(way, "Query"), text, args)
}

// wantedKind is the kind of error the Beta must give one way in, from 0.x's
// outcome the same way, as SQL.md's "Errors and their kinds" sets out. The
// kind follows the statement through the methods, whichever of them runs
// it: a rule a write breaks, or a function's refusal in a write, wraps
// ErrInvalid, and a SELECT's errors wrap nothing. 0.x wraps an error in
// Exec alone, every one whose message starts "hypercrux: ", a SELECT's
// among them. Through SQL() every error is of kind "error", as in 0.x.
func wantedKind(way string, write bool, zero outcome) string {
	switch {
	case write && (way == "db.Query" || way == "tx.Query") && zero.kind == "error" && strings.HasPrefix(zero.msg, "hypercrux: "):
		return "invalid"
	case !write && (way == "db.Exec" || way == "tx.Exec") && zero.kind == "invalid":
		return "error"
	}
	return zero.kind
}

// ruleWords are the words of the three refusals the Beta keeps word for
// word from 0.x's triggers, since the command's tests look for them.
var ruleWords = []string{"keys in table docs are text that starts with docs:", "a key never changes", "vec must be a blob of float32 values"}

// sqlState is what an engine holds, as SQL and Neighbours read it: every
// table's records, with every field, and every fixture key's links.
func sqlState(query func(string, ...any) (*sql.Rows, error), h handle) string {
	var parts []string
	for _, tbl := range []string{"docs", "people", "plain"} {
		parts = append(parts, tbl+": "+rowsOf(query("SELECT * FROM "+tbl+" ORDER BY key")).String())
	}
	for _, r := range sqlFixture {
		links, err := h.neighbours(r.key, int(hc.Both), "")
		parts = append(parts, r.key+": "+describeErr(err)+" "+links)
	}
	return strings.Join(parts, "\n")
}

// TestStatementsGive0xsAnswersEveryWayIn runs SELECTs, INSERTs, UPDATEs and
// DELETEs on both engines through every way into the driver: Exec and
// Query on a database and on a transaction, and SQL()'s, with and without
// a prepared statement. Every way gives 0.x's rows, counts and errors' kinds,
// with the Beta's own kinds where SQL.md says the kind follows the
// statement, and the three refusals 0.x's triggers word read word for word.
// After each statement both engines hold the same data. The queries cover
// the planner's shapes, the crux query in 0.x's form, walk() as a function,
// the one-record subquery and vectors as arguments, and the writes cover
// the rules a write can break.
func TestStatementsGive0xsAnswersEveryWayIn(t *testing.T) {
	z, b := openPair(t)
	statements := []struct {
		sql  string
		args []any
	}{
		{"SELECT key, title, n FROM docs ORDER BY key", nil},
		{"SELECT key FROM docs WHERE status = ? ORDER BY n DESC", []any{"open"}},
		{"SELECT * FROM docs WHERE key = ?", []any{"docs:3"}},
		{"SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, ?) LIMIT 3", []any{vec{1, 0, 0}}},
		{"SELECT key FROM docs ORDER BY distance(vec, ?), key", []any{vec{0, 1, 0}}},
		{"SELECT key FROM docs WHERE vec IS NOT NULL ORDER BY distance(vec, '[0.9, 0.1, 0]') LIMIT 2", nil},
		{"SELECT walk('people:1', 2), walk('docs:4', 3, NULL, 'in')", nil},
		{"SELECT d.key, d.title FROM json_each(walk(?, ?)) w JOIN docs d ON d.key = w.value WHERE d.status = ? AND d.vec IS NOT NULL ORDER BY distance(d.vec, ?) LIMIT ?",
			[]any{"people:1", 2, "open", vec{1, 0, 0}, 10}},
		{"SELECT count(*), sum(n), min(title), max(n), avg(n) FROM docs", nil},
		{"SELECT key FROM docs WHERE key IN (SELECT value FROM json_each(walk('people:2', 3))) ORDER BY key", nil},
		{"SELECT title, (SELECT title FROM docs WHERE key = 'docs:1') AS firsttitle FROM docs WHERE key = ?", []any{"docs:2"}},
		{"SELECT typeof(vec), length(vec), distance(vec, ?) IS NOT NULL FROM docs WHERE key = 'docs:1'", []any{vec{0.5, 0.5, 0}}},
		{"SELECT vector('[1, 2]'), vec FROM docs WHERE key = 'docs:6'", nil},
		{"SELECT ?, ?, ?, ?, ?", []any{nil, int64(-7), 0.5, "text", []byte{0, 255}}},
		{"SELECT key FROM docs ORDER BY key LIMIT ? OFFSET ?", []any{2, 1}},
		{"SELECT nosuch FROM docs", nil},
		{"SELECT * FROM nosuch", nil},
		{"SELEC 1", nil},
		{"SELECT distance(vec, ?) FROM docs ORDER BY key", []any{vec{1, 0}}},
		{"SELECT walk('people:1', 0)", nil},
		{"SELECT abs(-9223372036854775808)", nil},
		{"SELECT key FROM docs ORDER BY key LIMIT ?", []any{"x"}},
		{"INSERT INTO docs (key, title, n, vec) VALUES (?, ?, ?, ?)", []any{"docs:9", "new", 9, vec{0.3, 0.3, 0.3}}},
		{"INSERT INTO docs (key, title) VALUES ('notes:1', 'x')", nil},
		{"INSERT INTO docs (key, vec) VALUES (?, ?)", []any{"docs:10", vec{1, 2}}},
		{"INSERT INTO docs (key) VALUES (7)", nil},
		{"UPDATE docs SET n = n + 1 WHERE status = ?", []any{"open"}},
		{"UPDATE docs SET key = 'docs:99' WHERE key = 'docs:2'", nil},
		{"UPDATE docs SET vec = 'text' WHERE key = 'docs:4'", nil},
		{"UPDATE docs SET vec = ? WHERE key = 'docs:4'", []any{make([]byte, 12)}},
		{"UPDATE docs SET vec = ? WHERE key = 'docs:4'", []any{vec{0.1, 0.2, 0.3}}},
		{"UPDATE docs SET title = walk('people:1', 1) WHERE key = 'docs:1'", nil},
		{"INSERT INTO docs (key, title, vec) VALUES ('docs:11', 'c', vector('[0.5, 0.5, 0.5]'))", nil},
		{"DELETE FROM docs WHERE key = ?", []any{"docs:3"}},
		{"DELETE FROM people WHERE key = 'people:2'", nil},
		{"SELECT d.key FROM walk('people:1', 3) w JOIN docs d ON d.key = w.key ORDER BY d.key", nil},
		{"DELETE FROM docs WHERE vec IS NULL", nil},
	}
	ways, checked := 0, 0
	for _, s := range statements {
		write := !strings.HasPrefix(s.sql, "SELECT") && !strings.HasPrefix(s.sql, "SELEC ")
		for _, way := range sqlWays {
			if strings.HasPrefix(s.sql, "SELECT d.key FROM walk(") && way != "tx.Query" && way != "db.Query" {
				continue // the Beta's walk as a table, which 0.x hasn't got
			}
			var zero outcome
			if strings.HasPrefix(s.sql, "SELECT d.key FROM walk(") {
				zero = through(way, z, nil, "SELECT d.key FROM json_each(walk('people:1', 3)) w JOIN docs d ON d.key = w.value ORDER BY d.key", nil)
			} else {
				zero = through(way, z, nil, s.sql, s.args)
			}
			beta := through(way, nil, b, s.sql, s.args)
			ways++
			want := wantedKind(way, write, zero)
			switch {
			case beta.kind != want:
				t.Errorf("%s, through %s: the Beta gives %v, of kind %s, where %s is wanted; 0.x gives %v", s.sql, way, beta, beta.kind, want, zero)
			case beta.kind == "ok" && !write && strings.HasSuffix(way, "Exec"):
				// After a SELECT, 0.x's count of changed rows is the last
				// write's, as SQLite leaves it, where the Beta's is 0.
			case beta.kind == "ok" && beta.text != zero.text:
				t.Errorf("%s, through %s: the Beta gives\n  %s\nand 0.x\n  %s", s.sql, way, beta.text, zero.text)
			}
			for _, words := range ruleWords {
				if beta.kind != "ok" && strings.Contains(zero.msg, words) {
					checked++
					got, wanted := beta.msg, zero.msg
					if strings.HasSuffix(way, "Query") && !strings.HasPrefix(way, "SQL") && !strings.HasPrefix(way, "prepared") {
						wanted = "hypercrux: invalid: " + strings.TrimPrefix(zero.msg, "hypercrux: ")
					}
					if got != wanted {
						t.Errorf("%s, through %s: the Beta says %q, where 0.x's words make %q", s.sql, way, got, wanted)
					}
				}
			}
		}
		zs := sqlState(z.Query, zeroOn{z})
		bs := sqlState(b.Query, betaOn{b})
		if zs != bs {
			t.Fatalf("after %s, the Beta holds\n%s\nand 0.x\n%s", s.sql, bs, zs)
		}
	}
	if checked < 30 {
		t.Errorf("the refusals' words were checked %d times", checked)
	}
	t.Logf("%d statements, %d ways in, each giving 0.x's answer, and the refusals' words checked %d times", len(statements), ways, checked)
}

// tickingClock gives a moment one second after the last each time it's
// read, and counts the reads.
type tickingClock struct {
	start time.Time
	reads atomic.Int64
}

func (c *tickingClock) now() time.Time {
	n := c.reads.Add(1)
	return c.start.Add(time.Duration(n) * time.Second)
}

// at is the text datetime('now') gives for the clock's nth read.
func (c *tickingClock) at(n int64) string {
	return c.start.Add(time.Duration(n) * time.Second).UTC().Format("2006-01-02 15:04:05")
}

// TestEachStatementReadsItsMomentOnce gives a database a clock that moves
// on a second each time it's read. A query reads it once, so every row has
// the same datetime('now'), and the next statement reads it again; an INSERT
// of several rows reads it once for all of them; each statement in an
// Update reads its own; and Nearest's filter reads it once for the call.
func TestEachStatementReadsItsMomentOnce(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "beta.hcx"))
	clock := &tickingClock{start: time.Date(2026, 10, 13, 23, 59, 58, 0, time.UTC)}
	hc.UseClock(db, clock.now)
	ok(t, db.Update(func(tx *hc.Tx) error {
		for i := range 40 {
			if err := tx.Put(fmt.Sprintf("docs:%02d", i), hc.Fields{"n": i, "vec": hc.Vector{1, float32(i)}}); err != nil {
				return err
			}
		}
		return nil
	}))
	moments := func(q string, args ...any) []string {
		t.Helper()
		rows, err := db.Query(q, args...)
		ok(t, err)
		defer rows.Close()
		var got []string
		for rows.Next() {
			var s string
			var n int
			ok(t, rows.Scan(&s, &n))
			got = append(got, s)
		}
		ok(t, rows.Err())
		return got
	}
	before := clock.reads.Load()
	first := moments("SELECT datetime('now'), n FROM docs ORDER BY key")
	second := moments("SELECT datetime('now'), n FROM docs ORDER BY n DESC")
	if len(first) != 40 || len(second) != 40 {
		t.Fatalf("the queries gave %d and %d rows", len(first), len(second))
	}
	for i := range first {
		if first[i] != clock.at(before+1) || second[i] != clock.at(before+2) {
			t.Fatalf("row %d gives %s, then %s, where each statement's moment is %s and then %s", i, first[i], second[i], clock.at(before+1), clock.at(before+2))
		}
	}
	if reads := clock.reads.Load() - before; reads != 2 {
		t.Errorf("two queries read the clock %d times", reads)
	}

	ok(t, db.Put("docs:00", hc.Fields{"at": nil})) // a field for the INSERT to name
	before = clock.reads.Load()
	res, err := db.Exec("INSERT INTO docs (key, n, at) VALUES ('docs:a', 1, datetime('now')), ('docs:b', 2, datetime('now', '+1 day'))")
	ok(t, err)
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("the INSERT changed %d rows", n)
	}
	a, err := db.Get("docs:a")
	ok(t, err)
	bf, err := db.Get("docs:b")
	ok(t, err)
	next := clock.start.Add(time.Duration(before+1) * time.Second).UTC()
	if a["at"] != next.Format("2006-01-02 15:04:05") || bf["at"] != next.AddDate(0, 0, 1).Format("2006-01-02 15:04:05") {
		t.Errorf("the INSERT's rows hold %v and %v, where its moment is %s", a["at"], bf["at"], next)
	}

	// Each statement inside an Update reads its own moment.
	before = clock.reads.Load()
	var inside []string
	ok(t, db.Update(func(tx *hc.Tx) error {
		for range 3 {
			var s string
			if err := tx.QueryRow("SELECT datetime('now')").Scan(&s); err != nil {
				return err
			}
			inside = append(inside, s)
		}
		_, err := tx.Exec("UPDATE docs SET at = datetime('now') WHERE key = 'docs:a'")
		return err
	}))
	for i, s := range inside {
		if s != clock.at(before+int64(i)+1) {
			t.Errorf("statement %d in the Update gives %s, where its moment is %s", i, s, clock.at(before+int64(i)+1))
		}
	}
	if a, _ := db.Get("docs:a"); a["at"] != clock.at(before+4) {
		t.Errorf("the UPDATE in the Update wrote %v, where its moment is %s", a["at"], clock.at(before+4))
	}

	// A search's filter reads it once for the call.
	before = clock.reads.Load()
	hits, err := db.Nearest("docs", hc.Vector{1, 1}, 3, "datetime('now') = ? AND n > ?", clock.at(before+1), 30)
	ok(t, err)
	if len(hits) != 3 || clock.reads.Load()-before != 1 {
		t.Errorf("a search whose filter holds this call's moment gives %v, reading the clock %d times", hits, clock.reads.Load()-before)
	}
	hits, err = db.Nearest("docs", hc.Vector{1, 1}, 3, "datetime('now') = ?", clock.at(before+1))
	if err != nil || hits == nil || len(hits) != 0 {
		t.Errorf("a search whose filter holds the last call's moment gives %v, %v, where it should find nothing", hits, err)
	}
}

// TestVectorArgumentsAreCopied writes vectors given as arguments, Vectors
// and []byte, through Exec on the database and on a transaction, and then
// changes the caller's vector. What the database holds, in memory and in
// the file, is what the statement was given. A vector a query gives out is
// a copy too: writing into the bytes a scan gave changes nothing.
func TestVectorArgumentsAreCopied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beta.hcx")
	db := open(t, path)
	ok(t, db.Put("docs:0", hc.Fields{"vec": hc.Vector{1, 1, 1}}))

	ok(t, db.Put("docs:0", hc.Fields{"data": nil, "more": nil})) // fields for the INSERT to name
	v := hc.Vector{1, 2, 3}
	raw := hc.Vector{4, 5, 6}.Bytes()
	_, err := db.Exec("INSERT INTO docs (key, vec, data, more) VALUES (?, ?, ?, ?)", "docs:1", v, v, raw)
	ok(t, err)
	v[0], raw[0] = 99, 99

	w := hc.Vector{7, 8, 9}
	ok(t, db.Update(func(tx *hc.Tx) error {
		if _, err := tx.Exec("UPDATE docs SET vec = ?, data = ? WHERE key = 'docs:0'", w, w); err != nil {
			return err
		}
		w[1] = 77 // before the commit writes the batch
		return nil
	}))

	want := map[string]string{
		"docs:0": show(hc.Fields{"vec": hc.Vector{7, 8, 9}, "data": hc.Vector{7, 8, 9}.Bytes()}),
		"docs:1": show(hc.Fields{"vec": hc.Vector{1, 2, 3}, "data": hc.Vector{1, 2, 3}.Bytes(), "more": hc.Vector{4, 5, 6}.Bytes()}),
	}
	check := func(db *hc.DB, when string) {
		t.Helper()
		for key, w := range want {
			f, err := db.Get(key)
			ok(t, err)
			if got := show(f); got != w {
				t.Errorf("%s, %s holds %s, where the statement was given %s", when, key, got, w)
			}
		}
	}
	check(db, "after the caller changed its vectors")

	// The bytes a query hands out are the caller's own.
	rows, err := db.Query("SELECT vec, data FROM docs ORDER BY key")
	ok(t, err)
	for rows.Next() {
		var a, b sql.RawBytes
		ok(t, rows.Scan(&a, &b))
		for i := range a {
			a[i] = 0xee
		}
		for i := range b {
			b[i] = 0xee
		}
	}
	ok(t, rows.Err())
	rows.Close()
	check(db, "after a scan's bytes were written into")
	var again []byte
	ok(t, db.QueryRow("SELECT vec FROM docs WHERE key = 'docs:1'").Scan(&again))
	if string(again) != string(hc.Vector{1, 2, 3}.Bytes()) {
		t.Errorf("a query after the scan gives %x", again)
	}

	ok(t, db.Close())
	db = open(t, path)
	check(db, "after a reopen")
}

// sameSearch fails the test unless the Beta's search gave what 0.x's did:
// the same hits, within conformance.DistanceBound where two distances fall
// within it, nil where 0.x gives nil, or an error of the same kind.
func sameSearch(t *testing.T, what string, bHits, zHits []c.Hit, bErr, zErr error) {
	t.Helper()
	switch {
	case kindOf(bErr) != kindOf(zErr):
		t.Errorf("%s: the Beta gives %v, of kind %s, and 0.x %v, of kind %s", what, bErr, kindOf(bErr), zErr, kindOf(zErr))
	case (bHits == nil) != (zHits == nil):
		t.Errorf("%s: the Beta gives %v, nil %v, and 0.x %v, nil %v", what, bHits, bHits == nil, zHits, zHits == nil)
	default:
		if err := c.CompareHits(bHits, zHits); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// TestNearestFiltersAs0x searches with filters on both engines: conditions
// on fields, quoted names, the one-record subquery, IN over a walk, terms
// that rule every record out at once, which give an empty list, and errors
// of kind "error" from the filter, after 0.x's own checks in 0.x's order.
// A broken filter on a table with no vectors gives nothing, as in 0.x, and
// on a table that doesn't exist, or with a bad k or query, gives that
// error. The arguments go through database/sql's conversion, filter or no
// filter, so a channel among them is refused in database/sql's words, and
// they aren't used without a filter. A search inside an Update sees its
// changes.
func TestNearestFiltersAs0x(t *testing.T) {
	z, b := openPair(t)
	q := []float32{1, 0, 0}
	type search struct {
		table string
		q     []float32
		k     int
		where string
		args  []any
	}
	searches := []search{
		{"docs", q, 10, "", nil},
		{"docs", q, 10, " \t\n", nil},
		{"docs", q, 10, "status = ?", []any{"open"}},
		{"docs", q, 3, `"group" = ?`, []any{1}},
		{"docs", q, 10, "status = 'open' AND n > 3", nil},
		{"docs", q, 10, "n > (SELECT n FROM docs WHERE key = 'docs:1')", nil},
		{"docs", q, 10, "key IN (SELECT value FROM json_each(walk('people:1', 2)))", nil},
		{"docs", q, 10, "key NOT IN (SELECT value FROM json_each(walk(?, 3)))", []any{"people:2"}},
		{"docs", q, 10, "title LIKE 'q3%'", nil},
		{"docs", q, 10, "key = 'docs:2' OR key = 'docs:6'", nil},
		{"docs", q, 10, "0", nil},
		{"docs", q, 10, "1 = 2 AND n > 0", nil},
		{"docs", q, 10, "? IS NULL", []any{1}},
		{"docs", q, 2, "n BETWEEN ? AND ?", []any{2, 6}},
		{"docs", q, 10, "abs(-9223372036854775808)", nil},
		{"docs", q, 10, "abs(-9223372036854775807 - (key = 'docs:5'))", nil},
		{"docs", q, 10, "nosuch = 1", nil},
		{"docs", q, 10, "status = ?", nil},
		{"docs", q, 10, "status = 'open' AND", nil},
		{"docs", q, 10, "count(*) > 1", nil},
		{"docs", q, 10, "a\x00b", nil},
		{"docs", q, 10, "walk('docs:1', 0) <> ''", nil},
		{"plain", []float32{1}, 10, "nosuch = 1", nil},
		{"plain", []float32{1}, 10, "status = 'open' AND", []any{make(chan int)}},
		{"nosuch", q, 10, "status = 'open' AND", nil},
		{"Docs", q, 10, "status = 'open' AND", nil},
		{"docs", q, 0, "status = 'open' AND", nil},
		{"docs", []float32{1, 0}, 10, "status = 'open' AND", nil},
		{"docs", []float32{0, 0, 0}, 10, "nosuch = 1", nil},
		{"docs", q, 10, "", []any{1, "open"}},
		{"docs", q, 10, "", []any{make(chan int)}},
		{"docs", q, 10, "status = ?", []any{"open", func() {}}},
		{"docs", q, 10, "status = ?", []any{struct{}{}}},
	}
	for _, s := range searches {
		what := fmt.Sprintf("Nearest(%q, %v, %d, %q, %v)", s.table, s.q, s.k, s.where, s.args)
		zHits, zErr := zeroOn{z}.nearest(s.table, s.q, s.k, s.where, s.args...)
		bHits, bErr := betaOn{b}.nearest(s.table, s.q, s.k, s.where, s.args...)
		sameSearch(t, what, bHits, zHits, bErr, zErr)
		if zErr != nil && strings.HasPrefix(zErr.Error(), "sql: converting argument") && (bErr == nil || bErr.Error() != zErr.Error()) {
			t.Errorf("%s: the Beta says %v, where 0.x says %v in database/sql's words", what, bErr, zErr)
		}
	}

	// More arguments than the filter's ? marks, or one with a name, fill no
	// mark: the Beta refuses them, where 0.x ignores the one and leaves the
	// mark NULL for the other (SQL.md, "Where the Beta differs from 0.x").
	for _, args := range [][]any{{"open", 2}, {sql.Named("s", "open")}} {
		if hits, err := b.Nearest("docs", hc.Vector(q), 10, "status = ?", args...); err == nil || kindOf(err) != "error" {
			t.Errorf("a filter of one ? mark given %v gives %v, %v", args, hits, err)
		}
		if _, err := z.Nearest("docs", zx.Vector(q), 10, "status = ?", args...); err != nil {
			t.Errorf("0.x refuses a filter of one ? mark given %v: %v, so the Beta's difference may have gone", args, err)
		}
	}

	// Inside an Update, the search sees the Update's changes.
	for _, h := range []struct {
		name   string
		update func(fn func(h handle) error) error
	}{
		{"0.x", func(fn func(h handle) error) error {
			return z.Update(func(tx *zx.Tx) error { return fn(zeroOn{tx}) })
		}},
		{"the Beta", func(fn func(h handle) error) error {
			return b.Update(func(tx *hc.Tx) error { return fn(betaOn{tx}) })
		}},
	} {
		var hits []c.Hit
		err := h.update(func(tx handle) error {
			if err := tx.put("docs:7", goFields{"status": "open", "n": 99, "vec": vec{1, 0, 0}}); err != nil {
				return err
			}
			var err error
			hits, err = tx.nearest("docs", q, 2, "n > ?", 50)
			if err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) || len(hits) != 1 || hits[0].Key != "docs:7" {
			t.Errorf("on %s, a search inside the Update gives %v, and the Update %v", h.name, hits, err)
		}
	}
}

// TestAFailedUnlockAfterACommitFailsNoUpdate makes letting go of flock fail
// once, after a commit is in the file. Update returns nil, so a caller
// never makes the commit twice, and the commit is in the copy and in the
// file. flock stays held, as the fault layer has it, so another handle
// can't write until this one's next Update takes it again and lets go.
// When the Update's function fails as well, its error is what comes back.
func TestAFailedUnlockAfterACommitFailsNoUpdate(t *testing.T) {
	d := fault.New(1)
	path := "/data/db.hcx"
	db := openFault(t, d, path)
	d.Add(fault.Rule{Op: fault.Unlock, Path: path, N: 1})
	if err := db.Put("docs:1", hc.Fields{"n": 1}); err != nil {
		t.Fatalf("a commit whose unlock failed gives %v", err)
	}
	if d.Failed() != 1 {
		t.Fatalf("the fault layer failed %d calls", d.Failed())
	}
	if f, err := db.Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Fatalf("after the commit, docs:1 gives %v, %v", f, err)
	}

	other, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: 100 * time.Millisecond})
	ok(t, err)
	defer other.Close()
	if err := other.Put("docs:2", hc.Fields{"n": 2}); !errors.Is(err, hc.ErrLockTimeout) {
		t.Fatalf("another handle's Put while flock stays held gives %v", err)
	}
	ok(t, db.Put("docs:3", hc.Fields{"n": 3}))
	ok(t, other.Put("docs:2", hc.Fields{"n": 2}))

	d.Add(fault.Rule{Op: fault.Unlock, Path: path, N: 1})
	boom := errors.New("boom")
	if err := db.Update(func(tx *hc.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("an Update that fails, and whose unlock fails, gives %v", err)
	}
	ok(t, db.Put("docs:4", hc.Fields{"n": 4}))

	ok(t, db.Close())
	ok(t, other.Close())
	again, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second})
	ok(t, err)
	defer again.Close()
	recs, err := again.Scan("docs:", "", 0)
	ok(t, err)
	var got []string
	for _, r := range recs {
		got = append(got, fmt.Sprintf("%s %v", r.Key, r.Fields["n"]))
	}
	if strings.Join(got, ", ") != "docs:1 1, docs:2 2, docs:3 3, docs:4 4" {
		t.Errorf("after a reopen the file holds %q", got)
	}
}
