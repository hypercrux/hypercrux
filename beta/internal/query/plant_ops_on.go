// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package query

import "os"

// The operators' planted bugs (Q4), which HYPERCRUX_PLANT switches on in a
// build with the hypercrux_planted tag, as plant_on.go's are. They're
// listed here so the operators and the evaluator change in files apart.
//
//   - query/sort-unstable: Sort uses an unstable sort, so rows that tie on
//     every key can lose the order they came in.
//   - query/sort-nulls-last: NULL sorts after every other value going up.
//   - query/top-k-later-tie-wins: TopK lets a row that ties its last row on
//     every key take that row's place, so a later row wins the tie.
//   - query/top-k-works-out-every-row: TopK works out every row's result
//     columns, where SQLite works out only those of the rows it keeps as
//     they come, so it raises errors SQLite doesn't.
//   - query/limit-counts-offset: Limit counts the rows OFFSET skips against
//     LIMIT, so LIMIT 2 OFFSET 1 gives one row.
//   - query/offset-after-zero-limit: Bounds works out OFFSET after a LIMIT
//     of 0, so LIMIT 0 OFFSET NULL raises "datatype mismatch".
//   - query/sum-uncompensated: sum() adds reals without the error term, so
//     ten 0.1s give 0.9999999999999999.
//   - query/sum-in-reverse: sum() adds its rows from the last to the first.
//   - query/sum-text-as-real: sum() takes text that reads as a whole number
//     as a real, so sum('5') is 5.0.
//   - query/sum-wraps: sum() of whole numbers wraps around past 64 bits.
//   - query/max-later-tie-wins: max() keeps the later of two values that
//     tie, so max() over 1 and then 1.0 is 1.0.
//   - query/aggregates-as-compiled: NewAggregates finds only the aggregate
//     calls the evaluator compiles, so 0 AND sum(x) never works out x.
func init() {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "query/sort-unstable", "query/sort-nulls-last", "query/top-k-later-tie-wins",
		"query/top-k-works-out-every-row", "query/limit-counts-offset", "query/offset-after-zero-limit",
		"query/sum-uncompensated", "query/sum-in-reverse", "query/sum-text-as-real", "query/sum-wraps",
		"query/max-later-tie-wins", "query/aggregates-as-compiled":
		plant = p
	}
}
