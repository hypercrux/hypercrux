// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// The file rules (F7), through the public package. A database reached by a
// symbolic link, a chain of them, a link to its folder or a relative path is
// the file the path leads to, whatever the working folder is later, and the
// links stay as they were. A file with two names, a folder, a FIFO, a socket,
// a device and a symbolic link to nothing are refused, with 0.x's kind of
// error where 0.x refuses them too. A hard link made while the database is
// open stops its writes, and a FIFO moved over it stops its reads and writes,
// until the path holds a database that keeps the rules again.

// errKind is the kind of an error by the differential harness's rule
// (difftest.Kind): "ok", "not found", "invalid" or "error", with each
// engine's own two errors.
func errKind(err, notFound, invalid error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, notFound):
		return "not found"
	case errors.Is(err, invalid):
		return "invalid"
	}
	return "error"
}

// openOnBoth opens path on 0.x and on the Beta, each within a deadline, so an
// open that waits can't hang the run, closes what opened, and returns the
// kind of each one's error, with the Beta's error.
func openOnBoth(t *testing.T, zeroPath, betaPath string) (zero, beta string, err error) {
	t.Helper()
	type result struct {
		kind string
		err  error
	}
	run := func(what string, open func() (string, error)) result {
		c := make(chan result, 1)
		go func() {
			k, err := open()
			c <- result{k, err}
		}()
		select {
		case r := <-c:
			return r
		case <-time.After(30 * time.Second):
			t.Fatalf("%s was still waiting after 30 seconds", what)
			return result{}
		}
	}
	if zeroPath != "" {
		zero = run("0.x's Open", func() (string, error) {
			db, err := zx.Open(zeroPath)
			if err == nil {
				// 0.x's SQLite opens the file when it first reads it.
				err = db.Put("docs:1", zx.Fields{"n": 1})
				db.Close()
			}
			return errKind(err, zx.ErrNotFound, zx.ErrInvalid), err
		}).kind
	}
	r := run("the Beta's Open", func() (string, error) {
		db, err := hc.Open(betaPath)
		if err == nil {
			db.Close()
		}
		return errKind(err, hc.ErrNotFound, hc.ErrInvalid), err
	})
	return zero, r.kind, r.err
}

// makeLink makes a symbolic link at name to target, and returns name.
func makeLink(t *testing.T, target, name string) string {
	t.Helper()
	ok(t, os.Symlink(target, name))
	return name
}

// realDir returns a new temporary folder by its real path.
func realTempFolder(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	ok(t, err)
	return dir
}

// linksStay checks that each symbolic link still leads where it did.
func linksStay(t *testing.T, links map[string]string) {
	t.Helper()
	for link, target := range links {
		if got, err := os.Readlink(link); err != nil || got != target {
			t.Errorf("the symbolic link %s leads to %q, %v, where it led to %q", link, got, err, target)
		}
	}
}

// TestThePathIsMadeReal: a database opened by a symbolic link to its file, a
// chain of links, a link to its folder, a relative path, or a relative path
// from a working folder reached through a link, is the file the path leads
// to. Path gives the path as it was given, as in 0.x. What's put through it
// is in that file, the links stay as they were, and for a relative path,
// what's put once the working folder has changed goes into the same file,
// while a database the path names from the new working folder stays as it
// was. Another process's commit by the real path is read through the link,
// and so is a backup moved into place there.
func TestThePathIsMadeReal(t *testing.T) {
	for _, c := range []struct {
		name string
		lay  func(t *testing.T, base string) (path string, links map[string]string, wd string)
	}{
		{"a symbolic link to its file", func(t *testing.T, base string) (string, map[string]string, string) {
			return makeLink(t, "real/db", filepath.Join(base, "link")), map[string]string{filepath.Join(base, "link"): "real/db"}, ""
		}},
		{"a chain of links", func(t *testing.T, base string) (string, map[string]string, string) {
			makeLink(t, filepath.Join(base, "real", "db"), filepath.Join(base, "one"))
			makeLink(t, "one", filepath.Join(base, "two"))
			return filepath.Join(base, "two"), map[string]string{filepath.Join(base, "one"): filepath.Join(base, "real", "db"), filepath.Join(base, "two"): "one"}, ""
		}},
		{"a link to its folder", func(t *testing.T, base string) (string, map[string]string, string) {
			dir := makeLink(t, "real", filepath.Join(base, "dir"))
			return filepath.Join(dir, "db"), map[string]string{dir: "real"}, ""
		}},
		{"a relative path", func(t *testing.T, base string) (string, map[string]string, string) {
			return filepath.Join("real", "db"), nil, base
		}},
		{"a relative path from a folder reached through a link", func(t *testing.T, base string) (string, map[string]string, string) {
			dir := makeLink(t, "real", filepath.Join(base, "dir"))
			return "db", map[string]string{dir: "real"}, dir
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := realTempFolder(t)
			ok(t, os.Mkdir(filepath.Join(base, "real"), 0o755))
			real := filepath.Join(base, "real", "db")
			first := open(t, real)
			ok(t, first.Put("docs:0", hc.Fields{"n": 0}))
			ok(t, first.Close())

			path, links, wd := c.lay(t, base)
			if wd != "" {
				t.Chdir(wd)
			}
			db := open(t, path)
			if db.Path() != path {
				t.Errorf("Path gives %q, where the path given was %q", db.Path(), path)
			}
			ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
			if wd != "" {
				newWD := filepath.Join(realTempFolder(t), "w")
				there := filepath.Join(newWD, path)
				ok(t, os.MkdirAll(filepath.Dir(there), 0o755))
				decoy := open(t, there)
				ok(t, decoy.Put("decoy:1", hc.Fields{"n": 1}))
				ok(t, decoy.Close())
				t.Chdir(newWD)
				defer func() {
					d := open(t, there)
					if _, err := d.Get("docs:2"); !errors.Is(err, hc.ErrNotFound) {
						t.Errorf("the database the relative path names from the new working folder gives docs:2: %v", err)
					}
				}()
			}
			ok(t, db.Put("docs:2", hc.Fields{"n": 2}))
			linksStay(t, links)

			other := open(t, real)
			for k := range 3 {
				if f, err := other.Get("docs:" + strconv.Itoa(k)); err != nil || f["n"] != int64(k) {
					t.Errorf("by the real path, docs:%d gives %v, %v", k, f, err)
				}
			}
			ok(t, other.Put("docs:3", hc.Fields{"n": 3}))
			if f, err := db.Get("docs:3"); err != nil || f["n"] != int64(3) {
				t.Errorf("the commit by the real path, read through %q, gives %v, %v", path, f, err)
			}

			backup := filepath.Join(base, "real", "backup")
			b := open(t, backup)
			ok(t, b.Put("restored:1", hc.Fields{"n": 1}))
			ok(t, b.Close())
			ok(t, os.Rename(backup, real))
			if f, err := db.Get("restored:1"); err != nil || f["n"] != int64(1) {
				t.Errorf("the backup moved into place, read through %q, gives %v, %v", path, f, err)
			}
			ok(t, db.Put("docs:4", hc.Fields{"n": 4}))
			if f, err := open(t, real).Get("docs:4"); err != nil || f["n"] != int64(4) {
				t.Errorf("by the real path, after the backup, docs:4 gives %v, %v", f, err)
			}
			linksStay(t, links)
		})
	}
}

// fifoWithWriter makes a FIFO at path with a writer waiting at its other end, on
// a goroutine of its own, until something opens the FIFO for reading. The
// writer opens the FIFO through a descriptor that names it without opening it
// (O_PATH, 0x200000 on amd64 and arm64, which the syscall package doesn't
// name), so it waits on the FIFO wherever its name goes. stillWaits reports
// whether the writer still waits after a tenth of a second, and the writer is
// let go when the test ends.
func fifoWithWriter(t *testing.T, path string) (stillWaits func() bool) {
	t.Helper()
	ok(t, syscall.Mkfifo(path, 0o644))
	ref, err := syscall.Open(path, 0x200000|syscall.O_CLOEXEC, 0)
	ok(t, err)
	via := "/proc/self/fd/" + strconv.Itoa(ref)
	opened := make(chan struct{})
	go func() {
		if f, err := os.OpenFile(via, os.O_WRONLY, 0); err == nil {
			f.Close()
		}
		close(opened)
	}()
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			if fd, err := syscall.Open(via, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err == nil {
				<-opened
				syscall.Close(fd)
			}
			syscall.Close(ref)
		})
	})
	return func() bool {
		select {
		case <-opened:
			return false
		case <-time.After(100 * time.Millisecond):
			return true
		}
	}
}

// TestWhatTheFileRulesRefuse: Open refuses a file with two names, made by a
// hard link, where 0.x opens it, with an error that wraps ErrInvalid. It
// refuses a folder, a FIFO and a socket as 0.x does, with a plain error, of
// the kind "error", which wraps ErrNotDatabase, and a device too, here
// /dev/null through a link. A FIFO with a writer waiting at its other end is
// refused within the deadline, and its writer goes on waiting. A symbolic link
// to nothing, where 0.x makes its database where the link leads, is refused
// with an error that wraps ErrInvalid, and nothing is made. Whatever is
// refused stays as it was.
func TestWhatTheFileRulesRefuse(t *testing.T) {
	dir := t.TempDir()
	twoNames := filepath.Join(dir, "two.hcx")
	db := open(t, twoNames)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	ok(t, db.Close())
	ok(t, os.Link(twoNames, filepath.Join(dir, "second.hcx")))
	zeroTwo := filepath.Join(dir, "two.db")
	z, err := zx.Open(zeroTwo)
	ok(t, err)
	ok(t, z.Put("docs:1", zx.Fields{"n": 1}))
	ok(t, z.Close())
	ok(t, os.Link(zeroTwo, filepath.Join(dir, "second.db")))
	before, err := os.ReadFile(twoNames)
	ok(t, err)

	folder := filepath.Join(dir, "folder")
	ok(t, os.Mkdir(folder, 0o755))
	fifo := filepath.Join(dir, "fifo")
	stillWaits := fifoWithWriter(t, fifo)
	zeroFIFO := filepath.Join(dir, "zero-fifo") // one of its own, since 0.x's SQLite opens it
	ok(t, syscall.Mkfifo(zeroFIFO, 0o644))
	// A socket's path holds 107 bytes at most, so it goes in a folder of its
	// own with a short path.
	sockets, err := os.MkdirTemp("", "hc")
	ok(t, err)
	defer os.RemoveAll(sockets)
	socket := filepath.Join(sockets, "s")
	ln, err := net.Listen("unix", socket)
	ok(t, err)
	defer ln.Close()
	device := makeLink(t, "/dev/null", filepath.Join(dir, "device"))
	nowhere := makeLink(t, "nowhere", filepath.Join(dir, "dangling"))
	zeroNowhere := makeLink(t, "zero-nowhere", filepath.Join(dir, "zero-dangling"))

	for _, c := range []struct {
		name, zeroPath, betaPath string
		zero, beta               string // the kinds of their errors
		betaErr                  error  // what the Beta's error wraps
		says                     string // what the Beta's error says
	}{
		{"a file with two names", zeroTwo, twoNames, "ok", "invalid", hc.ErrInvalid, "2 names"},
		{"a folder", folder, folder, "error", "error", hc.ErrNotDatabase, "a folder"},
		{"a FIFO", zeroFIFO, fifo, "error", "error", hc.ErrNotDatabase, "a FIFO"},
		{"a socket", socket, socket, "error", "error", hc.ErrNotDatabase, "a socket"},
		{"a device", "", device, "", "error", hc.ErrNotDatabase, "a character device"},
		{"a symbolic link to nothing", zeroNowhere, nowhere, "ok", "invalid", hc.ErrInvalid, "symbolic link"},
	} {
		zero, beta, err := openOnBoth(t, c.zeroPath, c.betaPath)
		if zero != c.zero || beta != c.beta || !errors.Is(err, c.betaErr) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: 0.x gives %q and the Beta %q, %v, where %q and %q, wrapping %v, are wanted", c.name, zero, beta, err, c.zero, c.beta, c.betaErr)
		}
	}

	if !stillWaits() {
		t.Error("the writer at the FIFO's other end stopped waiting: the Beta opened the FIFO")
	}
	after, err := os.ReadFile(twoNames)
	ok(t, err)
	if string(after) != string(before) {
		t.Error("the file with two names changed")
	}
	for path, mode := range map[string]fs.FileMode{folder: fs.ModeDir, fifo: fs.ModeNamedPipe, socket: fs.ModeSocket, nowhere: fs.ModeSymlink, "/dev/null": fs.ModeCharDevice} {
		if info, err := os.Lstat(path); err != nil || info.Mode()&mode == 0 {
			t.Errorf("%s is no longer what it was: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "nowhere")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the Beta made something where the link to nothing leads: %v", err)
	}
	entries, err := os.ReadDir(dir)
	ok(t, err)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".new-") || strings.HasSuffix(e.Name(), ".compact") {
			t.Errorf("the folder holds %s", e.Name())
		}
	}
	if got, err := os.ReadDir(folder); err != nil || len(got) != 0 {
		t.Errorf("the folder that was refused holds %v, %v", got, err)
	}

	ok(t, os.Remove(filepath.Join(dir, "second.hcx")))
	if f, err := open(t, twoNames).Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Errorf("once its second name has gone, the file gives docs:1 as %v, %v", f, err)
	}
}

// TestAHardLinkMadeWhileOpen: a hard link made to a database that's open
// stops every write through it, Updates that change nothing included, since
// each takes the write lock, with an error that wraps ErrInvalid, while reads
// go on. Once the second name has gone, writes go on.
func TestAHardLinkMadeWhileOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.hcx")
	db := open(t, path)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	second := filepath.Join(dir, "second.hcx")
	ok(t, os.Link(path, second))
	wantErr(t, db.Put("docs:2", hc.Fields{"n": 2}), hc.ErrInvalid)
	wantErr(t, db.Update(func(tx *hc.Tx) error { return nil }), hc.ErrInvalid)
	if f, err := db.Get("docs:1"); err != nil || f["n"] != int64(1) {
		t.Errorf("a read with the second name there gives %v, %v", f, err)
	}
	if _, err := db.Get("docs:2"); !errors.Is(err, hc.ErrNotFound) {
		t.Errorf("the refused put left docs:2: %v", err)
	}
	ok(t, os.Remove(second))
	ok(t, db.Put("docs:2", hc.Fields{"n": 2}))
	if f, err := open(t, path).Get("docs:2"); err != nil || f["n"] != int64(2) {
		t.Errorf("by a fresh handle, docs:2 gives %v, %v", f, err)
	}
}

// TestAFIFOMovedOverTheDatabase: a FIFO moved over a database that's open,
// with a writer waiting at its other end, makes every read and write through
// the handle fail with an error that wraps ErrNotDatabase, and none opens it,
// so the writer goes on waiting and the FIFO stays at the path. Once a
// database is moved back there, the handle reads it and writes to it.
func TestAFIFOMovedOverTheDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.hcx")
	db := open(t, path)
	ok(t, db.Put("docs:1", hc.Fields{"n": 1}))
	fifo := filepath.Join(dir, "fifo")
	stillWaits := fifoWithWriter(t, fifo)
	ok(t, os.Rename(fifo, path))
	_, err := db.Get("docs:1")
	wantErr(t, err, hc.ErrNotDatabase)
	wantErr(t, db.Put("docs:2", hc.Fields{"n": 2}), hc.ErrNotDatabase)
	_, err = db.Get("docs:1")
	wantErr(t, err, hc.ErrNotDatabase)
	if !stillWaits() {
		t.Error("the writer at the FIFO's other end stopped waiting: the FIFO was opened")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("the path no longer holds the FIFO: %v", err)
	}

	back := filepath.Join(dir, "back.hcx")
	b := open(t, back)
	ok(t, b.Put("back:1", hc.Fields{"n": 1}))
	ok(t, b.Close())
	ok(t, os.Rename(back, path))
	if f, err := db.Get("back:1"); err != nil || f["n"] != int64(1) {
		t.Errorf("once a database is back at the path, back:1 gives %v, %v", f, err)
	}
	ok(t, db.Put("docs:2", hc.Fields{"n": 2}))
	if f, err := open(t, path).Get("docs:2"); err != nil || f["n"] != int64(2) {
		t.Errorf("by a fresh handle, docs:2 gives %v, %v", f, err)
	}
}
