// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Reloading's cases one by one (F9): a Log that follows the database finds
// another file at the path, a compaction's or a backup moved into place,
// and Reload reads it from its start.

// loads is a recorder that's a Reloader too: it notes the error of each
// Loaded, and calls at in it when that's set.
type loads struct {
	*recorder
	errs []error
	at   func(err error)
}

func (r *loads) Loaded(err error) {
	r.errs = append(r.errs, err)
	if r.at != nil {
		r.at(err)
	}
}

// loadsWere checks the errors the Target's Loaded calls were given, by
// whether each is nil.
func (r *loads) loadsWere(t *testing.T, ok ...bool) {
	t.Helper()
	var got []bool
	for _, err := range r.errs {
		got = append(got, err == nil)
	}
	if fmt.Sprint(got) != fmt.Sprint(ok) {
		t.Fatalf("Loaded was called with %v, where calls that worked %v are wanted", r.errs, ok)
	}
}

// openLoads opens the database at path through files, with a loads, and
// closes it when the test ends.
func openLoads(t *testing.T, files fsys.FS, path string, o Options) (*Log, *loads) {
	t.Helper()
	r := &loads{recorder: &recorder{}}
	l, err := Open(files, path, r, o)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, l)
	return l, r
}

// moveIn makes a database beside path with a batch for each name, and moves
// it into place, as a backup is put back.
func moveIn(t *testing.T, path string, tables ...string) {
	t.Helper()
	other := path + ".other"
	another(t, other, tables...)
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
}

// replacedNow calls l.Follow where a test wants it to find another file at
// the path.
func replacedNow(t *testing.T, l *Log) {
	t.Helper()
	if err := l.Follow(); !errors.Is(err, ErrReplaced) {
		t.Fatalf("Follow gave %v, where ErrReplaced is wanted", err)
	}
}

// closeHook is a file that counts the times it's closed.
type closeHook struct {
	fsys.File
	closes *int
}

func (f *closeHook) Close() error {
	*f.closes++
	return f.File.Close()
}

// TestReloadReadsTheFileAtThePath: once Follow has found another file at the
// path, Reload closes the old file, resets the Target, reads the new file
// from its start and tells the Target it has, and Follow then follows the
// new file: what another Log commits there comes with the numbers after the
// new file's batches, and the follower's own commits go in after them too.
// Through a compaction the copy holds the same as before, and through a
// backup moved into place, what the backup holds. A Reload with nothing to
// reload reads nothing: when the path names the Log's own file, when nothing
// is at the path, and after Close, when it gives errs.ErrClosed.
func TestReloadReadsTheFileAtThePath(t *testing.T) {
	t.Run("a backup moved into place", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		w, _ := openLog(t, path, Options{})
		commit(t, w, table("a1"), table("a2"))
		closes := 0
		files := &hookFS{FS: fsys.OS{}, file: func(f fsys.File) fsys.File { return &closeHook{File: f, closes: &closes} }}
		f, rec := openLoads(t, files, path, Options{})
		moveIn(t, path, "b1", "b2", "b3")
		replacedNow(t, f)
		if err := f.Reload(); err != nil {
			t.Fatal(err)
		}
		if rec.resets != 1 || closes != 1 {
			t.Errorf("the reload reset the Target %d times, and closed %d files", rec.resets, closes)
		}
		rec.loadsWere(t, true)
		rec.holds(t, table("b1"), table("b2"), table("b3"))
		follow(t, f)
		if err := f.Reload(); err != nil || rec.resets != 1 || len(rec.errs) != 1 {
			t.Errorf("a Reload with nothing to reload gave %v, and reset the Target %d times in all", err, rec.resets)
		}
		commit(t, w, table("after")) // w's Lock moves to the new file by itself
		follow(t, f)
		seqs(t, rec.recorder, 1, 2, 3, 4)
		commit(t, f, table("mine"))
		reread(t, path).holds(t, table("b1"), table("b2"), table("b3"), table("after"), table("mine"))
	})

	t.Run("a compaction", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		d := openDBT(t, fsys.OS{}, path, Options{})
		fill(t, d, 1, 12)
		o := openDBT(t, fsys.OS{}, path, Options{})
		if err := d.compact(); err != nil {
			t.Fatal(err)
		}
		replacedNow(t, o.l)
		if err := o.l.Reload(); err != nil {
			t.Fatal(err)
		}
		if o.c.resets != 1 || o.state() != d.state() {
			t.Errorf("after %d resets, the follower's copy holds\n%s\nwhere the compacting one holds\n%s", o.c.resets, o.state(), d.state())
		}
		fill(t, d, 13, 20)
		if err := o.l.Follow(); err != nil {
			t.Fatal(err)
		}
		fill(t, o, 21, 22)
		if err := d.l.Follow(); err != nil {
			t.Fatal(err)
		}
		if o.state() != d.state() || stateOfFile(t, path) != d.state() || o.c.resets != 1 {
			t.Error("the two copies and the file differ")
		}
	})

	t.Run("nothing to reload", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		f, rec := openLoads(t, fsys.OS{}, path, Options{})
		commit(t, f, table("one"))
		if err := f.Reload(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, filepath.Join(dir, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := f.Reload(); err != nil {
			t.Fatal(err)
		}
		if rec.resets != 0 || len(rec.errs) != 0 {
			t.Errorf("Reloads with nothing to reload reset the Target %d times, and called Loaded %d", rec.resets, len(rec.errs))
		}
		rec.holds(t)
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Reload(); !errors.Is(err, errs.ErrClosed) {
			t.Errorf("Reload after Close gave %v", err)
		}
	})
}

// TestAReloadNeverWaitsForTheMutex: Reload keeps Follow's place in the lock
// order. While another holds the write lock's mutex, as an Update in this
// process does from Lock to Unlock, Reload reads nothing and returns at
// once, since that would be a wait for the Update's function, whose Lock
// moves to the new file itself. Once the mutex is free, it reloads. While a
// Reload is under way, a Follow waits for it, and finds nothing more to read
// once it has ended.
func TestAReloadNeverWaitsForTheMutex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w, _ := openLog(t, path, Options{})
	commit(t, w, table("one"))
	rec := &recorder{}
	var mu sync.Mutex
	var block func()
	f, err := Open(fsys.OS{}, path, hookTarget{rec, func(uint64) {
		mu.Lock()
		b := block
		mu.Unlock()
		if b != nil {
			b()
		}
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, f)
	moveIn(t, path, "b1", "b2")
	replacedNow(t, f)

	f.mu <- struct{}{} // an Update's, from its Lock to its Unlock
	held := true
	defer func() {
		if held { // a failure below left it, and Close would wait for it
			<-f.mu
		}
	}()
	began := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- f.Reload() }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		<-f.mu
		held = false
		t.Fatalf("Reload while the mutex was held hadn't returned %v later: it waited for the mutex, and returned %v once it came free", time.Since(began), <-errc)
	}
	if took := time.Since(began); took > 50*time.Millisecond {
		t.Errorf("Reload while the mutex was held took %v", took)
	}
	if rec.resets != 0 {
		t.Error("Reload read the new file while the mutex was held")
	}
	<-f.mu
	held = false

	entered, gate := newSignal(), newSignal()
	mu.Lock()
	block = func() { entered.fire(); gate.wait() }
	mu.Unlock()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- f.Reload() }()
	entered.wait()
	go func() { second <- f.Follow() }()
	waited := true
	select {
	case err := <-second:
		waited = false
		t.Errorf("a Follow returned, with %v, while a Reload was under way", err)
	case <-time.After(100 * time.Millisecond):
	}
	mu.Lock()
	block = nil
	mu.Unlock()
	gate.fire()
	if err := <-first; err != nil {
		t.Error(err)
	}
	if waited {
		if err := <-second; err != nil {
			t.Error(err)
		}
	}
	if rec.resets != 1 {
		t.Errorf("the Target was reset %d times", rec.resets)
	}
	rec.holds(t, table("b1"), table("b2"))
}

// TestAReloadChecksTheNewFile: Reload checks the new file as Open does. A
// batch a writer left at its end, synced and unmarked when the writer died,
// is written again, marked and handed to the Target, since the reload gets
// the lock at once. Damage past the end of its log is reported, after the
// batches before it: the Target is told so, and the Log holds no file, so
// Follow asks for another reload, which reads the next file at the path.
func TestAReloadChecksTheNewFile(t *testing.T) {
	t.Run("a batch a writer left", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		another(t, path, "one")
		f, rec := openLoads(t, fsys.OS{}, path, Options{})
		b := newBuilder(t)
		b.commit(table("b1"))
		sum := b.batch(table("b2")) // its writer died after its sync
		b.write(path + ".other")
		if err := os.Rename(path+".other", path); err != nil {
			t.Fatal(err)
		}
		replacedNow(t, f)
		if err := f.Reload(); err != nil {
			t.Fatal(err)
		}
		rec.loadsWere(t, true)
		rec.holds(t, table("b1"), table("b2"))
		b.marker(2, sum)
		if !bytes.Equal(fileBytes(t, path), b.b) {
			t.Error("the reload didn't mark the batch the writer left")
		}
	})

	t.Run("damage past the end of its log", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		another(t, path, "one")
		f, rec := openLoads(t, fsys.OS{}, path, Options{})
		b := newBuilder(t)
		b.commit(table("b1"))
		end := len(b.b)
		b.commit(table("b2"))
		b.b[end+format.BatchHeadSize+2] ^= 0xff // batch 2 fails its checksum, and its marker stays
		b.write(path + ".other")
		if err := os.Rename(path+".other", path); err != nil {
			t.Fatal(err)
		}
		before := fileBytes(t, path)
		replacedNow(t, f)
		var d *errs.Damage
		if err := f.Reload(); !errors.As(err, &d) || d.Batch != 2 || d.Path != path {
			t.Fatalf("Reload gave %v, where damage in batch 2 is wanted", err)
		}
		rec.loadsWere(t, false)
		rec.holds(t, table("b1"))
		if !bytes.Equal(fileBytes(t, path), before) {
			t.Error("the file changed")
		}
		replacedNow(t, f) // the Log holds no file
		moveIn(t, path, "c1")
		if err := f.Reload(); err != nil {
			t.Fatal(err)
		}
		rec.loadsWere(t, false, true)
		rec.holds(t, table("c1"))
		if rec.resets != 2 {
			t.Errorf("the Target was reset %d times", rec.resets)
		}
	})
}

// failFS is the real calls, with a read of a file it opened failing with
// errDrive once armed, at the failAt-th read since then.
type failFS struct {
	fsys.FS
	mu     sync.Mutex
	armed  bool
	reads  int
	failAt int
}

func (f *failFS) Open(path string) (fsys.File, error) {
	file, err := f.FS.Open(path)
	if err != nil {
		return nil, err
	}
	return &failFile{File: file, fs: f}, nil
}

func (f *failFS) arm(at int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.reads, f.failAt = true, 0, at
}

func (f *failFS) disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = false
}

type failFile struct {
	fsys.File
	fs *failFS
}

func (f *failFile) ReadAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	fail := false
	if f.fs.armed {
		f.fs.reads++
		fail = f.fs.reads == f.fs.failAt
	}
	f.fs.mu.Unlock()
	if fail {
		return 0, errDrive
	}
	return f.File.ReadAt(p, off)
}

// bigPut is a batch of one put with a text of a little over a megabyte, so
// that each batch is read by itself, past the reader's window.
func bigPut(i int) []format.Change {
	return []format.Change{{Op: format.Put, Key: fmt.Sprintf("big:%d", i), Fields: []format.Field{{Name: "s", Value: value.Text(strings.Repeat(string(rune('a'+i)), window+window/4))}}}}
}

// TestAReloadThatFailsPartway: a read of the new file that fails partway,
// once the first of its batches has been read, and a batch of it that the
// Target refuses. Either way Reload gives the error, the Target holds the
// batches before the failure, each whole, and is told the reload failed, and
// the Log holds no file, so Follow asks for another reload. The next Reload,
// once nothing fails, reads the whole file from its start.
func TestAReloadThatFailsPartway(t *testing.T) {
	t.Run("a read that fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		another(t, path, "one")
		files := &failFS{FS: fsys.OS{}}
		f, rec := openLoads(t, files, path, Options{})
		other := path + ".other"
		w, _ := openLog(t, other, Options{})
		commit(t, w, bigPut(1), bigPut(2), bigPut(3))
		if err := os.Rename(other, path); err != nil {
			t.Fatal(err)
		}
		replacedNow(t, f)
		// The reads: the header, the window from the first batch, the first
		// batch with its marker, then the window from the second.
		files.arm(4)
		if err := f.Reload(); !errors.Is(err, errDrive) {
			t.Fatalf("Reload when a read failed gave %v", err)
		}
		files.disarm()
		rec.loadsWere(t, false)
		rec.holds(t, bigPut(1))
		replacedNow(t, f)
		if err := f.Reload(); err != nil {
			t.Fatal(err)
		}
		rec.loadsWere(t, false, true)
		rec.holds(t, bigPut(1), bigPut(2), bigPut(3))
		if rec.resets != 2 {
			t.Errorf("the Target was reset %d times", rec.resets)
		}
	})

	t.Run("a batch the Target refuses", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		another(t, path, "one")
		f, rec := openLoads(t, fsys.OS{}, path, Options{})
		rec.refuse, rec.damage = 2, true
		moveIn(t, path, "b1", "b2", "b3")
		replacedNow(t, f)
		var d *errs.Damage
		if err := f.Reload(); !errors.As(err, &d) || d.Batch != 2 || !strings.Contains(err.Error(), storeDamage) {
			t.Fatalf("Reload gave %v, where the Target's damage in batch 2 is wanted", err)
		}
		rec.loadsWere(t, false)
		rec.holds(t, table("b1"))
		replacedNow(t, f)
		rec.refuse = 0
		if err := f.Reload(); err != nil {
			t.Fatal(err)
		}
		rec.loadsWere(t, false, true)
		rec.holds(t, table("b1"), table("b2"), table("b3"))
	})
}

// TestAnotherFileTakesThePathDuringAReload: when another file takes the path
// while Reload reads the new one, its check finds it holding the lock, and
// Reload reads that one from its start instead, as Open does, and tells the
// Target once at the end. Open itself, when another file takes the path
// before it holds the lock, resets the Target once, reads the file at the
// path, and tells the Target too, while an Open with nothing of the kind
// tells it nothing.
func TestAnotherFileTakesThePathDuringAReload(t *testing.T) {
	t.Run("in Reload", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		another(t, path, "one")
		third := path + ".third"
		another(t, third, "c1", "c2")
		f, rec := openLoads(t, hooked(map[int]func(){2: func() {
			if err := os.Rename(third, path); err != nil {
				t.Error(err)
			}
		}}), path, Options{})
		rec.loadsWere(t)
		moveIn(t, path, "b1")
		replacedNow(t, f)
		if err := f.Reload(); err != nil {
			t.Fatal(err)
		}
		rec.loadsWere(t, true)
		rec.holds(t, table("c1"), table("c2"))
		if rec.resets != 2 {
			t.Errorf("the Target was reset %d times", rec.resets)
		}
		follow(t, f)
	})

	t.Run("in Open", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db")
		another(t, path, "one")
		other := path + ".other"
		another(t, other, "b1")
		_, rec := openLoads(t, hooked(map[int]func(){1: func() {
			if err := os.Rename(other, path); err != nil {
				t.Error(err)
			}
		}}), path, Options{})
		rec.loadsWere(t, true)
		rec.holds(t, table("b1"))
		if rec.resets != 1 {
			t.Errorf("the Target was reset %d times", rec.resets)
		}
	})
}

// TestLockReadsTheNewFileBeforeItWaits: a Lock that finds another file at
// the path reads it from its start and tells the Target, before it waits for
// the new file's lock, which another writer holds. So a Target keeps its
// readers off no longer than the read takes. Then the Lock gets the lock,
// and the commit goes into the new file.
func TestLockReadsTheNewFileBeforeItWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	f, rec := openLoads(t, fsys.OS{}, path, Options{Wait: 5 * time.Second})
	commit(t, f, table("one"))
	moveIn(t, path, "b1")
	release := holdLock(t, path)
	defer release()
	loaded := make(chan error, 1)
	rec.at = func(err error) { loaded <- err }
	locked := make(chan error, 1)
	go func() { locked <- f.Lock() }()
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-locked:
		t.Fatalf("Lock returned %v before the Target heard the new file was read", err)
	case <-time.After(3 * time.Second):
		t.Fatal("the Target hadn't heard the new file was read 3 seconds after Lock began")
	}
	select {
	case err := <-locked:
		t.Fatalf("Lock returned %v while another held the new file's lock", err)
	case <-time.After(50 * time.Millisecond):
	}
	rec.holds(t, table("b1"))
	release()
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	err := f.Append(table("mine"))
	if e := f.Unlock(); err == nil {
		err = e
	}
	if err != nil {
		t.Fatal(err)
	}
	rec.loadsWere(t, true)
	reread(t, path).holds(t, table("b1"), table("mine"))
}

// TestAStuckLogDoesntReload: a stuck Log keeps its file and its lock until
// it's closed (F5), so when another file takes the path, its Reload gives
// errs.ErrStuck and reads nothing.
func TestAStuckLogDoesntReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := &spy{}
	rec := &recorder{}
	a, err := Open(spyOn(s), path, rec, Options{})
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	commit(t, a, table("one"))
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
	moveIn(t, path, "b1")
	replacedNow(t, a)
	if err := a.Reload(); !errors.Is(err, errs.ErrStuck) {
		t.Errorf("Reload on a stuck Log gave %v", err)
	}
	if rec.resets != 0 {
		t.Error("a stuck Log reloaded")
	}
}

// reloadTarget is the public package's target in small, a Reloader: while
// Open reads the file, and from a Reset until Loaded, nobody can reach the
// copy, so each batch goes straight in, and otherwise through a transaction
// of its own. before, when it's set, is called ahead of each batch that goes
// straight in after a Reset.
type reloadTarget struct {
	s                *store.Store
	opening, loading bool
	before           func()
}

func (r *reloadTarget) Apply(seq uint64, changes []format.Change) error {
	if r.loading && r.before != nil {
		r.before()
	}
	if r.opening || r.loading {
		return r.s.LoadBatch(seq, changes)
	}
	return r.s.ApplyBatch(seq, changes)
}

func (r *reloadTarget) Reset() {
	r.s = store.New()
	r.loading = true
}

func (r *reloadTarget) Loaded(error) { r.loading = false }

// heap is the bytes the heap holds now, after a garbage collection when
// collect is true.
func heap(collect bool) uint64 {
	if collect {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestAReloadCollectsTheOldCopy measures, roughly, what a reload of a
// database of 4,000 records with 384-value vectors holds in memory, with the
// store as the Target, as the public package joins them, and the garbage
// collector's own runs switched off, so only the reload's collection counts.
// The heap is measured without a collection as the first batch of the new
// file goes in: by then the old copy has gone, so the heap never holds the
// two at once. With the collector's runs switched off, the heap after the
// reload also holds what the load threw away as the copy grew, so that's
// logged and not held to anything; the new copy, once collected, is as large
// as the old one.
func TestAReloadCollectsTheOldCopy(t *testing.T) {
	records := 4000
	if testing.Short() {
		records = 2000
	}
	path := filepath.Join(t.TempDir(), "db")
	d, err := openDB(fsys.OS{}, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 384)
	for i := 0; i < records; i += 500 {
		err := d.commit(func(tx *store.Tx) error {
			for j := i; j < i+500 && j < records; j++ {
				for k := range vec {
					vec[k] = float32(j%97+k) + 0.5
				}
				if err := tx.Put(fmt.Sprintf("docs:%06d", j), []format.Field{{Name: "vec", Value: value.Vector(vec)}}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := d.l.Close(); err != nil {
		t.Fatal(err)
	}
	d = nil
	base := heap(true)

	r := &reloadTarget{s: store.New(), opening: true}
	o, err := Open(fsys.OS{}, path, r, Options{})
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, o)
	r.opening = false
	copyOver(t, path, path+".other") // a backup, the same database in another file
	if err := os.Rename(path+".other", path); err != nil {
		t.Fatal(err)
	}
	replacedNow(t, o)
	before := heap(true)
	one := before - base // a copy, with the Log
	var first uint64
	r.before = func() {
		if first == 0 {
			first = heap(false)
		}
	}
	old := debug.SetGCPercent(-1)
	err = o.Reload()
	after := heap(false)
	debug.SetGCPercent(old)
	if err != nil {
		t.Fatal(err)
	}
	live := heap(true) - base
	t.Logf("a copy of %d records takes %.1f MB, with the Log; at the reload's first batch the heap held %.1f MB past what it holds without one, and after the reload, uncollected, %.1f MB, where the new copy takes %.1f MB",
		records, float64(one)/1e6, (float64(first)-float64(base))/1e6, (float64(after)-float64(base))/1e6, float64(live)/1e6)
	if one < 2e6 {
		t.Fatalf("a copy takes %d bytes, too few to measure by", one)
	}
	if first == 0 || first > base+one/2 {
		t.Errorf("at the first batch of the new file, the heap held %d bytes past what it holds without a copy, where a copy takes %d: the old copy hadn't gone", first-base, one)
	}
	if live < one*3/4 || live > one*5/4 {
		t.Errorf("after the reload, the new copy takes %d bytes, where the old one took %d", live, one)
	}
}
