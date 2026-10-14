// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"fmt"
	"iter"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/query"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/internal/export"
)

// OpenWith opens the database at path through the file calls files, with
// the log's options o, as Open does through the real ones. The tests use it
// for the fault layer's disk, and for a short wait for the write lock.
func OpenWith(files fsys.FS, path string, o logfile.Options) (*DB, error) {
	return open(files, path, o)
}

// Engine runs the statements that parse, for the driver's tests, in place
// of the Beta's engine: Query runs a SELECT, giving its columns and its
// rows, and Write runs an INSERT, UPDATE or DELETE, giving the count of
// rows it changed. Each is handed what the driver hands the engine (the
// engine interface in driver.go). A nil func leaves that kind of statement
// to the Beta's engine.
type Engine struct {
	Query func(r store.Reader, s *query.Select, args []value.Value) ([]string, query.Rows, error)
	Write func(tx *store.Tx, s query.Statement, args []value.Value) (int64, error)

	db *DB // the database UseEngine gave it to
}

func (e Engine) query(r store.Reader, s *query.Select, args []value.Value) ([]string, query.Rows, error) {
	if e.Query == nil {
		return sqlEngine{e.db}.query(r, s, args)
	}
	return e.Query(r, s, args)
}

func (e Engine) write(tx *store.Tx, s query.Statement, args []value.Value) (int64, error) {
	if e.Write == nil {
		return sqlEngine{e.db}.write(tx, s, args)
	}
	return e.Write(tx, s, args)
}

// UseEngine has db run the statements that parse through e from now on,
// through SQL() and the methods alike. A test calls it before it hands db
// to anything else.
func UseEngine(db *DB, e Engine) {
	e.db = db
	db.engine = e
}

// UseClock has db read the moment for each statement, and for each search's
// filter, from now on. A test calls it before it hands db to anything else.
func UseClock(db *DB, now func() time.Time) { db.clock = now }

// ReadTheCopy runs fn on db's copy as it is, inside the copy's Read, without
// following the file and whatever a reload left, for a test that looks at
// what a read through db doesn't show: the copy after damage, or after a
// reload that failed.
func ReadTheCopy(db *DB, fn func(r store.Reader) error) error {
	s, err := db.current()
	if err != nil {
		return err
	}
	return s.Read(fn)
}

// UseSnapshot has every compaction write what f gives of a read of the copy,
// in place of the store's own snapshot, until the func it returns puts that
// back. The tests that use it run one at a time.
func UseSnapshot(f func(r store.Reader) iter.Seq[format.Change]) (restore func()) {
	old := snapshotOf
	snapshotOf = f
	return func() { snapshotOf = old }
}

// ImportItems puts tables, records and links into db as Import does, in one
// Update, as an export's reader hands them over, each counted as the line
// it would be on after an export's first line, for a test whose values are
// too long to write out as an export (L1). The reader takes a line of any
// length, and Import's other checks come before the first table, so a value
// too long meets the importer's checks here as it would in Import.
func ImportItems(db *DB, items ...any) error {
	return db.Update(func(tx *Tx) error {
		im := importer{tx: tx.stx}
		for i, item := range items {
			var err error
			switch x := item.(type) {
			case export.Table:
				err = im.table(x)
			case export.Record:
				err = im.record(x)
			case export.Link:
				err = im.link(x)
			default:
				err = fmt.Errorf("ImportItems was given a %T, where it takes export.Table, export.Record and export.Link", item)
			}
			if err != nil {
				return lineError(i+2, err)
			}
		}
		return nil
	})
}

// Pending gives what the log's Due weighs for db (C1), as db last read the
// file: the bytes the commits since the file's compacted part have made
// dead, as the copy counts them, and the commits past the compacted part. It
// takes no lock, so a test calls it on the goroutine that has db to itself:
// between its calls, once a read has followed the file, or inside a
// compaction db is making, from the func UseSnapshot gives, where the
// commit that set the compaction off is in what it weighs.
func Pending(db *DB) (dead int64, commits uint64) {
	return db.log.Dead(), db.log.Commits()
}
