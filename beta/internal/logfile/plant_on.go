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
//
// F3's, in the check of the end of the log:
//
//   - logfile/cut-what-counts: a batch that counts and lost its marker is
//     cut off with the rest, where it's written again and marked, so a
//     commit that succeeded is lost when a power cut takes its marker.
//   - logfile/sync-without-rewrite: such a batch is synced and marked
//     without being written again, so after a failed sync marked its pages
//     clean, the sync puts nothing on the drive.
//   - logfile/no-marker-again: the batch is written again without the
//     marker before it, which a failed sync can have left off the drive too.
//
// F4's, in the look past the end of the log for damage:
//
//   - logfile/cut-at-zeros: zeros where a batch should start are taken for
//     the end of everything, and the look past them is left out, so the
//     check cuts off commits that a damaged batch's header hid.
//   - logfile/damage-unconfirmed: Open, when another holds the write lock,
//     reports what it finds past the end of the log as damage at once,
//     without reading it again holding the lock, so a writer at work there
//     makes a sound database fail to open.
//   - logfile/window-edge: the look past reads the file a window at a time
//     with no overlap between windows, so a marker across the edge of two
//     is missed.
//   - logfile/any-sequence-number: a whole marker past the end of the log
//     shows a commit there whatever batch it names, so an older copy of the
//     database stored in a torn batch makes the file read as damaged.
//   - logfile/compacted-held-only: the rule for a compacted file is kept
//     only holding the lock, so Open, when another holds it, reads part of
//     a damaged compacted part as a whole database.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "logfile/no-inode-check", "logfile/one-try", "logfile/any-marker", "logfile/marker-before-sync", "logfile/no-read-check",
		"logfile/cut-what-counts", "logfile/sync-without-rewrite", "logfile/no-marker-again",
		"logfile/cut-at-zeros", "logfile/damage-unconfirmed", "logfile/window-edge", "logfile/any-sequence-number", "logfile/compacted-held-only":
		return p
	}
	return ""
}()
