// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package format

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
// R2's, in the decoder's memory:
//
//   - format/changes-grown: decodeChanges grows its list of changes, and the
//     arrays of fields, names and vectors behind them, an append at a time,
//     where it makes each the size its count gives, so a batch of many
//     changes costs about five times its arrays in allocations, and copies
//     them over and over as they grow.
//   - format/block-count-trusted: a vector block's count of vectors is
//     taken without checking that its keys and values can fit in the bytes
//     left, so a block that claims billions of vectors over a batch of
//     zeros makes the decoder hold 32 bytes for every 2 bytes of the batch
//     before the read fails.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "format/changes-grown", "format/block-count-trusted":
		return p
	}
	return ""
}()
