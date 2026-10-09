// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package query

import "os"

// The planner's planted bugs (Q5), which HYPERCRUX_PLANT switches on in a
// build with the hypercrux_planted tag, as plant_on.go's are. They're
// listed here so the planner changes in files of its own.
//
//   - query/nearest-ignores-filter: the nearest search's filter lets every
//     record with a vector through, so WHERE's other terms go unchecked.
//   - query/nearest-without-vec-check: the planner takes a nearest search
//     without vec IS NOT NULL, so the rows without a vector, which come
//     first, are left out.
//   - query/nearest-k-is-the-limit: the search finds LIMIT records, where it
//     needs LIMIT plus OFFSET, so an OFFSET leaves too few rows.
//   - query/walk-join-drops-a-depth: the walk as a table leaves out the
//     records it reached last, at its greatest depth.
//   - query/walk-lookup-in-walk-order: key IN over a walk reads the records
//     in the walk's order, where they come in key order.
//   - query/null-not-in-walk-kept: NULL NOT IN over a walk keeps the row,
//     where it's NULL.
//   - query/record-every-time: the one-record subquery is worked out again
//     for every row that reaches it, where once for the statement is all.
//   - query/record-of-any-table: the one-record subquery finds a record of
//     another table, so (SELECT key FROM docs WHERE key = 'people:1') is
//     'people:1', where it's NULL.
//   - query/once-terms-for-each-row: WHERE's terms that read no table are
//     worked out for each row, so over an empty table they raise nothing.
//   - query/join-terms-after-lookup: a join's terms on the walk alone are
//     worked out after the table's record is found, so a step to another
//     table's record never meets them.
//   - query/and-not-folded: x AND 0 isn't folded as SQLite's parser folds
//     it, so WHERE (SELECT ...) AND 0 works out the subquery.
//   - query/order-alias-first: in ORDER BY's expressions, a result
//     column's alias is found before a field of the same name.
//   - query/filter-first-term-dropped: NearestFilter leaves out the first
//     term of its condition, as if vec IS NOT NULL took two places.
func init() {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "query/nearest-ignores-filter", "query/nearest-without-vec-check", "query/nearest-k-is-the-limit",
		"query/walk-join-drops-a-depth", "query/walk-lookup-in-walk-order", "query/null-not-in-walk-kept",
		"query/record-every-time", "query/record-of-any-table", "query/once-terms-for-each-row",
		"query/join-terms-after-lookup", "query/and-not-folded", "query/order-alias-first",
		"query/filter-first-term-dropped":
		plant = p
	}
}
