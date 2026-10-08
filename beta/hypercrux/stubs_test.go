// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// errOf is the error of a call that returns a value and an error.
func errOf[T any](_ T, err error) error { return err }

// TestTheStubsSayWhatsMissing calls every stub. Each returns an error that
// wraps errors.ErrUnsupported and neither of 0.x's errors, naming the call
// and the task that makes it work, and does nothing else. The task that
// makes a call work takes its line out of this test.
func TestTheStubsSayWhatsMissing(t *testing.T) {
	db, tx := new(hc.DB), new(hc.Tx)
	q := hc.Vector{1, 0}
	var out bytes.Buffer
	stubs := []struct {
		name, task string
		err        error
	}{
		{"DB.Scan", "G2", errOf(db.Scan("docs:", "", 0))},
		{"Tx.Scan", "G2", errOf(tx.Scan("docs:", "", 0))},
		{"DB.Drop", "G2", db.Drop("docs")},
		{"Tx.Drop", "G2", tx.Drop("docs")},
		{"DB.Link", "G2", db.Link("docs:1", "cites", "docs:2")},
		{"Tx.Link", "G2", tx.Link("docs:1", "cites", "docs:2")},
		{"DB.Unlink", "G2", db.Unlink("docs:1", "", "docs:2")},
		{"Tx.Unlink", "G2", tx.Unlink("docs:1", "", "docs:2")},
		{"DB.Neighbours", "G2", errOf(db.Neighbours("docs:1", hc.Both, ""))},
		{"Tx.Neighbours", "G2", errOf(tx.Neighbours("docs:1", hc.Both, ""))},
		{"DB.Walk", "G2", errOf(db.Walk("docs:1", hc.Out, "", 2))},
		{"Tx.Walk", "G2", errOf(tx.Walk("docs:1", hc.Out, "", 2))},
		{"DB.Nearest", "G2", errOf(db.Nearest("docs", q, 10, ""))},
		{"Tx.Nearest", "G2", errOf(tx.Nearest("docs", q, 10, " "))},
		{"DB.Nearest", "G4", errOf(db.Nearest("docs", q, 10, "status = ?", "open"))},
		{"Tx.Nearest", "G4", errOf(tx.Nearest("docs", q, 10, "status = ?", "open"))},
		{"DB.Exec", "G4", errOf(db.Exec("DELETE FROM docs"))},
		{"Tx.Exec", "G4", errOf(tx.Exec("DELETE FROM docs"))},
		{"DB.Query", "G4", errOf(db.Query("SELECT 1"))},
		{"Tx.Query", "G4", errOf(tx.Query("SELECT 1"))},
		{"SQL", "G4", db.QueryRow("SELECT 1").Scan(new(int))},
		{"SQL", "G4", tx.QueryRow("SELECT ?", 1).Scan(new(int))},
		{"SQL", "G4", db.SQL().Ping()},
		{"SQL", "G4", errOf(db.SQL().Begin())},
		{"DB.Export", "G6", db.Export(&out)},
		{"DB.Import", "G6", db.Import(strings.NewReader(""))},
		{"DB.Compact", "G5", db.Compact()},
	}
	for _, s := range stubs {
		want := "hypercrux: " + s.name + " is an unsupported operation in the Beta until task " + s.task
		switch {
		case !errors.Is(s.err, errors.ErrUnsupported):
			t.Errorf("%s: %v, which doesn't wrap errors.ErrUnsupported", s.name, s.err)
		case errors.Is(s.err, hc.ErrNotFound) || errors.Is(s.err, hc.ErrInvalid):
			t.Errorf("%s: %v, which wraps one of 0.x's errors", s.name, s.err)
		case s.err.Error() != want:
			t.Errorf("%s: %q, want %q", s.name, s.err, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("Export wrote %q", out.String())
	}
	if db.Path() != "" {
		t.Errorf("a DB that was never opened has the path %q", db.Path())
	}

	// SQL hands out a handle that a program can set up without a panic,
	// which the conformance suite's OneConnection does first.
	h := db.SQL()
	h.SetMaxOpenConns(1)
	if err := h.Ping(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Ping on one connection: %v", err)
	}
	h.SetMaxOpenConns(0)
	if tx.QueryRow("SELECT 1").Err() == nil {
		t.Error("QueryRow's row has no error")
	}
}

// TestOpenRefusesWhat0xRefuses checks Open's rules for a path against
// 0.x's Open, which refuses each of these names before it touches a file.
// Each error reads the same and wraps ErrInvalid.
func TestOpenRefusesWhat0xRefuses(t *testing.T) {
	t.Chdir(t.TempDir()) // in case a name ever reached a file
	for _, p := range []string{"", ":memory:", "file::memory:", "file:test.db", "file:", "a?b", "a#b", "?", "#", "dir/a?b.db", "x.db?mode=ro", "x.db#frag"} {
		_, got := hc.Open(p)
		_, want := zx.Open(p)
		if !errors.Is(want, zx.ErrInvalid) {
			t.Fatalf("0.x's Open(%q) gives %v, so the name doesn't belong here", p, want)
		}
		if !sameError(got, want) {
			t.Errorf("Open(%q) = %v; 0.x gives %v", p, got, want)
		}
	}
}
