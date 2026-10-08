// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package crash

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
)

// Run runs the workload once with no fault, to count its calls, and then
// once for each point of the kinds o asks for, with each seed in turn, in
// this order:
//
//  1. Cuts: the power cut after the last call, then in each call of the
//     kinds CutIn names, in CutIn's order and then the calls'.
//  2. Failures: each call failing, from the first to the last.
//  3. Copies: a copy taken from each call on, from the first to once the
//     workload has ended.
//
// Every run starts on a new disk, fault.New with the point's seed, holding
// the database's folder and whatever Setup leaves. A cut or a failure comes
// from a fault.Rule, which counts the calls from when it's set, after
// Setup. Once the workload returns, the driver clears the rule and ends the
// workload's process as Linux would, closing every file it left open, so
// its flocks go and the page cache keeps what it wrote. Then it checks the
// database:
//
//   - After a cut, it opens the database on the disk the cut left. After a
//     failure, or a run with nothing done to it, it opens it as a process
//     started once the workload's had ended, with no cut. The state found
//     has to be the last commit that succeeded or one begun after it, as
//     Model says.
//   - Then it cuts the power once more and opens the database again. That
//     state has to be allowed too, and no earlier than the first open's: a
//     reader applies a batch only once it's on the drive, so a commit a
//     reader has seen has to last, and so does whatever the first open did
//     to recover the file.
//   - A copy goes onto a disk of its own and is checked in the same way,
//     against the commits from the last that had succeeded when the copy
//     began to the last begun before it ended.
//
// Run returns a *Failure for the first point that goes wrong, and a plain
// error when the workload or the options can't be used: when Setup fails,
// or Reopen fails on a new disk. The Report says what was run, so far.
func Run(w Workload, o Options) (Report, error) {
	r, err := newRunner(w, o)
	if err != nil {
		return Report{}, err
	}
	if r.replay != nil {
		err := r.point(*r.replay)
		return r.rep, err
	}
	if err := r.count(); err != nil {
		return r.rep, err
	}
	each := func(pt Point) error {
		for _, seed := range r.seeds {
			pt.Seed = seed
			if err := r.point(pt); err != nil {
				return err
			}
		}
		return nil
	}
	if r.kinds&Cuts != 0 {
		if err := each(Point{Kind: Cuts, Op: fault.Any}); err != nil {
			return r.rep, err
		}
		for _, op := range r.cutIn {
			for n := 1; n <= r.counts[op]; n++ {
				if err := each(Point{Kind: Cuts, Op: op, N: n}); err != nil {
					return r.rep, err
				}
			}
		}
	}
	if r.kinds&Failures != 0 {
		for n := 1; n <= r.counts[fault.Any]; n++ {
			if err := each(Point{Kind: Failures, Op: fault.Any, N: n}); err != nil {
				return r.rep, err
			}
		}
	}
	if r.kinds&Copies != 0 {
		for n := 1; n <= r.counts[fault.Any]+1; n++ {
			if err := each(Point{Kind: Copies, Op: fault.Any, N: n}); err != nil {
				return r.rep, err
			}
		}
	}
	return r.rep, nil
}

// runner is one call of Run.
type runner struct {
	w      Workload
	o      Options
	kinds  Kind
	seeds  []uint64
	cutIn  []fault.Op
	replay *Point // the one point to run, or nil

	start  string           // the state a run starts from, when there's no Setup
	counts map[fault.Op]int // the calls of each kind a run makes, once they've been counted
	buf    []byte           // a copy's chunk
	rep    Report
}

func newRunner(w Workload, o Options) (*runner, error) {
	if !path.IsAbs(w.Path) || w.Run == nil || w.Reopen == nil {
		return nil, errors.New("crash: a Workload needs an absolute Path, a Run and a Reopen")
	}
	r := &runner{w: w, o: o, kinds: o.Kinds, cutIn: o.CutIn}
	if r.kinds == 0 {
		r.kinds = allKinds
	}
	if r.kinds&^allKinds != 0 {
		return nil, fmt.Errorf("crash: Kinds is %#x, which holds a kind of point there isn't", uint8(o.Kinds))
	}
	if len(r.cutIn) == 0 {
		r.cutIn = []fault.Op{fault.WriteAt, fault.Truncate, fault.Create, fault.Rename, fault.RenameNoReplace, fault.Remove, fault.SyncDir}
	}
	for _, op := range r.cutIn {
		if op < fault.Any || op > fault.List {
			return nil, fmt.Errorf("crash: CutIn names %v, which isn't a kind of call", op)
		}
	}
	chunk := o.Chunk
	if chunk <= 0 {
		chunk = 128 << 10
	}
	r.buf = make([]byte, chunk)
	seeds := max(o.Seeds, 1)
	if s := os.Getenv("HYPERCRUX_CRASH_SEEDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("crash: HYPERCRUX_CRASH_SEEDS is %q, where a whole number from 1 is wanted", s)
		}
		seeds = n
	}
	for i := range seeds {
		r.seeds = append(r.seeds, o.Seed+uint64(i))
	}
	r.rep.Seeds = seeds
	replay := o.Replay
	if replay == "" {
		replay = os.Getenv("HYPERCRUX_CRASH_POINT")
	}
	if replay != "" {
		pt, err := ParsePoint(replay)
		if err != nil {
			return nil, err
		}
		r.replay = &pt
		r.rep.Seeds = 1
	}
	if w.Setup == nil {
		d := fault.New(o.Seed)
		if err := d.Mkdir(path.Dir(w.Path)); err != nil {
			return nil, err
		}
		p := newProc(d.FS())
		s, err := w.Reopen(p)
		p.kill()
		if err != nil {
			return nil, fmt.Errorf("crash: Reopen fails on a new disk: %w", err)
		}
		r.start = s
	}
	return r, nil
}

// fresh makes the disk for a run with the seed seed: a new one, with the
// database's folder and whatever Setup leaves on it. It returns the disk
// and the run's model, holding the state the run starts from.
func (r *runner) fresh(seed uint64) (*fault.Disk, *Model, error) {
	d := fault.New(seed)
	if err := d.Mkdir(path.Dir(r.w.Path)); err != nil {
		return nil, nil, err
	}
	start := r.start
	if r.w.Setup != nil {
		p := newProc(d.FS())
		s, err := r.w.Setup(p)
		p.kill()
		if err != nil {
			return nil, nil, fmt.Errorf("crash: Setup fails: %w", err)
		}
		start = s
	}
	return d, &Model{states: []string{start}}, nil
}

// count runs the workload with no fault and counts the calls it makes: of
// every kind, and of each kind in CutIn.
func (r *runner) count() error {
	pt := Point{Kind: Cuts, Op: fault.Any, Seed: r.seeds[0]}
	d, m, err := r.fresh(pt.Seed)
	if err != nil {
		return err
	}
	before := r.calls(d)
	p := newProc(d.FS())
	err = r.w.Run(p, m)
	after := r.calls(d)
	p.kill()
	if err != nil {
		return &Failure{Point: pt, Problem: RunFailed, Reason: fmt.Sprintf("the workload fails with no fault: %v", err)}
	}
	r.counts = map[fault.Op]int{}
	for op, n := range after {
		r.counts[op] = n - before[op]
	}
	if r.counts[fault.Any] != p.calls {
		return fmt.Errorf("crash: the disk counted %d calls, and the driver's process %d", r.counts[fault.Any], p.calls)
	}
	r.rep.Calls = p.calls
	return nil
}

// calls reads the disk's counts of the kinds of call the driver needs.
func (r *runner) calls(d *fault.Disk) map[fault.Op]int {
	c := map[fault.Op]int{fault.Any: d.Calls(fault.Any, "")}
	for _, op := range r.cutIn {
		c[op] = d.Calls(op, "")
	}
	return c
}

// failer makes a point's Failure.
type failer func(problem Problem, format string, args ...any) error

// point runs the workload once with pt done to it, and checks the
// database it leaves.
func (r *runner) point(pt Point) error {
	d, m, err := r.fresh(pt.Seed)
	if err != nil {
		return err
	}
	fail := func(problem Problem, format string, args ...any) error {
		return &Failure{Point: pt, Problem: problem, Reason: fmt.Sprintf(format, args...)}
	}
	p := newProc(d.FS())
	var c *copier
	switch pt.Kind {
	case Cuts:
		r.rep.Cuts++
		if pt.N > 0 {
			d.Add(fault.Rule{Op: pt.Op, N: pt.N, Cut: true})
		}
	case Failures:
		r.rep.Failures++
		d.Add(fault.Rule{Op: fault.Any, N: pt.N, Err: r.o.Err, Times: r.o.Times})
	case Copies:
		c = &copier{sys: d.FS(), path: r.w.Path, at: pt.N, buf: r.buf, m: m}
		p.copy = c
	default:
		return fmt.Errorf("crash: %v isn't a point", pt)
	}
	err = r.w.Run(p, m)
	d.Clear()
	if c != nil {
		c.finish(p.calls)
	}
	p.kill()
	lo, hi := m.last, len(m.states)-1
	differs := "so it doesn't make the same calls in every run"
	ended := [2]string{"once the workload's process had ended", "after a cut"}
	switch {
	case pt.Kind == Cuts && pt.N == 0:
		if err != nil {
			return fail(RunFailed, "the workload fails with no fault: %v", err)
		}
		if r.counts != nil && p.calls != r.rep.Calls {
			return fail(Diverged, "the workload made %d calls, and %d when they were counted, %s", p.calls, r.rep.Calls, differs)
		}
		return r.check(d, m, lo, hi, ended, fail)
	case pt.Kind == Cuts:
		if d.Cuts() == 0 {
			return fail(Diverged, "the power was to be cut in %v call %d, and the workload made fewer %v calls than that, %s", pt.Op, pt.N, pt.Op, differs)
		}
		return r.check(d, m, lo, hi, [2]string{"after the cut", "after a second cut"}, fail)
	case pt.Kind == Failures:
		if d.Failed() == 0 {
			return fail(Diverged, "call %d was to fail, and the workload made fewer calls than that, %s", pt.N, differs)
		}
		return r.check(d, m, lo, hi, ended, fail)
	}
	switch {
	case c.err != nil:
		return fmt.Errorf("crash: %v: the copy's read fails: %w", pt, c.err)
	case c.stage == waiting:
		return fail(Diverged, "the copy was to start just before call %d, and the workload made %d calls, %s", pt.N, p.calls, differs)
	case c.stage == noFile:
		r.rep.NoFile++
		return nil
	}
	r.rep.Copies++
	cd := fault.New(pt.Seed)
	if err := put(cd, r.w.Path, c.data); err != nil {
		return err
	}
	taken := fmt.Sprintf("as a copy taken just before call %d", pt.N)
	switch {
	case pt.N > p.calls:
		taken = "as a copy taken once the workload had ended"
	case c.end > p.calls:
		taken = fmt.Sprintf("as a copy read from just before call %d to the workload's end", pt.N)
	case c.end > pt.N:
		taken = fmt.Sprintf("as a copy read from just before call %d to just before call %d", pt.N, c.end)
	}
	return r.check(cd, m, c.from, c.to-1, [2]string{taken, "as a copy after a cut"}, fail)
}

// check opens the database on d and holds the state it finds to the
// model's states from lo to hi, which are what the point may leave. The
// first open comes at once, at the moment when[0] names. Then the power is
// cut, and the second open, at the moment when[1] names, has to find the
// first one's state or a later one.
func (r *runner) check(d *fault.Disk, m *Model, lo, hi int, when [2]string, fail failer) error {
	first, _, err := r.open(d, m, lo, hi, when[0], fail)
	if err != nil {
		return err
	}
	d.Cut()
	_, second, err := r.open(d, m, lo, hi, when[1], fail)
	if err != nil {
		return err
	}
	if second < first {
		return fail(Lost, "%s, the database opened at %s, and %s at %s, so a commit a reader had seen was lost", when[0], m.name(first), when[1], m.name(second))
	}
	return nil
}

// open opens the database on d once, as a process of its own. It returns
// the first and the last of the model's states from lo to hi that equal
// the state it finds, or a Failure when the database doesn't open or
// matches none of them.
func (r *runner) open(d *fault.Disk, m *Model, lo, hi int, when string, fail failer) (int, int, error) {
	p := newProc(d.FS())
	s, err := r.w.Reopen(p)
	p.kill()
	if err != nil {
		return 0, 0, fail(NotOpened, "%s, the database doesn't open: %v", when, err)
	}
	first, last := -1, -1
	for i := lo; i <= hi; i++ {
		if m.states[i] == s {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return 0, 0, fail(NotAllowed, "%s, the database opened at %s, where the model allows %s", when, m.describe(s), m.span(lo, hi))
	}
	if first == lo {
		r.rep.Last++
	} else {
		r.rep.Later++
	}
	return first, last, nil
}

// put writes data as the file at name on the new disk d, with its folder,
// and syncs the file and the folder, so the copy is on the drive with its
// name.
func put(d *fault.Disk, name string, data []byte) error {
	dir := path.Dir(name)
	if err := d.Mkdir(dir); err != nil {
		return err
	}
	sys := d.FS()
	f, err := sys.Create(name, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteAt(data, 0); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return sys.SyncDir(dir)
}

// name names the state at index i in the model.
func (m *Model) name(i int) string {
	return fmt.Sprintf("commit %d's state", i)
}

// describe names the state s: the latest commit that gives it, or the
// state itself, shortened, when no commit does.
func (m *Model) describe(s string) string {
	for i := len(m.states) - 1; i >= 0; i-- {
		if m.states[i] == s {
			return m.name(i)
		}
	}
	const most = 200
	if len(s) > most {
		s = s[:most] + "..."
	}
	return fmt.Sprintf("a state no commit gives, %q", s)
}

// span names the states from lo to hi.
func (m *Model) span(lo, hi int) string {
	if lo == hi {
		return "only " + m.name(lo)
	}
	return fmt.Sprintf("commit %d's state to commit %d's", lo, hi)
}
