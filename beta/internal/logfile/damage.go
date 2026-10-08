// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// Damage past the end of the log (F4): FORMAT.md's "Looking past the end of
// the log", and step 1 of "Checking the end of the log".
//
// Wherever the log has been read to its end, what lies past it is looked
// at for damage before anything else happens (pastEnd):
//
//   - in a compacted file, a log that ends before the compacted part does,
//     since a compacted file is whole before anyone can see it;
//   - from the end of the log to the end of the file, at every offset, a
//     whole marker naming the next batch or a later one, which shows that a
//     commit was made past the end of the log. A marker survives damage to
//     its own batch, so this is how damage in the middle of a file is told
//     from a torn end, even past a run of zeros.
//
// While the write lock is held, nothing past the end of the log can change,
// so what the look finds is damage, and nothing is written: the check makes
// it at its top, before markLeft writes anything (checkEnd). Without the
// lock, a writer may be at work there. A marker can appear behind a batch
// that was half written a moment earlier, and a failed commit's batch can
// be cut and another written in its place. So Open, when another holds the
// lock, looks without it (suspect), and when it finds what may be damage,
// it waits for the lock as a writer does and reads the file again holding
// it (confirm). Damage is reported only if it's still there. A follower
// stops at every commit under way, and a search of the whole tail each time
// would cost too much, so it looks at the batch it stopped at alone
// (followOn, in follow.go), and confirms in the same way.

// pastEnd is the look past the end of the log, step 1 of the check, in a
// file of size bytes. It returns what shows damage there, or nil. The damage
// names the batch that should come next, at the offset where the log ends,
// and says what showed it.
func (l *Log) pastEnd(size int64) (*errs.Damage, error) {
	if d := l.inCompacted(); d != nil {
		return d, nil
	}
	return l.markerPast(size)
}

// inCompacted is the rule for a compacted file: the log ends before the
// compacted part does, which is damage, and a cut must never reach into the
// compacted part (FORMAT.md, "Compaction").
func (l *Log) inCompacted() *errs.Damage {
	if l.hdr.Gen > 1 && uint64(l.end) < l.hdr.CompactedEnd {
		return l.past(fmt.Sprintf("the log ends at offset %d, inside the compacted part, which ends at offset %d", l.end, l.hdr.CompactedEnd))
	}
	return nil
}

// markerPast searches the file from the end of the log to size, at every
// offset, for a whole marker naming the next batch or a later one. A marker
// naming an earlier batch is passed over, since an older copy of this
// database, stored as a value inside a torn batch, holds such markers; so is
// one of another database or generation, which fails its check value.
//
// It reads through the window, a megabyte at a time, and each read after the
// first starts a marker's size less a byte before the one before it ended,
// so a marker across the edge of two reads is found whole in the second.
func (l *Log) markerPast(size int64) (*errs.Damage, error) {
	defer l.r.release()
	if plant == "logfile/cut-at-zeros" && size > l.end {
		if b, err := l.r.read(l.f, l.end, 1, size); err == nil && b[0] == 0 {
			return nil, nil
		}
	}
	next := l.seq + 1
	for off := l.end; size-off >= format.MarkerSize; {
		n := min(window, size-off)
		b, err := l.r.read(l.f, off, n, size)
		if errors.Is(err, io.EOF) {
			break // the file has been cut since its size was read, which a reader without the lock can see
		}
		if err != nil {
			return nil, err
		}
		for i := 0; ; {
			j, m := format.FindMarker(b[i:], l.hdr.ID, l.hdr.Gen)
			if j < 0 {
				break
			}
			if at := off + int64(i+j); m.Seq >= next || plant == "logfile/any-sequence-number" {
				return l.past(fmt.Sprintf("a whole marker at offset %d names batch %d, so a commit was made past the end of the log", at, m.Seq)), nil
			}
			i += j + 1
		}
		if off+n == size {
			break
		}
		if plant == "logfile/window-edge" {
			off += n
			continue
		}
		off += n - (format.MarkerSize - 1)
	}
	return nil, nil
}

// past is damage found past the end of the log: it names the batch that
// should come next, at the offset where the log ends, since that's the first
// batch that can't be read, and why says what showed it.
func (l *Log) past(why string) *errs.Damage {
	return &errs.Damage{Path: l.path, Offset: l.end, Batch: l.seq + 1, Reason: why}
}

// endBatch reads the first batch after the end of the log, in a file of
// size bytes, when it counts. It returns the batch, and the file's bytes
// from its start with room for its marker after it, as far as the file
// goes, which are good until the window is read again or released. When
// what's there isn't a batch that counts, the bytes are nil. Changes in a
// batch that counts that are malformed, or break the rules for a change on
// its own, are the codec's damage, placed in the file.
func (l *Log) endBatch(size int64) (format.Batch, []byte, error) {
	off, room, seq := l.end, size-l.end, l.seq+1
	if room < format.BatchHeadSize {
		return format.Batch{}, nil, nil
	}
	head, err := l.r.read(l.f, off, format.BatchHeadSize, size)
	if err != nil {
		return format.Batch{}, nil, unread(err)
	}
	n, err := format.BatchLength(head, l.hdr.Gen, seq, room)
	if err != nil {
		return format.Batch{}, nil, nil // not a batch that counts
	}
	b, err := l.r.read(l.f, off, min(n+format.MarkerSize, room), size)
	if err != nil {
		return format.Batch{}, nil, unread(err)
	}
	bt, err := format.DecodeBatch(b[:n], l.hdr.Gen, seq)
	if errors.Is(err, format.ErrDoesNotCount) {
		return format.Batch{}, nil, nil
	}
	if err != nil {
		return format.Batch{}, nil, l.placed(off, err)
	}
	return bt, b, nil
}

// otherMarker is the damage when a whole marker naming another batch, by
// sequence number or checksum, follows bt, the batch that counts at the end
// of the log, whose bytes and its marker's room are b. No crash leaves such
// a marker (FORMAT.md, "Markers"). It returns nil when what follows the
// batch isn't a whole marker, or is the batch's own.
func (l *Log) otherMarker(bt format.Batch, b []byte) *errs.Damage {
	n := int64(bt.Length)
	if int64(len(b)) < n+format.MarkerSize {
		return nil
	}
	m, whole := format.DecodeMarker(b[n:], l.hdr.ID, l.hdr.Gen)
	if !whole || m == (format.Marker{Seq: bt.Seq, Sum: bt.Sum}) {
		return nil
	}
	return &errs.Damage{Path: l.path, Offset: l.end + n, Batch: bt.Seq, Reason: fmt.Sprintf(
		"batch %d counts, with the checksum %#08x, and the whole marker after it names batch %d with the checksum %#08x, which no crash leaves", bt.Seq, bt.Sum, m.Seq, m.Sum)}
}

// openCheck is Open's check of the end of the log, once load has read the
// log of a file of size bytes. It tries the write lock without waiting, and
// when it gets it, it checks the end of the log holding it (tryCheck).
//
// When another holds the lock, a writer may be at work past the end of the
// log, and that writer checks the end before it appends anything. Open looks
// past the end without the lock all the same (suspect), so damage in the
// middle of a file is never taken for its end. When it finds what may be
// damage, it waits for the lock, until deadline, and reads the file again
// holding it (confirm). deadline is set at the first wait, and kept when
// Open starts again on another file. Otherwise Open leaves the end of the
// log as it is.
func (l *Log) openCheck(size int64, deadline *time.Time) error {
	if l.empty {
		return nil
	}
	got, err := l.tryCheck()
	if err != nil || got {
		return err
	}
	d, err := l.suspect(size)
	if err != nil || d == nil {
		return err
	}
	if plant == "logfile/damage-unconfirmed" {
		return d
	}
	if deadline.IsZero() {
		*deadline = time.Now().Add(l.wait)
	}
	return l.confirm(*deadline, d)
}

// suspect is the look past the end of the log that a reader makes without
// the write lock, in a file of size bytes. It looks for what pastEnd looks
// for, and for a batch that counts at the end of the log followed by a
// whole marker naming another batch, which the check under the lock reports
// as damage in its step 2. It returns what it finds, which is damage only
// once a read under the lock agrees, or nil.
func (l *Log) suspect(size int64) (*errs.Damage, error) {
	if plant != "logfile/compacted-held-only" {
		if d := l.inCompacted(); d != nil {
			return d, nil
		}
	}
	if d, err := l.markerPast(size); d != nil || err != nil {
		return d, err
	}
	defer l.r.release()
	bt, b, err := l.endBatch(size)
	var d *errs.Damage
	switch {
	case errors.As(err, &d):
		// Changes that break the rules, in a batch that didn't count when it
		// was read a moment ago: a writer may have been at work on it.
		return d, nil
	case err != nil || b == nil:
		return nil, err
	}
	return l.otherMarker(bt, b), nil
}

// confirm is the read under the write lock that a reader makes before it
// reports damage, once it has found d past the end of the log without the
// lock. It waits for flock until deadline, as a writer does (flock). Holding
// it, nothing can change, so the read is final: it makes the checks tryCheck
// makes once it holds the lock, which read on to the end of the log and
// check its end, and so report damage only when it's still there, and then
// it lets go. When what it found is gone, the log has moved on to whatever
// the writer it waited for committed meanwhile, and the end has been
// checked as a writer would check it.
//
// When the wait runs out, the error wraps errs.ErrLockTimeout and says what
// was found. It doesn't wrap errs.ErrDamaged, since nothing confirmed the
// damage.
func (l *Log) confirm(deadline time.Time, d *errs.Damage) error {
	if err := l.flock(deadline, nil); err != nil {
		if errors.Is(err, errs.ErrLockTimeout) {
			return fmt.Errorf("%w; it was needed to read the file again, since batch %d at offset %d may be damaged: %s", err, d.Batch, d.Offset, d.Reason)
		}
		return err
	}
	err := l.checkLocked()
	if e := l.f.Unlock(); err == nil {
		err = e
	}
	return err
}
