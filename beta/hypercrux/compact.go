// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"fmt"
	"iter"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// Compaction through the public package (G5): Compact on demand, and the
// compaction after a commit, which Update runs once the log's Due says one
// is due. Both go the way F8 set out. Holding the log's write lock, a read
// of the copy hands its compacted snapshot to the log's Compact, which
// writes it into NAME.compact, each table's vectors in vector blocks (V2),
// syncs it, renames it over the database and syncs the folder. The copy
// holds that state already, so this process reads nothing afresh, and other
// processes read the new file at their next read or Lock (F9).
//
// The lock order holds: the log's mutex and flock, then the copy's lock,
// which Read holds shared. Reads in this process go on during a compaction,
// since it only reads the copy, and their Follow and Reload return at once
// while the mutex is held. Writers in this process and in others wait it
// out, past their usual 10 seconds, until 10 seconds after it ends
// (logfile's until).
//
// What became of the last compaction is kept for Check (failed): its error,
// or nothing once one works. A compaction after a commit never fails its
// Update, so Check is where its error shows. A compaction that fails before
// its switch leaves the database whole in its old file, and the next is due
// once the file has grown as much again. One whose folder sync fails after
// the rename leaves this DB stuck, so every later write fails with an error
// that wraps ErrStuck until the DB is closed.

// Compact rewrites the database's file with only the live data, which also
// takes deleted data off the disk, as VACUUM does in 0.x. It takes the write
// lock, waiting for it as Update does, reads on to the end of the log, and
// writes the copy in memory into a new file beside the database,
// NAME.compact, which it syncs and renames over the database. Writers wait
// while it runs, in this process and in others, past their usual 10 seconds,
// until 10 seconds after it ends. Reads in this process go on meanwhile.
// Other processes read the new file from its start at their next call, which
// takes about as long as opening it.
//
// The database compacts itself as well, at the end of a commit, once the
// file has grown to twice the size its last compaction left it at, and by a
// megabyte at least (Update).
//
// When Compact fails before the new file has taken the path, because the
// disk is full for example, NAME.compact is removed and the database carries
// on in its old file, which is whole. When the folder's sync fails after the
// rename, nobody can tell which file a power cut would leave at the path, so
// the error wraps ErrStuck, and every later write through db fails with
// ErrStuck until db is closed, as after a commit that couldn't be undone in
// the file. Until a compaction works, Check reports the last one's error in
// Report.CompactionError.
//
// Inside an Update, Compact fails at once with an error that wraps
// ErrInsideUpdate, since it would wait for that Update. On a closed database
// it fails with ErrClosed.
func (db *DB) Compact() error {
	s, err := db.current()
	if err != nil {
		return err
	}
	if err := s.Outside(); err != nil {
		return err
	}
	if plant == "hypercrux/compact-stale-snapshot" {
		return db.compactStale(s)
	}
	// Lock reads on to the end of the log first, so the copy holds every
	// commit, other processes' too, and it may move to another file that
	// has taken the path, with a copy of its own.
	if err := db.log.Lock(); err != nil {
		return err
	}
	// As in Update, a failure to let go of flock leaves it held until this
	// DB's next Update or Close, and the compaction stands.
	defer db.log.Unlock()
	return db.compactLocked()
}

// compactLocked compacts the database holding the write lock, from a read of
// the copy, and keeps the outcome for Check.
func (db *DB) compactLocked() error {
	err := db.mem.Load().Read(func(r store.Reader) error { return db.log.Compact(snapshotOf(r)) })
	db.keep(err)
	return err
}

// snapshotOf is what a compaction writes of a read of the copy: the copy's
// compacted snapshot, which gives each table's vectors for vector blocks,
// straight from its arrays (V2). The Reader is the copy itself, as its Read
// hands it over. A test can put another in its place, to make one panic.
var snapshotOf = func(r store.Reader) iter.Seq[format.Change] {
	if plant == "hypercrux/compaction-without-blocks" {
		return r.Snapshot()
	}
	return r.(*store.Store).Compacted()
}

// compactAfter is the compaction after a commit. Update runs it holding the
// write lock, once the commit is in the file, when the log's Due says one is
// due. The commit has succeeded whatever the compaction does, so its error
// is Check's to report, and Update returns nil. A panic while the snapshot is
// read, which the log carries on with once it has removed NAME.compact, is
// kept as the compaction's error too.
func (db *DB) compactAfter() (err error) {
	if !db.log.Due() {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("hypercrux: %s: compacting after a commit panicked, and the database carries on in its old file: %v", db.path, r)
			db.keep(err)
		}
	}()
	return db.compactLocked()
}

// keep notes what became of a compaction, for Check: its error, or nil once
// one has worked.
func (db *DB) keep(err error) {
	if plant == "hypercrux/compaction-error-dropped" {
		return
	}
	if err == nil {
		db.failed.Store(nil)
		return
	}
	db.failed.Store(&err)
}

// compactionError is the error of the last compaction this DB ran, by Compact
// or after a commit, as Check reports it: "" when it worked or none has run.
func (db *DB) compactionError() string {
	if err := db.failed.Load(); err != nil {
		return (*err).Error()
	}
	return ""
}
