// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package query

import "os"

// The planted bugs in SQL's limits on length (L1), which HYPERCRUX_PLANT
// switches on in a build with the hypercrux_planted tag, as plant_on.go's
// are.
//
//   - query/statement-length-unchecked: Parse takes a statement of any
//     length, so a literal can be longer than SQLite's limit on values, where
//     0.x refuses the statement as "statement too long".
//   - query/filter-argument-unchecked: NearestFilter leaves its arguments'
//     lengths unchecked, so a search through a filter takes an argument of
//     more than 1,000,000,000 bytes, which 0.x's bind refuses.
//   - query/walk-length-unchecked: walk() gives its JSON text however long
//     it is, where SQLite refuses 0.x's a result past its limit.
func init() {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "query/statement-length-unchecked", "query/filter-argument-unchecked", "query/walk-length-unchecked":
		plant = p
	}
}
