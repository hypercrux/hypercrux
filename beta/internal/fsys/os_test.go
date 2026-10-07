// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fsys

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// These tests run the real calls on files in a temporary folder. They're
// in package fsys, so they can look at a file's descriptor.

func create(t *testing.T, path string, perm fs.FileMode) File {
	t.Helper()
	f, err := OS{}.Create(path, perm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func reopen(t *testing.T, path string) File {
	t.Helper()
	f, err := OS{}.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestOSOpenAndCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	_, err := OS{}.Open(path)
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), path) {
		t.Errorf("Open of a missing file: %v, where fs.ErrNotExist with the path is wanted", err)
	}

	// The umask would take 0o044 away from a file made in the usual way.
	// Create sets the permissions asked for, whatever it is.
	old := syscall.Umask(0o077)
	f, err := OS{}.Create(path, 0o644)
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != 0o644 {
		t.Errorf("Create(0o644) under umask 0o077 made a file with mode %v", st.Mode())
	}

	_, err = OS{}.Create(path, 0o600)
	if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), path) {
		t.Errorf("Create over a file: %v, where fs.ErrExist with the path is wanted", err)
	}
	if st, _ := os.Stat(path); st.Mode() != 0o644 {
		t.Errorf("a refused Create changed the file's mode to %v", st.Mode())
	}

	g, err := OS{}.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if _, err := (OS{}).Open(t.TempDir()); !errors.Is(err, syscall.EISDIR) {
		t.Errorf("Open of a folder: %v, where EISDIR is wanted", err)
	}
}

// TestOSCloseOnExec checks that every file the real calls open has
// FD_CLOEXEC, so a child process never inherits one, or its lock.
func TestOSCloseOnExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	for name, f := range map[string]File{"Create": create(t, path, 0o600), "Open": reopen(t, path)} {
		flags, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(f.(*file).fd), syscall.F_GETFD, 0)
		if e != 0 {
			t.Fatal(e)
		}
		if flags&syscall.FD_CLOEXEC == 0 {
			t.Errorf("a file from %s is open without FD_CLOEXEC", name)
		}
	}
}

func TestOSReadAtAndWriteAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	f := create(t, path, 0o600)
	if n, err := f.WriteAt([]byte("hello world"), 0); n != 11 || err != nil {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if n, err := f.WriteAt([]byte("X"), 20); n != 1 || err != nil {
		t.Fatalf("WriteAt past the end: %d, %v", n, err)
	}
	want := []byte("hello world\x00\x00\x00\x00\x00\x00\x00\x00\x00X")

	p := make([]byte, 64)
	if n, err := f.ReadAt(p[:21], 0); n != 21 || err != nil || !bytes.Equal(p[:21], want) {
		t.Errorf("ReadAt of the whole file: %d, %v, %q", n, err, p[:n])
	}
	// io.ReaderAt's promise: fewer bytes than asked for come with an
	// error, which is io.EOF at the end of the file.
	if n, err := f.ReadAt(p[:10], 15); n != 6 || err != io.EOF || !bytes.Equal(p[:6], want[15:]) {
		t.Errorf("ReadAt across the end: %d, %v, %q", n, err, p[:n])
	}
	if n, err := f.ReadAt(p[:5], 21); n != 0 || err != io.EOF {
		t.Errorf("ReadAt at the end: %d, %v", n, err)
	}
	if n, err := f.ReadAt(p[:0], 100); n != 0 || err != nil {
		t.Errorf("ReadAt of nothing: %d, %v", n, err)
	}
	if _, err := f.ReadAt(p[:1], -1); err == nil {
		t.Error("ReadAt at a negative offset worked")
	}
	if _, err := f.WriteAt(p[:1], -1); err == nil {
		t.Error("WriteAt at a negative offset worked")
	}

	// Several goroutines can read one file at once.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := make([]byte, 5)
			for off := range int64(16) {
				if n, err := f.ReadAt(q, off); n != 5 || err != nil || !bytes.Equal(q, want[off:off+5]) {
					t.Errorf("goroutine %d, ReadAt at %d: %d, %v, %q", i, off, n, err, q[:n])
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := f.Sync(); err != nil {
		t.Error(err)
	}
	if err := f.Truncate(5); err != nil {
		t.Fatal(err)
	}
	if st, err := f.Stat(); err != nil || st.Size != 5 {
		t.Errorf("Stat after a cut to 5: %+v, %v", st, err)
	}
	if err := f.Truncate(8); err != nil {
		t.Fatal(err)
	}
	if n, err := f.ReadAt(p[:9], 0); n != 8 || err != io.EOF || string(p[:8]) != "hello\x00\x00\x00" {
		t.Errorf("ReadAt after growing to 8: %d, %v, %q", n, err, p[:n])
	}
}

func TestOSFlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	create(t, path, 0o600)
	a, b := reopen(t, path), reopen(t, path)
	try := func(f File, name string, want bool) {
		t.Helper()
		got, err := f.TryLock()
		if err != nil || got != want {
			t.Fatalf("%s.TryLock() = %v, %v, where %v is wanted", name, got, err, want)
		}
	}
	// A flock belongs to an open file, so two opens in one process keep
	// each other out, as two processes do.
	try(a, "a", true)
	try(b, "b", false)
	try(a, "a", true) // a holds it already
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	try(b, "b", true)
	try(a, "a", false)
	// Closing a file lets go of its lock.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	try(a, "a", true)
}

func TestOSRenames(t *testing.T) {
	dir := t.TempDir()
	at := func(name string) string { return filepath.Join(dir, name) }
	write := func(name, s string) {
		if err := os.WriteFile(at(name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	holds := func(name, want string) {
		t.Helper()
		b, err := os.ReadFile(at(name))
		if err != nil || string(b) != want {
			t.Errorf("%s holds %q, %v, where %q is wanted", name, b, err, want)
		}
	}
	gone := func(name string) {
		t.Helper()
		if _, err := os.Stat(at(name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there: %v", name, err)
		}
	}

	write("a", "a")
	write("b", "b")
	if err := (OS{}).Rename(at("a"), at("b")); err != nil {
		t.Fatal(err)
	}
	holds("b", "a")
	gone("a")

	write("c", "c")
	err := OS{}.RenameNoReplace(at("c"), at("b"))
	var le *os.LinkError
	if !errors.Is(err, fs.ErrExist) || !errors.As(err, &le) || le.Old != at("c") || le.New != at("b") {
		t.Errorf("RenameNoReplace over a file: %v, where fs.ErrExist with both paths is wanted", err)
	}
	holds("b", "a")
	holds("c", "c")

	if err := (OS{}).RenameNoReplace(at("c"), at("d")); err != nil {
		t.Fatal(err)
	}
	holds("d", "c")
	gone("c")
	if err := (OS{}).RenameNoReplace(at("c"), at("e")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("RenameNoReplace of a missing file: %v", err)
	}

	// renameat2 refuses a flag it doesn't know with EINVAL, as it does
	// RENAME_NOREPLACE on a file system without it, and that becomes an
	// error matching errors.ErrUnsupported.
	err = renameat2(at("d"), at("f"), 1<<30)
	if !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, syscall.EINVAL) {
		t.Errorf("renameat2 with an unknown flag: %v, where errors.ErrUnsupported and EINVAL are wanted", err)
	}
	holds("d", "c")
	gone("f")
}

func TestOSStat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	f := create(t, path, 0o640)
	if _, err := f.WriteAt([]byte("0123456789"), 0); err != nil {
		t.Fatal(err)
	}
	mine, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	there, err := OS{}.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mine != there {
		t.Errorf("fstat and stat disagree: %+v and %+v", mine, there)
	}
	sys, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := sys.Sys().(*syscall.Stat_t)
	want := Info{Dev: st.Dev, Ino: st.Ino, Nlink: 1, Size: 10, Mode: 0o640, Uid: int(st.Uid), Gid: int(st.Gid)}
	if mine != want {
		t.Errorf("Stat gave %+v, where os.Stat says %+v", mine, want)
	}

	if err := os.Link(path, path+".2"); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Stat(); st.Nlink != 2 {
		t.Errorf("a file with two names has Nlink %d", st.Nlink)
	}
	if err := os.Symlink("db", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if st, err := (OS{}).Stat(filepath.Join(dir, "link")); err != nil || !st.Same(mine) || st.Mode != 0o640 {
		t.Errorf("Stat of a symbolic link didn't follow it: %+v, %v", st, err)
	}
	other := create(t, filepath.Join(dir, "other"), 0o600)
	if st, _ := other.Stat(); st.Same(mine) {
		t.Error("two files are the same")
	}
	if st, err := (OS{}).Stat(dir); err != nil || !st.Mode.IsDir() {
		t.Errorf("Stat of a folder: %+v, %v", st, err)
	}
	if _, err := (OS{}).Stat(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat of a missing file: %v", err)
	}
}

func TestOSNamesInAFolder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b", "a", "C", "a.new-x"} {
		create(t, filepath.Join(dir, name), 0o600)
	}
	names, err := OS{}.List(dir)
	if want := []string{"C", "a", "a.new-x", "b"}; err != nil || strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("List gave %q, %v, where %q is wanted", names, err, want)
	}
	if _, err := (OS{}).List(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("List of a missing folder: %v", err)
	}

	if err := (OS{}).Remove(filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := (OS{}).Remove(filepath.Join(dir, "a")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Remove of a missing file: %v", err)
	}
	if err := (OS{}).SyncDir(dir); err != nil {
		t.Error(err)
	}
	if err := (OS{}).SyncDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SyncDir of a missing folder: %v", err)
	}
	if err := (OS{}).SyncDir(filepath.Join(dir, "b")); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("SyncDir of a file: %v", err)
	}
}

func TestOSRealPath(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	create(t, filepath.Join(dir, "real", "db"), 0o600)
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "real", "db")
	got, err := OS{}.RealPath(filepath.Join(dir, "link", "db"))
	if err != nil || got != want {
		t.Errorf("RealPath through a symbolic link: %q, %v, where %q is wanted", got, err, want)
	}

	// From a working folder reached through the symbolic link, a relative
	// path and one with ".." both come out as the real path.
	t.Chdir(filepath.Join(dir, "link"))
	for _, rel := range []string{"db", "../real/db", "./../link/db"} {
		if got, err := (OS{}).RealPath(rel); err != nil || got != want {
			t.Errorf("RealPath(%q): %q, %v, where %q is wanted", rel, got, err, want)
		}
	}
	if _, err := (OS{}).RealPath("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("RealPath of a missing file: %v", err)
	}
}

func TestOSChownAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	f, err := OS{}.Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Chown(st.Uid, st.Gid); err != nil {
		t.Errorf("Chown to the file's own owner: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("a second Close: %v", err)
	}
	if _, err := f.ReadAt(make([]byte, 1), 0); err == nil {
		t.Error("ReadAt worked after Close")
	}
	if _, err := f.TryLock(); err == nil {
		t.Error("TryLock worked after Close")
	}
}

func TestRetryOnEINTR(t *testing.T) {
	calls := 0
	err := retry(func() error {
		calls++
		if calls < 4 {
			return syscall.EINTR
		}
		return syscall.EIO
	})
	if calls != 4 || err != syscall.EIO {
		t.Errorf("retry made %d calls and returned %v, where 4 and EIO are wanted", calls, err)
	}
}

func TestUmask(t *testing.T) {
	old := syscall.Umask(0o027)
	got := Umask()
	syscall.Umask(old)
	if got != 0o027 {
		t.Errorf("Umask() = %#o under umask 0o027", got)
	}
	if got := Umask(); got != fs.FileMode(old) {
		t.Errorf("Umask() = %#o under umask %#o", got, old)
	}
}
