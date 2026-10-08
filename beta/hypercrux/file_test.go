// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The database through the file: what a reopen reads back, a commit whose
// write fails, a file moved into place, and batches the store refuses.

func open(t *testing.T, path string) *hc.DB {
	t.Helper()
	db, err := hc.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got the error %v, where one that wraps %v is wanted", err, target)
	}
}

// openFault creates a database at path on the fault layer's disk d, and
// opens it again, so that the calls on its file count on path: the fault
// layer counts a file's calls on the path it was opened with, and a new
// database is made under another name and renamed into place.
func openFault(t *testing.T, d *fault.Disk, path string) *hc.DB {
	t.Helper()
	ok(t, d.Mkdir(filepath.Dir(path)))
	db, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second})
	ok(t, err)
	ok(t, db.Close())
	db, err = hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second})
	ok(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// look describes what db holds at the keys named, and Check's counts, with
// every number's bits.
func look(db *hc.DB, keys ...string) string {
	var b strings.Builder
	for _, k := range keys {
		f, err := db.Get(k)
		fmt.Fprintf(&b, "%s: %s %s\n", k, show(f), describeErr(err))
	}
	rep, err := db.Check()
	fmt.Fprintf(&b, "check: %+v %v", rep, err)
	return b.String()
}

// TestAReopenReadsBackWhatWasCommitted commits awkward values, field
// spellings, vector sizes, deletes, rollbacks and a few hundred commits,
// and checks that a reopen reads all of it back, bit for bit, and keeps the
// rules that depend on it.
func TestAReopenReadsBackWhatWasCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	big := make(hc.Vector, hc.MaxDims)
	for i := range big {
		big[i] = float32(i%7) - 3
	}
	awkward := hc.Fields{
		"neg_zero": math.Copysign(0, -1), "tiny": math.SmallestNonzeroFloat64, "huge": -math.MaxFloat64,
		"min": int64(math.MinInt64), "max": int64(math.MaxInt64), "empty": "", "nul": "a\x00b",
		"scripts": "é ✓ 😀 \U0010ffff", "bytes": []byte{0, 255, 0}, "no_bytes": []byte{},
		"vec": hc.Vector{float32(math.Copysign(0, -1)), math.SmallestNonzeroFloat32, math.MaxFloat32, -1},
	}
	ok(t, db.Put("docs:1", awkward))
	ok(t, db.Put("big:1", hc.Fields{"vec": big}))
	ok(t, db.Put("docs:2", hc.Fields{"Title": "first", "vec": hc.Vector{1, 2, 3, 4}}))
	ok(t, db.Put("docs:3", hc.Fields{"n": 3}))
	ok(t, db.Delete("docs:3"))
	ok(t, db.Put("docs:4", hc.Fields{"n": 4, "vec": hc.Vector{0, 0, 0, 1}}))
	ok(t, db.Put("docs:4", hc.Fields{"vec": nil})) // a vector cleared
	boom := errors.New("boom")
	if err := db.Update(func(tx *hc.Tx) error {
		ok(t, tx.Put("docs:5", hc.Fields{"n": 5}))
		ok(t, tx.Delete("docs:1"))
		ok(t, tx.Put("gone:1", nil))
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Update gave %v", err)
	}

	// A few hundred commits of a few changes each, against a model.
	commits := 300
	if testing.Short() {
		commits = 60
	}
	model := map[string]int64{}
	for i := range commits {
		ok(t, db.Update(func(tx *hc.Tx) error {
			for j := range 3 {
				key := fmt.Sprintf("many:%d", (i*7+j*13)%97)
				if (i+j)%5 == 0 {
					if err := tx.Delete(key); errors.Is(err, hc.ErrNotFound) {
						continue
					} else if err != nil {
						return err
					}
					delete(model, key)
					continue
				}
				if err := tx.Put(key, hc.Fields{"i": i, "j": j}); err != nil {
					return err
				}
				model[key] = int64(i)
			}
			return nil
		}))
	}
	keys := []string{"docs:1", "docs:2", "docs:3", "docs:4", "docs:5", "gone:1", "big:1"}
	for i := range 97 {
		keys = append(keys, fmt.Sprintf("many:%d", i))
	}
	before := look(db, keys...)
	ok(t, db.Close())

	db = open(t, path)
	if after := look(db, keys...); after != before {
		t.Fatalf("after a reopen the database holds\n%s\nwhere before it, it held\n%s", after, before)
	}
	for i := range 97 {
		key := fmt.Sprintf("many:%d", i)
		f, err := db.Get(key)
		if n, ok := model[key]; ok != (err == nil) || ok && f["i"] != n {
			t.Fatalf("%s holds %v, %v after a reopen, where the model has %d, %v", key, f, err, n, ok)
		}
	}
	// Every value went in as a type Get gives back, so it comes back the
	// same.
	if f, err := db.Get("docs:1"); err != nil || show(f) != show(awkward) {
		t.Fatalf("docs:1 came back as\n  %s, %v\nwhere\n  %s\nwent in", show(f), err, show(awkward))
	}
	if f, _ := db.Get("big:1"); show(f) != show(hc.Fields{"vec": big}) {
		t.Fatal("a vector of 65,536 values came back changed")
	}
	// The rules that hang on what was committed hold after the reopen: a
	// table's spelling of a field, and its vector size.
	ok(t, db.Put("docs:2", hc.Fields{"TITLE": "second"}))
	if f, _ := db.Get("docs:2"); f["Title"] != "second" || len(f) != 2 {
		t.Fatalf("after a reopen, a put of TITLE gave %v", f)
	}
	err := db.Put("docs:6", hc.Fields{"vec": hc.Vector{1, 2}})
	if want := "hypercrux: invalid: table docs holds vectors of 4 values, and docs:6 has 2"; err == nil || err.Error() != want {
		t.Fatalf("after a reopen, a vector of the wrong size gave %v", err)
	}
	ok(t, db.Put("docs:6", hc.Fields{"vec": hc.Vector{4, 3, 2, 1}}))
}

// TestUpdateRollsBackWhenTheCommitFails fails the batch's write, its sync
// and its marker's write in turn, on the fault layer's disk, in an Update
// that puts, adds a field and a table, and deletes. Each time Update gives
// the disk's error, and the copy holds what it held before. A reopen reads
// what was there before, or the whole commit when the batch was written
// whole: a commit whose error comes from the file has an outcome that's
// unknown, as Update says, and F3 marks a whole batch that lost its marker.
// Part of a commit is never read.
func TestUpdateRollsBackWhenTheCommitFails(t *testing.T) {
	const path = "/db/test.hcx"
	keys := []string{"docs:1", "docs:2", "docs:3", "fresh:1"}
	setUp := func(t *testing.T, seed uint64) (*fault.Disk, *hc.DB) {
		d := fault.New(seed)
		db := openFault(t, d, path)
		ok(t, db.Put("docs:1", hc.Fields{"n": 1, "vec": hc.Vector{1, 2}}))
		ok(t, db.Put("docs:3", hc.Fields{"title": "three"}))
		return d, db
	}
	commit := func(tx *hc.Tx) error {
		ok(t, tx.Put("docs:1", hc.Fields{"n": 2, "new_field": "x"}))
		ok(t, tx.Put("docs:2", hc.Fields{"vec": hc.Vector{3, 4}}))
		ok(t, tx.Put("fresh:1", hc.Fields{"a": 1}))
		ok(t, tx.Delete("docs:3"))
		return nil
	}
	// What the database holds without the commit, and with it.
	d, db := setUp(t, 0)
	without := look(db, keys...)
	ok(t, db.Update(commit))
	with := look(db, keys...)
	db.Close()

	for _, c := range []struct {
		what string
		rule fault.Rule
	}{
		{"the batch's write", fault.Rule{Op: fault.WriteAt, Path: path, N: 1}},
		{"the sync", fault.Rule{Op: fault.Sync, Path: path, N: 1}},
		{"the marker's write", fault.Rule{Op: fault.WriteAt, Path: path, N: 2}},
	} {
		for seed := uint64(1); seed <= 6; seed++ {
			d, db = setUp(t, seed)
			d.Add(c.rule)
			err := db.Update(commit)
			if !errors.Is(err, syscall.EIO) || d.Failed() != 1 {
				t.Fatalf("%s failing, seed %d: Update gave %v, and the disk failed %d calls", c.what, seed, err, d.Failed())
			}
			if got := look(db, keys...); got != without {
				t.Fatalf("%s failing, seed %d: the copy holds\n%s\nwhere before the Update, it held\n%s", c.what, seed, got, without)
			}
			d.Clear()
			again, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second})
			if err != nil {
				t.Fatalf("%s failing, seed %d: reopening: %v", c.what, seed, err)
			}
			got := look(again, keys...)
			if got != without && (got != with || c.what == "the batch's write") {
				t.Fatalf("%s failing, seed %d: a reopen reads\n%s\nwhich is neither\n%s\nnor\n%s", c.what, seed, got, without, with)
			}
			again.Close()
			db.Close()
		}
	}
}

// TestAnUpdateThatChangesNothingWritesNothing checks that the log is
// written only by a commit with changes: not by an Update whose function
// fails, panics, makes no change, or makes only writes that fail.
func TestAnUpdateThatChangesNothingWritesNothing(t *testing.T) {
	const path = "/db/test.hcx"
	d := fault.New(1)
	db := openFault(t, d, path)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	writes := d.Calls(fault.WriteAt, path)
	boom := errors.New("boom")
	for _, fn := range []func(tx *hc.Tx) error{
		func(tx *hc.Tx) error { tx.Put("docs:2", hc.Fields{"n": 2}); return boom },
		func(tx *hc.Tx) error { tx.Put("docs:2", hc.Fields{"n": 2}); panic(boom) },
		func(tx *hc.Tx) error { _, err := tx.Get("docs:1"); return err },
		func(tx *hc.Tx) error {
			tx.Put("Docs:2", nil)
			tx.Put("docs:2", hc.Fields{"n": math.NaN()})
			tx.Delete("docs:9")
			return nil
		},
	} {
		func() {
			defer func() { recover() }()
			db.Update(fn)
		}()
	}
	ok(t, db.Delete("docs:1"))
	if err := db.Delete("docs:1"); !errors.Is(err, hc.ErrNotFound) {
		t.Fatalf("deleting a missing record gave %v", err)
	}
	if n := d.Calls(fault.WriteAt, path) - writes; n != 2 {
		t.Errorf("the Updates that changed nothing and one Delete made %d writes, where the Delete's batch and marker make 2", n)
	}
}

// TestAFileMovedIntoPlaceIsReadAfresh moves another database over the one
// a DB has open, as a backup put back would be. The DB's next Update finds
// the new file at the path, puts an empty copy in place of its own, reads
// the new file into it, and commits there.
func TestAFileMovedIntoPlaceIsReadAfresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.hcx")
	db := open(t, path)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	other, err := hc.Open(path + ".other")
	ok(t, err)
	ok(t, other.Put("people:1", hc.Fields{"name": "Dana"}))
	ok(t, other.Close())
	ok(t, os.Rename(path+".other", path))

	ok(t, db.Update(func(tx *hc.Tx) error {
		if _, err := tx.Get("people:1"); err != nil {
			t.Errorf("inside the Update after the move, people:1 gives %v", err)
		}
		if _, err := tx.Get("docs:1"); !errors.Is(err, hc.ErrNotFound) {
			t.Errorf("inside the Update after the move, docs:1 from the old file gives %v", err)
		}
		return tx.Put("people:2", hc.Fields{"name": "Sam"})
	}))
	_, err = db.Get("docs:1")
	wantErr(t, err, hc.ErrNotFound)
	if f, err := db.Get("people:1"); err != nil || f["name"] != "Dana" {
		t.Fatalf("after the move, people:1 gives %v, %v", f, err)
	}
	if rep, err := db.Check(); err != nil || rep.Tables != 1 || rep.Records != 2 {
		t.Fatalf("after the move, Check gives %+v, %v", rep, err)
	}
	db2 := open(t, path)
	if got, want := look(db2, "docs:1", "people:1", "people:2"), look(db, "docs:1", "people:1", "people:2"); got != want {
		t.Fatalf("a reopen reads\n%s\nwhere the DB holds\n%s", got, want)
	}
}

// writeBatches appends each change list to the database at path as a
// batch of its own, through the log alone, with no store to refuse what
// breaks the rules for the state.
func writeBatches(t *testing.T, path string, lists ...[]format.Change) {
	t.Helper()
	l, err := logfile.Open(fsys.OS{}, path, nothing{}, logfile.Options{})
	ok(t, err)
	defer l.Close()
	ok(t, l.Lock())
	defer l.Unlock()
	for _, c := range lists {
		ok(t, l.Append(c))
	}
}

// nothing is a logfile.Target that takes every batch and keeps nothing.
type nothing struct{}

func (nothing) Apply(uint64, []format.Change) error { return nil }
func (nothing) Reset()                              {}

// TestABatchThatBreaksTheRulesIsDamage writes batches the codec takes
// whose changes break the rules for the state they apply to, as no store
// would write them. Opening the file fails with damage naming the batch.
// A DB that has the file open already finds the batch at its next Update,
// which fails the same way, and its copy takes none of the batch: the
// change before the bad one doesn't go in either.
func TestABatchThatBreaksTheRulesIsDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	n := func(i int64) []format.Field { return []format.Field{{Name: "n", Value: value.Int(i)}} }
	good := []format.Change{{Op: format.CreateTable, Table: "docs"}, {Op: format.Put, Key: "docs:1", Fields: n(1)}}
	bad := []format.Change{{Op: format.Put, Key: "docs:2", Fields: n(2)}, {Op: format.Put, Key: "nosuch:1", Fields: n(3)}}
	writeBatches(t, path, good)
	db := open(t, path)
	writeBatches(t, path, bad)

	isDamage := func(what string, err error) {
		t.Helper()
		var d *hc.Damage
		if !errors.Is(err, hc.ErrDamaged) || !errors.As(err, &d) || d.Batch != 2 || !strings.Contains(err.Error(), "change 2") {
			t.Fatalf("%s gave %v, where damage naming batch 2 and its change 2 is wanted", what, err)
		}
	}
	isDamage("an Update after the bad batch", db.Update(func(*hc.Tx) error { return nil }))
	if f, err := db.Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Fatalf("after the bad batch, docs:1 gives %v, %v", f, err)
	}
	_, err := db.Get("docs:2")
	wantErr(t, err, hc.ErrNotFound) // the change before the bad one stayed out too
	if rep, err := db.Check(); err != nil || rep.Records != 1 {
		t.Fatalf("after the bad batch, Check gives %+v, %v", rep, err)
	}
	_, err = hc.Open(path)
	isDamage("opening the file", err)
}

// TestOpeningSomethingElse opens an empty file, which becomes a database
// at the first commit, keeping its permissions, and files that aren't
// HyperCrux Beta databases, which Open refuses with the Beta's errors.
func TestOpeningSomethingElse(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.hcx")
	ok(t, os.WriteFile(empty, nil, 0o600))
	db := open(t, empty)
	if rep, err := db.Check(); err != nil || rep.Tables != 0 {
		t.Fatalf("an empty file gives %+v, %v", rep, err)
	}
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	ok(t, db.Close())
	if info, err := os.Stat(empty); err != nil || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		t.Fatalf("the empty file became %v, %v", info, err)
	}
	if f, err := open(t, empty).Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Fatalf("after a reopen, docs:1 gives %v, %v", f, err)
	}

	short := filepath.Join(dir, "short.hcx")
	ok(t, os.WriteFile(short, []byte("not a database"), 0o600))
	_, err := hc.Open(short)
	wantErr(t, err, hc.ErrNotDatabase)

	zero := filepath.Join(dir, "zero.db")
	z, err := zx.Open(zero)
	ok(t, err)
	ok(t, z.Put("docs:1", zx.Fields{"n": 1}))
	ok(t, z.Close())
	_, err = hc.Open(zero)
	wantErr(t, err, hc.ErrZeroX)

	if db, err := hc.Open(dir); err == nil {
		db.Close()
		t.Fatal("opened a folder")
	}
}
