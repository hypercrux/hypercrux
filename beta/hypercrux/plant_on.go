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
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "hypercrux/append-error-dropped", "hypercrux/reset-kept", "hypercrux/load-after-open", "hypercrux/vector-shared":
		return p
	}
	return ""
}()
