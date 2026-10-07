// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// another makes a database at path with a batch for each name, as a
// backup moved into place, or a compaction's file, would be.
func another(t *testing.T, path string, tables ...string) {
	t.Helper()
	l, err := Open(fsys.OS{}, path, &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range tables {
		commit(t, l, table(name))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAReplacedFileRestartsTheCommit is the third part of F2's closing
// test. Another database is moved into place under a writer, between two
// of its commits. The second commit finds by device and inode that the
// path names another file, reads that file from its start, and goes into
// it, and the old file stays as it was.
func TestAReplacedFileRestartsTheCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	l, rec := openLog(t, path, Options{})
	commit(t, l, table("a1"))

	another(t, filepath.Join(dir, "backup"), "b1", "b2")
	if err := os.Link(path, filepath.Join(dir, "old")); err != nil { // keeps the old file to look at
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "backup"), path); err != nil {
		t.Fatal(err)
	}

	commit(t, l, table("a2"))
	if rec.resets != 1 {
		t.Errorf("the Target was reset %d times", rec.resets)
	}
	rec.holds(t, table("b1"), table("b2"))
	if l.seq != 3 {
		t.Errorf("the commit went in as batch %d, where 3 comes next in the new file", l.seq)
	}
	reread(t, path).holds(t, table("b1"), table("b2"), table("a2"))
	reread(t, filepath.Join(dir, "old")).holds(t, table("a1"))
}

// TestAFileReplacedWhileAWriterWaits: the replacement happens while the
// writer waits for the lock, as when a compaction renames its file into
// place and then lets go. The writer gets the old file's lock, finds
// another file at the path, and takes the lock there.
func TestAFileReplacedWhileAWriterWaits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	a, ra := openLog(t, path, Options{Wait: 10 * time.Second})
	b, _ := openLog(t, path, Options{})
	commit(t, b, table("b1"))
	if err := b.Lock(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- a.Lock() }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Lock returned while another Log held the lock: %v", err)
	default:
	}

	another(t, filepath.Join(dir, "compacted"), "c1", "c2")
	if err := os.Rename(filepath.Join(dir, "compacted"), path); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Lock once the file was replaced: %v", err)
	}
	err := a.Append(table("a1"))
	if e := a.Unlock(); err == nil {
		err = e
	}
	if err != nil {
		t.Fatal(err)
	}
	if ra.resets != 1 {
		t.Errorf("the Target was reset %d times", ra.resets)
	}
	ra.holds(t, table("c1"), table("c2"))
	reread(t, path).holds(t, table("c1"), table("c2"), table("a1"))
}

// TestAFileReplacedByAnEmptyOne: the commit starts again on the empty file,
// which becomes a database, with the empty file's permissions.
func TestAFileReplacedByAnEmptyOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	l, rec := openLog(t, path, Options{})
	commit(t, l, table("one"))
	first := l.hdr.ID
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(empty, path); err != nil {
		t.Fatal(err)
	}
	commit(t, l, table("two"))
	if rec.resets != 1 || len(rec.batches) != 0 {
		t.Errorf("the Target was reset %d times, and holds %d batches", rec.resets, len(rec.batches))
	}
	reread(t, path).holds(t, table("two"))
	if l.hdr.ID == first {
		t.Error("the new database has the old one's ID")
	}
	if info := inode(t, path); info.Mode != 0o600 {
		t.Errorf("the new database has mode %v", info.Mode)
	}
}

// TestAFileGoneFromItsPath: with nothing at the path, a commit has nowhere
// to go, and fails. Once a database is back at the path, the next commit
// starts again there.
func TestAFileGoneFromItsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	l, rec := openLog(t, path, Options{})
	commit(t, l, table("one"))
	if err := os.Rename(path, filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := lockErr(l); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Lock with nothing at the path: %v", err)
		}
	}
	another(t, path, "back")
	commit(t, l, table("two"))
	rec.holds(t, table("back"))
	reread(t, path).holds(t, table("back"), table("two"))
	reread(t, filepath.Join(dir, "moved")).holds(t, table("one"))
}

// TestAFileChangedInPlaceIsDamage: nothing before the end of a marker ever
// changes in a HyperCrux file. When the marker that ends what the writer
// has read, or the header when it has read no batch, is no longer there,
// or the file is shorter than what the writer has read, the file was
// copied over or cut in place, and Lock reports damage and writes nothing.
func TestAFileChangedInPlaceIsDamage(t *testing.T) {
	cases := []struct {
		name    string
		commits int                                  // the writer's commits before the change
		change  func(t *testing.T, dir, path string) // the change in place
		batch   uint64                               // the batch the damage names
	}{
		{"another database copied over it", 2, func(t *testing.T, dir, path string) {
			other := filepath.Join(dir, "other")
			another(t, other, "x1", "x2", "x3")
			copyOver(t, other, path)
		}, 2},
		{"another database copied over one with no batches", 0, func(t *testing.T, dir, path string) {
			other := filepath.Join(dir, "other")
			another(t, other)
			copyOver(t, other, path)
		}, 0},
		{"an older copy of the same database copied over it", 2, func(t *testing.T, dir, path string) {
			copyOver(t, filepath.Join(dir, "copy"), path)
		}, 0},
		{"cut to its header", 1, func(t *testing.T, dir, path string) {
			if err := os.Truncate(path, 68); err != nil {
				t.Fatal(err)
			}
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "db")
			l, _ := openLog(t, path, Options{})
			copyOver(t, path, filepath.Join(dir, "copy")) // the database with no batches yet
			for i := range c.commits {
				commit(t, l, table("t"+string(rune('a'+i))))
			}
			c.change(t, dir, path)
			before := fileBytes(t, path)
			err := lockErr(l)
			var d *errs.Damage
			if !errors.As(err, &d) || d.Path != path || d.Batch != c.batch {
				t.Fatalf("Lock after the file changed in place: %v, where damage naming batch %d is wanted", err, c.batch)
			}
			if !bytes.Equal(fileBytes(t, path), before) {
				t.Error("the file changed")
			}
		})
	}
}

// copyOver copies the file at from over the file at to, in place, as cp
// does: the same inode, with new contents.
func copyOver(t *testing.T, from, to string) {
	t.Helper()
	b := fileBytes(t, from)
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
