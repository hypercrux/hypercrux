// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// held returns what the file at name holds, or "missing" when there's no
// file there.
func held(t testing.TB, sys fsys.FS, name string) string {
	t.Helper()
	_, err := sys.Stat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "missing"
	case err != nil:
		t.Fatal(err)
	}
	return string(readAll(t, sys, name))
}

// newFile makes a file at name holding s, with its name and its data
// synced, so it lasts every cut, and returns it open.
func newFile(t testing.TB, sys fsys.FS, name, s string) fsys.File {
	t.Helper()
	f := mustCreate(t, sys, name, 0o640)
	mustWrite(t, f, []byte(s), 0)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	return f
}

// mustSyncDir syncs the folder dir.
func mustSyncDir(t testing.TB, sys fsys.FS, dir string) {
	t.Helper()
	if err := sys.SyncDir(dir); err != nil {
		t.Fatal(err)
	}
}

// newDisk returns a disk made with seed, holding the folders dirs, which
// last every cut.
func newDisk(t testing.TB, seed uint64, dirs ...string) (*fault.Disk, fsys.FS) {
	t.Helper()
	d := fault.New(seed)
	for _, dir := range dirs {
		if err := d.Mkdir(dir); err != nil {
			t.Fatal(err)
		}
	}
	return d, d.FS()
}

// both checks that seen counts each of a and b at least once, and logs
// it.
func both(t *testing.T, where string, seen map[string]int, a, b string) {
	t.Helper()
	if seen[a] == 0 || seen[b] == 0 {
		t.Errorf("%s: %q came up %d times and %q %d times: %v", where, a, seen[a], b, seen[b], seen)
	}
	t.Logf("%s: %v", where, seen)
}

// TestARenameVanishesWithoutASyncDir is T2's closing test. A file is
// renamed over another, as compaction switches files, or to a free name
// with RenameNoReplace, as a database is created, and then the power is
// cut, with the folder synced after the rename or not. Both files' data
// and names were synced before the rename. Without the SyncDir the rename
// vanishes in some seeds and lasts in others; with it, it lasts in every
// seed. Either way each name holds one whole file.
func TestARenameVanishesWithoutASyncDir(t *testing.T) {
	const seeds = 200
	for _, op := range []fault.Op{fault.Rename, fault.RenameNoReplace} {
		for _, synced := range []bool{false, true} {
			seen := map[string]int{}
			for seed := range uint64(seeds) {
				d, sys := newDisk(t, seed, "/d")
				old := "missing"
				if op == fault.Rename {
					old = "old"
					newFile(t, sys, "/d/db", old)
				}
				newFile(t, sys, "/d/db.next", "new")
				rename := sys.Rename
				if op == fault.RenameNoReplace {
					rename = sys.RenameNoReplace
				}
				if err := rename("/d/db.next", "/d/db"); err != nil {
					t.Fatal(err)
				}
				if synced {
					mustSyncDir(t, sys, "/d")
				}
				d.Cut()
				sys = d.FS()
				switch db, next := held(t, sys, "/d/db"), held(t, sys, "/d/db.next"); {
				case db == "new" && next == "missing":
					seen["lasted"]++
				case db == old && next == "new":
					seen["vanished"]++
				default:
					t.Fatalf("%v, SyncDir %v, seed %d: after the cut /d/db holds %q and /d/db.next %q", op, synced, seed, db, next)
				}
			}
			where := fmt.Sprintf("%v, SyncDir %v", op, synced)
			switch {
			case !synced:
				both(t, where, seen, "lasted", "vanished")
			case seen["lasted"] != seeds:
				t.Errorf("%s: %v", where, seen)
			default:
				t.Logf("%s: %v", where, seen)
			}
		}
	}
}

// TestCreationsAndRemovalsNeedASyncDir does the same for a creation and a
// removal. A file is made, written and synced, which syncs its data and
// does nothing for its name: without a SyncDir it's gone after the cut in
// some seeds, data and all, and with one it's there in every seed. A file
// with its name synced is removed: without a SyncDir it's back after the
// cut in some seeds, with its data, and with one it's gone in every seed.
func TestCreationsAndRemovalsNeedASyncDir(t *testing.T) {
	const seeds = 200
	for _, remove := range []bool{false, true} {
		for _, synced := range []bool{false, true} {
			seen := map[string]int{}
			for seed := range uint64(seeds) {
				d, sys := newDisk(t, seed, "/d")
				if remove {
					newFile(t, sys, "/d/f", "data")
					if err := sys.Remove("/d/f"); err != nil {
						t.Fatal(err)
					}
				} else {
					f, err := sys.Create("/d/f", 0o640)
					if err != nil {
						t.Fatal(err)
					}
					mustWrite(t, f, []byte("data"), 0)
					if err := f.Sync(); err != nil {
						t.Fatal(err)
					}
				}
				if synced {
					mustSyncDir(t, sys, "/d")
				}
				d.Cut()
				seen[held(t, d.FS(), "/d/f")]++
			}
			what, want, other := "a creation", "data", "missing"
			if remove {
				what, want, other = "a removal", other, want
			}
			where := fmt.Sprintf("%s, SyncDir %v", what, synced)
			switch {
			case seen[want]+seen[other] != seeds:
				t.Errorf("%s: the file held something else: %v", where, seen)
			case !synced:
				both(t, where, seen, want, other)
			case seen[want] != seeds:
				t.Errorf("%s: %v", where, seen)
			default:
				t.Logf("%s: %v", where, seen)
			}
		}
	}
}

// A step in a sequence of calls a test cuts the power in, with what the
// calls before it left in s.
type callStep func(sys fsys.FS, s *stepState) error

type stepState struct{ f fsys.File }

// cutEach runs steps once with the power cut in each in turn, and once
// with it cut after the last, on a disk setup makes for each seed, and
// hands check the disk started again and how many steps returned. Rules
// count from when they're set, so setup's own calls don't move the cut.
func cutEach(t *testing.T, seeds int, setup func(seed uint64) (*fault.Disk, fsys.FS), steps []callStep, check func(sys fsys.FS, done int, where string)) {
	t.Helper()
	for i := 1; i <= len(steps)+1; i++ {
		for seed := range uint64(seeds) {
			d, sys := setup(seed)
			if i <= len(steps) {
				d.Add(fault.Rule{Op: fault.Any, N: i, Cut: true})
			}
			s, done := &stepState{}, 0
			for _, step := range steps {
				if err := step(sys, s); err != nil {
					mustCut(t, err)
					break
				}
				done++
			}
			cuts := 1
			if i > len(steps) {
				cuts = 0
			}
			if done != min(i-1, len(steps)) || d.Cuts() != cuts {
				t.Fatalf("a cut in call %d left %d calls done, with %d cuts", i, done, d.Cuts())
			}
			if i > len(steps) {
				d.Cut()
			}
			d.Clear()
			check(d.FS(), done, fmt.Sprintf("cut in call %d, seed %d", i, seed))
		}
	}
}

// TestCompactionSwitches runs compaction's switch, as BETA.md sets it out,
// with the power cut in each of its calls in turn, and after the last. The
// database holds "old", with its data, its name and an owner of its own
// synced, and a writer holds its lock. NAME.compact is made with the
// database's permissions, locked, given the database's owner, written with
// "new" and synced, then renamed over the database; the folder is synced,
// and the lock let go. After every cut the path holds "old" or "new", and
// "new" has the database's owner and permissions. Until the SyncDir
// returns, either can be there; once it has, "new" is, in every seed.
func TestCompactionSwitches(t *testing.T) {
	setup := func(seed uint64) (*fault.Disk, fsys.FS) {
		d, sys := newDisk(t, seed, "/d")
		db := newFile(t, sys, "/d/db", "old")
		if err := db.Chown(7, 8); err != nil {
			t.Fatal(err)
		}
		mustSyncDir(t, sys, "/d")
		if ok, err := db.TryLock(); !ok || err != nil {
			t.Fatal(ok, err)
		}
		return d, sys
	}
	steps := []callStep{
		func(sys fsys.FS, s *stepState) (err error) { s.f, err = sys.Create("/d/db.compact", 0o640); return err },
		func(sys fsys.FS, s *stepState) error { _, err := s.f.TryLock(); return err },
		func(sys fsys.FS, s *stepState) error { return s.f.Chown(7, 8) },
		func(sys fsys.FS, s *stepState) error { _, err := s.f.WriteAt([]byte("new"), 0); return err },
		func(sys fsys.FS, s *stepState) error { return s.f.Sync() },
		func(sys fsys.FS, s *stepState) error { return sys.Rename("/d/db.compact", "/d/db") },
		func(sys fsys.FS, s *stepState) error { return sys.SyncDir("/d") },
		func(sys fsys.FS, s *stepState) error { return s.f.Unlock() },
		func(sys fsys.FS, s *stepState) error { return s.f.Close() },
	}
	const synced, renamed, dirSynced = 5, 6, 7 // the steps, counted from 1
	seen := map[string]int{}
	cutEach(t, 60, setup, steps, func(sys fsys.FS, done int, where string) {
		db, compact := held(t, sys, "/d/db"), held(t, sys, "/d/db.compact")
		switch {
		case db == "new":
			if compact != "missing" {
				t.Fatalf("%s: /d/db.compact holds %q beside the new database", where, compact)
			}
			in, err := sys.Stat("/d/db")
			if err != nil || in.Uid != 7 || in.Gid != 8 || in.Mode != 0o640 || in.Nlink != 1 {
				t.Fatalf("%s: the new database is %+v, %v", where, in, err)
			}
		case db != "old":
			t.Fatalf("%s: /d/db holds %q", where, db)
		case done >= dirSynced:
			t.Fatalf("%s: the database is the old one after the SyncDir", where)
		case done >= synced && compact != "missing" && compact != "new":
			t.Fatalf("%s: /d/db.compact holds %q after its sync", where, compact)
		}
		if done < renamed-1 && db != "old" {
			t.Fatalf("%s: /d/db holds %q before the rename", where, db)
		}
		if done == renamed-1 || done == dirSynced-1 {
			seen[fmt.Sprintf("%s, cut in call %d", db, done+1)]++
		}
	})
	for _, call := range []int{renamed, dirSynced} {
		both(t, fmt.Sprintf("a cut in call %d", call), seen, fmt.Sprintf("old, cut in call %d", call), fmt.Sprintf("new, cut in call %d", call))
	}
}

// TestCreatingADatabase runs the creation of a database as FORMAT.md sets
// it out, with the power cut in each call in turn, and after the last: a
// .new- file made and locked, checked to be still at its name, given a
// header and synced, renamed to the database's path with RenameNoReplace,
// the folder synced and the lock let go. After every cut the path holds
// nothing or the whole header, and a .new- file is left only beside
// nothing. Once the SyncDir has returned, the header is there in every
// seed.
//
// Then a creator finds that another one's database got there first: its
// RenameNoReplace fails and it removes its file. After a cut the database
// is the other one's in every seed, and the removed file is back in some.
func TestCreatingADatabase(t *testing.T) {
	const name = "/d/db.new-abcdefghijkl"
	setup := func(seed uint64) (*fault.Disk, fsys.FS) { return newDisk(t, seed, "/d") }
	steps := []callStep{
		func(sys fsys.FS, s *stepState) (err error) { s.f, err = sys.Create(name, 0o640); return err },
		func(sys fsys.FS, s *stepState) error { _, err := s.f.TryLock(); return err },
		func(sys fsys.FS, s *stepState) error { _, err := s.f.Stat(); return err },
		func(sys fsys.FS, s *stepState) error { _, err := sys.Stat(name); return err },
		func(sys fsys.FS, s *stepState) error { _, err := s.f.WriteAt([]byte("HEADER"), 0); return err },
		func(sys fsys.FS, s *stepState) error { return s.f.Sync() },
		func(sys fsys.FS, s *stepState) error { return sys.RenameNoReplace(name, "/d/db") },
		func(sys fsys.FS, s *stepState) error { return sys.SyncDir("/d") },
		func(sys fsys.FS, s *stepState) error { return s.f.Unlock() },
		func(sys fsys.FS, s *stepState) error { return s.f.Close() },
	}
	const synced, renamed, dirSynced = 6, 7, 8
	seen := map[string]int{}
	cutEach(t, 60, setup, steps, func(sys fsys.FS, done int, where string) {
		db, left := held(t, sys, "/d/db"), held(t, sys, name)
		switch {
		case db == "HEADER" && left != "missing":
			t.Fatalf("%s: the .new- file holds %q beside the new database", where, left)
		case db == "HEADER":
		case db != "missing":
			t.Fatalf("%s: /d/db holds %q", where, db)
		case done >= dirSynced:
			t.Fatalf("%s: no database after the SyncDir", where)
		case done >= synced && left != "missing" && left != "HEADER":
			t.Fatalf("%s: the .new- file holds %q after its sync", where, left)
		case left != "missing":
			seen["a .new- file left"]++
		}
		if done < renamed-1 && db != "missing" {
			t.Fatalf("%s: /d/db holds %q before the rename", where, db)
		}
		if done == renamed-1 || done == dirSynced-1 {
			seen[fmt.Sprintf("%s, cut in call %d", db, done+1)]++
		}
	})
	for _, call := range []int{renamed, dirSynced} {
		both(t, fmt.Sprintf("a cut in call %d", call), seen, fmt.Sprintf("missing, cut in call %d", call), fmt.Sprintf("HEADER, cut in call %d", call))
	}
	if seen["a .new- file left"] == 0 {
		t.Errorf("no cut left a .new- file: %v", seen)
	}

	seen = map[string]int{}
	for seed := range uint64(200) {
		d, sys := newDisk(t, seed, "/d")
		newFile(t, sys, "/d/db", "OTHER")
		f, err := sys.Create(name, 0o640)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, f, []byte("HEADER"), 0)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := sys.RenameNoReplace(name, "/d/db"); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("seed %d: RenameNoReplace over a database gives %v", seed, err)
		}
		if err := sys.Remove(name); err != nil {
			t.Fatal(err)
		}
		f.Close()
		d.Cut()
		sys = d.FS()
		if db := held(t, sys, "/d/db"); db != "OTHER" {
			t.Fatalf("seed %d: /d/db holds %q after the cut", seed, db)
		}
		seen[held(t, sys, name)]++
	}
	both(t, "the creator that lost", seen, "missing", "HEADER")
}

// TestLostNamesTakeTheirFiles looks at what a cut leaves of files whose
// names it lost or gave back. A file made and synced, whose name wasn't,
// is gone with its data when its name goes: Stat and Open find nothing,
// List leaves it out, and a new file made at the name starts empty. A file
// a lost rename had replaced comes back with its data and one name. A file
// whose removal was lost comes back too, with what was written to it
// through a handle still open after the removal, once that was synced.
func TestLostNamesTakeTheirFiles(t *testing.T) {
	seen := map[string]int{}
	for seed := range uint64(100) {
		d, sys := newDisk(t, seed, "/d")
		f, err := sys.Create("/d/f", 0o640)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, f, []byte("synced data"), 0)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		d.Cut()
		sys = d.FS()
		if held(t, sys, "/d/f") != "missing" {
			continue
		}
		seen["gone"]++
		if _, err := sys.Open("/d/f"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("seed %d: a file whose name was lost opens with %v", seed, err)
		}
		if list, err := sys.List("/d"); err != nil || len(list) != 0 {
			t.Fatalf("seed %d: /d lists %q, %v", seed, list, err)
		}
		g := mustCreate(t, sys, "/d/f", 0o640)
		if in, err := g.Stat(); err != nil || in.Size != 0 || in.Nlink != 1 {
			t.Fatalf("seed %d: a new file at the lost name is %+v, %v", seed, in, err)
		}
		if got := held(t, sys, "/d/f"); got != "" {
			t.Fatalf("seed %d: a new file at the lost name holds %q", seed, got)
		}
	}

	for seed := range uint64(100) {
		d, sys := newDisk(t, seed, "/d")
		newFile(t, sys, "/d/db", "old")
		newFile(t, sys, "/d/next", "new")
		if err := sys.Rename("/d/next", "/d/db"); err != nil {
			t.Fatal(err)
		}
		d.Cut()
		sys = d.FS()
		if held(t, sys, "/d/db") != "old" {
			continue
		}
		seen["replaced, back"]++
		for name, want := range map[string]string{"/d/db": "old", "/d/next": "new"} {
			in, err := sys.Stat(name)
			if err != nil || in.Nlink != 1 || held(t, sys, name) != want {
				t.Fatalf("seed %d: after a lost rename %s is %+v, %v, and holds %q", seed, name, in, err, held(t, sys, name))
			}
		}
	}

	for seed := range uint64(100) {
		d, sys := newDisk(t, seed, "/d")
		f := newFile(t, sys, "/d/f", "data")
		if err := sys.Remove("/d/f"); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, f, []byte(" and more"), 4)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		d.Cut()
		sys = d.FS()
		switch got := held(t, sys, "/d/f"); got {
		case "missing":
		case "data and more":
			seen["removed, back"]++
			if in, err := sys.Stat("/d/f"); err != nil || in.Nlink != 1 {
				t.Fatalf("seed %d: a file whose removal was lost is %+v, %v", seed, in, err)
			}
		default:
			t.Fatalf("seed %d: a file whose removal was lost holds %q", seed, got)
		}
	}
	for _, want := range []string{"gone", "replaced, back", "removed, back"} {
		if seen[want] == 0 {
			t.Errorf("no seed gave %q: %v", want, seen)
		}
	}
	t.Log(seen)
}

// TestOwnersNeedASyncedFolder checks the model for owners. A Chown, with
// its file's Sync after it, is lost at a cut in some seeds and kept in
// others, since fdatasync promises nothing for it; once the folder that
// holds the file's name is synced, it's kept in every seed. After a Chown
// and then a rename, neither synced, a rename that lasts comes with the
// Chown, since a cut keeps changes in their order. And a file that's there
// after a cut has the permissions it was made with.
func TestOwnersNeedASyncedFolder(t *testing.T) {
	me := os.Getuid()
	for _, synced := range []bool{false, true} {
		seen := map[string]int{}
		for seed := range uint64(200) {
			d, sys := newDisk(t, seed, "/d")
			f := newFile(t, sys, "/d/f", "data")
			if err := f.Chown(me+7, -1); err != nil {
				t.Fatal(err)
			}
			if err := f.Sync(); err != nil {
				t.Fatal(err)
			}
			if synced {
				mustSyncDir(t, sys, "/d")
			}
			d.Cut()
			in, err := d.FS().Stat("/d/f")
			switch {
			case err != nil:
				t.Fatal(err)
			case in.Uid == me+7:
				seen["kept"]++
			case in.Uid == me:
				seen["lost"]++
			default:
				t.Fatalf("seed %d: the owner is %d", seed, in.Uid)
			}
		}
		if synced && seen["kept"] != 200 {
			t.Errorf("a Chown with its folder synced: %v", seen)
		}
		if !synced {
			both(t, "a Chown without a SyncDir", seen, "kept", "lost")
		}
	}

	seen := map[string]int{}
	for seed := range uint64(200) {
		d, sys := newDisk(t, seed, "/d")
		f := newFile(t, sys, "/d/f", "data")
		if err := f.Chown(me+7, me+8); err != nil {
			t.Fatal(err)
		}
		if err := sys.Rename("/d/f", "/d/g"); err != nil {
			t.Fatal(err)
		}
		g, err := sys.Create("/d/odd", 0o604)
		if err != nil {
			t.Fatal(err)
		}
		g.Close()
		d.Cut()
		sys = d.FS()
		switch in, err := sys.Stat("/d/g"); {
		case err == nil:
			seen["renamed"]++
			if in.Uid != me+7 || in.Gid != me+8 {
				t.Fatalf("seed %d: the rename lasted and the Chown before it didn't: %+v", seed, in)
			}
		case held(t, sys, "/d/f") != "data":
			t.Fatalf("seed %d: the file is at neither name", seed)
		default:
			seen["not renamed"]++
		}
		if in, err := sys.Stat("/d/odd"); err == nil && in.Mode != 0o604 {
			t.Fatalf("seed %d: a file made with 0o604 is %v after the cut", seed, in.Mode)
		}
	}
	both(t, "a Chown and a rename", seen, "renamed", "not renamed")
}

// TestASyncDirPromisesItsOwnFolder makes a file in one folder, then one in
// another, and syncs the second folder alone. After a cut the second file
// is there in every seed, and the first is gone in some, so a later change
// lasts while an earlier one is lost. ext4 would keep the first too, since
// its fsync on a folder commits every folder's changes, but Linux promises
// only the folder's own, and the model holds the engine to that.
func TestASyncDirPromisesItsOwnFolder(t *testing.T) {
	seen := map[string]int{}
	for seed := range uint64(200) {
		d, sys := newDisk(t, seed, "/a", "/b")
		for _, name := range []string{"/a/x", "/b/y"} {
			f, err := sys.Create(name, 0o640)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
		mustSyncDir(t, sys, "/b")
		d.Cut()
		sys = d.FS()
		if held(t, sys, "/b/y") != "" {
			t.Fatalf("seed %d: the file in the synced folder is gone", seed)
		}
		seen[held(t, sys, "/a/x")]++
	}
	both(t, "the other folder's file", seen, "", "missing")
}

// TestARenameBetweenFolders moves a file from one folder to another. After
// a cut it's at exactly one of its two names, since a rename is kept or
// lost whole. A SyncDir of either folder makes the move last. And a file
// made in one folder and moved to another lasts once the second folder is
// synced: its creation goes on the drive with the move that leans on it.
func TestARenameBetweenFolders(t *testing.T) {
	for _, sync := range []string{"", "/a", "/b"} {
		seen := map[string]int{}
		for seed := range uint64(200) {
			d, sys := newDisk(t, seed, "/a", "/b")
			newFile(t, sys, "/a/x", "x")
			if err := sys.Rename("/a/x", "/b/y"); err != nil {
				t.Fatal(err)
			}
			if sync != "" {
				mustSyncDir(t, sys, sync)
			}
			d.Cut()
			sys = d.FS()
			switch from, to := held(t, sys, "/a/x"), held(t, sys, "/b/y"); {
			case from == "x" && to == "missing":
				seen["not moved"]++
			case from == "missing" && to == "x":
				seen["moved"]++
			default:
				t.Fatalf("SyncDir %q, seed %d: after the cut /a/x holds %q and /b/y %q", sync, seed, from, to)
			}
		}
		if sync != "" && seen["moved"] != 200 {
			t.Errorf("a move with %s synced: %v", sync, seen)
		}
		if sync == "" {
			both(t, "a move without a SyncDir", seen, "moved", "not moved")
		}
	}

	for seed := range uint64(200) {
		d, sys := newDisk(t, seed, "/a", "/b")
		f, err := sys.Create("/a/x", 0o640)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, f, []byte("x"), 0)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := sys.Rename("/a/x", "/b/y"); err != nil {
			t.Fatal(err)
		}
		mustSyncDir(t, sys, "/b")
		d.Cut()
		sys = d.FS()
		if from, to := held(t, sys, "/a/x"), held(t, sys, "/b/y"); from != "missing" || to != "x" {
			t.Fatalf("seed %d: after the cut /a/x holds %q and /b/y %q", seed, from, to)
		}
	}
}

// TestACutInACallOnNames cuts the power in each kind of call that changes
// names, or an owner. The call returns ErrCut, and its change was made
// before the cut, so it lasts in some seeds and is lost in others, though
// the call never returned. A cut in a SyncDir comes before it does
// anything, so a creation before it is lost in some seeds.
func TestACutInACallOnNames(t *testing.T) {
	me := os.Getuid()
	for _, op := range []fault.Op{fault.Create, fault.Rename, fault.RenameNoReplace, fault.Remove, fault.Chown, fault.SyncDir} {
		seen := map[string]int{}
		for seed := range uint64(200) {
			d, sys := newDisk(t, seed, "/d")
			f := newFile(t, sys, "/d/f", "f")
			if op == fault.SyncDir {
				g, err := sys.Create("/d/g", 0o640)
				if err != nil {
					t.Fatal(err)
				}
				g.Close()
			}
			d.Add(fault.Rule{Op: op, N: 1, Cut: true})
			var err error
			switch op {
			case fault.Create:
				_, err = sys.Create("/d/g", 0o640)
			case fault.Rename:
				err = sys.Rename("/d/f", "/d/g")
			case fault.RenameNoReplace:
				err = sys.RenameNoReplace("/d/f", "/d/g")
			case fault.Remove:
				err = sys.Remove("/d/f")
			case fault.Chown:
				err = f.Chown(me+7, -1)
			case fault.SyncDir:
				err = sys.SyncDir("/d")
			}
			mustCut(t, err)
			sys = d.FS()
			kept := false
			switch op {
			case fault.Create, fault.SyncDir:
				kept = held(t, sys, "/d/g") != "missing"
			case fault.Rename, fault.RenameNoReplace:
				kept = held(t, sys, "/d/g") == "f"
				if kept == (held(t, sys, "/d/f") == "f") {
					t.Fatalf("%v, seed %d: the file is at both names, or neither", op, seed)
				}
			case fault.Remove:
				kept = held(t, sys, "/d/f") == "missing"
			case fault.Chown:
				in, err := sys.Stat("/d/f")
				if err != nil {
					t.Fatal(err)
				}
				kept = in.Uid == me+7
			}
			if kept {
				seen["kept"]++
			} else {
				seen["lost"]++
			}
		}
		both(t, fmt.Sprintf("a cut in %v", op), seen, "kept", "lost")
	}
}

// TestAFailedSyncDirPromisesNothing fails a SyncDir with a rule. It puts
// nothing on the drive, so a file made before it is gone after a cut in
// some seeds, and a good SyncDir after the failure makes it last in every
// seed. A rename a rule fails leaves nothing behind for a cut to keep.
func TestAFailedSyncDirPromisesNothing(t *testing.T) {
	for _, again := range []bool{false, true} {
		seen := map[string]int{}
		for seed := range uint64(200) {
			d, sys := newDisk(t, seed, "/d")
			f, err := sys.Create("/d/g", 0o640)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			d.Add(fault.Rule{Op: fault.SyncDir, N: 1})
			if err := sys.SyncDir("/d"); !errors.Is(err, syscall.EIO) {
				t.Fatalf("a SyncDir a rule fails gives %v", err)
			}
			if again {
				mustSyncDir(t, sys, "/d")
			}
			d.Cut()
			seen[held(t, d.FS(), "/d/g")]++
		}
		if again && seen[""] != 200 {
			t.Errorf("a good SyncDir after a failed one: %v", seen)
		}
		if !again {
			both(t, "a failed SyncDir", seen, "", "missing")
		}
	}

	for seed := range uint64(100) {
		d, sys := newDisk(t, seed, "/d")
		newFile(t, sys, "/d/f", "f")
		d.Add(fault.Rule{Op: fault.Rename, N: 1})
		if err := sys.Rename("/d/f", "/d/h"); !errors.Is(err, syscall.EIO) {
			t.Fatalf("a Rename a rule fails gives %v", err)
		}
		d.Cut()
		sys = d.FS()
		if list, err := sys.List("/d"); err != nil || !slices.Equal(list, []string{"f"}) {
			t.Fatalf("seed %d: after a failed rename and a cut /d holds %q, %v", seed, list, err)
		}
	}
}
