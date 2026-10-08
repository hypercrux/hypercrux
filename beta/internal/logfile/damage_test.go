// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// writeAt writes b at offset off in the file at path, in place.
func writeAt(path string, off int64, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(b, off); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// tryHook is a file whose nth TryLock, counted over every file opened
// through one FS, calls do[n] first, when there's one. Open's first try at
// the lock is the one without waiting, after its read of the log, and its
// second comes while it waits for the lock to read the file again.
type tryHook struct {
	fsys.File
	tries *int
	do    map[int]func()
}

func (f *tryHook) TryLock() (bool, error) {
	*f.tries++
	if do := f.do[*f.tries]; do != nil {
		do()
	}
	return f.File.TryLock()
}

// hooked is the real calls, with every file a tryHook doing do.
func hooked(do map[int]func()) fsys.FS {
	tries := 0
	return &hookFS{FS: fsys.OS{}, file: func(f fsys.File) fsys.File { return &tryHook{File: f, tries: &tries, do: do} }}
}

// TestAReaderReadsAgainUnderTheLock: what lies past the end of the log can
// change while a writer is at work, so Open, when another holds the write
// lock, reports what looks like damage there only once a read holding the
// lock agrees. The file is mostly one a reader can see while a writer is at
// work: batch 2 read half written, and the batch's marker, which the writer
// wrote a moment later. The writer's moves come at Open's tries at the lock.
//
//   - The writer finishes the batch and lets go while Open waits: Open
//     reads again holding the lock, finds batch 2 whole and marked, and
//     applies it.
//   - The writer cuts a torn end, or commits a batch, between Open's read
//     and its look past the end, holding the lock throughout: Open finds no
//     damage in what it read, and returns without waiting for the lock.
//   - A torn end turns into a batch that counts, with a change that breaks
//     the rules, between Open's read and its look: Open takes it for what
//     may be damage, and once the lock comes free, reads it again holding
//     the lock and reports it.
//   - The wait runs out: Open fails with an error that wraps
//     errs.ErrLockTimeout and says what it found. Nothing confirmed the
//     damage, so the error doesn't wrap errs.ErrDamaged.
//   - Another file takes the path, and the writer lets go: Open reads the
//     file at the path, and the file it read first stays as it was.
//   - A writer's commit fails, and the writer cuts it back and commits two
//     shorter batches where it was, while the reader opens the file (F5).
//     The reader's read of the log finds the failed batch whole, without its
//     marker, and stops there. By its try at the lock, the first new batch's
//     marker lies inside what the reader took for the file, which looks like
//     a commit made past the end of the log. The reader waits for the lock,
//     reads the file again holding it once the writer lets go, and applies
//     both new batches. Nothing is reported.
func TestAReaderReadsAgainUnderTheLock(t *testing.T) {
	whole := newBuilder(t)
	whole.commit(table("one"))
	end := len(whole.b) // where the log ends, as Open reads it
	at := int64(end + format.BatchHeadSize + 2)
	whole.commit(table("two"))
	torn := slices.Clone(whole.b)
	torn[at] ^= 0x20

	t.Run("the writer finishes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		write(t, path, torn)
		release := holdLock(t, path)
		defer release()
		rec := &recorder{}
		l, err := Open(hooked(map[int]func(){2: func() {
			if err := writeAt(path, at, whole.b[at:at+1]); err != nil {
				t.Error(err)
			}
			release()
		}}), path, rec, Options{})
		if err != nil {
			t.Fatalf("Open gave %v, where it reads batch 2 whole once the writer has let go", err)
		}
		defer l.Close()
		rec.holds(t, table("one"), table("two"))
		if !bytes.Equal(fileBytes(t, path), whole.b) {
			t.Error("the file isn't as the writer left it")
		}
	})

	cut := slices.Clone(whole.b[:len(whole.b)-format.MarkerSize-5]) // batch 2 torn, without its marker
	bad := newBuilder(t)
	bad.commit(table("one"))
	bad.raw(1, 2, []byte("X\x03\x00Bad")) // a table name with a capital letter
	for _, c := range []struct {
		name string
		then func(path string) error // what the writer does at Open's first try
		want error                   // what Open gives once the lock comes free, or nil when it doesn't wait
	}{
		{"the writer cuts a torn end", func(path string) error { return os.Truncate(path, int64(end)) }, nil},
		{"the writer commits a batch", func(path string) error { return writeAt(path, int64(end), whole.b[end:]) }, nil},
		{"a torn end turns into bad changes", func(path string) error { return writeAt(path, int64(end), bad.b[end:]) }, errs.ErrDamaged},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			write(t, path, cut[:end+40])
			release := holdLock(t, path)
			defer release()
			do := map[int]func(){1: func() {
				if err := c.then(path); err != nil {
					t.Error(err)
				}
			}}
			if c.want != nil {
				do[2] = release
			}
			rec := &recorder{}
			l, err := Open(hooked(do), path, rec, Options{Wait: 300 * time.Millisecond})
			if c.want != nil {
				mustBeDamage(t, err, path, 2)
				return
			}
			if err != nil {
				t.Fatalf("Open gave %v, where it returns without waiting for the lock", err)
			}
			defer l.Close()
			rec.holds(t, table("one"))
		})
	}

	t.Run("the wait runs out", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		if err := os.WriteFile(path, torn, 0o644); err != nil {
			t.Fatal(err)
		}
		defer holdLock(t, path)()
		const wait = 300 * time.Millisecond
		began := time.Now()
		_, err := Open(fsys.OS{}, path, &recorder{}, Options{Wait: wait})
		took := time.Since(began)
		if !errors.Is(err, errs.ErrLockTimeout) || errors.Is(err, errs.ErrDamaged) || !strings.Contains(err.Error(), "batch 2") {
			t.Fatalf("Open gave %v, where a timeout that names batch 2 and isn't damage is wanted", err)
		}
		if took < wait {
			t.Errorf("Open gave up after %v, where the wait is %v", took, wait)
		}
		if !bytes.Equal(fileBytes(t, path), torn) {
			t.Error("the file changed")
		}
	})

	t.Run("another file takes the path", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		if err := os.WriteFile(path, torn, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path, filepath.Join(dir, "old")); err != nil { // keeps the first file to look at
			t.Fatal(err)
		}
		another(t, filepath.Join(dir, "new"), "n1", "n2")
		release := holdLock(t, path)
		defer release()
		rec := &recorder{}
		l, err := Open(hooked(map[int]func(){2: func() {
			if err := os.Rename(filepath.Join(dir, "new"), path); err != nil {
				t.Error(err)
			}
			release()
		}}), path, rec, Options{})
		if err != nil {
			t.Fatalf("Open gave %v, where it reads the file that took the path", err)
		}
		defer l.Close()
		rec.holds(t, table("n1"), table("n2"))
		if rec.resets != 1 {
			t.Errorf("the Target was reset %d times", rec.resets)
		}
		if !bytes.Equal(fileBytes(t, filepath.Join(dir, "old")), torn) {
			t.Error("the file Open read first changed")
		}
	})

	t.Run("a failed commit cut back and written again", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		s := &spy{}
		w, err := Open(spyOn(s), path, &recorder{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		closeAtEnd(t, w)
		commit(t, w, table("one"))

		// The writer, on a goroutine of its own, holding the lock throughout.
		// Its commit's sync fails once the reader has read the log.
		written, read, committed, waiting := newSignal(), newSignal(), newSignal(), newSignal()
		errc := make(chan error, 1)
		go func() {
			err := func() error {
				if err := w.Lock(); err != nil {
					return err
				}
				defer w.Unlock()
				s.fail = func(call string) error {
					if call != "sync" || written.fired() {
						return nil
					}
					written.fire()
					read.wait()
					return errDrive
				}
				if err := w.Append(table(strings.Repeat("x", 40))); !errors.Is(err, errDrive) || errors.Is(err, errs.ErrStuck) {
					return fmt.Errorf("the commit whose sync fails gave %v", err)
				}
				for _, name := range []string{"two", "three"} {
					if err := w.Append(table(name)); err != nil {
						return err
					}
				}
				committed.fire()
				waiting.wait()
				return nil
			}()
			written.fire()
			committed.fire()
			errc <- err
		}()

		written.wait()
		rec := &recorder{}
		waited := false
		l, err := Open(hooked(map[int]func(){
			1: func() { read.fire(); committed.wait() },
			2: func() { waited = true; waiting.fire() },
		}), path, rec, Options{})
		read.fire() // in case Open stopped short of its tries
		waiting.fire()
		if werr := <-errc; werr != nil {
			t.Fatal(werr)
		}
		if err != nil {
			t.Fatalf("Open gave %v, where it reads the batches committed where the failed one was", err)
		}
		defer l.Close()
		if !waited {
			t.Error("Open didn't wait for the lock")
		}
		rec.holds(t, table("one"), table("two"), table("three"))
	})
}

// signal is a moment that one goroutine waits for another to reach. fire
// can be called more than once, from any goroutine.
type signal struct {
	once sync.Once
	c    chan struct{}
}

func newSignal() *signal { return &signal{c: make(chan struct{})} }

func (s *signal) fire() { s.once.Do(func() { close(s.c) }) }

func (s *signal) wait() { <-s.c }

func (s *signal) fired() bool {
	select {
	case <-s.c:
		return true
	default:
		return false
	}
}

// memFile is a file held in memory, for reads alone.
type memFile struct {
	fsys.File
	b []byte
}

func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.b)) {
		return 0, io.EOF
	}
	n := copy(p, f.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// TestTheLookPastFindsAMarkerAnywhere: the look past the end of the log
// tries every offset, reading a window of the file at a time, so a whole
// marker naming the next batch is found wherever it lies: just past the
// end of the log, on either side of the edge between the first two windows
// and across it, two windows on, and ending the file. Between the end of the
// log and the marker lie zeros, or bytes full of magic numbers whose check
// values don't fit. The look passes over those, and over a marker naming
// an earlier batch, a marker of another database and one of a later
// generation: with those alone, it finds nothing, and the check cuts the
// file back to the end of the log.
//
// The look reads a file held in memory here, so the many files it reads
// cost no disk. Then two files go through Open on the disk: one the check
// reports as damage, and one it cuts.
func TestTheLookPastFindsAMarkerAnywhere(t *testing.T) {
	w := newBuilder(t)
	w.commit(table("one"))
	end := len(w.b)
	edge := end + window // where the first window the look reads ends
	size := end + 2*window + 100
	marker := func(id [16]byte, gen, seq uint64) []byte {
		return format.AppendMarker(nil, id, gen, format.Marker{Seq: seq, Sum: 0x1234})
	}
	next := marker(w.hdr.ID, w.hdr.Gen, 2)

	// Magic numbers a few hundred bytes apart, each followed by random bytes,
	// so their check values don't fit. format's TestFindMarker packs them
	// tighter.
	zeros := append(slices.Clone(w.b), make([]byte, size-end)...)
	magics := slices.Clone(zeros)
	r := rand.New(rand.NewPCG(8, 9))
	for i := end; i < size; {
		i += copy(magics[i:], "HCRM")
		for k := r.IntN(400); k > 0 && i < size; k-- {
			magics[i] = byte(r.Uint32())
			i++
		}
	}

	look := func(file []byte) *errs.Damage {
		t.Helper()
		l := &Log{f: &memFile{b: file}, path: "mem", hdr: w.hdr, seq: 1, end: int64(end)}
		d, err := l.pastEnd(int64(len(file)))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	positions := []int{end, end + 1, edge - format.MarkerSize - 1, edge - format.MarkerSize}
	for k := format.MarkerSize - 1; k > 0; k-- {
		positions = append(positions, edge-k) // across the edge
	}
	positions = append(positions, edge, edge+1, end+2*window+3, size-format.MarkerSize)
	for _, fill := range []struct {
		name string
		b    []byte
	}{{"zeros", zeros}, {"magic numbers", magics}} {
		file := slices.Clone(fill.b)
		put := func(at int, m []byte) *errs.Damage { // the marker m at offset at, for one look
			copy(file[at:], m)
			defer copy(file[at:], fill.b[at:at+len(m)])
			return look(file)
		}
		for _, at := range positions {
			d := put(at, next)
			if d == nil || d.Batch != 2 || d.Offset != int64(end) || !strings.Contains(d.Reason, "offset "+strconv.Itoa(at)+" ") {
				t.Errorf("after %s, a marker at offset %d (the first window ends at %d) gives %v, where damage naming batch 2 is wanted", fill.name, at, edge, d)
			}
		}
		if d := look(fill.b); d != nil {
			t.Errorf("%s alone give %v", fill.name, d)
		}
		for _, c := range []struct {
			what string
			b    []byte
		}{
			{"a marker naming batch 1", marker(w.hdr.ID, w.hdr.Gen, 1)},
			{"another database's marker", marker([16]byte{'o', 't', 'h', 'e', 'r'}, w.hdr.Gen, 2)},
			{"a later generation's marker", marker(w.hdr.ID, w.hdr.Gen+1, 2)},
		} {
			if d := put(edge-9, c.b); d != nil {
				t.Errorf("after %s, %s gives %v", fill.name, c.what, d)
			}
		}
	}

	path := filepath.Join(t.TempDir(), "db")
	file := slices.Clone(zeros[:edge+50])
	copy(file[edge-9:], next)
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(fsys.OS{}, path, &recorder{}, Options{})
	mustBeDamage(t, err, path, 2)
	if !bytes.Equal(fileBytes(t, path), file) {
		t.Error("the file changed")
	}
	if err := os.WriteFile(path, magics[:edge+50], 0o644); err != nil {
		t.Fatal(err)
	}
	reread(t, path).holds(t, table("one"))
	if n := len(fileBytes(t, path)); n != end {
		t.Errorf("the check left %d bytes, where it cuts back to the end of the log at %d", n, end)
	}
}
