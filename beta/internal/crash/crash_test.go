// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package crash

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// TestTheToyLogPassesEveryPoint is the half of T3's closing test where the
// toy log has no bug. With the power cut in every write and after the last
// call, with every call failing, and with a copy taken from every call on,
// each from several seeds, the toy log always opens at the last commit
// that succeeded or the one under way, and both of those come up. It does
// so as a new log, and opened again after Setup's commits, where the cut
// of the end of the log adds a Truncate to cut in, and with copies read in
// small chunks over several calls. It does so too when the log is
// compacted midway and the new file renamed over it, where a copy that
// opened the old file reads it to its end, and when every call fails from
// the one picked on, as on a disk that has died.
//
// In a build with the hypercrux_planted tag and
// HYPERCRUX_PLANT=crash/marker-before-sync, the toy logs here have the bug,
// and the test fails.
func TestTheToyLogPassesEveryPoint(t *testing.T) {
	commits, seeds := 16, 12
	if testing.Short() {
		commits, seeds = 8, 3
	}
	cases := []struct {
		name string
		w    Workload
		o    Options
	}{
		{"a new log", toyWorkload(commits, plant), Options{Seeds: seeds}},
		{"after Setup", toySetupWorkload(commits/2, commits/2, plant), Options{Seeds: seeds, Seed: 100}},
		{"copies in chunks", toyWorkload(commits, plant), Options{Kinds: Copies, Seeds: 2, Chunk: 100}},
		{"compacted midway", toyCompactWorkload(commits/2, plant), Options{Seeds: seeds, Seed: 200, Chunk: 256}},
		{"on a disk that dies", toyWorkload(commits, plant), Options{Kinds: Failures, Seeds: seeds, Seed: 300, Times: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, err := Run(c.w, c.o)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(rep)
			if rep.Last == 0 || rep.Later == 0 {
				t.Errorf("of the opens, %d found the last commit that succeeded and %d a later one, where both should come up", rep.Last, rep.Later)
			}
			if c.o.Kinds == 0 && (rep.Cuts == 0 || rep.Failures == 0 || rep.Copies == 0) {
				t.Errorf("a kind of point never ran: %v", rep)
			}
		})
	}
}

// TestTheDriverCatchesMarkerBeforeSync is the half of T3's closing test
// where the toy log writes each batch's marker before the batch's sync.
// The driver catches it with cuts alone and with failures alone, and the
// failure it reports replays the same way from its point alone.
//
// A cut in a marker's write catches it from 2 to 6 per cent of seeds, and
// mostly the same seeds at every write, since the disk draws nothing from
// its seed before the cut. So the cuts get 256 seeds. A failed sync catches
// it from about 80 per cent. The driver stops at the first catch either way.
func TestTheDriverCatchesMarkerBeforeSync(t *testing.T) {
	w := toyWorkload(16, markerBeforeSync)
	cases := []struct {
		kinds   Kind
		seeds   int
		problem Problem
	}{
		// A cut before the sync can keep the marker and lose or tear the
		// batch, which leaves a whole marker past the end of the log.
		{Cuts, 256, NotOpened},
		// A failed sync can leave the batch and its marker in the page
		// cache with neither on the drive: a reader finds the commit, and
		// the next cut takes it away.
		{Failures, 16, Lost},
	}
	for _, c := range cases {
		_, err := Run(w, Options{Kinds: c.kinds, Seeds: c.seeds})
		f := mustFail(t, err, c.kinds, c.problem)
		if c.kinds == Cuts && (f.Point.Op != fault.WriteAt || f.Point.N%2 != 0) {
			t.Errorf("the cut that caught it came in %v call %d, where a marker's write is an even-numbered WriteAt", f.Point.Op, f.Point.N)
		}
		_, again := Run(w, Options{Replay: f.Point.String()})
		if again == nil || again.Error() != err.Error() {
			t.Errorf("replayed alone, %v gives\n  %v\nwhere it gave\n  %v", f.Point, again, err)
		}
	}
}

// TestTheDriverCatchesASkippedSync: a toy log that never syncs can lose a
// commit that succeeded at a cut, and the model catches that.
func TestTheDriverCatchesASkippedSync(t *testing.T) {
	_, err := Run(toyWorkload(8, noSync), Options{Kinds: Cuts, Seeds: 8})
	mustFail(t, err, Cuts, NotAllowed)
}

// TestACopyCatchesAMarkerAheadOfItsBatch: a toy log that writes each marker
// before its batch leaves, between the two writes, a whole marker past the
// end of the log, and a copy taken there opens as damaged.
func TestACopyCatchesAMarkerAheadOfItsBatch(t *testing.T) {
	_, err := Run(toyWorkload(4, markerFirst), Options{Kinds: Copies})
	mustFail(t, err, Copies, NotOpened)
}

// TestTheDriverCatchesAWorkloadThatChanges: a workload that makes one
// commit fewer after its first run doesn't make the calls the driver
// counted, and every kind of point says so.
func TestTheDriverCatchesAWorkloadThatChanges(t *testing.T) {
	for _, kinds := range []Kind{Cuts, Failures, Copies} {
		runs := 0
		w := toyWorkload(4, "")
		w.Run = func(sys fsys.FS, m *Model) error {
			runs++
			if runs > 1 {
				return toyWorkload(3, "").Run(sys, m)
			}
			return toyWorkload(4, "").Run(sys, m)
		}
		_, err := Run(w, Options{Kinds: kinds})
		mustFail(t, err, kinds, Diverged)
	}

	// After its first run, this one makes a Stat where it made its second
	// write, so it makes as many calls as before, and only the cut that was
	// to come in that write can tell.
	runs := 0
	w := Workload{
		Path: toyPath,
		Run: func(sys fsys.FS, m *Model) error {
			runs++
			f, err := sys.Create(toyPath, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := f.WriteAt([]byte("a"), 0); err != nil {
				return err
			}
			if runs > 1 {
				_, err = sys.Stat(toyPath)
				return err
			}
			_, err = f.WriteAt([]byte("b"), 1)
			return err
		},
		Reopen: func(fsys.FS) (string, error) { return "", nil },
	}
	_, err := Run(w, Options{Kinds: Cuts})
	if f := mustFail(t, err, Cuts, Diverged); f.Point.String() != "cut WriteAt 2 seed 0" {
		t.Errorf("the change was found at %v, where it's in WriteAt 2", f.Point)
	}
}

// TestTheDriverCatchesAWorkloadThatFails: a workload that fails with no
// fault can't be driven.
func TestTheDriverCatchesAWorkloadThatFails(t *testing.T) {
	w := toyWorkload(2, "")
	w.Run = func(fsys.FS, *Model) error { return errors.New("broken") }
	_, err := Run(w, Options{Kinds: Copies})
	mustFail(t, err, Cuts, RunFailed)
}

// TestAProcessEndsWithItsFiles: the driver ends the workload's process by
// closing the files it left open, so the flock it held goes, and a process
// that opens the database afterwards can take it.
func TestAProcessEndsWithItsFiles(t *testing.T) {
	w := Workload{
		Path: toyPath,
		Run: func(sys fsys.FS, m *Model) error {
			f, err := sys.Create(toyPath, 0o644)
			if err != nil {
				return err
			}
			if ok, err := f.TryLock(); err != nil || !ok {
				return fmt.Errorf("TryLock gives %v and %v", ok, err)
			}
			return nil // with the file left open and locked
		},
		Reopen: func(sys fsys.FS) (string, error) {
			f, err := sys.Open(toyPath)
			if errors.Is(err, fs.ErrNotExist) {
				return "", nil
			}
			if err != nil {
				return "", err
			}
			defer f.Close()
			if ok, err := f.TryLock(); err != nil || !ok {
				return "", fmt.Errorf("the flock is still held: TryLock gives %v and %v", ok, err)
			}
			return "", nil
		},
	}
	if _, err := Run(w, Options{Kinds: Cuts | Failures}); err != nil {
		t.Fatal(err)
	}
}

// TestACopyReadsAChunkBeforeEachCall: a copy reads its file from the
// start, a chunk just before each of the workload's calls from the one it
// starts at, and the rest once the workload has ended. So it can hold
// bytes from different moments, as a copy by cp can.
func TestACopyReadsAChunkBeforeEachCall(t *testing.T) {
	d := fault.New(1)
	if err := d.Mkdir("/db"); err != nil {
		t.Fatal(err)
	}
	m := &Model{states: []string{""}}
	c := &copier{sys: d.FS(), path: "/db/f", at: 3, buf: make([]byte, 4), m: m}
	p := newProc(d.FS())
	p.copy = c
	f, err := p.Create("/db/f", 0o644) // call 1
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		s   string
		off int64
	}{
		{"aaaabbbb", 0}, // call 2
		{"cccc", 8},     // call 3, with the copy reading aaaa just before it
		{"dddd", 0},     // call 4, with the copy reading bbbb just before it
	} {
		if _, err := f.WriteAt([]byte(w.s), w.off); err != nil {
			t.Fatal(err)
		}
	}
	c.finish(p.calls) // cccc, then a short read
	if string(c.data) != "aaaabbbbcccc" || c.stage != copied || c.end != 5 || c.err != nil {
		t.Errorf("the copy holds %q, at stage %d, ending at call %d, with %v", c.data, c.stage, c.end, c.err)
	}

	// A copy that's to start before the file exists finds nothing to copy.
	c = &copier{sys: d.FS(), path: "/db/g", at: 1, buf: make([]byte, 4), m: m}
	c.before(1)
	if c.stage != noFile {
		t.Errorf("a copy of a file that isn't there is at stage %d", c.stage)
	}
}

// TestPointsReadBack: every kind of point reads back from what String
// writes, and a string that isn't a point is refused.
func TestPointsReadBack(t *testing.T) {
	for _, p := range []Point{
		{Kind: Cuts, Op: fault.Any, Seed: 3},
		{Kind: Cuts, Op: fault.WriteAt, N: 5, Seed: 3},
		{Kind: Cuts, Op: fault.SyncDir, N: 1, Seed: 1 << 40},
		{Kind: Cuts, Op: fault.Any, N: 2},
		{Kind: Failures, Op: fault.Any, N: 12},
		{Kind: Copies, Op: fault.Any, N: 7, Seed: 9},
	} {
		if got, err := ParsePoint(p.String()); err != nil || got != p {
			t.Errorf("%q reads back as %+v, with %v, where %+v is wanted", p.String(), got, err, p)
		}
	}
	for _, s := range []string{
		"", "cut", "cut end", "cut WriteAt 5", "cut Write 5 seed 1", "cut WriteAt 0 seed 1",
		"fail x seed 1", "copy 7 seed -1", "fail 1 2 seed 3", "lose 1 seed 2", "copy 3 grain 1",
	} {
		if p, err := ParsePoint(s); err == nil {
			t.Errorf("%q reads as the point %+v", s, p)
		}
	}
}

// TestTheEnvironmentSetsSeedsAndReplays: HYPERCRUX_CRASH_SEEDS sets how
// many seeds each point runs with, for long runs, and HYPERCRUX_CRASH_POINT
// runs one point alone.
func TestTheEnvironmentSetsSeedsAndReplays(t *testing.T) {
	w := toyWorkload(2, "")
	t.Setenv("HYPERCRUX_CRASH_SEEDS", "3")
	rep, err := Run(w, Options{Kinds: Cuts, Seeds: 1})
	if err != nil {
		t.Fatal(err)
	}
	// A cut after the last call, in the Create, in the SyncDir and in each
	// of the 4 writes, from 3 seeds.
	if rep.Seeds != 3 || rep.Cuts != 21 {
		t.Errorf("with HYPERCRUX_CRASH_SEEDS=3: %v", rep)
	}
	t.Setenv("HYPERCRUX_CRASH_POINT", "cut WriteAt 2 seed 1")
	rep, err = Run(w, Options{})
	if err != nil || rep.Seeds != 1 || rep.Cuts != 1 || rep.Failures != 0 || rep.Copies != 0 {
		t.Errorf("with HYPERCRUX_CRASH_POINT set: %v, %v", rep, err)
	}
	t.Setenv("HYPERCRUX_CRASH_POINT", "")
	t.Setenv("HYPERCRUX_CRASH_SEEDS", "some")
	if _, err := Run(w, Options{}); err == nil {
		t.Error("HYPERCRUX_CRASH_SEEDS=some is taken")
	}
}

// mustFail checks that err is a Failure at a point of the kind kind, with
// the problem problem, and returns it.
func mustFail(t *testing.T, err error, kind Kind, problem Problem) *Failure {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("the driver found nothing wrong: %v", err)
	}
	if f.Point.Kind != kind || f.Problem != problem {
		t.Fatalf("the driver found %v with %v, where %v with %v is wanted: %v", f.Problem, f.Point.Kind, problem, kind, err)
	}
	t.Log(err)
	return f
}
