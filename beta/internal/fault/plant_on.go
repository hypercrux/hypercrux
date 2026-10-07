// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package fault

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
// Each makes the disk kinder than its model, or breaks something the crash
// tests lean on:
//
//   - fault/cut-keeps-everything: a cut keeps every change since the last
//     sync, at the size reads saw.
//   - fault/torn-with-zeros: a torn sector holds zeros where it should keep
//     what the drive held, so synced bytes beside a torn write are lost.
//   - fault/clean-pages-written: a sector a failed sync marked clean, or
//     tore, stays pending, so the next good sync writes it after all.
//   - fault/second-open-locks: TryLock takes the flock from another open
//     file that holds it.
//   - fault/nth-plus-one: a rule picks the call after its nth.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "fault/cut-keeps-everything", "fault/torn-with-zeros", "fault/clean-pages-written",
		"fault/second-open-locks", "fault/nth-plus-one":
		return p
	}
	return ""
}()
