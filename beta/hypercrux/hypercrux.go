// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
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
	return nil, notYet("Open", "G1")
}

// Close closes the database. A call through it after that fails with an
// error that wraps ErrClosed.
func (db *DB) Close() error { return notYet("DB.Close", "G1") }

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
	db *DB // the database the transaction is on
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
// caught that way, and hangs.
//
// An error from the commit itself means its outcome is unknown. The
// database in memory goes back to how it was before fn, but a power cut
// could still leave the commit in the file. If the file can't be put back,
// every later write through db fails with an error that wraps ErrStuck,
// until db is closed.
func (db *DB) Update(fn func(tx *Tx) error) error { return notYet("DB.Update", "G1") }

// Compact rewrites the file with only the live data, which also takes
// deleted data off the disk, as VACUUM does in 0.x. The database compacts
// itself at the end of a commit once the file holds about twice as much as
// its live data, and Compact does it at once. Writers wait while it runs,
// in this process and in others, and other processes then reload the file.
func (db *DB) Compact() error { return notYet("DB.Compact", "G5") }
