// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
)

// What a child process gets in its environment: its role, its workload,
// the database, its number in the run and its seed.
const (
	envRole      = "HYPERCRUX_PROCS_ROLE"
	envWorkload  = "HYPERCRUX_PROCS_WORKLOAD"
	envPath      = "HYPERCRUX_PROCS_PATH"
	envID        = "HYPERCRUX_PROCS_ID"
	envChildSeed = "HYPERCRUX_PROCS_CHILD_SEED"
)

// The roles.
const (
	roleWriter = "writer"
	roleReader = "reader"
)

// reportFD is the file a child writes its reports to: the first of
// exec.Cmd's ExtraFiles, which is the write end of a pipe the parent
// reads.
const reportFD = 3

// How a child ends when it ends by itself.
const (
	exitFailed = 1 // its work failed, or the harness found a problem in it
	exitOrphan = 3 // its parent has gone, or can't hear it
)

// Main is for the TestMain of a test that runs workloads. When the test
// binary was started as a child of a run, Main runs its role, with the
// workload of its run among ws, and ends the process. Otherwise it runs
// the tests:
//
//	func TestMain(m *testing.M) { procs.Main(m, toy, real) }
func Main(m *testing.M, ws ...Workload) {
	role := os.Getenv(envRole)
	if role == "" {
		os.Exit(m.Run())
	}
	rep := &reporter{w: os.NewFile(reportFD, "procs reports"), exit: os.Exit}
	go watchParent()
	runChild(rep, role, ws)
	// The work has returned nil. The parent hears of it, so it doesn't
	// kill the process on its way out.
	rep.send(kindEnd)
	os.Exit(0)
}

// watchParent ends the process once its parent has gone. The parent holds
// the other end of a pipe that is the child's standard input, and Linux
// closes it when the parent ends, however it ends, so the child doesn't
// outlive a test that timed out or was killed.
func watchParent() {
	io.Copy(io.Discard, os.Stdin)
	os.Exit(exitOrphan)
}

// runChild runs a child's role, and returns once its work has returned
// nil. When the work fails, it reports the error and ends the process.
func runChild(rep *reporter, role string, ws []Workload) {
	var w *Workload
	for i := range ws {
		if ws[i].Name == os.Getenv(envWorkload) {
			w = &ws[i]
		}
	}
	if w == nil {
		rep.fail(fmt.Errorf("procs: Main wasn't given a workload named %q", os.Getenv(envWorkload)))
	}
	id, err := strconv.Atoi(os.Getenv(envID))
	if err != nil {
		rep.fail(fmt.Errorf("procs: %s is %q: %w", envID, os.Getenv(envID), err))
	}
	seed, err := strconv.ParseUint(os.Getenv(envChildSeed), 10, 64)
	if err != nil {
		rep.fail(fmt.Errorf("procs: %s is %q: %w", envChildSeed, os.Getenv(envChildSeed), err))
	}
	p := proc{path: os.Getenv(envPath), id: id, rng: rand.New(rand.NewPCG(seed, uint64(id))), rep: rep}
	rep.send(kindStart, strconv.Itoa(os.Getpid()))
	switch role {
	case roleWriter:
		err = w.Write(&Writer{proc: p})
	case roleReader:
		err = w.Read(&Reader{proc: p, next: 1})
	default:
		err = fmt.Errorf("procs: no role is called %q", role)
	}
	if err != nil {
		rep.fail(err)
	}
}

// proc is what a writer and a reader have in common.
type proc struct {
	path string
	id   int
	rng  *rand.Rand
	rep  *reporter
}

// Path is the database's file.
func (p *proc) Path() string { return p.path }

// ID is the process's number, from 1, which no other process in the run
// has, writer or reader. A failure's reason names the process by it.
func (p *proc) ID() int { return p.id }

// Rand is a source of random numbers for the process's own choices, seeded
// from the run's seed and the process's number. It's for one goroutine at
// a time.
func (p *proc) Rand() *rand.Rand { return p.rng }

// Writer is a writer process's account of its commits, kept by its work
// through Begin, and Done or Failed. Each sends its report to the parent
// before it returns, so the parent hears of a commit before any of it can
// reach the file.
type Writer struct {
	proc
	under bool // a commit is under way
}

// Begin says a commit is starting, which holds commit. From now until Done
// or Failed, the commit may reach the file, and once it's there, it has to
// be there whole. Begin ends the process with a failure when a commit is
// under way already.
func (w *Writer) Begin(commit string) {
	if w.under {
		w.rep.fail(errors.New("procs: Writer.Begin with a commit under way"))
	}
	w.under = true
	w.rep.send(kindBegin, strconv.Quote(commit))
}

// Done says the commit under way has succeeded, so the file has to hold it
// at the end. It ends the process with a failure when no commit is under
// way.
func (w *Writer) Done() {
	if !w.under {
		w.rep.fail(errors.New("procs: Writer.Done with no commit under way"))
	}
	w.under = false
	w.rep.send(kindDone)
}

// Failed says the commit under way has failed with err, so its outcome is
// unknown: the file may hold it, whole, or not at all. A writer may go on
// to the next commit, as one whose lock timed out would. Failed ends the
// process with a failure when no commit is under way.
func (w *Writer) Failed(err error) {
	switch {
	case !w.under:
		w.rep.fail(errors.New("procs: Writer.Failed with no commit under way"))
	case err == nil:
		w.rep.fail(errors.New("procs: Writer.Failed with a nil error"))
	}
	w.under = false
	w.rep.send(kindFailed, strconv.Quote(err.Error()))
}

// Reader is a reader process's account of what it sees, kept by its work
// through Reset and Apply. It checks the reader's own sight as it goes,
// and a problem it finds ends the process and fails the run:
//
//   - Commits come in order, from 1 after a Reset, or from the start, each
//     the one after the last.
//   - A commit the reader has seen before holds the same when it comes
//     again.
//   - A read from the first commit comes at least as far as the reader had
//     seen before it, since a commit a reader has seen has to last. It ends
//     at the next Reset.
//
// The parent hears of each commit the reader sees for the first time. What
// the parent checks across readers, and against the file at the end, is
// Run's.
type Reader struct {
	proc
	kept []string // what each commit the reader has seen holds, by sequence number less 1
	next uint64   // the sequence number the next commit has to have
}

// Reset says the reader is starting again from the first commit, as one
// that opens the database afresh does for each read, or one that finds
// another file at the path. The read that ends here has to have come at
// least as far as the reader had seen.
func (r *Reader) Reset() {
	if came := r.next - 1; came < uint64(len(r.kept)) {
		end := fmt.Sprintf("ended after commit %d", came)
		if came == 0 {
			end = "found none"
		}
		r.rep.problem(Lost, "reader %d had seen commits 1 to %d, and its next read from the first commit %s, so commit %d had gone",
			r.id, len(r.kept), end, came+1)
	}
	r.next = 1
	r.rep.send(kindReset)
}

// Apply says the reader has found commit seq, holding commit, in its place
// in the log: the one after the last it applied, or the first after a
// Reset.
func (r *Reader) Apply(seq uint64, commit string) {
	switch {
	case seq != r.next:
		r.rep.problem(OutOfOrder, "reader %d was handed commit %d where commit %d comes next", r.id, seq, r.next)
	case seq <= uint64(len(r.kept)):
		if was := r.kept[seq-1]; commit != was {
			r.rep.problem(Disagree, "reader %d found commit %d holding %s, and on a later read from the first commit, holding %s",
				r.id, seq, short(was), short(commit))
		}
	default:
		r.kept = append(r.kept, commit)
		r.rep.send(kindSaw, strconv.FormatUint(seq, 10), strconv.Quote(commit))
	}
	r.next++
}
