// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// The glue between the file and the in-memory copy (G1). A call goes like
// this:
//
//   - A read through db first brings the copy up to date with what other
//     processes have committed (db.follow): the log's Follow, and when
//     another file has taken the path, a compaction's or a backup moved into
//     place, the log's Reload, then Follow again (F9). Then it loads the copy
//     and reads it inside the copy's Read (db.read), which holds the copy's
//     lock shared, and copies out what it returns. A read through a Tx reads
//     the copy's transaction, which sees the Update's changes, and copies out
//     the same way; it needn't follow, since its Update caught up under the
//     write lock.
//   - A write through db is an Update of its own.
//   - Update checks that it isn't running inside an Update on the same
//     goroutine (the copy's Outside), then takes the log's write lock,
//     which reads what other processes have committed into the copy first.
//     It begins the copy's transaction, runs fn, and commits: the copy hands
//     its change list to the log's Append, which writes the batch, syncs and
//     writes the marker. Then it lets go of the lock. If fn fails or panics,
//     or Append fails, the copy's undo list puts the copy back as it was.
//
// The locks come in the order S2 and F6 settled: the log's fol, then its
// mutex and flock, then the copy's transaction, then the copy's own lock at
// the transaction's first change. A read follows before it takes any lock of
// the copy's, so a batch Follow applies never waits for the read's own hold
// on the copy. A reload holds the gate alone (target), from the Target's
// Reset until the log has read the new file, holding the log's mutex; reads
// hold it shared, around their look at the copy and nothing else. Nothing
// takes the log's locks while it holds the gate shared, or the copy's locks.

// target is the logfile.Target that every marked batch the log reads goes
// to: the copy. While the log opens the file, nobody else can reach the
// copy, so a batch goes straight in (LoadBatch). After that, readers can,
// so each batch goes in through a transaction of its own (ApplyBatch),
// whole or not at all, and readers see all of it or none. A batch that
// breaks the rules for the state it applies to is damage, and the store
// says so with a *errs.Damage naming the batch, which Apply returns as it
// is.
//
// When another file has taken the path, the log reloads (F9): it calls Reset,
// runs a garbage collection, hands over the new file's batches, and calls
// Loaded once it has read and checked the new file. Reset takes the gate
// alone, which waits for the reads under way to finish on the old copy and
// holds new ones off, and puts an empty copy in place of the old one, which
// then has nothing left to keep it, so the log's collection takes it. Until
// Loaded, the batches go straight into the new copy, which no reader can
// reach, as they do while the log opens the file, which saves a reload about
// a quarter of its time. Loaded lets the readers in. When the reload failed,
// the copy is left as the batches the log handed over before the failure
// left it, and since they went straight in, a batch that broke the rules can
// be in it in part. No read sees it: each gives the reload's error until a
// reload works (unread), and an Update's Lock reads the file again first.
type target struct {
	db      *DB
	opening bool // logfile.Open is reading the file, before db is handed out
	held    bool // a reload holds the gate, from Reset until Loaded

	kept *store.Store // the old copy, kept through a reload only by a planted bug
}

var _ logfile.Reloader = (*target)(nil)

func (t *target) Apply(seq uint64, changes []format.Change) error {
	s := t.db.mem.Load()
	if t.opening || t.held || plant == "hypercrux/load-after-open" {
		return s.LoadBatch(seq, changes)
	}
	return s.ApplyBatch(seq, changes)
}

func (t *target) Reset() {
	if plant == "hypercrux/reset-kept" {
		return
	}
	if !t.opening && !t.held && plant != "hypercrux/reload-unheld" {
		t.db.gate.Lock()
		t.held = true
	}
	if plant == "hypercrux/reload-keeps-the-old-copy" {
		t.kept = t.db.mem.Load()
	}
	t.db.mem.Store(store.New())
}

func (t *target) Loaded(err error) {
	t.kept = nil
	if !t.held {
		return
	}
	t.db.unread = err
	t.held = false
	t.db.gate.Unlock()
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
	// From here on the log hands batches over only from Lock, Follow and
	// Reload, in a call made through db once Open has handed it out.
	t.opening = false
	db.log = l
	// The database/sql handle that SQL gives, over connections bound to db,
	// and what runs the statements that parse until task G4 brings SQL.
	db.sql = sql.OpenDB(connector{db: db})
	db.engine = notYetEngine{}
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

// maxReloads bounds follow's loop. Each turn reloads, or finds the mutex
// held by an Update whose Lock moves to the new file by itself, so it ends
// at once unless other files keep taking the path.
const maxReloads = 100

// follow brings the copy up to date with the file, for a read through db:
// it hands the copy what other processes have committed since (the log's
// Follow), and when another file has taken the path, reads that one from its
// start (the log's Reload), then follows again. It never waits for an
// Update: while one holds the log's mutex, Follow and Reload return at once,
// and the Update reads on itself. An error is the read's: damage, a lock
// timeout from a wait to read again what looked like damage, a failed read,
// or the reload's error.
func (db *DB) follow() error {
	if plant == "hypercrux/reads-never-follow" {
		return nil
	}
	for range maxReloads {
		err := db.log.Follow()
		if !errors.Is(err, logfile.ErrReplaced) {
			return err
		}
		if err := db.log.Reload(); err != nil {
			return err
		}
	}
	return fmt.Errorf("hypercrux: %s: other files kept taking the database's path", db.path)
}

// read runs fn on the in-memory copy inside the copy's Read, for a read
// through db: fn sees one point in the log, and copies out what it keeps.
// It follows the file first (follow), outside every lock of the copy's, and
// then holds the gate shared, so it never sees a copy a reload is filling.
// It fails with an error that wraps ErrClosed once the database is closed,
// with the error of a reload that failed until one works, and, inside an
// Update after its first change, with ErrInsideUpdate at once, since Read
// would wait for that Update.
func (db *DB) read(fn func(r store.Reader) error) error {
	if _, err := db.current(); err != nil {
		return err
	}
	if err := db.follow(); err != nil {
		return err
	}
	db.gate.RLock()
	defer db.gate.RUnlock()
	s, err := db.current()
	if err != nil {
		return err
	}
	if db.unread != nil && plant != "hypercrux/failed-reload-read" {
		return db.unread
	}
	return s.Read(fn)
}
