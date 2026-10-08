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
	rec := &recorder{}
	for j := 1; j <= i; j++ {
		rec.Apply(uint64(j), crashBatch(j))
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

// newWorkload creates a database and makes commits commits on it.
func newWorkload(commits int) crash.Workload {
	return crash.Workload{
		Path: crashPath,
		Run: func(sys fsys.FS, m *crash.Model) error {
			l, err := Open(sys, crashPath, &recorder{}, crashOptions)
			if err != nil {
				return err
			}
			defer l.Close()
			return crashCommits(l, m, 1, commits)
		},
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
// opened again for them. When end leaves a batch that counts, it's commit
// setup+1, and Setup's state holds it, since it's on the drive and the
// check of the end of the log marks it.
func againWorkload(setup, commits int, end string) crash.Workload {
	after := setup
	if end == noMarker || end == tornMarker {
		after++
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
		Run: func(sys fsys.FS, m *crash.Model) error {
			l, err := Open(sys, crashPath, &recorder{}, crashOptions)
			if err != nil {
				return err
			}
			defer l.Close()
			return crashCommits(l, m, after+1, after+commits)
		},
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
// a few per cent of seeds (T3.md), and only once F4's look past the end of
// the log is in: the bug leaves a whole marker after a torn batch, and until
// F4 the check cuts both, which the model allows for a commit under way.
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
		{"a new database", newWorkload(commits), seeds},
		{"opened again", againWorkload(commits/2, commits/2, whole), others},
		{noMarker, againWorkload(commits/2, commits/2, noMarker), others},
		{tornMarker, againWorkload(commits/2, commits/2, tornMarker), others},
		{cutShort, againWorkload(commits/2, commits/2, cutShort), others},
		{zeros, againWorkload(commits/2, commits/2, zeros), others},
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

// TestTheRealLogAfterAFailedCall drives the real log with a failure at every
// call, in workloads that stop at their first error, as the public package
// would hand the error back. The database then opens, as a process started
// once the workload's had ended, at the last commit that succeeded or the
// one that failed, and after a cut it opens there or later.
//
// That's the case FORMAT.md writes a batch again for. After a failed sync,
// the batch's pages and the marker before it can be marked clean without
// reaching the drive, while reads go on seeing them, so the opening that
// finds the batch whole has to write both again before its sync, or the cut
// takes them, and with them a commit the opening handed over. F5 adds a
// workload that carries on after a failed commit, and a disk that dies.
func TestTheRealLogAfterAFailedCall(t *testing.T) {
	t.Parallel()
	commits, seeds := 8, 24
	if testing.Short() {
		commits, seeds = 6, 4
	}
	cases := []struct {
		name string
		w    crash.Workload
	}{
		{"a new database", newWorkload(commits)},
		{tornMarker, againWorkload(commits/2, commits/2, tornMarker)},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, err := crash.Run(c.w, crash.Options{Kinds: crash.Failures, Seeds: seeds, Seed: uint64(1000 * i)})
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

// TestCopiesOfTheRealLog: a copy taken mid-commit, as cp would take it,
// opens at a state the model allows, from the database's creation on, and
// its own check of the end of the log recovers whatever commit the copy
// caught half done. T3 wrote it, in the crash package's tests.
func TestCopiesOfTheRealLog(t *testing.T) {
	for _, chunk := range []int{0, 100} {
		rep, err := crash.Run(newWorkload(5), crash.Options{Kinds: crash.Copies, Seeds: 2, Chunk: chunk})
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
