// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// Where the later file tasks fit in. Each has a comment of its own at the
// place named, starting with the task's name. F3's check of the end of the
// log is in check.go: Lock runs it once the log is read to its end
// (lockFile), and Open once it holds the lock (tryCheck, or confirm). F4's
// look past the end of the log for damage is in damage.go: the check makes
// it at its top (checkEnd), and Open makes it without the lock when another
// holds it, then reads again holding the lock before it reports damage
// (openCheck). F5's failed commits are in failed.go: a commit that fails is
// cut back out of the file before the lock is let go, or the Log keeps the
// lock until Close (stuck), and a failure in the check leaves the end of the
// log for the next check. F6's following of other processes is in
// follow.go: Follow reads on holding the write lock's mutex, which it never
// waits for, and without flock, and when it finds the writer gone, it takes
// flock and checks the end of the log. F8's compaction is in compact.go:
// Compact writes the live data into NAME.compact and renames it over the
// database, holding the lock, Due says when one is due, a waiter for the
// lock waits past its deadline while a compaction runs (takeMutex and
// flock), and the holder of the lock removes a leftover NAME.compact once
// the inode check has passed (lockFile, and checkLocked).
//
//   - F7, the file rules: in Open, before anything else.
//   - F9, reloading: in reopen, between the reset and the read.

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
// calls it from Open, Lock and Follow, on the goroutine that called them,
// one call at a time. Lock and Follow call it holding the write lock's
// mutex. An Update in the public package begins its transaction after Lock
// and ends it before Unlock (G1), holding the mutex throughout, so no call
// comes while an Update's transaction is open, and a transaction the
// Target begins for a batch never waits for one (follow.go).
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
	// together, and how long Open and Follow wait for it when they have to
	// read the file again holding it, to confirm what looks like damage. 0
	// means DefaultWait. The tests set a shorter one. A wait goes on past it
	// while a compaction runs, until Wait after the compaction ends
	// (compact.go).
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
// A transaction that changes nothing calls Unlock without Append. Between
// commits, Follow reads what other processes commit (follow.go).
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

	// fol keeps followers apart, so a follower that finds another reading
	// on waits for it, and reads at least as far. Only Follow takes it, and
	// a follower holding it never waits for the mutex, so anyone may wait
	// for fol, an Update's function included (follow.go).
	fol sync.Mutex

	// seen is how far this process has applied the log, as the mutex's
	// holder last left it: the file and the end of its log, or nil when the
	// Log has no file. Follow reads it without the mutex, to find at the
	// cost of one stat that there's nothing to read.
	seen atomic.Pointer[spot]

	// busy is when a wait for the write lock may end at the earliest, in
	// Unix nanoseconds, since a compaction was under way: Options.Wait after
	// the mutex's holder last found NAME.compact locked while it waited for
	// flock, or after this Log's own compaction ended, and the largest value
	// there is while that runs. A wait for the mutex reads it without the
	// mutex (until, in compact.go).
	busy atomic.Int64

	// mu is the mutex in front of flock, the write lock's first half. It's
	// a channel holding one token at most, so a wait for it can time out.
	// Its holder owns every field below. Open owns them until it returns.
	// Follow takes it too, without waiting (follow.go).
	mu chan struct{}

	f     fsys.File // the database file, or nil after a switch to another file failed, or after Close
	file  fsys.Info // what fstat said about f once it was opened, whose device and inode number say which file it is
	empty bool      // the file was empty when it was read: no database yet, and the fields up to r aren't set
	hdr   format.Header
	head  [format.HeaderSize]byte // the header, as the file holds it
	seq   uint64                  // the last batch read, or 0 when there's none
	end   int64                   // where the log ends: just past the last batch's marker, or the header
	last  [format.MarkerSize]byte // the marker that ends the log, as the file holds it, when seq isn't 0
	r     reader                  // reads the log through a window onto the file

	locked bool   // the write lock is held, both halves
	stuck  error  // set when the lock stays held until Close; every Lock and Append returns it
	tidied bool   // leftover .new- files have been looked for
	closed bool   // Close has been called
	batch  []byte // a commit's batch and marker, built in the same buffer each time
	retry  int64  // after a compaction that failed, the end of the log the next waits for (Due), or 0
	part   int    // the size of a compacted part's batches, when a test sets it; 0 means partSize
}

// Open opens the database at path, or creates it when nothing's there, as
// FORMAT.md's "Creating a database" says, and reads its log. It hands each
// marked batch to t, in order, and stops at the end of the log: the first
// batch that isn't marked, or the end of the file.
//
// Then Open tries the write lock once, without waiting. When it gets it, no
// writer is at work, so it reads on to the end of the log and checks the end
// of the log, as FORMAT.md's "Checking the end of the log" says, before it
// lets go: it looks past the end of the log for damage first, then a batch
// that counts with no whole marker after it, which a writer left when it
// died, is written again, synced, marked and handed to t, and everything
// after the last marker is cut off.
//
// When another holds the lock, that writer checks the end before it appends
// anything, and Open leaves the end as it is. It still looks past the end of
// the log for damage, so damage in the middle of a file is never taken for
// its end. Without the lock, a writer may be at work there, so what Open
// finds is damage only once a read holding the lock agrees: Open waits for
// the lock, up to Options.Wait, as a writer does, and then reads on and
// checks the end of the log holding it, as when it gets the lock at once.
// When the wait runs out, Open fails with an error that wraps
// errs.ErrLockTimeout and says what it found. When another file has taken
// the path by the time Open holds the lock, Open reads that one instead.
//
// An empty file holds no database yet, and Open leaves it as it is: the
// first Lock makes a database of it. A file that isn't a database fails
// with an error that wraps errs.ErrNotDatabase, a 0.x database with
// errs.ErrZeroX, another format version with errs.ErrFormatVersion, and a
// damaged header, a batch that counts whose changes are malformed or that t
// refuses, or damage the check finds, with a *errs.Damage, and nothing in
// the file is changed. When a write, a sync or a cut fails in the check,
// Open fails, and the end of the log is left for the next check, which
// starts again from it (unfinished).
func Open(files fsys.FS, path string, t Target, o Options) (*Log, error) {
	l := &Log{fsys: files, path: path, dir: filepath.Dir(path), t: t, wait: o.Wait, id: o.ID, mu: make(chan struct{}, 1)}
	if l.wait <= 0 {
		l.wait = DefaultWait
	}
	var deadline time.Time // for the wait for the write lock, when Open has to read again holding it
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
			l.publish()
			return l, nil
		}
		if err != nil {
			return nil, err
		}
		l.f = f
		size, err := l.load()
		if err != nil {
			f.Close()
			return nil, err
		}
		// The look past the end of the log, whether or not Open gets the
		// lock, and the check of the end once it holds it (damage.go).
		err = l.openCheck(size, &deadline)
		if errors.Is(err, ErrReplaced) {
			// Another file took the path before Open held the lock: read that
			// one from its start instead.
			f.Close()
			l.f = nil
			l.t.Reset()
			continue
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		l.publish()
		return l, nil
	}
	return nil, fmt.Errorf("hypercrux: %s: a file kept appearing at the path and going again while it was opened", path)
}

// Lock takes the write lock: the mutex inside the process, then flock on
// the database file, waiting up to Options.Wait for the two together.
// When the lock doesn't come free in time, it fails with an error that
// wraps errs.ErrLockTimeout. While a compaction runs, in this process or
// another, the wait goes on past that, until Options.Wait after the
// compaction ends (compact.go). While it waits for flock, holding the
// mutex, it reads on between its tries, as Follow would, handing the Target
// what other processes commit meanwhile, since Follow reads nothing while
// the mutex is held (follow.go).
//
// Holding both, it checks by device and inode number that the file it
// locked is still the one at the path. When another file has taken the
// path, a compaction's or a backup moved into place, Lock lets go of the
// old file, calls the Target's Reset, reads the new file from its start,
// and takes the lock there instead: the commit starts again on the new
// file. Once the file it locked is the one at the path, it removes a
// leftover NAME.compact, which a compaction left when its process died,
// unless it's locked. An empty file at the path becomes a database now, as
// FORMAT.md's "Creating a database" says. Then Lock checks that what this
// Log has read is still in the file, as FORMAT.md's "Writing" asks, reads
// on to the end of the log, handing each new marked batch to the Target,
// and checks the end of the log, as Open does when it gets the lock: it
// looks past the end of the log for damage, then a batch a writer left
// without its marker when it died is written again, synced, marked and
// handed to the Target, before the caller's transaction runs, and what a
// crash left half written is cut off. The first Lock of each Log also
// removes leftover .new- files.
//
// Damage the check finds is a *errs.Damage, and nothing is changed. When a
// write, a sync or a cut fails in the check, Lock fails, and the end of the
// log is left as it is for the next check, which starts again from it
// (unfinished). A batch whose commit may have succeeded is never cut that
// way.
//
// On success, the caller holds the lock until Unlock. When Lock fails, the
// caller holds nothing, and a later Lock tries again, unless the error
// wraps errs.ErrStuck: then this Log keeps the lock until Close, and every
// Lock gives the same error at once. That happens when a commit failed and
// couldn't be cut back out of the file (failed), when an empty file was made
// a database and the folder's sync failed after the rename (adopt), and when
// the folder's sync failed after a compaction's rename (Compact).
func (l *Log) Lock() error {
	deadline := time.Now().Add(l.wait)
	if err := l.takeMutex(deadline); err != nil {
		return err
	}
	switch {
	case l.closed:
		l.letGo()
		return l.closedError()
	case l.stuck != nil:
		l.letGo()
		return l.stuck
	}
	if err := l.lockFile(deadline); err != nil {
		l.letGo()
		return err
	}
	l.locked = true
	// Caught up, holding flock, so nothing is committed elsewhere until
	// Unlock: Follow's look without the mutex finds nothing to read.
	l.publish()
	return nil
}

// letGo lets go of the mutex, once it has told Follow how far this process
// has applied the log (publish).
func (l *Log) letGo() {
	l.publish()
	<-l.mu
}

// takeMutex waits for the mutex until deadline, or for longer while the
// mutex's holder compacts, or waits for flock while another process
// compacts, until Options.Wait after the compaction ends (until).
func (l *Log) takeMutex(deadline time.Time) error {
	select {
	case l.mu <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case l.mu <- struct{}{}:
			return nil
		case <-timer.C:
		}
		left := time.Until(l.until(deadline))
		if left <= 0 {
			return l.timeout()
		}
		timer.Reset(min(left, l.wait)) // while a compaction runs here, looks again after each wait
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
		if err := l.flock(deadline, l.waiting()); err != nil {
			return err
		}
		if plant == "logfile/compact-leftover-before-inode-check" {
			l.removeCompact()
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
		// Now that the file locked is the one at the path, nobody else holds
		// the write lock, so a NAME.compact that isn't locked is a leftover
		// (compact.go).
		if plant != "logfile/compact-leftover-before-inode-check" {
			l.removeCompact()
		}
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
			// Damage, a failed read, or a failed write, sync or cut in the
			// check, which leaves the end of the log for the next check: the
			// lock goes either way.
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
// After each pause, before it tries again, it calls between, when that
// isn't nil. A compaction under way holds the lock for as long as it takes
// to write and sync the live data, so each time flock finds the lock held,
// it looks for one, and while NAME.compact is locked, the wait goes on past
// deadline, until Options.Wait after it was last found locked (until). That
// covers the moments after the compaction's rename too, while the folder is
// synced and the locks let go.
func (l *Log) flock(deadline time.Time, between func()) error {
	pause := time.Millisecond
	for {
		ok, err := l.f.TryLock()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if l.compacting() {
			l.hold(time.Now().Add(l.wait))
		}
		left := time.Until(l.until(deadline))
		if left <= 0 || plant == "logfile/one-try" {
			return l.timeout()
		}
		time.Sleep(min(pause, left))
		pause = min(2*pause, maxPause)
		if between != nil {
			between()
		}
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
	if _, err := l.load(); err != nil {
		l.f = nil
		f.Close()
		return err
	}
	return nil
}

// Unlock lets go of the write lock, in the reverse order: flock, then the
// mutex. A Log that's stuck keeps flock until Close, and lets go of the
// mutex alone.
func (l *Log) Unlock() error {
	if !l.locked {
		return fmt.Errorf("hypercrux: %s: Unlock without the write lock", l.path)
	}
	l.locked = false
	var err error
	if l.f != nil && (l.stuck == nil || plant == "logfile/stuck-lets-go") {
		err = l.f.Unlock()
	}
	l.letGo()
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
// file.
//
// When the batch's write, the sync or the marker's write fails, Append cuts
// the file back to just after the last marker before it returns (failed),
// and the error wraps the call's. Once that has worked, the commit is gone
// for good, and the next Append, under this lock or a later one, goes in
// where it was. When the cut back fails too, the Log keeps the lock until
// Close, the error wraps errs.ErrStuck as well, and the commit's outcome is
// unknown. Every Append and Lock after that fails with an error that wraps
// errs.ErrStuck.
func (l *Log) Append(changes []format.Change) error {
	switch {
	case !l.locked:
		return fmt.Errorf("hypercrux: %s: Append without the write lock", l.path)
	case l.stuck != nil:
		return l.stuck
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
		return l.failed(err, seq)
	}
	if plant == "logfile/marker-before-sync" {
		l.f.WriteAt(marker, at+int64(n))
	}
	if err := l.f.Sync(); err != nil {
		return l.failed(err, seq)
	}
	if _, err := l.f.WriteAt(marker, at+int64(n)); err != nil {
		return l.failed(err, seq)
	}
	l.seq = seq
	l.end = at + int64(len(b))
	copy(l.last[:], marker)
	// When Due says so after the commit, the public package compacts, still
	// holding the lock, from a read of the copy once the copy's transaction
	// has ended (compact.go).
	return nil
}

// Close closes the database file, which lets go of its flock. It waits for
// a commit under way in another goroutine to end, so the caller mustn't
// hold the write lock itself. After Close, Lock fails with an error that
// wraps errs.ErrClosed.
func (l *Log) Close() error {
	l.mu <- struct{}{}
	defer l.letGo()
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
