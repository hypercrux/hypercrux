// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
)

// TestNames checks the calls on names against what Linux does, as fsys.FS
// describes it, and that names last through a cut here.
func TestNames(t *testing.T) {
	d := fault.New(1)
	sys := d.FS()
	is := func(err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("got %v, where %v was wanted", err, want)
		}
	}

	f, err := sys.Create("/db", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	in, err := sys.Stat("db")
	if err != nil {
		t.Fatal(err)
	}
	if in.Mode != 0o640 || in.Nlink != 1 || in.Size != 0 || in.Uid != os.Getuid() || in.Gid != os.Getgid() || in.Ino == 0 {
		t.Errorf("a new file's Info is %+v", in)
	}
	if fin, err := f.Stat(); err != nil || fin != in {
		t.Errorf("the open file's Info is %+v, %v, and the path's %+v", fin, err, in)
	}
	_, err = sys.Create("/db", 0o640)
	is(err, fs.ErrExist)
	_, err = sys.Create("/no/db", 0o640)
	is(err, fs.ErrNotExist)
	_, err = sys.Create("/db/x", 0o640)
	is(err, syscall.ENOTDIR)
	_, err = sys.Open("/missing")
	is(err, fs.ErrNotExist)
	_, err = sys.Open("/")
	is(err, syscall.EISDIR)
	for _, err := range []error{sys.Remove(""), sys.SyncDir(""), sys.Rename("", "/x"), sys.Rename("/db", "")} {
		is(err, fs.ErrNotExist)
	}
	_, err = sys.Open("")
	is(err, fs.ErrNotExist)
	_, err = sys.Create("", 0o600)
	is(err, fs.ErrNotExist)

	// Folders.
	if err := d.Mkdir("/x/y"); err != nil {
		t.Fatal(err)
	}
	if err := d.Mkdir("/x/y"); err != nil {
		t.Fatal(err)
	}
	is(d.Mkdir("/db/z"), syscall.ENOTDIR)
	if in, err := sys.Stat("/x"); err != nil || !in.Mode.IsDir() {
		t.Errorf("a folder's Info is %+v, %v", in, err)
	}
	for _, name := range []string{"B", "b", "a", "a.new-1"} {
		if _, err := sys.Create("/x/"+name, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := sys.List("/x"); err != nil || !slices.Equal(got, []string{"B", "a", "a.new-1", "b", "y"}) {
		t.Errorf("List gives %q, %v", got, err)
	}
	if got, err := sys.List("/x/y"); err != nil || len(got) != 0 {
		t.Errorf("List of an empty folder gives %q, %v", got, err)
	}
	_, err = sys.List("/db")
	is(err, syscall.ENOTDIR)
	_, err = sys.List("/missing")
	is(err, fs.ErrNotExist)
	is(sys.SyncDir("/x"), nil)
	is(sys.SyncDir("/db"), syscall.ENOTDIR)
	is(sys.SyncDir("/missing"), fs.ErrNotExist)
	for give, want := range map[string]string{"db": "/db", "/x/../db": "/db", "x/y/": "/x/y", "/": "/"} {
		if got, err := sys.RealPath(give); got != want || err != nil {
			t.Errorf("RealPath(%q) gives %q, %v", give, got, err)
		}
	}
	_, err = sys.RealPath("/x/missing")
	is(err, fs.ErrNotExist)

	// Renames and removals.
	mustWrite(t, f, []byte("old file"), 0)
	g, err := sys.Create("/new", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, g, []byte("new file"), 0)
	is(sys.RenameNoReplace("/new", "/db"), fs.ErrExist)
	is(sys.RenameNoReplace("/new", "/new"), fs.ErrExist)
	is(sys.Rename("/new", "/new"), nil)
	is(sys.Rename("/missing", "/db"), fs.ErrNotExist)
	is(sys.Rename("/x", "/elsewhere"), syscall.EISDIR)
	is(sys.Rename("/new", "/x"), syscall.EISDIR)
	is(sys.Rename("/new", "/missing/new"), fs.ErrNotExist)
	is(sys.Rename("/new", "/db"), nil)
	if old, err := f.Stat(); err != nil || old.Nlink != 0 {
		t.Errorf("the replaced file's Info is %+v, %v", old, err)
	}
	b := make([]byte, 8)
	if n, err := f.ReadAt(b, 0); n != 8 || err != nil || string(b) != "old file" {
		t.Errorf("the replaced file reads %q, %v", b[:n], err)
	}
	at, _ := sys.Stat("/db")
	gin, _ := g.Stat()
	if fin, _ := f.Stat(); at.Same(fin) || !at.Same(gin) {
		t.Error("the path doesn't name the file renamed to it")
	}
	if got := readAll(t, sys, "/db"); string(got) != "new file" {
		t.Errorf("/db holds %q", got)
	}
	_, err = sys.Stat("/new")
	is(err, fs.ErrNotExist)
	is(sys.RenameNoReplace("/db", "/other"), nil)
	is(sys.Remove("/other"), nil)
	if gin, err := g.Stat(); err != nil || gin.Nlink != 0 {
		t.Errorf("a removed file's Info is %+v, %v", gin, err)
	}
	mustWrite(t, g, []byte("still open"), 0)
	is(sys.Remove("/other"), fs.ErrNotExist)
	is(sys.Remove("/x"), syscall.EISDIR)

	// Names last through a cut, synced or not, and a file with no name is
	// gone.
	h, err := sys.Create("/kept", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Chown(7, 8); err != nil {
		t.Fatal(err)
	}
	if err := h.Chown(-1, 9); err != nil {
		t.Fatal(err)
	}
	is(sys.Rename("/x/a", "/x/moved"), nil)
	is(sys.Remove("/x/b"), nil)
	before, err := sys.List("/x")
	if err != nil {
		t.Fatal(err)
	}
	d.Cut()
	sys = d.FS()
	if got, err := sys.List("/x"); err != nil || !slices.Equal(got, before) {
		t.Errorf("after a cut /x holds %q, %v, where it held %q", got, err, before)
	}
	if got, err := sys.List("/"); err != nil || !slices.Equal(got, []string{"kept", "x"}) {
		t.Errorf("after a cut / holds %q, %v", got, err)
	}
	if in, err := sys.Stat("/kept"); err != nil || in.Uid != 7 || in.Gid != 9 || in.Mode != 0o600 {
		t.Errorf("after a cut /kept is %+v, %v", in, err)
	}
}

// TestFileCalls checks the calls on an open file that the crash tests
// don't: offsets and sizes out of range, a read past the end, a write
// past it leaving zeros, a Truncate growing a file, and calls on a closed
// file.
func TestFileCalls(t *testing.T) {
	d := fault.New(1)
	sys := d.FS()
	f, err := sys.Create("/f", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	is := func(err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("got %v, where %v was wanted", err, want)
		}
	}
	_, err = f.WriteAt([]byte("x"), -1)
	is(err, syscall.EINVAL)
	_, err = f.WriteAt([]byte("x"), fault.MaxSize)
	is(err, syscall.EFBIG)
	_, err = f.ReadAt(make([]byte, 1), -1)
	is(err, syscall.EINVAL)
	is(f.Truncate(-1), syscall.EINVAL)
	is(f.Truncate(fault.MaxSize+1), syscall.EFBIG)
	if n, err := f.ReadAt(make([]byte, 4), 0); n != 0 || err != io.EOF {
		t.Errorf("a read of an empty file gives %d, %v", n, err)
	}
	if n, err := f.WriteAt(nil, 100); n != 0 || err != nil {
		t.Errorf("an empty write gives %d, %v", n, err)
	}
	mustWrite(t, f, []byte("end"), 5)
	if got := readAll(t, sys, "/f"); !bytes.Equal(got, []byte("\x00\x00\x00\x00\x00end")) {
		t.Errorf("a write past the end leaves %q", got)
	}
	b := make([]byte, 6)
	if n, err := f.ReadAt(b, 4); n != 4 || err != io.EOF || string(b[:n]) != "\x00end" {
		t.Errorf("a read past the end gives %d, %q, %v", n, b[:n], err)
	}
	if n, err := f.ReadAt(b, 8); n != 0 || err != io.EOF {
		t.Errorf("a read from the end gives %d, %v", n, err)
	}
	is(f.Truncate(2), nil)
	is(f.Truncate(6), nil)
	if got := readAll(t, sys, "/f"); !bytes.Equal(got, make([]byte, 6)) {
		t.Errorf("a file cut and grown holds %q", got)
	}
	is(f.Close(), nil)
	_, err = f.ReadAt(b, 0)
	is(err, fs.ErrClosed)
	_, err = f.WriteAt(b, 0)
	is(err, fs.ErrClosed)
	is(f.Sync(), fs.ErrClosed)
	is(f.Truncate(0), fs.ErrClosed)
	_, err = f.Stat()
	is(err, fs.ErrClosed)
	is(f.Chown(1, 1), fs.ErrClosed)
	_, err = f.TryLock()
	is(err, fs.ErrClosed)
	is(f.Unlock(), fs.ErrClosed)
	is(f.Close(), fs.ErrClosed)
}

// TestCalls checks the counts: by kind and by path, a rename for both its
// paths, a file's calls on the path it was opened with, relative paths
// cleaned, and nothing for a closed file or a dead process.
func TestCalls(t *testing.T) {
	d := fault.New(1)
	sys := d.FS()
	f, err := sys.Create("/f", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, f, []byte("x"), 0)
	if err := sys.Rename("/f", "/g"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, f, []byte("y"), 1)
	g, err := sys.Open("g")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, g, []byte("z"), 2)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g.Close()
	g.Sync()
	d.Cut()
	f.Sync()
	sys.Stat("/g")
	for _, c := range []struct {
		op   fault.Op
		path string
		want int
	}{
		{fault.Any, "", 7},
		{fault.Any, "/f", 4},
		{fault.Any, "f", 4},
		{fault.Any, "/g", 4},
		{fault.WriteAt, "", 3},
		{fault.WriteAt, "/f", 2},
		{fault.WriteAt, "/g", 1},
		{fault.Rename, "/f", 1},
		{fault.Rename, "/g", 1},
		{fault.Rename, "", 1},
		{fault.Close, "", 1},
		{fault.Sync, "", 0},
		{fault.Stat, "", 0},
	} {
		if got := d.Calls(c.op, c.path); got != c.want {
			t.Errorf("Calls(%v, %q) is %d, where %d were made", c.op, c.path, got, c.want)
		}
	}
	d = fault.New(1)
	f, err = d.FS().Create("/f", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	d.Add(fault.Rule{Op: fault.WriteAt, N: 1})
	if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	if got := d.Calls(fault.WriteAt, "/f"); got != 1 {
		t.Errorf("a call a rule failed counted %d times", got)
	}
	if fault.Sync.String() != "Sync" || fault.RenameNoReplace.String() != "RenameNoReplace" || fault.Op(99).String() != "Op(99)" {
		t.Error("the kinds' names")
	}
}
