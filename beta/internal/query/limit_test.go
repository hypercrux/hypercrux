// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// L1: what SQL is given and what it makes agree with SQLite on the limit of
// 1,000,000,000 bytes and its error. An argument is refused as go-sqlite3
// binds it, a statement as SQLite prepares it, and a value SQL makes as
// SQLite makes it: concatenation and replace() did already, and walk() now
// does. The long values here cost no memory: huge maps them from the
// kernel's page of zeros, and nothing copies them before it refuses them.

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

// sqliteSays checks that err is an *Error saying msg, of kind "error".
func sqliteSays(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || err.Error() != msg || errors.Is(err, errs.ErrInvalid) || errors.Is(err, errs.ErrNotFound) {
		t.Errorf("%s gave %v, want %q, of kind error", what, err, msg)
	}
}

// TestArgumentsAreBoundAsSQLiteBindsThem: Bind takes text and bytes up to
// the limit, and refuses one byte more with SQLite's error.
func TestArgumentsAreBoundAsSQLiteBindsThem(t *testing.T) {
	at := huge(t, rules.MaxLength, "", "")
	over := huge(t, rules.MaxLength+1, "", "")
	if err := Bind([]value.Value{value.Text(at), value.Bytes(at), value.Int(1), value.Real(2), {}}); err != nil {
		t.Errorf("arguments at the limit gave %v", err)
	}
	sqliteSays(t, "text one byte over", Bind([]value.Value{value.Int(1), value.Text(over)}), "string or blob too big")
	sqliteSays(t, "bytes one byte over", Bind([]value.Value{value.Bytes(over), value.Text("x")}), "string or blob too big")

	// A search's filter binds its arguments once it's planned, before the
	// terms that read no field are worked out, so 0 AND ... refuses too, as
	// in 0.x. The filter takes one at the limit, compares it with each
	// record's status, and finds none.
	s := planStore(t)
	for _, where := range []string{"status = ?", "0 AND status = ?", "? IS NULL"} {
		cond, params, err := ParseCondition(where)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = NearestFilter(s, "docs", cond, params, &Frame{Args: []value.Value{value.Text(over)}, Now: planNow})
		sqliteSays(t, "the filter "+where+" with an argument one byte over", err, "string or blob too big")
	}
	hits, err := plannedNearest(s, "docs", []float32{1, 0, 0}, 10, "status = ?", []value.Value{value.Text(at)}, planNow)
	if err != nil || len(hits) != 0 {
		t.Errorf("a filter with an argument at the limit gave %v, %v", hits, err)
	}
	cond, params, err := ParseCondition("nosuch = ?")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = NearestFilter(s, "docs", cond, params, &Frame{Args: []value.Value{value.Text(over)}, Now: planNow})
	sqliteSays(t, "a filter with a field that isn't there", err, "no such column: nosuch")
}

// TestStatementsAsLongAsSQLiteTakes: a statement of more than 1,000,000,000
// bytes is "statement too long", as SQLite refuses it before reading it,
// even when a NUL byte would end it sooner. One of exactly that length is
// read up to its first NUL, as SQLite reads it. A literal can't be longer
// than the statement it's in, so no literal is longer than the limit on
// values.
func TestStatementsAsLongAsSQLiteTakes(t *testing.T) {
	for _, n := range []int{maxSQLLength, maxSQLLength + 1} {
		text := huge(t, n, "SELECT 1", "x")
		s, err := Parse(text)
		switch {
		case n == maxSQLLength && err != nil:
			t.Errorf("a statement of %d bytes gave %v", n, err)
		case n == maxSQLLength:
			if got := s.(*Select).Results[0].Text; got != "1" {
				t.Errorf("a statement of %d bytes read as SELECT %s", n, got)
			}
		default:
			sqliteSays(t, "a statement one byte over", err, "statement too long")
		}
	}
	// A text that ends in a NUL is read as a C string, and its length
	// doesn't count past the first NUL.
	if _, err := Parse(huge(t, maxSQLLength+1, "SELECT 1", "")); err != nil {
		t.Errorf("SELECT 1 and a gigabyte of NUL bytes gave %v", err)
	}

	// Nearest's filter goes into 0.x's query, of 52 bytes and the table's
	// name besides the filter.
	room := maxSQLLength - len(`SELECT key, vec FROM "docs" WHERE vec IS NOT NULL AND ()`)
	if err := FilterLength("docs", huge(t, room, "n = 1", "")); err != nil {
		t.Errorf("a filter that makes 0.x's query 1,000,000,000 bytes gave %v", err)
	}
	sqliteSays(t, "a filter one byte longer", FilterLength("docs", huge(t, room+1, "n = 1", "")), "statement too long")
	sqliteSays(t, "the same in a table with a longer name", FilterLength("docs2", huge(t, room, "n = 1", "")), "statement too long")
	for _, where := range []string{"", " \t\n", "n = 1"} {
		if err := FilterLength("docs", where); err != nil {
			t.Errorf("the filter %q gave %v", where, err)
		}
	}
}

// TestWhatSQLMakesStopsAtTheLimit: concatenation refuses to make text one
// byte past the limit before it builds any, and walk() gives its JSON text
// up to the limit and refuses one byte more, as SQLite refuses 0.x's. Text
// at the limit takes a gigabyte to build, so concatenation is held to its
// edge only past it, where it builds nothing. A walk to a gigabyte of keys
// takes gigabytes, so walkJSON is given a smaller limit, the length of a
// walk's text.
func TestWhatSQLMakesStopsAtTheLimit(t *testing.T) {
	half := huge(t, rules.MaxLength/2, "", "")
	more := huge(t, rules.MaxLength/2+1, "", "")
	s, err := Parse("SELECT ? || ?")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Compile(s.(*Select).Results[0].Expr, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]value.Value{
		{value.Text(half), value.Text(more)},
		{value.Bytes(more), value.Text(half)},
		{value.Text(huge(t, rules.MaxLength+1, "", "")), value.Text("")},
	} {
		_, err = ev(&Frame{Args: args})
		sqliteSays(t, "concatenating one byte past the limit", err, "string or blob too big")
	}

	steps := []store.Step{{Key: "docs:1"}, {Key: "docs:<2>"}, {Key: strings.Repeat("é", 20)}}
	want, _ := json.Marshal([]string{steps[0].Key, steps[1].Key, steps[2].Key})
	call := &Call{At: 7}
	v, err := walkJSON(call, steps, len(want))
	if err != nil || v.Raw() != string(want) {
		t.Errorf("walk()'s text at the limit gave %v, %v", v, err)
	}
	_, err = walkJSON(call, steps, len(want)-1)
	sqliteSays(t, "walk()'s text one byte over the limit", err, "string or blob too big")
	var e *Error
	if errors.As(err, &e) && e.Pos != 7 {
		t.Errorf("walk()'s error is at %d, want the call's place", e.Pos)
	}
	if v, err := walkJSON(call, nil, 2); err != nil || v.Raw() != "[]" {
		t.Errorf("a walk to nothing gave %v, %v", v, err)
	}
}
