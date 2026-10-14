// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package rules

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
// L1's, in the limit on text and bytes:
//
//   - rules/old-length-limit: Length takes text and bytes up to
//     4,294,967,295 bytes, the most FORMAT.md's u32 says, as the store did
//     before L1, where 0.x stops at 1,000,000,000.
//   - rules/length-off-by-one: Length refuses a value of 1,000,000,000
//     bytes, which SQLite takes.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "rules/old-length-limit", "rules/length-off-by-one":
		return p
	}
	return ""
}()
