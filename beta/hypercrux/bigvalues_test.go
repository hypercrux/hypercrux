// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/conformance/betax"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/internal/export"
)

// L1: text and bytes stop at SQLite's 1,000,000,000 bytes, as in 0.x,
// wherever a value comes in: Put, SQL's writes and the arguments of a
// statement or of Nearest's filter, a statement's own text, and Import. A
// value one byte over the limit is refused with 0.x's error and its kind,
// before anything changes.
//
// The values are a gigabyte long without taking a gigabyte: huge maps them
// from the kernel's page of zeros, which costs nothing to read, and neither
// engine copies a value it refuses before it refuses it, which the tests
// check by the process's memory. Neither engine can take a value at the
// limit into a record without copying it, so the store's limit is held to
// its edge on lengths (rules.TestLength) and on values the store only reads
// (store.TestTheLimitOnTextAndBytes). An argument the Beta only reads goes
// right up to the limit here. 0.x copies each argument it binds, so a value
// at the limit is the Beta's alone here, and so are a statement too long and
// an import, which take 0.x a gigabyte or three: L1.md has what 0.x gave
// for those, once, by hand.

// maxLength is SQLite's limit, rules.MaxLength.
const maxLength = 1000000000

// huge returns n bytes, head followed by zero bytes and then tail, in memory
// the kernel maps from its page of zeros, which costs nothing to read. Only
// the pages head and tail fall on take memory, and the mapping is read only.
func huge(t testing.TB, n int, head, tail string) string {
	t.Helper()
	prot := syscall.PROT_READ
	if head != "" || tail != "" {
		prot |= syscall.PROT_WRITE
	}
	b, err := syscall.Mmap(-1, 0, n, prot, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		t.Fatalf("mapping %d bytes: %v", n, err)
	}
	t.Cleanup(func() { syscall.Munmap(b) })
	syscall.Madvise(b, 14) // MADV_HUGEPAGE: the zeros in pages of 2 MB, read with fewer faults
	if prot&syscall.PROT_WRITE != 0 {
		copy(b, head)
		copy(b[n-len(tail):], tail)
		if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
			t.Fatal(err)
		}
	}
	return unsafe.String(&b[0], n)
}

// memoryPeak resets the process's high-water mark of memory, which it can
// on Linux since 4.0, and gives a func that says how far it has risen since,
// in bytes. Where the reset isn't allowed, the rise is from the mark as it
// was, which is all the more lenient.
func memoryPeak(t *testing.T) func() int {
	t.Helper()
	hwm := func() int {
		b, err := os.ReadFile("/proc/self/status")
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if rest, found := strings.CutPrefix(line, "VmHWM:"); found {
				kb, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")))
				if err != nil {
					t.Fatalf("reading %q: %v", line, err)
				}
				return kb << 10
			}
		}
		t.Fatal("/proc/self/status gives no VmHWM")
		return 0
	}
	os.WriteFile("/proc/self/clear_refs", []byte("5"), 0)
	from := hwm()
	return func() int { return hwm() - from }
}

// smallPeak fails the test when memory rose by anything like a value's
// worth since memoryPeak, which a copy of one would take.
func smallPeak(t *testing.T, rise func() int) {
	t.Helper()
	if n := rise(); n > 256<<20 {
		t.Errorf("memory rose by %d MB, as if a value of a gigabyte was copied", n>>20)
	}
}

// fixture is what the tests run on, put the same way on both engines.
func fixture(t *testing.T, db c.DB) {
	t.Helper()
	ok(t, db.Put("docs:1", c.Fields{"t": "x", "n": 1, "vec": c.Vector{1, 2}}))
	ok(t, db.Put("plain:1", c.Fields{"t": "x"}))
}

// TestTheLimitOnTextAndBytesAs0xHasIt makes each call that hands an engine
// a value one byte over the limit on 0.x and on the Beta, and wants the
// same kind of error, with the same message, and both databases as they
// were. Where a value breaks another rule as well, 0.x's order decides
// which error comes: Put's other checks come first, as 0.x makes them
// before it binds the values, and the bind comes before SQL's own checks of
// a row.
func TestTheLimitOnTextAndBytesAs0xHasIt(t *testing.T) {
	over := huge(t, maxLength+1, "", "")
	rise := memoryPeak(t)
	var n int64
	ways := []struct {
		what string
		call func(db c.DB) error
	}{
		{"Put of text", func(db c.DB) error { return db.Put("docs:9", c.Fields{"t": over}) }},
		{"Put into a table that isn't there", func(db c.DB) error { return db.Put("notes:1", c.Fields{"t": over, "n": 1}) }},
		{"Put of text over a record's", func(db c.DB) error { return db.Put("docs:1", c.Fields{"t": over}) }},
		{"Put with an infinite number too", func(db c.DB) error { return db.Put("docs:9", c.Fields{"a": over, "b": math.Inf(1)}) }},
		{"Put with a vector of another size too", func(db c.DB) error {
			return db.Put("docs:9", c.Fields{"t": over, "vec": c.Vector{1, 2, 3}})
		}},
		{"Put with a key that breaks the rules too", func(db c.DB) error { return db.Put("Docs:9", c.Fields{"t": over}) }},
		{"INSERT", func(db c.DB) error {
			_, err := db.Exec("INSERT INTO docs (key, t) VALUES (?, ?)", "docs:9", over)
			return err
		}},
		{"INSERT of a key that doesn't fit", func(db c.DB) error {
			_, err := db.Exec("INSERT INTO docs (key, t) VALUES (?, ?)", "plain:9", over)
			return err
		}},
		{"INSERT of a key that's there", func(db c.DB) error {
			_, err := db.Exec("INSERT INTO docs (key, t) VALUES (?, ?)", "docs:1", over)
			return err
		}},
		{"UPDATE", func(db c.DB) error {
			_, err := db.Exec("UPDATE docs SET t = ? WHERE key = 'docs:1'", over)
			return err
		}},
		{"UPDATE of no record", func(db c.DB) error {
			_, err := db.Exec("UPDATE docs SET t = ? WHERE key = 'docs:404'", over)
			return err
		}},
		{"Exec of a SELECT", func(db c.DB) error {
			_, err := db.Exec("SELECT ?", over)
			return err
		}},
		{"Query", func(db c.DB) error {
			rows, err := db.Query("SELECT length(?)", over)
			if rows != nil {
				rows.Close()
			}
			return err
		}},
		{"Query whose WHERE rules every row out", func(db c.DB) error {
			rows, err := db.Query("SELECT key FROM docs WHERE 0 AND ? IS NULL", over)
			if rows != nil {
				rows.Close()
			}
			return err
		}},
		{"QueryRow", func(db c.DB) error { return db.QueryRow("SELECT length(?)", over).Scan(&n) }},
		{"SQL().Exec", func(db c.DB) error {
			_, err := db.SQL().Exec("INSERT INTO docs (key, t) VALUES (?, ?)", "docs:9", over)
			return err
		}},
		{"a Put inside Update", func(db c.DB) error {
			return db.Update(func(tx c.Handle) error { return tx.Put("docs:9", c.Fields{"t": over}) })
		}},
		{"an Exec inside Update", func(db c.DB) error {
			return db.Update(func(tx c.Handle) error {
				_, err := tx.Exec("UPDATE docs SET t = ?", over)
				return err
			})
		}},
		{"a Query inside Update", func(db c.DB) error {
			return db.Update(func(tx c.Handle) error {
				rows, err := tx.Query("SELECT ?", over)
				if rows != nil {
					rows.Close()
				}
				return err
			})
		}},
		{"Nearest through a filter", func(db c.DB) error {
			_, err := db.Nearest("docs", c.Vector{1, 2}, 3, "t = ?", over)
			return err
		}},
		{"Nearest through a filter that rules every record out", func(db c.DB) error {
			_, err := db.Nearest("docs", c.Vector{1, 2}, 3, "0 AND t = ?", over)
			return err
		}},
		{"Nearest without a filter, which leaves the argument out", func(db c.DB) error {
			hits, err := db.Nearest("docs", c.Vector{1, 2}, 3, "", over)
			if err == nil && (len(hits) != 1 || hits[0].Key != "docs:1") {
				t.Errorf("Nearest without a filter found %v", hits)
			}
			return err
		}},
		{"Nearest in a table with no vectors yet", func(db c.DB) error {
			hits, err := db.Nearest("plain", c.Vector{1, 2}, 3, "t = ?", over)
			if hits != nil {
				t.Errorf("Nearest in a table with no vectors found %v", hits)
			}
			return err
		}},
	}
	got := map[string][]string{}
	for _, e := range []c.Engine{zerox.Engine{}, betax.Engine{}} {
		db, err := e.Open(filepath.Join(t.TempDir(), "db"))
		ok(t, err)
		defer db.Close()
		fixture(t, db)
		before := string(exportOf(t, db))
		for _, w := range ways {
			err := w.call(db)
			o := kindOf(err)
			if err != nil {
				o += ": " + err.Error()
			}
			got[w.what] = append(got[w.what], o)
		}
		if after := string(exportOf(t, db)); after != before {
			t.Errorf("%s: the calls changed the database:\n%s\nwas\n%s", e.Name(), after, before)
		}
	}
	for _, w := range ways {
		zero, beta := got[w.what][0], got[w.what][1]
		switch {
		case zero != beta:
			t.Errorf("%s: 0.x gives %s, and the Beta %s", w.what, zero, beta)
		case strings.HasPrefix(w.what, "Nearest without") || strings.HasPrefix(w.what, "Nearest in"):
			if zero != "ok" {
				t.Errorf("%s: %s", w.what, zero)
			}
		case strings.Contains(w.what, " too"):
			if !strings.HasPrefix(zero, "invalid: ") {
				t.Errorf("%s: %s, where the other rule's error comes first", w.what, zero)
			}
		case zero != "error: string or blob too big":
			t.Errorf("%s: %s, where SQLite refuses the value", w.what, zero)
		}
	}
	smallPeak(t, rise)
}

// TestAFailedPutOfAValueTooLong: inside an Update that goes on to commit, a
// Put of a value too long leaves the table as it was in the Beta, as every
// write that fails does. 0.x keeps the table, if the Put made it, and the
// new fields it named, since it adds them before SQLite refuses the value,
// as it does for a vector of the wrong size (SQL.md, "Where the Beta
// differs from 0.x").
func TestAFailedPutOfAValueTooLong(t *testing.T) {
	over := huge(t, maxLength+1, "", "")
	fields := map[string]string{}
	for _, e := range []c.Engine{zerox.Engine{}, betax.Engine{}} {
		db, err := e.Open(filepath.Join(t.TempDir(), "db"))
		ok(t, err)
		defer db.Close()
		ok(t, db.Update(func(tx c.Handle) error {
			if err := tx.Put("notes:1", c.Fields{"zz": over}); kindOf(err) != "error" {
				t.Errorf("%s: the Put gave %v", e.Name(), err)
			}
			return tx.Put("notes:2", c.Fields{"n": 1})
		}))
		rows, err := db.Query("SELECT * FROM notes")
		ok(t, err)
		cols, err := rows.Columns()
		ok(t, err)
		rows.Close()
		fields[e.Name()] = strings.Join(cols, " ")
	}
	if got := fields[zerox.Engine{}.Name()]; got != "key zz n" {
		t.Errorf("0.x's notes holds %s", got)
	}
	if got := fields[betax.Engine{}.Name()]; got != "key n" {
		t.Errorf("the Beta's notes holds %s, where the failed Put should leave nothing", got)
	}
}

// TestTheBetaTakesValuesUpToTheLimit: an argument at the limit goes into a
// statement and into Nearest's filter, which only read it. A statement of
// 1,000,000,000 bytes is read up to its first NUL byte, as SQLite reads it,
// and one byte more is "statement too long", as is a filter that makes
// 0.x's query longer. An import of a value one byte over names the line and
// the key, as 0.x's does, before a record there twice, and after a value
// that breaks another rule. Each is 0.x's answer, which L1.md records.
func TestTheBetaTakesValuesUpToTheLimit(t *testing.T) {
	at := huge(t, maxLength, "", "")
	over := huge(t, maxLength+1, "", "")
	rise := memoryPeak(t)
	db := open(t, filepath.Join(t.TempDir(), "beta.hcx"))
	ok(t, db.Put("docs:1", hc.Fields{"t": "x", "n": 1, "vec": hc.Vector{1, 2}}))
	var n int64
	ok(t, db.QueryRow("SELECT length(CAST(? AS BLOB))", at).Scan(&n))
	if n != maxLength {
		t.Errorf("an argument at the limit reads as %d bytes", n)
	}
	hits, err := db.Nearest("docs", hc.Vector{1, 2}, 3, "t = ? OR n = 1", at)
	if err != nil || len(hits) != 1 {
		t.Errorf("a filter with an argument at the limit gave %v, %v", hits, err)
	}

	sqliteSays := func(what string, err error, msg string) {
		t.Helper()
		if kindOf(err) != "error" || err.Error() != msg {
			t.Errorf("%s gave %v, want %q, of kind error", what, err, msg)
		}
	}
	_, err = db.Exec(huge(t, maxLength, "SELECT 1", "x"))
	ok(t, err)
	_, err = db.Exec(huge(t, maxLength+1, "SELECT 1", "x"))
	sqliteSays("a statement one byte over", err, "statement too long")
	_, err = db.SQL().Prepare(huge(t, maxLength+1, "SELECT 1", "x"))
	sqliteSays("SQL().Prepare of a statement one byte over", err, "statement too long")
	_, err = db.Exec(huge(t, maxLength+1, "SELECT 1", ""))
	ok(t, err)
	room := maxLength - len(`SELECT key, vec FROM "docs" WHERE vec IS NOT NULL AND ()`)
	_, err = db.Nearest("docs", hc.Vector{1, 2}, 3, huge(t, room+1, "n = 1", ""))
	sqliteSays("a filter one byte too long for 0.x's query", err, "statement too long")
	_, err = db.Nearest("docs", hc.Vector{1, 2}, 3, huge(t, room, "n = 1", ""))
	sqliteSays("a filter that fills 0.x's query", err, "incomplete input")

	// An import, into a database of its own each time. A value in an export
	// comes as text, since bytes come as base64, which Put's conversion
	// copies as 0.x's does.
	docs := export.Table{Name: "docs", Fields: []string{"n", "t"}}
	one := export.Record{Key: "docs:1", Fields: []export.Field{{Name: "n", Value: int64(1)}}}
	for i, tc := range []struct {
		what  string
		items []any
		kind  string
		msg   string
	}{
		{"a value one byte over", []any{docs, export.Record{Key: "docs:1", Fields: []export.Field{{Name: "t", Value: over}}}},
			"error", "line 3 of the export: docs:1: string or blob too big"},
		{"a value one byte over after a record", []any{docs, one, export.Record{Key: "docs:2", Fields: []export.Field{{Name: "t", Value: over}}}},
			"error", "line 4 of the export: docs:2: string or blob too big"},
		{"a record there twice", []any{docs, one, export.Record{Key: "docs:1", Fields: []export.Field{{Name: "t", Value: over}}}},
			"error", "line 4 of the export: docs:1: string or blob too big"},
		{"an infinite number after it", []any{docs, export.Record{Key: "docs:1", Fields: []export.Field{{Name: "n", Value: over}, {Name: "t", Value: math.Inf(1)}}}},
			"invalid", "hypercrux: invalid: line 3 of the export: docs:1: field t is +Inf"},
	} {
		db := open(t, filepath.Join(t.TempDir(), "import"+strconv.Itoa(i)+".hcx"))
		err := hc.ImportItems(db, tc.items...)
		if kindOf(err) != tc.kind || err.Error() != tc.msg {
			t.Errorf("an import of %s gave %v, want %s: %s", tc.what, err, tc.kind, tc.msg)
		}
		if got := string(exportOf(t, db)); got != emptyExport {
			t.Errorf("an import of %s that failed kept %s", tc.what, got)
		}
	}
	smallPeak(t, rise)
}
