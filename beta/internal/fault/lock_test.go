// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// TestFlockAcrossOpens checks the flock against Linux's rules: it belongs
// to an open file, so two opens contend for it whether one process made
// them or two; the open that holds it can take it again; it stays with the
// file through a rename and a removal, and a new file at the old path has
// one of its own; Unlock from an open that doesn't hold it changes
// nothing; Close lets go of it, and so does a cut, once the names are
// synced so the file is still there.
func TestFlockAcrossOpens(t *testing.T) {
	d := fault.New(1)
	one, two := d.FS(), d.FS()
	open := func(sys fsys.FS, name string) fsys.File {
		t.Helper()
		f, err := sys.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	lock := func(f fsys.File, want bool) {
		t.Helper()
		if ok, err := f.TryLock(); ok != want || err != nil {
			t.Fatalf("TryLock gave %v and %v, where it should give %v", ok, err, want)
		}
	}
	unlock := func(f fsys.File) {
		t.Helper()
		if err := f.Unlock(); err != nil {
			t.Fatal(err)
		}
	}

	a, err := one.Create("/db", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	b, c := open(two, "/db"), open(one, "/db")
	lock(a, true)
	lock(b, false)
	lock(c, false)
	lock(a, true)
	unlock(b)
	lock(b, false)
	unlock(a)
	lock(b, true)
	lock(a, false)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	lock(c, true)
	lock(a, false)

	if err := one.Rename("/db", "/old"); err != nil {
		t.Fatal(err)
	}
	moved := open(two, "/old")
	lock(moved, false)
	fresh, err := two.Create("/db", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	lock(fresh, true)
	lock(open(one, "/db"), false)
	if err := two.Remove("/old"); err != nil {
		t.Fatal(err)
	}
	lock(moved, false)
	unlock(c)
	lock(moved, true)

	if err := one.SyncDir("/"); err != nil {
		t.Fatal(err)
	}
	d.Cut()
	sys := d.FS()
	again := open(sys, "/db")
	lock(again, true)
	lock(open(sys, "/db"), false)
}

// TestFlockKeepsGoroutinesApart has goroutines, each with an open file of
// its own, take the flock and let go of it over and over, counting the
// holders, of whom there must never be two. It's for the race detector
// too.
func TestFlockKeepsGoroutinesApart(t *testing.T) {
	d := fault.New(1)
	sys := d.FS()
	if _, err := sys.Create("/lock", 0o644); err != nil {
		t.Fatal(err)
	}
	var holders, taken atomic.Int32
	var shared atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		f, err := sys.Open("/lock")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				ok, err := f.TryLock()
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					continue
				}
				taken.Add(1)
				if holders.Add(1) > 1 {
					shared.Store(true)
				}
				holders.Add(-1)
				if err := f.Unlock(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if shared.Load() || taken.Load() == 0 {
		t.Errorf("two holders at once: %v; the lock was taken %d times", shared.Load(), taken.Load())
	}
}
