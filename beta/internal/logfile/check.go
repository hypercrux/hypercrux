// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"io"
	"io/fs"

	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// FORMAT.md's "Checking the end of the log". Nothing is cut, and nothing is
// written again in place, without the write lock. Its holder checks the
// end of the log before it appends anything (lockFile, through catchUp),
// and so does opening once it holds the lock: when it gets it without
// waiting (tryCheck), and when it waited for it to read again what looked
// like damage (confirm, in damage.go). So does a follower that finds it can
// take the lock, since the writer is gone (Follow, in follow.go), through
// tryCheck and confirm too.
//
// The check reads the log on to its end first, under the lock, so the end
// it looks at is just past the last marked batch. Then:
//
//  1. It looks past the end of the log for damage (pastEnd, in damage.go):
//     in a compacted file, a log that ends before the compacted part does,
//     and a whole marker further on naming the next batch or a later one.
//     Holding the lock, that's damage, and nothing is written.
//  2. It looks at the first batch after the last marked one (markLeft).
//     When that batch counts, and no whole marker naming another batch
//     follows it, a writer left it there and died, before or after its
//     sync, with nothing or a torn marker after it. The batch is written
//     again in place, with the marker before it, synced, marked, and handed
//     to the Target.
//  3. It cuts off everything after the last marker, as a failed commit is
//     cut back (cutBack, in failed.go): the last marker is written again
//     and synced, then the file is cut and the cut synced.
//
// A write, a sync or a cut that fails stops the check, and leaves the end of
// the log as it is for the next check, which starts again from it
// (unfinished).

// tryCheck is the try at the write lock that Open makes once it has read
// the log, and that Follow makes when it stops short of the end of the
// file, holding the mutex, to find whether the writer has gone. It tries
// flock once, without waiting, and reports whether it got it. When it does,
// no writer is at work: it makes the checks a writer makes once it holds
// the lock, reads on to the end of the log, checks the end of the log, and
// lets go.
//
// When another holds the lock, that writer checks the end of the log before
// it appends anything, and tryCheck does nothing more. Open then looks past
// the end of the log without the lock (openCheck), and Follow makes its
// cheap test (followOn). Nor does tryCheck check anything when the file is
// empty, since the first Lock makes a database of it, or when nothing is at
// the path, since the next Lock reads what's there. When another file has
// taken the path, its error wraps ErrReplaced. Holding the lock, it removes
// a leftover NAME.compact too, as Lock does (checkLocked).
func (l *Log) tryCheck() (bool, error) {
	if l.empty {
		return false, nil
	}
	ok, err := l.f.TryLock()
	if err != nil || !ok {
		return false, err
	}
	err = l.checkLocked()
	if e := l.f.Unlock(); err == nil {
		err = e
	}
	return true, err
}

// checkLocked is the work of tryCheck and confirm once flock is held. When
// another file has taken the path, what was read is no longer the
// database, and its error wraps ErrReplaced. Once the file locked is the one
// at the path, a leftover NAME.compact goes, as in Lock (compact.go).
func (l *Log) checkLocked() error {
	info, same, err := l.atPath()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !same:
		return l.replaced()
	}
	l.removeCompact()
	if err := l.checkRead(info); err != nil {
		return err
	}
	return l.catchUp(info.Size)
}

// catchUp reads on to the end of the log and checks the end of the log,
// holding the write lock, in a file that fstat gave as size bytes long once
// the lock was held. This Log's own failed commits are cut back out of the
// file before the lock goes, so what lies past the end of the log is another
// writer's, or what a check that failed left, and the check deals with it
// either way.
func (l *Log) catchUp(size int64) error {
	if err := l.readOn(size); err != nil {
		return err
	}
	return l.checkEnd(size)
}

// checkEnd checks the end of the log, holding the write lock, once the log
// has been read to its end in a file of size bytes. Damage it finds is a
// *errs.Damage, and nothing is changed: in a compacted file a log that ends
// before the compacted part does, a whole marker past the end of the log
// naming the next batch or a later one, a batch that counts followed by a
// whole marker naming another batch, and a batch that counts with changes
// that break the rules. A write, a sync or a cut that fails stops the check,
// and the end of the log is left for the next check (unfinished).
func (l *Log) checkEnd(size int64) error {
	// Step 1, before anything is written. Holding the lock, what the look
	// past the end finds is damage. In a compacted file, it also keeps the
	// cut below from ever reaching into the compacted part.
	d, err := l.pastEnd(size)
	switch {
	case d != nil:
		return d
	case err != nil:
		return err
	case l.end == size:
		return nil // the log is whole
	}
	defer l.r.release()
	if err := l.markLeft(size); err != nil {
		return err
	}
	// Step 3. The marker markLeft writes can take the file past size, and
	// then nothing is left after it.
	if l.end >= size {
		return nil
	}
	if err := l.cutBack(); err != nil {
		return l.unfinished(err, "cutting the file back to the end of the log at offset %d", l.end)
	}
	return nil
}

// markLeft is step 2 of the check, at the end of the log in a file of size
// bytes. When the first batch after the end of the log counts, and no whole
// marker naming another batch follows it, a writer left it there and died
// before marking it, or a power cut took its marker. markLeft writes it
// again in place, with the marker before it, syncs it, marks it and hands it
// to the Target, and the log ends after it. A batch that doesn't count is
// left for the cut.
//
// Both are written again because after a failed sync, Linux can mark their
// pages clean, and a sync on its own could then report success without them
// ever reaching the drive. The batch is FORMAT.md's reason. The marker
// before it was written after the last sync that worked, so the same goes
// for it. If a power cut then took that marker, the next check would find
// the batch before it unmarked, mark it, and cut off this one, which a
// reader may have seen.
//
// The Target judges the rules for the state, and takes a batch only once
// it's marked and on the drive, as every reader does. So a batch that
// counts but breaks those rules is found only once it's marked, and is
// damage then, as it is on every open after.
func (l *Log) markLeft(size int64) error {
	// Changes that are malformed, or break the rules for a change on its
	// own, are damage, which endBatch returns.
	bt, b, err := l.endBatch(size)
	if err != nil || b == nil {
		return err
	}
	if d := l.otherMarker(bt, b); d != nil {
		return d
	}
	off, n := l.end, int64(bt.Length)
	mine := format.Marker{Seq: bt.Seq, Sum: bt.Sum}
	if plant == "logfile/cut-what-counts" {
		return nil
	}

	at, again := off, l.batch[:0]
	if l.seq > 0 && plant != "logfile/no-marker-again" {
		at, again = off-format.MarkerSize, append(again, l.last[:]...)
	}
	again = append(again, b[:n]...)
	if cap(again) <= maxKept {
		l.batch = again // kept for the next commit, as Append keeps its own
	}
	if plant != "logfile/sync-without-rewrite" {
		if _, err := l.f.WriteAt(again, at); err != nil {
			return l.unfinished(err, "writing batch %d again, which a writer left without its marker,", bt.Seq)
		}
	}
	if err := l.f.Sync(); err != nil {
		return l.unfinished(err, "syncing batch %d, which a writer left without its marker,", bt.Seq)
	}
	var marker [format.MarkerSize]byte
	format.AppendMarker(marker[:0], l.hdr.ID, l.hdr.Gen, mine)
	if _, err := l.f.WriteAt(marker[:], off+n); err != nil {
		return l.unfinished(err, "marking batch %d, which a writer left without its marker,", bt.Seq)
	}
	if err := l.t.Apply(bt.Seq, bt.Changes); err != nil {
		return l.refused(off, bt.Seq, err)
	}
	l.seq, l.end, l.last = bt.Seq, off+n+format.MarkerSize, marker
	return nil
}

// unread is what the check makes of an error from reading the end of the
// log. Holding the lock, the file can't be shorter than fstat said, unless
// something other than HyperCrux cut it; then there's less to check, and the
// cut goes back to the end of the log all the same.
func unread(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
