// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package logfile

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
//   - logfile/no-inode-check: Lock takes the file it locked for the one at
//     the path, without comparing device and inode numbers, so a commit
//     goes into a file that another has replaced.
//   - logfile/one-try: Lock tries flock once and gives up, without
//     waiting for the lock to come free.
//   - logfile/any-marker: a whole marker marks the batch before it, even
//     when it names another batch.
//   - logfile/marker-before-sync: Append writes the batch's marker before
//     the sync as well as after it.
//   - logfile/no-read-check: Lock doesn't check that what it has read is
//     still in the file, so it appends to a file that was copied over.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "logfile/no-inode-check", "logfile/one-try", "logfile/any-marker", "logfile/marker-before-sync", "logfile/no-read-check":
		return p
	}
	return ""
}()
