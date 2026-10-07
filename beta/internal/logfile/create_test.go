// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// names returns the names in dir.
func names(t *testing.T, dir string) string {
	t.Helper()
	got, err := fsys.OS{}.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(got, " ")
}

// TestCreatingADatabase: a new database is a file holding only the
// header of generation 1, with a random ID, and the permissions 0o644 less
// the umask, and nothing is left beside it.
func TestCreatingADatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	old := syscall.Umask(0o027)
	l, rec := openLog(t, path, Options{})
	syscall.Umask(old)
	rec.holds(t)
	file := fileBytes(t, path)
	h, err := format.DecodeHeader(file, int64(len(file)))
	if err != nil || len(file) != format.HeaderSize {
		t.Fatalf("the new file is %d bytes, with the header %+v, %v", len(file), h, err)
	}
	if h.Gen != 1 || h.ID == ([16]byte{}) || h != l.hdr {
		t.Errorf("the new header is %+v, and the Log has %+v", h, l.hdr)
	}
	if st, _ := os.Stat(path); st.Mode() != 0o640 {
		t.Errorf("a database made under umask 0o027 has mode %v", st.Mode())
	}
	if got := names(t, dir); got != "db" {
		t.Errorf("the folder holds %q", got)
	}
	other, _ := openLog(t, filepath.Join(dir, "other"), Options{})
	if other.hdr.ID == l.hdr.ID {
		t.Error("two databases have the same ID")
	}

	missing := filepath.Join(dir, "no such folder", "db")
	if _, err := Open(fsys.OS{}, missing, &recorder{}, Options{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open in a missing folder: %v", err)
	}
}

// TestCreatorsRacing: creators that find nothing at the path at the same
// moment all end up with one database, the first to be renamed into
// place, and nothing is left beside it.
func TestCreatorsRacing(t *testing.T) {
	dir := t.TempDir()
	rounds := 30
	if testing.Short() {
		rounds = 10
	}
	for round := range rounds {
		path := filepath.Join(dir, fmt.Sprintf("db%d", round))
		const n = 6
		ids := make([][16]byte, n)
		errc := make(chan error, n)
		var ready, wg sync.WaitGroup
		ready.Add(n)
		wg.Add(n)
		for i := range n {
			go func() {
				defer wg.Done()
				ready.Done()
				ready.Wait()
				l, err := Open(fsys.OS{}, path, &recorder{}, Options{})
				if err != nil {
					errc <- err
					return
				}
				ids[i] = l.hdr.ID
				errc <- l.Close()
			}()
		}
		wg.Wait()
		for range n {
			if err := <-errc; err != nil {
				t.Fatal(err)
			}
		}
		file := fileBytes(t, path)
		h, err := format.DecodeHeader(file, int64(len(file)))
		if err != nil || len(file) != format.HeaderSize {
			t.Fatalf("round %d left a file of %d bytes: %v", round, len(file), err)
		}
		for i, id := range ids {
			if id != h.ID {
				t.Fatalf("round %d: creator %d opened the database %x, and %x is at the path", round, i, id, h.ID)
			}
		}
	}
	got, _ := fsys.OS{}.List(dir)
	if len(got) != rounds || strings.Contains(strings.Join(got, " "), ".new-") {
		t.Errorf("the folder holds %q, where %d databases and nothing else are wanted", got, rounds)
	}
}

// TestCreatorsRacingAcrossProcesses: the same race between processes,
// four at a time, each started and waiting before they're all let go.
func TestCreatorsRacingAcrossProcesses(t *testing.T) {
	rounds := 5
	if testing.Short() {
		rounds = 2
	}
	for round := range rounds {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		var ps []*proc
		for range 4 {
			ps = append(ps, start(t, "create", path))
		}
		for _, p := range ps {
			fmt.Fprintln(p.in, "go")
		}
		ids := map[string]bool{}
		for _, p := range ps {
			ids[p.line(t)] = true
			p.end(t)
		}
		l, _ := openLog(t, path, Options{})
		if want := fmt.Sprintf("%x", l.hdr.ID); len(ids) != 1 || !ids[want] {
			t.Errorf("round %d: the creators opened the databases %v, and %s is at the path", round, ids, want)
		}
		if got := names(t, dir); got != "db" {
			t.Errorf("round %d: the folder holds %q", round, got)
		}
	}
}

// TestACreatorThatLosesOpensTheWinners: when another creator's database
// appears at the path before the rename, the rename fails, and the creator
// removes its own file and opens the other one.
func TestACreatorThatLosesOpensTheWinners(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	var winner [16]byte
	h := &hookFS{FS: fsys.OS{}}
	h.renameNoReplace = func(from, to string) error {
		h.renameNoReplace = nil
		w, err := Open(fsys.OS{}, path, &recorder{}, Options{})
		if err != nil {
			return err
		}
		winner = w.hdr.ID
		w.Close()
		return h.FS.RenameNoReplace(from, to)
	}
	l, err := Open(h, path, &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.hdr.ID != winner {
		t.Errorf("the creator that lost opened %x, where the winner's database is %x", l.hdr.ID, winner)
	}
	if got := names(t, dir); got != "db" {
		t.Errorf("the folder holds %q", got)
	}
}

// TestCreatingNeedsARenameThatDoesntReplace: on a file system without
// RENAME_NOREPLACE, creating a database fails, and leaves nothing behind.
func TestCreatingNeedsARenameThatDoesntReplace(t *testing.T) {
	dir := t.TempDir()
	h := &hookFS{FS: fsys.OS{}, renameNoReplace: func(from, to string) error {
		return &os.LinkError{Op: "renameat2", Old: from, New: to, Err: fmt.Errorf("%w: no RENAME_NOREPLACE here", errors.ErrUnsupported)}
	}}
	_, err := Open(h, filepath.Join(dir, "db"), &recorder{}, Options{})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Open where the rename can't refuse to replace: %v", err)
	}
	if got := names(t, dir); got != "" {
		t.Errorf("the folder holds %q", got)
	}
}

// TestCreatingWhenTheFolderWontSync: until the folder's sync makes the
// rename last, nobody may commit to the new database, so when the sync
// fails, the new database's name goes again.
func TestCreatingWhenTheFolderWontSync(t *testing.T) {
	dir := t.TempDir()
	h := &hookFS{FS: fsys.OS{}, syncDir: func(string) error { return syscall.EIO }}
	_, err := Open(h, filepath.Join(dir, "db"), &recorder{}, Options{})
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("Open with a folder that won't sync: %v", err)
	}
	if got := names(t, dir); got != "" {
		t.Errorf("the folder holds %q", got)
	}
}

// TestLeftoversAreRemoved: the first Lock removes the .new- files that can
// be locked, which crashed creators left, and leaves alone one that's
// locked, a creation under way, and every name of another shape.
func TestLeftoversAreRemoved(t *testing.T) {
	dir := t.TempDir()
	at := func(name string) string { return filepath.Join(dir, name) }
	l, _ := openLog(t, at("db"), Options{})
	head := fileBytes(t, at("db"))
	for _, name := range []string{"db.new-abcdefghijkl", "db.new-Qx", "db.new-mnopqrstuvwx", "db.new-", "db.new-123", "db.new-abc.txt", "other.new-abcdefghijkl", "db.new-abcdefghijk"} {
		if err := os.WriteFile(at(name), head, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(at("db.new-abcdefghijk"))
	if err := os.Mkdir(at("db.new-abcdefghijk"), 0o700); err != nil {
		t.Fatal(err)
	}
	busy, err := fsys.OS{}.Open(at("db.new-mnopqrstuvwx"))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if ok, err := busy.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if want := "db db.new- db.new-123 db.new-Qx db.new-abc.txt db.new-abcdefghijk db.new-abcdefghijkl db.new-mnopqrstuvwx other.new-abcdefghijkl"; names(t, dir) != want {
		t.Fatalf("the folder holds %q before Lock", names(t, dir))
	}
	commit(t, l)
	if want := "db db.new- db.new-123 db.new-abc.txt db.new-abcdefghijk db.new-mnopqrstuvwx other.new-abcdefghijkl"; names(t, dir) != want {
		t.Errorf("after Lock the folder holds\n  %q\nwhere\n  %q\nis wanted", names(t, dir), want)
	}

	// The next Lock of the same Log doesn't look again.
	if err := os.WriteFile(at("db.new-later"), head, 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, l)
	if _, err := os.Stat(at("db.new-later")); err != nil {
		t.Errorf("a later Lock of the same Log looked for leftovers: %v", err)
	}
}

// TestAnEmptyFileBecomesADatabase: an empty file holds no database until
// the first writer makes one of it, under the empty file's lock, as a new
// file with the empty one's permissions renamed over it. Another Log that
// had the empty file open finds the new one at the path, and its commit
// starts again there.
func TestAnEmptyFileBecomesADatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o604); err != nil {
		t.Fatal(err)
	}
	empty := inode(t, path)
	a, ra := openLog(t, path, Options{})
	b, rb := openLog(t, path, Options{})
	if !a.empty || !b.empty {
		t.Fatal("an empty file was read as a database")
	}
	if n := len(fileBytes(t, path)); n != 0 || !inode(t, path).Same(empty) {
		t.Fatalf("opening changed the empty file: %d bytes", n)
	}

	commit(t, a, table("one"))
	ra.holds(t)
	now := inode(t, path)
	if now.Same(empty) || now.Mode != 0o604 {
		t.Errorf("the database that replaced the empty file has mode %v, and is the empty file: %v", now.Mode, now.Same(empty))
	}
	commit(t, b, table("two"))
	if rb.resets != 1 {
		t.Errorf("b reset %d times", rb.resets)
	}
	rb.holds(t, table("one"))
	reread(t, path).holds(t, table("one"), table("two"))
	if got := names(t, dir); got != "db" {
		t.Errorf("the folder holds %q", got)
	}
}

// TestAnEmptyFileWhoseFolderWontSync: once the empty file has been
// replaced, a failed sync of the folder leaves nobody able to tell which
// file a power cut would leave at the path, so the Log keeps the lock
// until it's closed, and every other writer waits meanwhile.
func TestAnEmptyFileWhoseFolderWontSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := &hookFS{FS: fsys.OS{}, syncDir: func(string) error { return syscall.EIO }}
	a, err := Open(h, path, &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, _ := openLog(t, path, Options{Wait: 300 * time.Millisecond})
	if err := lockErr(a); !errors.Is(err, errs.ErrStuck) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("Lock when the folder won't sync: %v", err)
	}
	if err := lockErr(a); !errors.Is(err, errs.ErrStuck) {
		t.Errorf("a second Lock: %v", err)
	}
	if err := lockErr(b); !errors.Is(err, errs.ErrLockTimeout) {
		t.Errorf("another Log's Lock while the first is stuck: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	commit(t, b, table("one"))
	reread(t, path).holds(t, table("one"))
}

// TestAnEmptyFileWrittenInPlace: something other than HyperCrux wrote a
// database into the empty file in place, after it was read. Lock reads the
// file again as it is.
func TestAnEmptyFileWrittenInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l, rec := openLog(t, path, Options{})
	w := newBuilder(t)
	w.commit(table("one"))
	w.write(path)
	commit(t, l, table("two"))
	rec.holds(t, table("one"))
	reread(t, path).holds(t, table("one"), table("two"))
	if !bytes.Equal(fileBytes(t, path)[:len(w.b)], w.b) {
		t.Error("the file's first batch changed")
	}
}
