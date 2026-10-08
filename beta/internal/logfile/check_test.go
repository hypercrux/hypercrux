// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// endLists are the change lists of the batches endCases write, in order.
var endLists = [][]format.Change{table("one"), table("two"), table("three")}

// endCase is a file whose log ends in one of the ways a crash, or damage,
// can leave it, after FORMAT.md's table under "Checking the end of the log".
type endCase struct {
	name  string
	build func(w *builder) // writes the file, after its header
	read  int              // the batches before the end of the log, which opening applies

	// What the check makes of it: want writes the file the check leaves,
	// after a header, in the same way. When want is nil, the check reports
	// damage, naming the batch damage, and changes nothing.
	want   func(w *builder)
	damage uint64
}

// endCases are the ends of a log that TestReadingStopsAtTheEndOfTheLog
// reads and TestTheCheckOfTheEndOfTheLog checks.
func endCases() []endCase {
	one, two, three := endLists[0], endLists[1], endLists[2]
	upTo := func(n int) func(w *builder) {
		return func(w *builder) {
			for _, c := range endLists[:n] {
				w.commit(c)
			}
		}
	}
	torn := func(w *builder) {
		w.commit(one)
		w.marker(2, w.batch(two))
		w.b = w.b[:len(w.b)-9]
	}
	return []endCase{
		// Nothing: the log is whole.
		{name: "only a header", build: upTo(0), want: upTo(0)},
		{name: "a whole log", build: upTo(2), read: 2, want: upTo(2)},

		// Zeros, or the start of a batch: a commit a crash cut short.
		{name: "a few bytes", build: func(w *builder) { w.commit(one); w.b = append(w.b, "HCRB\x01\x00\x00"...) }, read: 1, want: upTo(1)},
		{name: "zeros", build: func(w *builder) { w.commit(one); w.b = append(w.b, make([]byte, 300)...) }, read: 1, want: upTo(1)},
		{name: "a batch's head alone", build: func(w *builder) {
			w.commit(one)
			at := len(w.b)
			w.batch(two)
			w.b = w.b[:at+format.BatchHeadSize]
		}, read: 1, want: upTo(1)},
		{name: "a batch cut short", build: func(w *builder) { w.commit(one); w.batch(two); w.b = w.b[:len(w.b)-5] }, read: 1, want: upTo(1)},
		{name: "a batch that fails its checksum", build: func(w *builder) {
			w.commit(one)
			at := len(w.b)
			w.batch(two)
			w.b[at+format.BatchHeadSize+2] ^= 0x20
		}, read: 1, want: upTo(1)},
		{name: "a torn batch holding a copy of the database", build: func(w *builder) {
			// The copy's markers name batches 1 and 2, which come before the
			// end of the log, so they show no commit past it.
			w.commit(one)
			w.commit(two)
			copied := value.Bytes(string(w.b))
			w.batch([]format.Change{{Op: format.Put, Key: "files:1", Fields: []format.Field{{Name: "data", Value: copied}}}})
			w.b = w.b[:len(w.b)-10]
		}, read: 2, want: upTo(2)},

		// A batch that counts, with valid changes, and nothing or a torn
		// marker after it: a writer died after writing it.
		{name: "the first batch without its marker", build: func(w *builder) { w.batch(one) }, want: upTo(1)},
		{name: "a batch without its marker", build: func(w *builder) { w.commit(one); w.batch(two) }, read: 1, want: upTo(2)},
		{name: "a torn marker", build: torn, read: 1, want: upTo(2)},
		{name: "a batch with no room for its marker", build: func(w *builder) { w.commit(one); w.commit(two); w.b = w.b[:len(w.b)-1] }, read: 1, want: upTo(2)},
		{name: "a marker with a bit changed", build: func(w *builder) { w.commit(one); w.commit(two); w.b[len(w.b)-3] ^= 4 }, read: 1, want: upTo(2)},
		{name: "a marker from another database", build: func(w *builder) {
			w.commit(one)
			sum := w.batch(two)
			w.b = format.AppendMarker(w.b, [16]byte{'o', 't', 'h', 'e', 'r'}, w.hdr.Gen, format.Marker{Seq: 2, Sum: sum})
		}, read: 1, want: upTo(2)},
		{name: "a torn marker, then zeros", build: func(w *builder) { torn(w); w.b = append(w.b, make([]byte, 700)...) }, read: 1, want: upTo(2)},
		{name: "a batch without its marker, then the start of another", build: func(w *builder) {
			w.commit(one)
			w.batch(two)
			at := len(w.b)
			w.batch(three)
			w.b = w.b[:at+40]
		}, read: 1, want: upTo(2)},
		{name: "two batches that count, neither marked", build: func(w *builder) { w.commit(one); w.batch(two); w.batch(three) }, read: 1, want: upTo(2)},

		// A batch that counts, then a whole marker naming another batch.
		{name: "a marker naming another batch", build: func(w *builder) { w.commit(one); w.marker(2, w.batch(two)+1) }, read: 1, damage: 2},
		{name: "a marker naming the batch before", build: func(w *builder) { w.commit(one); w.batch(two); w.marker(1, crc(w, 1)) }, read: 1, damage: 2},
		{name: "a marker naming a later batch", build: func(w *builder) { w.commit(one); w.marker(3, w.batch(two)) }, read: 1, damage: 2},

		// A whole marker further on, naming the next sequence number or a
		// later one: damage, which the look past the end of the log finds.
		{name: "a marked batch that fails its checksum", build: func(w *builder) {
			w.commit(one)
			at := len(w.b)
			w.commit(two)
			w.b[at+format.BatchHeadSize+2] ^= 0x20
		}, read: 1, damage: 2},
		{name: "a marked batch with the wrong sequence number", build: func(w *builder) { w.commit(one); w.marker(3, w.raw(1, 3, []byte("X\x03\x00two"))) }, read: 1, damage: 2},
		{name: "a marked batch of another generation", build: func(w *builder) { w.commit(one); w.marker(2, w.raw(2, 2, []byte("X\x03\x00two"))) }, read: 1, damage: 2},
		{name: "a whole marker past zeros", build: func(w *builder) {
			w.commit(one)
			w.b = append(w.b, make([]byte, 64)...)
			w.marker(2, 0x1234)
		}, read: 1, damage: 2},
		{name: "a batch's head of zeros, then its marker and another batch", build: func(w *builder) {
			w.commit(one)
			at := len(w.b)
			w.commit(two)
			clear(w.b[at : at+format.BatchHeadSize])
			w.commit(three)
		}, read: 1, damage: 2},
		{name: "a batch that counts without its marker, then a marked batch", build: func(w *builder) { w.commit(one); w.batch(two); w.commit(three) }, read: 1, damage: 2},
	}
}

// holdLock takes flock on the database at path through an open file of its
// own, as another process's writer holds it, and returns a function that
// lets go. The function can be called more than once, from any goroutine,
// and lets go the first time.
func holdLock(t *testing.T, path string) func() {
	t.Helper()
	f, err := fsys.OS{}.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.TryLock(); !ok || err != nil {
		f.Close()
		t.Fatalf("TryLock gave %v, %v", ok, err)
	}
	var once sync.Once
	return func() { once.Do(func() { f.Close() }) }
}

// releaseAfter lets go of a lock holdLock took, once d has gone by, and
// lets go when the test ends if that's sooner.
func releaseAfter(t *testing.T, release func(), d time.Duration) {
	timer := time.AfterFunc(d, release)
	t.Cleanup(func() {
		timer.Stop()
		release()
	})
}

// TestTheCheckOfTheEndOfTheLog takes each of endCases through the check of
// the end of the log, both ways a writer runs it: Open, when it gets the
// write lock without waiting, and Lock, before it appends anything. A batch
// that counts and lost its marker is written again and marked, and handed
// to the Target, and everything after the last marker is cut off. Damage is
// reported, and the file stays as it was. Each file the check leaves opens
// again without change, and takes a commit after its last batch.
//
// For Lock, the Log is opened on the file as far as the end of its log,
// and then the rest is written in place, as another process would leave
// it, so the Log's own Open has nothing to check or confirm.
func TestTheCheckOfTheEndOfTheLog(t *testing.T) {
	for _, c := range endCases() {
		t.Run(c.name, func(t *testing.T) {
			w := newBuilder(t)
			c.build(w)
			var want *builder
			if c.want != nil {
				want = newBuilder(t)
				c.want(want)
			}
			marked := int(c.damage) - 1
			if want != nil {
				marked = int(want.seq)
			}

			t.Run("Open", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "db")
				w.write(path)
				rec := &recorder{}
				_, err := Open(fsys.OS{}, path, rec, Options{})
				if want == nil {
					mustBeDamage(t, err, path, c.damage)
					if !bytes.Equal(fileBytes(t, path), w.b) {
						t.Error("the file changed")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				rec.holds(t, endLists[:marked]...)
				if !bytes.Equal(fileBytes(t, path), want.b) {
					t.Fatalf("the check left\n  % x\nwhere\n  % x\nis wanted", fileBytes(t, path), want.b)
				}
				reread(t, path).holds(t, endLists[:marked]...)
				if !bytes.Equal(fileBytes(t, path), want.b) {
					t.Error("opening the file again changed it")
				}
			})

			t.Run("Lock", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "db")
				read := newBuilder(t)
				for _, list := range endLists[:c.read] {
					read.commit(list)
				}
				if !bytes.HasPrefix(w.b, read.b) {
					t.Fatal("the case doesn't start with the batches it says opening reads")
				}
				read.write(path)
				l, rec := openLog(t, path, Options{})
				rec.holds(t, endLists[:c.read]...)
				w.write(path)
				err := lockErr(l)
				if want == nil {
					mustBeDamage(t, err, path, c.damage)
					if !bytes.Equal(fileBytes(t, path), w.b) {
						t.Error("the file changed")
					}
					return
				}
				if err.Error() != "Lock worked" {
					t.Fatal(err)
				}
				rec.holds(t, endLists[:marked]...)
				next := table("next")
				commit(t, l, next)
				want.commit(next)
				if !bytes.Equal(fileBytes(t, path), want.b) {
					t.Errorf("after the check and a commit, the file holds\n  % x\nwhere\n  % x\nis wanted", fileBytes(t, path), want.b)
				}
				reread(t, path).holds(t, append(endLists[:marked:marked], next)...)
			})
		})
	}
}

// TestOpenReadsOnBeforeItChecks: another writer commits two batches between
// Open's read of the log and its try at the lock. Holding the lock, Open
// reads them before it checks the end of the log, so it hands them to the
// Target and leaves them as they are. Checking from where the first read
// ended, it would take the first for a batch that lost its marker, and cut
// the second. And it makes the checks a writer makes once it holds the
// lock, as Lock does.
func TestOpenReadsOnBeforeItChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	other, _ := openLog(t, path, Options{})
	commit(t, other, table("one"))
	h := &hookFS{FS: fsys.OS{}, file: func(f fsys.File) fsys.File {
		return &lockHook{File: f, before: func() { commit(t, other, table("two"), table("three")) }}
	}}
	l, err := Open(h, path, &recorder{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.t.(*recorder).holds(t, table("one"), table("two"), table("three"))
	reread(t, path).holds(t, table("one"), table("two"), table("three"))

	// Another database copied over the file in place, between the read and
	// the try at the lock, is damage, as it is for Lock, and Open changes
	// nothing.
	copied := filepath.Join(t.TempDir(), "copied")
	another(t, copied, "x1", "x2", "x3", "x4")
	h.file = func(f fsys.File) fsys.File {
		return &lockHook{File: f, before: func() { copyOver(t, copied, path) }}
	}
	_, err = Open(h, path, &recorder{}, Options{})
	var d *errs.Damage
	if !errors.As(err, &d) || d.Path != path || d.Batch != 3 {
		t.Fatalf("Open of a file copied over before its try at the lock gave %v, where damage naming batch 3 is wanted", err)
	}
	if !bytes.Equal(fileBytes(t, path), fileBytes(t, copied)) {
		t.Error("the file changed")
	}
}

// lockHook is a file whose first TryLock calls before first.
type lockHook struct {
	fsys.File
	before func()
}

func (f *lockHook) TryLock() (bool, error) {
	if f.before != nil {
		f.before()
		f.before = nil
	}
	return f.File.TryLock()
}

// mustBeDamage checks that err is damage in the file at path, naming the
// batch seq.
func mustBeDamage(t *testing.T, err error, path string, seq uint64) {
	t.Helper()
	var d *errs.Damage
	if !errors.As(err, &d) || d.Path != path || d.Batch != seq {
		t.Fatalf("%v, where damage naming batch %d is wanted", err, seq)
	}
}

// TestAWriterThatDiedLeavesItsBatchForTheNext: a helper process takes the
// write lock, writes a batch and syncs it, and dies before it writes the
// marker, which lets go of its flock. The next holder of the lock, in
// another process, checks the end of the log before it appends: the whole
// batch is written again, marked and handed to its Target before its own
// commit goes in after it, and half a batch is cut off.
func TestAWriterThatDiedLeavesItsBatchForTheNext(t *testing.T) {
	for _, tail := range []string{"whole", "half"} {
		t.Run(tail, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			l, rec := openLog(t, path, Options{})
			commit(t, l, table("one"))
			h := start(t, "die", path, "HYPERCRUX_LOGFILE_TAIL="+tail)
			if s := h.line(t); s != "written" {
				t.Fatalf("the helper said %q", s)
			}
			h.end(t)
			commit(t, l, table("mine"))
			if tail == "whole" {
				rec.holdsFrom(t, 2, helperDied)
				reread(t, path).holds(t, table("one"), helperDied, table("mine"))
			} else {
				rec.holds(t)
				reread(t, path).holds(t, table("one"), table("mine"))
			}
		})
	}
}

// TestTheTargetsDamageIsNamedOnce: the store refuses a batch with a
// *errs.Damage that names the batch and the change already (S3), so the
// log takes it as its error, as it takes the codec's, with the path filled
// in and the batch's offset added. It doesn't wrap it in damage of its own,
// which would name the batch twice. That holds for a marked batch on
// opening, and for a batch the check marks: the Target judges the rules for
// the state, and takes a batch only once it's marked, so the check marks
// it before the Target finds it breaks them, and every open after reports
// the same damage.
func TestTheTargetsDamageIsNamedOnce(t *testing.T) {
	for _, marked := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "db")
		w := newBuilder(t)
		w.commit(table("one"))
		at := int64(len(w.b))
		if marked {
			w.commit(table("two"))
			w.commit(table("three"))
		} else {
			w.batch(table("two"))
		}
		w.write(path)
		check := func(when string, rec *recorder) {
			t.Helper()
			_, err := Open(fsys.OS{}, path, rec, Options{})
			var d *errs.Damage
			if !errors.As(err, &d) || d.Path != path || d.Batch != 2 || d.Offset != at || d.Reason != storeDamage {
				t.Fatalf("%s, Open gave %v, where the Target's damage in batch 2 at offset %d is wanted", when, err, at)
			}
			if n := strings.Count(err.Error(), "batch 2"); n != 1 {
				t.Errorf("%s, the error names batch 2 %d times: %v", when, n, err)
			}
		}
		check("with the batch marked "+map[bool]string{true: "already", false: "by the check"}[marked], &recorder{refuse: 2, damage: true})
		check("opened again", &recorder{refuse: 2, damage: true})
		if !marked {
			reread(t, path).holds(t, table("one"), table("two"))
		}
	}
}

// TestAFailedCheckIsHandledLikeAFailedCommit: when the sync of a batch the
// check writes again fails, Lock fails, saying the outcome is unknown, and
// the Log then handles the bytes as F2 handles a failed commit's, until F5:
// it appends nothing after them and doesn't check them, while another
// Log's check marks the batch. Once the log has moved on past it, the first
// Log appends again.
func TestAFailedCheckIsHandledLikeAFailedCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w := newBuilder(t)
	w.commit(table("one"))
	w.batch(table("two"))
	w.write(path)
	fail := 0
	h := &hookFS{FS: fsys.OS{}, file: func(f fsys.File) fsys.File { return &spyFile{File: f, failSync: &fail} }}
	release := holdLock(t, path)
	l, err := Open(h, path, &recorder{}, Options{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fail = 1
	err = lockErr(l)
	if !strings.Contains(err.Error(), "the drive failed the sync") || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Lock when the check's sync fails: %v", err)
	}
	if !bytes.Equal(fileBytes(t, path), w.b) {
		t.Error("the file changed")
	}
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(table("three")); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Append after the failed check: %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	reread(t, path).holds(t, table("one"), table("two"))
	commit(t, l, table("three"))
	reread(t, path).holds(t, table("one"), table("two"), table("three"))
}

// compacted returns a builder holding a compacted file of generation 2: a
// header naming the batch it continues from and where the compacted part
// ends, and a compacted part of the batches given, each with its marker.
func compacted(t *testing.T, lists ...[]format.Change) *builder {
	w := newBuilder(t)
	w.hdr = format.Header{ID: w.hdr.ID, Gen: 2, FromGen: 1, FromSeq: 9, FromSum: 0x1234}
	w.b = make([]byte, format.HeaderSize)
	for _, c := range lists {
		w.commit(c)
	}
	w.hdr.CompactedEnd = uint64(len(w.b))
	head, err := format.AppendHeader(nil, w.hdr)
	if err != nil {
		t.Fatal(err)
	}
	copy(w.b, head)
	return w
}

// TestTheCheckNeverCutsIntoACompactedPart: a compacted file is whole before
// anyone can see it, so a log that ends inside its compacted part is
// damage, and the check reports it and cuts nothing, where it would
// otherwise cut a batch of the compacted part and every commit after it. A
// torn end after the compacted part is cut as in any file.
func TestTheCheckNeverCutsIntoACompactedPart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	w := compacted(t, table("one"), table("two"))
	at := len(w.b)
	w.commit(table("three"))
	w.b[at-format.MarkerSize-5] ^= 1 // in the compacted part's second batch
	w.write(path)
	_, err := Open(fsys.OS{}, path, &recorder{}, Options{})
	var d *errs.Damage
	if !errors.As(err, &d) || d.Path != path || d.Batch != 2 || !strings.Contains(d.Reason, "compacted part") {
		t.Fatalf("Open gave %v, where damage in the compacted part is wanted", err)
	}
	if !bytes.Equal(fileBytes(t, path), w.b) {
		t.Error("the file changed")
	}

	w = compacted(t, table("one"), table("two"))
	w.commit(table("three"))
	whole := len(w.b)
	w.batch(table("four"))
	w.b = w.b[:len(w.b)-3]
	w.write(path)
	reread(t, path).holds(t, table("one"), table("two"), table("three"))
	if got := fileBytes(t, path); !bytes.Equal(got, w.b[:whole]) {
		t.Errorf("the file is %d bytes long, where the check leaves %d", len(got), whole)
	}
}
