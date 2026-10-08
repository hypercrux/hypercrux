// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"fmt"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// Failed commits (F5): FORMAT.md's "Writing", and the failures of the check
// of the end of the log.
//
// When a commit fails, in the batch's write, the sync or the marker's write,
// the writer cuts the file back to just after the last marker before it lets
// go of the lock (failed). So no check ever finds the failed batch and marks
// it, which matters because the caller's copy has put the commit's changes
// back, and the commits after it are built without them. Once the cut back
// has worked, the batch is gone for good: a power cut can't bring it back.
//
// If the cut back fails too, the Log keeps the lock until Close (stuck), and
// every Lock and Append through it fails with an error that wraps
// errs.ErrStuck. Other handles and processes wait for the lock meanwhile.
// Close, or the end of the process, lets it go, and the next holder's check
// deals with what the stuck handle left as it deals with what a crash
// leaves: it marks the failed batch when the batch counts, since a failed
// commit's outcome is unknown, and cuts it otherwise.
//
// The cut back (cutBack) is also step 3 of the check. It:
//
//  1. writes the last marker again, in place, unless the log has no batch
//     yet. A failed sync can mark the marker's page clean without it
//     reaching the drive, while reads go on seeing it, and then no later
//     sync would write it (F3.md). Before the first batch comes the header,
//     which was synced when the database was made and is never written
//     again;
//  2. syncs, so the last marker is on the drive before anything is cut;
//  3. cuts the file back to just after the last marker;
//  4. syncs the cut.
//
// With the sync before the cut, a writer that stops anywhere in the cut back,
// because a call fails or its process dies, leaves either the end it was
// cutting, which the next check finds and cuts again, or the last marker on
// the drive. A cut synced only once could leave the last marker off the drive
// with nothing after it to show it, and a power cut after a later commit
// would then lose that commit, or leave the file reading as damaged.
//
// A failure in the check, while it writes a batch again, syncs it, marks it
// or cuts the end back, leaves the end of the log for the next check
// (unfinished): the check stops, the lock is let go, and Lock or Open fails.
// The next check finds the same end, or a cut that's complete with the last
// marker on the drive, and starts again. Each step writes only bytes the
// file holds already, so it can be made again, and a batch whose commit may
// have succeeded, synced before a power cut took its marker, is never cut.

// failed handles a commit that failed in Append with err, holding the lock:
// batch seq's write, the sync, or the marker's write. It cuts the file back
// to just after the last marker. When that works, the error says the batch
// was cut back out of the file, and the Log carries on. When it fails too,
// the Log keeps the lock until Close, and the error wraps errs.ErrStuck as
// well as err, and says the commit's outcome is unknown.
func (l *Log) failed(err error, seq uint64) error {
	if plant == "logfile/failed-left-uncut" {
		return fmt.Errorf("hypercrux: %s: committing batch %d failed, and it was cut back out of the file: %w", l.path, seq, err)
	}
	cut := l.cutBack()
	if cut == nil {
		return fmt.Errorf("hypercrux: %s: committing batch %d failed, and it was cut back out of the file: %w", l.path, seq, err)
	}
	l.stuck = fmt.Errorf("%w: %s: committing batch %d failed, and cutting it back out of the file failed too: %w", errs.ErrStuck, l.path, seq, cut)
	return fmt.Errorf("hypercrux: %s: committing batch %d failed, so its outcome is unknown: %w; %w", l.path, seq, err, l.stuck)
}

// cutBack cuts the file back to just after the last marker, holding the
// lock: it writes the last marker again and syncs, then cuts the file and
// syncs the cut. Nothing in the Log changes, since the log ends there
// already. It stops at the first call that fails.
func (l *Log) cutBack() error {
	if l.seq > 0 {
		if plant != "logfile/cut-without-marker" {
			if _, err := l.f.WriteAt(l.last[:], l.end-format.MarkerSize); err != nil {
				return fmt.Errorf("writing batch %d's marker again: %w", l.seq, err)
			}
		}
		if err := l.f.Sync(); err != nil {
			return fmt.Errorf("syncing batch %d's marker: %w", l.seq, err)
		}
	}
	if err := l.f.Truncate(l.end); err != nil {
		return fmt.Errorf("cutting the file back to offset %d: %w", l.end, err)
	}
	if plant == "logfile/cut-unsynced" {
		return nil
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("syncing the cut back to offset %d: %w", l.end, err)
	}
	return nil
}

// unfinished is the error when a write, a sync or a cut fails in the check of
// the end of the log, holding the lock, with err. The check stops there, and
// what's left of the end of the log stays for the next check, which starts
// again from it. what says what failed, as a format for args.
func (l *Log) unfinished(err error, what string, args ...any) error {
	if plant == "logfile/check-cuts-what-failed" {
		l.cutBack()
	}
	return fmt.Errorf("hypercrux: %s: %s failed, and the end of the log is left as it is for the next check: %w", l.path, fmt.Sprintf(what, args...), err)
}
