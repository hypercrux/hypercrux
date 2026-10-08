// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import "database/sql"

// The methods hand each statement to database/sql through calls, with
// the DB and the Tx in its context, and the driver runs it there
// (driver.go). A closed database and an ended transaction fail with
// ErrClosed before anything looks at the statement or its arguments, as
// every other call does.

// Exec runs an SQL statement that changes the database: an INSERT, an
// UPDATE or a DELETE on one table, in the subset beta/SQL.md sets out. Each
// row it writes goes through the same checks as Put and Delete, and a
// statement that breaks one of their rules fails with an error that wraps
// ErrInvalid. A statement is all or nothing.
//
// The result's RowsAffected gives the count of rows the statement changed,
// and 0 for a SELECT, whose rows Exec drops. Its LastInsertId returns an
// error, since records have no row numbers.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	if _, err := db.current(); err != nil {
		return nil, err
	}
	return calls().ExecContext(db.context(), query, args...)
}

// Exec is DB.Exec inside the transaction. A statement that fails undoes its
// own changes, and the transaction carries on.
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	if _, err := t.open(); err != nil {
		return nil, err
	}
	return calls().ExecContext(t.context(), query, args...)
}

// Query runs an SQL query in the subset beta/SQL.md sets out. Besides its
// text and number functions it can use distance(a, b), vector(json) and
// walk(key, depth [, type [, direction]]), which works as a table and as a
// function giving JSON text.
//
// Query reads every row before it returns, so rows left open hold up no
// Update, and they go on giving what the database held when the query ran.
// A write through Query runs, and gives no columns and no rows.
func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	if _, err := db.current(); err != nil {
		return nil, err
	}
	return calls().QueryContext(db.context(), query, args...)
}

// Query is DB.Query inside the transaction, which sees the transaction's
// changes.
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	if _, err := t.open(); err != nil {
		return nil, err
	}
	return calls().QueryContext(t.context(), query, args...)
}

// QueryRow runs an SQL query that returns at most one row.
func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	if _, err := db.current(); err != nil {
		return failedRow(err)
	}
	return calls().QueryRowContext(db.context(), query, args...)
}

// QueryRow is DB.QueryRow inside the transaction.
func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	if _, err := t.open(); err != nil {
		return failedRow(err)
	}
	return calls().QueryRowContext(t.context(), query, args...)
}
