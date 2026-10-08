// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"syscall"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// The oracle knows what the model promises about each byte of a file, and
// nothing more: what reads see, and which bytes a cut must leave as they
// are. It's written apart from the disk, from Disk's comment alone. It
// knows whether a file's name is on the drive too, since a cut can take a
// file away with its name.
type oracle struct {
	files map[string]*ofile
	maybe map[string]bool // files the power was cut in the creation of
	gone  int             // files a cut took away with their names
	found int             // files found after a cut in their creation
}

type ofile struct {
	data   []byte // what reads see
	state  []byte // what's promised about each byte
	lo, hi int    // the smallest and largest sizes since the last good sync
	named  bool   // a SyncDir, or a cut, has put its name on the drive
}

// What's promised about a byte.
const (
	sure     = iota // a good sync put it on the drive, and nothing changed it since: every cut keeps it
	unsynced        // changed since the last good sync
	tainted         // changed before a sync that failed, and not since: no sync promises it any more
)

func newOracle() *oracle { return &oracle{files: map[string]*ofile{}, maybe: map[string]bool{}} }

func (f *ofile) write(off int, p []byte) {
	end := off + len(p)
	if len(p) == 0 {
		return
	}
	for len(f.data) < end {
		f.data = append(f.data, 0)
		f.state = append(f.state, unsynced)
	}
	f.hi = max(f.hi, end)
	copy(f.data[off:], p)
	for i := off; i < end; i++ {
		f.state[i] = unsynced
	}
}

func (f *ofile) truncate(size int) {
	if size < len(f.data) {
		f.data, f.state = f.data[:size], f.state[:size]
		f.lo = min(f.lo, size)
		return
	}
	for len(f.data) < size {
		f.data = append(f.data, 0)
		f.state = append(f.state, unsynced)
	}
	f.hi = max(f.hi, size)
}

func (f *ofile) sync() {
	for i, s := range f.state {
		if s == unsynced {
			f.state[i] = sure
		}
	}
	f.lo, f.hi = len(f.data), len(f.data)
}

func (f *ofile) failSync() {
	for i, s := range f.state {
		if s == unsynced {
			f.state[i] = tainted
		}
	}
}

// check reads every file after a cut, through sys, and holds it to what
// was promised: a size it had since its last good sync, and every byte that
// sync promised. A file whose name no SyncDir put on the drive may be gone,
// and then the oracle forgets it. A file whose creation the cut came in
// may be there, empty. Then what was read is what's promised from now on,
// since it's what the drive holds.
func (o *oracle) check(t testing.TB, sys fsys.FS, names []string, where string) {
	t.Helper()
	for _, name := range names {
		f := o.files[name]
		_, err := sys.Stat(name)
		there := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: %s gives %v", where, name, err)
		}
		switch {
		case f == nil && there && o.maybe[name]:
			f = &ofile{}
			o.files[name] = f
			o.found++
		case f == nil && there:
			t.Fatalf("%s: %s, which was never made, is there", where, name)
		case f == nil:
			continue
		case !there && f.named:
			t.Fatalf("%s: %s, whose name was synced, is gone", where, name)
		case !there:
			o.files[name] = nil
			o.gone++
			continue
		}
		f.named = true
		got := readAll(t, sys, name)
		if len(got) < f.lo || len(got) > f.hi {
			t.Fatalf("%s: %s is %d bytes long after the cut, and since its last good sync it was from %d to %d",
				where, name, len(got), f.lo, f.hi)
		}
		for p, s := range f.state {
			if s == sure && got[p] != f.data[p] {
				t.Fatalf("%s: %s: byte %d was synced as %#x and reads %#x after the cut", where, name, p, f.data[p], got[p])
			}
		}
		f.data, f.state = bytes.Clone(got), make([]byte, len(got))
		f.lo, f.hi = len(got), len(got)
	}
	clear(o.maybe)
}

// synced marks every file's name as on the drive, after a SyncDir of "/",
// where they all are.
func (o *oracle) synced() {
	for _, f := range o.files {
		if f != nil {
			f.named = true
		}
	}
}

// unnamed reports whether a file's name is waiting for a SyncDir.
func (o *oracle) unnamed() bool {
	for _, f := range o.files {
		if f != nil && !f.named {
			return true
		}
	}
	return false
}

var names = []string{"/a", "/b", "/c"}

// A step is one call, and what the oracle makes of it.
type step struct {
	do   int
	file int
	off  int64  // where a write goes
	size int64  // where a truncate cuts
	data []byte // what a write writes
}

const (
	doCreate    = iota
	doSyncDir   // SyncDir of "/", which follows each creation
	doWrite     // WriteAt
	doTruncate  // Truncate
	doSync      // Sync, which works
	doFailSync  // Sync, which a rule fails
	doFailWrite // WriteAt, which a rule fails
	doRead      // ReadAt of the whole file, held to the oracle
	doCut       // the power is cut between calls
	doCutNext   // the power is cut in the next call
)

// world runs steps on a disk and keeps the oracle in line with them.
type world struct {
	t     testing.TB
	d     *fault.Disk
	sys   fsys.FS
	files []fsys.File
	o     *oracle
}

func newWorld(t testing.TB, d *fault.Disk) *world {
	return &world{t: t, d: d, sys: d.FS(), files: make([]fsys.File, len(names)), o: newOracle()}
}

// do makes the step's call, and reports whether the power was cut in it.
func (w *world) do(s step) bool {
	t := w.t
	name := names[s.file]
	f := w.files[s.file]
	of := w.o.files[name]
	var err error
	switch s.do {
	case doCreate:
		w.files[s.file], err = w.sys.Create(name, 0o644)
		switch {
		case err == nil:
			w.o.files[name] = &ofile{}
		case errors.Is(err, fault.ErrCut):
			w.o.maybe[name] = true
		}
	case doSyncDir:
		if err = w.sys.SyncDir("/"); err == nil {
			w.o.synced()
		}
	case doWrite:
		_, err = f.WriteAt(s.data, s.off)
		if err == nil || errors.Is(err, fault.ErrCut) {
			of.write(int(s.off), s.data)
		}
	case doFailWrite:
		w.d.Add(fault.Rule{Op: fault.WriteAt, Path: name, N: 1, Err: syscall.ENOSPC})
		var n int
		n, err = f.WriteAt(s.data, s.off)
		switch {
		case errors.Is(err, fault.ErrCut):
			of.write(int(s.off), s.data)
		case !errors.Is(err, syscall.ENOSPC) || n >= len(s.data):
			t.Fatalf("a write that a rule fails gives %d bytes and %v", n, err)
		default:
			of.write(int(s.off), s.data[:n])
			err = nil
		}
	case doTruncate:
		err = f.Truncate(s.size)
		if err == nil || errors.Is(err, fault.ErrCut) {
			of.truncate(int(s.size))
		}
	case doSync:
		if err = f.Sync(); err == nil {
			of.sync()
		}
	case doFailSync:
		w.d.Add(fault.Rule{Op: fault.Sync, Path: name, N: 1})
		err = f.Sync()
		switch {
		case errors.Is(err, fault.ErrCut):
		case !errors.Is(err, syscall.EIO):
			t.Fatalf("a sync that a rule fails gives %v", err)
		default:
			of.failSync()
			err = nil
		}
	case doRead:
		b := make([]byte, len(of.data)+1)
		var n int
		n, err = f.ReadAt(b, 0)
		if !errors.Is(err, fault.ErrCut) {
			if err != io.EOF || !bytes.Equal(b[:n], of.data) {
				t.Fatalf("%s reads %d bytes and %v, where %d were written", name, n, err, len(of.data))
			}
			err = nil
		}
	case doCut:
		w.d.Cut()
		return true
	case doCutNext:
		w.d.Add(fault.Rule{Op: fault.Any, N: 1, Cut: true})
		return false
	}
	if errors.Is(err, fault.ErrCut) {
		return true
	}
	if err != nil {
		t.Fatalf("step %+v: %v", s, err)
	}
	return false
}

// restart checks every file after a cut, and opens them again on the
// machine started again. A rule still waiting for its call, when the power
// was cut between calls, goes with the cut.
func (w *world) restart(where string) {
	w.d.Clear()
	w.sys = w.d.FS()
	w.o.check(w.t, w.sys, names, where)
	for i, name := range names {
		w.files[i] = nil
		if w.o.files[name] != nil {
			f, err := w.sys.Open(name)
			if err != nil {
				w.t.Fatal(err)
			}
			w.files[i] = f
		}
	}
}

// next picks a step at random for the files as the oracle has them. Only
// with all set does it pick failed writes and cuts. A creation is followed
// by a SyncDir, as the engine syncs its folder, chosen without drawing from
// r, so the other steps are the ones T1 picked.
func (w *world) next(r *rand.Rand, all bool) step {
	if w.o.unnamed() {
		return step{do: doSyncDir}
	}
	for {
		s := step{file: r.IntN(len(names))}
		of := w.o.files[names[s.file]]
		if of == nil {
			s.do = doCreate
			return s
		}
		size := int64(len(of.data))
		switch x := r.IntN(40); {
		case x < 16, x < 18 && all:
			s.do = doWrite
			if x >= 16 {
				s.do = doFailWrite
			}
			l := 1 + r.IntN(1500)
			switch y := r.IntN(10); {
			case y < 6 || size == 0:
				s.off = size
			case y < 9:
				s.off = r.Int64N(size)
			default:
				s.off = size + 1 + r.Int64N(1000)
			}
			s.data = randomBytes(r, l)
		case x < 21:
			s.do = doTruncate
			if size > 0 && r.IntN(3) > 0 {
				s.size = r.Int64N(size)
			} else {
				s.size = size + r.Int64N(2000)
			}
		case x < 31:
			s.do = doSync
		case x < 35:
			s.do = doFailSync
		case x < 38:
			s.do = doRead
		case !all:
			continue
		case x == 38:
			s.do = doCut
		default:
			s.do = doCutNext
		}
		return s
	}
}

// workload returns a fixed list of steps, each one call, starting with the
// three files' creation, each followed by a SyncDir.
func workload(seed uint64, n int) []step {
	r := rand.New(rand.NewPCG(seed, 0x7431))
	w := &world{o: newOracle()}
	var steps []step
	for len(steps) < n {
		s := w.next(r, false)
		switch s.do {
		case doCreate:
			w.o.files[names[s.file]] = &ofile{}
		case doSyncDir:
			w.o.synced()
		case doWrite:
			w.o.files[names[s.file]].write(int(s.off), s.data)
		case doTruncate:
			w.o.files[names[s.file]].truncate(int(s.size))
		}
		steps = append(steps, s)
	}
	return steps
}

// TestSyncedDataSurvivesEveryCut is T1's closing test. Fixed workloads of
// writes, truncates, syncs that work, syncs a rule fails and reads, on
// three files, run once for each call with the power cut in that call, and
// again with it cut after the last, with several seeds each time. After
// every cut each file must have a size it had since its last good sync,
// and every byte that sync put on the drive, with nothing written over it
// since, must read back as it was. Since T2, each file's creation is
// followed by a SyncDir, and a cut before the SyncDir returns may take the
// file away.
func TestSyncedDataSurvivesEveryCut(t *testing.T) {
	workloads, seeds := 8, 6
	if testing.Short() {
		workloads, seeds = 3, 2
	}
	cuts, gone, found := 0, 0, 0
	for wl := range workloads {
		steps := workload(uint64(wl), 90)
		for i := 1; i <= len(steps)+1; i++ {
			for seed := range seeds {
				d := fault.New(uint64(seed))
				if i <= len(steps) {
					d.Add(fault.Rule{Op: fault.Any, N: i, Cut: true})
				}
				w := newWorld(t, d)
				cut := false
				for _, s := range steps {
					if cut = w.do(s); cut {
						break
					}
				}
				if i > len(steps) {
					if cut {
						t.Fatal("the power was cut with no rule to cut it")
					}
					if got := d.Calls(fault.Any, ""); got != len(steps) {
						t.Fatalf("%d steps made %d calls", len(steps), got)
					}
					d.Cut()
				} else if !cut {
					t.Fatalf("workload %d: no cut in call %d", wl, i)
				}
				w.restart(fmt.Sprintf("workload %d, cut in call %d, seed %d", wl, i, seed))
				cuts++
				gone, found = gone+w.o.gone, found+w.o.found
			}
		}
	}
	if !testing.Short() && (gone == 0 || found == 0) {
		t.Errorf("no cut took a file away with its name (%d), or left one it came in the creation of (%d)", gone, found)
	}
	t.Logf("%d cuts, and synced data survived every one; %d files went with their names, and %d were found after a cut in their creation",
		cuts, gone, found)
}

// TestSyncedDataSurvivesLongRuns takes a disk through thousands of random
// steps, with failed writes and syncs, and the power cut now and then,
// between calls or in one, so a run goes through hundreds of cuts with
// what earlier ones left.
func TestSyncedDataSurvivesLongRuns(t *testing.T) {
	runs, steps := 6, 6000
	if testing.Short() {
		runs, steps = 2, 2000
	}
	total, failed, gone := 0, 0, 0
	for run := range runs {
		r := rand.New(rand.NewPCG(uint64(run), 0x6c6f6e67))
		d := fault.New(uint64(run))
		w := newWorld(t, d)
		cuts := 0
		for i := range steps {
			if w.do(w.next(r, true)) {
				cuts++
				w.restart(fmt.Sprintf("run %d, step %d", run, i))
			}
		}
		if cuts < steps/100 {
			t.Errorf("run %d: %d cuts in %d steps", run, cuts, steps)
		}
		total, failed, gone = total+cuts, failed+d.Failed(), gone+w.o.gone
	}
	t.Logf("%d runs of %d steps: %d cuts and %d failed calls, and synced data survived every cut; %d files went with their names",
		runs, steps, total, failed, gone)
}

// TestUnsyncedDataLostOrTorn writes over a synced file and past its end,
// cuts the power, and holds every sector to the model: one that no change
// touched as the drive held it, and one that a change touched kept, lost,
// or torn at one point. Over many seeds every fate has to come up, both
// ways of tearing, sizes old, new and between, and files kept whole, lost
// whole and mixed.
func TestUnsyncedDataLostOrTorn(t *testing.T) {
	seen := map[string]int{}
	for seed := range uint64(600) {
		d := fault.New(seed)
		sys := d.FS()
		f := mustCreate(t, sys, "/f", 0o644)
		r := rand.New(rand.NewPCG(seed, 1))
		synced := randomBytes(r, 3000)
		mustWrite(t, f, synced, 0)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		over, past := randomBytes(r, 1200), randomBytes(r, 1100)
		mustWrite(t, f, over, 700)
		mustWrite(t, f, past, 3000)
		seenBytes := append(append(append([]byte{}, synced[:700]...), over...), synced[1900:]...)
		seenBytes = append(seenBytes, past...)
		d.Cut()

		got := readAll(t, d.FS(), "/f")
		classify(t, seen, fmt.Sprintf("seed %d", seed), got, synced, seenBytes, 3000, 4100, sectorsOf(700, 1900, 3000, 4100))
	}
	for _, want := range []string{"kept", "lost", "torn, new first", "torn, old first",
		"size as synced", "size as seen", "size between", "file kept", "file lost", "file mixed"} {
		if seen[want] == 0 {
			t.Errorf("no seed gave %q: %v", want, seen)
		}
	}
	t.Log(seen)
}

// sectorsOf returns the sectors holding the bytes of each range, given as
// pairs of a start and an end.
func sectorsOf(ranges ...int) map[int]bool {
	s := map[int]bool{}
	for i := 0; i < len(ranges); i += 2 {
		for x := ranges[i] / fault.SectorSize; x*fault.SectorSize < ranges[i+1]; x++ {
			s[x] = true
		}
	}
	return s
}

// classify holds what a file reads after a cut to the model, given what
// the drive held and what reads saw before it, the size its last sync left
// and the size reads saw, and which sectors changes touched since. It
// counts each fate it finds in seen.
func classify(t *testing.T, seen map[string]int, where string, got, held, saw []byte, syncedSize, seenSize int, pending map[int]bool) {
	t.Helper()
	switch n := len(got); {
	case n < min(syncedSize, seenSize) || n > max(syncedSize, seenSize):
		t.Fatalf("%s: %d bytes after the cut, from sizes %d and %d", where, n, syncedSize, seenSize)
	case n == syncedSize:
		seen["size as synced"]++
	case n == seenSize:
		seen["size as seen"]++
	default:
		seen["size between"]++
	}
	held, saw = padded(held, len(got)), padded(saw, len(got))
	fates := map[string]int{}
	for start := 0; start < len(got); start += fault.SectorSize {
		end := min(start+fault.SectorSize, len(got))
		fate := fateOf(got[start:end], held[start:end], saw[start:end])
		switch {
		case !pending[start/fault.SectorSize]:
			if !bytes.Equal(got[start:end], held[start:end]) {
				t.Fatalf("%s: sector %d, which nothing changed, isn't as the drive held it", where, start/fault.SectorSize)
			}
		case fate == "":
			t.Fatalf("%s: sector %d is neither kept nor lost, nor torn at one point", where, start/fault.SectorSize)
		case bytes.Equal(held[start:end], saw[start:end]):
		default:
			seen[fate]++
			fates[fate]++
		}
	}
	switch {
	case len(fates) == 1 && fates["kept"] > 1:
		seen["file kept"]++
	case len(fates) == 1 && fates["lost"] > 1:
		seen["file lost"]++
	case len(fates) > 1:
		seen["file mixed"]++
	}
}

func mustWrite(t testing.TB, f fsys.File, p []byte, off int64) {
	t.Helper()
	if n, err := f.WriteAt(p, off); n != len(p) || err != nil {
		t.Fatalf("wrote %d of %d bytes at %d: %v", n, len(p), off, err)
	}
}

// TestCutInAWrite cuts the power in a WriteAt. The call fails with ErrCut,
// and the write is as pending at the cut as one that returned: any of its
// sectors kept, lost or torn, and the size anywhere from before it to
// after it.
func TestCutInAWrite(t *testing.T) {
	seen := map[string]int{}
	for seed := range uint64(300) {
		d := fault.New(seed)
		sys := d.FS()
		f := mustCreate(t, sys, "/f", 0o644)
		r := rand.New(rand.NewPCG(seed, 2))
		synced := randomBytes(r, 1000)
		mustWrite(t, f, synced, 0)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		d.Add(fault.Rule{Op: fault.WriteAt, N: 1, Cut: true})
		past := randomBytes(r, 2000)
		n, err := f.WriteAt(past, 1000)
		mustCut(t, err)
		if n != 0 {
			t.Fatalf("a write the power was cut in returns %d", n)
		}
		if d.Cuts() != 1 {
			t.Fatalf("%d cuts", d.Cuts())
		}
		got := readAll(t, d.FS(), "/f")
		classify(t, seen, fmt.Sprintf("seed %d", seed), got, synced, append(bytes.Clone(synced), past...), 1000, 3000, sectorsOf(1000, 3000))
	}
	for _, want := range []string{"kept", "lost", "torn, new first", "torn, old first", "size as synced", "size as seen", "size between"} {
		if seen[want] == 0 {
			t.Errorf("no seed gave %q: %v", want, seen)
		}
	}
}

// TestCutInATruncate cuts the power in a Truncate that shortens a synced
// file. The bytes it cut off may come back, as the drive held them, or
// read as zeros up to whatever size the file ends at, and the bytes before
// the new end stay as they were.
func TestCutInATruncate(t *testing.T) {
	sizes := map[int]bool{}
	for seed := range uint64(200) {
		d := fault.New(seed)
		sys := d.FS()
		f := mustCreate(t, sys, "/f", 0o644)
		synced := randomBytes(rand.New(rand.NewPCG(seed, 3)), 3000)
		mustWrite(t, f, synced, 0)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		d.Add(fault.Rule{Op: fault.Truncate, N: 1, Cut: true})
		mustCut(t, f.Truncate(1300))
		got := readAll(t, d.FS(), "/f")
		if len(got) < 1300 || len(got) > 3000 {
			t.Fatalf("seed %d: %d bytes", seed, len(got))
		}
		if !bytes.Equal(got[:1300], synced[:1300]) {
			t.Fatalf("seed %d: the bytes before the new end changed", seed)
		}
		for p := 1300; p < len(got); p++ {
			if got[p] != 0 && got[p] != synced[p] {
				t.Fatalf("seed %d: byte %d reads %#x, which is neither zero nor what was synced", seed, p, got[p])
			}
		}
		sizes[len(got)] = true
	}
	if !sizes[1300] || !sizes[3000] || len(sizes) < 3 {
		t.Errorf("sizes after the cut: %v", sizes)
	}
}

// TestCutKillsTheProcess checks what a cut does to the processes running
// then: every call through their FS, or on files they opened, fails with
// ErrCut and never reaches the disk; their flocks are gone; synced names
// stay. FS gives the machine started again.
func TestCutKillsTheProcess(t *testing.T) {
	d := fault.New(1)
	sys := d.FS()
	f := mustCreate(t, sys, "/f", 0o600)
	if ok, err := f.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	mustWrite(t, f, []byte("synced"), 0)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	d.Add(fault.Rule{Op: fault.Stat, N: 1, Cut: true})
	_, err := sys.Stat("/f")
	mustCut(t, err)
	if pathOf(err) != "/f" {
		t.Errorf("the cut call's error names %q", pathOf(err))
	}
	calls := d.Calls(fault.Any, "")

	_, err = f.ReadAt(make([]byte, 1), 0)
	mustCut(t, err)
	_, err = f.WriteAt([]byte("x"), 0)
	mustCut(t, err)
	mustCut(t, f.Sync())
	mustCut(t, f.Truncate(0))
	_, err = f.Stat()
	mustCut(t, err)
	mustCut(t, f.Chown(1, 1))
	_, err = f.TryLock()
	mustCut(t, err)
	mustCut(t, f.Unlock())
	mustCut(t, f.Close())
	_, err = sys.Open("/f")
	mustCut(t, err)
	_, err = sys.Create("/g", 0o600)
	mustCut(t, err)
	mustCut(t, sys.Rename("/f", "/g"))
	mustCut(t, sys.RenameNoReplace("/f", "/g"))
	mustCut(t, sys.Remove("/f"))
	mustCut(t, sys.SyncDir("/"))
	_, err = sys.Stat("/f")
	mustCut(t, err)
	_, err = sys.RealPath("/f")
	mustCut(t, err)
	_, err = sys.List("/")
	mustCut(t, err)
	if got := d.Calls(fault.Any, ""); got != calls {
		t.Errorf("calls from a dead process counted: %d, then %d", calls, got)
	}
	if d.Cuts() != 1 {
		t.Errorf("%d cuts", d.Cuts())
	}

	sys = d.FS()
	if got := readAll(t, sys, "/f"); string(got) != "synced" {
		t.Errorf("after the cut /f holds %q", got)
	}
	g, err := sys.Open("/f")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := g.TryLock(); !ok || err != nil {
		t.Errorf("the flock outlived the cut: %v %v", ok, err)
	}
	if list, err := sys.List("/"); err != nil || len(list) != 1 || list[0] != "f" {
		t.Errorf("after the cut / holds %q, %v", list, err)
	}
}
