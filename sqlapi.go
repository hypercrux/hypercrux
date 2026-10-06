// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import "database/sql"

// Exec runs an SQL statement that changes the file, such as an INSERT, an
// UPDATE or a CREATE INDEX. Changes to record tables and links go through
// the same triggers as Put, Delete and Link, so they keep the file
// consistent too.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	return db.sql.Exec(query, args...)
}

// Exec is DB.Exec inside the transaction.
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(query, args...)
}

// Query runs an SQL query. Besides SQLite's own functions it can use
// distance(a, b), vector(json) and walk(key, depth [, type [, direction]]),
// described in FORMAT.md.
func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.sql.Query(query, args...)
}

// Query is DB.Query inside the transaction.
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(query, args...)
}

// QueryRow runs an SQL query that returns at most one row.
func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	return db.sql.QueryRow(query, args...)
}

// QueryRow is DB.QueryRow inside the transaction.
func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRow(query, args...)
}
