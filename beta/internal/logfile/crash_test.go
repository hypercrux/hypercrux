// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/crash"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The real log driven by T3's crash-point driver, on the fault layer's
// disk. Each workload makes its commits as the public package will: m.Begin
// with the state the commit gives, Lock, Append, then m.Done once Append has
// returned nil, then Unlock. Reopen opens the database as a process started
// after a crash, which checks the end of the log when it gets the lock, and
// a state is each batch the Target was handed, a line each.

// crashPath is where the crash tests' database is.
const crashPath = "/db/crash.hcx"

// crashOptions fixes the database ID, so a workload's file holds the same
// bytes in every run and a crash point replays from its seed.
var crashOptions = Options{ID: [16]byte{'c', 'r', 'a', 's', 'h', ' ', 't', 'e', 's', 't', 's'}}

// crashBatch is commit i's batch: a put of the record t:i with a text field
// of 1 to 1,300 letters, as many as i picks, so that batches and their
// markers fall across sectors in many ways, as in the crash package's toy
// log.
func crashBatch(i int) []format.Change {
	text := make([]byte, i*389%1300+1)
	for j := range text {
		text[j] = 'a' + byte((i+j)%26)
	}
	return []format.Change{{Op: format.Put, Key: fmt.Sprintf("t:%d", i), Fields: []format.Field{{Name: "x", Value: value.Text(string(text))}}}}
}

// crashState is the state after commit i, as a recorder writes it.
func crashState(i int) string {
	commits := make([]int, i)
	for j := range commits {
		commits[j] = j + 1
	}
	return stateOf(commits)
}

// stateOf is the state of a file holding the batches of the commits given,
// in order and numbered from 1, as a recorder writes it.
func stateOf(commits []int) string {
	rec := &recorder{}
	for j, c := range commits {
		rec.Apply(uint64(j+1), crashBatch(c))
	}
	return rec.state()
}

// model is what the commits tell a crash.Model, when there's one.
type model interface {
	Begin(state string)
	Done()
}

// crashCommits makes the commits first to last on l, telling m about each
// one when m isn't nil.
func crashCommits(l *Log, m model, first, last int) error {
	for i := first; i <= last; i++ {
		if m != nil {
			m.Begin(crashState(i))
		}
		if err := l.Lock(); err != nil {
			return err
		}
		if err := l.Append(crashBatch(i)); err != nil {
			l.Unlock()
			return err
		}
		if m != nil {
			m.Done() // once Append returns, the commit is on the drive
		}
		if err := l.Unlock(); err != nil {
			return err
		}
	}
	return nil
}

// carryOn makes the commits first to last on l, telling m about each one,
// and carries on after a commit that fails, as a program would carry on
// after an Update that returned an error. done is the commits whose batches
// the file holds already, in order. A commit's state holds the batches of
// done and of the commits that succeeded since, then its own, so a failed
// commit is left out of every later state: the log cuts it back out of the
// file before it lets go of the lock, or keeps the lock and fails every
// commit after it.
func carryOn(l *Log, m model, done []int, first, last int) {
	for i := first; i <= last; i++ {
		next := append(slices.Clone(done), i)
		m.Begin(stateOf(next))
		if err := l.Lock(); err != nil {
			continue // nothing was written: a call failed, or the Log is stuck
		}
		if err := l.Append(crashBatch(i)); err != nil {
			l.Unlock()
			continue
		}
		m.Done()
		done = next
		l.Unlock()
	}
}

// crashOpen opens the database at crashPath through sys, as a process
// started after a crash would, and returns the state it reads, once the
// check of the end of the log has run.
func crashOpen(sys fsys.FS) (string, error) {
	rec := &recorder{}
	l, err := Open(sys, crashPath, rec, crashOptions)
	if err != nil {
		return "", err
	}
	return rec.state(), l.Close()
}

// runCommits is a workload's Run: it opens the database and makes the
// commits first to last, where done is the commits whose batches the file
// holds already. Without carry, it stops at its first error, as a crash
// would stop it. With carry, it carries on after a failed commit (carryOn),
// and opens the database a second time when the first Open fails, since a
// failure in Open's check of the end of the log leaves the end for the next
// check.
func runCommits(done []int, first, last int, carry bool) func(fsys.FS, *crash.Model) error {
	return func(sys fsys.FS, m *crash.Model) error {
		l, err := Open(sys, crashPath, &recorder{}, crashOptions)
		if err != nil && carry {
			l, err = Open(sys, crashPath, &recorder{}, crashOptions)
		}
		if err != nil {
			return err
		}
		defer l.Close()
		if carry {
			carryOn(l, m, done, first, last)
			return nil
		}
		return crashCommits(l, m, first, last)
	}
}

// newWorkload creates a database and makes commits commits on it, carrying
// on after a failed one when carry is set.
func newWorkload(commits int, carry bool) crash.Workload {
	return crash.Workload{
		Path:   crashPath,
		Run:    runCommits(nil, 1, commits, carry),
		Reopen: crashOpen,
	}
}

// The ends Setup can leave for againWorkload, as a writer that died there
// would leave them, synced. The check finds them when Run opens the
// database.
const (
	whole      = ""                                // nothing past the end of the log
	noMarker   = "a batch left without its marker" // a batch that counts, and nothing after it
	tornMarker = "a torn marker, then zeros"       // a batch that counts, a third of its marker, then zeros
	cutShort   = "a batch cut short"               // half a batch
	zeros      = "zeros"                           // 1,000 zero bytes
)

// againWorkload is setup commits made by Setup on a new database, with the
// end of the log left as end says, then commits more on the same database,
// opened again for them, carrying on after a failed one when carry is set.
// When end leaves a batch that counts, it's commit setup+1, and Setup's
// state holds it, since it's on the drive and the check of the end of the
// log marks it.
func againWorkload(setup, commits int, end string, carry bool) crash.Workload {
	after := setup
	if end == noMarker || end == tornMarker {
		after++
	}
	done := make([]int, after)
	for j := range done {
		done[j] = j + 1
	}
	return crash.Workload{
		Path: crashPath,
		Setup: func(sys fsys.FS) (string, error) {
			l, err := Open(sys, crashPath, &recorder{}, crashOptions)
			if err != nil {
				return "", err
			}
			if err := crashCommits(l, nil, 1, setup); err != nil {
				l.Close()
				return "", err
			}
			at, hdr := l.end, l.hdr
			if err := l.Close(); err != nil {
				return "", err
			}
			b, sum, err := format.AppendBatch(nil, hdr.Gen, uint64(setup+1), crashBatch(setup+1))
			if err != nil {
				return "", err
			}
			switch end {
			case tornMarker:
				m := format.AppendMarker(nil, hdr.ID, hdr.Gen, format.Marker{Seq: uint64(setup + 1), Sum: sum})
				b = append(append(b, m[:7]...), make([]byte, 700)...)
			case cutShort:
				b = b[:len(b)/2]
			case zeros:
				b = make([]byte, 1000)
			case whole:
				b = nil
			}
			f, err := sys.Open(crashPath)
			if err != nil {
				return "", err
			}
			defer f.Close()
			if _, err := f.WriteAt(b, at); err != nil {
				return "", err
			}
			return crashState(after), f.Sync() // so no cut can take what Setup's state holds
		},
		Run:    runCommits(done, after+1, after+commits, carry),
		Reopen: crashOpen,
	}
}

// TestTheRealLogPassesACutAtEveryWrite is F3's closing test: T3's driver on
// the real log, with the power cut in every write, in every call that
// changes names, and after the last call, each from many seeds. After every
// cut the database opens at the last commit that succeeded or the one under
// way, and after a second cut it opens there or later, so nothing a reader
// saw is lost. It does so for a new database, created by the workload, and
// for one opened again after Setup's commits. Then the same with an end
// that a writer left when it died, which the workload's own Open checks,
// so the cuts come in the check's writes and its cut too.
//
// A new database gets 128 seeds. A cut finds logfile/marker-before-sync in
// a few per cent of seeds (T3.md): the bug can leave a whole marker after a
// batch a cut tore, which the look past the end of the log reports as
// damage, so the database doesn't open. A new database first shows it at
// cut WriteAt 6 seed 22, and four of the other five workloads within their
// first seven seeds. Without the bug, no cut leaves anything the look past
// reports.
func TestTheRealLogPassesACutAtEveryWrite(t *testing.T) {
	t.Parallel()
	commits, seeds, others := 8, 128, 32
	if testing.Short() {
		commits, seeds, others = 6, 8, 4
	}
	cases := []struct {
		name  string
		w     crash.Workload
		seeds int
	}{
		{"a new database", newWorkload(commits, false), seeds},
		{"opened again", againWorkload(commits/2, commits/2, whole, false), others},
		{noMarker, againWorkload(commits/2, commits/2, noMarker, false), others},
		{tornMarker, againWorkload(commits/2, commits/2, tornMarker, false), others},
		{cutShort, againWorkload(commits/2, commits/2, cutShort, false), others},
		{zeros, againWorkload(commits/2, commits/2, zeros, false), others},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, err := crash.Run(c.w, crash.Options{Kinds: crash.Cuts, Seeds: c.seeds, Seed: uint64(1000 * i)})
			if err != nil {
				t.Fatal(err)
			}
			t.Log(rep)
			if rep.Last == 0 || rep.Later == 0 {
				t.Errorf("of the opens, %d found the last commit that succeeded and %d a later one, where both should come up", rep.Last, rep.Later)
			}
		})
	}
}

// TestTheRealLogAfterAFailedCall is F5's closing test: T3's driver on the
// real log, with a failure at every call, in workloads that carry on after
// a failed commit, as a program would after Update returned an error, and
// give each later commit the state it has without the failed one. Once the
// workload's process has ended, the database opens, as a process started
// after it, at the last commit that succeeded or one begun after it, and
// after a power cut it opens there or later, so nothing a reader saw is
// lost.
//
//   - With one call failing, every failed commit is cut back out of the
//     file before the workload goes on, so every open finds the last commit
//     that succeeded, and none finds a failed one, before the power cut or
//     after it. A failure in the check of the end of the log, when Open
//     marks a batch that Setup left without its marker, leaves the batch
//     for the next Open, which marks it.
//   - On a disk that dies, every call fails from the one picked on. When
//     that's in a commit, its cut back fails too, and the Log is stuck, so
//     every commit after it fails at Lock. The process that opens the
//     database next checks the end of the log the stuck Log left, and marks
//     its failed batch when the batch counts, writing it again first, with
//     the marker before it, since after a failed sync their pages can be
//     marked clean without reaching the drive while reads go on seeing them.
//     So both kinds of open come up there.
func TestTheRealLogAfterAFailedCall(t *testing.T) {
	t.Parallel()
	commits, seeds := 8, 24
	if testing.Short() {
		commits, seeds = 6, 4
	}
	cases := []struct {
		name  string
		w     crash.Workload
		times int
	}{
		{"a new database", newWorkload(commits, true), 0},
		{noMarker, againWorkload(commits/2, commits/2, noMarker, true), 0},
		{tornMarker, againWorkload(commits/2, commits/2, tornMarker, true), 0},
		{"a new database on a disk that dies", newWorkload(commits, true), -1},
		{tornMarker + ", on a disk that dies", againWorkload(commits/2, commits/2, tornMarker, true), -1},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, err := crash.Run(c.w, crash.Options{Kinds: crash.Failures, Seeds: seeds, Seed: uint64(1000 * i), Times: c.times})
			if err != nil {
				t.Fatal(err)
			}
			t.Log(rep)
			switch {
			case rep.Last == 0:
				t.Errorf("no open found the last commit that succeeded")
			case c.times == 0 && rep.Later != 0:
				t.Errorf("%d opens found a commit begun after the last that succeeded, where every failed commit is cut back out of the file", rep.Later)
			case c.times != 0 && rep.Later == 0:
				t.Errorf("no open found a commit begun after the last that succeeded, where a stuck Log leaves its failed batch, which the next check marks when it counts")
			}
		})
	}
}

// TestCopiesOfTheRealLog: a copy taken mid-commit, as cp would take it,
// opens at a state the model allows, from the database's creation on, and
// its own check of the end of the log recovers whatever commit the copy
// caught half done. T3 wrote it, in the crash package's tests.
func TestCopiesOfTheRealLog(t *testing.T) {
	for _, chunk := range []int{0, 100} {
		rep, err := crash.Run(newWorkload(5, false), crash.Options{Kinds: crash.Copies, Seeds: 2, Chunk: chunk})
		if err != nil {
			t.Fatal(err)
		}
		t.Log(rep)
		if rep.Copies == 0 || rep.NoFile == 0 {
			t.Errorf("%d copies, and %d points with no file to copy, where the database is created by the workload", rep.Copies, rep.NoFile)
		}
	}
}

// TestACrashPointReplays: with Options.ID fixed, a workload run twice on new
// disks with the same seed and the power cut in the same write leaves the
// same file, byte for byte, so a crash point the driver reports replays.
// With a random ID, the two files differ.
func TestACrashPointReplays(t *testing.T) {
	run := func(seed uint64, n int, o Options) []byte {
		d := fault.New(seed)
		if err := d.Mkdir("/db"); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			d.Add(fault.Rule{Op: fault.WriteAt, N: n, Cut: true})
		}
		if l, err := Open(d.FS(), crashPath, &recorder{}, o); err == nil {
			crashCommits(l, nil, 1, 6) // fails at the cut
			l.Close()
		}
		if d.Cuts() == 0 {
			d.Cut()
		}
		f, err := d.FS().Open(crashPath)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	d := fault.New(0)
	if err := d.Mkdir("/db"); err != nil {
		t.Fatal(err)
	}
	l, err := Open(d.FS(), crashPath, &recorder{}, crashOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := crashCommits(l, nil, 1, 6); err != nil {
		t.Fatal(err)
	}
	l.Close()
	writes := d.Calls(fault.WriteAt, "")
	differ := 0
	for n := 0; n <= writes; n++ {
		for seed := range uint64(4) {
			if a, b := run(seed, n, crashOptions), run(seed, n, crashOptions); !bytes.Equal(a, b) {
				t.Errorf("with the power cut in write %d, seed %d left files of %d and %d bytes that differ", n, seed, len(a), len(b))
			}
			if a, b := run(seed, n, Options{}), run(seed, n, Options{}); !bytes.Equal(a, b) {
				differ++
			}
		}
	}
	if differ == 0 {
		t.Error("with random IDs, every pair of runs left the same file")
	}
}
