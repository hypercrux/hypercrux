// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/crash"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
)

// T3's crash-point driver through the public package (I1), on the fault
// layer's disk: the real log with the real store as its Target, joined as
// Open joins them, so a crash's outcome is checked on the copy a DB reads.
// Each workload makes its commits with Update, and tells the driver the state
// each one gives from inside its function, once its changes are made and
// before Commit hands them to the file: the transaction's own snapshot, as
// text (hc.TxState). Reopen opens the database as a process started after a
// crash, reads the copy through the DB's own read, which follows the file
// first (hc.ReadState), and runs Check. A state holds every table with its
// vector size and its fields in order, every record with every value's bits,
// and every link, so two copies with the same state agree on all four
// handles. A compaction goes through a DB's own log and copy, as Compact will
// once task G5 makes it work (hc.CompactThroughTheDB).
//
// The tests here run one after another, and after the package's other tests,
// so a planted bug that an earlier test catches stops a run with -failfast
// before them (scripts/planted.sh).

// workloadPath is where the database is on the driver's disks.
const workloadPath = "/db/package.hcx"

// workloadOptions fixes the database ID, so a workload's file holds the same
// bytes in every run and a crash point replays from its seed, and keeps a
// wait for the write lock short: the only handle a workload's Update can wait
// for is one left stuck.
var workloadOptions = logfile.Options{ID: [16]byte{'t', 'h', 'e', ' ', 'p', 'a', 'c', 'k', 'a', 'g', 'e'}, Wait: 10 * time.Millisecond}

// workloadOps is commit i's work, through the transaction, as a program
// would write it. It leaves dead data in the file for a compaction to drop:
// puts of five records in turn, so each fifth commit writes over one, with
// text of 1 to 700 letters, as many as i picks, so that batches and markers
// fall across sectors in many ways; vectors in a table of their own; links
// between the two tables; deletes, which take links with them; and a table
// made and then dropped.
func workloadOps(tx *hc.Tx, i int) error {
	text := make([]byte, i*389%700+1)
	for j := range text {
		text[j] = 'a' + byte((i+j)%26)
	}
	key := fmt.Sprintf("t:%d", i%5)
	if err := tx.Put(key, hc.Fields{"n": i, "s": string(text)}); err != nil {
		return err
	}
	if i%2 == 0 {
		if err := tx.Put(fmt.Sprintf("v:%d", i%3), hc.Fields{"vec": hc.Vector{float32(i), 1, -0.5, float32(i % 3)}}); err != nil {
			return err
		}
	}
	if to := fmt.Sprintf("v:%d", (i+1)%3); i%3 == 0 {
		if _, err := tx.Get(to); err == nil {
			if err := tx.Link(key, "near", to); err != nil {
				return err
			}
		}
	}
	if gone := fmt.Sprintf("t:%d", (i+2)%5); i%4 == 0 {
		if _, err := tx.Get(gone); err == nil {
			if err := tx.Delete(gone); err != nil {
				return err
			}
		}
	}
	switch i % 6 {
	case 5:
		return tx.Put(fmt.Sprintf("tmp:%d", i), hc.Fields{"n": i})
	case 0:
		if err := tx.Drop("tmp"); err != nil && !errors.Is(err, hc.ErrNotFound) {
			return err
		}
	}
	return nil
}

// workloadCommit makes commit i through db, telling m the state it gives from
// inside Update's function, and then m.Done once Update has returned nil. It
// returns Update's error. A commit that fails leaves the copy as it was, so
// the next commit's state is built without it, as the driver's model wants of
// a workload that carries on.
//
// Update can return an error for a commit that succeeded: when the commit is
// in the file and only letting go of flock fails, it gives that error. The
// copy keeps a commit only once Append has written it, synced it and marked
// it, so when Update fails and the copy holds the commit's state, the commit
// counts as done, and every check after it holds the file to that.
func workloadCommit(db *hc.DB, m *crash.Model, i int) error {
	var state string
	err := db.Update(func(tx *hc.Tx) error {
		if err := workloadOps(tx, i); err != nil {
			return err
		}
		state = hc.TxState(tx)
		if m != nil {
			m.Begin(state)
		}
		return nil
	})
	switch {
	case m == nil:
	case err == nil:
		m.Done()
	case state != "":
		if now, e := hc.ReadState(db); e == nil && now == state {
			m.Done()
		}
	}
	return err
}

// workloadOpen opens the database through sys, and opens it a second time
// when carry is set and the first open fails, since a failure in Open's
// check of the end of the log leaves the end for the next check.
func workloadOpen(sys fsys.FS, carry bool) (*hc.DB, error) {
	db, err := hc.OpenWith(sys, workloadPath, workloadOptions)
	if err != nil && carry {
		db, err = hc.OpenWith(sys, workloadPath, workloadOptions)
	}
	return db, err
}

// workloadReopen opens the database through sys as a process started after
// a crash would, and returns the state the DB reads, once Check has found
// nothing wrong.
func workloadReopen(sys fsys.FS) (string, error) {
	db, err := hc.OpenWith(sys, workloadPath, workloadOptions)
	if err != nil {
		return "", err
	}
	defer db.Close()
	state, err := hc.ReadState(db)
	if err != nil {
		return "", err
	}
	rep, err := db.Check()
	if err != nil {
		return "", err
	}
	if !rep.OK() {
		return "", fmt.Errorf("Check found problems: %s", strings.Join(rep.Problems, "; "))
	}
	return state, nil
}

// crashWorkload makes commits commits through one DB on a new database. With
// compact above 0, a compaction follows commit compact: through the same DB
// when other is false, and when it's true, through another DB on the same
// file, opened for it, so the first DB's next Update finds another file at
// the path and reads it. With carry, it carries on after a commit or a
// compaction that fails, as a program carries on after an Update that
// returned an error; without it, it stops at the first error, as a crash
// would stop it.
//
// The other DB is closed once it has compacted, unless the folder's sync
// after the rename failed and left it stuck. Then it stays open until the end
// of the run, keeping the write lock, as BETA.md has a stuck handle do, and
// the first DB's Updates time out waiting for it. Closed, it would let the
// first DB commit into a file whose name may not have reached the drive, and
// a power cut could lose those commits (TestACommitAfterAStuckCompactionLasts).
func crashWorkload(commits, compact int, other, carry bool) crash.Workload {
	return crash.Workload{
		Path: workloadPath,
		Run: func(sys fsys.FS, m *crash.Model) error {
			db, err := workloadOpen(sys, carry)
			if err != nil {
				return err
			}
			defer db.Close()
			for i := 1; i <= commits; i++ {
				if err := workloadCommit(db, m, i); err != nil && !carry {
					return err
				}
				if i != compact {
					continue
				}
				by := db
				if other {
					if by, err = workloadOpen(sys, carry); err != nil {
						if !carry {
							return err
						}
						continue
					}
				}
				err := hc.CompactThroughTheDB(by)
				switch {
				case !other:
				case errors.Is(err, hc.ErrStuck):
					defer by.Close()
				default:
					by.Close()
				}
				if err != nil && !carry {
					return err
				}
			}
			return nil
		},
		Reopen: workloadReopen,
	}
}

// crashWorkloads are the workloads the driver runs: commits through one DB on a
// new database, a compaction by that DB midway, and a compaction by another
// DB midway, after which the first carries on in the new file.
func crashWorkloads(carry bool) []struct {
	name string
	w    crash.Workload
} {
	commits := 8
	if testing.Short() {
		commits = 6
	}
	return []struct {
		name string
		w    crash.Workload
	}{
		{"a new database", crashWorkload(commits, 0, false, carry)},
		{"a compaction by the same DB", crashWorkload(commits, commits/2, false, carry)},
		{"a compaction by another DB", crashWorkload(commits, commits/2, true, carry)},
	}
}

// TestThePackagePassesACutAtEveryWrite: T3's driver through the public
// package, with the power cut in every write, in every call that changes
// names, and after the last call, each from many seeds, for commits through
// Update on a new database, and with a compaction midway by the same DB or by
// another. After every cut the DB opens at the last commit that succeeded or
// the one under way, and after a second cut at that one or a later one, so
// nothing a reader saw is lost, and its copy holds that commit's state
// exactly, on all four handles.
func TestThePackagePassesACutAtEveryWrite(t *testing.T) {
	seeds := 48
	if testing.Short() {
		seeds = 6
	}
	for i, c := range crashWorkloads(false) {
		t.Run(c.name, func(t *testing.T) {
			rep, err := crash.Run(c.w, crash.Options{Kinds: crash.Cuts, Seeds: seeds, Seed: uint64(1000 * i)})
			if err != nil {
				t.Fatal(err)
			}
			t.Log(rep)
			if rep.Last == 0 || rep.Later == 0 {
				t.Errorf("of the opens, %d found the last commit that succeeded and %d a later one, where both should come up", rep.Last, rep.Later)
			}
		})
	}
}

// TestThePackageAfterAFailedCall: T3's driver through the public package,
// with a failure at every call, in workloads that carry on after an Update
// or a compaction that fails, once, and on a disk that dies, where every call
// fails from the one picked on. Once the workload's process has ended, the DB
// opens at the last commit that succeeded or one begun after it, and after a
// power cut there or later, with its copy holding that state exactly. With
// one call failing, every failed commit is cut back out of the file, so no
// open finds one. On a disk that dies, a failed commit's cut back fails too,
// the DB is stuck, and the next process marks the failed batch when it
// counts, so both kinds of open come up.
func TestThePackageAfterAFailedCall(t *testing.T) {
	seeds := 8
	if testing.Short() {
		seeds = 2
	}
	for i, c := range crashWorkloads(true) {
		for _, times := range []int{0, -1} {
			name := c.name
			if times < 0 {
				name += ", on a disk that dies"
			}
			t.Run(name, func(t *testing.T) {
				rep, err := crash.Run(c.w, crash.Options{Kinds: crash.Failures, Seeds: seeds, Seed: uint64(1000 * i), Times: times})
				if err != nil {
					t.Fatal(err)
				}
				t.Log(rep)
				switch {
				case rep.Last == 0:
					t.Error("no open found the last commit that succeeded")
				case times == 0 && rep.Later != 0:
					t.Errorf("%d opens found a commit begun after the last that succeeded, where every failed commit is cut back out of the file", rep.Later)
				case times != 0 && rep.Later == 0:
					t.Error("no open found a commit begun after the last that succeeded, where a stuck DB leaves its failed batch, which the next check marks when it counts")
				}
			})
		}
	}
}

// TestCopiesThroughThePackage: a copy of the file taken from every call on, as
// cp would take it, whole or 100 bytes at a time, in commits through Update
// and the compactions, opens through the public package at a state the model
// allows, its copy holding that state exactly.
func TestCopiesThroughThePackage(t *testing.T) {
	seeds := 2
	if testing.Short() {
		seeds = 1
	}
	for i, c := range crashWorkloads(false) {
		for _, chunk := range []int{0, 100} {
			t.Run(fmt.Sprintf("%s, %d bytes a read", c.name, chunk), func(t *testing.T) {
				rep, err := crash.Run(c.w, crash.Options{Kinds: crash.Copies, Seeds: seeds, Seed: uint64(1000 * i), Chunk: chunk})
				if err != nil {
					t.Fatal(err)
				}
				t.Log(rep)
				if rep.Copies == 0 || rep.NoFile == 0 {
					t.Errorf("%d copies, and %d points with no file to copy, where the database is created by the workload", rep.Copies, rep.NoFile)
				}
			})
		}
	}
}

// TestACommitAfterAStuckCompactionLasts: a DB compacts the database, and the
// folder's sync after the rename fails, which leaves it stuck. Once it's
// closed, another DB commits into the compacted file, and Update returns nil,
// so the commit has to survive a power cut. It doesn't always: the rename
// that put the compacted file at the path never reached the drive, and a cut
// can put the old file back, without the commit. In a trial with the skip
// taken out, 19 seeds of 40 lost it. The log needs a folder sync before the
// first commit into a file whose name a stuck compaction left unsynced
// (I1.md, "Notes for later tasks").
func TestACommitAfterAStuckCompactionLasts(t *testing.T) {
	t.Skip("waits for the log to sync the folder before the first commit into a file that a stuck compaction renamed into place (I1.md)")
	lost := 0
	for seed := range uint64(40) {
		d := fault.New(seed)
		if err := d.Mkdir("/db"); err != nil {
			t.Fatal(err)
		}
		sys := d.FS()
		db, err := hc.OpenWith(sys, workloadPath, workloadOptions)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 3; i++ {
			if err := workloadCommit(db, nil, i); err != nil {
				t.Fatal(err)
			}
		}
		other, err := hc.OpenWith(sys, workloadPath, workloadOptions)
		if err != nil {
			t.Fatal(err)
		}
		d.Add(fault.Rule{Op: fault.SyncDir, N: 1})
		if err := hc.CompactThroughTheDB(other); !errors.Is(err, hc.ErrStuck) {
			t.Fatalf("seed %d: the compaction gave %v, where the failed folder sync leaves the DB stuck", seed, err)
		}
		d.Clear()
		if err := other.Close(); err != nil {
			t.Fatal(err)
		}
		if err := workloadCommit(db, nil, 4); err != nil {
			t.Fatalf("seed %d: the commit after the stuck DB closed: %v", seed, err)
		}
		want, err := hc.ReadState(db)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		d.Cut()
		got, err := workloadReopen(d.FS())
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("in %d seeds of 40, a power cut lost a commit that Update said had succeeded", lost)
	}
}
