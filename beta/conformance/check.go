// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package conformance

import (
	"errors"
	"testing"
)

// Check on a healthy database (task G7), from 0.x's TestCheckCounts and
// TestCheckReportsProblems, the parts that don't look inside SQLite. Like
// RoundTrip and Across, CheckCounts and SameCounts aren't among Tests,
// since beta/SQL.md's named tests have to list every test there, so each
// adapter's tests call them.

// CheckCounts makes calls whose counts are known, on one engine, and holds
// Check's counts to them after each, and after a reopen, which reads them
// back from the file. A table stays once its records have gone, and goes
// when it's dropped; a record counts as having a vector until the vector is
// cleared; a link counts once, from a record to itself too, and adding it
// again changes nothing; a delete takes a record's links both ways; and an
// Update that fails changes no count. Check finds no problems throughout.
func CheckCounts(t *testing.T, e Engine) {
	t.Logf("engine: %s", e.Name())
	db, path := openTemp(t, e)
	counts := func(what string, tables, records, links, vectors int) {
		t.Helper()
		rep := checkOK(t, db)
		if rep.Tables != tables || rep.Records != records || rep.Links != links || rep.Vectors != vectors {
			t.Fatalf("%s: Check counts %+v, where %d tables, %d records, %d links and %d vectors are wanted", what, rep, tables, records, links, vectors)
		}
	}
	counts("a new database", 0, 0, 0, 0)
	ok(t, db.Put("docs:1", Fields{"title": "one", "vec": Vector{1, 0}}))
	ok(t, db.Put("docs:2", Fields{"vec": Vector{0, 1}}))
	ok(t, db.Put("docs:3", Fields{"n": 3}))
	ok(t, db.Put("people:1", Fields{"name": "Ann"}))
	ok(t, db.Put("gone:1", nil))
	counts("five records", 3, 5, 0, 2)
	for _, l := range []Link{
		{"people:1", "owns", "docs:1"}, {"people:1", "owns", "docs:2"}, {"docs:1", "cites", "docs:2"},
		{"docs:1", "owns", "docs:2"}, {"docs:2", "cites", "docs:2"}, {"gone:1", "x", "docs:3"},
	} {
		ok(t, db.Link(l.From, l.Type, l.To))
	}
	counts("six links", 3, 5, 6, 2)
	ok(t, db.Link("people:1", "owns", "docs:1"))
	counts("a link added again", 3, 5, 6, 2)
	failed := errors.New("the Update gives up")
	err := db.Update(func(tx Handle) error {
		if err := tx.Put("docs:9", Fields{"vec": Vector{1, 1}}); err != nil {
			return err
		}
		if err := tx.Link("docs:9", "cites", "docs:1"); err != nil {
			return err
		}
		if err := tx.Delete("docs:2"); err != nil {
			return err
		}
		return failed
	})
	wantErr(t, err, failed)
	counts("an Update that failed", 3, 5, 6, 2)
	ok(t, db.Put("docs:1", Fields{"vec": nil}))
	counts("a vector cleared", 3, 5, 6, 1)
	ok(t, db.Unlink("docs:1", "", "docs:2"))
	counts("two links taken out at once", 3, 5, 4, 1)
	ok(t, db.Delete("docs:2"))
	counts("a delete that takes two links with it, one to itself", 3, 4, 2, 0)
	ok(t, db.Drop("gone"))
	counts("a table dropped with its record and its link", 2, 3, 1, 0)
	ok(t, db.Delete("people:1"))
	counts("a table left empty", 2, 2, 0, 0)
	ok(t, db.Close())
	db = must[DB](t)(e.Open(path))
	t.Cleanup(func() { db.Close() })
	counts("after a reopen", 2, 2, 0, 0)
}

// SameCounts makes the same calls on a database of each engine: fill's
// awkward values, deletes and drops, and its random records and links. Check
// finds no problems on either, and counts the same on both, before and
// after a reopen.
func SameCounts(t *testing.T, a, b Engine) {
	t.Logf("%s and %s", a.Name(), b.Name())
	dbA, pathA := openTemp(t, a)
	dbB, pathB := openTemp(t, b)
	fill(t, a, dbA, 3)
	fill(t, b, dbB, 3)
	same := func(when string) {
		t.Helper()
		ra, rb := checkOK(t, dbA), checkOK(t, dbB)
		if ra.Tables != rb.Tables || ra.Records != rb.Records || ra.Links != rb.Links || ra.Vectors != rb.Vectors {
			t.Fatalf("%s, Check counts %+v on %s, and %+v on %s", when, rb, b.Name(), ra, a.Name())
		}
		if ra.Records == 0 || ra.Links == 0 || ra.Vectors == 0 {
			t.Fatalf("%s, Check counts %+v, where fill makes records, links and vectors", when, ra)
		}
	}
	same("after the calls")
	ok(t, dbA.Close())
	ok(t, dbB.Close())
	dbA = must[DB](t)(a.Open(pathA))
	t.Cleanup(func() { dbA.Close() })
	dbB = must[DB](t)(b.Open(pathB))
	t.Cleanup(func() { dbB.Close() })
	same("after a reopen")
}
