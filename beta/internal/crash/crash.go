// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package crash

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// Workload is what a test gives the driver: the commits to make, a way to
// open the database and read its state back, and where the database is.
// A state is a string the test chooses, such as the keys of the records in
// order, and two states are the same when their strings are.
type Workload struct {
	// Path is the database's file, as an absolute path. Its folder is made
	// on every new disk, and a copy mid-commit reads the file there.
	Path string

	// Setup, when it isn't nil, runs first on every new disk, with no
	// faults and none of its calls counted, so the commits have something
	// to start from. It returns the state it leaves, which counts as
	// committed: no cut afterwards may take it away. Without Setup, the
	// state before the first commit is the one Reopen finds on a new disk.
	Setup func(sys fsys.FS) (string, error)

	// Run makes the commits through sys, and tells m about each one: Begin
	// before it starts, and Done once it has succeeded. The driver runs it
	// many times, and it has to make the same calls in the same order every
	// time, until a fault changes its course. So it can't make file calls
	// from goroutines side by side, and anything random in it has to be
	// seeded the same way each time. In a run with no fault, an error from
	// Run fails the driver. In a run with one, an error is the fault's
	// doing, and the driver goes on to check the database.
	Run func(sys fsys.FS, m *Model) error

	// Reopen opens the database through sys, as a process started after a
	// crash would, and returns its state. The driver calls it after every
	// cut and every failure, and on every copy. An error from it is a
	// failure, since the database has to open every time.
	Reopen func(sys fsys.FS) (string, error)
}

// Model is a run's account of its commits: the state each one gives, and
// which have succeeded. The driver hands a new one to every run of the
// workload, holding the state the run starts from.
//
// After a cut or a failure, the database has to hold the state of the last
// commit that succeeded, or of one begun after it: the commit under way, or
// one that failed, since a failed commit's outcome is unknown. A workload
// that carries on after a failed commit gives each later commit the state
// it would give without the failed one. A log can't build on a failed
// commit, since it makes the failed batch unusable before it lets go of the
// lock, or else refuses every write after it (BETA.md, "Commits").
type Model struct {
	states []string // the state the run started from, then the state each commit begun gives
	last   int      // the index in states of the last commit that succeeded, or 0
}

// Begin says a commit is starting, which gives the state state once it
// succeeds. From now until Done, a cut or a failure may leave the database
// at that state or at the one before.
func (m *Model) Begin(state string) {
	m.states = append(m.states, state)
}

// Done says the commit Begin started has succeeded, so from now on a cut
// has to leave the database at its state or a later one. It panics when no
// commit is under way.
func (m *Model) Done() {
	if m.last == len(m.states)-1 {
		panic("crash: Model.Done with no commit under way")
	}
	m.last = len(m.states) - 1
}

// Kind is a kind of point. Options.Kinds holds a set of them.
type Kind uint8

// The kinds of point.
const (
	// Cuts: the power cut after the last call, and in each call of the
	// kinds Options.CutIn names.
	Cuts Kind = 1 << iota
	// Failures: each call failing in turn.
	Failures
	// Copies: a copy of the file, as cp would take it, from each call on.
	Copies

	allKinds = Cuts | Failures | Copies
)

func (k Kind) String() string {
	switch k {
	case Cuts:
		return "cuts"
	case Failures:
		return "failures"
	case Copies:
		return "copies"
	}
	return fmt.Sprintf("Kind(%#x)", uint8(k))
}

// Options says which points the driver runs, and how.
type Options struct {
	// Kinds is the kinds of point to run: all of them when it's 0.
	Kinds Kind

	// Seeds is how many seeds each point runs with: Seed and the ones
	// after it. 0 means 1. When HYPERCRUX_CRASH_SEEDS is set, it overrides
	// Seeds, so a long run (R1) can try more of what a cut can do.
	Seeds int
	Seed  uint64

	// CutIn is the kinds of call the power is cut in, at each call of each
	// kind in turn. When it's empty, they're the calls that change what a
	// cut can leave on the disk, data or names: WriteAt, Truncate, Create,
	// Rename, RenameNoReplace, Remove and SyncDir. fault.Any cuts it in
	// every call. A cut in a WriteAt or a Truncate comes once the call has
	// reached the page cache, and in any other call before the call, so a
	// cut in a SyncDir comes after the names it would have made last. A cut
	// in a Sync is left out, since it comes straight after the write before
	// it, where there's a cut already.
	CutIn []fault.Op

	// Err is the error a failed call gives: syscall.EIO when it's nil.
	// Times is fault.Rule's: 0 fails the one call, a larger number that many
	// calls in a row from it, and a negative number every call from it on,
	// as on a disk that has died.
	Err   error
	Times int

	// Chunk is how many bytes a copy reads at a time. It reads one chunk
	// just before each of the workload's calls from the one it starts at,
	// and the rest once the workload has ended, so a small chunk spreads a
	// copy over several calls. 0 means 128 KiB, the buffer cp reads with,
	// which copies a small file between two calls.
	Chunk int

	// Replay, when it isn't "", is one point to run alone, as Point.String
	// writes it. When it's "", HYPERCRUX_CRASH_POINT sets it.
	Replay string
}

// Point is one run of the workload with one thing done to it.
type Point struct {
	Kind Kind

	// Op is the kind of call a cut comes in. It's fault.Any for a failure,
	// for a copy, and for a cut after the last call.
	Op fault.Op

	// N picks the call, counted from 1: for a cut, among the calls of the
	// kind Op, or 0 for a cut after the last call; for a failure, among all
	// the calls; for a copy, the call it starts just before, which is one
	// past the last call for a copy taken once the workload has ended.
	N int

	// Seed is the disk's seed.
	Seed uint64
}

// String writes p as Options.Replay and HYPERCRUX_CRASH_POINT take it:
// "cut WriteAt 5 seed 3", "cut end seed 3", "fail 12 seed 0" or "copy 7
// seed 0".
func (p Point) String() string {
	switch {
	case p.Kind == Cuts && p.N == 0:
		return fmt.Sprintf("cut end seed %d", p.Seed)
	case p.Kind == Cuts:
		return fmt.Sprintf("cut %v %d seed %d", p.Op, p.N, p.Seed)
	case p.Kind == Failures:
		return fmt.Sprintf("fail %d seed %d", p.N, p.Seed)
	case p.Kind == Copies:
		return fmt.Sprintf("copy %d seed %d", p.N, p.Seed)
	}
	return fmt.Sprintf("Point{Kind %d, Op %v, N %d, seed %d}", p.Kind, p.Op, p.N, p.Seed)
}

// ParsePoint reads a point as Point.String writes it.
func ParsePoint(s string) (Point, error) {
	f := strings.Fields(s)
	bad := fmt.Errorf("crash: %q isn't a point, which reads like \"cut WriteAt 5 seed 3\", \"cut end seed 3\", \"fail 12 seed 0\" or \"copy 7 seed 0\"", s)
	if len(f) < 4 || f[len(f)-2] != "seed" {
		return Point{}, bad
	}
	seed, err := strconv.ParseUint(f[len(f)-1], 10, 64)
	if err != nil {
		return Point{}, bad
	}
	p := Point{Op: fault.Any, Seed: seed}
	n := f[1]
	switch {
	case f[0] == "cut" && len(f) == 4 && f[1] == "end":
		p.Kind = Cuts
		return p, nil
	case f[0] == "cut" && len(f) == 5:
		p.Kind, n = Cuts, f[2]
		op, ok := opNamed(f[1])
		if !ok {
			return Point{}, bad
		}
		p.Op = op
	case f[0] == "fail" && len(f) == 4:
		p.Kind = Failures
	case f[0] == "copy" && len(f) == 4:
		p.Kind = Copies
	default:
		return Point{}, bad
	}
	if p.N, err = strconv.Atoi(n); err != nil || p.N < 1 {
		return Point{}, bad
	}
	return p, nil
}

// opNamed returns the kind of call with the name fault.Op's String gives.
func opNamed(name string) (fault.Op, bool) {
	for op := fault.Any; op <= fault.List; op++ {
		if op.String() == name {
			return op, true
		}
	}
	return 0, false
}

// Failure is the first point where something went wrong. Its reason
// numbers commits from 1, in the order the workload began them, and calls
// the state the run started from commit 0's.
type Failure struct {
	Point   Point
	Problem Problem
	Reason  string
}

// Problem is what went wrong at a point.
type Problem uint8

// The problems a point can find.
const (
	// NotOpened: the database didn't open.
	NotOpened Problem = iota + 1
	// NotAllowed: it opened at a state the model doesn't allow.
	NotAllowed
	// Lost: after a cut it opened at an earlier state than the open before
	// the cut had found.
	Lost
	// RunFailed: the workload failed with nothing done to it.
	RunFailed
	// Diverged: the workload made fewer calls, or more, than it made when
	// they were counted, so the call a point picked never came.
	Diverged
)

var problemNames = [...]string{NotOpened: "not opened", NotAllowed: "not allowed", Lost: "lost", RunFailed: "run failed", Diverged: "diverged"}

func (p Problem) String() string {
	if p >= NotOpened && p <= Diverged {
		return problemNames[p]
	}
	return fmt.Sprintf("Problem(%d)", uint8(p))
}

func (f *Failure) Error() string {
	return fmt.Sprintf("crash: %v: %s. Set HYPERCRUX_CRASH_POINT to %q and run the test alone to replay it", f.Point, f.Reason, f.Point.String())
}

// Report says what the driver did when nothing went wrong.
type Report struct {
	Calls int // the calls one run of the workload makes
	Seeds int // the seeds each point ran with

	// The points of each kind, each seed counted.
	Cuts, Failures, Copies int

	// NoFile counts the copy points with no file at the path to copy yet.
	// Those copies weren't taken.
	NoFile int

	// The opens that found the last commit that succeeded, and those that
	// found a commit begun after it. For a copy, that's the last commit
	// that succeeded before the copy began.
	Last, Later int
}

func (r Report) String() string {
	return fmt.Sprintf("%d calls a run; %d cuts, %d failures and %d copies, from %d seeds each, with %d copy points before the file existed; "+
		"%d opens found the last commit that succeeded and %d a commit begun after it",
		r.Calls, r.Cuts, r.Failures, r.Copies, r.Seeds, r.NoFile, r.Last, r.Later)
}
