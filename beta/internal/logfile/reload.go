// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"time"
)

// Reloading (F9): BETA.md's "Compaction" and "Other processes". When another
// file has taken the database's path, a compaction's or a backup moved into
// place, what this process has read is no longer the database, and the file
// at the path is read from its start. The old file is closed, the Target
// drops what it holds, and a garbage collection runs before the new file is
// read, so memory never holds the old copy and the new one at once (drop).
// Then the new file's log is read, handing every marked batch to the Target,
// and checked as Open checks it.
//
// Three calls do it. Open starts again when another file takes the path
// while it reads (Open's loop), Lock moves to the new file by itself before
// it commits (reopen, and fill for an empty file written into in place), and
// Reload does it for a process's reads, once Follow has reported
// ErrReplaced. Follow itself never reloads: it reads nothing and reports
// ErrReplaced, and the caller reloads, then follows again.
//
// Reload keeps Follow's place in the lock order. It takes fol, then tries the
// write lock's mutex without waiting, and when anything but a follower holds
// the mutex, it reads nothing and returns at once: an Update, whose Lock
// moves to the new file by itself, or a Close. So a read never waits for an
// Update's function. Holding both, it reloads, and other followers in the
// process wait for it at fol, so a read never finds the Log half moved.
//
// The Target hears of a reload through Reset, then the batches of the new
// file from its first. A Target that holds readers of its own, such as the
// public package's copy, has to keep them off what it holds from the Reset
// until the log is done with the new file, or they would see it fill. So a
// Target that's a Reloader hears that too: Loaded, once the log has read the
// new file's log and, in Open and Reload, checked its end, or failed to.
// Every Reset is followed by one Loaded, or several Resets by one Loaded when
// other files kept taking the path meanwhile.
//
// When a reload fails, the Target holds the batches the log handed it before
// the failure, and the Log holds no file. Then Follow reports ErrReplaced
// again, and the next Reload, or Lock, reads the file at the path from its
// start once more.

// Reloader is a Target that wants to know when the log is done with a file
// it read from its start after a Reset, so that it can keep its own readers
// off what it holds meanwhile.
type Reloader interface {
	Target

	// Loaded says the log is done with the file it read from its start since
	// the last Reset: it has read the file's log and, in Open and Reload,
	// checked its end. err is nil when that worked, or why it didn't, and
	// then the Target has been handed the batches before the failure. The
	// log calls Loaded from the calls that call Reset, on the same goroutine,
	// before they return, and in Lock before it waits for flock on the new
	// file, so the batches that come after Loaded are the ones Lock and
	// Follow read on to, one at a time.
	Loaded(err error)
}

// drop has the Target drop what it holds, since a file is to be read from
// its start, and runs a garbage collection, so the old copy has gone before
// the new file is read into memory. The Target is owed a Loaded from then
// on (loaded).
func (l *Log) drop() {
	l.t.Reset()
	l.owed = true
	if plant != "logfile/reload-without-collecting" {
		runtime.GC()
	}
}

// loaded tells the Target that the log is done with the file it read from
// its start, with err, when a drop since the last call owes it that and the
// Target is a Reloader.
func (l *Log) loaded(err error) {
	if !l.owed {
		return
	}
	l.owed = false
	if r, ok := l.t.(Reloader); ok {
		r.Loaded(err)
	}
}

// Reload reads the file at the database's path from its start, once another
// file has taken the path since this Log read the one it holds, a
// compaction's or a backup moved into place, as Follow reports with an
// error that wraps ErrReplaced. It's for a process's reads: they call
// Follow, and Reload when Follow asks for it, then Follow again (F9). It
// can be called from any goroutine.
//
// Holding the write lock's mutex, it opens the file at the path, closes the
// one the Log holds, calls the Target's Reset, runs a garbage collection so
// memory doesn't hold the old copy and the new one at once, reads the new
// file's log, handing each marked batch to the Target, and checks it as Open
// does: it tries the write lock without waiting, and holding it checks the
// end of the log, or else looks past the end of the log for damage, and
// waits for the lock to read again what looks like damage, up to
// Options.Wait. When another file takes the path meanwhile, it reads that
// one from its start instead. Then, when the Target is a Reloader, it calls
// Loaded.
//
// Reload never waits for the mutex. It waits for another Follow or Reload
// under way, and when anything else holds the mutex, it reads nothing and
// returns nil at once, since that would be a wait for an Update's function:
// the Update's Lock moves to the new file by itself. So does a Reload that
// finds the Log on the file at the path already, or nothing at the path,
// when Follow reads on in the file it holds.
//
// Its errors are Open's for the new file: one that wraps errs.ErrNotDatabase,
// errs.ErrZeroX or errs.ErrFormatVersion, a *errs.Damage, a lock timeout that
// wraps errs.ErrLockTimeout, or a read, write, sync or cut that failed. Then
// the Target holds the batches handed to it before the failure, and the Log
// holds no file, so Follow reports ErrReplaced and the next Reload or Lock
// tries again. A stuck Log keeps its file and its lock until
// it's closed, so its Reload fails with an error that wraps errs.ErrStuck,
// and after Close Reload gives errs.ErrClosed.
func (l *Log) Reload() error {
	l.fol.Lock()
	defer l.fol.Unlock()
	if plant == "logfile/reload-waits-for-the-mutex" {
		l.mu <- struct{}{}
	} else {
		select {
		case l.mu <- struct{}{}:
		default:
			return nil // an Update, whose Lock moves to the new file itself, or a Close
		}
	}
	defer l.letGo()
	return l.reload()
}

// reload is Reload's work, holding fol and the mutex.
func (l *Log) reload() error {
	if l.closed {
		return l.closedError()
	}
	stale, err := l.stale()
	if err != nil || !stale {
		return err
	}
	if l.stuck != nil {
		return fmt.Errorf("%w; another file has taken the database's path, and a stuck handle keeps its own file until it's closed", l.stuck)
	}
	var deadline time.Time // for the wait for the write lock, when the check has to read again holding it
	for range maxTries {
		f, err := l.fsys.Open(l.path)
		if err != nil {
			// The first time, nothing has changed: the Log keeps the file it
			// holds, if any, and the Target what it holds. After another file
			// took the path while the reload read, the Target holds that one,
			// which is no longer at the path either.
			if l.owed && l.f != nil {
				l.f.Close()
				l.f = nil
			}
			l.loaded(err)
			return err
		}
		if l.f != nil {
			l.f.Close()
		}
		l.f = f
		l.drop()
		size, err := l.load()
		if err == nil && plant != "logfile/reload-unchecked" {
			err = l.openCheck(size, &deadline)
		}
		if errors.Is(err, ErrReplaced) {
			continue // another file took the path while this one was read: read that one instead
		}
		if err != nil {
			l.f.Close()
			l.f = nil
		}
		l.loaded(err)
		return err
	}
	err = fmt.Errorf("hypercrux: %s: other files kept taking the database's path while it was reloaded", l.path)
	l.f.Close()
	l.f = nil
	l.loaded(err)
	return err
}

// stale reports whether the file this Log holds is no longer the one to
// read, so that a reload reads the file at the path: the Log holds no file,
// since a switch to another failed; another file has taken the path; or the
// empty file the Log read has been written into in place, which HyperCrux
// never does, as Follow reports. With nothing at the path, the Log's own file
// is still the one to read, as Follow reads on in it.
func (l *Log) stale() (bool, error) {
	if l.f == nil {
		return true, nil
	}
	there, err := l.fsys.Stat(l.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !there.Same(l.file):
		return true, nil
	}
	return l.empty && there.Size > 0, nil
}
