// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package main

import "os"

// A build with the hypercrux_planted tag can switch on one of the command's
// planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs the tests
// with each bug listed in beta/plants.txt, and every one must make them
// fail. A name the command doesn't know switches nothing on. The command
// suite in beta/conformance/cmdtest builds the command with the tag when its
// own tests have it, and the command reads HYPERCRUX_PLANT as it starts.
//
//   - cmd/failure-exits-zero: a verb that fails prints its error and exits
//     with 0, so a script takes the failure for success.
//   - cmd/two-statements-taken: sql hands the database everything it's
//     given, so a second statement is refused as the database's error, with
//     exit code 1, where the command refuses it itself with 2.
//   - cmd/failed-put-leaves-file: a put that fails on a file it created
//     leaves the new, empty database behind.
//   - cmd/compact-skipped: compact says it compacted without compacting.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "cmd/failure-exits-zero", "cmd/two-statements-taken", "cmd/failed-put-leaves-file", "cmd/compact-skipped":
		return p
	}
	return ""
}()
