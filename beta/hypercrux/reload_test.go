// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Reads that follow other processes, and reloads (F9): a DB whose file is
// replaced under it, by a compaction in another process or a backup moved
// into place, reads the new file at its next read, holding its other reads
// off meanwhile, and never holds the old copy and the new one at once.

// loadInto is a logfile.Target that puts every batch straight into a copy
// of its own, which nothing else reads.
type loadInto struct{ s *store.Store }

func (l *loadInto) Apply(seq uint64, changes []format.Change) error {
	return l.s.LoadBatch(seq, changes)
}

func (l *loadInto) Reset() { l.s = store.New() }

// compactFile compacts the database at path, as Compact in another process
// would (G5): it opens the file with a copy of its own, takes the write lock,
// and writes the copy's snapshot into NAME.compact, which takes the path.
func compactFile(t *testing.T, path string) {
	t.Helper()
	c := &loadInto{s: store.New()}
	l, err := logfile.Open(fsys.OS{}, path, c, logfile.Options{})
	ok(t, err)
	defer l.Close()
	ok(t, l.Lock())
	defer l.Unlock()
	ok(t, c.s.Read(func(r store.Reader) error { return l.Compact(r.Snapshot()) }))
}

// backUp copies the database at path beside it and moves the copy into
// place, as a backup is put back: the same commits, in another file.
func backUp(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	ok(t, err)
	ok(t, os.WriteFile(path+".backup", b, 0o644))
	ok(t, os.Rename(path+".backup", path))
}

// moveIn makes a database beside path, has fill commit to it, and moves it
// into place, as a backup of other contents is put back.
func moveIn(t *testing.T, path string, fill func(db *hc.DB)) {
	t.Helper()
	other := path + ".other"
	db, err := hc.Open(other)
	ok(t, err)
	fill(db)
	ok(t, db.Close())
	ok(t, os.Rename(other, path))
}

// bigRecords puts records big:1 to big:n, each in a batch of its own, with
// a text a little over a megabyte, so the log reads them one at a time.
func bigRecords(n int) func(db *hc.DB) {
	return func(db *hc.DB) {
		for i := 1; i <= n; i++ {
			if err := db.Put(fmt.Sprintf("big:%d", i), hc.Fields{"s": strings.Repeat(string(rune('a'+i)), 1<<20+1000)}); err != nil {
				panic(err)
			}
		}
	}
}

// trapFS is the real calls, with hooks a test sets: one ahead of each read
// of a file opened through it, given the path the file was opened at, which
// can fail the read, and one ahead of each open.
type trapFS struct {
	fsys.FS
	mu     sync.Mutex
	read   func(path string) error
	opened func(path string)
}

func (f *trapFS) setRead(fn func(path string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = fn
}

func (f *trapFS) setOpen(fn func(path string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = fn
}

func (f *trapFS) Open(path string) (fsys.File, error) {
	f.mu.Lock()
	hook := f.opened
	f.mu.Unlock()
	if hook != nil {
		hook(path)
	}
	file, err := f.FS.Open(path)
	if err != nil {
		return nil, err
	}
	return &trapFile{File: file, fs: f, path: path}, nil
}

type trapFile struct {
	fsys.File
	fs   *trapFS
	path string
}

func (f *trapFile) ReadAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	hook := f.fs.read
	f.fs.mu.Unlock()
	if hook != nil {
		if err := hook(f.path); err != nil {
			return 0, err
		}
	}
	return f.File.ReadAt(p, off)
}

// errDisk is the error a trapFS's read fails with when a test says so.
var errDisk = errors.New("the disk failed the read")

// await waits for c to close, and fails the test if it hasn't after 10
// seconds, so a test that goes wrong fails instead of hanging.
func await(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatalf("10 seconds went by waiting for %s", what)
	}
}

// watchdog panics, with every goroutine's stack, if the test is still going
// a minute after it starts, so a test that waits for ever on a lock fails at
// once with what waits, where it would hang until the test binary's own
// timeout. The test calls the function it returns as it ends.
func watchdog() func() bool {
	timer := time.AfterFunc(time.Minute, func() {
		stacks := make([]byte, 1<<20)
		panic(fmt.Sprintf("the test was still going a minute in, so something waits for ever:\n%s", stacks[:runtime.Stack(stacks, true)]))
	})
	return timer.Stop
}

// openTrapped opens the database at path through a trapFS, and closes it
// when the test ends.
func openTrapped(t *testing.T, path string) (*hc.DB, *trapFS) {
	t.Helper()
	trap := &trapFS{FS: fsys.OS{}}
	db, err := hc.OpenWith(trap, path, logfile.Options{})
	ok(t, err)
	t.Cleanup(func() { db.Close() })
	return db, trap
}

// TestAFileReplacedUnderADBIsReadAtItsNextRead: a compaction in another
// process, then a backup moved into place, then a backup of other contents.
// After each, the DB's next read finds the file at the path, reads it from
// its start, and gives what that file holds, and its next Update commits
// there.
func TestAFileReplacedUnderADBIsReadAtItsNextRead(t *testing.T) {
	defer watchdog()()
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	for i := range 20 {
		ok(t, db.Put(fmt.Sprintf("docs:%d", i%7), hc.Fields{"n": i}))
	}
	before := look(db, "docs:0", "docs:6")
	compactFile(t, path)
	if got := look(db, "docs:0", "docs:6"); got != before {
		t.Fatalf("after a compaction, the DB reads\n%s\nwhere before it, it read\n%s", got, before)
	}
	ok(t, db.Put("docs:7", hc.Fields{"n": 7}))
	backUp(t, path)
	if f, err := db.Get("docs:7"); err != nil || f["n"] != int64(7) {
		t.Fatalf("after a backup moved into place, docs:7 gives %v, %v", f, err)
	}
	moveIn(t, path, func(o *hc.DB) { ok(t, o.Put("people:1", hc.Fields{"name": "Dana"})) })
	if _, err := db.Get("docs:7"); !errors.Is(err, hc.ErrNotFound) {
		t.Fatalf("after another database moved into place, docs:7 gives %v", err)
	}
	if rep, err := db.Check(); err != nil || rep.Records != 1 {
		t.Fatalf("after another database moved into place, Check gives %+v, %v", rep, err)
	}
	ok(t, db.Put("people:2", nil))
	if got, want := look(open(t, path), "docs:7", "people:1", "people:2"), look(db, "docs:7", "people:1", "people:2"); got != want {
		t.Fatalf("a fresh open reads\n%s\nwhere the DB reads\n%s", got, want)
	}
}

// TestTwoProcessesOnOneFile: this process and a helper take turns on one
// file. Each commits, and the other's next read gives the commit, with no
// Update of its own in between. Along the way another process compacts the
// file, and then a backup is moved into place, and both carry on in the new
// file at their next call. At the end each holds every commit of both.
func TestTwoProcessesOnOneFile(t *testing.T) {
	defer watchdog()()
	n := 60
	if testing.Short() {
		n = 24
	}
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "HYPERCRUX_BETA_ROLE=turns", "HYPERCRUX_BETA_PATH="+path)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	ok(t, err)
	outPipe, err := cmd.StdoutPipe()
	ok(t, err)
	ok(t, cmd.Start())
	defer func() {
		in.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()
	out := bufio.NewScanner(outPipe)
	line := func() string {
		t.Helper()
		if !out.Scan() {
			t.Fatalf("the helper ended: %v", out.Err())
		}
		return out.Text()
	}
	if l := line(); l != "open" {
		t.Fatalf("the helper said %q", l)
	}
	for k := 1; k <= n; k++ {
		switch k {
		case n / 3:
			compactFile(t, path)
		case 2 * n / 3:
			backUp(t, path)
		}
		ok(t, db.Put(fmt.Sprintf("a:%d", k), hc.Fields{"n": k}))
		fmt.Fprintln(in, k)
		if l := line(); l != fmt.Sprintf("saw %d", k) {
			t.Fatalf("the other process's next read after commit a:%d: %s", k, l)
		}
		if f, err := db.Get(fmt.Sprintf("b:%d", k)); err != nil || f["n"] != int64(k) {
			t.Fatalf("this process's next read after the other's commit b:%d gives %v, %v", k, f, err)
		}
	}
	in.Close()
	if l := line(); l != fmt.Sprintf("records %d", 2*n) {
		t.Fatalf("at the end, the other process says %q, where it holds %d records", l, 2*n)
	}
	ok(t, cmd.Wait())
	if rep, err := db.Check(); err != nil || rep.Records != 2*n {
		t.Fatalf("at the end, this process's Check gives %+v, %v", rep, err)
	}
}

// TestReadsWaitWhileAReloadFillsTheCopy: an Update's Lock reloads a new file
// of six records of a megabyte each, slowly, and reads through the database
// on four goroutines start once the reload has. They wait for the reload,
// and every one sees the whole new file, or that and the Update's commit:
// none sees the old copy or part of the new one.
func TestReadsWaitWhileAReloadFillsTheCopy(t *testing.T) {
	defer watchdog()()
	path := filepath.Join(t.TempDir(), "test.hcx")
	db, trap := openTrapped(t, path)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	moveIn(t, path, bigRecords(6))
	started := make(chan struct{})
	var once sync.Once
	trap.setRead(func(p string) error {
		if p == path {
			once.Do(func() { close(started) })
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	})
	defer trap.setRead(nil)
	updated := make(chan error, 1)
	go func() { updated <- db.Update(func(tx *hc.Tx) error { return tx.Put("big:7", nil) }) }()
	await(t, started, "the reload to begin")

	stop := make(chan struct{})
	var mu sync.Mutex
	counts := map[int]int{}
	var readErr error
	var wg sync.WaitGroup
	var stopping sync.Once
	end := func() {
		stopping.Do(func() { close(stop) })
		wg.Wait()
	}
	defer end() // so no reader goes on after a failure
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rep, err := db.Check()
				mu.Lock()
				counts[rep.Records]++
				if err != nil && readErr == nil {
					readErr = err
				}
				mu.Unlock()
			}
		}()
	}
	ok(t, <-updated)
	trap.setRead(nil)
	time.Sleep(20 * time.Millisecond)
	end()
	ok(t, readErr)
	for records, times := range counts {
		if records != 6 && records != 7 {
			t.Errorf("%d reads saw %d records, where a reload of 6 and an Update of 1 more leave 6 or 7", times, records)
		}
	}
	t.Logf("the reads saw, by records: %v", counts)
}

// TestReadsGoOnThroughReloads: four goroutines read through a DB while
// another handle commits, and after every few commits a compaction, or a
// backup of the same commits, puts another file at the path, so the reads
// keep finding one, several of them at once. Each read sees the commits from
// the first, in order, at least as many as the read before it, and nothing
// waits for ever, which a watchdog reports.
func TestReadsGoOnThroughReloads(t *testing.T) {
	commits := 120
	if testing.Short() {
		commits = 40
	}
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	w := open(t, path)
	defer watchdog()()
	stop := make(chan struct{})
	errc := make(chan error, 4)
	reads := make([]int, 4)
	var wg sync.WaitGroup
	for g := range reads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			last := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				recs, err := db.Scan("r:", "", 0)
				if err != nil {
					errc <- err
					return
				}
				for i, rec := range recs {
					if rec.Key != fmt.Sprintf("r:%04d", i+1) {
						errc <- fmt.Errorf("a read found %s where r:%04d comes", rec.Key, i+1)
						return
					}
				}
				if len(recs) < last {
					errc <- fmt.Errorf("a read found %d commits, after one that found %d", len(recs), last)
					return
				}
				last = len(recs)
				reads[g]++
			}
		}()
	}
	for i := 1; i <= commits; i++ {
		ok(t, w.Put(fmt.Sprintf("r:%04d", i), nil))
		switch i % 10 {
		case 3:
			compactFile(t, path)
		case 7:
			backUp(t, path)
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if recs, err := db.Scan("r:", "", 0); err != nil || len(recs) != commits {
		t.Errorf("at the end, a read finds %d commits, with %v, where %d were made", len(recs), err, commits)
	}
	t.Logf("%d commits, %d compactions and %d backups, and %v reads", commits, (commits+7)/10, (commits+3)/10, reads)
}

// TestAReloadWhileAnUpdateWaits: an Update whose Lock waits for flock while
// a backup is moved into place, and an Update that starts while a read's
// reload is under way.
//
//   - Another handle holds the write lock when the backup goes in. The DB's
//     Update waits for it, holding the DB's mutex, and a read meanwhile goes
//     ahead at once on the copy as it is. Once the other handle's commit,
//     which goes into the old file, has let go, the Update's Lock reloads,
//     and its function sees the backup. Both handles then read the backup
//     and the Update's commit, and not the commit that raced the move.
//   - A read's reload of a slow file holds the DB's mutex, and an Update
//     started meanwhile waits for it, then sees the new file.
func TestAReloadWhileAnUpdateWaits(t *testing.T) {
	defer watchdog()()
	t.Run("the Update's Lock waits for flock", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.hcx")
		a := open(t, path)
		b, err := hc.OpenWith(fsys.OS{}, path, logfile.Options{Wait: 5 * time.Second})
		ok(t, err)
		t.Cleanup(func() { b.Close() })
		ok(t, a.Put("docs:1", hc.Fields{"n": 1}))
		inside, release := make(chan struct{}), make(chan struct{})
		var releasing sync.Once
		let := func() { releasing.Do(func() { close(release) }) }
		defer let() // before the cleanup's Close, which would wait for b's Update
		bDone := make(chan error, 1)
		go func() {
			bDone <- b.Update(func(tx *hc.Tx) error {
				close(inside)
				<-release
				return tx.Put("raced:1", nil)
			})
		}()
		await(t, inside, "b's Update to begin")
		moveIn(t, path, func(o *hc.DB) { ok(t, o.Put("backup:1", nil)) })
		aDone := make(chan error, 1)
		go func() {
			aDone <- a.Update(func(tx *hc.Tx) error {
				if _, err := tx.Get("backup:1"); err != nil {
					return fmt.Errorf("a's Update doesn't see the backup: %w", err)
				}
				if _, err := tx.Get("docs:1"); !errors.Is(err, hc.ErrNotFound) {
					return fmt.Errorf("a's Update sees docs:1 from the old file: %v", err)
				}
				return tx.Put("after:1", nil)
			})
		}()
		time.Sleep(50 * time.Millisecond) // a's Lock waits for flock on the old file
		began := time.Now()
		if f, err := a.Get("docs:1"); err != nil || f["n"] != int64(1) {
			t.Errorf("a read while a's Update waits gives %v, %v, where the copy as it is holds docs:1", f, err)
		}
		if took := time.Since(began); took > time.Second {
			t.Errorf("a read while a's Update waited took %v", took)
		}
		select {
		case err := <-aDone:
			t.Fatalf("a's Update returned %v while b held the write lock", err)
		default:
		}
		let()
		ok(t, <-bDone)
		ok(t, <-aDone)
		for _, h := range []*hc.DB{a, b} {
			for _, k := range []string{"backup:1", "after:1"} {
				if _, err := h.Get(k); err != nil {
					t.Errorf("%s gives %v", k, err)
				}
			}
			for _, k := range []string{"docs:1", "raced:1"} {
				if _, err := h.Get(k); !errors.Is(err, hc.ErrNotFound) {
					t.Errorf("%s, from the old file, gives %v", k, err)
				}
			}
		}
	})

	t.Run("an Update while a read's reload is under way", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.hcx")
		db, trap := openTrapped(t, path)
		ok(t, db.Put("docs:1", nil))
		moveIn(t, path, bigRecords(3))
		started := make(chan struct{})
		var once sync.Once
		trap.setRead(func(p string) error {
			if p == path {
				once.Do(func() { close(started) })
				time.Sleep(30 * time.Millisecond)
			}
			return nil
		})
		defer trap.setRead(nil)
		read := make(chan error, 1)
		go func() {
			_, err := db.Get("big:3")
			read <- err
		}()
		await(t, started, "the reload to begin")
		updated := make(chan error, 1)
		go func() {
			updated <- db.Update(func(tx *hc.Tx) error {
				if _, err := tx.Get("big:3"); err != nil {
					return fmt.Errorf("the Update doesn't see the new file: %w", err)
				}
				return tx.Put("after:1", nil)
			})
		}()
		select {
		case err := <-updated:
			t.Fatalf("the Update returned %v while the reload was under way", err)
		case <-time.After(30 * time.Millisecond):
		}
		trap.setRead(nil)
		ok(t, <-read)
		ok(t, <-updated)
		if _, err := db.Get("after:1"); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAReloadThatFailsPartway: a reload whose read of the new file fails
// once the first of its batches has gone in, and a new file with a batch
// that breaks the rules. Each read gives the error, an Update too, and the
// copy holds what came before the failure, which no read sees: a read that
// finds an Update holding the write lock, before its Lock has read the file
// again, gives the error too. Once the file can be read, the next read
// reloads it and gives what it holds.
func TestAReloadThatFailsPartway(t *testing.T) {
	defer watchdog()()
	t.Run("a read that fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.hcx")
		db, trap := openTrapped(t, path)
		ok(t, db.Put("docs:1", nil))
		moveIn(t, path, bigRecords(3))
		// The reads: the header, the window from the first batch, the first
		// batch with its marker, then the window from the second.
		var mu sync.Mutex
		reads := 0
		trap.setRead(func(p string) error {
			mu.Lock()
			defer mu.Unlock()
			if reads++; reads == 4 {
				return errDisk
			}
			return nil
		})
		_, err := db.Get("big:1")
		wantErr(t, err, errDisk)
		trap.setRead(nil)
		ok(t, hc.ReadTheCopy(db, func(r store.Reader) error {
			if _, err := r.Get("big:1"); err != nil {
				t.Errorf("the copy after the failed reload has big:1 as %v", err)
			}
			for _, k := range []string{"big:2", "docs:1"} {
				if _, err := r.Get(k); !errors.Is(err, hc.ErrNotFound) {
					t.Errorf("the copy after the failed reload has %s, with %v", k, err)
				}
			}
			return nil
		}))

		entered, gate := make(chan struct{}), make(chan struct{})
		var once, opening sync.Once
		letOpen := func() { opening.Do(func() { close(gate) }) }
		defer letOpen() // before the cleanup's Close, which would wait for the Update
		trap.setOpen(func(p string) {
			if p == path {
				once.Do(func() { close(entered); <-gate })
			}
		})
		updated := make(chan error, 1)
		go func() { updated <- db.Update(func(tx *hc.Tx) error { return tx.Put("after:1", nil) }) }()
		await(t, entered, "the Update's Lock to read the file again")
		_, err = db.Get("big:1")
		letOpen()
		ok(t, <-updated)
		if !errors.Is(err, errDisk) {
			t.Errorf("a read while the Update's Lock was about to read the file again gave %v, where the failed reload's error is wanted", err)
		}
		for _, k := range []string{"big:1", "big:3", "after:1"} {
			if _, err := db.Get(k); err != nil {
				t.Errorf("once the file can be read, %s gives %v", k, err)
			}
		}
	})

	t.Run("a batch that breaks the rules", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.hcx")
		db := open(t, path)
		ok(t, db.Put("docs:1", nil))
		other := path + ".other"
		o, err := hc.Open(other)
		ok(t, err)
		ok(t, o.Put("good:1", nil))
		ok(t, o.Put("good:2", nil))
		ok(t, o.Close())
		writeBatches(t, other, []format.Change{{Op: format.Put, Key: "nosuch:1", Fields: []format.Field{{Name: "n", Value: value.Int(3)}}}})
		ok(t, os.Rename(other, path))
		isDamage := func(what string, err error) {
			t.Helper()
			var d *hc.Damage
			if !errors.Is(err, hc.ErrDamaged) || !errors.As(err, &d) || d.Batch != 3 {
				t.Fatalf("%s gave %v, where damage naming batch 3 is wanted", what, err)
			}
		}
		_, err = db.Get("good:1")
		isDamage("a read", err)
		_, err = db.Check()
		isDamage("Check", err)
		isDamage("an Update", db.Update(func(tx *hc.Tx) error { return tx.Put("docs:2", nil) }))
		ok(t, hc.ReadTheCopy(db, func(r store.Reader) error {
			for _, k := range []string{"good:1", "good:2"} {
				if _, err := r.Get(k); err != nil {
					t.Errorf("the copy after the failed reload has %s as %v", k, err)
				}
			}
			return nil
		}))
		moveIn(t, path, func(o *hc.DB) { ok(t, o.Put("fixed:1", nil)) })
		if _, err := db.Get("fixed:1"); err != nil {
			t.Fatalf("once a sound file is at the path, fixed:1 gives %v", err)
		}
	})
}

// heapNow is the bytes the heap holds now, after a garbage collection when
// collect is true.
func heapNow(collect bool) uint64 {
	if collect {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestAReloadDoesntHoldTwoCopies measures, roughly, a reload's memory: a
// database of 4,000 records with 384-value vectors, with the same commits in
// another file moved into place, as a backup put back. With the garbage
// collector's own runs switched off, so that only the reload's collection
// counts, the heap is measured without a collection as the reload reads the
// new file's header, before the first batch goes in: the old copy has gone
// by then, so the heap never holds the two at once. Once collected, the new
// copy is as large as the old one.
func TestAReloadDoesntHoldTwoCopies(t *testing.T) {
	defer watchdog()()
	records := 4000
	if testing.Short() {
		records = 2000
	}
	path := filepath.Join(t.TempDir(), "test.hcx")
	db, trap := openTrapped(t, path)
	for i := 0; i < records; i += 500 {
		ok(t, db.Update(func(tx *hc.Tx) error {
			vec := make(hc.Vector, 384)
			for j := i; j < i+500 && j < records; j++ {
				for k := range vec {
					vec[k] = float32(j%97+k) + 0.5
				}
				if err := tx.Put(fmt.Sprintf("docs:%06d", j), hc.Fields{"vec": vec}); err != nil {
					return err
				}
			}
			return nil
		}))
	}
	ok(t, db.Close())
	base := heapNow(true)
	db, trap = openTrapped(t, path)
	one := heapNow(true) - base // a copy, with its Log
	backUp(t, path)
	var first uint64
	trap.setRead(func(p string) error {
		if first == 0 {
			first = heapNow(false)
		}
		return nil
	})
	old := debug.SetGCPercent(-1)
	_, err := db.Get("docs:000001")
	debug.SetGCPercent(old)
	trap.setRead(nil)
	ok(t, err)
	live := heapNow(true) - base
	t.Logf("a copy of %d records takes %.1f MB, with its Log; as the reload read the new file's header, the heap held %.1f MB past what it holds without one, and the new copy takes %.1f MB",
		records, float64(one)/1e6, (float64(first)-float64(base))/1e6, float64(live)/1e6)
	if one < 2e6 {
		t.Fatalf("a copy takes %d bytes, too few to measure by", one)
	}
	if first == 0 || first > base+one/2 {
		t.Errorf("as the reload read the new file's header, the heap held %d bytes past what it holds without a copy, where a copy takes %d: the old copy hadn't gone", first-base, one)
	}
	if live < one*3/4 || live > one*5/4 {
		t.Errorf("after the reload, the new copy takes %d bytes, where the old one took %d", live, one)
	}
}
