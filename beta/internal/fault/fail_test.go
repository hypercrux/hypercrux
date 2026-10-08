// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"slices"
	"syscall"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// A logged call: its kind, the paths it names and what it returned.
type call struct {
	op    fault.Op
	paths []string
	err   error
}

// script makes calls of every kind, in three rounds, and logs each one. It
// stops at the first that fails, since what follows a failure is the
// caller's business.
func script(sys fsys.FS) []call {
	var log []call
	ok := func(op fault.Op, err error, paths ...string) bool {
		log = append(log, call{op, paths, err})
		return err == nil
	}
	for round := range 3 {
		a, b, c := fmt.Sprintf("/a%d", round), fmt.Sprintf("/b%d", round), fmt.Sprintf("/c%d", round)
		f, err := sys.Create(a, 0o640)
		if !ok(fault.Create, err, a) {
			return log
		}
		g, err := sys.Open(a)
		if !ok(fault.Open, err, a) {
			return log
		}
		_, err = f.WriteAt([]byte("round"), 0)
		if !ok(fault.WriteAt, err, a) {
			return log
		}
		_, err = g.ReadAt(make([]byte, 5), 0)
		if !ok(fault.ReadAt, err, a) ||
			!ok(fault.Sync, f.Sync(), a) ||
			!ok(fault.Truncate, f.Truncate(3), a) {
			return log
		}
		_, err = f.Stat()
		if !ok(fault.Fstat, err, a) || !ok(fault.Chown, f.Chown(-1, -1), a) {
			return log
		}
		_, err = f.TryLock()
		if !ok(fault.TryLock, err, a) ||
			!ok(fault.Unlock, f.Unlock(), a) ||
			!ok(fault.SyncDir, sys.SyncDir("/"), "/") {
			return log
		}
		_, err = sys.Stat(a)
		if !ok(fault.Stat, err, a) {
			return log
		}
		_, err = sys.RealPath(a)
		if !ok(fault.RealPath, err, a) {
			return log
		}
		_, err = sys.List("/")
		if !ok(fault.List, err, "/") ||
			!ok(fault.Rename, sys.Rename(a, b), a, b) ||
			!ok(fault.RenameNoReplace, sys.RenameNoReplace(b, c), b, c) ||
			!ok(fault.Remove, sys.Remove(c), c) ||
			!ok(fault.Close, f.Close(), a) ||
			!ok(fault.Close, g.Close(), a) {
			return log
		}
	}
	return log
}

var errBoom = errors.New("boom")

// TestEveryCallFailsAtItsNthUse runs the script once to count its calls,
// then once for each call of each kind, with a rule failing that call: by
// its kind across every path, by its kind on one path, and by its place
// among all calls. The call picked, and no other, must fail, with the
// rule's error wrapped with its path.
func TestEveryCallFailsAtItsNthUse(t *testing.T) {
	ref := fault.New(0)
	want := script(ref.FS())
	if len(want) != 3*19 {
		t.Fatalf("the script made %d calls", len(want))
	}
	for _, c := range want {
		if c.err != nil {
			t.Fatalf("the script fails on its own: %v", c.err)
		}
	}
	tried := 0
	try := func(r fault.Rule) {
		t.Helper()
		var at []int // where the matching calls are in the script
		for i, c := range want {
			if (r.Op == fault.Any || c.op == r.Op) && (r.Path == "" || slices.Contains(c.paths, path.Clean("/"+r.Path))) {
				at = append(at, i)
			}
		}
		if len(at) == 0 {
			t.Fatalf("no call matches %+v", r)
		}
		if got := ref.Calls(r.Op, r.Path); got != len(at) {
			t.Fatalf("Calls(%v, %q) is %d, and the script made %d", r.Op, r.Path, got, len(at))
		}
		for n := 1; n <= len(at); n++ {
			d := fault.New(uint64(n))
			r.N, r.Err = n, errBoom
			d.Add(r)
			got := script(d.FS())
			i := at[n-1]
			if len(got) != i+1 || got[i].op != want[i].op {
				t.Fatalf("%+v: the script stopped after %d calls, where call %d should fail", r, len(got), i+1)
			}
			if err := got[i].err; !errors.Is(err, errBoom) || pathOf(err) != want[i].paths[0] {
				t.Fatalf("%+v: call %d gave %v", r, i+1, err)
			}
			if d.Failed() != 1 {
				t.Fatalf("%+v: %d calls failed", r, d.Failed())
			}
			tried++
		}
	}
	for op := fault.ReadAt; op <= fault.List; op++ {
		try(fault.Rule{Op: op})
		if op != fault.SyncDir && op != fault.List && op != fault.RenameNoReplace && op != fault.Remove {
			try(fault.Rule{Op: op, Path: "/a1"})
		}
	}
	try(fault.Rule{Op: fault.Any})
	try(fault.Rule{Op: fault.Any, Path: "b2"})
	try(fault.Rule{Op: fault.RenameNoReplace, Path: "/c0"})
	t.Logf("%d calls failed, one at a time", tried)
}

// TestRuleTimes checks a rule that fails several calls in a row, one that
// fails every call from its nth on, a cut that comes once, rules counting
// from when they're set, a rule's default error, and Clear.
func TestRuleTimes(t *testing.T) {
	writes := func(d *fault.Disk, n int) (failed []int) {
		f, err := d.FS().Create(fmt.Sprintf("/f%d", d.Calls(fault.Create, "")), 0o600)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= n; i++ {
			if _, err := f.WriteAt([]byte("x"), int64(i)); err != nil {
				if !errors.Is(err, syscall.EIO) {
					t.Fatalf("write %d: %v", i, err)
				}
				failed = append(failed, i)
			}
		}
		return failed
	}
	d := fault.New(1)
	writes(d, 3)
	d.Add(fault.Rule{Op: fault.WriteAt, N: 2, Times: 3})
	if got := writes(d, 7); !slices.Equal(got, []int{2, 3, 4}) {
		t.Errorf("with Times 3 from the 2nd, writes %v failed", got)
	}
	d = fault.New(1)
	d.Add(fault.Rule{Op: fault.WriteAt, N: 3, Times: -1})
	if got := writes(d, 6); !slices.Equal(got, []int{3, 4, 5, 6}) {
		t.Errorf("with Times -1 from the 3rd, writes %v failed", got)
	}
	d.Clear()
	if got := writes(d, 3); got != nil {
		t.Errorf("after Clear, writes %v failed", got)
	}
	if d.Failed() != 4 {
		t.Errorf("Failed is %d", d.Failed())
	}

	d = fault.New(1)
	d.Add(fault.Rule{Op: fault.WriteAt, N: 2, Times: 5, Cut: true})
	f := mustCreate(t, d.FS(), "/f", 0o600)
	mustWrite(t, f, []byte("x"), 0)
	_, err := f.WriteAt([]byte("x"), 1)
	mustCut(t, err)
	f, err = d.FS().Open("/f")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		mustWrite(t, f, []byte("x"), int64(i))
	}
	if d.Cuts() != 1 {
		t.Errorf("a cut with Times 5 came %d times", d.Cuts())
	}

	for _, bad := range []fault.Rule{{Op: fault.Sync}, {Op: fault.Op(-1), N: 1}, {Op: fault.List + 1, N: 1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Add took %+v", bad)
				}
			}()
			fault.New(1).Add(bad)
		}()
	}
}

// TestFailedCallsChangeNothing checks what a failed call leaves behind:
// nothing, apart from the prefix a failed write writes and a failed read
// reads, and a failed Close closing the file all the same.
func TestFailedCallsChangeNothing(t *testing.T) {
	setup := func(seed uint64) (*fault.Disk, fsys.FS, fsys.File) {
		d := fault.New(seed)
		sys := d.FS()
		f, err := sys.Create("/f", 0o600)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, f, []byte("0123456789"), 0)
		return d, sys, f
	}

	prefixes := map[int]bool{}
	for seed := range uint64(100) {
		d, sys, f := setup(seed)
		d.Add(fault.Rule{Op: fault.WriteAt, N: 1})
		n, err := f.WriteAt([]byte("abcdefghij"), 5)
		if !errors.Is(err, syscall.EIO) || n < 0 || n >= 10 {
			t.Fatalf("a failed write gave %d and %v", n, err)
		}
		want := padded([]byte("0123456789"), max(10, 5+n))
		copy(want[5:], []byte("abcdefghij")[:n])
		if got := readAll(t, sys, "/f"); !bytes.Equal(got, want) {
			t.Fatalf("after a failed write of %d bytes the file holds %q", n, got)
		}
		prefixes[n] = true

		d.Add(fault.Rule{Op: fault.ReadAt, N: 1})
		b := make([]byte, 8)
		n, err = f.ReadAt(b, 1)
		if !errors.Is(err, syscall.EIO) || n < 0 || n >= 8 || !bytes.Equal(b[:n], want[1:1+n]) || !bytes.Equal(b[n:], make([]byte, 8-n)) {
			t.Fatalf("a failed read gave %d, %q and %v", n, b, err)
		}
	}
	if !prefixes[0] || len(prefixes) < 8 {
		t.Errorf("failed writes wrote %v bytes", prefixes)
	}

	d, sys, f := setup(1)
	d.Add(fault.Rule{Op: fault.Any, N: 1, Times: -1, Err: errBoom})
	in := func() fsys.Info {
		d.Clear()
		defer d.Add(fault.Rule{Op: fault.Any, N: 1, Times: -1, Err: errBoom})
		i, err := sys.Stat("/f")
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	before := in()
	for _, err := range []error{f.Truncate(2), f.Chown(4, 5), f.Sync(), sys.Rename("/f", "/g"),
		sys.RenameNoReplace("/f", "/g"), sys.Remove("/f"), sys.SyncDir("/")} {
		if !errors.Is(err, errBoom) {
			t.Fatalf("a call gave %v", err)
		}
	}
	if _, err := sys.Create("/g", 0o600); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if _, err := sys.Open("/f"); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if after := in(); after != before {
		t.Errorf("failed calls changed the file from %+v to %+v", before, after)
	}
	d.Clear()
	for _, name := range []string{"/g"} {
		if _, err := sys.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a failed call left %s: %v", name, err)
		}
	}

	// A failed TryLock takes nothing, a failed Unlock lets go of nothing,
	// and a failed Close closes the file and lets go of its lock.
	g, err := sys.Open("/f")
	if err != nil {
		t.Fatal(err)
	}
	d.Add(fault.Rule{Op: fault.TryLock, N: 1})
	if ok, err := f.TryLock(); ok || !errors.Is(err, syscall.EIO) {
		t.Fatalf("a failed TryLock gave %v and %v", ok, err)
	}
	if ok, err := g.TryLock(); !ok || err != nil {
		t.Fatalf("after a failed TryLock elsewhere, TryLock gave %v and %v", ok, err)
	}
	d.Add(fault.Rule{Op: fault.Unlock, N: 1})
	if err := g.Unlock(); !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	if ok, err := f.TryLock(); ok || err != nil {
		t.Fatalf("a failed Unlock let go: %v and %v", ok, err)
	}
	d.Add(fault.Rule{Op: fault.Close, N: 1})
	if err := g.Close(); !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	if ok, err := f.TryLock(); !ok || err != nil {
		t.Fatalf("a failed Close kept the lock: %v and %v", ok, err)
	}
	if _, err := g.Stat(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("after a failed Close, Stat gives %v", err)
	}
	if err := g.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("a second Close gives %v", err)
	}

	// The errors wrap the rule's error with the call's name and its path.
	d.Add(fault.Rule{Op: fault.Sync, N: 1, Err: syscall.ENOSPC})
	err = f.Sync()
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Op != "fdatasync" || pe.Path != "/f" || pe.Err != syscall.ENOSPC {
		t.Errorf("a failed Sync gives %#v", err)
	}
	d.Add(fault.Rule{Op: fault.Rename, N: 1})
	err = sys.Rename("f", "g")
	var le *os.LinkError
	if !errors.As(err, &le) || le.Op != "rename" || le.Old != "/f" || le.New != "/g" || le.Err != syscall.EIO {
		t.Errorf("a failed Rename gives %#v", err)
	}
}

// TestFailedSync checks the failed sync's model. Bytes synced before it
// survive. Bytes it failed to sync can be lost at a cut even after a later
// sync succeeds, as on Linux, where a failed sync can mark pages clean,
// unless they're written again first, as beta/FORMAT.md does with a batch.
// And the size stays pending through the failure.
func TestFailedSync(t *testing.T) {
	seen := map[string]int{}
	for seed := range uint64(400) {
		r := rand.New(rand.NewPCG(seed, 4))
		synced, unsynced := randomBytes(r, 1024), randomBytes(r, 2048)
		for _, again := range []bool{false, true} {
			for _, later := range []bool{false, true} {
				d := fault.New(seed)
				sys := d.FS()
				f := mustCreate(t, sys, "/f", 0o600)
				mustWrite(t, f, synced, 0)
				if err := f.Sync(); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, f, unsynced, 1024)
				d.Add(fault.Rule{Op: fault.Sync, N: 1})
				if err := f.Sync(); !errors.Is(err, syscall.EIO) {
					t.Fatalf("the sync gave %v", err)
				}
				b := make([]byte, 3072)
				if n, err := f.ReadAt(b, 0); n != 3072 || err != nil || !bytes.Equal(b[1024:], unsynced) {
					t.Fatalf("after the failed sync, reads give %d bytes and %v", n, err)
				}
				if again {
					mustWrite(t, f, unsynced, 1024)
				}
				if later {
					if err := f.Sync(); err != nil {
						t.Fatal(err)
					}
				}
				d.Cut()
				got := readAll(t, d.FS(), "/f")
				where := fmt.Sprintf("seed %d, written again %v, synced later %v", seed, again, later)
				switch {
				case len(got) < 1024 || len(got) > 3072:
					t.Fatalf("%s: %d bytes", where, len(got))
				case later && len(got) != 3072:
					t.Fatalf("%s: %d bytes after a good sync of 3072", where, len(got))
				case !bytes.Equal(got[:1024], synced):
					t.Fatalf("%s: the bytes synced before the failure changed", where)
				case again && later && !bytes.Equal(got[1024:], unsynced):
					t.Fatalf("%s: the bytes written again and synced were lost", where)
				}
				if again {
					continue
				}
				fates := map[string]bool{}
				saw, held := padded(unsynced, len(got)-1024), make([]byte, len(got)-1024)
				for s := 0; s < len(got)-1024; s += fault.SectorSize {
					e := min(s+fault.SectorSize, len(got)-1024)
					fate := fateOf(got[1024+s:1024+e], held[s:e], saw[s:e])
					if fate == "" {
						t.Fatalf("%s: sector %d is neither kept nor lost, nor torn at one point", where, 2+s/fault.SectorSize)
					}
					fates[fate] = true
				}
				switch {
				case later && len(fates) == 1 && fates["lost"]:
					seen["all lost after a later good sync"]++
				case later && len(fates) == 1 && fates["kept"]:
					seen["all kept after a later good sync"]++
				case later:
					seen["mixed after a later good sync"]++
				case len(got) == 1024:
					seen["size as synced"]++
				case len(got) == 3072:
					seen["size as written"]++
				}
			}
		}
	}
	for _, want := range []string{"all lost after a later good sync", "all kept after a later good sync",
		"mixed after a later good sync", "size as synced", "size as written"} {
		if seen[want] == 0 {
			t.Errorf("no seed gave %q: %v", want, seen)
		}
	}
	t.Log(seen)
}
