// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"math"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Compaction (F8): BETA.md's "Compaction", and FORMAT.md's. The holder of the
// write lock writes the live data into a new file beside the database,
// NAME.compact, and renames it over the database. The old file never changes
// again, so a copy taken at any moment holds the one file or the other.
//
//  1. NAME.compact is made with the database's permissions and, where the
//     process may give it one, the database's owner, and locked before
//     anything is written to it (startCompact).
//  2. The live data goes into it from offset 68 as ordinary batches, each
//     with its marker, from a snapshot of the in-memory copy in FORMAT.md's
//     order: every table with its records, then every link. A batch ends at
//     the change that would take it past about a megabyte, and the links
//     start a batch of their own, as in P2's fixture (writeCompact). Then
//     comes the header, which names the next generation, the batch the file
//     continues from, and where the compacted part ends. That offset is
//     known only now, so the header is written last.
//  3. The new file is synced, once. Nobody can read it before the rename, so
//     its markers need no sync of their own.
//  4. Just before the rename, the file at the path is checked to be the one
//     this Log holds, so a backup moved into place meanwhile is never
//     replaced, and by the file rules to have one name still, at a path
//     whose last element hasn't become a symbolic link (rules.go). The
//     rename over the database is the switch, and the folder's sync makes it
//     last.
//  5. The Log moves over to the new file (switchTo). The old file is closed,
//     which lets go of its lock, and the caller's Unlock lets go of the new
//     file's lock and then the mutex, in BETA.md's order. A writer waiting
//     for the old file's lock gets it, finds another file at the path, and
//     starts again there, waiting for the new file's lock.
//
// The snapshot comes from a read of the copy after the commit that set the
// compaction off, with the write lock still held, so it holds exactly what
// the log holds, and this process's copy needs no reload: the Target isn't
// told. Other processes find another file at the path, and reload (F9).
//
// A failure before the switch, in any of steps 1 to 4, removes NAME.compact,
// and the Log carries on with the old file, which is whole (dropCompact). A
// failed folder sync after the rename can leave either file at the path after
// a power cut, so the Log moves over to the new file and keeps its lock until
// Close, as it does when a failed commit can't be cut back (stuck). Either
// way the commit before the compaction has succeeded.
//
// A crash before the rename leaves a NAME.compact behind. The next holder of
// the write lock removes it, once it has checked that the file it locked is
// still the one at the path, unless it's locked (removeCompact). Nobody else
// removes it.
//
// Writers wait while a compaction runs. One in another process finds
// NAME.compact locked between its tries at flock (compacting), and one in
// this process finds the compaction's Log has noted it (busy). Either way the
// wait goes on past its deadline, until Options.Wait after the compaction was
// last seen under way (until), so it ends once the compaction has, and a
// handle stuck after its compaction holds writers off no longer than one
// that's stuck after a failed commit.

// partSize is about how many bytes of changes a batch of the compacted part
// holds: a batch ends at the change that would take it past this, and a
// change longer than this goes in a batch of its own. Batches of this size
// keep the memory a compaction takes flat, whatever the size of the database.
const partSize = 1 << 20

// minDead is how far, at least, the end of the log must be past the live
// data before a compaction is due, so a small database isn't compacted after
// every few commits. It's a megabyte, like a batch of the compacted part.
const minDead = 1 << 20

// compactName is the compaction's file: the database's path with .compact
// added.
func (l *Log) compactName() string { return l.path + ".compact" }

// Due reports whether a compaction is due by BETA.md's rule of thumb, a file
// holding about twice as much as its live data. The live data it measures is
// the most the log knows of without a pass over the copy: what the last
// compaction wrote, which is the file up to the end of its compacted part, or
// the header alone in generation 1. A compaction is due once the end of the
// log is at least twice as far into the file as that, and at least a
// megabyte past it. Every process reads the same from the file, so every
// process agrees.
//
// Of what commits add, the log can't tell what's live, so a database that
// only grows is compacted each time it doubles too, which rewrites its data
// about once more in all. After a compaction that failed before the switch,
// the next isn't due until the file has grown by as much again as it had
// since its last compaction, so a disk without room for a second copy isn't
// written to the full after every commit.
//
// Due is for the holder of the write lock, after a commit, and makes no
// call. It's false for a stuck Log.
func (l *Log) Due() bool {
	if !l.locked || l.stuck != nil || l.empty {
		return false
	}
	base := l.liveBase()
	return l.end >= 2*base && l.end-base >= minDead && l.end >= l.retry
}

// liveBase is the live data as the log last knew it: the end of the file's
// compacted part, or the header in generation 1.
func (l *Log) liveBase() int64 {
	return max(int64(l.hdr.CompactedEnd), format.HeaderSize)
}

// Compact compacts the database, holding the write lock. It writes snapshot,
// the in-memory copy as the changes of a compacted part in FORMAT.md's order,
// into NAME.compact, beside the database, syncs it, renames it over the
// database and syncs the folder, as the comment at the top of compact.go
// says. Then the Log holds the new file, still locked, with the compacted
// part as its log: the next Append goes in after it, numbered on from its
// last batch. The Target is told nothing, since it holds that state already.
//
// The snapshot has to hold exactly what the log has read, so it comes from a
// read of the copy taken after the last commit, with the write lock held
// throughout. Changes that break FORMAT.md's rules for a change on its own
// fail the compaction with an error that wraps errs.ErrInvalid, before the
// switch.
//
// Writers wait while it runs, in this process and in others, past their
// usual deadline: until Options.Wait after it ends.
//
// When anything fails before the switch, NAME.compact is removed, and the Log
// carries on with the old file, which is whole, and the error says so. When
// the folder's sync fails after the rename, nobody can tell which file a
// power cut would leave at the path, so the Log keeps the new file's lock
// until Close, and the error wraps errs.ErrStuck, as every Lock and Append
// after it does. Either way, a commit made under the same lock before the
// compaction has succeeded.
//
// Just before the rename, by the file rules, a database file that has been
// given a second name by a hard link, or a path whose last element has
// become a symbolic link, fails the compaction before the switch, with an
// error that wraps errs.ErrInvalid: the rename would leave the other name, or
// the file the link leads to, holding the old data (rules.go).
func (l *Log) Compact(snapshot iter.Seq[format.Change]) error {
	switch {
	case !l.locked:
		return fmt.Errorf("hypercrux: %s: Compact without the write lock", l.path)
	case l.stuck != nil:
		return l.stuck
	}
	// Writers in this process wait while it runs, and Options.Wait after.
	l.busy.Store(math.MaxInt64)
	defer func() { l.busy.Store(time.Now().Add(l.wait).UnixNano()) }()

	c, err := l.startCompact()
	if err != nil {
		l.retry = l.end + (l.end - l.liveBase())
		return fmt.Errorf("hypercrux: %s: compacting failed making %s, and the database carries on in its old file: %w", l.path, l.compactName(), err)
	}
	if err := l.writeCompact(c, snapshot); err != nil {
		return l.dropCompact(c, err, "writing %s")
	}
	if plant == "logfile/compact-lets-go-early" {
		l.f.Unlock()
	}
	if plant != "logfile/compact-rename-before-sync" {
		if err := c.f.Sync(); err != nil {
			return l.dropCompact(c, err, "syncing %s")
		}
	}
	// The file at the path is still this one, with one name, and the path's
	// last element is still its own name, so the rename replaces exactly
	// the database (rules.go).
	there, err := l.fsys.Stat(l.path)
	switch {
	case err != nil:
	case !there.Same(l.file):
		err = fmt.Errorf("another file has taken the database's path, which the compacted file would replace")
	case there.Nlink > 1 && plant != "logfile/second-name-unseen":
		err = l.names(there.Nlink)
	default:
		err = l.stillReal()
	}
	if err != nil {
		return l.dropCompact(c, err, "checking the database's path before renaming %s over it")
	}
	if err := l.fsys.Rename(c.name, l.path); err != nil {
		// A rename that failed changes nothing. If the path names the new
		// file all the same, the switch was made, and the folder's sync has
		// to make it last.
		if there, e := l.fsys.Stat(l.path); e != nil || !there.Same(c.info) {
			return l.dropCompact(c, err, "renaming %s over the database")
		}
	}
	var synced error
	if plant != "logfile/compact-no-folder-sync" {
		synced = l.fsys.SyncDir(l.dir)
	}
	if plant == "logfile/compact-rename-before-sync" {
		c.f.Sync()
	}
	l.switchTo(c)
	if synced != nil && plant != "logfile/compact-folder-sync-ignored" {
		l.stuck = fmt.Errorf("%w: %s: the folder's sync failed once the compacted file had taken the database's path: %w", errs.ErrStuck, l.path, synced)
		return l.stuck
	}
	return nil
}

// compaction is a compaction's new file while it's written: what the Log
// takes over at the switch.
type compaction struct {
	f    fsys.File
	info fsys.Info // what fstat said about it once it was locked, whose device and inode say which file it is
	name string
	hdr  format.Header
	head [format.HeaderSize]byte
	seq  uint64                  // the last batch written, or 0 when there's none
	end  int64                   // just past the last batch's marker, or the header
	last [format.MarkerSize]byte // the last batch's marker, when seq isn't 0
}

// startCompact makes NAME.compact beside the database, with the database's
// permissions and, where it's allowed, its owner, and locks it before
// anything is written to it. A leftover at the name goes first, as Lock
// removes one (removeCompact).
//
// Nothing but the holder of the write lock makes or removes NAME.compact, so
// the file this makes is its own. A waiter looking for a compaction under way
// takes its lock for a moment when it can (compacting), so the lock is tried
// again a few times. Once it's held, the name is checked to lead to the file
// still, as newFile checks a .new- file's, since a writer holding the lock of
// a file that has taken the path since, a backup moved into place, can
// remove one it can lock.
func (l *Log) startCompact() (*compaction, error) {
	old, err := l.f.Stat()
	if err != nil {
		return nil, err
	}
	name := l.compactName()
	f, err := l.fsys.Create(name, old.Mode.Perm())
	if errors.Is(err, fs.ErrExist) {
		l.removeCompact()
		f, err = l.fsys.Create(name, old.Mode.Perm())
	}
	if err != nil {
		return nil, err
	}
	for try := 1; ; try++ {
		ok, err := f.TryLock()
		if err != nil {
			l.fsys.Remove(name)
			f.Close()
			return nil, err
		}
		if ok {
			break
		}
		if try == maxTries {
			f.Close() // whoever holds its lock leaves a leftover, for the next holder of the write lock
			return nil, fmt.Errorf("%s stayed locked by another open file", name)
		}
		time.Sleep(time.Millisecond)
	}
	mine, err := f.Stat()
	if err != nil {
		l.fsys.Remove(name)
		f.Close()
		return nil, err
	}
	if there, err := l.fsys.Stat(name); err != nil || !there.Same(mine) {
		f.Close() // removed between its creation and its lock, so the name isn't this file's
		return nil, fmt.Errorf("%s was removed as soon as it was made", name)
	}
	if mine.Uid != old.Uid || mine.Gid != old.Gid {
		f.Chown(old.Uid, old.Gid) // a refusal leaves the file this process's
	}
	c := &compaction{f: f, info: mine, name: name, end: format.HeaderSize}
	c.hdr = format.Header{ID: l.hdr.ID, Gen: l.hdr.Gen + 1, FromGen: l.hdr.Gen}
	if l.seq > 0 {
		from, _ := format.DecodeMarker(l.last[:], l.hdr.ID, l.hdr.Gen)
		c.hdr.FromSeq, c.hdr.FromSum = from.Seq, from.Sum
	}
	return c, nil
}

// writeCompact writes snapshot into c's file as its compacted part, a batch
// at a time from offset 68, each with its marker, and then the header.
//
// The snapshot uses a put's Fields slice again for the next put, so the
// fields of the puts in a batch are copied into one slice that serves the
// whole batch. A batch ends at the change that would take its changes past
// partSize, or l.part when a test has set it, and before the first link.
//
// Should ranging over the snapshot panic, NAME.compact goes before the panic
// carries on, as it does when the compaction fails, so a caller that
// recovers isn't left holding its lock, which every later compaction would
// find in its way.
func (l *Log) writeCompact(c *compaction, snapshot iter.Seq[format.Change]) error {
	defer func() {
		if r := recover(); r != nil {
			l.dropCompact(c, fmt.Errorf("%v", r), "writing %s")
			panic(r)
		}
	}()
	limit := partSize
	if l.part > 0 {
		limit = l.part
	}
	var (
		changes []format.Change
		fields  []format.Field // the fields of the batch's puts, one after another
		size    int            // the batch's changes, in bytes
		links   bool           // the links have begun
		buf     []byte
		err     error
	)
	flush := func() bool {
		if len(changes) == 0 {
			return true
		}
		seq := c.seq + 1
		var sum uint32
		buf, sum, err = format.AppendBatch(buf[:0], c.hdr.Gen, seq, changes)
		if err != nil {
			return false
		}
		n := len(buf)
		buf = format.AppendMarker(buf, c.hdr.ID, c.hdr.Gen, format.Marker{Seq: seq, Sum: sum})
		if _, err = c.f.WriteAt(buf, c.end); err != nil {
			return false
		}
		c.seq, c.end = seq, c.end+int64(len(buf))
		copy(c.last[:], buf[n:])
		clear(changes)
		clear(fields)
		changes, fields, size = changes[:0], fields[:0], 0
		return true
	}
	for ch := range snapshot {
		k := changeSize(&ch)
		if plant != "logfile/compact-one-batch" {
			if ch.Op == format.Link && !links {
				links = true
				if !flush() {
					break
				}
			}
			if size > 0 && size+k > limit && !flush() {
				break
			}
		}
		if len(ch.Fields) > 0 {
			start := len(fields)
			fields = append(fields, ch.Fields...)
			ch.Fields = fields[start:len(fields):len(fields)]
		}
		changes = append(changes, ch)
		size += k
	}
	if err == nil {
		flush()
	}
	if err != nil {
		return err
	}
	c.hdr.CompactedEnd = uint64(c.end)
	head, err := format.AppendHeader(c.head[:0], c.hdr)
	if err != nil {
		return err
	}
	_, err = c.f.WriteAt(head, 0)
	return err
}

// changeSize is how many bytes c takes in a batch, as format.AppendBatch
// writes it. The compaction splits its batches by it.
func changeSize(c *format.Change) int {
	n := 1 // the kind
	switch c.Op {
	case format.CreateTable:
		n += 2 + len(c.Table) + 4 + 2
		for _, name := range c.Names {
			n += 2 + len(name)
		}
	case format.Put:
		n += 2 + len(c.Key) + 2
		for _, f := range c.Fields {
			n += 2 + len(f.Name) + valueSize(f.Value)
		}
	case format.Delete:
		n += 2 + len(c.Key)
	case format.Link, format.Unlink:
		n += 2 + len(c.Key) + 2 + len(c.Type) + 2 + len(c.To)
	case format.Drop:
		n += 2 + len(c.Table)
	}
	return n
}

// valueSize is how many bytes v takes in a put: its kind, then what the kind
// holds.
func valueSize(v value.Value) int {
	switch v.Kind() {
	case value.KindInt, value.KindReal:
		return 1 + 8
	case value.KindText, value.KindBytes, value.KindVector:
		return 1 + 4 + len(v.Raw())
	}
	return 1
}

// dropCompact ends a compaction that failed before the switch with err, in
// what it was doing to c's file, a format for its name: NAME.compact goes,
// and the Log carries on with the old file, which is whole. It's removed
// while it's still locked, so nobody else can take the name meanwhile. When
// the removal fails too, the file is left for the next holder of the write
// lock, who removes it once it's unlocked. The next compaction isn't due
// until the file has grown by as much again (Due).
func (l *Log) dropCompact(c *compaction, err error, what string) error {
	l.retry = l.end + (l.end - l.liveBase())
	var gone error
	if plant != "logfile/compact-left-behind" {
		gone = l.fsys.Remove(c.name)
	}
	c.f.Close()
	doing := fmt.Sprintf(what, c.name)
	if gone != nil && !errors.Is(gone, fs.ErrNotExist) {
		return fmt.Errorf("hypercrux: %s: compacting failed %s, and removing it failed too, which leaves it for the next writer; the database carries on in its old file: %w; %w", l.path, doing, err, gone)
	}
	return fmt.Errorf("hypercrux: %s: compacting failed %s, so it was removed, and the database carries on in its old file: %w", l.path, doing, err)
}

// switchTo moves the Log over to the compacted file, which has taken the
// database's path, and closes the old file, which lets go of its lock. The
// new file's lock stays held, as the write lock, until Unlock or, for a stuck
// Log, Close. The Target holds the compacted state already.
func (l *Log) switchTo(c *compaction) {
	old := l.f
	l.f, l.file, l.empty = c.f, c.info, false
	l.hdr, l.head = c.hdr, c.head
	l.seq, l.end, l.last = c.seq, c.end, c.last
	l.retry = 0
	old.Close()
}

// removeCompact removes a NAME.compact that a compaction left beside the
// database when its process died. It's for the holder of the write lock,
// once the inode check has passed: Lock, and the checks Open and Follow make
// holding it (checkLocked). It removes one it can lock that's a regular file,
// while the name still leads to the file it locked, as removeLeftovers does
// for .new- files. One that's locked stays: a compaction under way holds it,
// in a process that locked a file the path named before a backup was moved
// into place, or a waiter looking for one holds it for a moment. Errors are
// ignored, since a leftover costs room on the disk and nothing else, and the
// next holder of the lock tries again. With nothing there, it costs one call,
// a stat, and nothing but a regular file is ever opened (openLeftover).
func (l *Log) removeCompact() {
	name := l.compactName()
	f := l.openLeftover(name) // opened only when a stat finds a regular file there (rules.go)
	if f == nil {
		return
	}
	defer f.Close()
	if ok, err := f.TryLock(); err != nil || (!ok && plant != "logfile/compact-locked-removed") {
		return
	}
	mine, err := f.Stat()
	if err != nil || !mine.Mode.IsRegular() {
		return
	}
	if there, err := l.fsys.Stat(name); err == nil && there.Same(mine) {
		l.fsys.Remove(name)
	}
}

// compacting reports whether a compaction is under way on the database, in
// any process: whether NAME.compact is there and locked. A writer waiting for
// flock looks between its tries. When it can take the lock, the file is a
// leftover or a compaction's file in the moment before its lock, and the lock
// goes again at once, with the file (startCompact tries again for that).
// With nothing there, it costs one call, a stat, and nothing but a regular
// file is ever opened (openLeftover, in rules.go).
func (l *Log) compacting() bool {
	f := l.openLeftover(l.compactName())
	if f == nil {
		return false
	}
	ok, err := f.TryLock()
	f.Close()
	return err == nil && !ok
}

// hold notes that a compaction was under way just now, so waits for the
// write lock go on until t, unless they go on later already (busy).
func (l *Log) hold(t time.Time) {
	n := t.UnixNano()
	for {
		b := l.busy.Load()
		if b >= n || l.busy.CompareAndSwap(b, n) {
			return
		}
	}
}

// until is when a wait for the write lock that would end at deadline ends:
// at deadline, or when busy says, whichever is later. busy is Options.Wait
// after this Log last saw a compaction under way, or for as long as its own
// runs.
func (l *Log) until(deadline time.Time) time.Time {
	if b := l.busy.Load(); b > deadline.UnixNano() && plant != "logfile/compact-wait-ignored" {
		return time.Unix(0, b)
	}
	return deadline
}
