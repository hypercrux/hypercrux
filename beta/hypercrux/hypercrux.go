// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// Version is the version of the package and of the hypercrux command. The
// Beta hasn't been released, so it's 0.0.0-beta, a pre-release version that
// sorts before every release. Task R4 sets the version the Beta is released
// as.
//
// The release workflow reads only the Version in the hypercrux.go at the
// root of the repository, which is 0.x's. So nothing in beta/ can set off a
// release, and raising this one releases nothing.
const Version = "0.0.0-beta"

// FormatVersion is the version of the file format that beta/FORMAT.md
// describes. Opening a file of another format version fails with an error
// that wraps ErrFormatVersion. Until the Beta's release every change to the
// format raises the version, so a build also refuses an older build's files.
const FormatVersion = 1

// The error values. ErrNotFound and ErrInvalid are 0.x's, with 0.x's texts.
// The rest are the Beta's own, for what 0.x reports with a plain error. Each
// is the same value the Beta's internal packages return, so errors.Is finds
// it in an error from any layer.
var (
	// ErrNotFound means a key, table or link doesn't exist.
	ErrNotFound = errs.ErrNotFound
	// ErrInvalid means an argument breaks HyperCrux's rules, such as a key
	// without a table name or a vector of the wrong size.
	ErrInvalid = errs.ErrInvalid

	// ErrDamaged means the file is damaged. An error that wraps it may
	// carry a *Damage, which says where.
	ErrDamaged = errs.ErrDamaged
	// ErrNotDatabase means the file at the path isn't a HyperCrux database.
	ErrNotDatabase = errs.ErrNotDatabase
	// ErrZeroX means the file is a HyperCrux 0.x database, which the Beta
	// can't open. hypercrux export in 0.x and hypercrux import in the Beta
	// move a database across.
	ErrZeroX = errs.ErrZeroX
	// ErrFormatVersion means the file's format version is one this build
	// doesn't read.
	ErrFormatVersion = errs.ErrFormatVersion
	// ErrLockTimeout means the write lock didn't come free in time: 10
	// seconds, as in 0.x, or until a running compaction ends.
	ErrLockTimeout = errs.ErrLockTimeout
	// ErrStuck means this handle refuses every write until it's closed,
	// since a failed write couldn't be undone in the file. Until then no
	// process can write to the database.
	ErrStuck = errs.ErrStuck
	// ErrInsideUpdate means a call went through the database inside its own
	// Update, where it would have to wait for that Update. The call belongs
	// on the transaction.
	ErrInsideUpdate = errs.ErrInsideUpdate
	// ErrClosed means a call on a database after Close, or on a transaction
	// after its Update returned.
	ErrClosed = errs.ErrClosed
)

// Damage says where a file is damaged and what's wrong there. An error that
// wraps ErrDamaged may carry one, and errors.As finds it.
type Damage = errs.Damage

// notYet is a stub's error, returned until the task it names makes the call
// work. It wraps errors.ErrUnsupported, so errors.Is tells a stub from a
// failure, and it wraps neither of 0.x's two errors, so the differential
// harness counts it as a plain error.
func notYet(what, task string) error {
	return fmt.Errorf("hypercrux: %s is an %w in the Beta until task %s", what, errors.ErrUnsupported, task)
}

// DB is an open HyperCrux database. It's safe for use by many goroutines,
// and many processes on one machine can open the same file at once. Each
// process holds the whole database in memory and keeps up with what the
// others commit.
type DB struct {
	path string
	// mem is the in-memory copy, or nil once the database is closed, and in
	// a DB that was never opened. The log's Reset puts a new one in its
	// place (target), so a call loads it once and works on what it loaded.
	mem atomic.Pointer[store.Store]
	// log is the file, with the write lock.
	log *logfile.Log
}

// Open opens the database at path, creating it if it doesn't exist, and
// reads the whole file into memory. As in 0.x, path has to name a file: an
// empty name, a name holding ? or #, :memory: and names starting with file:
// are refused with an error that wraps ErrInvalid. The database is opened by
// its real path, with symbolic links resolved, and a file with more than one
// hard link is refused.
//
// A file that isn't a HyperCrux database fails with an error that wraps
// ErrNotDatabase, a 0.x database with ErrZeroX, a format version this build
// doesn't read with ErrFormatVersion, and a damaged file with ErrDamaged.
func Open(path string) (*DB, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("%w: file name %q", ErrInvalid, path)
	}
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("%w: %s: HyperCrux needs a file name; for a throwaway database, use a file in a temporary folder", ErrInvalid, path)
	}
	return open(fsys.OS{}, path, logfile.Options{})
}

// Close closes the database. A call through it after that fails with an
// error that wraps ErrClosed, and closing it again does nothing. Close waits
// for an Update under way in another goroutine to commit or roll back.
// Inside an Update, Close fails at once with an error that wraps
// ErrInsideUpdate, since it would wait for that Update.
func (db *DB) Close() error {
	s := db.mem.Load()
	if s == nil {
		return nil
	}
	if err := s.Outside(); err != nil {
		return err
	}
	// The log's Close waits for the write lock's mutex, so for any Update
	// under way, and once it has closed, every Update fails at the lock. So
	// nothing reaches the copy through the log after this.
	err := db.log.Close()
	db.mem.Store(nil) // reads fail from now on, and the copy can go
	if errors.Is(err, ErrClosed) {
		return nil // another goroutine's Close got there first
	}
	return err
}

// Path returns the file name the database was opened with.
func (db *DB) Path() string { return db.path }

// SQL returns a database/sql handle on the database, for prepared statements
// and anything else the methods don't cover. It takes the Beta's SQL, which
// beta/SQL.md sets out, through a driver of the Beta's own, and writes
// through it keep the same rules as Put and Delete. Transactions go through
// Update, so its Begin returns an error.
func (db *DB) SQL() *sql.DB { return stubSQL() }

// Tx is a write transaction, passed to the function given to Update. It has
// DB's methods for records, links, vectors and SQL, and everything done
// through it commits together or not at all. Once the function returns, a
// call through it fails with an error that wraps ErrClosed.
type Tx struct {
	db   *DB       // the database the transaction is on
	stx  *store.Tx // the copy's transaction, which holds the changes
	done bool      // Update has returned
}

// open returns the copy's transaction, or an error that wraps ErrClosed
// once Update has returned, and on a Tx that no Update made.
func (t *Tx) open() (*store.Tx, error) {
	if t.stx == nil || t.done {
		return nil, fmt.Errorf("%w: a transaction used outside its Update", ErrClosed)
	}
	return t.stx, nil
}

// Update runs fn in one write transaction. If fn returns an error or
// panics, nothing it did is kept. Otherwise the transaction commits, and
// every key, field, link and vector it changed changes together.
//
// Update takes the write lock first. It waits up to 10 seconds for it, as
// 0.x does, or longer while a compaction runs, and then fails with an error
// that wraps ErrLockTimeout. Until fn's first change, other goroutines in
// this process read as usual. From then until the commit they wait, while
// other processes read throughout.
//
// Inside fn, make every call through tx. A call through db would have to
// wait for fn if it's a write, or a read after fn's first change, so such a
// call fails at once with an error that wraps ErrInsideUpdate. A call
// through db from a goroutine that fn starts and then waits for can't be
// caught that way: a write waits for the write lock and fails once the
// wait is over, and a read after fn's first change, or Close, hangs.
//
// When the commit itself fails, the database in memory goes back to how it
// was before fn, and the commit is cut back out of the file for good. If
// the file can't be put back, the error wraps ErrStuck and the commit's
// outcome is unknown, and every later write through db fails with an error
// that wraps ErrStuck until db is closed, while other handles and processes
// wait for the write lock.
func (db *DB) Update(fn func(tx *Tx) error) (err error) {
	s, err := db.current()
	if err != nil {
		return err
	}
	if err := s.Outside(); err != nil {
		return err
	}
	// The write lock. Lock reads on to the end of the log first, so the
	// copy holds every commit before fn runs, and a read in fn followed by
	// a write can't lose another process's commit.
	if err := db.log.Lock(); err != nil {
		return err
	}
	defer func() {
		// The lock goes last, after the copy's transaction has ended, and
		// on a panic too.
		if e := db.log.Unlock(); e != nil && err == nil {
			err = fmt.Errorf("hypercrux: %s: letting go of the write lock after the Update failed: %w", db.path, e)
		}
	}()
	// Lock may have put a new copy in place, when another file had taken
	// the path, so the transaction begins on the copy there now.
	stx, err := db.mem.Load().Begin()
	if err != nil {
		return err
	}
	defer stx.Rollback() // does nothing once Commit has ended it
	tx := &Tx{db: db, stx: stx}
	defer func() { tx.done = true }()
	if err := fn(tx); err != nil {
		return err
	}
	// Commit hands the change list to Append with the copy still locked,
	// so readers here see the changes only once they're in the file. When
	// Append fails, Commit takes them back with the undo list. A transaction
	// with no changes writes nothing.
	return stx.Commit(db.write)
}

// write writes a commit's changes to the file, as Commit's write: the
// batch, a sync and the marker.
func (db *DB) write(changes []format.Change) error {
	err := db.log.Append(changes)
	if plant == "hypercrux/append-error-dropped" {
		return nil
	}
	return err
}

// Compact rewrites the file with only the live data, which also takes
// deleted data off the disk, as VACUUM does in 0.x. The database compacts
// itself at the end of a commit once the file holds about twice as much as
// its live data, and Compact does it at once. Writers wait while it runs,
// in this process and in others, and other processes then reload the file.
func (db *DB) Compact() error { return notYet("DB.Compact", "G5") }
