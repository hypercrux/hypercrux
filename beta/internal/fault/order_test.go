// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault_test

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// The name oracle knows what the model promises about names and owners,
// and nothing more. It's written from Disk's comment alone: the drive's
// names after the last cut, every change since in call order, and which of
// them a SyncDir has put on the drive. After a cut, the names must be what
// the promised changes, with the others up to some point, make of the
// drive's, for one point or another. It applies the changes in call order,
// where the disk puts a folder's on the drive when it's synced.
type nameOracle struct {
	base    map[string]uint64 // the drive's names after the last cut, as inode numbers
	owners  map[uint64]int    // the drive's owners then, by inode number
	changes []nameChange      // every change since, in call order
}

// nameChange is a change to names, or to an owner.
type nameChange struct {
	dirs []string  // the folders it's in
	sets []nameSet // the names it sets, in order; inode number 0 takes one away
	ino  uint64    // the file whose owner it sets, or 0
	uid  int       // the owner it sets
	sure bool      // a SyncDir has put it on the drive
}

type nameSet struct {
	path string
	ino  uint64
}

// unknown stands for the inode number of a file made by a Create the power
// was cut in, which the test never learns.
const unknown = math.MaxUint64

// syncDir marks as sure every change made so far in dir, and every earlier
// change in a folder that a sure change is in too, over and over until
// nothing more is marked.
func (o *nameOracle) syncDir(dir string) {
	upto := map[string]int{dir: len(o.changes)} // how many changes, from the first, a folder needs sure
	for more := true; more; {
		more = false
		for i := range o.changes {
			c := &o.changes[i]
			for _, f := range c.dirs {
				if !c.sure && i < upto[f] {
					c.sure, more = true, true
				}
			}
			if c.sure {
				for _, f := range c.dirs {
					upto[f] = max(upto[f], i)
				}
			}
		}
	}
}

// nameState is what the drive holds of names and owners.
type nameState struct {
	names  map[string]uint64
	owners map[uint64]int
}

// states returns every state a cut may leave: for k from 0 to the number
// of changes no SyncDir made sure, the drive's state with the sure changes
// applied, and the first k of the others.
func (o *nameOracle) states() []nameState {
	unsure := 0
	for _, c := range o.changes {
		if !c.sure {
			unsure++
		}
	}
	out := make([]nameState, 0, unsure+1)
	for k := 0; k <= unsure; k++ {
		s := nameState{names: maps.Clone(o.base), owners: maps.Clone(o.owners)}
		rank := 0
		for _, c := range o.changes {
			keep := c.sure || rank < k
			if !c.sure {
				rank++
			}
			if !keep {
				continue
			}
			for _, e := range c.sets {
				if e.ino == 0 {
					delete(s.names, e.path)
				} else {
					s.names[e.path] = e.ino
				}
			}
			if c.ino != 0 {
				s.owners[c.ino] = c.uid
			}
		}
		out = append(out, s)
	}
	return out
}

// seenFile is what Stat says of a file after a cut.
type seenFile struct {
	ino uint64
	uid int
}

// matches reports whether what a cut left is the state s. A file the
// oracle knows only as unknown matches an inode number it doesn't know.
func (s nameState) matches(got map[string]seenFile, known map[uint64]bool) bool {
	if len(got) != len(s.names) {
		return false
	}
	for p, ino := range s.names {
		g, ok := got[p]
		switch {
		case !ok, ino == unknown && known[g.ino], ino != unknown && g.ino != ino, g.uid != s.owners[ino]:
			return false
		}
	}
	return true
}

// nameRun takes a disk through random calls on names in two folders, with
// SyncDirs, file syncs, cuts and failures among them, and holds what every
// cut leaves to the oracle. It writes down what each cut left.
type nameRun struct {
	t     *testing.T
	d     *fault.Disk
	sys   fsys.FS
	r     *rand.Rand
	o     *nameOracle
	live  map[string]uint64    // the names as the calls see them
	open  map[uint64]fsys.File // an open file on each file the calls can reach
	me    int                  // the owner a new file gets
	cutIn bool                 // the power was cut in a call that made a change
	cuts  int
	seen  map[string]int
	out   strings.Builder
}

var namePaths = []string{"/a/p", "/a/q", "/a/r", "/b/p", "/b/q"}

func newNameRun(t *testing.T, diskSeed, callSeed uint64) *nameRun {
	d, sys := newDisk(t, diskSeed, "/a", "/b")
	return &nameRun{
		t: t, d: d, sys: sys, r: rand.New(rand.NewPCG(callSeed, 0x6e616d6573)),
		o:    &nameOracle{base: map[string]uint64{}, owners: map[uint64]int{}},
		live: map[string]uint64{}, open: map[uint64]fsys.File{}, me: os.Getuid(), seen: map[string]int{},
	}
}

// How a call is made.
const (
	plainCall = iota
	cutCall   // the power is cut in it
	failCall  // a rule fails it
)

// step makes one call, or cuts the power between calls.
func (n *nameRun) step() {
	how := plainCall
	switch n.r.IntN(20) {
	case 0:
		n.d.Cut()
		n.restart()
		return
	case 1:
		how = cutCall
	case 2:
		how = failCall
	}
	switch x := n.r.IntN(16); {
	case x < 4:
		n.create(how)
	case x < 7:
		n.rename(false, how)
	case x < 9:
		n.rename(true, how)
	case x < 11:
		n.remove(how)
	case x < 13:
		n.chown(how)
	case x < 15:
		n.syncDir(how)
	default:
		n.syncFile(how)
	}
}

// arm sets the rule a call made as how needs.
func (n *nameRun) arm(how int) {
	switch how {
	case cutCall:
		n.d.Add(fault.Rule{Op: fault.Any, N: 1, Cut: true})
	case failCall:
		n.d.Add(fault.Rule{Op: fault.Any, N: 1, Err: errBoom})
	}
}

// done checks what a call made as how returned, where want is what it
// gives on its own, nil when it works, and reports whether its change was
// made. After a cut in it, the disk is checked.
func (n *nameRun) done(how int, err, want error) bool {
	n.t.Helper()
	switch {
	case how == cutCall:
		mustCut(n.t, err)
	case how == failCall && !errors.Is(err, errBoom):
		n.t.Fatalf("a call a rule fails gives %v", err)
	case how == failCall:
		return false
	case want == nil && err != nil, want != nil && !errors.Is(err, want):
		n.t.Fatalf("a call gives %v, where %v was wanted", err, want)
	}
	return want == nil
}

// after notes a change a call made, then checks the disk if the power was
// cut in the call.
func (n *nameRun) after(how int, c *nameChange) {
	if c != nil {
		n.o.changes = append(n.o.changes, *c)
		for _, e := range c.sets {
			if e.ino == 0 {
				delete(n.live, e.path)
			} else {
				n.live[e.path] = e.ino
			}
		}
	}
	if how == cutCall {
		n.cutIn = c != nil
		n.restart()
	}
}

// pick returns one of xs at random, or false when there are none.
func pick[T any](r *rand.Rand, xs []T) (T, bool) {
	var zero T
	if len(xs) == 0 {
		return zero, false
	}
	return xs[r.IntN(len(xs))], true
}

func (n *nameRun) named() []string {
	return slices.Sorted(maps.Keys(n.live))
}

func (n *nameRun) create(how int) {
	var free []string
	for _, p := range namePaths {
		if _, ok := n.live[p]; !ok {
			free = append(free, p)
		}
	}
	p, ok := pick(n.r, free)
	if !ok {
		return
	}
	n.arm(how)
	f, err := n.sys.Create(p, 0o600)
	if !n.done(how, err, nil) {
		return
	}
	ino := uint64(unknown)
	if how == plainCall {
		in, err := f.Stat()
		if err != nil {
			n.t.Fatal(err)
		}
		ino = in.Ino
		n.open[ino] = f
	}
	n.after(how, &nameChange{dirs: []string{path.Dir(p)}, sets: []nameSet{{p, ino}}, ino: ino, uid: n.me})
}

func (n *nameRun) rename(noReplace bool, how int) {
	from, ok := pick(n.r, n.named())
	if !ok {
		return
	}
	to, _ := pick(n.r, slices.DeleteFunc(slices.Clone(namePaths), func(p string) bool { return p == from }))
	var want error
	if _, taken := n.live[to]; noReplace && taken {
		want = fs.ErrExist
	}
	n.arm(how)
	var err error
	if noReplace {
		err = n.sys.RenameNoReplace(from, to)
	} else {
		err = n.sys.Rename(from, to)
	}
	var c *nameChange
	if n.done(how, err, want) {
		dirs := []string{path.Dir(from)}
		if path.Dir(to) != dirs[0] {
			dirs = append(dirs, path.Dir(to))
		}
		c = &nameChange{dirs: dirs, sets: []nameSet{{from, 0}, {to, n.live[from]}}}
	}
	n.after(how, c)
}

func (n *nameRun) remove(how int) {
	p, ok := pick(n.r, n.named())
	if !ok {
		return
	}
	n.arm(how)
	var c *nameChange
	if n.done(how, n.sys.Remove(p), nil) {
		c = &nameChange{dirs: []string{path.Dir(p)}, sets: []nameSet{{p, 0}}}
	}
	n.after(how, c)
}

// chown gives one of the open files a new owner, whether a name leads to
// it or not.
func (n *nameRun) chown(how int) {
	ino, ok := pick(n.r, slices.Sorted(maps.Keys(n.open)))
	if !ok {
		return
	}
	uid := n.me + 1 + n.r.IntN(1000)
	n.arm(how)
	var c *nameChange
	if n.done(how, n.open[ino].Chown(uid, -1), nil) {
		c = &nameChange{ino: ino, uid: uid}
		for p, i := range n.live {
			if i == ino {
				c.dirs = []string{path.Dir(p)}
			}
		}
	}
	n.after(how, c)
}

func (n *nameRun) syncDir(how int) {
	dir, _ := pick(n.r, []string{"/a", "/b"})
	n.arm(how)
	if n.done(how, n.sys.SyncDir(dir), nil) && how == plainCall {
		n.o.syncDir(dir)
	}
	n.after(how, nil)
}

// syncFile syncs one of the open files, which does nothing for names.
func (n *nameRun) syncFile(how int) {
	ino, ok := pick(n.r, slices.Sorted(maps.Keys(n.open)))
	if !ok {
		return
	}
	n.arm(how)
	n.done(how, n.open[ino].Sync(), nil)
	n.after(how, nil)
}

// restart looks at what a cut left, through the machine started again,
// holds it to the oracle, and takes it as the drive's state from then on,
// with a file open on each file there.
func (n *nameRun) restart() {
	n.t.Helper()
	n.d.Clear()
	n.sys = n.d.FS()
	n.cuts++
	got := map[string]seenFile{}
	for _, dir := range []string{"/a", "/b"} {
		list, err := n.sys.List(dir)
		if err != nil {
			n.t.Fatalf("cut %d: a folder Mkdir made: %v", n.cuts, err)
		}
		for _, name := range list {
			in, err := n.sys.Stat(dir + "/" + name)
			if err != nil {
				n.t.Fatal(err)
			}
			got[dir+"/"+name] = seenFile{in.Ino, in.Uid}
		}
	}
	n.check(got)
	fmt.Fprintf(&n.out, "cut %d:", n.cuts)
	n.o.base, n.o.owners, n.o.changes = map[string]uint64{}, map[uint64]int{}, nil
	n.live, n.open, n.cutIn = map[string]uint64{}, map[uint64]fsys.File{}, false
	for _, p := range slices.Sorted(maps.Keys(got)) {
		g := got[p]
		fmt.Fprintf(&n.out, " %s %d %d", p, g.ino, g.uid)
		n.o.base[p], n.o.owners[g.ino], n.live[p] = g.ino, g.uid, g.ino
		f, err := n.sys.Open(p)
		if err != nil {
			n.t.Fatal(err)
		}
		n.open[g.ino] = f
	}
	n.out.WriteByte('\n')
}

// check holds what a cut left to the oracle's states, and counts what kind
// of outcome it was.
func (n *nameRun) check(got map[string]seenFile) {
	n.t.Helper()
	known := map[uint64]bool{}
	for _, ino := range n.o.base {
		known[ino] = true
	}
	for _, c := range n.o.changes {
		for _, e := range c.sets {
			known[e.ino] = true
		}
	}
	states := n.o.states()
	var match []int
	for k, s := range states {
		if s.matches(got, known) {
			match = append(match, k)
		}
	}
	if len(match) == 0 {
		var b strings.Builder
		for k, s := range states {
			fmt.Fprintf(&b, "\n  keeping %d: %v %v", k, s.names, s.owners)
		}
		n.t.Fatalf("cut %d left %v, which is none of the states the model allows:%s", n.cuts, got, b.String())
	}
	u, lo, hi := len(states)-1, match[0], match[len(match)-1]
	switch {
	case u == 0:
	case lo == u:
		n.seen["every change kept"]++
	case hi == 0:
		n.seen["every change lost"]++
	case lo > 0 && hi < u:
		n.seen["kept up to a point"]++
	}
	if n.cutIn && lo == u {
		n.seen["the change the power was cut in kept"]++
	}
	if n.cutIn && hi < u {
		n.seen["the change the power was cut in lost"]++
	}
	rank := 0
	for i, c := range n.o.changes {
		if c.sure {
			continue
		}
		later := slices.ContainsFunc(n.o.changes[i:], func(c nameChange) bool { return c.sure })
		if hi <= rank && later {
			n.seen["a synced change kept, an earlier one lost"]++
			break
		}
		rank++
	}
}

// TestNamesKeepTheirOrder takes disks through random calls on names in two
// folders, with SyncDirs, file syncs, cuts and failures among them, and
// holds what every cut leaves to the oracle: the drive's names and owners
// as the SyncDirs promised them, with the other changes kept in call order
// up to a point. Every kind of outcome has to come up: every change kept,
// every one lost, some kept up to a point, a synced change kept while an
// earlier one elsewhere was lost, and a change the power was cut in kept,
// and lost.
func TestNamesKeepTheirOrder(t *testing.T) {
	runs, steps := 8, 4000
	if testing.Short() {
		runs, steps = 3, 2000
	}
	seen, cuts := map[string]int{}, 0
	for run := range uint64(runs) {
		n := newNameRun(t, run, run)
		for range steps {
			n.step()
		}
		n.d.Cut()
		n.restart()
		for k, v := range n.seen {
			seen[k] += v
		}
		cuts += n.cuts
	}
	for _, want := range []string{"every change kept", "every change lost", "kept up to a point",
		"a synced change kept, an earlier one lost", "the change the power was cut in kept",
		"the change the power was cut in lost"} {
		if seen[want] == 0 {
			t.Errorf("no cut gave %q: %v", want, seen)
		}
	}
	t.Logf("%d runs of %d steps, %d cuts: %v", runs, steps, cuts, seen)
}

// TestSameSeedSameNames makes the same random calls on names on disks made
// with the same seed, which must leave the same names after every cut, and
// on disks made with other seeds, which mustn't.
func TestSameSeedSameNames(t *testing.T) {
	seen := map[string]uint64{}
	for seed := range uint64(8) {
		run := func() string {
			n := newNameRun(t, seed, 7)
			for range 1500 {
				n.step()
			}
			n.d.Cut()
			n.restart()
			fmt.Fprintf(&n.out, "%d cuts, %d failed calls, %d calls\n", n.d.Cuts(), n.d.Failed(), n.d.Calls(fault.Any, ""))
			return n.out.String()
		}
		got := run()
		if again := run(); again != got {
			t.Fatalf("seed %d left two sets of names:\n%s\nand\n%s", seed, got, again)
		}
		if other, ok := seen[got]; ok {
			t.Fatalf("seeds %d and %d left the same names", other, seed)
		}
		seen[got] = seed
		if strings.Count(got, "\n") < 50 {
			t.Fatalf("seed %d: only %d cuts", seed, strings.Count(got, "\n")-1)
		}
	}
}
