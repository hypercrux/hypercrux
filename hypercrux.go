// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// Version is the version of the package and of the hypercrux command.
const Version = "0.2.0"

// FormatVersion is the version of the file layout described in FORMAT.md.
// HyperCrux refuses to open a file with a newer format version.
const FormatVersion = 1

// ApplicationID marks a file HyperCrux created for itself. It is stored in
// the SQLite header and spells "HCRX".
const ApplicationID = 0x48435258

// DriverName is the database/sql driver HyperCrux registers. It is
// go-sqlite3 with HyperCrux's SQL functions, distance, vector and walk,
// added to every connection.
const DriverName = "hypercrux-sqlite3"

var (
	// ErrNotFound means a key, table or link doesn't exist.
	ErrNotFound = errors.New("hypercrux: not found")
	// ErrInvalid means an argument breaks HyperCrux's rules, such as a key
	// without a table name or a vector of the wrong size.
	ErrInvalid = errors.New("hypercrux: invalid")
)

func init() {
	sql.Register(DriverName, &sqlite3.SQLiteDriver{ConnectHook: registerFunctions})
}

// DB is an open HyperCrux file. It is safe for use by many goroutines, and
// many processes on one machine can open the same file at once.
type DB struct {
	sql  *sql.DB
	path string

	mu     sync.Mutex
	schema int64                 // schema_version the cache was read at
	tables map[string]*tableInfo // record tables by name
}

// runner runs statements on the database, or in a transaction when tx is
// set. Each connection keeps its own cache of prepared statements, which
// go-sqlite3 manages and refreshes when the schema changes.
type runner struct {
	db *DB
	tx *sql.Tx
}

func (r runner) Exec(q string, args ...any) (sql.Result, error) {
	if r.tx != nil {
		return r.tx.Exec(q, args...)
	}
	return r.db.sql.Exec(q, args...)
}

func (r runner) Query(q string, args ...any) (*sql.Rows, error) {
	if r.tx != nil {
		return r.tx.Query(q, args...)
	}
	return r.db.sql.Query(q, args...)
}

func (r runner) QueryRow(q string, args ...any) *sql.Row {
	if r.tx != nil {
		return r.tx.QueryRow(q, args...)
	}
	return r.db.sql.QueryRow(q, args...)
}

func (db *DB) run() runner { return runner{db: db} }

func (t *Tx) run() runner { return runner{db: t.db, tx: t.tx} }

// Open opens the HyperCrux file at path, creating it if it doesn't exist.
// An existing SQLite database gets HyperCrux's tables added next to its own,
// and its tables become records only when you put into them or Adopt them.
func Open(path string) (*DB, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("%w: file name %q", ErrInvalid, path)
	}
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("%w: %s: HyperCrux needs a file name; for a throwaway database, use a file in a temporary folder", ErrInvalid, path)
	}
	// Every connection waits up to 10 seconds for a lock, starts write
	// transactions with BEGIN IMMEDIATE, and uses WAL so readers carry on
	// while a writer commits. synchronous stays at SQLite's default, FULL,
	// so a committed write survives a crash or a power cut. Recursive
	// triggers are on so that a row removed by REPLACE fires its delete
	// trigger like any other deleted row. Each connection keeps up to 128
	// prepared statements.
	dsn := path + "?_busy_timeout=10000&_txlock=immediate&_journal_mode=WAL&_synchronous=FULL" +
		"&_recursive_triggers=1&_stmt_cache_size=128"
	s, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, err
	}
	s.SetMaxOpenConns(8)
	s.SetMaxIdleConns(8)
	db := &DB{sql: s, path: path}
	if err := db.setup(); err != nil {
		s.Close()
		return nil, err
	}
	return db, nil
}

// Close closes the file.
func (db *DB) Close() error { return db.sql.Close() }

// Path returns the file name the database was opened with.
func (db *DB) Path() string { return db.path }

// SQL returns the underlying database/sql handle, for prepared statements
// and anything else the methods don't cover. Writes through it still go
// through HyperCrux's triggers.
func (db *DB) SQL() *sql.DB { return db.sql }

// Tx is a write transaction, passed to the function given to Update. It has
// the same methods as DB, and everything done through it commits together
// or not at all. Don't use it after the function returns.
type Tx struct {
	db  *DB
	tx  *sql.Tx
	ddl bool // the transaction may have changed the schema
}

// Update runs fn in one write transaction. If fn returns an error or
// panics, nothing it did is kept. Otherwise the transaction commits, and
// every key, field, link and vector it changed changes together.
//
// Inside fn, use tx and not db. The methods of db run outside the
// transaction: they don't see its changes, and a write through db waits
// for the transaction to finish and fails after 10 seconds.
func (db *DB) Update(fn func(tx *Tx) error) error {
	stx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	t := &Tx{db: db, tx: stx}
	committed := false
	defer func() {
		if !committed {
			stx.Rollback()
			db.forget() // whatever the transaction saw of the schema is gone
		} else if t.ddl {
			db.forget()
		}
	}()
	if err := fn(t); err != nil {
		return err
	}
	if err := stx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// querier is what *sql.DB and *sql.Tx have in common.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// setup adds HyperCrux's own tables to the file if they're missing, and
// checks the format version.
func (db *DB) setup() error {
	var version sql.NullString
	err := db.sql.QueryRow(`SELECT value FROM hc_meta WHERE name = 'format_version'`).Scan(&version)
	switch {
	case err == nil:
		return checkFormat(version.String)
	case !isNoSuchTable(err):
		return err
	}
	return db.Update(func(tx *Tx) error {
		var others int
		if err := tx.tx.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`).Scan(&others); err != nil {
			return err
		}
		for _, stmt := range schemaSQL {
			if _, err := tx.tx.Exec(stmt); err != nil {
				return fmt.Errorf("hypercrux: setting up %s: %w", db.path, err)
			}
		}
		var v string
		if err := tx.tx.QueryRow(`SELECT value FROM hc_meta WHERE name = 'format_version'`).Scan(&v); err != nil {
			return err
		}
		if err := checkFormat(v); err != nil {
			return err
		}
		var appID int64
		if err := tx.tx.QueryRow(`PRAGMA application_id`).Scan(&appID); err != nil {
			return err
		}
		if others == 0 && appID == 0 {
			if _, err := tx.tx.Exec(`PRAGMA application_id = ` + strconv.Itoa(ApplicationID)); err != nil {
				return err
			}
		}
		return nil
	})
}

func checkFormat(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return fmt.Errorf("hypercrux: the file has a broken format_version %q", v)
	}
	if n > FormatVersion {
		return fmt.Errorf("hypercrux: the file uses format version %d, and this HyperCrux reads up to %d; upgrade HyperCrux", n, FormatVersion)
	}
	return nil
}

func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// isMissing reports an error from querying a table that isn't there, or
// that has no key column, which for a key lookup means "not found".
func isMissing(err error) bool {
	return isNoSuchTable(err) || (err != nil && strings.Contains(err.Error(), "no such column"))
}

// quote returns name as an SQL identifier. HyperCrux checks its own names
// before they get here, so this is a second guard.
func quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
