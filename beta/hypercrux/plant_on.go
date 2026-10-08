// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package hypercrux

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
//   - hypercrux/append-error-dropped: Update takes no notice when Append
//     fails, so the copy keeps a commit the file may not have.
//   - hypercrux/reset-kept: Reset keeps the old copy, so the batches of a
//     file that has taken the path go in on top of it.
//   - hypercrux/load-after-open: batches the log reads after Open go
//     straight into the copy, as on opening, instead of through a
//     transaction, so a batch that breaks the rules goes in by halves.
//   - hypercrux/vector-shared: Get hands out the copy's own vector, so a
//     caller that changes it changes the database.
//   - hypercrux/scan-limit-first: Scan checks its limit before its prefix,
//     so a bad prefix with a limit below 0 gives the limit's error, where
//     0.x gives the prefix's.
//   - hypercrux/scan-vector-kept: Scan gives each record's vector among its
//     fields, which 0.x's Scan leaves out.
//   - hypercrux/neighbours-unlocked: DB.Neighbours reads the copy without
//     its Read, so it neither waits for an Update's changes to commit nor
//     fails inside one, and can read changes that roll back.
//   - hypercrux/nearest-nil-for-none: Nearest gives nil for a search that
//     finds nothing in a table with a vector size, where 0.x gives an empty
//     list.
//   - hypercrux/filter-ignored: Nearest with a filter searches without it,
//     where it should wait for task G4 as a stub.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "hypercrux/append-error-dropped", "hypercrux/reset-kept", "hypercrux/load-after-open", "hypercrux/vector-shared",
		"hypercrux/scan-limit-first", "hypercrux/scan-vector-kept", "hypercrux/neighbours-unlocked",
		"hypercrux/nearest-nil-for-none", "hypercrux/filter-ignored":
		return p
	}
	return ""
}()
