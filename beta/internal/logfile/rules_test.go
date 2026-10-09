// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// The file rules (F7), through the log, on real files under t.TempDir(): a
// database reached by a symbolic link, a chain of them, a link to its folder
// or a relative path, which the log opens by its real path; a file with two
// names, refused at Open, at a reload, and by a writer once a hard link is
// made while it's open; anything that isn't a regular file, refused before
// it's opened, at Open and at a reload; a symbolic link to nothing; and a link
// that takes the path while the database is open. Then the rules on the fault
// layer, where its names show them.

// pathsFS is the real calls, noting each call on names with its paths, such
// as "stat /x/db", so a test can see which paths the log worked from.
type pathsFS struct {
	fsys.FS
	mu    sync.Mutex
	calls []string
}

func (p *pathsFS) note(call string, paths ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call+" "+strings.Join(paths, " "))
}

func (p *pathsFS) Open(path string) (fsys.File, error) {
	p.note("open", path)
	return p.FS.Open(path)
}

func (p *pathsFS) Create(path string, perm fs.FileMode) (fsys.File, error) {
	p.note("create", path)
	return p.FS.Create(path, perm)
}

func (p *pathsFS) Rename(from, to string) error {
	p.note("rename", from, to)
	return p.FS.Rename(from, to)
}

func (p *pathsFS) RenameNoReplace(from, to string) error {
	p.note("renameat2", from, to)
	return p.FS.RenameNoReplace(from, to)
}

func (p *pathsFS) Remove(path string) error {
	p.note("remove", path)
	return p.FS.Remove(path)
}

func (p *pathsFS) SyncDir(dir string) error {
	p.note("syncdir", dir)
	return p.FS.SyncDir(dir)
}

func (p *pathsFS) Stat(path string) (fsys.Info, error) {
	p.note("stat", path)
	return p.FS.Stat(path)
}

func (p *pathsFS) RealPath(path string) (string, error) {
	p.note("realpath", path)
	return p.FS.RealPath(path)
}

func (p *pathsFS) List(dir string) ([]string, error) {
	p.note("list", dir)
	return p.FS.List(dir)
}

// taken returns the calls noted so far, and forgets them.
func (p *pathsFS) taken() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	calls := p.calls
	p.calls = nil
	return calls
}

// outside returns the calls that name a path outside the folder dir, leaving
// out the realpath calls when realpaths is set.
func outside(calls []string, dir string, realpaths bool) []string {
	var out []string
	for _, c := range calls {
		op, rest, _ := strings.Cut(c, " ")
		if op == "realpath" && realpaths {
			continue
		}
		for _, p := range strings.Fields(rest) {
			if p != dir && !strings.HasPrefix(p, dir+"/") {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// realTempDir returns a new temporary folder by its real path.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// symlink makes a symbolic link at name to target, and returns name.
func symlink(t *testing.T, target, name string) string {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
	return name
}

// linksAsTheyWere checks that each of the symbolic links is still a link,
// to the target it had.
func linksAsTheyWere(t *testing.T, links map[string]string) {
	t.Helper()
	for link, target := range links {
		got, err := os.Readlink(link)
		if err != nil || got != target {
			t.Errorf("the symbolic link %s leads to %q, %v, where it led to %q", link, got, err, target)
		}
	}
}

// readlinks returns the targets of the symbolic links named.
func readlinks(t *testing.T, names ...string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, name := range names {
		target, err := os.Readlink(name)
		if err != nil {
			t.Fatal(err)
		}
		m[name] = target
	}
	return m
}

// TestADatabaseIsOpenedByItsRealPath: whatever path reaches a database, a
// symbolic link to its file, a chain of them, a link to its folder, a
// relative path, or one from a working folder reached through a link, the
// log opens it by its real path, and works from that path and its folder
// alone from then on: every call on names after Open names the real folder
// or a file in it. Commits go into the file the path led to, a compaction
// replaces that file and leaves every link as it was, and so does a reload
// after another process's compaction. For a relative path, a change of the
// working folder after Open changes nothing: the commits after it, and the
// compaction, go into the same file, and a database of the same name in the
// new working folder stays as it was.
func TestADatabaseIsOpenedByItsRealPath(t *testing.T) {
	cases := []struct {
		name string
		// lay makes the links in base, which holds the database's folder real,
		// and returns the path to open by, the links it made, and the folder to
		// work from, or "" to stay where the test runs.
		lay func(t *testing.T, base string) (path string, links []string, wd string)
	}{
		{"a symbolic link to its file", func(t *testing.T, base string) (string, []string, string) {
			link := symlink(t, "real/db", filepath.Join(base, "link"))
			return link, []string{link}, ""
		}},
		{"a chain of links", func(t *testing.T, base string) (string, []string, string) {
			one := symlink(t, filepath.Join(base, "real", "db"), filepath.Join(base, "one"))
			two := symlink(t, "one", filepath.Join(base, "two"))
			three := symlink(t, "./two", filepath.Join(base, "three"))
			return three, []string{one, two, three}, ""
		}},
		{"a link to its folder", func(t *testing.T, base string) (string, []string, string) {
			dir := symlink(t, "real", filepath.Join(base, "dir"))
			return filepath.Join(dir, "db"), []string{dir}, ""
		}},
		{"a relative path", func(t *testing.T, base string) (string, []string, string) {
			return filepath.Join("real", "db"), nil, base
		}},
		{"a relative path from a folder reached through a link", func(t *testing.T, base string) (string, []string, string) {
			dir := symlink(t, "real", filepath.Join(base, "dir"))
			return filepath.Join("..", "dir", "db"), []string{dir}, dir
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := realTempDir(t)
			realDir := filepath.Join(base, "real")
			if err := os.Mkdir(realDir, 0o755); err != nil {
				t.Fatal(err)
			}
			real := filepath.Join(realDir, "db")
			first := openDBT(t, fsys.OS{}, real, Options{})
			fill(t, first, 1, 10)
			first.l.Close()

			path, links, wd := c.lay(t, base)
			targets := readlinks(t, links...)
			if wd != "" {
				t.Chdir(wd)
			}
			p := &pathsFS{FS: fsys.OS{}}
			d := openDBT(t, p, path, Options{})
			if got := d.l.Path(); got != real {
				t.Fatalf("the Log works from %s, where the real path is %s", got, real)
			}
			if out := outside(p.taken(), realDir, true); out != nil {
				t.Errorf("Open's calls outside the real folder: %q", out)
			}

			fill(t, d, 11, 20)
			if err := d.compact(); err != nil {
				t.Fatal(err)
			}
			if wd != "" {
				// Another working folder, where the same relative path names
				// another database.
				newWD := filepath.Join(realTempDir(t), "w")
				there := filepath.Join(newWD, path)
				for _, dir := range []string{newWD, filepath.Dir(there)} {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				another(t, there, "decoy")
				decoy := fileBytes(t, there)
				t.Chdir(newWD)
				defer func() {
					if !bytes.Equal(fileBytes(t, there), decoy) {
						t.Error("the database the relative path names from the new working folder changed")
					}
				}()
			}
			fill(t, d, 21, 25)
			if out := outside(p.taken(), realDir, false); out != nil {
				t.Errorf("calls outside the real folder after Open: %q", out)
			}

			if h := headerOf(t, real); h.Gen != 2 {
				t.Errorf("the file at the real path is of generation %d, where the compaction made 2", h.Gen)
			}
			if got := stateOfFile(t, real); got != d.state() {
				t.Errorf("the file at the real path holds\n%s\nwhere the copy holds\n%s", got, d.state())
			}
			linksAsTheyWere(t, targets)
			if got := names(t, realDir); got != "db" {
				t.Errorf("the real folder holds %q", got)
			}

			// Another process compacts, by the real path, and the Log reloads.
			o := openDBT(t, fsys.OS{}, real, Options{})
			fill(t, o, 26, 30)
			if err := o.compact(); err != nil {
				t.Fatal(err)
			}
			if err := d.l.Follow(); !errors.Is(err, ErrReplaced) {
				t.Fatalf("Follow after another's compaction: %v", err)
			}
			if err := d.l.Reload(); err != nil {
				t.Fatal(err)
			}
			follow(t, d.l)
			if d.state() != o.state() {
				t.Errorf("after the reload the copy holds\n%s\nwhere the file holds\n%s", d.state(), o.state())
			}
			if h := headerOf(t, real); h.Gen != 3 {
				t.Errorf("the file at the real path is of generation %d after the second compaction", h.Gen)
			}
			if out := outside(p.taken(), realDir, false); out != nil {
				t.Errorf("the reload's calls outside the real folder: %q", out)
			}
			linksAsTheyWere(t, targets)
			if got := names(t, realDir); got != "db" {
				t.Errorf("after the reload the real folder holds %q", got)
			}
		})
	}
}

// TestANewDatabaseGoesInItsFoldersRealPath: a database made by a path that
// names nothing yet goes in its folder's real path, with its name: the .new-
// file, its rename into place and the folder's sync, through a link to the
// folder or by a relative path, and the link stays as it was.
func TestANewDatabaseGoesInItsFoldersRealPath(t *testing.T) {
	base := realTempDir(t)
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := symlink(t, "real", filepath.Join(base, "dir"))
	for _, c := range []struct {
		name, path, wd string
	}{
		{"through a link to its folder", filepath.Join(dir, "linked"), ""},
		{"by a relative path", filepath.Join("real", "relative"), base},
		{"by a relative path from the linked folder", "inside", dir},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.wd != "" {
				t.Chdir(c.wd)
			}
			p := &pathsFS{FS: fsys.OS{}}
			l, _ := openWith(t, p, c.path, Options{})
			want := filepath.Join(realDir, filepath.Base(c.path))
			if l.Path() != want {
				t.Errorf("the new database's Log works from %s, where %s is wanted", l.Path(), want)
			}
			calls := p.taken()
			if out := outside(calls, realDir, true); out != nil {
				t.Errorf("calls outside the real folder: %q", out)
			}
			made := slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "create "+want+".new-") })
			moved := slices.ContainsFunc(calls, func(c string) bool {
				return strings.HasPrefix(c, "renameat2 "+want+".new-") && strings.HasSuffix(c, " "+want)
			})
			if !made || !moved || !slices.Contains(calls, "syncdir "+realDir) {
				t.Errorf("the creation's calls were %q", calls)
			}
			commit(t, l, table("one"))
			if _, err := os.Lstat(want); err != nil {
				t.Fatal(err)
			}
			reread(t, want).holds(t, table("one"))
		})
	}
	if got := names(t, realDir); got != "inside linked relative" {
		t.Errorf("the real folder holds %q", got)
	}
	if got := names(t, base); got != "dir real" {
		t.Errorf("the folder above holds %q", got)
	}
	linksAsTheyWere(t, map[string]string{dir: "real"})
}

// TestAFileWithTwoNamesIsRefused: a file with a second name, made by a hard
// link, isn't opened by either name, with an error that wraps errs.ErrInvalid,
// and nothing in the file or the folder changes. Once the second name has
// gone, it opens. A hard link made while the database is open stops every
// commit, at Lock, and every compaction, also one whose Lock came before the
// link, while reads go on. A backup with a second name, moved into place, is
// refused at the reload and at Lock, before the Target is reset, and read
// once the second name has gone.
func TestAFileWithTwoNamesIsRefused(t *testing.T) {
	t.Run("at Open", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		another(t, path, "one", "two")
		second := filepath.Join(dir, "second")
		if err := os.Link(path, second); err != nil {
			t.Fatal(err)
		}
		before := fileBytes(t, path)
		for _, p := range []string{path, second} {
			_, err := Open(fsys.OS{}, p, &recorder{}, Options{})
			if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "2 names") {
				t.Errorf("Open by %s gave %v, where a refusal of a file with 2 names is wanted", filepath.Base(p), err)
			}
		}
		if !bytes.Equal(fileBytes(t, path), before) {
			t.Error("the file changed")
		}
		if got := names(t, dir); got != "db second" {
			t.Errorf("the folder holds %q", got)
		}
		if err := os.Remove(second); err != nil {
			t.Fatal(err)
		}
		reread(t, path).holds(t, table("one"), table("two"))
	})

	t.Run("made while it's open", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		d := openDBT(t, fsys.OS{}, path, Options{})
		fill(t, d, 1, 5)
		f, _ := openLog(t, path, Options{}) // a reader, as another process would be
		second := filepath.Join(dir, "second")
		if err := os.Link(path, second); err != nil {
			t.Fatal(err)
		}
		before := fileBytes(t, path)
		if err := d.commit(func(tx *store.Tx) error { return compactOps(tx, 6) }); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("a commit once the file has a second name: %v", err)
		}
		if err := d.compact(); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("a compaction once the file has a second name: %v", err)
		}
		if !bytes.Equal(fileBytes(t, path), before) {
			t.Error("the file changed")
		}
		follow(t, f)
		if got := names(t, dir); got != "db second" {
			t.Errorf("the folder holds %q", got)
		}

		// A link made after Lock, while the transaction runs: the compaction
		// at its end fails before the switch, and leaves both names on the
		// live file.
		if err := os.Remove(second); err != nil {
			t.Fatal(err)
		}
		live := inode(t, path)
		if err := d.l.Lock(); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path, second); err != nil {
			t.Fatal(err)
		}
		err := d.compactLocked()
		d.l.Unlock()
		if !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("a compaction once the file has a second name, after its Lock: %v", err)
		}
		if !inode(t, path).Same(live) || !inode(t, second).Same(live) {
			t.Error("a name no longer leads to the live file")
		}
		if got := names(t, dir); got != "db second" {
			t.Errorf("after the compaction that failed, the folder holds %q", got)
		}

		if err := os.Remove(second); err != nil {
			t.Fatal(err)
		}
		fill(t, d, 6, 7)
		if err := d.compact(); err != nil {
			t.Fatal(err)
		}
		if got := stateOfFile(t, path); got != d.state() {
			t.Errorf("the file holds\n%s\nwhere the copy holds\n%s", got, d.state())
		}
	})

	t.Run("a backup with a second name moved into place", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		l, rec := openLog(t, path, Options{})
		commit(t, l, table("one"))
		backup := filepath.Join(dir, "backup")
		another(t, backup, "b1", "b2")
		second := filepath.Join(dir, "second")
		if err := os.Link(backup, second); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := l.Follow(); !errors.Is(err, ErrReplaced) {
				t.Fatalf("Follow with the backup at the path: %v", err)
			}
			if err := l.Reload(); !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("Reload with the backup at the path: %v", err)
			}
		}
		if err := lockErr(l); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("Lock with the backup at the path: %v", err)
		}
		if rec.resets != 0 {
			t.Errorf("the Target was reset %d times", rec.resets)
		}
		rec.holds(t) // the Log's own commit, which it hands to no Target
		if err := os.Remove(second); err != nil {
			t.Fatal(err)
		}
		if err := l.Reload(); err != nil {
			t.Fatal(err)
		}
		rec.holds(t, table("b1"), table("b2"))
		commit(t, l, table("three"))
		reread(t, path).holds(t, table("b1"), table("b2"), table("three"))
	})
}

// within runs fn on a goroutine of its own and returns what it returns. When
// fn hasn't returned after d, the test fails, so a call that blocks, as an
// open of a FIFO for reading does, can't hang the run.
func within(t *testing.T, d time.Duration, what string, fn func() error) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- fn() }()
	select {
	case err := <-errc:
		return err
	case <-time.After(d):
		t.Fatalf("%s was still waiting after %v", what, d)
		return nil
	}
}

// oPath is Linux's O_PATH, which the syscall package doesn't name: an open
// that gives a descriptor naming a file, without opening it for reading or
// writing, so it does nothing to a FIFO. It has this value on amd64 and arm64.
const oPath = 0x200000

// fifo is a FIFO with a writer waiting at its other end, on a goroutine of its
// own, until something opens the FIFO for reading. The writer opens the FIFO
// through a descriptor of the test's, so it waits on the FIFO wherever its
// name goes, and whether or not it still has one.
type fifo struct {
	ref    int           // the descriptor, made with oPath
	opened chan struct{} // closed once the writer's open has returned
	once   sync.Once
}

// newFIFO makes a FIFO at path, with its writer, and lets the writer go when
// the test ends.
func newFIFO(t *testing.T, path string) *fifo {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	w := &fifo{ref: ref, opened: make(chan struct{})}
	go func() {
		if f, err := os.OpenFile(w.proc(), os.O_WRONLY, 0); err == nil {
			f.Close()
		}
		close(w.opened)
	}()
	t.Cleanup(w.letGo)
	return w
}

// proc is the name that opens the FIFO through the descriptor.
func (w *fifo) proc() string { return "/proc/self/fd/" + strconv.Itoa(w.ref) }

// stillWaits reports whether the writer still waits, after a tenth of a
// second, so whether nothing has opened the FIFO for reading. A writer that
// hasn't reached its open yet counts as waiting, so a busy machine can't make
// this false.
func (w *fifo) stillWaits() bool {
	select {
	case <-w.opened:
		return false
	case <-time.After(100 * time.Millisecond):
		return true
	}
}

// letGo lets the writer go by opening the FIFO for reading, which a FIFO lets
// through at once when the open is asked not to wait, and holds it open until
// the writer's open has returned.
func (w *fifo) letGo() {
	w.once.Do(func() {
		if fd, err := syscall.Open(w.proc(), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err == nil {
			<-w.opened
			syscall.Close(fd)
		}
		syscall.Close(w.ref)
	})
}

// shortSocket makes a Unix socket, listening until the test ends, in a
// folder of its own with a short path, since a socket's path holds 107 bytes
// at most, and returns its path.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return path
}

// opens returns the calls among calls that open path.
func opens(calls []string, path string) []string {
	var out []string
	for _, c := range calls {
		if c == "open "+path {
			out = append(out, c)
		}
	}
	return out
}

// TestOnlyARegularFileIsOpened: a folder, a FIFO, a socket or a device at the
// path is refused, with an error that wraps errs.ErrNotDatabase, whose kind
// is "error", as 0.x's is for a folder, so it mustn't wrap errs.ErrInvalid.
// Nothing opens it. A FIFO with a writer waiting at its other end is never
// opened, so the writer goes on waiting, and Open returns within its
// deadline. The folder, the FIFO, the socket and the device are as they were,
// and nothing is made beside them.
func TestOnlyARegularFileIsOpened(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "folder")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(dir, "fifo")
	w := newFIFO(t, pipe)
	socket := shortSocket(t)
	device := symlink(t, "/dev/null", filepath.Join(dir, "device"))

	for _, c := range []struct {
		path, kind, opened string
	}{
		{folder, "a folder", folder},
		{pipe, "a FIFO", pipe},
		{socket, "a socket", socket},
		{device, "a character device", "/dev/null"},
	} {
		p := &pathsFS{FS: fsys.OS{}}
		err := within(t, 30*time.Second, "Open of "+c.kind, func() error {
			l, err := Open(p, c.path, &recorder{}, Options{})
			if err == nil {
				l.Close()
			}
			return err
		})
		if !errors.Is(err, errs.ErrNotDatabase) || errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), c.kind) {
			t.Errorf("Open of %s gave %v, where a refusal that names it and wraps ErrNotDatabase is wanted", c.kind, err)
		}
		if o := opens(p.taken(), c.opened); o != nil {
			t.Errorf("Open of %s opened it: %q", c.kind, o)
		}
	}
	if !w.stillWaits() {
		t.Error("the writer at the FIFO's other end stopped waiting: the FIFO was opened")
	}
	if got := names(t, dir); got != "device fifo folder" {
		t.Errorf("the folder holds %q", got)
	}
	if got := names(t, folder); got != "" {
		t.Errorf("the folder at the path holds %q", got)
	}
	for path, mode := range map[string]fs.FileMode{folder: fs.ModeDir, pipe: fs.ModeNamedPipe, socket: fs.ModeSocket, "/dev/null": fs.ModeCharDevice} {
		if info, err := os.Lstat(path); err != nil || info.Mode()&mode == 0 {
			t.Errorf("%s is no longer what it was: %v, %v", path, info.Mode(), err)
		}
	}
}

// TestAFIFOThatTakesThePathIsRefused: a FIFO moved over a database that's
// open, with a writer waiting at its other end, is refused at the reload, and
// at Lock, which would otherwise make a database of what looks like an empty
// file, renamed over the FIFO. Neither opens it, the Target isn't reset, and
// the FIFO stays at the path. Once a database is back there, Lock moves to it.
func TestAFIFOThatTakesThePathIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	p := &pathsFS{FS: fsys.OS{}}
	l, rec := openWith(t, p, path, Options{})
	commit(t, l, table("one"))
	w := newFIFO(t, filepath.Join(dir, "fifo"))
	if err := os.Rename(filepath.Join(dir, "fifo"), path); err != nil {
		t.Fatal(err)
	}
	p.taken()
	if err := l.Follow(); !errors.Is(err, ErrReplaced) {
		t.Fatalf("Follow with a FIFO at the path: %v", err)
	}
	err := within(t, 30*time.Second, "Reload with a FIFO at the path", l.Reload)
	if !errors.Is(err, errs.ErrNotDatabase) || !strings.Contains(err.Error(), "a FIFO") {
		t.Errorf("Reload with a FIFO at the path: %v", err)
	}
	err = within(t, 30*time.Second, "Lock with a FIFO at the path", func() error { return lockErr(l) })
	if !errors.Is(err, errs.ErrNotDatabase) {
		t.Errorf("Lock with a FIFO at the path: %v", err)
	}
	if o := opens(p.taken(), path); o != nil {
		t.Errorf("the FIFO at the path was opened: %q", o)
	}
	if !w.stillWaits() {
		t.Error("the writer at the FIFO's other end stopped waiting: the FIFO was opened")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("the path no longer holds the FIFO: %v, %v", info.Mode(), err)
	}
	if got := names(t, dir); got != "db" {
		t.Errorf("the folder holds %q", got)
	}
	if rec.resets != 0 {
		t.Errorf("the Target was reset %d times", rec.resets)
	}
	rec.holds(t) // the Log's own commit, which it hands to no Target

	back := filepath.Join(dir, "back")
	another(t, back, "b1")
	if err := os.Rename(back, path); err != nil {
		t.Fatal(err)
	}
	commit(t, l, table("two"))
	rec.holds(t, table("b1"))
	reread(t, path).holds(t, table("b1"), table("two"))
}

// TestASymbolicLinkToNothingIsRefused: a path that's a symbolic link leading
// to no file can't take a new database, since the rename into place would
// replace the link. Open refuses it with an error that wraps errs.ErrInvalid,
// where 0.x makes its database where the link leads, and leaves the link as
// it was, with nothing made where it leads or beside it.
func TestASymbolicLinkToNothingIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := symlink(t, "nowhere", filepath.Join(dir, "db"))
	_, err := Open(fsys.OS{}, path, &recorder{}, Options{})
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("Open of a symbolic link to nothing: %v", err)
	}
	linksAsTheyWere(t, map[string]string{path: "nowhere"})
	if got := names(t, dir); got != "db" {
		t.Errorf("the folder holds %q", got)
	}
}

// TestALinkThatTakesThePath: a symbolic link moved over the database while
// it's open. One that leads to another database is refused at the reload and
// at Lock, with an error that wraps errs.ErrInvalid, since a compaction would
// replace the link: the database is opened again by its new real path. One
// that leads to the database's own file, by another name, is the same file
// to every stat, so commits go on into it, and the compaction fails before
// its rename, which would replace the link and leave the file with the old
// data at its other name. A folder on the way that becomes a link to where it
// was moved does no harm: commits and a compaction go on in the folder it
// leads to, and the link stays.
func TestALinkThatTakesThePath(t *testing.T) {
	t.Run("to another database", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		l, rec := openLog(t, path, Options{})
		commit(t, l, table("one"))
		other := filepath.Join(dir, "other")
		another(t, other, "o1")
		before := fileBytes(t, other)
		if err := os.Rename(symlink(t, "other", filepath.Join(dir, "tmp")), path); err != nil {
			t.Fatal(err)
		}
		if err := l.Follow(); !errors.Is(err, ErrReplaced) {
			t.Fatalf("Follow with a link at the path: %v", err)
		}
		if err := l.Reload(); !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("Reload with a link at the path: %v", err)
		}
		if err := lockErr(l); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("Lock with a link at the path: %v", err)
		}
		if rec.resets != 0 {
			t.Errorf("the Target was reset %d times", rec.resets)
		}
		linksAsTheyWere(t, map[string]string{path: "other"})
		if !bytes.Equal(fileBytes(t, other), before) {
			t.Error("the database the link leads to changed")
		}
	})

	t.Run("to its own file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db")
		d := openDBT(t, fsys.OS{}, path, Options{})
		fill(t, d, 1, 5)
		other := filepath.Join(dir, "other")
		if err := os.Link(path, other); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(symlink(t, "other", filepath.Join(dir, "tmp")), path); err != nil {
			t.Fatal(err)
		}
		live := inode(t, other)
		fill(t, d, 6, 8)
		if err := d.compact(); !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("a compaction with a link to the file at the path: %v", err)
		}
		linksAsTheyWere(t, map[string]string{path: "other"})
		if !inode(t, other).Same(live) {
			t.Error("the file's other name no longer leads to it")
		}
		if got := names(t, dir); got != "db other" {
			t.Errorf("the folder holds %q", got)
		}
		if got := stateOfFile(t, other); got != d.state() {
			t.Errorf("the file holds\n%s\nwhere the copy holds\n%s", got, d.state())
		}
	})

	t.Run("a folder on the way", func(t *testing.T) {
		base := realTempDir(t)
		path := filepath.Join(base, "real", "db")
		if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		d := openDBT(t, fsys.OS{}, path, Options{})
		fill(t, d, 1, 5)
		moved := filepath.Join(base, "moved")
		if err := os.Rename(filepath.Join(base, "real"), moved); err != nil {
			t.Fatal(err)
		}
		symlink(t, "moved", filepath.Join(base, "real"))
		fill(t, d, 6, 8)
		if err := d.compact(); err != nil {
			t.Fatal(err)
		}
		fill(t, d, 9, 10)
		if h := headerOf(t, filepath.Join(moved, "db")); h.Gen != 2 {
			t.Errorf("the file in the folder the link leads to is of generation %d", h.Gen)
		}
		if got := stateOfFile(t, filepath.Join(moved, "db")); got != d.state() {
			t.Errorf("the file holds\n%s\nwhere the copy holds\n%s", got, d.state())
		}
		linksAsTheyWere(t, map[string]string{filepath.Join(base, "real"): "moved"})
		if got := names(t, moved); got != "db" {
			t.Errorf("the moved folder holds %q", got)
		}
	})
}

// TestTheRulesOnTheFaultLayer: the rules where the fault layer's names show
// them. A folder at the path is refused, with ErrNotDatabase, and never
// opened. A relative path is taken from "/", as the Disk takes it, and the
// log asks the Disk for the real path, and makes NAME.compact beside it. A
// failed RealPath fails Open before anything is made.
func TestTheRulesOnTheFaultLayer(t *testing.T) {
	d := fault.New(1)
	if err := d.Mkdir("/db/folder"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(d.FS(), "/db/folder", &recorder{}, Options{}); !errors.Is(err, errs.ErrNotDatabase) || !strings.Contains(err.Error(), "a folder") {
		t.Errorf("Open of a folder on the Disk: %v", err)
	}
	if n := d.Calls(fault.Open, "/db/folder"); n != 0 {
		t.Errorf("the folder was opened %d times", n)
	}

	l, err := Open(d.FS(), "db/rel.hcx", &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Path() != "/db/rel.hcx" || d.Calls(fault.RealPath, "/db/rel.hcx") == 0 {
		t.Errorf("the Log works from %q, after %d RealPath calls on it", l.Path(), d.Calls(fault.RealPath, "/db/rel.hcx"))
	}
	commit(t, l, table("one"))
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	err = l.Compact(func(yield func(format.Change) bool) { yield(table("one")[0]) })
	l.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if n := d.Calls(fault.Create, "/db/rel.hcx.compact"); n != 1 {
		t.Errorf("NAME.compact was made %d times beside the real path", n)
	}

	d = fault.New(2)
	if err := d.Mkdir("/db"); err != nil {
		t.Fatal(err)
	}
	d.Add(fault.Rule{Op: fault.RealPath, N: 1})
	if _, err := Open(d.FS(), "/db/new.hcx", &recorder{}, Options{}); !errors.Is(err, syscall.EIO) {
		t.Errorf("Open whose RealPath fails: %v", err)
	}
	if n := d.Calls(fault.Create, ""); n != 0 {
		t.Errorf("Open whose RealPath failed made %d files", n)
	}
}
