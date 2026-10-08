// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package procs

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
// The bug is in the toy log the package's tests run, and the harness has
// to catch it. The child processes are copies of the same test binary,
// started with the same environment, so they have it too:
//
//   - procs/marker-before-batch: the toy log's writer puts each commit's
//     marker down before its batch, where it belongs after it, so another
//     process can find the marker while the batch's place still holds
//     zeros.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "procs/marker-before-batch":
		return p
	}
	return ""
}()
