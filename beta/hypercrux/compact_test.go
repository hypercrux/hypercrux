// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bytes"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// Compact, and the compaction after a commit (G5): what they leave in the
// file, what other DBs on it read, and what a compaction that fails or
// sticks leaves for Update and Check. The crash suites through the package,
// in workloads_test.go, run both through every crash point.

// header reads the header of the database file at path through files.
func header(t *testing.T, files fsys.FS, path string) format.Header {
	t.Helper()
	f, err := files.Open(path)
	ok(t, err)
	defer f.Close()
	info, err := f.Stat()
	ok(t, err)
	head := make([]byte, format.HeaderSize)
	_, err = f.ReadAt(head, 0)
	ok(t, err)
	h, err := format.DecodeHeader(head, info.Size)
	ok(t, err)
	return h
}

// fileSize is the size of the file at path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	ok(t, err)
	return info.Size()
}

// deadData commits records, links and vectors through db, then writes over
// some, deletes others and drops a table, so the file holds dead data.
func deadData(t *testing.T, db *hc.DB) {
	t.Helper()
	for i := range 20 {
		ok(t, db.Put(fmt.Sprintf("docs:%d", i%5), hc.Fields{"n": i, "text": strings.Repeat("x", 100+i), "vec": hc.Vector{float32(i), 1, 2}}))
	}
	ok(t, db.Put("people:ann", hc.Fields{"name": "Ann"}))
	ok(t, db.Link("people:ann", "wrote", "docs:1"))
	ok(t, db.Link("people:ann", "wrote", "docs:2"))
	ok(t, db.Delete("docs:2"))
	ok(t, db.Put("tmp:1", hc.Fields{"n": 1}))
	ok(t, db.Drop("tmp"))
}

// TestCompactRewritesTheFile: Compact writes the live data into a file of
// the next generation, which takes the path and is smaller than the file of
// commits it replaces, and holds what the DB holds, byte for byte as Export
// writes it. Check finds nothing, and no compaction error. Another DB on the
// file reads the new one at its next read, and its next commit goes into it.
// A second Compact goes to generation 3, and a reopen reads it all back.
func TestCompactRewritesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	deadData(t, db)
	other := open(t, path)
	keys := []string{"docs:0", "docs:1", "docs:2", "people:ann", "tmp:1"}
	before, out, size := look(db, keys...), exportOf(t, db), fileSize(t, path)
	if got := look(other, keys...); got != before {
		t.Fatalf("the other DB reads\n%s\nwhere the first holds\n%s", got, before)
	}

	ok(t, db.Compact())
	if h := header(t, fsys.OS{}, path); h.Gen != 2 || int64(h.CompactedEnd) != fileSize(t, path) {
		t.Errorf("the compacted file has generation %d and its compacted part ends at %d, in a file of %d bytes", h.Gen, h.CompactedEnd, fileSize(t, path))
	}
	if fileSize(t, path) >= size {
		t.Errorf("the compacted file is %d bytes, where the file of commits was %d", fileSize(t, path), size)
	}
	if got := look(db, keys...); got != before {
		t.Errorf("after Compact the DB holds\n%s\nwhere it held\n%s", got, before)
	}
	if got := exportOf(t, db); !bytes.Equal(got, out) {
		t.Errorf("after Compact the export is\n%s\nwhere it was\n%s", got, out)
	}
	rep, err := db.Check()
	ok(t, err)
	if !rep.OK() || rep.CompactionError != "" {
		t.Errorf("after Compact, Check gives %+v", rep)
	}
	if got := look(other, keys...); got != before {
		t.Errorf("after the compaction, the other DB reads\n%s", got)
	}
	ok(t, other.Put("docs:9", hc.Fields{"n": 9}))
	ok(t, db.Compact())
	if h := header(t, fsys.OS{}, path); h.Gen != 3 {
		t.Errorf("a second compaction leaves generation %d", h.Gen)
	}
	want := look(db, append(keys, "docs:9")...)
	again := open(t, path)
	if got := look(again, append(keys, "docs:9")...); got != want {
		t.Errorf("a reopen of the compacted file reads\n%s\nwhere the DB holds\n%s", got, want)
	}
	if f, err := again.Get("docs:9"); err != nil || f["n"] != int64(9) {
		t.Errorf("a reopen gives the other DB's commit as %v, %v", f, err)
	}
}

// TestCompactReadsOnFirst: a DB that hasn't read what another DB committed
// since its last read compacts with those commits in, since Compact reads on
// to the end of the log under the write lock before it takes its snapshot.
func TestCompactReadsOnFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	a := open(t, path)
	b := open(t, path)
	ok(t, a.Put("docs:1", hc.Fields{"n": 1}))
	if _, err := b.Get("docs:1"); err != nil {
		t.Fatal(err)
	}
	ok(t, a.Update(func(tx *hc.Tx) error {
		if err := tx.Put("docs:2", hc.Fields{"n": 2, "vec": hc.Vector{1, 2}}); err != nil {
			return err
		}
		if err := tx.Put("later:1", nil); err != nil {
			return err
		}
		return tx.Link("docs:2", "next", "docs:1")
	}))
	ok(t, b.Compact())
	want := look(a, "docs:1", "docs:2", "later:1")
	if got := look(open(t, path), "docs:1", "docs:2", "later:1"); got != want {
		t.Errorf("the compacted file holds\n%s\nwhere the database held\n%s", got, want)
	}
	ok(t, a.Put("docs:3", hc.Fields{"n": 3}))
	again := open(t, path)
	if f, err := again.Get("docs:3"); err != nil || f["n"] != int64(3) {
		t.Errorf("after a commit into the compacted file, a reopen gives it as %v, %v", f, err)
	}
	if _, err := again.Get("later:1"); err != nil {
		t.Errorf("after a commit into the compacted file, a reopen gives the record compacted as %v", err)
	}
}

// TestUpdateCompactsWhenDue: a record written over again and again, 100 KB
// at a time, is compacted once its puts since the last compaction have made
// a megabyte dead, and half the file, by the Update whose commit took it
// there, before it returns. The file stays near its live data, and a reopen
// holds the last value. With a small MinGrowth, a compaction comes after the
// first commits that make one due, and not before.
func TestUpdateCompactsWhenDue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	big := func(i int) string { return strings.Repeat(string(rune('a'+i%26)), 100<<10) }
	gens := map[uint64]bool{}
	for i := range 30 {
		ok(t, db.Put("docs:1", hc.Fields{"text": big(i)}))
		h := header(t, fsys.OS{}, path)
		gens[h.Gen] = true
		if size := fileSize(t, path); size > 1<<20+2*(100<<10)+int64(h.CompactedEnd) {
			t.Fatalf("after commit %d the file is %d bytes, with its compacted part ending at %d", i+1, size, h.CompactedEnd)
		}
	}
	if len(gens) < 3 {
		t.Errorf("30 commits of 100 KB over one record went through the generations %v, where at least three are wanted", gens)
	}
	again := open(t, path)
	if f, err := again.Get("docs:1"); err != nil || f["text"] != big(29) {
		t.Errorf("a reopen of the compacted file reads the record back as %.20v, %v", f["text"], err)
	}

	small := filepath.Join(t.TempDir(), "small.hcx")
	sdb, err := hc.OpenWith(fsys.OS{}, small, logfile.Options{MinGrowth: 1000})
	ok(t, err)
	defer sdb.Close()
	for i := 1; ; i++ {
		ok(t, sdb.Put("docs:1", hc.Fields{"text": strings.Repeat("y", 300)}))
		if header(t, fsys.OS{}, small).Gen == 2 {
			if i < 3 {
				t.Errorf("a compaction came after commit %d, before 1,000 bytes were dead", i)
			}
			break
		}
		if i == 10 {
			t.Fatal("no compaction came in 10 commits of 300 bytes with a MinGrowth of 1,000")
		}
	}
}

// TestAFailedCompactionFailsNoUpdate: when the compaction after a commit
// fails before its switch, here at making NAME.compact once three commits
// reach the limit on commits the DB is opened with, Update returns nil,
// since its commit is in the file, which a reopen reads back. The database
// carries on in its old file, and Check reports the compaction's error until
// a compaction works: Compact, here, which takes it away.
func TestAFailedCompactionFailsNoUpdate(t *testing.T) {
	const path = "/db/test.hcx"
	d := fault.New(0)
	ok(t, d.Mkdir("/db"))
	db, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second, MaxCommits: 3})
	ok(t, err)
	defer db.Close()
	d.Add(fault.Rule{Op: fault.Create, Path: path + ".compact", N: 1})
	for i := 1; header(t, d.FS(), path).Gen == 1 && i <= 10; i++ {
		ok(t, db.Put(fmt.Sprintf("docs:%d", i), hc.Fields{"text": strings.Repeat("z", 300)}))
		if d.Failed() > 0 {
			break
		}
	}
	if d.Failed() != 1 {
		t.Fatalf("%d calls failed, where the compaction's creation of its file is to fail once", d.Failed())
	}
	if h := header(t, d.FS(), path); h.Gen != 1 {
		t.Errorf("after the failed compaction the file has generation %d", h.Gen)
	}
	rep, err := db.Check()
	ok(t, err)
	if !rep.OK() || !strings.Contains(rep.CompactionError, "compacting failed making") || !strings.Contains(rep.CompactionError, syscall.EIO.Error()) {
		t.Errorf("after the failed compaction Check gives %+v", rep)
	}
	want := look(db, "docs:1", "docs:2", "docs:3", "docs:4")
	other, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: time.Second})
	ok(t, err)
	if got := look(other, "docs:1", "docs:2", "docs:3", "docs:4"); got != want {
		t.Errorf("a reopen reads\n%s\nwhere the DB holds\n%s", got, want)
	}
	ok(t, other.Close())
	ok(t, db.Compact())
	if rep, err := db.Check(); err != nil || rep.CompactionError != "" || header(t, d.FS(), path).Gen != 2 {
		t.Errorf("after Compact worked, Check gives %+v, %v, and the file's generation is %d", rep, err, header(t, d.FS(), path).Gen)
	}
}

// TestAStuckCompactionFailsNoUpdate: when the folder's sync fails after the
// rename of the compaction that three commits set off, at the limit on
// commits the DB is opened with, the Update whose commit set it off returns
// nil, and the next write fails with ErrStuck, as Check reports. The stuck
// DB keeps the write lock until it's closed. Every commit Update reported
// survives a power cut after that, whichever file the cut leaves at the
// path.
func TestAStuckCompactionFailsNoUpdate(t *testing.T) {
	const path = "/db/test.hcx"
	d := fault.New(1)
	ok(t, d.Mkdir("/db"))
	db, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: 100 * time.Millisecond, MaxCommits: 3})
	ok(t, err)
	d.Add(fault.Rule{Op: fault.SyncDir, N: 1})
	n := 0
	for header(t, d.FS(), path).Gen == 1 {
		n++
		ok(t, db.Put(fmt.Sprintf("docs:%d", n), hc.Fields{"text": strings.Repeat("s", 300)}))
		if n == 10 {
			t.Fatal("no compaction came in 10 commits")
		}
	}
	d.Clear()
	err = db.Put("docs:late", nil)
	wantErr(t, err, hc.ErrStuck)
	if _, err := db.Get("docs:late"); !errors.Is(err, hc.ErrNotFound) {
		t.Errorf("a write refused as stuck reads back as %v", err)
	}
	rep, err := db.Check()
	ok(t, err)
	if !rep.OK() || !strings.Contains(rep.CompactionError, hc.ErrStuck.Error()) {
		t.Errorf("after the stuck compaction Check gives %+v", rep)
	}
	other, err := hc.OpenWith(d.FS(), path, logfile.Options{Wait: 100 * time.Millisecond})
	ok(t, err)
	wantErr(t, other.Put("docs:other", nil), hc.ErrLockTimeout)
	ok(t, db.Close())
	ok(t, other.Put("docs:other", nil))
	ok(t, other.Close())
	d.Cut()
	again, err := hc.OpenWith(d.FS(), path, logfile.Options{})
	ok(t, err)
	defer again.Close()
	for i := 1; i <= n; i++ {
		if _, err := again.Get(fmt.Sprintf("docs:%d", i)); err != nil {
			t.Errorf("after a power cut, docs:%d gives %v", i, err)
		}
	}
	if _, err := again.Get("docs:other"); err != nil {
		t.Errorf("after a power cut, the commit made once the stuck DB had closed gives %v", err)
	}
}

// TestAPanicInACompactionFailsNoUpdate: when ranging over the snapshot
// panics in the compaction after a commit, which the DB's limit of three
// commits sets off, Update returns nil, since its commit is in the file, and
// Check reports the panic as the compaction's error. NAME.compact is gone,
// and the database carries on in its old file. Compact itself lets the panic
// carry on to its caller.
func TestAPanicInACompactionFailsNoUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.hcx")
	db, err := hc.OpenWith(fsys.OS{}, path, logfile.Options{MaxCommits: 3})
	ok(t, err)
	defer db.Close()
	restore := hc.UseSnapshot(func(r store.Reader) iter.Seq[format.Change] {
		return func(yield func(format.Change) bool) {
			for c := range r.Snapshot() {
				if c.Op == format.Put {
					panic("the snapshot's own")
				}
				if !yield(c) {
					return
				}
			}
		}
	})
	defer restore()
	for i := 1; i <= 5; i++ {
		ok(t, db.Put(fmt.Sprintf("docs:%d", i), hc.Fields{"text": strings.Repeat("p", 300)}))
	}
	rep, err := db.Check()
	ok(t, err)
	if !strings.Contains(rep.CompactionError, "panicked") || !strings.Contains(rep.CompactionError, "the snapshot's own") {
		t.Errorf("after a compaction that panicked, Check gives %+v", rep)
	}
	if names, err := os.ReadDir(dir); err != nil || len(names) != 1 || header(t, fsys.OS{}, path).Gen != 1 {
		t.Errorf("after compactions that panicked, the folder holds %v, %v", names, err)
	}
	func() {
		defer func() {
			if r := recover(); r != "the snapshot's own" {
				t.Errorf("Compact's panic was %v", r)
			}
		}()
		db.Compact()
	}()
	restore()
	ok(t, db.Compact())
	if f, err := open(t, path).Get("docs:5"); err != nil || len(f["text"].(string)) != 300 {
		t.Errorf("after a compaction that worked, a reopen gives docs:5 as %v, %v", f, err)
	}
	if rep, err := db.Check(); err != nil || rep.CompactionError != "" {
		t.Errorf("after a compaction that worked, Check gives %+v, %v", rep, err)
	}
}

// TestCompactOnAnEmptyFile: Compact on an empty file, which holds no
// database yet, makes a database of it first, as any writer does, then
// compacts that, a header alone of generation 2, which a reopen reads as an
// empty database.
func TestCompactOnAnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.hcx")
	ok(t, os.WriteFile(path, nil, 0o644))
	db := open(t, path)
	ok(t, db.Compact())
	if h := header(t, fsys.OS{}, path); h.Gen != 2 || fileSize(t, path) != format.HeaderSize {
		t.Errorf("an empty file compacted gives generation %d in %d bytes", h.Gen, fileSize(t, path))
	}
	rep, err := open(t, path).Check()
	if err != nil || !rep.OK() || rep.Tables != 0 {
		t.Errorf("a reopen of an empty file compacted gives %+v, %v", rep, err)
	}
	if errors.Is(err, hc.ErrNotDatabase) {
		t.Error("the compacted file isn't a database")
	}
}
