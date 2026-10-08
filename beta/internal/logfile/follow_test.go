// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The follower's cases one by one (F6): a Log that keeps the database open
// and follows what other Logs commit, as a process follows other processes.
// Another Log in this process, a helper process, or bytes written into the
// file by hand stand in for the writers, and holdLock for a writer at work.

// openWith opens the database at path through files, with a recorder, and
// closes it when the test ends.
func openWith(t *testing.T, files fsys.FS, path string, o Options) (*Log, *recorder) {
	t.Helper()
	rec := &recorder{}
	l, err := Open(files, path, rec, o)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, l)
	return l, rec
}

// follow calls l.Follow where a test wants it to work.
func follow(t *testing.T, l *Log) {
	t.Helper()
	if err := l.Follow(); err != nil {
		t.Fatalf("Follow: %v", err)
	}
}

// appendTo appends b to the file at path, as another process's writer
// would write it.
func appendTo(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// seqs checks the sequence numbers of the batches r was handed.
func seqs(t *testing.T, r *recorder, want ...uint64) {
	t.Helper()
	var got []uint64
	for _, b := range r.batches {
		got = append(got, b.seq)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the Target was handed batches %v, where %v are wanted", got, want)
	}
}

// watchFS is the real calls, counting the stats of a path and the tries at
// flock, and noting every read of every file it opens, as "n at off".
type watchFS struct {
	fsys.FS
	mu     sync.Mutex
	stats  int
	tries  int
	reads  []string
	onRead func(p []byte, off int64) // called after each read, with what it read, when set
}

func (w *watchFS) Stat(path string) (fsys.Info, error) {
	w.mu.Lock()
	w.stats++
	w.mu.Unlock()
	return w.FS.Stat(path)
}

func (w *watchFS) Open(path string) (fsys.File, error) {
	f, err := w.FS.Open(path)
	if err != nil {
		return nil, err
	}
	return &watchFile{File: f, w: w}, nil
}

func (w *watchFS) Create(path string, perm fs.FileMode) (fsys.File, error) {
	f, err := w.FS.Create(path, perm)
	if err != nil {
		return nil, err
	}
	return &watchFile{File: f, w: w}, nil
}

// clear forgets what w has counted and noted so far.
func (w *watchFS) clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats, w.tries, w.reads = 0, 0, nil
}

type watchFile struct {
	fsys.File
	w *watchFS
}

func (f *watchFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	f.w.mu.Lock()
	f.w.reads = append(f.w.reads, fmt.Sprintf("%d at %d", len(p), off))
	hook := f.w.onRead
	f.w.mu.Unlock()
	if hook != nil {
		hook(p[:n], off)
	}
	return n, err
}

func (f *watchFile) TryLock() (bool, error) {
	f.w.mu.Lock()
	f.w.tries++
	f.w.mu.Unlock()
	return f.File.TryLock()
}

// TestAFollowerReadsOn: a Log that keeps the database open follows what
// another writer commits. Each Follow hands the Target every batch committed
// since, in order, and nothing more, whether one batch has come, a few, or
// thousands with some longer than the reader's window, which it reads in a
// few reads. The follower's own commits aren't handed to it, and the
// batches after them come with the numbers after them. With nothing new,
// Follow makes one stat and nothing more.
func TestAFollowerReadsOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	w.commit(table("one"))
	w.write(path)
	watch := &watchFS{FS: fsys.OS{}}
	f, rec := openWith(t, watch, path, Options{})
	rec.holds(t, table("one"))

	at := len(w.b)
	w.commit(table("two"))
	appendTo(t, path, w.b[at:])
	follow(t, f)
	rec.holds(t, table("one"), table("two"))
	at = len(w.b)
	w.commit(table("three"))
	w.commit(table("four"))
	appendTo(t, path, w.b[at:])
	follow(t, f)
	rec.holds(t, table("one"), table("two"), table("three"), table("four"))

	// The follower's own commit, batch 5, and then another of the writer's.
	commit(t, f, table("mine"))
	w.b, w.seq = fileBytes(t, path), 5
	at = len(w.b)
	w.commit(table("six"))
	appendTo(t, path, w.b[at:])
	follow(t, f)
	seqs(t, rec, 1, 2, 3, 4, 6)

	// Thousands at once, a few of them longer than the window.
	r := rand.New(rand.NewPCG(6, 1))
	at = len(w.b)
	var lists [][]format.Change
	for i := range 2000 {
		c := randomChanges(r)
		if i%700 == 350 {
			c = []format.Change{{Op: format.Put, Key: "docs:long", Fields: []format.Field{{Name: "body", Value: value.Text(strings.Repeat("y", window+r.IntN(window)))}}}}
		}
		w.commit(c)
		lists = append(lists, c)
	}
	appendTo(t, path, w.b[at:])
	rec.batches = nil
	watch.clear()
	follow(t, f)
	rec.holdsFrom(t, 7, lists...)
	if n := len(watch.reads); n > 40 {
		t.Errorf("following %d batches, %d MB, took %d reads", len(lists), (len(w.b)-at)>>20, n)
	}
	t.Logf("%d batches, %d bytes, in %d reads", len(lists), len(w.b)-at, len(watch.reads))

	// Nothing new.
	watch.clear()
	follow(t, f)
	if watch.stats != 1 || len(watch.reads) != 0 || watch.tries != 0 {
		t.Errorf("Follow with nothing new made %d stats, %d reads and %d tries at the lock, where one stat alone is wanted", watch.stats, len(watch.reads), watch.tries)
	}
	if len(rec.batches) != len(lists) {
		t.Errorf("Follow with nothing new handed over %d batches more", len(rec.batches)-len(lists))
	}
}

// TestFollowingReadsTheHeadThenTheMarkerThenTheRest: a follower reads each
// batch's head, then the place for its marker, and the rest of the batch
// only once the marker is whole, so it never reads a commit still being
// written. A writer is at work throughout, holding the lock, on a batch of
// three times the reader's window. While the file ends with the batch, the
// follower reads its head alone. With a torn marker after it, the head and
// the marker's place. With the marker whole, the head, the marker, then the
// batch, and it applies it.
func TestFollowingReadsTheHeadThenTheMarkerThenTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	w.commit(table("one"))
	w.write(path)
	defer holdLock(t, path)()
	watch := &watchFS{FS: fsys.OS{}}
	f, rec := openWith(t, watch, path, Options{})
	end := int64(len(w.b))
	long := []format.Change{{Op: format.Put, Key: "docs:long", Fields: []format.Field{{Name: "body", Value: value.Text(strings.Repeat("z", 3*window))}}}}
	sum := w.batch(long)
	n := int64(len(w.b)) - end
	marker := format.AppendMarker(nil, w.hdr.ID, w.hdr.Gen, format.Marker{Seq: 2, Sum: sum})
	torn := slices.Clone(marker)
	torn[19] ^= 1
	head := fmt.Sprintf("%d at %d", format.BatchHeadSize, end)
	place := fmt.Sprintf("%d at %d", format.MarkerSize, end+n)
	for _, c := range []struct {
		name  string
		after []byte // what follows the batch
		reads []string
	}{
		{"the batch alone", nil, []string{head}},
		{"a torn marker", torn, []string{head, place}},
		{"the marker whole", marker, []string{head, place, fmt.Sprintf("%d at %d", n, end)}}, // longer than the window, so read alone
	} {
		file := append(slices.Clone(w.b), c.after...)
		if err := os.WriteFile(path, file, 0o644); err != nil {
			t.Fatal(err)
		}
		watch.clear()
		follow(t, f)
		if strings.Join(watch.reads, ", ") != strings.Join(c.reads, ", ") {
			t.Errorf("%s: the follower read\n  %s\nwhere\n  %s\nis wanted", c.name, strings.Join(watch.reads, ", "), strings.Join(c.reads, ", "))
		}
		if c.after == nil || bytes.Equal(c.after, torn) {
			rec.holds(t, table("one"))
		} else {
			rec.holds(t, table("one"), long)
		}
		if !bytes.Equal(fileBytes(t, path), file) {
			t.Errorf("%s: the file changed", c.name)
		}
	}
}

// TestAFollowerKeepsNothingUnmarked: while a writer is at work, holding the
// lock, a batch that counts without its marker is a commit under way, and
// a follower applies none of it. When the writer then writes another batch
// in its place, of the same length, with its marker, as after a failed
// commit cut back and the next commit, the follower applies the new one,
// read afresh.
func TestAFollowerKeepsNothingUnmarked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	w.commit(table("one"))
	w.write(path)
	defer holdLock(t, path)()
	f, rec := openLog(t, path, Options{})
	end := len(w.b)
	under := slices.Clone(w.b)
	w.batch(table("aaaa"))
	under = append(under, w.b[end:]...)
	if err := os.WriteFile(path, under, 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		follow(t, f)
		rec.holds(t, table("one"))
	}
	w.b, w.seq = w.b[:end], 1
	w.commit(table("bbbb"))
	if len(w.b) != len(under)+format.MarkerSize {
		t.Fatal("the two batches differ in length")
	}
	w.write(path)
	follow(t, f)
	rec.holds(t, table("one"), table("bbbb"))
}

// TestAFollowerAndAFailedCommit: a writer's commit fails at its sync while a
// follower reads, and the writer cuts it back and commits two batches where
// it was before it lets go (F5). The follower reads the failed batch whole
// and stops there, since its marker never came. By its try at the lock, the
// new batches are in the file, and the lock is held. The follower takes
// nothing of the failed batch, and once the writer lets go, it applies the
// two new ones. When the first new batch is as long as the failed one was,
// its marker lies where the failed one's would have, naming the same
// sequence number with another checksum.
func TestAFollowerAndAFailedCommit(t *testing.T) {
	for _, c := range []struct {
		name    string
		failing []format.Change
		then    [][]format.Change
	}{
		{"shorter batches", table(strings.Repeat("x", 40)), [][]format.Change{table("two"), table("three")}},
		{"a batch of the same length", table("aaaa"), [][]format.Change{table("bbbb"), table("three")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			s := &spy{}
			w, err := Open(spyOn(s), path, &recorder{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			closeAtEnd(t, w)
			commit(t, w, table("one"))

			// The writer, on a goroutine of its own, holding the lock
			// throughout. Its commit's sync fails once the follower has read.
			written, read, committed, letGo := newSignal(), newSignal(), newSignal(), newSignal()
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
					if err := w.Append(c.failing); !errors.Is(err, errDrive) || errors.Is(err, errs.ErrStuck) {
						return fmt.Errorf("the commit whose sync fails gave %v", err)
					}
					for _, list := range c.then {
						if err := w.Append(list); err != nil {
							return err
						}
					}
					committed.fire()
					letGo.wait()
					return nil
				}()
				written.fire()
				committed.fire()
				errc <- err
			}()

			// The follower's Open makes the first try at the lock, and its
			// Follow, once it has stopped at the failed batch, the second.
			f, rec := openWith(t, hooked(map[int]func(){2: func() { read.fire(); committed.wait() }}), path, Options{Wait: 300 * time.Millisecond})
			rec.holds(t, table("one"))
			written.wait()
			err = f.Follow()
			read.fire()
			letGo.fire()
			if werr := <-errc; werr != nil {
				t.Fatal(werr)
			}
			if err != nil {
				t.Fatalf("Follow while the writer cut back its failed commit gave %v", err)
			}
			rec.holds(t, table("one"))
			follow(t, f)
			rec.holds(t, append([][]format.Change{table("one")}, c.then...)...)
		})
	}
}

// TestAFollowerReadsAgainWhatItCaughtHalfWritten: a read can run beside a
// writer's write, and catch a batch before the writer has finished it and
// the marker the writer wrote a moment later. Here the read through the
// window that brings in batch 3 and its marker has one byte of batch 3 as
// it was before the writer finished it. The follower reads batch 3 again,
// from its head, finds it whole and marked, and applies it, without waiting
// for the lock, which a writer holds throughout.
func TestAFollowerReadsAgainWhatItCaughtHalfWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	w.commit(table("one"))
	w.write(path)
	defer holdLock(t, path)()
	watch := &watchFS{FS: fsys.OS{}}
	f, rec := openWith(t, watch, path, Options{Wait: 300 * time.Millisecond})
	end := len(w.b)
	w.commit(table("two"))
	at := int64(len(w.b) + format.BatchHeadSize + 3) // in batch 3's changes
	w.commit(table("three"))
	appendTo(t, path, w.b[end:])
	var mu sync.Mutex
	torn := false
	watch.onRead = func(p []byte, off int64) {
		mu.Lock()
		defer mu.Unlock()
		if !torn && off <= at && at < off+int64(len(p)) && len(p) > format.MarkerSize {
			torn = true
			p[at-off] ^= 0x20
		}
	}
	began := time.Now()
	follow(t, f)
	if took := time.Since(began); took > 200*time.Millisecond {
		t.Errorf("Follow took %v, so it waited for the lock", took)
	}
	if !torn {
		t.Fatal("no read brought in batch 3")
	}
	rec.holds(t, table("one"), table("two"), table("three"))
}

// TestAFollowerFindsTheWriterGone: a helper process takes the write lock,
// writes a batch and syncs it, and dies before it writes the marker, or dies
// with half the batch written. With no writer at work, a follower that
// stops at what it left gets the lock when it tries, and checks the end of
// the log itself, as the next writer would: the whole batch is written again
// with the marker before it, synced, marked and applied, and half a batch is
// cut off, after the last marker is written again and synced.
func TestAFollowerFindsTheWriterGone(t *testing.T) {
	for _, tail := range []string{"whole", "half"} {
		t.Run(tail, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			s := &spy{}
			f, err := Open(spyOn(s), path, &recorder{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			closeAtEnd(t, f)
			rec := f.t.(*recorder)
			commit(t, f, table("one"))
			end := f.end
			h := start(t, "die", path, "HYPERCRUX_LOGFILE_TAIL="+tail)
			if line := h.line(t); line != "written" {
				t.Fatalf("the helper said %q", line)
			}
			h.end(t)
			left := int64(len(fileBytes(t, path))) - end
			s.calls = nil
			follow(t, f)
			var want []string
			if tail == "whole" {
				rec.holdsFrom(t, 2, helperDied)
				want = []string{fmt.Sprintf("write %d at %d", format.MarkerSize+left, end-format.MarkerSize), "sync", fmt.Sprintf("write %d at %d", format.MarkerSize, end+left), "unlock"}
				reread(t, path).holds(t, table("one"), helperDied)
			} else {
				rec.holds(t)
				want = []string{fmt.Sprintf("write %d at %d", format.MarkerSize, end-format.MarkerSize), "sync", fmt.Sprintf("truncate to %d", end), "sync", "unlock"}
				if n := int64(len(fileBytes(t, path))); n != end {
					t.Errorf("the file is %d bytes long, where the check cuts it back to %d", n, end)
				}
			}
			if strings.Join(s.calls, ", ") != strings.Join(want, ", ") {
				t.Errorf("the follower made the calls\n  %s\nwhere\n  %s\nare wanted", strings.Join(s.calls, ", "), strings.Join(want, ", "))
			}
			s.calls = nil
			follow(t, f)
			if len(s.calls) != 0 {
				t.Errorf("Follow after the check made the calls %s", strings.Join(s.calls, ", "))
			}
		})
	}
}

// TestAFollowerConfirmsWhatLooksLikeDamage: a whole marker after the batch a
// follower stopped at names that batch while the batch fails its checks,
// or names another batch while the batch counts. That's damage, or a
// failed commit's batch written over as the follower read it, so while a
// writer holds the lock, the follower waits for the lock and reads again
// holding it. It reports the damage once the lock comes free, and a timeout
// that isn't damage when the wait runs out, and changes nothing either way.
// While it waits, another Follow on the same Log returns at once. With the
// lock free, the follower's try gets it, and the check reports the damage
// without a wait.
func TestAFollowerConfirmsWhatLooksLikeDamage(t *testing.T) {
	for _, c := range []struct {
		name  string
		build func(w *builder)
	}{
		{"a marked batch that fails its checksum", func(w *builder) {
			at := len(w.b)
			w.commit(table("two"))
			w.b[at+format.BatchHeadSize+2] ^= 0x20
		}},
		{"a batch that counts, then a marker naming another batch", func(w *builder) { w.marker(3, w.batch(table("two"))) }},
		{"a batch that counts, then a marker naming it with another checksum", func(w *builder) { w.marker(2, w.batch(table("two"))+1) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newBuilder(t)
			w.commit(table("one"))
			end := len(w.b)
			c.build(w)
			// follower opens a Log on the file as it was before the batch,
			// then writes the rest, as another process's writer would.
			follower := func(t *testing.T, o Options) (string, *Log, *recorder) {
				path := filepath.Join(t.TempDir(), "db")
				if err := os.WriteFile(path, w.b[:end], 0o644); err != nil {
					t.Fatal(err)
				}
				f, rec := openLog(t, path, o)
				appendTo(t, path, w.b[end:])
				return path, f, rec
			}

			t.Run("the lock comes free", func(t *testing.T) {
				path, f, rec := follower(t, Options{Wait: 5 * time.Second})
				release := holdLock(t, path)
				defer release()
				errc := make(chan error, 1)
				began := time.Now()
				go func() { errc <- f.Follow() }()
				time.Sleep(100 * time.Millisecond)
				other := time.Now()
				if err := f.Follow(); err != nil {
					t.Errorf("a second Follow while the first waits gave %v", err)
				}
				if took := time.Since(other); took > 50*time.Millisecond {
					t.Errorf("a second Follow while the first waits took %v", took)
				}
				release()
				mustBeDamage(t, <-errc, path, 2)
				if took := time.Since(began); took < 100*time.Millisecond {
					t.Errorf("Follow reported damage after %v, before the lock came free", took)
				}
				rec.holds(t, table("one"))
				if !bytes.Equal(fileBytes(t, path), w.b) {
					t.Error("the file changed")
				}
			})

			t.Run("the wait runs out", func(t *testing.T) {
				const wait = 300 * time.Millisecond
				path, f, rec := follower(t, Options{Wait: wait})
				defer holdLock(t, path)()
				began := time.Now()
				err := f.Follow()
				if !errors.Is(err, errs.ErrLockTimeout) || errors.Is(err, errs.ErrDamaged) || !strings.Contains(err.Error(), "batch 2") {
					t.Fatalf("Follow gave %v, where a timeout that names batch 2 and isn't damage is wanted", err)
				}
				if took := time.Since(began); took < wait {
					t.Errorf("Follow gave up after %v, where the wait is %v", took, wait)
				}
				rec.holds(t, table("one"))
				if !bytes.Equal(fileBytes(t, path), w.b) {
					t.Error("the file changed")
				}
			})

			t.Run("the lock free", func(t *testing.T) {
				path, f, rec := follower(t, Options{})
				mustBeDamage(t, f.Follow(), path, 2)
				rec.holds(t, table("one"))
				if !bytes.Equal(fileBytes(t, path), w.b) {
					t.Error("the file changed")
				}
			})
		})
	}
}

// TestAShrunkFileIsDamage: nothing before the end of a marker is ever cut,
// so a file shorter than the end of the log a follower has applied was cut
// or copied over, and Follow reports damage, changing nothing.
func TestAShrunkFileIsDamage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	w := newBuilder(t)
	w.commit(table("one"))
	first := len(w.b)
	w.write(filepath.Join(dir, "older"))
	w.commit(table("two"))
	w.commit(table("three"))
	w.write(path)
	f, rec := openLog(t, path, Options{})
	for _, c := range []struct {
		name   string
		change func()
	}{
		{"cut to its first batch", func() {
			if err := os.Truncate(path, int64(first)); err != nil {
				t.Fatal(err)
			}
		}},
		{"an older copy copied over it", func() { copyOver(t, filepath.Join(dir, "older"), path) }},
	} {
		w.write(path)
		c.change()
		before := fileBytes(t, path)
		err := f.Follow()
		var d *errs.Damage
		if !errors.As(err, &d) || d.Path != path || d.Offset != int64(len(before)) || !strings.Contains(d.Reason, "shorter") {
			t.Errorf("%s: Follow gave %v, where damage at offset %d is wanted", c.name, err, len(before))
		}
		if !bytes.Equal(fileBytes(t, path), before) {
			t.Errorf("%s: the file changed", c.name)
		}
	}
	rec.holds(t, table("one"), table("two"), table("three"))
}

// TestAnotherFileAtThePath: when a compaction's file or a backup has been
// moved into place, Follow reports it with an error that wraps ErrReplaced,
// and reads nothing: the Target isn't reset, and the Log stays on the file
// it holds, for a reload to deal with (F9). A Lock moves to the new file by
// itself, as before, and Follow then follows that one. With nothing at the
// path, Follow reads on in the file it holds, which a writer that locked it
// before it went can still commit to. A Log that opened an empty file finds
// nothing to follow, until another process makes a database of the empty
// file, which puts a new file at the path.
func TestAnotherFileAtThePath(t *testing.T) {
	t.Run("a backup moved into place", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		w, _ := openLog(t, path, Options{})
		commit(t, w, table("one"), table("two"))
		f, rec := openLog(t, path, Options{})
		// The backup is as long as the file it replaces, so only its inode
		// tells it apart.
		another(t, filepath.Join(dir, "backup"), "b_1", "b_2")
		if len(fileBytes(t, filepath.Join(dir, "backup"))) != len(fileBytes(t, path)) {
			t.Fatal("the backup's length differs")
		}
		if err := os.Rename(filepath.Join(dir, "backup"), path); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := f.Follow(); !errors.Is(err, ErrReplaced) || !strings.Contains(err.Error(), path) {
				t.Fatalf("Follow after another file took the path gave %v, where ErrReplaced is wanted", err)
			}
		}
		rec.holds(t, table("one"), table("two"))
		if rec.resets != 0 || f.seq != 2 {
			t.Errorf("Follow reset the Target %d times, and the Log has read to batch %d", rec.resets, f.seq)
		}
		commit(t, f, table("mine"))
		if rec.resets != 1 {
			t.Errorf("Lock reset the Target %d times", rec.resets)
		}
		rec.holds(t, table("b_1"), table("b_2"))
		next, _ := openLog(t, path, Options{})
		commit(t, next, table("after"))
		follow(t, f)
		seqs(t, rec, 1, 2, 4)
		reread(t, path).holds(t, table("b_1"), table("b_2"), table("mine"), table("after"))
	})

	t.Run("nothing at the path", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		w, _ := openLog(t, path, Options{})
		f, rec := openLog(t, path, Options{})
		commit(t, w, table("one"))
		follow(t, f)
		if err := w.Lock(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, filepath.Join(dir, "moved")); err != nil {
			t.Fatal(err)
		}
		err := w.Append(table("two"))
		if e := w.Unlock(); err == nil {
			err = e
		}
		if err != nil {
			t.Fatal(err)
		}
		follow(t, f)
		rec.holds(t, table("one"), table("two"))
	})

	t.Run("an empty file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		f, rec := openLog(t, path, Options{})
		follow(t, f)
		w, _ := openLog(t, path, Options{})
		follow(t, f)
		commit(t, w, table("one")) // makes a database of the empty file, in a new file
		if err := f.Follow(); !errors.Is(err, ErrReplaced) {
			t.Fatalf("Follow once the empty file became a database gave %v, where ErrReplaced is wanted", err)
		}
		rec.holds(t)

		// A database copied into an empty file in place, which HyperCrux
		// never does, has to be read from its start too.
		empty := filepath.Join(dir, "empty")
		if err := os.WriteFile(empty, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		g, rg := openLog(t, empty, Options{})
		copyOver(t, path, empty)
		if err := g.Follow(); !errors.Is(err, ErrReplaced) {
			t.Fatalf("Follow once a database was copied into the empty file gave %v, where ErrReplaced is wanted", err)
		}
		rg.holds(t)
	})
}

// TestAHalfWrittenBatch: while a writer is at work, holding the lock, half a
// batch past the end of the log is a commit under way: a follower applies
// nothing and changes nothing, and applies the batch once the writer has
// finished it and its marker. When the writer dies with half the next batch
// written, the follower's try at the lock gets it, and the check cuts the
// half batch off.
func TestAHalfWrittenBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	w.commit(table("one"))
	w.write(path)
	release := holdLock(t, path)
	f, rec := openLog(t, path, Options{})
	end := len(w.b)
	w.commit(table("two"))
	half := end + (len(w.b)-end)/2
	appendTo(t, path, w.b[end:half])
	follow(t, f)
	rec.holds(t, table("one"))
	if !bytes.Equal(fileBytes(t, path), w.b[:half]) {
		t.Error("the file changed")
	}
	appendTo(t, path, w.b[half:])
	follow(t, f)
	rec.holds(t, table("one"), table("two"))

	end = len(w.b)
	w.commit(table("three"))
	appendTo(t, path, w.b[end:end+30])
	follow(t, f)
	release() // the writer dies
	follow(t, f)
	rec.holds(t, table("one"), table("two"))
	if n := len(fileBytes(t, path)); n != end {
		t.Errorf("the file is %d bytes long, where the follower's check cuts it back to %d", n, end)
	}
}

// hookTarget is a Target that calls before ahead of each batch it's handed,
// then hands it on.
type hookTarget struct {
	Target
	before func(seq uint64)
}

func (h hookTarget) Apply(seq uint64, changes []format.Change) error {
	h.before(seq)
	return h.Target.Apply(seq, changes)
}

// TestAFollowerNeverWaitsForTheMutex: the read position belongs to the
// holder of the write lock's mutex. While another holds it, as an Update in
// this process does from Lock to Unlock, even while its Lock still waits
// for flock, Follow reads nothing and returns at once, and the next Follow
// after the mutex comes free reads what was committed meanwhile. Follow from
// the goroutine that holds the lock returns at once too. A Follow that finds
// another reading on waits for it, so it never comes up short of it, and
// each batch is applied once.
func TestAFollowerNeverWaitsForTheMutex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w, _ := openLog(t, path, Options{})
	rec := &recorder{}
	var mu sync.Mutex
	var block func(seq uint64)
	f, err := Open(fsys.OS{}, path, hookTarget{rec, func(seq uint64) {
		mu.Lock()
		b := block
		mu.Unlock()
		if b != nil {
			b(seq)
		}
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, f)

	f.mu <- struct{}{} // another goroutine's Lock, waiting for flock
	held := true
	defer func() {
		if held { // a failure below left it, and Close would wait for it
			<-f.mu
		}
	}()
	commit(t, w, table("one"))
	began := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- f.Follow() }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		<-f.mu
		held = false
		t.Fatalf("Follow while the mutex was held hadn't returned %v later: it waited for the mutex, and returned %v once it came free", time.Since(began), <-errc)
	}
	if took := time.Since(began); took > 50*time.Millisecond {
		t.Errorf("Follow while the mutex was held took %v", took)
	}
	rec.holds(t)
	<-f.mu
	held = false
	follow(t, f)
	rec.holds(t, table("one"))

	if err := f.Lock(); err != nil {
		t.Fatal(err)
	}
	follow(t, f)
	err = f.Append(table("two"))
	if e := f.Unlock(); err == nil {
		err = e
	}
	if err != nil {
		t.Fatal(err)
	}

	entered, gate := newSignal(), newSignal()
	mu.Lock()
	block = func(uint64) { entered.fire(); gate.wait() }
	mu.Unlock()
	commit(t, w, table("three"), table("four"))
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- f.Follow() }()
	entered.wait()
	go func() { second <- f.Follow() }()
	waited := true
	select {
	case err := <-second:
		waited = false
		t.Errorf("a second Follow returned, with %v, while the first was reading on", err)
	case <-time.After(100 * time.Millisecond):
	}
	gate.fire()
	if err := <-first; err != nil {
		t.Error(err)
	}
	if waited {
		if err := <-second; err != nil {
			t.Error(err)
		}
	}
	seqs(t, rec, 1, 3, 4)
}

// writeHook is a file whose writes call after once they're made.
type writeHook struct {
	fsys.File
	after func(p []byte, off int64)
}

func (f *writeHook) WriteAt(p []byte, off int64) (int, error) {
	n, err := f.File.WriteAt(p, off)
	f.after(p, off)
	return n, err
}

// TestAFollowerNeverAppliesItsOwnCommit: a commit in this process moves the
// read position itself, once its marker is written, while the Update's
// transaction is still open. A Follow made between the marker's write and
// that move, here from inside the marker's write, finds the mutex held and
// reads nothing, so the commit is never handed to the Target as well. Then
// goroutines committing through the follower's Log, goroutines following
// it, and another Log committing, all at once: every batch in the file is
// either one of the follower's own or one handed to its Target, none both,
// once each and in order.
func TestAFollowerNeverAppliesItsOwnCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	reread(t, path)
	var f *Log
	armed := false
	h := &hookFS{FS: fsys.OS{}, file: func(file fsys.File) fsys.File {
		return &writeHook{File: file, after: func(p []byte, off int64) {
			if armed && len(p) == format.MarkerSize && off > f.end {
				armed = false
				if err := f.Follow(); err != nil {
					t.Errorf("Follow inside the commit gave %v", err)
				}
			}
		}}
	}}
	f, rec := openWith(t, h, path, Options{})
	armed = true
	commit(t, f, table("mine"))
	if armed {
		t.Fatal("the commit's marker wasn't written")
	}
	follow(t, f)
	rec.holds(t)

	w, _ := openLog(t, path, Options{})
	n := 30
	if testing.Short() {
		n = 10
	}
	var mine sync.Map // the follower's own commits, by sequence number
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	for g := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range n {
				if err := f.Lock(); err != nil {
					errc <- err
					return
				}
				err := f.Append(table(fmt.Sprintf("f%d_%d", g, i)))
				if err == nil {
					mine.Store(f.seq, true)
				}
				if e := f.Unlock(); err == nil {
					err = e
				}
				if err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range n {
			if err := w.Lock(); err != nil {
				errc <- err
				return
			}
			err := w.Append(table(fmt.Sprintf("w%d", i)))
			if e := w.Unlock(); err == nil {
				err = e
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()
	stop := make(chan struct{})
	var followers sync.WaitGroup
	for range 2 {
		followers.Add(1)
		go func() {
			defer followers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := f.Follow(); err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	followers.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	follow(t, f)

	file := reread(t, path)
	total := uint64(len(file.batches))
	if total != uint64(1+3*n) {
		t.Fatalf("the file holds %d batches, where %d are wanted", total, 1+3*n)
	}
	handed := map[uint64]bool{}
	var last uint64
	for _, b := range rec.batches {
		if b.seq <= last {
			t.Fatalf("batch %d was handed over after batch %d", b.seq, last)
		}
		last = b.seq
		handed[b.seq] = true
		if !sameChanges(b.changes, file.batches[b.seq-1].changes) {
			t.Errorf("batch %d was handed over as %v, where the file holds %v", b.seq, b.changes, file.batches[b.seq-1].changes)
		}
	}
	for seq := uint64(2); seq <= total; seq++ {
		_, own := mine.Load(seq)
		if own == handed[seq] {
			t.Errorf("batch %d is the follower's own: %v, and was handed to its Target: %v", seq, own, handed[seq])
		}
	}
	t.Logf("%d batches: %d of the follower's own, %d handed to it", total, 2*n+1, len(rec.batches))
}

// TestAStuckLogFollows: a stuck Log keeps the write lock and writes nothing
// until it's closed (F5). Its Follow reads on, finds the failed batch it
// couldn't cut back, unmarked, and applies nothing. It makes no call that
// writes, syncs, cuts or lets go of flock, so another Log's Lock still times
// out. Once the stuck Log is closed, the other Log's check marks the failed
// batch, and Follow on the closed Log gives errs.ErrClosed.
func TestAStuckLogFollows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := &spy{}
	a, err := Open(spyOn(s), path, &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	rec := a.t.(*recorder)
	commit(t, a, table("one"))
	b, rb := openLog(t, path, Options{Wait: 200 * time.Millisecond})
	if err := a.Lock(); err != nil {
		t.Fatal(err)
	}
	s.fail = failCalls(2, 3) // the sync, then the last marker written again in the cut back
	err = a.Append(table("two"))
	s.fail = nil
	a.Unlock()
	if !errors.Is(err, errs.ErrStuck) {
		t.Fatalf("the commit gave %v, where errs.ErrStuck is wanted", err)
	}
	before := fileBytes(t, path)
	s.calls = nil
	for range 2 {
		follow(t, a)
	}
	rec.holds(t)
	if len(s.calls) != 0 {
		t.Errorf("Follow on a stuck Log made the calls %s", strings.Join(s.calls, ", "))
	}
	if !bytes.Equal(fileBytes(t, path), before) {
		t.Error("the file changed")
	}
	if err := lockErr(b); !errors.Is(err, errs.ErrLockTimeout) {
		t.Errorf("another Log's Lock after the stuck one followed gave %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Follow(); !errors.Is(err, errs.ErrClosed) {
		t.Errorf("Follow after Close gave %v", err)
	}
	commit(t, b, table("three"))
	rb.holds(t, table("one"), table("two"))
}

// TestALockThatWaitsReadsOn: while a Lock waits for flock, it holds the
// mutex, so Follow reads nothing in its process. So the Lock reads on
// itself between its tries. Another Log commits a batch and keeps the lock,
// and the waiting Lock hands the batch to the Target while it still waits,
// and Follow meanwhile returns at once. Once the other Log lets go, the
// Lock gets the lock, and the next commit goes in after the batch.
func TestALockThatWaitsReadsOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w, _ := openLog(t, path, Options{})
	rec := &recorder{}
	applied := make(chan uint64, 8)
	f, err := Open(fsys.OS{}, path, hookTarget{rec, func(seq uint64) { applied <- seq }}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, f)
	if err := w.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if w.locked {
			w.Unlock()
		}
	}()
	if err := w.Append(table("one")); err != nil {
		t.Fatal(err)
	}
	locked := make(chan error, 1)
	go func() { locked <- f.Lock() }()
	select {
	case seq := <-applied:
		if seq != 1 {
			t.Errorf("the waiting Lock handed over batch %d", seq)
		}
	case <-time.After(2 * time.Second):
		t.Error("the waiting Lock hadn't read the batch committed meanwhile 2 seconds later")
	}
	began := time.Now()
	follow(t, f)
	if took := time.Since(began); took > 50*time.Millisecond {
		t.Errorf("Follow while a Lock waited took %v", took)
	}
	if err := w.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	err = f.Append(table("mine"))
	if e := f.Unlock(); err == nil {
		err = e
	}
	if err != nil {
		t.Fatal(err)
	}
	rec.holds(t, table("one"))
	reread(t, path).holds(t, table("one"), table("mine"))
}

// TestDamageAFollowerReportsAtOnce: a batch that counts holds what its
// writer wrote, so when it's marked and its changes are malformed, or the
// Target refuses them, that's damage no writer at work explains. Follow
// reports it at once, with the batch and its offset, though a writer holds
// the lock, after applying the batches before it, and reports it again at
// the next Follow. Nothing in the file changes.
func TestDamageAFollowerReportsAtOnce(t *testing.T) {
	for _, c := range []struct {
		name   string
		build  func(w *builder)
		refuse uint64
		offset int64 // past the batch's start
	}{
		{"malformed changes", func(w *builder) { w.marker(3, w.raw(1, 3, []byte("X\x03\x00Bad"))) }, 0, format.BatchHeadSize},
		{"changes the Target refuses", func(w *builder) { w.commit(table("three")) }, 3, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			w := newBuilder(t)
			w.commit(table("one"))
			w.write(path)
			rec := &recorder{refuse: c.refuse, damage: true}
			f, err := Open(fsys.OS{}, path, rec, Options{Wait: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			closeAtEnd(t, f)
			end := len(w.b)
			w.commit(table("two"))
			at := int64(len(w.b))
			c.build(w)
			appendTo(t, path, w.b[end:])
			defer holdLock(t, path)()
			for range 2 {
				began := time.Now()
				err := f.Follow()
				var d *errs.Damage
				if !errors.As(err, &d) || d.Path != path || d.Batch != 3 || d.Offset != at+c.offset {
					t.Fatalf("Follow gave %v, where damage in batch 3 at offset %d is wanted", err, at+c.offset)
				}
				if took := time.Since(began); took > time.Second {
					t.Errorf("Follow reported damage after %v, so it waited for the lock", took)
				}
			}
			rec.holds(t, table("one"), table("two"))
			if !bytes.Equal(fileBytes(t, path), w.b) {
				t.Error("the file changed")
			}
		})
	}
}
