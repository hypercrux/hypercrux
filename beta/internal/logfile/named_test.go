// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// G5's fix for I1's first finding: the first commit into a file whose name
// this Log didn't make last itself syncs the database's folder before it
// writes anything (named, in Append). Each of the first three tests leaves a
// file at the path whose name a power cut can still take back, as a
// compaction stuck after its rename leaves one, or an empty file made a
// database and stuck the same way, or a backup moved into place without a
// folder sync. Another Log commits into that file, and then the power is
// cut. The commit has to be there afterwards, from every seed. Without the
// fix, about half the seeds lose it.

// namedSeeds is how many seeds each of those tests runs.
const namedSeeds = 40

// namedDisk is a new fault disk with the database's folder, from seed.
func namedDisk(t *testing.T, seed uint64) (*fault.Disk, fsys.FS) {
	t.Helper()
	d := fault.New(seed)
	if err := d.Mkdir(filepath.Dir(crashPath)); err != nil {
		t.Fatal(err)
	}
	return d, d.FS()
}

// openNamed opens the database at crashPath through sys, with the store as
// the log's Target.
func openNamed(t *testing.T, sys fsys.FS) *db {
	t.Helper()
	d, err := openDB(sys, crashPath, crashOptions)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// lostAtACut closes d, cuts the power, opens the database again, and
// reports whether it holds less than d held.
func lostAtACut(t *testing.T, disk *fault.Disk, d *db) bool {
	t.Helper()
	want := d.state()
	if err := d.l.Close(); err != nil {
		t.Fatal(err)
	}
	disk.Cut()
	got, err := crashOpen(disk.FS())
	if err != nil {
		t.Fatal(err)
	}
	return got != want
}

// TestACommitAfterAStuckCompactionLasts: Log b compacts the database, and the
// folder's sync after the rename fails, which leaves b stuck. Once b is
// closed, Log a, which made the database and committed to it, commits again,
// which takes it to the compacted file. Its commit has to survive a power
// cut, so a syncs the folder before it, and the rename can't be taken back.
func TestACommitAfterAStuckCompactionLasts(t *testing.T) {
	lost := 0
	for seed := range uint64(namedSeeds) {
		disk, sys := namedDisk(t, seed)
		a := openNamed(t, sys)
		fill(t, a, 1, 3)
		b := openNamed(t, sys)
		disk.Add(fault.Rule{Op: fault.SyncDir, N: 1})
		if err := b.compact(); !errors.Is(err, errs.ErrStuck) {
			t.Fatalf("seed %d: the compaction gave %v, where the failed folder sync leaves the Log stuck", seed, err)
		}
		disk.Clear()
		if err := b.l.Close(); err != nil {
			t.Fatal(err)
		}
		fill(t, a, 4, 4)
		if a.l.hdr.Gen != 2 {
			t.Fatalf("seed %d: the commit after the compaction went into a file of generation %d", seed, a.l.hdr.Gen)
		}
		if lostAtACut(t, disk, a) {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("in %d seeds of %d, a power cut lost a commit made into the file a stuck compaction left", lost, namedSeeds)
	}
}

// TestACommitAfterAStuckEmptyFileLasts: an empty file is at the path, and
// two Logs have read it. Log a's commit makes a database of it, and the
// folder's sync after the rename fails, which leaves a stuck. Once a is
// closed, Log b commits, which takes it to the new file, a database of
// generation 1 like any other. Its commit has to survive a power cut too, so
// the rule can't go by the generation.
func TestACommitAfterAStuckEmptyFileLasts(t *testing.T) {
	lost := 0
	for seed := range uint64(namedSeeds) {
		disk, sys := namedDisk(t, seed)
		f, err := sys.Create(crashPath, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := sys.SyncDir(filepath.Dir(crashPath)); err != nil {
			t.Fatal(err)
		}
		a, b := openNamed(t, sys), openNamed(t, sys)
		disk.Add(fault.Rule{Op: fault.SyncDir, N: 1})
		if err := a.commit(func(tx *store.Tx) error { return compactOps(tx, 1) }); !errors.Is(err, errs.ErrStuck) {
			t.Fatalf("seed %d: the commit that made a database of the empty file gave %v, where the failed folder sync leaves the Log stuck", seed, err)
		}
		disk.Clear()
		if err := a.l.Close(); err != nil {
			t.Fatal(err)
		}
		fill(t, b, 2, 3)
		if b.l.hdr.Gen != 1 || b.l.seq != 2 {
			t.Fatalf("seed %d: the commits went into a file of generation %d, as batch %d", seed, b.l.hdr.Gen, b.l.seq)
		}
		if lostAtACut(t, disk, b) {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("in %d seeds of %d, a power cut lost a commit made into the database a stuck Log made of an empty file", lost, namedSeeds)
	}
}

// TestACommitAfterABackupMovedInLasts: a backup is moved into place with a
// rename and no folder sync, which is up to whoever moves it. A Log that was
// open on the database commits, which takes it to the backup, and its
// commit has to survive a power cut, so it syncs the folder first.
func TestACommitAfterABackupMovedInLasts(t *testing.T) {
	const backup = "/db/backup.hcx"
	lost := 0
	for seed := range uint64(namedSeeds) {
		disk, sys := namedDisk(t, seed)
		a := openNamed(t, sys)
		fill(t, a, 1, 3)
		copyFile(t, sys, crashPath, backup)
		fill(t, a, 4, 4)
		if err := sys.Rename(backup, crashPath); err != nil {
			t.Fatal(err)
		}
		fill(t, a, 5, 5)
		if a.c.resets != 1 {
			t.Fatalf("seed %d: the copy was reset %d times, where the commit moves it to the backup", seed, a.c.resets)
		}
		if lostAtACut(t, disk, a) {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("in %d seeds of %d, a power cut lost a commit made into a backup moved into place", lost, namedSeeds)
	}
}

// copyFile copies the file at from to to through sys, and syncs the copy
// and its folder, so the copy is on the drive with its name.
func copyFile(t *testing.T, sys fsys.FS, from, to string) {
	t.Helper()
	f, err := sys.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, info.Size)
	if _, err := f.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	g, err := sys.Create(to, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.WriteAt(b, 0); err != nil {
		t.Fatal(err)
	}
	if err := g.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := sys.SyncDir(filepath.Dir(to)); err != nil {
		t.Fatal(err)
	}
}

// TestTheFolderIsSyncedOnceForEachFile: a Log syncs the folder before its
// first commit into a file only when it didn't make the file's name last
// itself, and only once for each file. A Log that creates the database, or
// compacts it, has synced the folder already. A Log that opens a database
// another made syncs once, at its first commit, and once more when another
// Log's compaction moves it to a new file. A failed folder sync writes
// nothing, and the next commit tries it again.
func TestTheFolderIsSyncedOnceForEachFile(t *testing.T) {
	disk, sys := namedDisk(t, 0)
	syncs := func(want int, what string) {
		t.Helper()
		if got := disk.Calls(fault.SyncDir, ""); got != want {
			t.Errorf("%s: the folder was synced %d times in all, where %d is wanted", what, got, want)
		}
	}
	a := openNamed(t, sys)
	syncs(1, "creating the database")
	fill(t, a, 1, 3)
	syncs(1, "three commits by the Log that created the database")
	b := openNamed(t, sys)
	syncs(1, "opening the database again")
	fill(t, b, 4, 6)
	syncs(2, "three commits by a Log that opened the database")
	if err := b.compact(); err != nil {
		t.Fatal(err)
	}
	syncs(3, "the compaction")
	fill(t, b, 7, 8)
	syncs(3, "two commits by the Log that compacted")
	fill(t, a, 9, 10)
	syncs(4, "two commits by a Log that another's compaction moved to the new file")

	c := openNamed(t, sys)
	size := func() int64 {
		t.Helper()
		info, err := sys.Stat(crashPath)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size
	}
	before, at := c.state(), size()
	disk.Add(fault.Rule{Op: fault.SyncDir, N: 1})
	err := c.commit(func(tx *store.Tx) error {
		return tx.Put("t:9", []format.Field{{Name: "n", Value: value.Int(11)}})
	})
	disk.Clear()
	switch {
	case !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), "wasn't written"):
		t.Errorf("a commit whose folder sync failed gave %v", err)
	case size() != at || c.state() != before:
		t.Error("a commit whose folder sync failed changed the file or the copy")
	}
	syncs(5, "a commit whose folder sync failed")
	fill(t, c, 11, 12)
	syncs(6, "two commits after it")
	if got, err := crashOpen(sys); err != nil || got != c.state() {
		t.Errorf("opened afresh, the database holds another state than the last Log's copy, or fails: %v", err)
	}
}

// TestMinGrowthSetsWhenACompactionIsDue: with Options.MinGrowth set, a
// compaction is due once the end of the log is that far past the live data,
// in place of a megabyte, as long as it's twice as far into the file.
func TestMinGrowthSetsWhenACompactionIsDue(t *testing.T) {
	const growth = 2000
	path := filepath.Join(t.TempDir(), "db")
	d := openDBT(t, fsys.OS{}, path, Options{MinGrowth: growth})
	text := value.Text(strings.Repeat("x", 300))
	// commitUntilDue puts a:1 again and again, and checks after each commit
	// that Due says what the rule says, until it says one is due.
	commitUntilDue := func() {
		t.Helper()
		for range 100 {
			if err := d.l.Lock(); err != nil {
				t.Fatal(err)
			}
			tx, err := d.c.s.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Put("a:1", []format.Field{{Name: "s", Value: text}}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(d.l.Append); err != nil {
				t.Fatal(err)
			}
			base := max(int64(d.l.hdr.CompactedEnd), format.HeaderSize)
			want := d.l.end >= 2*base && d.l.end-base >= growth
			due := d.l.Due()
			if err := d.l.Unlock(); err != nil {
				t.Fatal(err)
			}
			if due != want {
				t.Fatalf("Due says %v at %d bytes, with the live data ending at %d", due, d.l.end, base)
			}
			if due {
				return
			}
		}
		t.Fatal("no compaction came due in 100 commits")
	}
	commitUntilDue()
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	commitUntilDue()
}
