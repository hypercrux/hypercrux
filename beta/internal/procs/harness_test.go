// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The workloads that show the harness dealing with children that go
// wrong in ways of their own. Each is the toy log with one thing changed.
var (
	failingWriter = Workload{Name: "a writer that fails", Read: toyRead, Final: toyFinal,
		Write: func(w *Writer) error {
			w.Begin("one commit")
			return errors.New("the drive is on fire")
		}}
	panickingWriter = Workload{Name: "a writer that panics", Read: toyRead, Final: toyFinal,
		Write: func(*Writer) error { panic("a writer gives up") }}
	exitingWriter = Workload{Name: "a writer that ends its own process", Read: toyRead, Final: toyFinal,
		Write: func(*Writer) error { os.Exit(0); return nil }}
	skippingReader = Workload{Name: "a reader that skips a commit", Write: func(w *Writer) error { return toyWrite(w, "") }, Final: toyFinal,
		Read: func(r *Reader) error { return toyReadInto(r.Path(), skipThird{r}) }}
	stuckReader = Workload{Name: "a reader that stops", Write: func(w *Writer) error { return toyWrite(w, "") }, Final: toyFinal,
		Read: func(r *Reader) error { return toyReadInto(r.Path(), stopAfter(r, 3)) }}
	pipeHolder = Workload{Name: "a writer that hands its reports' pipe on", Read: toyRead, Final: toyFinal,
		Write: func(w *Writer) error {
			// The sleep holds the pipe open for a while after the writer
			// has been killed, as a process the work started might.
			cmd := exec.Command("sleep", "3")
			cmd.ExtraFiles = []*os.File{os.NewFile(reportFD, "procs reports")}
			if err := cmd.Start(); err != nil {
				return err
			}
			return toyWrite(w, "")
		}}
	racyWriter = Workload{Name: "a writer with a data race", Read: toyRead, Final: toyFinal,
		Write: func(w *Writer) error {
			n, done := 0, make(chan bool)
			go func() { n++; done <- true }()
			n++
			<-done
			return toyWrite(w, "")
		}}
	quittingWriter = Workload{Name: "a writer that stops by itself", Read: toyRead, Final: toyFinal,
		Write: func(w *Writer) error { return toyWriteSome(w, 1+w.Rand().IntN(20)) }}
	idleWriter = Workload{Name: "a writer that never commits", Read: toyRead, Final: toyFinal,
		Write: func(w *Writer) error {
			w.Begin("a commit that never ends " + strconv.Itoa(w.ID()))
			for {
				time.Sleep(time.Hour)
			}
		}}
	countingReader = Workload{Name: "a reader that counts", Write: func(w *Writer) error { return toyWrite(w, "") }, Final: toyFinal,
		Read: func(r *Reader) error {
			return toyReadInto(r.Path(), applyFunc(func(seq uint64, commit string) {
				r.Apply(seq, commit)
				if seq%20 == 0 {
					r.Count("twentieth")
				}
			}))
		}}
)

func TestOptionsThatCantBeUsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toy")
	for _, c := range []struct {
		name string
		w    Workload
		o    Options
	}{
		{"no Final", Workload{Name: "x", Write: toy.Write, Read: toy.Read}, Options{Path: path}},
		{"no name", Workload{Write: toy.Write, Read: toy.Read, Final: toy.Final}, Options{Path: path}},
		{"a relative path", toy, Options{Path: "toy"}},
		{"writers below 0", toy, Options{Path: path, Writers: -1}},
		{"a life that ends before it starts", toy, Options{Path: path, ReaderLife: Span{Min: time.Second, Max: time.Millisecond}}},
		{"a life of nothing", toy, Options{Path: path, WriterLife: Span{Min: 0, Max: 0}, ReaderLife: Span{Min: -time.Second}}},
		{"a gap that ends before it starts", toy, Options{Path: path, WriterGap: Span{Min: time.Second, Max: time.Millisecond}}},
		{"a least count below 0", toy, Options{Path: path, Least: map[string]int{"checks": -1}}},
		{"a least count without a name", toy, Options{Path: path, Least: map[string]int{"": 1}}},
	} {
		if _, err := Run(c.w, c.o); err == nil || errors.As(err, new(*Failure)) {
			t.Errorf("%s: Run gives %v", c.name, err)
		}
	}
	t.Setenv(envTime, "a while")
	if _, err := Run(toy, Options{Path: path}); err == nil || !strings.Contains(err.Error(), envTime) {
		t.Errorf("with %s=a while, Run gives %v", envTime, err)
	}
	t.Setenv(envTime, "")
	t.Setenv(envSeed, "-3")
	if _, err := Run(toy, Options{Path: path}); err == nil || !strings.Contains(err.Error(), envSeed) {
		t.Errorf("with %s=-3, Run gives %v", envSeed, err)
	}
}

// TestTheSeedComesFromTheEnvironment: HYPERCRUX_PROCS_SEED sets the seed
// of a run whose Options don't, and HYPERCRUX_PROCS_TIME its time.
func TestTheSeedComesFromTheEnvironment(t *testing.T) {
	t.Setenv(envSeed, "77")
	t.Setenv(envTime, "1ms")
	rep, err := Run(toy, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Seed != 77 {
		t.Errorf("the run's seed was %d", rep.Seed)
	}
	t.Log(rep)
}

// TestARunCanEndAfterACountOfKills: with Kills set, a run ends once that
// many processes have been killed at the end of their lives, and it has
// done the least it has to, long before its Time is up.
func TestARunCanEndAfterACountOfKills(t *testing.T) {
	rep, err := Run(toy, Options{Path: filepath.Join(t.TempDir(), "toy"), Time: time.Hour, Kills: 20})
	if err != nil {
		t.Fatal(err)
	}
	// Every process running at the end is killed too: the writers, and then
	// the readers once they've caught up.
	if rep.WritersKilled+rep.ReadersKilled < 20 || rep.Took > time.Minute {
		t.Errorf("a run to 20 kills: %v", rep)
	}
	t.Log(rep)
}

// TestAChildThatFailsFailsTheRun: a writer whose work returns an error,
// one that panics, and one that ends its own process, each fail the run,
// with what they said.
func TestAChildThatFailsFailsTheRun(t *testing.T) {
	_, err := Run(failingWriter, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if f := mustFail(t, err, Failed); !strings.Contains(f.Reason, "failed: the drive is on fire") {
		t.Errorf("the reason is %q", f.Reason)
	}
	_, err = Run(panickingWriter, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if f := mustFail(t, err, Failed); !strings.Contains(f.Reason, "ended by itself") || !strings.Contains(f.Reason, "panic: a writer gives up") {
		t.Errorf("the reason is %q", f.Reason)
	}
	_, err = Run(exitingWriter, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if f := mustFail(t, err, Failed); !strings.Contains(f.Reason, "ended by itself, with exit status 0") {
		t.Errorf("the reason is %q", f.Reason)
	}
}

// TestARaceInAChildFailsTheRun: in a race detector's build, a writer with
// a data race ends at the race, with the race's report, and the run fails
// with it, where a kill would have hidden it in the writer's output.
func TestARaceInAChildFailsTheRun(t *testing.T) {
	if !raceBuild {
		t.Skip("the race detector is in -race builds only")
	}
	_, err := Run(racyWriter, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if f := mustFail(t, err, Failed); !strings.Contains(f.Reason, "ended by itself, with exit status 66") || !strings.Contains(f.Reason, "WARNING: DATA RACE") {
		t.Errorf("the reason is %q", f.Reason)
	}
}

// TestChildrenGetGORACE: a child's GORACE has the race detector end it at
// the first race, and at once when it ends, unless the test's own GORACE
// says otherwise.
func TestChildrenGetGORACE(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "halt_on_error=1 atexit_sleep_ms=0"},
		{"log_path=/tmp/race", "log_path=/tmp/race halt_on_error=1 atexit_sleep_ms=0"},
		{"halt_on_error=0", "halt_on_error=0 atexit_sleep_ms=0"},
		{"atexit_sleep_ms=50 halt_on_error=0", "atexit_sleep_ms=50 halt_on_error=0"},
	} {
		if got := childRace(c.in); got != c.want {
			t.Errorf("GORACE=%q gives a child GORACE=%q, where %q is wanted", c.in, got, c.want)
		}
	}
}

// TestAChildWhoseWorkReturnsIsReplaced: writers whose work returns nil
// after 1 to 20 commits, in lives of 5 to 40 milliseconds, often end by
// themselves before their kill, and new ones start in their place, as
// after a kill. The run passes.
func TestAChildWhoseWorkReturnsIsReplaced(t *testing.T) {
	rep, err := Run(quittingWriter, Options{Path: filepath.Join(t.TempDir(), "toy"), Time: 300 * time.Millisecond,
		WriterLife: Span{Min: 5 * time.Millisecond, Max: 40 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.WritersKilled >= rep.Writers || rep.WritersKilled == 0 {
		t.Errorf("of %d writers, %d were killed, where some should have ended by themselves first", rep.Writers, rep.WritersKilled)
	}
	t.Log(rep)
}

// TestAWorkloadMainWasntGiven: a run whose workload TestMain didn't hand
// to Main fails, and says so.
func TestAWorkloadMainWasntGiven(t *testing.T) {
	w := toy
	w.Name = "a workload TestMain doesn't know"
	_, err := Run(w, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if f := mustFail(t, err, Failed); !strings.Contains(f.Reason, `Main wasn't given a workload named "a workload TestMain doesn't know"`) {
		t.Errorf("the reason is %q", f.Reason)
	}
}

// TestAReaderThatSkipsACommitIsCaught: a reader whose work skips its third
// commit is caught by its own Reader, in its own process.
func TestAReaderThatSkipsACommitIsCaught(t *testing.T) {
	_, err := Run(skippingReader, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if f := mustFail(t, err, OutOfOrder); !strings.Contains(f.Reason, "was handed commit 4 where commit 3 comes next") {
		t.Errorf("the reason is %q", f.Reason)
	}
}

// TestAReaderThatStopsIsCaught: a reader that stops reading after its
// third commit doesn't come to the end of the file once the writers have
// gone, and the run fails once it has waited for it.
func TestAReaderThatStopsIsCaught(t *testing.T) {
	r, err := newRun(stuckReader, Options{Path: filepath.Join(t.TempDir(), "toy"), Time: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r.waits.catchUp = 300 * time.Millisecond
	_, err = r.run()
	if f := mustFail(t, err, Behind); !strings.Contains(f.Reason, "had seen 3") {
		t.Errorf("the reason is %q", f.Reason)
	}
}

// TestARunThatDoesNothingIsCaught: writers that never finish a commit make
// a run that means nothing, which fails once it has waited for the least
// work.
func TestARunThatDoesNothingIsCaught(t *testing.T) {
	r, err := newRun(idleWriter, Options{Path: filepath.Join(t.TempDir(), "toy"), Time: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r.waits.idle = 300 * time.Millisecond
	_, err = r.run()
	if f := mustFail(t, err, Idle); !strings.Contains(f.Reason, "writers saw 0 commits succeed") {
		t.Errorf("the reason is %q", f.Reason)
	}
}

// TestAPipeHeldOpenDoesntHangTheRun: a writer that started a process
// which holds its reports' pipe open seems not to end when it's killed,
// since its reports don't. The run stops waiting for it after a while,
// and fails.
func TestAPipeHeldOpenDoesntHangTheRun(t *testing.T) {
	r, err := newRun(pipeHolder, Options{Path: filepath.Join(t.TempDir(), "toy"), Writers: 1, Readers: 1})
	if err != nil {
		t.Fatal(err)
	}
	r.waits.kill = 300 * time.Millisecond
	_, err = r.run()
	if f := mustFail(t, err, Failed); !strings.Contains(f.Reason, "its reports' pipe was still open") {
		t.Errorf("the reason is %q", f.Reason)
	}
}

// TestAChildEndsWhenItsParentGoes: a child whose parent has gone, so that
// the other end of its standard input has closed, ends by itself.
func TestAChildEndsWhenItsParentGoes(t *testing.T) {
	r, err := newRun(toy, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if err != nil {
		t.Fatal(err)
	}
	k := &kid{id: 1, role: roleReader, out: &tail{}}
	reports, err := r.command(k, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer reports.Close()
	go lines(reports, func(string) {}, func() {})
	time.Sleep(50 * time.Millisecond)
	k.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- k.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		k.cmd.Process.Kill()
		t.Fatal("the child is still running 10 seconds after its parent's end of its standard input closed")
	}
	if ws, _ := k.cmd.ProcessState.Sys().(syscall.WaitStatus); !ws.Exited() || ws.ExitStatus() != exitOrphan {
		t.Errorf("the child ended with %v, where exit status %d is wanted%s", k.cmd.ProcessState, exitOrphan, k.out)
	}
}

// skipThird is a Reader's stand-in that hands on every commit but the
// third.
type skipThird struct{ r *Reader }

func (s skipThird) Apply(seq uint64, commit string) {
	if seq != 3 {
		s.r.Apply(seq, commit)
	}
}

// stopAfter hands on the first n commits to r, then stops the reader's work
// for good.
func stopAfter(r *Reader, n uint64) applier {
	return applyFunc(func(seq uint64, commit string) {
		r.Apply(seq, commit)
		for seq == n {
			time.Sleep(time.Hour)
		}
	})
}

type applyFunc func(seq uint64, commit string)

func (f applyFunc) Apply(seq uint64, commit string) { f(seq, commit) }

// TestAWriterGapLeavesItsSlotEmpty: with a gap of 40 to 60 milliseconds
// after each writer, in lives of 10 to 20, each writer's slot starts a new
// writer at most once every 50 milliseconds, so the run starts far fewer
// writers than it would without the gap, and passes as before.
func TestAWriterGapLeavesItsSlotEmpty(t *testing.T) {
	rep, err := Run(toy, Options{Path: filepath.Join(t.TempDir(), "toy"), Time: 600 * time.Millisecond,
		WriterLife: Span{Min: 10 * time.Millisecond, Max: 20 * time.Millisecond}, WriterGap: Span{Min: 40 * time.Millisecond, Max: 60 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if most := 2 * (int(rep.Took/(50*time.Millisecond)) + 1); rep.Writers > most {
		t.Errorf("%d writers started in %v, where the gaps leave room for %d at most", rep.Writers, rep.Took, most)
	}
	t.Log(rep)
}

// TestARunWaitsForTheLeastCounts: a reader counts each twentieth commit it
// sees, and a run whose Least wants 10 of them goes on past its time until
// it has them, which its report gives. A run that wants a count no process
// ever makes fails once it has waited for the least work, and says which.
func TestARunWaitsForTheLeastCounts(t *testing.T) {
	rep, err := Run(countingReader, Options{Path: filepath.Join(t.TempDir(), "toy"), Time: 50 * time.Millisecond, Least: map[string]int{"twentieth": 10}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts["twentieth"] < 10 {
		t.Errorf("the run ended with %d of the 10 counts it wants", rep.Counts["twentieth"])
	}
	t.Log(rep)

	r, err := newRun(countingReader, Options{Path: filepath.Join(t.TempDir(), "toy"), Time: 100 * time.Millisecond, Least: map[string]int{"never": 1}})
	if err != nil {
		t.Fatal(err)
	}
	r.waits.idle = 500 * time.Millisecond
	_, err = r.run()
	if f := mustFail(t, err, Idle); !strings.Contains(f.Reason, `counted "never" 0 times, short of the 1 wanted`) {
		t.Errorf("the reason is %q", f.Reason)
	}
}
