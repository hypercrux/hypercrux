// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// TestBatchesSurviveAReopen is the first half of F2's closing test: random
// change lists, committed over several opens of one database, come back
// from every reopen in order, numbered from 1, and the file holds exactly
// the header and each batch with its marker after it.
func TestBatchesSurviveAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	r := rand.New(rand.NewPCG(2, 8))
	var want [][]format.Change
	var file []byte
	for round := range 8 {
		l, rec := openLog(t, path, Options{})
		rec.holds(t, want...)
		if round == 0 {
			file = append(file, l.head[:]...)
		}
		for range 2 + r.IntN(3) {
			// Each Lock commits one batch or a few, as transactions in a
			// row would.
			var lists [][]format.Change
			for range 1 + r.IntN(2) {
				lists = append(lists, randomChanges(r))
			}
			commit(t, l, lists...)
			for _, c := range lists {
				want = append(want, c)
				b, sum, err := format.AppendBatch(file, l.hdr.Gen, uint64(len(want)), c)
				if err != nil {
					t.Fatal(err)
				}
				file = format.AppendMarker(b, l.hdr.ID, l.hdr.Gen, format.Marker{Seq: uint64(len(want)), Sum: sum})
			}
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		if got := fileBytes(t, path); !bytes.Equal(got, file) {
			t.Fatalf("after round %d the file holds %d bytes that differ from the %d the codec makes of the header and the batches", round, len(got), len(file))
		}
	}
	reread(t, path).holds(t, want...)
	if len(want) < 20 {
		t.Errorf("only %d batches were committed", len(want))
	}
	t.Logf("%d batches over 8 opens", len(want))
}

// TestReadingStopsAtTheEndOfTheLog writes files whose logs end in each of
// the ways FORMAT.md lists, and checks that opening applies the marked
// batches before the end and nothing after it. Holding the lock, F2
// refuses to append after a tail it hasn't checked, and leaves it as it is,
// so each file comes through Lock and a refused Append unchanged.
func TestReadingStopsAtTheEndOfTheLog(t *testing.T) {
	one, two := table("one"), table("two")
	cases := []struct {
		name  string
		build func(w *builder)
		read  int // the batches before the end of the log
	}{
		{"only a header", func(w *builder) {}, 0},
		{"a whole log", func(w *builder) { w.commit(one); w.commit(two) }, 2},
		{"a batch without its marker", func(w *builder) { w.commit(one); w.batch(two) }, 1},
		{"a torn marker", func(w *builder) {
			w.commit(one)
			w.marker(2, w.batch(two))
			w.b = w.b[:len(w.b)-9]
		}, 1},
		{"a marker naming another batch", func(w *builder) { w.commit(one); w.marker(2, w.batch(two)+1) }, 1},
		{"a marker naming the batch before", func(w *builder) {
			w.commit(one)
			w.batch(two)
			w.marker(1, crc(w, 1))
		}, 1},
		{"zeros", func(w *builder) { w.commit(one); w.b = append(w.b, make([]byte, 300)...) }, 1},
		{"a batch cut short", func(w *builder) {
			w.commit(one)
			w.batch(two)
			w.b = w.b[:len(w.b)-5]
		}, 1},
		{"a marked batch that fails its checksum", func(w *builder) {
			w.commit(one)
			start := len(w.b)
			w.commit(two)
			w.b[start+format.BatchHeadSize+2] ^= 0x20 // F4 reports this one as damage
		}, 1},
		{"a batch with the wrong sequence number", func(w *builder) {
			w.commit(one)
			w.marker(3, w.raw(1, 3, []byte("X\x03\x00two")))
		}, 1},
		{"a batch of another generation", func(w *builder) {
			w.commit(one)
			w.marker(2, w.raw(2, 2, []byte("X\x03\x00two")))
		}, 1},
		{"a batch with no room for its marker", func(w *builder) { w.commit(one); w.commit(two); w.b = w.b[:len(w.b)-1] }, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			w := newBuilder(t)
			c.build(w)
			w.write(path)

			l, rec := openLog(t, path, Options{})
			rec.holds(t, [][]format.Change{one, two}[:c.read]...)
			if err := l.Lock(); err != nil {
				t.Fatal(err)
			}
			err := l.Append(table("three"))
			tail := l.end < int64(len(w.b))
			if tail && !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("Append after a tail nothing has checked: %v", err)
			}
			if !tail && err != nil {
				t.Errorf("Append after a whole log: %v", err)
			}
			if err := l.Unlock(); err != nil {
				t.Fatal(err)
			}
			if tail && !bytes.Equal(fileBytes(t, path), w.b) {
				t.Error("the file changed")
			}
		})
	}
}

// crc returns the checksum of the batch numbered seq in what w has written.
func crc(w *builder, seq uint64) uint32 {
	off := int64(format.HeaderSize)
	for s := uint64(1); ; s++ {
		bt, err := format.DecodeBatch(w.b[off:], w.hdr.Gen, s)
		if err != nil {
			w.t.Fatal(err)
		}
		if s == seq {
			return bt.Sum
		}
		off += int64(bt.Length) + format.MarkerSize
	}
}

// TestDamageInABatchThatCounts: a batch that counts holds what its writer
// wrote, so changes in it that break the rules are damage, marked or not,
// with the path, the batch and the offset of the bad change in the file.
func TestDamageInABatchThatCounts(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(fmt.Sprintf("marked %v", marked), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			w := newBuilder(t)
			w.commit(table("one"))
			at := int64(len(w.b))
			sum := w.raw(1, 2, []byte("X\x03\x00Bad")) // a table name with a capital letter
			if marked {
				w.marker(2, sum)
			}
			w.write(path)
			_, err := Open(fsys.OS{}, path, &recorder{}, Options{})
			var d *errs.Damage
			if !errors.As(err, &d) || d.Path != path || d.Batch != 2 || d.Offset != at+format.BatchHeadSize {
				t.Fatalf("Open gave %v, where damage in batch 2 at offset %d is wanted", err, at+format.BatchHeadSize)
			}
			if !bytes.Equal(fileBytes(t, path), w.b) {
				t.Error("the file changed")
			}
		})
	}
}

// TestARefusedBatchIsDamage: a marked batch whose changes the Target
// refuses breaks the rules for the state, which no crash explains.
func TestARefusedBatchIsDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	w.commit(table("one"))
	at := int64(len(w.b))
	w.commit(table("two"))
	w.commit(table("three"))
	w.write(path)
	rec := &recorder{refuse: 2}
	_, err := Open(fsys.OS{}, path, rec, Options{})
	var d *errs.Damage
	if !errors.As(err, &d) || d.Path != path || d.Batch != 2 || d.Offset != at || !strings.Contains(d.Reason, "doesn't exist") {
		t.Fatalf("Open gave %v, where damage in batch 2 at offset %d is wanted", err, at)
	}
	if len(rec.batches) != 1 {
		t.Errorf("%d batches applied before the refused one, where 1 is wanted", len(rec.batches))
	}
}

// TestNotADatabase checks the header's errors, in FORMAT.md's order, and
// that opening changes none of the files.
func TestNotADatabase(t *testing.T) {
	good, err := format.AppendHeader(nil, format.Header{ID: [16]byte{1}, Gen: 1})
	if err != nil {
		t.Fatal(err)
	}
	version2 := bytes.Clone(good)
	version2[8] = 2
	badSum := bytes.Clone(good)
	badSum[64] ^= 1
	cases := []struct {
		name string
		file []byte
		want error
	}{
		{"text", []byte(strings.Repeat("not a database at all. ", 5)), errs.ErrNotDatabase},
		{"ten bytes", []byte("HCRX\r\n\x1a\n\x01\x00"), errs.ErrNotDatabase},
		{"a 0.x database", append([]byte("SQLite format 3\x00"), make([]byte, 100)...), errs.ErrZeroX},
		{"format version 2", version2, errs.ErrFormatVersion},
		{"a damaged header", badSum, errs.ErrDamaged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			if err := os.WriteFile(path, c.file, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Open(fsys.OS{}, path, &recorder{}, Options{})
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), path) {
				t.Errorf("Open gave %v, where %v with the path is wanted", err, c.want)
			}
			if !bytes.Equal(fileBytes(t, path), c.file) {
				t.Error("the file changed")
			}
		})
	}
}

// TestALongLogReadsThroughItsWindow reads a log of thousands of small
// batches and a few longer than the reader's window, on opening and when
// Lock reads on, so batches fall across the window's edges in every way.
func TestALongLogReadsThroughItsWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	r := rand.New(rand.NewPCG(3, 9))
	w := newBuilder(t)
	var want [][]format.Change
	longs := 0
	add := func(n int) {
		for range n {
			c := randomChanges(r)
			if r.IntN(500) == 0 {
				long := strings.Repeat("x", window+r.IntN(window))
				c = []format.Change{{Op: format.Put, Key: "docs:long", Fields: []format.Field{{Name: "body", Value: value.Text(long)}}}}
				longs++
			}
			w.commit(c)
			want = append(want, c)
		}
	}
	add(4000)
	w.write(path)
	l, rec := openLog(t, path, Options{})
	rec.holds(t, want...)

	// Another writer's commits, appended to the file, are read on by Lock.
	before := len(w.b)
	add(3000)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(w.b[before:]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	commit(t, l)
	rec.holds(t, want...)
	if l.end != int64(len(w.b)) {
		t.Errorf("the log ends at %d, where the file does at %d", l.end, len(w.b))
	}
	if longs == 0 {
		t.Error("no batch was longer than the window")
	}
	t.Logf("%d batches, %d of them longer than the window, in %d bytes", len(want), longs, len(w.b))
}

// TestACommitWritesTheBatchSyncsThenWritesTheMarker checks the calls a
// commit makes, in order, and what they write.
func TestACommitWritesTheBatchSyncsThenWritesTheMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	var calls []string
	h := &hookFS{FS: fsys.OS{}, file: func(f fsys.File) fsys.File { return &spyFile{File: f, calls: &calls} }}
	l, err := Open(h, path, &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	commit(t, l, table("one"))
	changes := []format.Change{{Op: format.Put, Key: "docs:1", Fields: []format.Field{{Name: "title", Value: value.Text("a")}}}}
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	at := l.end
	calls = nil
	if err := l.Append(changes); err != nil {
		t.Fatal(err)
	}
	batch, sum, err := format.AppendBatch(nil, 1, 2, changes)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("write %d at %d", len(batch), at),
		"sync",
		fmt.Sprintf("write %d at %d", format.MarkerSize, at+int64(len(batch))),
	}
	if strings.Join(calls, ", ") != strings.Join(want, ", ") {
		t.Errorf("a commit made the calls\n  %s\nwhere\n  %s\nare wanted", strings.Join(calls, ", "), strings.Join(want, ", "))
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	file := fileBytes(t, path)
	marker := format.AppendMarker(nil, l.hdr.ID, l.hdr.Gen, format.Marker{Seq: 2, Sum: sum})
	if !bytes.Equal(file[at:], append(batch, marker...)) {
		t.Error("the file doesn't end with the batch and its marker")
	}
}

// TestUnlockLetsGoOfFlockFirst checks the order of the write lock's
// release: flock, while the mutex is still held, then the mutex.
func TestUnlockLetsGoOfFlockFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	reread(t, path) // creates the database, so the Log below makes no calls before it's there
	var l *Log
	var held []bool
	h := &hookFS{FS: fsys.OS{}, file: func(f fsys.File) fsys.File {
		return &unlockSpy{File: f, held: func() { held = append(held, len(l.mu) == 1) }}
	}}
	l, err := Open(h, path, &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	commit(t, l, table("one"))
	if len(held) != 1 || !held[0] || len(l.mu) != 0 {
		t.Errorf("flock was let go with the mutex held: %v, and the mutex is held after Unlock: %v", held, len(l.mu) == 1)
	}
}

type unlockSpy struct {
	fsys.File
	held func()
}

func (f *unlockSpy) Unlock() error {
	f.held()
	return f.File.Unlock()
}

// TestAFailedCommitStopsAppends: until F5 cuts a failed commit back out of
// the file, the bytes it left past the end of the log are a tail nothing
// has checked, so neither this Log nor a later Lock appends after them, and
// they stay as they are.
func TestAFailedCommitStopsAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	fail := 0
	h := &hookFS{FS: fsys.OS{}, file: func(f fsys.File) fsys.File { return &spyFile{File: f, failSync: &fail} }}
	l, err := Open(h, path, &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	commit(t, l, table("one"))
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	fail = 1
	if err := l.Append(table("two")); err == nil || !strings.Contains(err.Error(), "the drive failed the sync") {
		t.Errorf("Append with a failing sync: %v", err)
	}
	if err := l.Append(table("three")); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Append after a failed one: %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	file := fileBytes(t, path)
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(table("three")); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Append under a later lock: %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fileBytes(t, path), file) {
		t.Error("the file changed")
	}
	reread(t, path).holds(t, table("one"))
}

// TestAppendRefusesBadChanges: a change the codec refuses never reaches
// the file.
func TestAppendRefusesBadChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	l, _ := openLog(t, path, Options{})
	if err := l.Append(table("one")); err == nil {
		t.Error("Append without the lock worked")
	}
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]format.Change{nil, table("Bad")} {
		if err := l.Append(c); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("Append(%v): %v, where ErrInvalid is wanted", c, err)
		}
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err == nil {
		t.Error("a second Unlock worked")
	}
	if n := len(fileBytes(t, path)); n != format.HeaderSize {
		t.Errorf("the file is %d bytes long, where the header alone is wanted", n)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lockErr(l); !errors.Is(err, errs.ErrClosed) {
		t.Errorf("Lock after Close: %v", err)
	}
	if err := l.Close(); !errors.Is(err, errs.ErrClosed) {
		t.Errorf("a second Close: %v", err)
	}
}
