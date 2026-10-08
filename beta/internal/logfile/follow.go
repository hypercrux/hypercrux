// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// Following other processes (F6): FORMAT.md's "Reading the log", for a
// reader that keeps the database open, and BETA.md's "Other processes". A
// process that keeps the database open calls Follow before each read, and
// Follow hands the Target what other processes have committed since.
//
// The read position. The fields that say how far the log has been read
// belong to the holder of the write lock's mutex, and a commit in this
// process moves them too: Append moves them once its marker is written,
// inside the store's Commit, while the Update's transaction is still open
// (S2.md). So Follow reads on only holding the mutex. It tries the mutex
// without waiting, and when anything but another follower holds it, Follow
// reads nothing and returns at once, since a read can't wait for the mutex:
// that would be a wait for an Update's function. The holder is an Update, a
// Lock or a Close in this process, and it reads on itself instead. Holding
// flock, a writer has read on to the end of the log, and no other process
// can commit until it lets go. While its Lock still waits for flock, other
// processes can, and Lock reads on between its tries (waiting), so a read
// in this process misses only what was committed since the last of them,
// one pause of at most 16 milliseconds before. Followers wait for each
// other (fol), so a read never comes up short of one that began before it.
//
// The lock order is fol, then the mutex, then flock, then the Target's own
// locks, which for the store are its writer's lock and then the copy's
// lock (S2). An Update takes the mutex and flock (Lock), then begins the
// store's transaction, and holds all three until Unlock. Follow calls the
// Target holding the mutex, so it never applies a batch while an Update's
// transaction is open, and it never applies this process's own commit,
// which Append has already moved the read position past. And since a
// follower never waits for the mutex, anyone may wait for fol: a read made
// through the database inside an Update's function before its first change,
// which S2 allows, calls Follow holding the mutex, waits at most for
// another follower, finds the mutex held, and goes on to read the copy. No
// one waits for a Target's lock while holding fol without also holding the
// mutex, and no one who holds a Target's lock waits for the mutex, so there
// is no cycle. That needs Follow never to be called holding one of the
// Target's own locks, such as inside the store's Read, since a batch it
// applies waits for those.
//
// Reading on. For each batch, a follower reads its head, then the place for
// its marker, and only once the marker is whole the rest of the batch, so
// it never reads a commit still being written (followOn). It applies each
// batch that's complete and marked, stops at the first that isn't, and
// keeps nothing of it for next time.
//
// The writer gone. When Follow stops short of the end of the file, it tries
// the write lock without waiting (tryCheck). When it gets it, whoever left
// what lies past the end of the log has gone, and Follow checks the end of
// the log as Lock does: a batch that counts is written again, synced,
// marked and applied, and what a crash left half written is cut off. When
// another holds the lock, a writer may be at work, and Follow leaves the end
// to it, unless the cheap test F4.md gives a follower finds what may be
// damage at the batch it stopped at: a whole marker after it that names it
// while the batch fails its checks, or names another batch while the batch
// counts. Then Follow waits for the lock, up to Options.Wait, and reads
// again holding it (confirm), as Open does, so it reports damage only when
// it's still there. Other followers don't wait meanwhile: they find the
// mutex held, and read the copy as it is.
//
// A stuck handle (F5) keeps flock and writes nothing until it's closed, so
// no other process can commit meanwhile. Its Follow reads on as any
// follower's does, applying a batch only when it's marked, and stops there:
// it never tries the lock, which it holds already, and never checks the
// end of the log, which would write.

// ErrReplaced means that another file has taken the database's path since
// this Log read the file it holds, a compaction's or a backup moved into
// place, so what was read has to be read again from the file at the path.
// Open reads that one from its start, and Lock moves to it before it
// commits. Follow reports it, and reads nothing: the reload is the caller's
// (F9). Follow reports it too for a file that was empty when it was read
// and has been written into in place since.
var ErrReplaced = errors.New("hypercrux: another file has taken the database's path")

// replaced is the error that wraps ErrReplaced, with the path.
func (l *Log) replaced() error { return fmt.Errorf("%w: %s", ErrReplaced, l.path) }

// spot is how far this process has applied the log: the file, by device and
// inode number, and the end of its log there.
type spot struct {
	dev, ino uint64
	end      int64
}

// publish tells Follow how far this process has applied the log, holding
// the mutex, which every holder does as it lets go (letGo). A Log with no
// file publishes nothing.
func (l *Log) publish() {
	var s *spot
	if l.f != nil && !l.closed {
		s = &spot{dev: l.file.Dev, ino: l.file.Ino, end: l.end}
		if old := l.seen.Load(); old != nil && *old == *s {
			return
		}
	}
	l.seen.Store(s)
}

// Follow reads what other processes have committed since this Log last
// read the file, and hands each new marked batch to the Target, in order.
// It's for a process that keeps the database open while others commit: the
// public package's reads call it before they read the copy (F9). It can be
// called from any goroutine, and from inside an Update's function too.
//
// It stats the database's path first. When the path names the file this
// Log holds, and the file ends where the log this process has applied ends,
// there's nothing to read, and Follow returns without taking a lock. When
// there's more, it reads on from the end of the log as FORMAT.md's "Reading
// the log" says: each batch's head, then its marker, then the rest. It
// applies each batch that's complete and marked, and stops at the first
// that isn't, keeping nothing of it.
//
// When it stops short of the end of the file, it tries the write lock
// without waiting. When it gets it, the writer has gone, and Follow checks
// the end of the log as Lock does before it lets go: a batch that a writer
// left without its marker is written again, synced, marked and handed to
// the Target, and what a crash left half written is cut off. When another
// holds the lock, a writer is at work, and Follow leaves the end as it is,
// unless a whole marker at the end of the batch it stopped at names that
// batch while the batch fails its checks, or names another while the batch
// counts. That can be damage, or a failed commit's batch cut back and
// written over as Follow read it, so Follow waits for the lock, up to
// Options.Wait, reads again holding it, and checks the end of the log, as
// Open does.
//
// The read position belongs to the holder of the write lock's mutex. Follow
// waits for another Follow under way, and when anything else holds the
// mutex, such as an Update in this process, Follow returns nil at once and
// reads nothing. The Update reads on itself: while its Lock waits for
// flock, between its tries, and once it holds flock, to the end of the log.
// So while a Lock waits, a read can miss what was committed since its last
// try, at most 16 milliseconds before, and once it holds flock, nothing.
//
// Follow's errors:
//
//   - another file at the path: an error that wraps ErrReplaced. Nothing is
//     read, and the Log stays on the file it holds, until a reload (F9) or
//     a Lock, which moves to the new file by itself;
//   - a file shorter than the end of the log this process has applied:
//     damage, since nothing before the end of a marker is ever cut, so the
//     file was copied over or cut;
//   - damage found reading on, by the check of the end of the log, or by a
//     read under the lock that agrees with what looked like damage: a
//     *errs.Damage, and nothing in the file is changed;
//   - a wait for the lock that runs out before anything confirmed the
//     damage: an error that wraps errs.ErrLockTimeout and not
//     errs.ErrDamaged;
//   - a write, a sync or a cut that fails in the check: an error, and the
//     end of the log is left for the next check (unfinished);
//   - after Close: errs.ErrClosed.
//
// The batches applied before an error stay applied. With nothing at the
// path, Follow reads on in the file it holds, which a writer that locked it
// before it went may still commit to, and checks nothing. A file that was
// empty when it was read holds no database yet: until a Lock makes one of
// it, in a file of its own, Follow finds nothing to read. If something
// writes into the empty file in place instead, which HyperCrux never does,
// the error wraps ErrReplaced too, since the file has to be read from its
// start. A stuck Log reads on, and stops there: it keeps the lock and
// writes nothing, so it neither tries the lock nor checks the end of the
// log.
func (l *Log) Follow() error {
	there, err := l.fsys.Stat(l.path)
	switch {
	case err == nil:
		if s := l.seen.Load(); s != nil && s.dev == there.Dev && s.ino == there.Ino && s.end == there.Size {
			return nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	l.fol.Lock()
	held := true
	select {
	case l.mu <- struct{}{}:
	default:
		if plant != "logfile/follow-without-mutex" {
			l.fol.Unlock()
			return nil
		}
		held = false
	}
	d, err := l.follow()
	if d == nil || err != nil || !held {
		if held {
			l.letGo()
		}
		l.fol.Unlock()
		return err
	}
	// What looks like damage, which a read holding the write lock has to
	// agree with first. Other followers needn't wait meanwhile: they find
	// the mutex held, and read the copy as it is.
	l.fol.Unlock()
	defer l.letGo()
	return l.confirm(time.Now().Add(l.wait), d)
}

// follow is Follow's work once it holds fol and the mutex: the stat of the
// path, made again now that nothing in this process can move the read
// position, the read on, and the try at the lock when that stops short of
// the end of the file. It returns what may be damage, which Follow reads
// again holding the write lock before it reports it, or nil.
func (l *Log) follow() (*errs.Damage, error) {
	switch {
	case l.closed:
		return nil, l.closedError()
	case l.f == nil:
		return nil, l.replaced() // a switch to another file failed, so the file at the path is still to be read
	}
	there, err := l.fsys.Stat(l.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Nothing at the path. A writer that locked the file before it went
		// can still commit to it, so the size to read to is the file's own.
		if there, err = l.f.Stat(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case !there.Same(l.file) && plant != "logfile/follow-path-unchecked":
		return nil, l.replaced()
	}
	size := there.Size
	switch {
	case l.empty && size == 0:
		return nil, nil // no database yet: the first Lock makes one, in a new file
	case l.empty:
		// Something has written into the empty file in place, which
		// HyperCrux never does, so it's read again from its start, as Lock
		// reads it (fill).
		return nil, l.replaced()
	case size < l.end && plant != "logfile/follow-shrink-unseen":
		return nil, l.shorter(size)
	case size <= l.end:
		return nil, nil
	}
	d, err := l.followOn(size)
	switch {
	case err != nil:
		return nil, err
	case l.end >= size:
		return nil, nil // read to the end of the file
	case l.stuck != nil && plant != "logfile/follow-stuck-checks":
		return nil, nil // it keeps the lock, so no other writer is at work, and it writes nothing
	case plant == "logfile/follow-never-tries":
		return nil, nil
	}
	// Stopped short of the end of the file: the writer may have gone.
	got, err := l.tryCheck()
	if err != nil || got || plant == "logfile/follow-no-confirm" {
		return nil, err
	}
	return d, nil
}

// waiting returns what Lock does between its tries at flock, holding the
// mutex: it reads on, as a follower does, so the reads in this process,
// which read nothing while the mutex is held, see what other processes
// commit while Lock waits. It reads on as far as the file goes when it
// looks, and stops at the first batch that isn't marked. What it can't
// make sense of, damage or a failed read, it leaves to the checks Lock
// makes once it holds flock, and it reads nothing more in this wait. It
// doesn't look at the path: the check that the file locked is the one at
// the path comes once flock is held.
func (l *Log) waiting() func() {
	done := false
	return func() {
		if done || l.empty || plant == "logfile/lock-waits-blind" {
			return
		}
		info, err := l.f.Stat()
		if err != nil || info.Size <= l.end {
			return
		}
		if d, err := l.followOn(info.Size); d != nil || err != nil {
			done = true
		}
		l.publish()
	}
}

// shorter is the damage when the file is size bytes long, shorter than the
// end of the log this process has read from it. Nothing before the end of a
// marker is ever cut, so the file was copied over or cut.
func (l *Log) shorter(size int64) *errs.Damage {
	return &errs.Damage{Path: l.path, Offset: size, Reason: fmt.Sprintf("the file is %d bytes long, shorter than the %d this process has read from it: it was copied over or cut", size, l.end)}
}

// followOn reads on from the end of the log, in a file of size bytes, as a
// follower does, and hands each batch that's complete and marked to the
// Target. It stops at the first batch that isn't, and returns what the
// cheap test finds there, which may be damage, or nil.
//
// For each batch it reads the head, then the place for its marker, and only
// once the marker is whole the rest of the batch, so it never reads a
// commit still being written. probe reads just the bytes asked for when the
// window doesn't hold them, and the batch itself is read through the
// window, so the batches after it come from the same read, and a catch-up
// over many batches takes few reads.
//
// A read through the window can catch a batch whose writer was finishing it
// as it was read, and the marker the writer wrote a moment later: then the
// batch fails its checks while the marker names it. So before the cheap
// test counts that, the batch is read once more from its head, with fresh
// reads, each made after the one before it.
func (l *Log) followOn(size int64) (*errs.Damage, error) {
	defer l.r.release()
	again := false // the batch at the end of the log is being read a second time
	for {
		off, seq := l.end, l.seq+1
		room := size - off
		if room < format.BatchHeadSize {
			return nil, nil
		}
		head, err := l.r.probe(l.f, off, format.BatchHeadSize)
		if err != nil {
			return nil, unread(err) // io.EOF: the file has been cut since its size was read
		}
		n, err := format.BatchLength(head, l.hdr.Gen, seq, room)
		if err != nil {
			return nil, nil // not a batch that counts
		}
		if n+format.MarkerSize > room {
			return nil, l.unmarked(off, n, size, seq) // no room for its marker: a commit under way
		}
		mb, err := l.r.probe(l.f, off+n, format.MarkerSize)
		if err != nil {
			return nil, unread(err)
		}
		var marker [format.MarkerSize]byte
		copy(marker[:], mb)
		m, whole := format.DecodeMarker(marker[:], l.hdr.ID, l.hdr.Gen)
		if !whole {
			return nil, l.unmarked(off, n, size, seq) // a commit under way, or the remains of one
		}
		if plant == "logfile/follow-keeps-unmarked" {
			if bt, ok := keptAt(l, off); ok && m.Seq == seq {
				if err := l.t.Apply(seq, bt.Changes); err != nil {
					return nil, l.refused(off, seq, err)
				}
				l.seq, l.end, l.last = seq, off+n+format.MarkerSize, marker
				continue
			}
		}
		b, err := l.r.read(l.f, off, n, size)
		if err != nil {
			return nil, unread(err)
		}
		bt, err := format.DecodeBatch(b, l.hdr.Gen, seq)
		counts := !errors.Is(err, format.ErrDoesNotCount)
		if counts && err != nil {
			// A batch that counts, whose changes are malformed or break the
			// rules for a change on its own: damage, marked or not, as
			// readOn has it.
			return nil, l.placed(off, err)
		}
		if counts && m == (format.Marker{Seq: seq, Sum: bt.Sum}) {
			if err := l.t.Apply(seq, bt.Changes); err != nil {
				return nil, l.refused(off, seq, err)
			}
			l.seq, l.end, l.last = seq, off+n+format.MarkerSize, marker
			again = false
			continue
		}
		// The cheap test.
		var why string
		switch {
		case counts:
			why = fmt.Sprintf("batch %d counts, with the checksum %#08x, and the whole marker after it names batch %d with the checksum %#08x, which no crash leaves", seq, bt.Sum, m.Seq, m.Sum)
		case m.Seq == seq:
			why = fmt.Sprintf("the whole marker at offset %d names batch %d, which fails its checks", off+n, seq)
		default:
			// A marker naming another batch after one that doesn't count:
			// the check of the end of the log deals with that.
			return nil, nil
		}
		if !again && plant != "logfile/follow-read-once" {
			l.r.release()
			again = true
			continue
		}
		return l.past(why), nil
	}
}

// unmarked is where a follower stops, at batch seq, n bytes long at off in a
// file of size bytes, with no whole marker after it: a commit under way, or
// the remains of one. It keeps nothing of the batch, so the next read takes
// it afresh, whatever has been written there by then. Two planted bugs do
// otherwise.
func (l *Log) unmarked(off, n, size int64, seq uint64) error {
	switch plant {
	case "logfile/follow-before-marker":
		return l.plantedApply(off, n, size, seq)
	case "logfile/follow-keeps-unmarked":
		l.plantedKeep(off, n, size, seq)
	}
	return nil
}
