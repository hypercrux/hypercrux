// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Compaction's cases one by one (F8), with the real store as the Target,
// joined to the log as the public package joins them (G1), and compacting
// as G5 will: after a commit, holding the lock, from a read of the copy.

// copyTarget is the public package's target in small, as glue is in
// order_test.go: while Open reads the file, each batch goes straight into
// the copy, and after that through a transaction of its own. Reset puts an
// empty copy in place, as the public package's does.
type copyTarget struct {
	s       *store.Store
	opening bool
	resets  int
}

func (c *copyTarget) Apply(seq uint64, changes []format.Change) error {
	if c.opening {
		return c.s.LoadBatch(seq, changes)
	}
	return c.s.ApplyBatch(seq, changes)
}

func (c *copyTarget) Reset() {
	c.s = store.New()
	c.resets++
}

// db is a database opened in small, as the public package opens one: its
// log and its copy.
type db struct {
	l *Log
	c *copyTarget
}

// openDB opens the database at path through files.
func openDB(files fsys.FS, path string, o Options) (*db, error) {
	c := &copyTarget{s: store.New(), opening: true}
	l, err := Open(files, path, c, o)
	if err != nil {
		return nil, err
	}
	c.opening = false
	return &db{l: l, c: c}, nil
}

// openDBT opens the database at path through files, and closes it when the
// test ends.
func openDBT(t *testing.T, files fsys.FS, path string, o Options) *db {
	t.Helper()
	d, err := openDB(files, path, o)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, d.l)
	return d
}

// commit is the public package's Update: Lock, the copy's transaction
// running fn, Commit with the log's Append, then Unlock.
func (d *db) commit(fn func(tx *store.Tx) error) (err error) {
	if err := d.l.Lock(); err != nil {
		return err
	}
	defer func() {
		if e := d.l.Unlock(); err == nil {
			err = e
		}
	}()
	tx, err := d.c.s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(d.l.Append)
}

// compactLocked compacts holding the write lock, from a read of the copy,
// as the public package will once a commit's Due says so (G5).
func (d *db) compactLocked() error {
	return d.c.s.Read(func(r store.Reader) error { return d.l.Compact(r.Snapshot()) })
}

// compact is the public package's Compact: Lock, the compaction, Unlock.
func (d *db) compact() error {
	if err := d.l.Lock(); err != nil {
		return err
	}
	err := d.compactLocked()
	if e := d.l.Unlock(); err == nil {
		err = e
	}
	return err
}

// state is the copy's state as text (dump).
func (d *db) state() string {
	var s string
	d.c.s.Read(func(r store.Reader) error { s = dump(r); return nil })
	return s
}

// dump is r's state as text: its snapshot, a change a line. Two copies that
// hold the same live data give the same text, however their files hold it.
func dump(r store.Reader) string {
	var b strings.Builder
	for c := range r.Snapshot() {
		b.WriteString(c.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// compactOps is commit i's work in the compaction tests, through a
// transaction of the copy. It leaves dead data in the file for a compaction
// to drop: puts of five records in turn, so each fifth commit writes over
// one, with text of 1 to 700 letters, as many as i picks, so that batches
// and markers fall across sectors in many ways; vectors in a table of their
// own; links between the two tables; deletes, which take links with them;
// and a table made and then dropped.
func compactOps(tx *store.Tx, i int) error {
	text := make([]byte, i*389%700+1)
	for j := range text {
		text[j] = 'a' + byte((i+j)%26)
	}
	key := fmt.Sprintf("t:%d", i%5)
	if err := tx.Put(key, []format.Field{{Name: "n", Value: value.Int(int64(i))}, {Name: "s", Value: value.Text(string(text))}}); err != nil {
		return err
	}
	if i%2 == 0 {
		vec := []float32{float32(i), 1, -0.5, float32(i % 3)}
		if err := tx.Put(fmt.Sprintf("v:%d", i%3), []format.Field{{Name: "vec", Value: value.Vector(vec)}}); err != nil {
			return err
		}
	}
	if to := fmt.Sprintf("v:%d", (i+1)%3); i%3 == 0 {
		if _, err := tx.Get(to); err == nil {
			if err := tx.Link(key, "near", to); err != nil {
				return err
			}
		}
	}
	if gone := fmt.Sprintf("t:%d", (i+2)%5); i%4 == 0 {
		if _, err := tx.Get(gone); err == nil {
			if err := tx.Delete(gone); err != nil {
				return err
			}
		}
	}
	switch _, there := tx.Table("tmp"); {
	case i%6 == 5:
		return tx.Put(fmt.Sprintf("tmp:%d", i), []format.Field{{Name: "n", Value: value.Int(int64(i))}})
	case i%6 == 0 && there:
		return tx.Drop("tmp")
	}
	return nil
}

// fill makes commits first to last on d, each with compactOps.
func fill(t *testing.T, d *db, first, last int) {
	t.Helper()
	for i := first; i <= last; i++ {
		if err := d.commit(func(tx *store.Tx) error { return compactOps(tx, i) }); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
}

// stateOfFile opens the database at path afresh and returns its state.
func stateOfFile(t *testing.T, path string) string {
	t.Helper()
	d, err := openDB(fsys.OS{}, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.l.Close()
	return d.state()
}

// headerOf reads the header of the database file at path.
func headerOf(t *testing.T, path string) format.Header {
	t.Helper()
	b := fileBytes(t, path)
	h, err := format.DecodeHeader(b, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// batchesOf reads the batches of the database file at path with the codec,
// up to the end of its compacted part.
func batchesOf(t *testing.T, path string) []format.Batch {
	t.Helper()
	b := fileBytes(t, path)
	h := headerOf(t, path)
	var out []format.Batch
	for off := int64(format.HeaderSize); off < int64(h.CompactedEnd); {
		bt, err := format.DecodeBatch(b[off:], h.Gen, uint64(len(out)+1))
		if err != nil {
			t.Fatalf("batch %d at offset %d: %v", len(out)+1, off, err)
		}
		if m, whole := format.DecodeMarker(b[off+int64(bt.Length):], h.ID, h.Gen); !whole || m != (format.Marker{Seq: bt.Seq, Sum: bt.Sum}) {
			t.Fatalf("batch %d at offset %d has no marker of its own", bt.Seq, off)
		}
		out = append(out, bt)
		off += int64(bt.Length) + format.MarkerSize
	}
	return out
}

// readFixture reads one of P2's fixtures in beta/internal/format/testdata:
// annotated hex, in which everything from a # to the end of a line is a
// comment.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("..", "format", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var b []byte
	for _, line := range strings.Split(string(text), "\n") {
		line, _, _ = strings.Cut(line, "#")
		for _, pair := range strings.Fields(line) {
			x, err := hex.DecodeString(pair)
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, x...)
		}
	}
	return b
}

// TestCompactingTheFixture: compacting file-new.hex, P2's small database,
// writes file-compacted.hex's header and compacted part, byte for byte: the
// next generation, the batch the file continues from, where the compacted
// part ends, every table with its records in one batch and the links in
// another. The commit after it, made under the same lock, is the fixture's
// too, so then the whole file matches.
func TestCompactingTheFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	write(t, path, readFixture(t, "file-new.hex"))
	d := openDBT(t, fsys.OS{}, path, Options{})
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := d.compactLocked(); err != nil {
		t.Fatal(err)
	}
	want := readFixture(t, "file-compacted.hex")
	h, err := format.DecodeHeader(want, int64(len(want)))
	if err != nil {
		t.Fatal(err)
	}
	if got := fileBytes(t, path); !bytes.Equal(got, want[:h.CompactedEnd]) {
		t.Fatalf("compacting file-new.hex wrote\n%x\nwhere file-compacted.hex's header and compacted part are\n%x", got, want[:h.CompactedEnd])
	}
	tx, err := d.c.s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Put("users:bob", []format.Field{{Name: "name", Value: value.Text("Bob")}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(d.l.Append); err != nil {
		t.Fatal(err)
	}
	if err := d.l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if got := fileBytes(t, path); !bytes.Equal(got, want) {
		t.Fatalf("with the commit after the compaction, the file is\n%x\nwhere file-compacted.hex is\n%x", got, want)
	}
}

// TestACompactionSwitchesFiles: a compaction writes the live data into a new
// file and renames it over the database. The new file has the next
// generation, names the batch it continues from, and ends where its
// compacted part does, which is shorter than the old file by the dead data.
// The compacted part's batches hold about as much as they're asked to, and
// the links start a batch of their own. The old file stays as it was, and
// nothing is left beside the database. The Log holds the new file, the copy
// is as it was without a reset, and the commits after the compaction go into
// the new file, numbered on from the compacted part. A second compaction
// goes from generation 2 to 3.
func TestACompactionSwitchesFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	d := openDBT(t, fsys.OS{}, path, Options{})
	fill(t, d, 1, 40)
	before := d.state()
	old := inode(t, path)
	oldBytes := fileBytes(t, path)
	kept := holdOpen(t, path) // the old file to look at, kept open, since a second name would be refused (F7)
	from, _ := format.DecodeMarker(d.l.last[:], d.l.hdr.ID, d.l.hdr.Gen)
	const part = 300
	d.l.part = part
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}

	if inode(t, path).Same(old) {
		t.Fatal("the database's path names the old file")
	}
	h := headerOf(t, path)
	size := int64(len(fileBytes(t, path)))
	switch {
	case h.ID != d.l.hdr.ID || h.Gen != 2 || h.FromGen != 1 || h.FromSeq != 40 || h.FromSeq != from.Seq || h.FromSum != from.Sum:
		t.Errorf("the compacted file's header is %+v, where generation 2 continuing from batch 40, %#08x, of generation 1 is wanted", h, from.Sum)
	case int64(h.CompactedEnd) != size || d.l.end != size:
		t.Errorf("the compacted part ends at %d, the Log's log at %d, and the file at %d", h.CompactedEnd, d.l.end, size)
	case size >= int64(len(oldBytes)):
		t.Errorf("the compacted file is %d bytes, and the old one %d", size, len(oldBytes))
	}
	batches := batchesOf(t, path)
	if d.l.seq != uint64(len(batches)) || len(batches) < 4 {
		t.Errorf("the compacted part holds %d batches, and the Log has read %d", len(batches), d.l.seq)
	}
	linked := false
	for _, bt := range batches {
		n := 0
		for i := range bt.Changes {
			n += changeSize(&bt.Changes[i])
		}
		if n > part && len(bt.Changes) > 1 {
			t.Errorf("batch %d holds %d bytes of changes in %d changes, past the %d asked for", bt.Seq, n, len(bt.Changes), part)
		}
		links := slices.ContainsFunc(bt.Changes, func(c format.Change) bool { return c.Op == format.Link })
		if links && slices.ContainsFunc(bt.Changes, func(c format.Change) bool { return c.Op != format.Link }) {
			t.Errorf("batch %d holds links and other changes: %v", bt.Seq, bt.Changes)
		}
		linked = linked || links
	}
	if !linked {
		t.Error("the compacted part holds no links")
	}
	if got := names(t, dir); got != "db" {
		t.Errorf("the folder holds %q", got)
	}
	if !bytes.Equal(heldBytes(t, kept), oldBytes) {
		t.Error("the old file changed")
	}
	if d.c.resets != 0 || d.state() != before {
		t.Errorf("the copy was reset %d times, or changed", d.c.resets)
	}
	if got := stateOfFile(t, path); got != before {
		t.Errorf("the compacted file holds\n%s\nwhere the old one held\n%s", got, before)
	}

	fill(t, d, 41, 44)
	if d.l.seq != uint64(len(batches)+4) {
		t.Errorf("the 4 commits after the compaction took the Log to batch %d, where %d is wanted", d.l.seq, len(batches)+4)
	}
	if got := stateOfFile(t, path); got != d.state() {
		t.Errorf("after the commits that followed it, the compacted file holds\n%s\nwhere the copy holds\n%s", got, d.state())
	}
	from, _ = format.DecodeMarker(d.l.last[:], d.l.hdr.ID, d.l.hdr.Gen)
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	if h := headerOf(t, path); h.Gen != 3 || h.FromGen != 2 || h.FromSeq != from.Seq || h.FromSum != from.Sum {
		t.Errorf("the second compaction's header is %+v", h)
	}
	if got := stateOfFile(t, path); got != d.state() {
		t.Error("the second compaction lost the live data")
	}
}

// TestCompactingAnEmptyDatabase: a database with no tables compacts to a
// header alone, whose compacted part ends at 68, and with no batch read
// before, it continues from batch 0 and the checksum 0. One whose tables are
// empty and have no links compacts to one batch.
func TestCompactingAnEmptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	d := openDBT(t, fsys.OS{}, path, Options{})
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	if h := headerOf(t, path); h.Gen != 2 || h.FromGen != 1 || h.FromSeq != 0 || h.FromSum != 0 || h.CompactedEnd != format.HeaderSize || len(fileBytes(t, path)) != format.HeaderSize {
		t.Errorf("an empty database compacts to the header %+v, in %d bytes", h, len(fileBytes(t, path)))
	}
	if err := d.commit(func(tx *store.Tx) error {
		if err := tx.Put("a:1", nil); err != nil {
			return err
		}
		return tx.Delete("a:1")
	}); err != nil {
		t.Fatal(err)
	}
	if d.l.seq != 1 {
		t.Errorf("the first commit after compacting an empty database is batch %d", d.l.seq)
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	if b := batchesOf(t, path); len(b) != 1 || len(b[0].Changes) != 1 || b[0].Changes[0].Op != format.CreateTable {
		t.Errorf("a database with an empty table compacts to %v", b)
	}
}

// TestChangeSize: the size the compaction splits its batches by is what the
// codec writes, for every kind of change and value.
func TestChangeSize(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 1))
	var changes []format.Change
	for range 300 {
		changes = append(changes, randomChanges(r)...)
	}
	changes = append(changes,
		format.Change{Op: format.CreateTable, Table: "docs", Size: 3, Names: []string{"title", "vec", "_x9"}},
		format.Change{Op: format.Put, Key: "docs:1"},
	)
	for _, c := range changes {
		b, _, err := format.AppendBatch(nil, 1, 1, []format.Change{c})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := changeSize(&c), len(b)-format.BatchHeadSize-4; got != want {
			t.Errorf("changeSize gives %d for %v, which the codec writes in %d bytes", got, c, want)
		}
	}
}

// TestACompactionKeepsPermissionsAndOwner: the compacted file has the
// database's permissions, and, where the process may give it one, the
// database's owner and group.
func TestACompactionKeepsPermissionsAndOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	d := openDBT(t, fsys.OS{}, path, Options{})
	fill(t, d, 1, 3)
	if err := os.Chmod(path, 0o604); err != nil {
		t.Fatal(err)
	}
	root := os.Getuid() == 0
	if root {
		if err := os.Chown(path, 12345, 23456); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	info := inode(t, path)
	if info.Mode != 0o604 {
		t.Errorf("the compacted file has mode %v", info.Mode)
	}
	if !root {
		t.Log("not run as root, so the owner can't be checked")
		return
	}
	if info.Uid != 12345 || info.Gid != 23456 {
		t.Errorf("the compacted file is owned by %d:%d", info.Uid, info.Gid)
	}
}

// trapFS is the real calls, with a test's say over each one. Before each
// call on a name or a file, other than reads, it asks trap with the call
// and the file's name, such as "create db.compact", "sync db" or "write
// db.compact at 68". When trap returns an error, the call fails with it and
// changes nothing. A trap may do something of its own first, such as move a
// backup into place.
type trapFS struct {
	fsys.FS
	mu   sync.Mutex
	trap func(call string) error
}

func (f *trapFS) ask(call string, args ...any) error {
	f.mu.Lock()
	trap := f.trap
	f.mu.Unlock()
	if trap == nil {
		return nil
	}
	return trap(fmt.Sprintf(call, args...))
}

func (f *trapFS) set(trap func(call string) error) {
	f.mu.Lock()
	f.trap = trap
	f.mu.Unlock()
}

func (f *trapFS) wrap(path string, file fsys.File, err error) (fsys.File, error) {
	if err != nil {
		return nil, err
	}
	return &trapFile{File: file, fs: f, name: filepath.Base(path)}, nil
}

func (f *trapFS) Open(path string) (fsys.File, error) {
	if err := f.ask("open %s", filepath.Base(path)); err != nil {
		return nil, err
	}
	file, err := f.FS.Open(path)
	return f.wrap(path, file, err)
}

func (f *trapFS) Create(path string, perm fs.FileMode) (fsys.File, error) {
	if err := f.ask("create %s", filepath.Base(path)); err != nil {
		return nil, err
	}
	file, err := f.FS.Create(path, perm)
	return f.wrap(path, file, err)
}

func (f *trapFS) Rename(from, to string) error {
	if err := f.ask("rename %s", filepath.Base(from)); err != nil {
		return err
	}
	return f.FS.Rename(from, to)
}

func (f *trapFS) Remove(path string) error {
	if err := f.ask("remove %s", filepath.Base(path)); err != nil {
		return err
	}
	return f.FS.Remove(path)
}

func (f *trapFS) SyncDir(dir string) error {
	if err := f.ask("syncdir"); err != nil {
		return err
	}
	return f.FS.SyncDir(dir)
}

func (f *trapFS) Stat(path string) (fsys.Info, error) {
	if err := f.ask("stat %s", filepath.Base(path)); err != nil {
		return fsys.Info{}, err
	}
	return f.FS.Stat(path)
}

type trapFile struct {
	fsys.File
	fs   *trapFS
	name string
}

func (f *trapFile) WriteAt(p []byte, off int64) (int, error) {
	if err := f.fs.ask("write %s at %d", f.name, off); err != nil {
		return 0, err
	}
	return f.File.WriteAt(p, off)
}

func (f *trapFile) Sync() error {
	if err := f.fs.ask("sync %s", f.name); err != nil {
		return err
	}
	return f.File.Sync()
}

func (f *trapFile) Stat() (fsys.Info, error) {
	if err := f.fs.ask("fstat %s", f.name); err != nil {
		return fsys.Info{}, err
	}
	return f.File.Stat()
}

func (f *trapFile) TryLock() (bool, error) {
	if err := f.fs.ask("trylock %s", f.name); err != nil {
		return false, err
	}
	return f.File.TryLock()
}

// failNth returns a trap that fails, with errDrive, the nth call from when
// it's set that is prefix, or starts with prefix and a space, and each call
// that is one of also, or starts with one, with errAgain. So "stat db" is a
// stat of the database alone, and "write db.compact" any write to
// NAME.compact.
func failNth(prefix string, n int, also ...string) func(string) error {
	seen := 0
	is := func(call, p string) bool { return call == p || strings.HasPrefix(call, p+" ") }
	return func(call string) error {
		for _, a := range also {
			if is(call, a) {
				return errAgain
			}
		}
		if is(call, prefix) {
			if seen++; seen == n {
				return errDrive
			}
		}
		return nil
	}
}

// TestAFailedCompactionCarriesOn: when any call fails before the switch,
// from making NAME.compact to the rename, the compaction removes
// NAME.compact, and the Log carries on with the old file, which is whole:
// the path still names it, and the commits after it go into it. When the
// removal fails too, NAME.compact stays until the next holder of the write
// lock, which removes it. The next compaction isn't due until the file has
// grown by as much again.
func TestAFailedCompactionCarriesOn(t *testing.T) {
	for _, c := range []struct {
		name   string
		prefix string
		n      int
		also   []string
	}{
		{"making NAME.compact", "create db.compact", 1, nil},
		{"locking it", "trylock db.compact", 1, nil},
		{"its first batch", "write db.compact at 68", 1, nil},
		{"a later batch", "write db.compact", 3, nil},
		{"its header", "write db.compact at 0", 1, nil},
		{"its sync", "sync db.compact", 1, nil},
		{"the look at the path before the rename", "stat db", 1, nil},
		{"the rename", "rename db.compact", 1, nil},
		{"its sync, and its removal", "sync db.compact", 1, []string{"remove db.compact"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "db")
			trap := &trapFS{FS: fsys.OS{}}
			d := openDBT(t, trap, path, Options{})
			fill(t, d, 1, 30)
			d.l.part = 300
			before, old := d.state(), inode(t, path)
			if err := d.l.Lock(); err != nil {
				t.Fatal(err)
			}
			trap.set(failNth(c.prefix, c.n, c.also...))
			err := d.compactLocked()
			trap.set(nil)
			left := c.also != nil
			switch {
			case !errors.Is(err, errDrive) || errors.Is(err, errs.ErrStuck) || !strings.Contains(err.Error(), "carries on in its old file"):
				t.Errorf("Compact: %v", err)
			case left != errors.Is(err, errAgain):
				t.Errorf("Compact, when removing NAME.compact fails %v: %v", left, err)
			}
			want := "db"
			if left {
				want = "db db.compact"
			}
			if got := names(t, dir); got != want {
				t.Errorf("the folder holds %q, where %q is wanted", got, want)
			}
			if !inode(t, path).Same(old) || !d.l.file.Same(old) {
				t.Error("the Log or the path left the old file")
			}
			if err := d.l.Unlock(); err != nil {
				t.Fatal(err)
			}
			if got := stateOfFile(t, path); got != before {
				t.Error("the old file doesn't hold what it did")
			}
			fill(t, d, 31, 32)
			if got := names(t, dir); got != "db" {
				t.Errorf("after the next commit the folder holds %q", got)
			}
			if !inode(t, path).Same(old) {
				t.Error("the next commit left the old file")
			}
			if got := stateOfFile(t, path); got != d.state() {
				t.Error("the commits after the failed compaction aren't in the old file")
			}
			if err := d.compact(); err != nil {
				t.Fatal(err)
			}
			if got := stateOfFile(t, path); got != d.state() || inode(t, path).Same(old) {
				t.Error("the compaction after the failed one didn't switch, or lost the live data")
			}
		})
	}
}

// TestASnapshotThatPanics: when ranging over the snapshot panics, the panic
// carries on out of Compact, and NAME.compact goes first, so a caller that
// recovers holds no lock on it. The Log carries on with the old file, and
// the next compaction works.
func TestASnapshotThatPanics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	d := openDBT(t, fsys.OS{}, path, Options{})
	fill(t, d, 1, 10)
	d.l.part = 100
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if r := recover(); r != "the snapshot's own" {
				t.Errorf("Compact's panic was %v", r)
			}
		}()
		d.c.s.Read(func(r store.Reader) error {
			n := 0
			return d.l.Compact(func(yield func(format.Change) bool) {
				for c := range r.Snapshot() {
					if n++; n == 5 {
						panic("the snapshot's own")
					}
					if !yield(c) {
						return
					}
				}
			})
		})
	}()
	if got := names(t, dir); got != "db" {
		t.Errorf("the folder holds %q", got)
	}
	if err := d.compactLocked(); err != nil {
		t.Fatal(err)
	}
	if err := d.l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if got := stateOfFile(t, path); got != d.state() || headerOf(t, path).Gen != 2 {
		t.Error("the compaction after the panic didn't switch to a file with the live data")
	}
}

// TestARenameThatTookEffectFailing: a rename that fails changes nothing,
// but should one take effect and fail all the same, the path names the new
// file, so the compaction goes on as after the rename, and syncs the
// folder.
func TestARenameThatTookEffectFailing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	trap := &trapFS{FS: fsys.OS{}}
	d := openDBT(t, trap, path, Options{})
	fill(t, d, 1, 10)
	old := inode(t, path)
	var calls []string
	trap.set(func(call string) error {
		calls = append(calls, call)
		if call == "rename db.compact" {
			if err := os.Rename(path+".compact", path); err != nil {
				t.Error(err)
			}
			return errDrive
		}
		return nil
	})
	if err := d.compact(); err != nil {
		t.Fatalf("Compact, when the rename took effect and failed: %v", err)
	}
	trap.set(nil)
	if inode(t, path).Same(old) || !d.l.file.Same(inode(t, path)) {
		t.Error("the Log isn't on the compacted file")
	}
	if i := slices.Index(calls, "rename db.compact"); i < 0 || !slices.Contains(calls[i:], "syncdir") {
		t.Errorf("the folder wasn't synced after the rename: %q", calls)
	}
	fill(t, d, 11, 12)
	if got := stateOfFile(t, path); got != d.state() {
		t.Error("the commits after the compaction aren't in the compacted file")
	}
}

// TestABackupMovedInDuringACompaction: just before its rename, a compaction
// checks that the file at the path is still the one it compacted, so a
// backup moved into place meanwhile is never replaced. The compaction fails
// and removes NAME.compact, and the next Lock moves to the backup.
func TestABackupMovedInDuringACompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	trap := &trapFS{FS: fsys.OS{}}
	d := openDBT(t, trap, path, Options{})
	fill(t, d, 1, 10)
	backup := filepath.Join(dir, "backup")
	another(t, backup, "b1", "b2")
	want := inode(t, backup)
	trap.set(func(call string) error {
		if call == "sync db.compact" {
			if err := os.Rename(backup, path); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	err := d.compact()
	trap.set(nil)
	if err == nil || !strings.Contains(err.Error(), "another file has taken the database's path") {
		t.Fatalf("Compact, with a backup moved in meanwhile: %v", err)
	}
	if !inode(t, path).Same(want) {
		t.Fatal("the backup was replaced")
	}
	if got := names(t, dir); got != "db" {
		t.Errorf("the folder holds %q", got)
	}
	if err := d.commit(func(tx *store.Tx) error { return tx.Put("c:1", nil) }); err != nil {
		t.Fatal(err)
	}
	if d.c.resets != 1 {
		t.Errorf("the copy was reset %d times", d.c.resets)
	}
	r := reread(t, path)
	if len(r.batches) != 3 {
		t.Errorf("the backup holds %d batches, where its own two and the next commit are wanted", len(r.batches))
	}
}

// TestAFailedFolderSyncAfterTheRename: once the compacted file has taken the
// path, a power cut could leave either file there until the folder's sync
// makes the rename last. So when that sync fails, the Log moves over to the
// new file and keeps its lock until Close. Compact's error wraps
// errs.ErrStuck, and so does every Lock after it, at once. Another Log, which
// was on the old file, moves to the new one and times out waiting for its
// lock. Once the stuck Log is closed, the other commits to the new file.
func TestAFailedFolderSyncAfterTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	trap := &trapFS{FS: fsys.OS{}}
	d := openDBT(t, trap, path, Options{})
	fill(t, d, 1, 10)
	const wait = 200 * time.Millisecond
	b, rb := openLog(t, path, Options{Wait: wait})
	old := inode(t, path)
	trap.set(failNth("syncdir", 1))
	err := d.compact()
	trap.set(nil)
	if !errors.Is(err, errs.ErrStuck) || !errors.Is(err, errDrive) {
		t.Fatalf("Compact, when the folder's sync fails after the rename: %v", err)
	}
	if inode(t, path).Same(old) || !d.l.file.Same(inode(t, path)) {
		t.Error("the Log isn't on the compacted file")
	}
	began := time.Now()
	if err := lockErr(d.l); !errors.Is(err, errs.ErrStuck) {
		t.Errorf("Lock on the stuck Log: %v", err)
	}
	if took := time.Since(began); took > wait/2 {
		t.Errorf("Lock on the stuck Log took %v", took)
	}
	for range 2 {
		began = time.Now()
		if err := lockErr(b); !errors.Is(err, errs.ErrLockTimeout) {
			t.Fatalf("another Log's Lock while the first is stuck: %v", err)
		}
		if took := time.Since(began); took < wait || took > 3*wait {
			t.Errorf("another Log's Lock gave up after %v, where its wait is %v", took, wait)
		}
	}
	state := d.state()
	if err := d.l.Close(); err != nil {
		t.Fatal(err)
	}
	commit(t, b, table("after"))
	if rb.resets != 1 || !b.file.Same(inode(t, path)) {
		t.Errorf("the other Log was reset %d times, and isn't on the compacted file", rb.resets)
	}
	if got := stateOfFile(t, path); !strings.HasPrefix(got, "create table after") || !strings.Contains(got, state) {
		t.Errorf("after the stuck Log was closed, the database holds\n%s", got)
	}
}

// TestALeftoverCompactFileIsRemoved: a NAME.compact that a compaction left
// when its process died goes once a writer holds the lock and has checked
// that the file it locked is the one at the path: at the next Lock, and at
// Open's check, which holds the lock. One that's locked stays, as does one
// that isn't a regular file. Nobody else removes one: not Open, nor Follow,
// while another holds the lock.
func TestALeftoverCompactFileIsRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	leftover := path + ".compact"
	leave := func() {
		t.Helper()
		write(t, leftover, bytes.Repeat([]byte("left"), 1000))
	}
	l, _ := openLog(t, path, Options{Wait: 200 * time.Millisecond})
	commit(t, l, table("one"))

	leave()
	commit(t, l, table("two"))
	if got := names(t, dir); got != "db" {
		t.Errorf("after a commit the folder holds %q", got)
	}
	leave()
	reread(t, path)
	if got := names(t, dir); got != "db" {
		t.Errorf("after an Open the folder holds %q", got)
	}

	// Another holds the database's lock: neither Open nor Follow removes it,
	// though Follow finds half a batch past the end of the log and tries the
	// lock.
	leave()
	release := holdLock(t, path)
	o, _ := openLog(t, path, Options{})
	w := newBuilder(t)
	w.b, w.hdr, w.seq = nil, o.hdr, o.seq
	w.batch(table("half"))
	appendTo(t, path, w.b[:len(w.b)/2])
	if err := o.Follow(); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); got != "db db.compact" {
		t.Errorf("with the lock held elsewhere, the folder holds %q", got)
	}
	release()

	// Locked, it stays; let go, it goes.
	busy, err := fsys.OS{}.Open(leftover)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := busy.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	commit(t, l, table("three"))
	reread(t, path)
	if got := names(t, dir); got != "db db.compact" {
		t.Errorf("with NAME.compact locked, the folder holds %q", got)
	}
	busy.Close()
	commit(t, l, table("four"))
	if got := names(t, dir); got != "db" {
		t.Errorf("once NAME.compact was let go, the folder holds %q", got)
	}

	// A folder at the name stays, and a compaction then fails before it
	// writes anything.
	if err := os.Mkdir(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	commit(t, l, table("five"))
	if got := names(t, dir); got != "db db.compact" {
		t.Errorf("with a folder at NAME.compact, the folder holds %q", got)
	}
	d := openDBT(t, fsys.OS{}, path, Options{})
	if err := d.compact(); !errors.Is(err, fs.ErrExist) {
		t.Errorf("Compact with a folder at NAME.compact: %v", err)
	}
	reread(t, path).holds(t, table("one"), table("two"), table("three"), table("four"), table("five"))
}

// TestALeftoverIsCheckedBeforeItGoes: the holder of the write lock removes a
// NAME.compact only when it's a regular file, and only while the name still
// leads to the file it locked. A pipe at the name stays, and so does a file
// that took the name while the lock was being taken.
func TestALeftoverIsCheckedBeforeItGoes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	leftover := path + ".compact"
	trap := &trapFS{FS: fsys.OS{}}
	d := openDBT(t, trap, path, Options{})
	fill(t, d, 1, 2)
	if err := syscall.Mkfifo(leftover, 0o644); err != nil {
		t.Fatal(err)
	}
	fill(t, d, 3, 3)
	if got := names(t, dir); got != "db db.compact" {
		t.Errorf("with a pipe at NAME.compact, the folder holds %q", got)
	}
	if err := os.Remove(leftover); err != nil {
		t.Fatal(err)
	}

	write(t, leftover, []byte("left"))
	write(t, filepath.Join(dir, "other"), []byte("other"))
	renamed := false
	trap.set(func(call string) error {
		if call == "trylock db.compact" && !renamed {
			renamed = true
			if err := os.Rename(filepath.Join(dir, "other"), leftover); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	fill(t, d, 4, 4)
	trap.set(nil)
	if b, err := os.ReadFile(leftover); err != nil || string(b) != "other" {
		t.Errorf("the file that took NAME.compact's name holds %q, %v", b, err)
	}
	fill(t, d, 5, 5)
	if got := names(t, dir); got != "db" {
		t.Errorf("the next commit left %q", got)
	}
}

// TestALeftoverWaitsForTheInodeCheck: a writer that gets the lock of a file
// that another has replaced at the path holds no write lock at all, since a
// compaction of the new file may be under way, in the moment between making
// NAME.compact and locking it. So it removes nothing until it holds the
// lock of the file at the path, and checked so. Here the new file's lock is
// held throughout, so the writer times out and NAME.compact stays.
func TestALeftoverWaitsForTheInodeCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	l, _ := openLog(t, path, Options{Wait: 200 * time.Millisecond})
	commit(t, l, table("one"))
	another(t, filepath.Join(dir, "next"), "n1")
	if err := os.Rename(filepath.Join(dir, "next"), path); err != nil {
		t.Fatal(err)
	}
	release := holdLock(t, path)
	defer release()
	write(t, path+".compact", nil) // a compaction's file, made a moment ago and not yet locked
	errc := make(chan error, 1)
	go func() { errc <- lockErr(l) }()
	select {
	case err := <-errc:
		if !errors.Is(err, errs.ErrLockTimeout) {
			t.Fatalf("Lock while the new file's lock is held: %v", err)
		}
	case <-time.After(5 * time.Second):
		// A NAME.compact that isn't locked is no compaction under way, so it
		// mustn't make the wait go on.
		release()
		t.Fatal("Lock was still waiting after 5 seconds, where its wait is 200 ms")
	}
	if got := names(t, dir); got != "db db.compact" {
		t.Errorf("the folder holds %q", got)
	}
}

// TestDue: by the rule the log measures, a compaction is due once the end of
// the log is twice as far into the file as the end of its compacted part,
// or the header in generation 1, and a megabyte past it. With live data of
// 300 KB, the megabyte decides; with 1.8 MB, twice does. After a compaction
// that failed before the switch, the next isn't due until the file has grown
// by as much again as it had since its last compaction. Due is false for a
// Log that doesn't hold the lock.
func TestDue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	trap := &trapFS{FS: fsys.OS{}}
	d := openDBT(t, trap, path, Options{})
	big := strings.Repeat("x", 300<<10)
	put := func(key string) {
		t.Helper()
		if err := d.commit(func(tx *store.Tx) error {
			return tx.Put(key, []format.Field{{Name: "s", Value: value.Text(big)}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	due := func() bool {
		t.Helper()
		if err := d.l.Lock(); err != nil {
			t.Fatal(err)
		}
		defer d.l.Unlock()
		return d.l.Due()
	}
	// upTo puts a:1 again and again while the next would leave the end of
	// the log short of at, and checks that no compaction is due meanwhile.
	// Then it puts it once more, past at, and checks that one is due.
	upTo := func(at int64) {
		t.Helper()
		before := d.l.end
		put("a:1")
		step := d.l.end - before // what each of these commits adds
		for d.l.end+step < at {
			if due() {
				t.Fatalf("due at %d bytes, before %d, with the compacted part ending at %d", d.l.end, at, d.l.hdr.CompactedEnd)
			}
			put("a:1")
		}
		if due() {
			t.Fatalf("due at %d bytes, before %d, with the compacted part ending at %d", d.l.end, at, d.l.hdr.CompactedEnd)
		}
		put("a:1")
		if !due() {
			t.Fatalf("not due at %d bytes, past %d, with the compacted part ending at %d", d.l.end, at, d.l.hdr.CompactedEnd)
		}
	}
	// In generation 1, due once the file holds a megabyte past its header.
	upTo(format.HeaderSize + minDead)
	if d.l.Due() {
		t.Error("due without the lock")
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	// Live data of about 300 KB: due a megabyte past it.
	upTo(int64(d.l.hdr.CompactedEnd) + minDead)
	// Live data of about 1.8 MB: due at twice that.
	for i := range 5 {
		put(fmt.Sprintf("b:%d", i))
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	upTo(2 * int64(d.l.hdr.CompactedEnd))

	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	trap.set(failNth("sync db.compact", 1))
	err := d.compactLocked()
	trap.set(nil)
	if err == nil {
		t.Fatal("the compaction worked with its sync failing")
	}
	retry := d.l.end + (d.l.end - int64(d.l.hdr.CompactedEnd))
	d.l.Unlock()
	upTo(retry)
}

// TestAWaitersLookDoesntStopACompaction: a writer waiting for the lock looks
// for a compaction under way by taking NAME.compact's lock for a moment, and
// can do so in the moment between a compaction making its file and locking
// it. The compaction tries the lock again, and goes on.
func TestAWaitersLookDoesntStopACompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	trap := &trapFS{FS: fsys.OS{}}
	d := openDBT(t, trap, path, Options{})
	fill(t, d, 1, 5)
	looked := false
	trap.set(func(call string) error {
		if call != "trylock db.compact" || looked {
			return nil
		}
		looked = true
		f, err := fsys.OS{}.Open(path + ".compact")
		if err != nil {
			t.Error(err)
			return nil
		}
		if ok, err := f.TryLock(); !ok || err != nil {
			t.Errorf("the waiter's look got %v, %v", ok, err)
		}
		time.AfterFunc(5*time.Millisecond, func() { f.Close() })
		return nil
	})
	err := d.compact()
	trap.set(nil)
	if err != nil || !looked {
		t.Fatalf("Compact, with a waiter's look at NAME.compact before its lock: %v", err)
	}
	if got := stateOfFile(t, path); got != d.state() || headerOf(t, path).Gen != 2 {
		t.Error("the compaction didn't switch to a file with the live data")
	}
}

// slowFS is the real calls, with a compaction's sync of its file slowed by
// delay, as on a slow disk or with a large database. The syncs of the commits
// made to the file once it has taken the database's path are as fast as
// ever.
type slowFS struct {
	fsys.FS
	delay time.Duration
}

func newSlowFS(delay time.Duration) *slowFS { return &slowFS{FS: fsys.OS{}, delay: delay} }

func (s *slowFS) Create(path string, perm fs.FileMode) (fsys.File, error) {
	f, err := s.FS.Create(path, perm)
	if err == nil && strings.HasSuffix(path, ".compact") {
		f = &slowFile{File: f, s: s}
	}
	return f, err
}

type slowFile struct {
	fsys.File
	s    *slowFS
	once sync.Once
}

func (f *slowFile) Sync() error {
	f.once.Do(func() { time.Sleep(f.s.delay) })
	return f.File.Sync()
}

// tablesAfter returns the tables the commits after the compacted part of the
// database at path create, in order.
func tablesAfter(t *testing.T, path string) []string {
	t.Helper()
	h := headerOf(t, path)
	r := reread(t, path)
	var got []string
	for _, b := range r.batches[len(batchesOf(t, path)):] {
		for _, c := range b.changes {
			if c.Op == format.CreateTable {
				got = append(got, c.Table)
			}
		}
	}
	if h.Gen < 2 {
		t.Errorf("the database at the path is of generation %d", h.Gen)
	}
	return got
}

// waitOut runs a slow compaction, of delay, on a database, while three
// writers wait for the lock, each with a wait of wait: another goroutine on
// the compacting Log, which waits for the mutex; another Log in this
// process, which waits for flock on the old file; and a helper process,
// which waits for flock on the old file too. It returns how long each of
// them waited, and checks that each commit went into the compacted file.
func waitOut(t *testing.T, wait, delay time.Duration) map[string]time.Duration {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	slow := newSlowFS(delay)
	d := openDBT(t, slow, path, Options{Wait: wait})
	fill(t, d, 1, 20)
	b, _ := openLog(t, path, Options{Wait: wait})
	h := start(t, "wait", path, "HYPERCRUX_LOGFILE_WAIT="+wait.String())
	if s := h.line(t); s != "ready" {
		t.Fatalf("the helper said %q", s)
	}

	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	type result struct {
		who  string
		took time.Duration
		err  error
	}
	results := make(chan result, 2)
	waiter := func(who string, l *Log) {
		began := time.Now()
		err := l.Lock()
		took := time.Since(began)
		if err == nil {
			err = l.Append(table(who))
			if e := l.Unlock(); err == nil {
				err = e
			}
		}
		results <- result{who, took, err}
	}
	go waiter("mutex", d.l)
	go waiter("flock", b)
	fmt.Fprintln(h.in, "go")
	time.Sleep(min(wait/4, 100*time.Millisecond)) // so they're waiting by the time the compaction begins
	began := time.Now()
	if err := d.compactLocked(); err != nil {
		t.Fatal(err)
	}
	compacted := time.Since(began)
	if err := d.l.Unlock(); err != nil {
		t.Fatal(err)
	}
	took := map[string]time.Duration{"compaction": compacted}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Errorf("the writer waiting for the %s: %v, after %v", r.who, r.err, r.took)
		}
		took[r.who] = r.took
	}
	line := h.line(t)
	if s, ok := strings.CutPrefix(line, "committed after "); !ok {
		t.Errorf("the helper said %q", line)
	} else if took["process"], _ = time.ParseDuration(s); took["process"] == 0 {
		t.Errorf("the helper said %q", line)
	}
	h.end(t)
	if t.Failed() {
		return took
	}
	got := tablesAfter(t, path)
	slices.Sort(got)
	if !slices.Equal(got, []string{"flock", "mutex", "waited"}) {
		t.Errorf("the commits after the compacted part create %q", got)
	}
	if n := names(t, dir); n != "db" {
		t.Errorf("the folder holds %q", n)
	}
	for _, who := range []string{"mutex", "flock", "process"} {
		if took[who] < delay {
			t.Errorf("the writer waiting for the %s got the lock after %v, during a compaction of %v", who, took[who], delay)
		}
	}
	t.Logf("a compaction of %v, with writers waiting %v each: the mutex %v, flock %v, another process %v", compacted.Round(time.Millisecond), wait,
		took["mutex"].Round(time.Millisecond), took["flock"].Round(time.Millisecond), took["process"].Round(time.Millisecond))
	return took
}

// TestWritersWaitACompactionOut: while a compaction runs, writers wait past
// their usual wait, in this process, for the mutex or for flock, and in
// another process, and once it's done, they commit to the compacted file.
// Here the waits are 400 ms, and the compaction takes 1.5 seconds.
func TestWritersWaitACompactionOut(t *testing.T) {
	t.Parallel()
	waitOut(t, 400*time.Millisecond, 1500*time.Millisecond)
}

// TestAWriterWaitsACompactionOutPastTenSeconds: the same with the default
// wait of 10 seconds, and a compaction that takes 11.5. It runs alongside
// the other parallel tests, and isn't run with -short, or with a planted bug
// switched on, as TestTheLockWaitsTenSeconds isn't.
func TestAWriterWaitsACompactionOutPastTenSeconds(t *testing.T) {
	if testing.Short() || os.Getenv("HYPERCRUX_PLANT") != "" {
		t.Skip("an 11.5-second compaction")
	}
	t.Parallel()
	took := waitOut(t, DefaultWait, 11500*time.Millisecond)
	if took["process"] <= DefaultWait {
		t.Errorf("the other process got the lock after %v", took["process"])
	}
}

// TestAStuckCompactionHoldsWritersOff: a waiting writer waits for a
// compaction no longer than Options.Wait after it was last seen under way,
// so a compaction that sticks after its rename holds writers off as a stuck
// commit does, until the stuck Log is closed.
func TestAStuckCompactionHoldsWritersOff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	const wait = 300 * time.Millisecond
	slow := newSlowFS(2 * wait)
	trap := &trapFS{FS: slow}
	trap.set(failNth("syncdir", 1))
	d := openDBT(t, slow, path, Options{Wait: wait})
	fill(t, d, 1, 10)
	d.l.fsys = trap
	b, _ := openLog(t, path, Options{Wait: wait})
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	var took time.Duration
	go func() {
		began := time.Now()
		err := lockErr(b)
		took = time.Since(began)
		errc <- err
	}()
	if err := d.compactLocked(); !errors.Is(err, errs.ErrStuck) {
		t.Fatalf("Compact, when the folder's sync fails: %v", err)
	}
	d.l.Unlock()
	if err := <-errc; !errors.Is(err, errs.ErrLockTimeout) {
		t.Fatalf("Lock while the compaction is stuck: %v", err)
	}
	if took < 2*wait || took > 2*wait+3*wait {
		t.Errorf("Lock gave up after %v, where the compaction took %v and the wait is %v", took, 2*wait, wait)
	}
}

// TestFollowersFindTheCompactedFile: another Log that follows the database
// finds another file at the path once the compaction's rename is made, and
// gets ErrReplaced, for F9's reload, having read nothing. Its next Lock moves
// to the compacted file, which holds what the old one held.
func TestFollowersFindTheCompactedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	d := openDBT(t, fsys.OS{}, path, Options{})
	fill(t, d, 1, 12)
	o := openDBT(t, fsys.OS{}, path, Options{})
	if err := o.l.Follow(); err != nil {
		t.Fatal(err)
	}
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := o.l.Follow(); !errors.Is(err, ErrReplaced) {
			t.Fatalf("Follow after a compaction: %v", err)
		}
	}
	if o.c.resets != 0 || o.state() != d.state() {
		t.Error("Follow changed the copy")
	}
	fill(t, o, 13, 14)
	if o.c.resets != 1 {
		t.Errorf("the follower's copy was reset %d times", o.c.resets)
	}
	if err := d.l.Follow(); err != nil {
		t.Fatal(err)
	}
	if o.state() != d.state() || stateOfFile(t, path) != d.state() {
		t.Error("the two copies and the file differ")
	}
}

// TestTheRateACompactionWritesAt measures, roughly, how fast a compaction
// writes and syncs the live data on this machine, for BETA.md's estimate of
// half a gigabyte a second: 20,000 records with 384-value vectors, about
// 32 MB, or HYPERCRUX_COMPACT_RECORDS of them. It logs what it found, and
// isn't run with -short, or with a planted bug switched on.
func TestTheRateACompactionWritesAt(t *testing.T) {
	if testing.Short() || os.Getenv("HYPERCRUX_PLANT") != "" {
		t.Skip("a measurement")
	}
	records := 20000
	if s := os.Getenv("HYPERCRUX_COMPACT_RECORDS"); s != "" {
		fmt.Sscan(s, &records)
	}
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	r := rand.New(rand.NewPCG(4, 1))
	vec := make([]float32, 384)
	w.commit([]format.Change{{Op: format.CreateTable, Table: "docs"}})
	for i := 0; i < records; {
		var changes []format.Change
		for ; len(changes) < 1000 && i < records; i++ {
			for j := range vec {
				vec[j] = float32(r.NormFloat64())
			}
			changes = append(changes, format.Change{Op: format.Put, Key: fmt.Sprintf("docs:%07d", i), Fields: []format.Field{
				{Name: "title", Value: value.Text(fmt.Sprintf("document %d", i))},
				{Name: "vec", Value: value.Vector(vec)},
			}})
		}
		w.commit(changes)
	}
	w.write(path)
	timer := &timeFS{FS: fsys.OS{}}
	d := openDBT(t, timer, path, Options{})
	if err := d.l.Lock(); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := d.compactLocked(); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	d.l.Unlock()
	size := float64(d.l.end)
	t.Logf("%d records, %.1f MB live: the compaction took %v, %.0f MB/s; of that, %v writing (%d writes) and %v in the sync, %v for the folder's",
		records, size/1e6, took.Round(time.Millisecond), size/1e6/took.Seconds(),
		timer.write.Round(time.Millisecond), timer.writes, timer.sync.Round(time.Millisecond), timer.dir.Round(time.Millisecond))
}

// timeFS is the real calls, adding up the time a compaction's file spends
// in its writes and its sync, and the folder's sync.
type timeFS struct {
	fsys.FS
	write, sync, dir time.Duration
	writes           int
}

func (f *timeFS) Create(path string, perm fs.FileMode) (fsys.File, error) {
	file, err := f.FS.Create(path, perm)
	if err == nil && strings.HasSuffix(path, ".compact") {
		file = &timeFile{File: file, fs: f}
	}
	return file, err
}

func (f *timeFS) SyncDir(dir string) error {
	began := time.Now()
	defer func() { f.dir += time.Since(began) }()
	return f.FS.SyncDir(dir)
}

type timeFile struct {
	fsys.File
	fs *timeFS
}

func (f *timeFile) WriteAt(p []byte, off int64) (int, error) {
	began := time.Now()
	defer func() { f.fs.write += time.Since(began); f.fs.writes++ }()
	return f.File.WriteAt(p, off)
}

func (f *timeFile) Sync() error {
	began := time.Now()
	defer func() { f.fs.sync += time.Since(began) }()
	return f.File.Sync()
}
