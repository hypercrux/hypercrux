// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import "github.com/hypercrux/hypercrux/beta/internal/value"

// Rows is the operator iterator: rows of values, pulled one at a time.
// Every operator is one, and most read from another: scan, filter,
// project, sort, top k, limit and offset, and the aggregates over the
// whole result, and also the walk and the nearest search that SQL reads
// as tables. The database/sql driver reads the top one into copies before
// Query returns (G3), so open rows hold nobody up.
//
// An operator reads the store through a store.Reader, so its rows are good
// only while that read lasts.
//
// A caller loops on Next, reads each row with Row, checks Err once Next
// returns false, and calls Close. It may call Close at any point to stop
// early.
type Rows interface {
	// Next moves to the next row and reports whether there is one. It
	// returns false at the end, and on an error, which Err then returns.
	Next() bool

	// Row returns the current row, one value for each column. An operator
	// may use the slice again, so it's good until the next call to Next or
	// Close, and a caller that keeps a row copies the slice. The values in
	// it never change.
	Row() []value.Value

	// Err returns the error that ended the rows, or nil when they ran to
	// the end or were closed.
	Err() error

	// Close stops the rows, and the rows they read from. It can be called
	// more than once.
	Close()
}
