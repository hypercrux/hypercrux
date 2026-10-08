// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/query"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// OpenWith opens the database at path through the file calls files, with
// the log's options o, as Open does through the real ones. The tests use it
// for the fault layer's disk, and for a short wait for the write lock.
func OpenWith(files fsys.FS, path string, o logfile.Options) (*DB, error) {
	return open(files, path, o)
}

// Engine runs the statements that parse, for the driver's tests, in place
// of the engine task G4 writes: Query runs a SELECT, giving its columns and
// its rows, and Write runs an INSERT, UPDATE or DELETE, giving the count of
// rows it changed. Each is handed what the driver hands the engine (the
// engine interface in driver.go). A nil func gives the stub's error, as the
// engine does until G4.
type Engine struct {
	Query func(r store.Reader, s *query.Select, args []value.Value) ([]string, query.Rows, error)
	Write func(tx *store.Tx, s query.Statement, args []value.Value) (int64, error)
}

func (e Engine) query(r store.Reader, s *query.Select, args []value.Value) ([]string, query.Rows, error) {
	if e.Query == nil {
		return notYetEngine{}.query(r, s, args)
	}
	return e.Query(r, s, args)
}

func (e Engine) write(tx *store.Tx, s query.Statement, args []value.Value) (int64, error) {
	if e.Write == nil {
		return notYetEngine{}.write(tx, s, args)
	}
	return e.Write(tx, s, args)
}

// UseEngine has db run the statements that parse through e from now on,
// through SQL() and the methods alike. A test calls it before it hands db
// to anything else.
func UseEngine(db *DB, e Engine) { db.engine = e }

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
