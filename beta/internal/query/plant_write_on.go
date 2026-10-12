// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package query

import "os"

// The writes' planted bugs (Q6), which HYPERCRUX_PLANT switches on in a
// build with the hypercrux_planted tag, as plant_on.go's are. They're
// listed here so the writes change in files of their own.
//
//   - query/statement-takes-all-back: a failed statement takes the whole
//     transaction back to its start, where only its own changes go.
//   - query/func-error-kept: distance(), vector() and walk() refusing their
//     arguments stay of kind "error" in a write, where they're "invalid".
//   - query/insert-values-as-written: an INSERT of one row works out its
//     values in the order written, where SQLite takes the table's columns
//     in their order.
//   - query/insert-row-by-row: an INSERT of several rows puts each row as
//     soon as its values are worked out, so a later row's error comes after
//     an earlier row's UNIQUE, where every row's values come first.
//   - query/taken-before-statement: an INSERT's UNIQUE check sees only the
//     records there before the statement, so a key twice in one INSERT goes
//     in as one record.
//   - query/key-of-any-table: an INSERT takes a key of another table, and
//     puts the record there.
//   - query/set-in-written-order: an UPDATE works out its SET values in the
//     order written, where SQLite takes the table's columns in their order.
//   - query/update-in-one-pass: an UPDATE always works out its SET values as
//     it reads each record, even when 0.x's SQLite takes two passes.
//   - query/update-writes-as-it-reads: an UPDATE puts each record as soon
//     as it's read, so a value worked out later reads the records the
//     statement has changed already.
//   - query/key-change-let-through: an UPDATE that sets the key to another
//     value goes through, and leaves the key as it was.
//   - query/count-every-record: an UPDATE's or a DELETE's count takes in
//     records its WHERE ruled out.
//   - query/delete-as-it-reads: a DELETE deletes each record as soon as its
//     WHERE passes it, so a later record's WHERE sees the earlier deletes.
func init() {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "query/statement-takes-all-back", "query/func-error-kept", "query/insert-values-as-written",
		"query/insert-row-by-row", "query/taken-before-statement", "query/key-of-any-table",
		"query/set-in-written-order", "query/update-in-one-pass", "query/update-writes-as-it-reads",
		"query/key-change-let-through", "query/count-every-record", "query/delete-as-it-reads":
		plant = p
	}
}
