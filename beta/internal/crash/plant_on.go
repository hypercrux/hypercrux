// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package crash

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
// The bug is in the toy log the package's tests drive, and the driver has
// to catch it:
//
//   - crash/marker-before-sync: the toy log writes each batch's marker
//     before the batch's sync, where it belongs after it, so a cut before
//     the sync can keep the marker and lose the batch.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "crash/marker-before-sync":
		return p
	}
	return ""
}()
