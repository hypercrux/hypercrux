// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
)

// Exec runs an SQL statement that changes the database: an INSERT, an
// UPDATE or a DELETE on one table, in the subset beta/SQL.md sets out. Each
// row it writes goes through the same checks as Put and Delete, and a
// statement that breaks one of their rules fails with an error that wraps
// ErrInvalid. A statement is all or nothing.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	return nil, notYet("DB.Exec", "G4")
}

// Exec is DB.Exec inside the transaction. A statement that fails undoes its
// own changes, and the transaction carries on.
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return nil, notYet("Tx.Exec", "G4")
}

// Query runs an SQL query in the subset beta/SQL.md sets out. Besides its
// text and number functions it can use distance(a, b), vector(json) and
// walk(key, depth [, type [, direction]]), which works as a table and as a
// function giving JSON text.
func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return nil, notYet("DB.Query", "G4")
}

// Query is DB.Query inside the transaction.
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return nil, notYet("Tx.Query", "G4")
}

// QueryRow runs an SQL query that returns at most one row.
func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	return stubSQL().QueryRow(query, args...)
}

// QueryRow is DB.QueryRow inside the transaction.
func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return stubSQL().QueryRow(query, args...)
}

// stubSQL is the database/sql handle that SQL and QueryRow hand out until
// task G3 writes the Beta's driver. Every connection it tries fails with a
// stub's error, so every call through it returns that error, and a caller
// that scans a row or sets a pool size doesn't panic on a nil handle. It's
// made on first use, since database/sql starts a goroutine for each handle.
var stubSQL = sync.OnceValue(func() *sql.DB { return sql.OpenDB(stubConnector{}) })

// stubConnector is a driver.Connector whose connections all fail.
type stubConnector struct{}

func (stubConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, notYet("SQL", "G4")
}

func (stubConnector) Driver() driver.Driver { return stubDriver{} }

// stubDriver is the driver.Driver behind stubConnector.
type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) { return nil, notYet("SQL", "G4") }
