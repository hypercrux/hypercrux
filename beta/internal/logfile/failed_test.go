// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// TestACutBackLastsAPowerCut: once Append has returned the error of a commit
// that failed and was cut back, a power cut leaves the database as it was
// before the commit. The batch's write, the sync and the marker's write fail
// in turn, on the fault layer's disk, from many seeds. When the marker's
// write is the one that fails, the batch is on the drive whole, with the
// file's size, so a cut back left unsynced could come undone at the power
// cut, and the check would mark the batch.
func TestACutBackLastsAPowerCut(t *testing.T) {
	seeds := 32
	if testing.Short() {
		seeds = 8
	}
	for _, c := range []struct {
		name string
		rule fault.Rule // counted from the failed commit's start
	}{
		{"the batch's write", fault.Rule{Op: fault.WriteAt, N: 1}},
		{"the sync", fault.Rule{Op: fault.Sync, N: 1}},
		{"the marker's write", fault.Rule{Op: fault.WriteAt, N: 2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for seed := range uint64(seeds) {
				d := fault.New(seed)
				if err := d.Mkdir("/db"); err != nil {
					t.Fatal(err)
				}
				l, err := Open(d.FS(), crashPath, &recorder{}, crashOptions)
				if err != nil {
					t.Fatal(err)
				}
				if err := crashCommits(l, nil, 1, 3); err != nil {
					t.Fatal(err)
				}
				if err := l.Lock(); err != nil {
					t.Fatal(err)
				}
				d.Add(c.rule)
				err = l.Append(crashBatch(4))
				if !errors.Is(err, syscall.EIO) || errors.Is(err, errs.ErrStuck) || d.Failed() != 1 {
					t.Fatalf("seed %d: Append gave %v, and the disk failed %d calls", seed, err, d.Failed())
				}
				l.Unlock()
				d.Clear()
				d.Cut()
				got, err := crashOpen(d.FS())
				if err != nil {
					t.Fatalf("seed %d: after the power cut: %v", seed, err)
				}
				if got != crashState(3) {
					t.Fatalf("seed %d: after the power cut, the database holds\n%s\nwhere the three commits before the failed one are wanted", seed, got)
				}
			}
		})
	}
}

// TestAStuckLogsBatchIsMadeToLast: every sync fails from a commit on, so the
// commit fails, and so does its cut back, and the Log is stuck. After a
// failed sync, the pages of the failed batch and of the marker before it can
// be marked clean without reaching the drive, while reads go on seeing them.
// Once the stuck Log is closed and the disk works again, the next Log's check
// finds the failed batch whole and marks it, writing it again first with the
// marker before it, and then that Log commits after it. A power cut then
// leaves both batches there. The marker before the failed batch lies across
// the edge of two sectors, 10 bytes in each, so writing the batch again
// doesn't touch all of it, and the check has to write the marker as well.
func TestAStuckLogsBatchIsMadeToLast(t *testing.T) {
	seeds := 32
	if testing.Short() {
		seeds = 8
	}
	padded := func(n int) []format.Change {
		return []format.Change{{Op: format.Put, Key: "t:pad", Fields: []format.Field{{Name: "x", Value: value.Text(strings.Repeat("p", n))}}}}
	}
	for seed := range uint64(seeds) {
		d := fault.New(seed)
		if err := d.Mkdir("/db"); err != nil {
			t.Fatal(err)
		}
		a, err := Open(d.FS(), crashPath, &recorder{}, crashOptions)
		if err != nil {
			t.Fatal(err)
		}
		if err := crashCommits(a, nil, 1, 2); err != nil {
			t.Fatal(err)
		}
		// Commit 3's batch is as long as it takes for its marker to end 10
		// bytes into a sector.
		b, _, err := format.AppendBatch(nil, a.hdr.Gen, 3, padded(1))
		if err != nil {
			t.Fatal(err)
		}
		pad := 1 + ((10-int(a.end)-len(b)-format.MarkerSize)%fault.SectorSize+fault.SectorSize)%fault.SectorSize
		lists := [][]format.Change{crashBatch(1), crashBatch(2), padded(pad), crashBatch(4), crashBatch(5)}
		commit(t, a, lists[2])
		if a.end%fault.SectorSize != 10 {
			t.Fatalf("batch 3's marker ends %d bytes into a sector", a.end%fault.SectorSize)
		}

		if err := a.Lock(); err != nil {
			t.Fatal(err)
		}
		d.Add(fault.Rule{Op: fault.Sync, N: 1, Times: -1})
		if err := a.Append(lists[3]); !errors.Is(err, errs.ErrStuck) {
			t.Fatalf("seed %d: Append when every sync fails: %v", seed, err)
		}
		a.Unlock()
		a.Close()
		d.Clear()
		rec := &recorder{}
		next, err := Open(d.FS(), crashPath, rec, crashOptions)
		if err != nil {
			t.Fatalf("seed %d: opening after the stuck Log closed: %v", seed, err)
		}
		rec.holds(t, lists[:4]...)
		commit(t, next, lists[4])
		next.Close()
		d.Cut()
		rec = &recorder{}
		after, err := Open(d.FS(), crashPath, rec, crashOptions)
		if err != nil {
			t.Fatalf("seed %d: after a power cut: %v", seed, err)
		}
		after.Close()
		rec.holds(t, lists...)
	}
}

// TestAStuckHandleKeepsTheLock: when a commit fails and its cut back fails
// too, the Log keeps the write lock until Close. Append's error wraps the
// call's and errs.ErrStuck, and says the commit's outcome is unknown. Every
// Append after it, under the same lock, and every Lock gives errs.ErrStuck,
// Lock at once. Another Log's Lock waits for the lock meanwhile, and times
// out. Close lets go of it, and the other Log's check deals with what the
// stuck one left, as with what a crash leaves: the failed batch, whole,
// when the cut back stopped before the cut, which counts and is marked,
// since a failed commit's outcome is unknown; or nothing, when the batch
// never reached the file or the cut was made and only its sync failed.
func TestAStuckHandleKeepsTheLock(t *testing.T) {
	for _, c := range []struct {
		name   string
		fail   []int // the calls that fail, counting the commit's and then the cut back's writes, syncs and truncates together from 1
		marked bool  // the other Log's check marks the failed batch
	}{
		{"the sync, then the marker written again", []int{2, 3}, true},
		{"the sync, then the sync before the cut", []int{2, 4}, true},
		{"the sync, then the cut", []int{2, 5}, true},
		{"the sync, then the cut's sync", []int{2, 6}, false},
		{"the batch's write, then the marker written again", []int{1, 2}, false},
		{"the marker's write, then the cut", []int{3, 6}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "db")
			s := &spy{}
			a, err := Open(spyOn(s), path, &recorder{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			closeAtEnd(t, a)
			commit(t, a, table("one"))
			const wait = 200 * time.Millisecond
			b, rb := openLog(t, path, Options{Wait: wait})

			if err := a.Lock(); err != nil {
				t.Fatal(err)
			}
			s.fail = failCalls(c.fail...) // the commit's call with errDrive, the cut back's with errAgain
			err = a.Append(table("two"))
			if !errors.Is(err, errs.ErrStuck) || !errors.Is(err, errDrive) || !errors.Is(err, errAgain) || !strings.Contains(err.Error(), "outcome is unknown") {
				t.Fatalf("Append when the commit and its cut back fail: %v", err)
			}
			s.fail = nil
			if err := a.Append(table("three")); !errors.Is(err, errs.ErrStuck) || !errors.Is(err, errAgain) {
				t.Errorf("a second Append under the same lock: %v", err)
			}
			if err := a.Unlock(); err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			if err := lockErr(a); !errors.Is(err, errs.ErrStuck) || !errors.Is(err, errAgain) {
				t.Errorf("Lock on the stuck Log: %v", err)
			}
			if took := time.Since(began); took > wait/2 {
				t.Errorf("Lock on the stuck Log took %v, where it fails at once", took)
			}
			began = time.Now()
			if err := lockErr(b); !errors.Is(err, errs.ErrLockTimeout) {
				t.Fatalf("another Log's Lock while the first is stuck: %v", err)
			}
			if took := time.Since(began); took < wait {
				t.Errorf("another Log's Lock gave up after %v, before its wait of %v was over", took, wait)
			}

			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			commit(t, b, table("four"))
			lists := [][]format.Change{table("one")}
			if c.marked {
				lists = append(lists, table("two"))
			}
			rb.holds(t, lists...)
			reread(t, path).holds(t, append(lists, table("four"))...)
		})
	}
}

// TestAStuckHandleBlocksOtherProcesses: a helper process's commit fails, and
// so does its cut back, since every sync fails from that commit on, so its
// Log is stuck. While the helper keeps the Log open, this process's writer
// waits for the lock and times out, each time it tries. Once the helper
// closes its Log, with its process still running, or once the process is
// killed, the lock comes free, and this process's next commit goes in. Its
// check finds the helper's failed batch whole, and marks it, since a failed
// commit's outcome is unknown.
func TestAStuckHandleBlocksOtherProcesses(t *testing.T) {
	for _, ending := range []string{"closed", "killed"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "db")
			const wait = 200 * time.Millisecond
			l, rec := openLog(t, path, Options{Wait: wait})
			commit(t, l, table("one"))
			h := start(t, "stick", path)
			if s := h.line(t); s != "stuck" {
				t.Fatalf("the helper said %q", s)
			}
			for range 2 {
				began := time.Now()
				if err := lockErr(l); !errors.Is(err, errs.ErrLockTimeout) {
					t.Fatalf("Lock while another process's Log is stuck: %v", err)
				}
				if took := time.Since(began); took < wait {
					t.Errorf("Lock gave up after %v, before its wait of %v was over", took, wait)
				}
			}
			if ending == "closed" {
				fmt.Fprintln(h.in, "close")
				if s := h.line(t); s != "closed" {
					t.Fatalf("the helper said %q", s)
				}
			} else {
				h.cmd.Process.Kill()
				h.cmd.Wait()
			}
			commit(t, l, table("mine")) // while a closing helper is still running
			rec.holdsFrom(t, 2, helperChanges)
			if ending == "closed" {
				h.end(t)
			}
			reread(t, path).holds(t, table("one"), helperChanges, table("mine"))
		})
	}
}
