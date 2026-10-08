// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// Where the later file tasks fit in. Each has a comment of its own at the
// place named, starting with the task's name. F3's check of the end of the
// log is in check.go: Lock runs it once the log is read to its end
// (lockFile), and Open when it gets the lock without waiting (tryCheck).
//
//   - F4, damage: where reading stops (readOn), and at the top of the check
//     (checkEnd), a look past the end of the log for a whole marker naming
//     the next batch or a later one, and in a compacted file a log that
//     ends before the compacted part does, for readers. A marked batch that
//     fails its checks is read again under the lock. Until F4, the check
//     cuts whatever doesn't count.
//   - F5, failed commits: Append's failures, and the check's, all go
//     through failed, which cuts the file back to the end of the log and
//     syncs, or keeps the lock and sets stuck. stuck is already how a
//     handle keeps the lock until Close, when an empty file became a
//     database and the folder's sync failed (adopt). Until F5, a failure
//     leaves its bytes where they are, and this Log appends nothing after
//     them (tail, and catchUp).
//   - F6, following other processes: a call that reads on without the
//     lock, made before each read: stat the path, read each batch's head,
//     then its marker, then the rest (format.BatchLength), and try the lock
//     without waiting when the log stops short of the end of the file,
//     which is tryCheck. The fields that say how far the log has been read
//     are the mutex holder's for now, so F6 decides how a reader shares
//     them: it can skip reading on while the mutex is held, since nothing
//     can be committed then.
//   - F7, the file rules: in Open, before anything else.
//   - F8, compaction: after a commit, holding the lock, and in flock, which
//     waits past the deadline while NAME.compact is locked.
//   - F9, reloading: in reopen, between the reset and the read, and in
//     lockFile, where a leftover NAME.compact goes once the inode check
//     has passed.

// DefaultWait is how long Lock waits for the write lock, as 0.x does.
const DefaultWait = 10 * time.Second

// maxPause is the longest pause between two tries at flock. The first
// pause is a millisecond, and each one after it twice as long.
const maxPause = 16 * time.Millisecond

// maxTries bounds the loops that start again when something changes under
// them: a name taken, a file appearing or going, another file taking the
// path. Each needs another process to win a race, so a loop that reaches
// this many tries has met something that isn't HyperCrux.
const maxTries = 100

// Target is where the log hands what it reads: the in-memory copy, which
// the public package joins to the log (G1), or a test's recorder. The log
// calls it from Open and Lock, on the goroutine that called them, one call
// at a time.
type Target interface {
	// Apply applies one marked batch: its sequence number, and its
	// changes, in order. The changes are the Target's to keep: nothing
	// changes them, and every string in them is a slice of one string
	// that holds the whole batch (format.DecodeBatch).
	//
	// An error means the changes break the rules for the state they apply
	// to, which a crash can't account for, so the log reports the file as
	// damaged, naming the batch. A Target applies a batch whole or not at
	// all (S3).
	Apply(seq uint64, changes []format.Change) error

	// Reset drops everything applied so far, since another file has taken
	// the database's path, a compaction's or a backup moved into place.
	// The batches of that file follow, from its first.
	Reset()
}

// Options holds a Log's settings.
type Options struct {
	// Wait is how long Lock waits for the write lock, the mutex and flock
	// together. 0 means DefaultWait. The tests set a shorter one.
	Wait time.Duration

	// ID, when it isn't all zeros, is the database ID that a database this
	// Log creates gets, in place of 16 random bytes: at Open when nothing is
	// at the path, and at the first Lock when an empty file becomes a
	// database. A database already there keeps its own. The crash tests set
	// it, so a file holds the same bytes in every run of a workload, and a
	// crash point replays from its seed: every marker's check value covers
	// the ID, and where a torn sector splits depends on the bytes it holds.
	ID [16]byte
}

// Log is a database file opened through fsys: what its header says, how
// far its log has been read, and the write lock.
//
// Lock, Append and Unlock make a commit. The caller's transaction runs
// between Lock and Append, holding the lock, and Lock first reads what
// other processes have committed, handing it to the Target, so the
// transaction starts from the latest state, as 0.x's BEGIN IMMEDIATE does.
// A transaction that changes nothing calls Unlock without Append.
//
// A Log can be used by many goroutines at once: the write lock keeps them
// apart.
type Log struct {
	fsys fsys.FS
	path string
	dir  string // the folder the database is in, which creation syncs
	t    Target
	wait time.Duration
	id   [16]byte // Options.ID

	// mu is the mutex in front of flock, the write lock's first half. It's
	// a channel holding one token at most, so a wait for it can time out.
	// Its holder owns every field below. Open owns them until it returns.
	mu chan struct{}

	f     fsys.File // the database file, or nil after a switch to another file failed, or after Close
	empty bool      // the file was empty when it was read: no database yet, and the fields up to r aren't set
	hdr   format.Header
	head  [format.HeaderSize]byte // the header, as the file holds it
	seq   uint64                  // the last batch read, or 0 when there's none
	end   int64                   // where the log ends: just past the last batch's marker, or the header
	last  [format.MarkerSize]byte // the marker that ends the log, as the file holds it, when seq isn't 0
	r     reader                  // reads the log through a window onto the file

	locked bool   // the write lock is held, both halves
	tail   bool   // a failed commit or check left bytes past the end of the log, which this Log leaves alone and appends nothing after until F5 cuts them (catchUp)
	stuck  error  // set when the lock stays held until Close; every Lock returns it
	tidied bool   // leftover .new- files have been looked for
	closed bool   // Close has been called
	batch  []byte // a commit's batch and marker, built in the same buffer each time
}

// Open opens the database at path, or creates it when nothing's there, as
// FORMAT.md's "Creating a database" says, and reads its log. It hands each
// marked batch to t, in order, and stops at the end of the log: the first
// batch that isn't marked, or the end of the file.
//
// Then Open tries the write lock once, without waiting. When it gets it, no
// writer is at work, so it reads on to the end of the log and checks the end
// of the log, as FORMAT.md's "Checking the end of the log" says, before it
// lets go: a batch that counts with no whole marker after it, which a writer
// left when it died, is written again, synced, marked and handed to t, and
// everything after the last marker is cut off. When another holds the lock,
// that writer checks before it appends anything, and Open leaves the end as
// it is.
//
// An empty file holds no database yet, and Open leaves it as it is: the
// first Lock makes a database of it. A file that isn't a database fails
// with an error that wraps errs.ErrNotDatabase, a 0.x database with
// errs.ErrZeroX, another format version with errs.ErrFormatVersion, and a
// damaged header, a batch that counts whose changes are malformed or that t
// refuses, or damage the check finds, with a *errs.Damage. A write, a sync
// or a cut that fails in the check is an error saying its outcome is
// unknown.
func Open(files fsys.FS, path string, t Target, o Options) (*Log, error) {
	l := &Log{fsys: files, path: path, dir: filepath.Dir(path), t: t, wait: o.Wait, id: o.ID, mu: make(chan struct{}, 1)}
	if l.wait <= 0 {
		l.wait = DefaultWait
	}
	// F7 goes first: the path made real, with every symbolic link
	// resolved, and a file with more than one name, or one that isn't a
	// regular file, refused.
	for range maxTries {
		f, err := files.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			err = l.create()
			if errors.Is(err, errAppeared) {
				continue // another creator's database is there now: open that
			}
			if err != nil {
				return nil, err
			}
			return l, nil
		}
		if err != nil {
			return nil, err
		}
		l.f = f
		if err := l.load(); err != nil {
			f.Close()
			return nil, err
		}
		// F4 goes here: a look past the end of the log, which reports
		// damage when it finds a whole marker naming the next batch or a
		// later one, or in a compacted file a log that ends before the
		// compacted part does, whether or not Open gets the lock below.
		if err := l.tryCheck(); err != nil {
			f.Close()
			return nil, err
		}
		return l, nil
	}
	return nil, fmt.Errorf("hypercrux: %s: a file kept appearing at the path and going again while it was opened", path)
}

// Lock takes the write lock: the mutex inside the process, then flock on
// the database file, waiting up to Options.Wait for the two together.
// When the lock doesn't come free in time, it fails with an error that
// wraps errs.ErrLockTimeout.
//
// Holding both, it checks by device and inode number that the file it
// locked is still the one at the path. When another file has taken the
// path, a compaction's or a backup moved into place, Lock lets go of the
// old file, calls the Target's Reset, reads the new file from its start,
// and takes the lock there instead: the commit starts again on the new
// file. An empty file at the path becomes a database now, as FORMAT.md's
// "Creating a database" says. Then Lock checks that what this Log has read
// is still in the file, as FORMAT.md's "Writing" asks, reads on to the end
// of the log, handing each new marked batch to the Target, and checks the
// end of the log, as Open does when it gets the lock: a batch a writer left
// without its marker when it died is written again, synced, marked and
// handed to the Target, before the caller's transaction runs, and what a
// crash left half written is cut off. The first Lock of each Log also
// removes leftover .new- files.
//
// Damage the check finds is a *errs.Damage, and nothing is changed. A
// write, a sync or a cut that fails in the check is handled as a failed
// commit is (failed).
//
// On success, the caller holds the lock until Unlock. When Lock fails, the
// caller holds nothing, and a later Lock tries again, unless the error
// wraps errs.ErrStuck: then this Log keeps the lock until Close.
func (l *Log) Lock() error {
	deadline := time.Now().Add(l.wait)
	if err := l.takeMutex(deadline); err != nil {
		return err
	}
	switch {
	case l.closed:
		<-l.mu
		return l.closedError()
	case l.stuck != nil:
		<-l.mu
		return l.stuck
	}
	if err := l.lockFile(deadline); err != nil {
		<-l.mu
		return err
	}
	l.locked = true
	return nil
}

// takeMutex waits for the mutex until deadline at most.
func (l *Log) takeMutex(deadline time.Time) error {
	select {
	case l.mu <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case l.mu <- struct{}{}:
		return nil
	case <-timer.C:
		return l.timeout()
	}
}

// lockFile takes flock on the database file, holding the mutex, and makes
// the checks Lock describes. It starts again on the file at the path for
// as long as another file keeps taking its place. On an error it holds no
// flock, unless stuck is set.
func (l *Log) lockFile(deadline time.Time) error {
	for range maxTries {
		if l.f == nil {
			if err := l.reopen(); err != nil {
				return err
			}
		}
		if err := l.flock(deadline); err != nil {
			return err
		}
		info, same, err := l.atPath()
		if err != nil {
			l.f.Unlock()
			return err
		}
		if !same {
			// Another file has taken the path. Drop this one; the next try
			// reads the new one from its start and locks it.
			l.f.Unlock()
			l.f.Close()
			l.f = nil
			continue
		}
		// F9 goes here: now that the file locked is the one at the path,
		// a leftover NAME.compact is removed.
		if l.empty {
			if err := l.fill(info); err != nil {
				return err
			}
			continue // the file is a database now: lock and check it as one
		}
		if err := l.checkRead(info); err != nil {
			l.f.Unlock()
			return err
		}
		if !l.tidied {
			l.removeLeftovers()
			l.tidied = true
		}
		if err := l.catchUp(info.Size); err != nil {
			// F5 goes here: a failed write, sync or cut in the check keeps
			// the lock when cutting back fails too (stuck).
			l.f.Unlock()
			return err
		}
		return nil
	}
	if l.f != nil {
		l.f.Unlock()
	}
	return fmt.Errorf("hypercrux: %s: other files kept taking the database's path", l.path)
}

// flock takes flock on l.f, trying again after a pause until deadline.
func (l *Log) flock(deadline time.Time) error {
	pause := time.Millisecond
	for {
		ok, err := l.f.TryLock()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		// F8 goes here: a writer that finds NAME.compact locked waits past
		// the deadline, until the compaction ends.
		left := time.Until(deadline)
		if left <= 0 || plant == "logfile/one-try" {
			return l.timeout()
		}
		time.Sleep(min(pause, left))
		pause = min(2*pause, maxPause)
	}
}

// atPath stats the locked file and the path, and reports whether they're
// the same file, by device and inode number. It returns what fstat said
// about the locked file, whose size nothing can change while the lock is
// held.
func (l *Log) atPath() (fsys.Info, bool, error) {
	mine, err := l.f.Stat()
	if err != nil {
		return mine, false, err
	}
	if plant == "logfile/no-inode-check" {
		return mine, true, nil
	}
	there, err := l.fsys.Stat(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return mine, false, fmt.Errorf("hypercrux: %s: the database's file has gone from its path: %w", l.path, err)
	}
	if err != nil {
		return mine, false, err
	}
	return mine, mine.Same(there), nil
}

// reopen opens the file at the path, once another file has taken it, and
// reads it from its start, after the Target drops what it had. When it
// fails, l has no file, and the next Lock tries again.
func (l *Log) reopen() error {
	f, err := l.fsys.Open(l.path)
	if err != nil {
		return err
	}
	l.f = f
	l.t.Reset()
	// F9 goes here: a garbage collection between the reset and the read,
	// so the process doesn't hold the old copy and the new one at once.
	if err := l.load(); err != nil {
		l.f = nil
		f.Close()
		return err
	}
	return nil
}

// Unlock lets go of the write lock, in the reverse order: flock, then the
// mutex.
func (l *Log) Unlock() error {
	if !l.locked {
		return fmt.Errorf("hypercrux: %s: Unlock without the write lock", l.path)
	}
	l.locked = false
	var err error
	if l.f != nil && l.stuck == nil {
		err = l.f.Unlock()
	}
	<-l.mu
	return err
}

// Append commits changes as the next batch, holding the write lock: it
// writes the batch at the end of the log, syncs the file, then writes the
// batch's marker after it. The marker needs no sync of its own (FORMAT.md,
// "Markers"). The changes are the ones the Target already holds, so Append
// doesn't hand them to it.
//
// Changes that break FORMAT.md's rules for a change on its own are refused
// with an error that wraps errs.ErrInvalid, before anything reaches the
// file. An error from a write or the sync means the commit's outcome is
// unknown.
func (l *Log) Append(changes []format.Change) error {
	switch {
	case !l.locked:
		return fmt.Errorf("hypercrux: %s: Append without the write lock", l.path)
	case l.tail:
		return fmt.Errorf("hypercrux: %s: the log ends at offset %d, and a failed write left bytes past it; nothing is appended after them until a failed write is cut back out of the file (task F5): %w", l.path, l.end, errors.ErrUnsupported)
	}
	seq := l.seq + 1
	b, sum, err := format.AppendBatch(l.batch[:0], l.hdr.Gen, seq, changes)
	if err != nil {
		return err
	}
	n := len(b)
	b = format.AppendMarker(b, l.hdr.ID, l.hdr.Gen, format.Marker{Seq: seq, Sum: sum})
	if cap(b) <= window {
		l.batch = b // kept for the next commit, unless it's grown large
	}
	batch, marker := b[:n], b[n:]
	at := l.end

	if _, err := l.f.WriteAt(batch, at); err != nil {
		return l.failed(err, "committing batch %d, whose outcome is unknown", seq)
	}
	if plant == "logfile/marker-before-sync" {
		l.f.WriteAt(marker, at+int64(n))
	}
	if err := l.f.Sync(); err != nil {
		return l.failed(err, "committing batch %d, whose outcome is unknown", seq)
	}
	if _, err := l.f.WriteAt(marker, at+int64(n)); err != nil {
		return l.failed(err, "committing batch %d, whose outcome is unknown", seq)
	}
	l.seq = seq
	l.end = at + int64(len(b))
	copy(l.last[:], marker)
	// F8 goes here, or in the public package after Append: when the file
	// holds about twice the live data, a compaction, still holding the
	// lock.
	return nil
}

// failed reports a write or a sync that failed, holding the lock: in a
// commit (Append), or in the check of the end of the log, while it writes a
// batch again, syncs it, marks it, or cuts the end and syncs the cut. The
// check's failures are handled as a failed commit's are (FORMAT.md,
// "Checking the end of the log"). what says what failed, as a format for
// args.
//
// F5 goes here: the file is cut back to the end of the log and the cut is
// synced, or when that fails, the lock stays held and writes are refused
// until Close (stuck). Until F5, nothing is cut, and the bytes left past the
// end of the log stay where they are. This Log appends nothing after them,
// and doesn't check them, for as long as they're there (catchUp). Another
// Log's check deals with them as it would with what a crash left: it cuts
// them, or when they're a batch that counts, it marks the batch, since a
// failed commit's outcome is unknown.
func (l *Log) failed(err error, what string, args ...any) error {
	l.tail = true
	return fmt.Errorf("hypercrux: %s: %s: %w", l.path, fmt.Sprintf(what, args...), err)
}

// Close closes the database file, which lets go of its flock. It waits for
// a commit under way in another goroutine to end, so the caller mustn't
// hold the write lock itself. After Close, Lock fails with an error that
// wraps errs.ErrClosed.
func (l *Log) Close() error {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	if l.closed {
		return l.closedError()
	}
	l.closed = true
	l.stuck = nil
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func (l *Log) timeout() error {
	return fmt.Errorf("%w: %s, after %v", errs.ErrLockTimeout, l.path, l.wait)
}

func (l *Log) closedError() error {
	return fmt.Errorf("%w: %s", errs.ErrClosed, l.path)
}
