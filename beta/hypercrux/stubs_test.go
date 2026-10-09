// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"path/filepath"
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
//
// A statement that parses, with an argument for each ? mark, waits for task
// G4, which runs SQL, through each way into the driver: Exec, Query and
// QueryRow on the database and on a transaction, and SQL(), with a prepared
// statement among them. They go through the driver on an open database,
// since on one that isn't open they fail with ErrClosed first.
func TestTheStubsSayWhatsMissing(t *testing.T) {
	type stub struct {
		name, task string
		err        error
	}
	db, tx := new(hc.DB), new(hc.Tx)
	q := hc.Vector{1, 0}
	stubs := []stub{
		{"DB.Nearest", "G4", errOf(db.Nearest("docs", q, 10, "status = ?", "open"))},
		{"Tx.Nearest", "G4", errOf(tx.Nearest("docs", q, 10, "status = ?", "open"))},
		{"DB.Compact", "G5", db.Compact()},
	}

	live := open(t, filepath.Join(t.TempDir(), "test.hcx"))
	ok(t, live.Put("docs:1", hc.Fields{"n": 1}))
	prepared, err := live.SQL().Prepare("SELECT n FROM docs WHERE n = ?")
	ok(t, err)
	defer prepared.Close()
	stubs = append(stubs,
		stub{"SQL", "G4", errOf(live.Exec("DELETE FROM docs"))},
		stub{"SQL", "G4", errOf(live.Query("SELECT n FROM docs"))},
		stub{"SQL", "G4", live.QueryRow("SELECT ?", 1).Scan(new(int))},
		stub{"SQL", "G4", errOf(live.SQL().Exec("UPDATE docs SET n = ?", 2))},
		stub{"SQL", "G4", errOf(live.SQL().Query("SELECT 1"))},
		stub{"SQL", "G4", prepared.QueryRow(1).Scan(new(int))},
	)
	ok(t, live.Update(func(tx *hc.Tx) error {
		stubs = append(stubs,
			stub{"SQL", "G4", errOf(tx.Exec("INSERT INTO docs (key) VALUES (?)", "docs:2"))},
			stub{"SQL", "G4", errOf(tx.Query("SELECT n FROM docs WHERE key = ?", "docs:1"))},
			stub{"SQL", "G4", tx.QueryRow("SELECT 1").Scan(new(int))},
		)
		return nil
	}))

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
	if db.Path() != "" {
		t.Errorf("a DB that was never opened has the path %q", db.Path())
	}
	if f, err := live.Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Errorf("after the SQL stubs, docs:1 gives %v, %v", f, err)
	}
	if _, err := live.Get("docs:2"); !errors.Is(err, hc.ErrNotFound) {
		t.Errorf("after the SQL stubs, docs:2 gives %v", err)
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
