// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Workload is what a test gives the harness: a writer's work and a
// reader's work, which run in child processes, and a way to read the
// database once at the end, which runs in the test's process. The
// children are copies of the test binary, so the test's TestMain hands
// every workload it runs to Main, each under a name of its own.
//
// A commit is a string the workload chooses, as a crash.Workload's state
// is: what the commit holds, written so that no two commits in a run hold
// the same, and so that a commit read back torn reads as another string.
// The harness compares commits as strings, so it needs nothing from the
// engine.
type Workload struct {
	// Name tells a child process which workload is its own, among the
	// ones Main was given.
	Name string

	// Write is a writer process's work. It opens the database at w.Path()
	// and commits until it's killed, and tells w about each commit: Begin
	// before any of it can reach the file, then Done once it has succeeded,
	// or Failed when it has failed, which leaves its outcome unknown. An
	// error from Write ends the process and fails the run. When Write
	// returns nil, the process ends, and a new writer starts in its place,
	// as after a kill.
	Write func(w *Writer) error

	// Read is a reader process's work. It reads the database at r.Path()
	// until it's killed, and hands r each commit it finds, in order, with
	// Apply. A reader that starts again from the first commit, as one that
	// opens the database afresh for each read does, calls Reset first. An
	// error from Read ends the process and fails the run, and a return of
	// nil ends it as Write's does.
	Read func(r *Reader) error

	// Final reads the database at path once every writer has ended,
	// holding the write lock, as the next writer would. So it checks first
	// whatever a killed writer left, as the next writer would check it, and
	// once it has, nothing is left for anyone to change in the file. It
	// returns every commit the file holds, in order, as Read hands them to a
	// Reader. The readers still running then have to come to the same end.
	Final func(path string) ([]string, error)

	// Backup and Restore are for a run that moves backups into place
	// (Options.Restores), in the run's own process, as Final is. Backup
	// copies the database at path to the new file at to, beside it, as the
	// database stands at one commit, holding the write lock while it copies,
	// and marks the copy as the file of era era, so that a reader that reads
	// it can say which era it reads (Reader.Restart). It returns the commits
	// the copy holds, in order, as Final returns them.
	Backup func(path, to string, era int) ([]string, error)

	// Restore moves the backup at from into place at path, as a backup is
	// put back, holding the write lock of the file at the path while it does,
	// and returns the commits that file held, in order. Those past the
	// backup's are lost with it.
	Restore func(from, path string) ([]string, error)
}

// Options says how a run goes. The zero value, with a Path, is a run of 2
// writers and 2 readers for 2 seconds, or for half a second with -short.
type Options struct {
	// Path is the database's file, as an absolute path, in a folder that
	// exists. A run usually starts with nothing there, and the first writer
	// creates it.
	Path string

	// Writers and Readers are how many processes of each kind run at once.
	// 0 means 2.
	Writers, Readers int

	// Time is how long the run goes on killing processes and starting new
	// ones in their place. When it's 0, it's HYPERCRUX_PROCS_TIME, a Go
	// duration such as "10m", for long runs (R1), or else 2 seconds, or
	// half a second with -short. A run goes on past Time until it has done
	// enough to mean something (see Run).
	Time time.Duration

	// Kills, when it's above 0, ends the run once that many processes have
	// been killed, if that comes before Time is up.
	Kills int

	// WriterLife and ReaderLife are how long each process of the kind
	// lives, drawn evenly from the span, from when it starts its work to
	// its kill. A zero span means 10 to 200 milliseconds. A process that
	// takes long to start loses none of its life to that.
	WriterLife, ReaderLife Span

	// WriterGap is how long a writer's slot stays empty once its writer has
	// ended, before a new writer starts there, drawn evenly from the span.
	// A zero span means no gap. With gaps long enough beside the lives,
	// there are times with no writer at all, when a reader that keeps the
	// database open finds the writer gone, and is the first to find what a
	// killed writer left.
	WriterGap Span

	// Least is what a run's workload has to have counted before the run
	// ends, by the names its processes count under (Writer.Count and
	// Reader.Count), on top of the least work every run does (see Run).
	Least map[string]int

	// Restores, when it isn't zero, has the run take a backup of the database
	// at random moments, and move it into place a while later, with the
	// workload's Backup and Restore, one backup at a time, until the run's
	// time is up. Each wait, from the start of the run or the last restore to
	// the next backup, and from a backup to its restore, is drawn from the
	// span. Each restore begins a new era of the log, numbered from 1, era 0
	// being the database before any: the file at the path then holds the
	// backup's commits, and the commits after them in it aren't the ones the
	// replaced file held. The checks are made era by era (see Run). A run
	// with restores goes on until it has made 2.
	Restores Span

	// Seed seeds the run's random choices: each process's life, and the
	// seed each child's Rand starts from. 0 means HYPERCRUX_PROCS_SEED when
	// that's set, and a seed of the run's own otherwise. The report and
	// every failure give it. The same seed makes the same choices, though
	// the processes' timing won't replay.
	Seed uint64
}

// Span is a span of time, from Min to Max.
type Span struct{ Min, Max time.Duration }

// Problem is what a run found wrong.
type Problem uint8

// The problems a run can find.
const (
	// NotBegun: a reader found a commit that no writer began, or the file
	// held one at the end. That's a commit read torn, or made up.
	NotBegun Problem = iota + 1
	// Disagree: two readers found different commits at one place in the
	// log, or one reader did in two reads, or a reader found a commit that
	// the file at the end holds something else in place of.
	Disagree
	// OutOfOrder: a reader was handed a commit out of order, or one was
	// skipped, or the file at the end holds a writer's commits in another
	// order than the writer made them.
	OutOfOrder
	// Twice: the file at the end holds one commit in two places.
	Twice
	// Lost: a commit a reader had seen was gone from its next read from the
	// first commit, or from the file at the end.
	Lost
	// Missing: a commit its writer saw succeed isn't in the file at the
	// end, nor among the commits a restore lost.
	Missing
	// Behind: a reader still running at the end didn't come to the end of
	// the file in time.
	Behind
	// Failed: a process's work failed, or a process ended without being
	// killed, or Final failed.
	Failed
	// Idle: the run didn't do enough to mean anything.
	Idle
)

var problemNames = [...]string{
	NotBegun: "not begun", Disagree: "disagree", OutOfOrder: "out of order", Twice: "twice",
	Lost: "lost", Missing: "missing", Behind: "behind", Failed: "failed", Idle: "idle",
}

func (p Problem) String() string {
	if p >= NotBegun && p <= Idle {
		return problemNames[p]
	}
	return fmt.Sprintf("Problem(%d)", uint8(p))
}

// Failure is the first problem a run found. Its reason names processes as
// writer 3 or reader 4, by the numbers Writer.ID and Reader.ID give, and
// commits by their sequence numbers, from 1, and in a run that has moved
// backups into place, their eras (Options.Restores), in which places in the
// log, and the problems about them, are an era's own.
type Failure struct {
	Problem Problem
	Reason  string
	Report  Report // what the run had done when it stopped
}

func (f *Failure) Error() string {
	return fmt.Sprintf("procs: %v: %s. The run's seed was %d, and HYPERCRUX_PROCS_SEED=%[3]d makes the same random choices again, though the processes' timing won't replay",
		f.Problem, f.Reason, f.Report.Seed)
}

// Report says what a run did.
type Report struct {
	Seed uint64        // the run's seed
	Took time.Duration // from the start of the first process to the end of the last

	// The processes started of each kind, and of those, the ones the
	// harness killed.
	Writers, Readers             int
	WritersKilled, ReadersKilled int

	// The commits writers began, and how they ended: Done, the ones their
	// writers saw succeed; Errors, the ones that failed; and UnderWay, the
	// ones under way when their writer was killed, of which UnderWayIn
	// were in the file at the end.
	Begun, Done, Errors, UnderWay, UnderWayIn int

	// Commits is how many commits the file held at the end.
	Commits int

	// Seen counts the commits the readers saw, each once for each reader
	// that saw it, and Reads the times a reader started again from the
	// first commit.
	Seen, Reads int

	// Cut counts the reports a kill cut short, which the harness drops.
	Cut int

	// Counts is what the workload's processes counted, by name (Writer.Count
	// and Reader.Count), or nil when they counted nothing.
	Counts map[string]int

	// Restores counts the backups moved into place (Options.Restores), and
	// Lost the commits the files they replaced held past the backup's.
	Restores, Lost int
}

func (r Report) String() string {
	s := fmt.Sprintf("seed %d, %v: %d writers and %d readers started, %d and %d of them killed; "+
		"%d commits begun: %d seen to succeed, %d failed, and %d under way when their writer was killed, %d of those in the file at the end; "+
		"%d commits in the file at the end; the readers saw %d commits between them, and started again from the first %d times; %d reports cut short by a kill",
		r.Seed, r.Took.Round(time.Millisecond), r.Writers, r.Readers, r.WritersKilled, r.ReadersKilled,
		r.Begun, r.Done, r.Errors, r.UnderWay, r.UnderWayIn, r.Commits, r.Seen, r.Reads, r.Cut)
	if r.Restores > 0 {
		s += fmt.Sprintf("; %d backups moved into place, which lost %d commits the files they replaced held past them", r.Restores, r.Lost)
	}
	if len(r.Counts) > 0 {
		var counts []string
		for _, name := range slices.Sorted(maps.Keys(r.Counts)) {
			counts = append(counts, fmt.Sprintf("%s %d", name, r.Counts[name]))
		}
		s += "; the workload counted " + strings.Join(counts, ", ")
	}
	return s
}
