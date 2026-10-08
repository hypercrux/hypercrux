// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"fmt"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// The glue between the file and the in-memory copy (G1). A call goes like
// this:
//
//   - A read through db loads the copy and reads it inside the copy's Read,
//     which holds the copy's lock shared, and copies out what it returns.
//   - A write through db is an Update of its own.
//   - Update checks that it isn't running inside an Update on the same
//     goroutine (the copy's Outside), then takes the log's write lock,
//     which reads what other processes have committed into the copy first.
//     It begins the copy's transaction, runs fn, and commits: the copy hands
//     its change list to the log's Append, which writes the batch, syncs and
//     writes the marker. Then it lets go of the lock. If fn fails or panics,
//     or Append fails, the copy's undo list puts the copy back as it was.
//
// The locks come in the order S2 settled: the log's write lock, then the
// copy's transaction, then the copy's own lock at the transaction's first
// change. Readers take the copy's lock alone, and nothing takes the log's
// lock while it holds one of the copy's.

// target is the logfile.Target that every marked batch the log reads goes
// to: the copy. While the log opens the file, nobody else can reach the
// copy, so a batch goes straight in (LoadBatch). After that, readers can,
// so each batch goes in through a transaction of its own (ApplyBatch),
// whole or not at all, and readers see all of it or none. A batch that
// breaks the rules for the state it applies to is damage, and the store
// says so with a *errs.Damage naming the batch, which Apply returns as it
// is.
//
// Reset puts an empty copy in place of the old one, and the new file's
// batches go into it after that. Readers in this process can see the new
// copy fill, batch by batch, until the log has read the new file. Task F9
// makes them wait instead.
type target struct {
	db      *DB
	opening bool // logfile.Open is reading the file, before db is handed out
}

func (t *target) Apply(seq uint64, changes []format.Change) error {
	s := t.db.mem.Load()
	if t.opening || plant == "hypercrux/load-after-open" {
		return s.LoadBatch(seq, changes)
	}
	return s.ApplyBatch(seq, changes)
}

func (t *target) Reset() {
	if plant == "hypercrux/reset-kept" {
		return
	}
	t.db.mem.Store(store.New())
}

// open opens the database at path through the file calls files, the real
// ones or a test's fault layer, with the log's options o. Open checks the
// path's rules first.
func open(files fsys.FS, path string, o logfile.Options) (*DB, error) {
	db := &DB{path: path}
	db.mem.Store(store.New())
	t := &target{db: db, opening: true}
	// F7's rules for the file, its real path and a file with more than one
	// name refused, go in the log's Open.
	l, err := logfile.Open(files, path, t, o)
	if err != nil {
		return nil, err
	}
	// From here on the log hands batches over only from Lock, in a call
	// made through db once Open has handed it out.
	t.opening = false
	db.log = l
	return db, nil
}

// current returns the in-memory copy, or an error that wraps ErrClosed
// once the database is closed.
func (db *DB) current() (*store.Store, error) {
	if s := db.mem.Load(); s != nil {
		return s, nil
	}
	if db.path == "" {
		return nil, fmt.Errorf("%w: a database that was never opened", ErrClosed)
	}
	return nil, fmt.Errorf("%w: %s", ErrClosed, db.path)
}
