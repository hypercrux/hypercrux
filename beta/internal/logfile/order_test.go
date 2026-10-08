// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The follower beside Update, with the real store as the Target, joined to
// the log as the public package joins them (G1), and with reads that follow
// first, as F9 makes them. It pins the lock order follow.go sets out.

// glue is the public package's target in small: while Open reads the file,
// nobody else can reach the copy, so a batch goes straight in, and after
// that each goes in through a transaction of its own.
type glue struct {
	s       *store.Store
	opening bool
}

func (g *glue) Apply(seq uint64, changes []format.Change) error {
	if g.opening {
		return g.s.LoadBatch(seq, changes)
	}
	return g.s.ApplyBatch(seq, changes)
}

func (g *glue) Reset() { panic("no other file takes the path in this test") }

// handle is a process's open database in small: its log and its copy.
type handle struct {
	l *Log
	s *store.Store
}

func openHandle(t *testing.T, path string) *handle {
	t.Helper()
	g := &glue{s: store.New(), opening: true}
	l, err := Open(fsys.OS{}, path, g, Options{})
	if err != nil {
		t.Fatal(err)
	}
	g.opening = false
	closeAtEnd(t, l)
	return &handle{l: l, s: g.s}
}

// update is the public package's Update: the copy's Outside, the log's
// Lock, the copy's Begin, fn, and Commit with the log's Append, then Unlock.
func (h *handle) update(fn func(tx *store.Tx) error) (err error) {
	if err := h.s.Outside(); err != nil {
		return err
	}
	if err := h.l.Lock(); err != nil {
		return err
	}
	defer func() {
		if e := h.l.Unlock(); err == nil {
			err = e
		}
	}()
	tx, err := h.s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(h.l.Append)
}

// read is a read through the database as F9 makes it: Follow, then the
// copy's Read.
func (h *handle) read(fn func(r store.Reader) error) error {
	if err := h.l.Follow(); err != nil {
		return err
	}
	return h.s.Read(fn)
}

// counted is how many of the records table:1, table:2 and on r holds, with
// none missing before the last, which is how one writer commits them.
func counted(r store.Reader, table string) (int, error) {
	n := 0
	for ; ; n++ {
		if _, err := r.Get(fmt.Sprintf("%s:%d", table, n+1)); errors.Is(err, errs.ErrNotFound) {
			break
		} else if err != nil {
			return 0, err
		}
	}
	for k := n + 2; k <= n+4; k++ {
		if _, err := r.Get(fmt.Sprintf("%s:%d", table, k)); !errors.Is(err, errs.ErrNotFound) {
			return 0, fmt.Errorf("%s:%d is there, and %s:%d isn't", table, k, table, n+1)
		}
	}
	return n, nil
}

// TestFollowingBesideUpdates: one process's handle commits while another's
// runs Updates on two goroutines and reads that follow on two more, all at
// once, with nothing waiting for ever. Each Update reads through the
// database before its first change, and goes on to read the copy; after its
// first change, such a read fails at once with errs.ErrInsideUpdate. Inside
// an Update the file can't grow, so Follow's look without the mutex finds
// nothing new. So every other Update first writes a few stray bytes past the
// end of the log, which its own commit then writes over, and its read finds
// the mutex its own Update holds, and goes on. The readers see each
// writer's records whole and in order, as many as before or more, and at the
// end the second handle, and a fresh one, hold every record.
func TestFollowingBesideUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	a, b := openHandle(t, path), openHandle(t, path)
	n := 40
	if testing.Short() {
		n = 15
	}
	put := func(tx *store.Tx, key string, i int) error {
		return tx.Put(key, []format.Field{{Name: "n", Value: value.Int(int64(i))}})
	}
	errc := make(chan error, 16)
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 1; i <= n; i++ {
			if err := a.update(func(tx *store.Tx) error { return put(tx, fmt.Sprintf("a:%d", i), i) }); err != nil {
				errc <- fmt.Errorf("the other process's commit %d: %w", i, err)
				return
			}
		}
	}()
	for g := range 2 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 1; i <= n; i++ {
				err := b.update(func(tx *store.Tx) error {
					if i%2 == 0 {
						// Holding the lock, b's log ends where the file does.
						if _, err := b.l.f.WriteAt(make([]byte, 10), b.l.end); err != nil {
							return err
						}
					}
					if err := b.read(func(r store.Reader) error { _, err := counted(r, "a"); return err }); err != nil {
						return fmt.Errorf("a read through the database before the first change: %w", err)
					}
					if err := put(tx, fmt.Sprintf("b%d:%d", g, i), i); err != nil {
						return err
					}
					if err := b.read(func(store.Reader) error { return nil }); !errors.Is(err, errs.ErrInsideUpdate) {
						return fmt.Errorf("a read through the database after the first change gave %v", err)
					}
					return nil
				})
				if err != nil {
					errc <- fmt.Errorf("update %d on goroutine %d: %w", i, g, err)
					return
				}
			}
		}()
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	reads := make([]int, 2)
	for k := range 2 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			last := map[string]int{}
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := b.read(func(r store.Reader) error {
					for _, table := range []string{"a", "b0", "b1"} {
						got, err := counted(r, table)
						if err != nil {
							return err
						}
						if got < last[table] {
							return fmt.Errorf("a read found %d of %s's records, after one that found %d", got, table, last[table])
						}
						last[table] = got
					}
					return nil
				})
				if err != nil {
					errc <- err
					return
				}
				reads[k]++
			}
		}()
	}
	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		stacks := make([]byte, 1<<20)
		stacks = stacks[:runtime.Stack(stacks, true)]
		panic(fmt.Sprintf("the writers were still at work 30 seconds in, so something waits for ever:\n%s", stacks))
	}
	close(stop)
	readers.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	fresh := openHandle(t, path)
	for _, h := range []*handle{b, fresh} {
		err := h.read(func(r store.Reader) error {
			for _, table := range []string{"a", "b0", "b1"} {
				if got, err := counted(r, table); err != nil || got != n {
					return fmt.Errorf("%d of %s's records, with %v, where %d are wanted", got, table, err, n)
				}
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	}
	t.Logf("%d commits, and %d and %d reads that followed", 3*n, reads[0], reads[1])
}
