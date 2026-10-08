// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"syscall"
	"time"
)

// The toy log is the harness's test subject, small enough to read at a
// sitting: a file of slots of one size, each holding one commit. Slot k,
// from 0, holds commit k+1: its batch, the 48 bytes the commit holds, then
// its marker, "M", the commit's sequence number in 14 digits, and a
// newline. A commit is in the log once its marker is there.
//
//   - A writer takes flock on the file, waiting for it, and finds the end
//     of the log: the first slot without its marker. It cuts the file
//     there, in case a killed writer left part of a commit, then writes the
//     batch, pauses, writes the marker, and lets go of flock.
//   - A reader keeps the file open and reads on from where it stopped. For
//     the next slot, it reads the marker, and the batch only once the
//     marker is there. It never takes flock.
//   - Final takes flock, as a writer would, and reads the commits up to the
//     end of the log.
//
// The pause stands in for a slow disk. It makes the time between a
// commit's two writes long enough for a reader to come in between, and for
// a kill to land there. On Linux, Go's sleeps of under a millisecond take
// about a millisecond, so the pause and a reader's wait for the next
// commit come to about that. Nothing is synced: the harness kills
// processes and never cuts the power, and the page cache keeps what a
// killed process wrote.
const (
	toyBatch  = 48
	toyMarker = 16
	toySlot   = toyBatch + toyMarker
	toyPause  = 500 * time.Microsecond // between a commit's two writes
	toyPoll   = 200 * time.Microsecond // a reader's wait when the next commit isn't there yet
)

// The ways the toy log can go wrong, for the tests that show the harness
// catching them. The first is also the package's planted bug, which the
// toy log in TestTheToyLogPasses has when HYPERCRUX_PLANT names it in a
// build with the hypercrux_planted tag.
const (
	markerBeforeBatch = "procs/marker-before-batch" // the marker written before the batch
	noLock            = "no lock"                   // writers that never take flock
)

// The toy log's workloads: as it should be, unless the plant is switched
// on, and with each of its bugs.
var (
	toy               = toyWorkload("toy log", func() string { return plant })
	toyMarkerFirst    = toyWorkload("toy log with the marker first", func() string { return markerBeforeBatch })
	toyWithoutTheLock = toyWorkload("toy log without the lock", func() string { return noLock })
)

func toyWorkload(name string, bug func() string) Workload {
	return Workload{
		Name:  name,
		Write: func(w *Writer) error { return toyWrite(w, bug()) },
		Read:  toyRead,
		Final: toyFinal,
	}
}

// toyMark is the marker of commit seq.
func toyMark(seq int) []byte { return fmt.Appendf(nil, "M%014d\n", seq) }

// toyCommit is what writer id's commit n holds: its name, then letters to
// fill the batch.
func toyCommit(id, n int) string {
	b := fmt.Appendf(nil, "w%d.%d ", id, n)
	for i := len(b); i < toyBatch; i++ {
		b = append(b, 'a'+byte((id*7+n*13+i)%26))
	}
	return string(b[:toyBatch])
}

// toyWrite is a toy writer's work: commits, one after another, until the
// process is killed. After about one commit in four, it waits as long as
// its pause before the next, so another writer can take the lock. A
// writer that never waited would keep the lock to itself, and one that
// always did would spend half its life between commits.
func toyWrite(w *Writer, bug string) error { return toyCommits(w, bug, -1) }

// toyWriteSome is a toy writer's work that returns once it has made
// commits commits.
func toyWriteSome(w *Writer, commits int) error { return toyCommits(w, "", commits) }

// toyCommits makes commits commits, or keeps on committing when commits is
// -1, with the bug bug.
func toyCommits(w *Writer, bug string, commits int) error {
	f, err := os.OpenFile(w.Path(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := int(f.Fd())
	end := 0 // the slots this writer knows to hold their markers
	for n := 1; n != commits+1; n++ {
		c := toyCommit(w.ID(), n)
		w.Begin(c)
		if bug != noLock {
			if err := flock(fd, syscall.LOCK_EX); err != nil {
				return err
			}
		}
		var size int64
		if end, size, err = toyEnd(f, end); err != nil {
			return err
		}
		at := int64(end) * toySlot
		if size > at {
			if err := f.Truncate(at); err != nil {
				return err
			}
		}
		writes := []func() error{
			func() error { _, err := f.WriteAt([]byte(c), at); return err },
			func() error { _, err := f.WriteAt(toyMark(end+1), at+toyBatch); return err },
		}
		if bug == markerBeforeBatch {
			slices.Reverse(writes)
		}
		if err := writes[0](); err != nil {
			return err
		}
		time.Sleep(toyPause)
		if err := writes[1](); err != nil {
			return err
		}
		end++
		w.Done()
		if bug != noLock {
			if err := flock(fd, syscall.LOCK_UN); err != nil {
				return err
			}
		}
		if w.Rand().IntN(4) == 0 {
			time.Sleep(toyPause)
		}
	}
	return nil
}

// toyEnd returns where the toy log in f ends, the first slot without its
// marker, looking from the slot from on, since the ones before it are known
// to have theirs. It returns the file's size too. When the file is shorter
// than from slots, which only writers without the lock can bring about, it
// looks from the first slot.
func toyEnd(f *os.File, from int) (int, int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := info.Size()
	if size < int64(from)*toySlot {
		from = 0
	}
	rest := make([]byte, size-int64(from)*toySlot)
	if _, err := f.ReadAt(rest, int64(from)*toySlot); err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, err
	}
	end := from
	for len(rest) >= toySlot && bytes.Equal(rest[toyBatch:toySlot], toyMark(end+1)) {
		rest = rest[toySlot:]
		end++
	}
	return end, size, nil
}

// toyRead is a toy reader's work: it reads on, a slot at a time, until the
// process is killed.
func toyRead(r *Reader) error { return toyReadInto(r.Path(), r) }

// applier is what a reader hands its commits to: its Reader, or a test's
// stand-in for one.
type applier interface {
	Apply(seq uint64, commit string)
}

// toyReadInto reads on in the toy log at path, a slot at a time, and
// hands each commit to r.
func toyReadInto(path string, r applier) error {
	f, err := os.Open(path)
	for errors.Is(err, fs.ErrNotExist) { // no writer has made the file yet
		time.Sleep(toyPoll)
		f, err = os.Open(path)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	marker, batch := make([]byte, toyMarker), make([]byte, toyBatch)
	for k := 0; ; {
		at := int64(k) * toySlot
		_, err := f.ReadAt(marker, at+toyBatch)
		if err == nil && bytes.Equal(marker, toyMark(k+1)) {
			_, err = f.ReadAt(batch, at)
			if err == nil {
				r.Apply(uint64(k+1), string(batch))
				k++
				continue
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		time.Sleep(toyPoll)
	}
}

// toyFinal reads the toy log at path holding flock, as the next writer
// would.
func toyFinal(path string) ([]string, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	end, _, err := toyEnd(f, 0)
	if err != nil {
		return nil, err
	}
	data := make([]byte, int64(end)*toySlot)
	if _, err := f.ReadAt(data, 0); err != nil {
		return nil, err
	}
	commits := make([]string, end)
	for k := range commits {
		commits[k] = string(data[k*toySlot : k*toySlot+toyBatch])
	}
	return commits, nil
}

// flock calls flock(2), again when a signal interrupts it.
func flock(fd, how int) error {
	for {
		if err := syscall.Flock(fd, how); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
