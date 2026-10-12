// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"errors"
	"fmt"
	"hash/crc32"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The real log, beta/internal/logfile, under the harness. Writers commit
// through it, as the public package does: Lock, the copy's transaction, its
// Commit with Append, then Unlock, with the real store as the copy (copyOf,
// in reload_test.go). Readers open the file afresh for every read, as the
// hypercrux command does, so every read goes from the first commit, and
// Open's try at the lock checks the end of the log whenever no writer holds
// it; follow_test.go has readers that open it once and follow it instead
// (F6). Final takes the lock, as the next writer would, so its check marks a
// batch that a killed writer left synced and unmarked, and cuts off whatever
// else a killed writer left. Readers and Final have the real store as the
// log's Target too, and tell the harness what the copy holds of each commit
// once its batch has gone in (I1).
//
// A commit puts one to three records of its writer's own, each with a
// field of 1 to 300 letters, and what it holds, as the harness compares
// it, names each put and the length and CRC32C of each value. So a commit
// read back with a put missing, or with a value torn, holds another string.
var realLog = Workload{Name: "real log", Write: realWrite, Read: realRead, Final: realFinal}

// realWrite is a writer's work on the real log: commits, one after
// another, until the process is killed, with a short wait between them so
// the writers take turns at the lock.
func realWrite(w *Writer) error { return realWriteOn(fsys.OS{}, w) }

// realWriteOn is realWrite through the file calls files. Each commit's puts
// go through the copy's transaction, so the first put into the writers'
// table makes the table first, as the public package's would.
func realWriteOn(files fsys.FS, w *Writer) error {
	d, err := openCopy(files, w.Path(), logfile.Options{})
	if err != nil {
		return err
	}
	defer d.l.Close()
	for n := 1; ; n++ {
		changes := realChanges(w, n)
		w.Begin(describe(changes))
		err, _ := d.commit(func(tx *store.Tx) error {
			for _, c := range changes {
				if err := tx.Put(c.Key, c.Fields); err != nil {
					return err
				}
			}
			return nil
		}, false)
		switch {
		case errors.Is(err, errs.ErrLockTimeout):
			w.Failed(err) // nothing was written, and the next commit tries again
			continue
		case err != nil:
			return err
		}
		w.Done()
		time.Sleep(time.Duration(w.Rand().IntN(1000)) * time.Microsecond)
	}
}

// realChanges is writer w's commit n: puts of one to three records of its
// own, each with a field x of 1 to 300 letters.
func realChanges(w *Writer, n int) []format.Change {
	changes := make([]format.Change, 1+w.Rand().IntN(3))
	for i := range changes {
		text := make([]byte, 1+w.Rand().IntN(300))
		for j := range text {
			text[j] = 'a' + byte(w.Rand().IntN(26))
		}
		changes[i] = format.Change{Op: format.Put, Key: fmt.Sprintf("w:%d-%d-%d", w.ID(), n, i), Fields: []format.Field{{Name: "x", Value: value.Text(string(text))}}}
	}
	return changes
}

// realRead is a reader's work on the real log: the database opened
// afresh, read from its first commit, and closed, again and again, until
// the process is killed.
func realRead(r *Reader) error {
	for {
		r.Reset()
		l, err := logfile.Open(fsys.OS{}, r.Path(), newReaderTarget(r.Apply, r.Reset), logfile.Options{})
		if err != nil {
			return err
		}
		if err := l.Close(); err != nil {
			return err
		}
		time.Sleep(time.Duration(r.Rand().IntN(5000)) * time.Microsecond)
	}
}

// realFinal reads the real log holding the write lock, as the next writer
// would, once its check of the end of the log has run. It waits longer for
// the lock than a writer does, since a reader's Open can hold it for a
// moment, and a slow machine can make that moment long.
func realFinal(path string) ([]string, error) {
	rec := &recorder{}
	l, err := logfile.Open(fsys.OS{}, path, newReaderTarget(rec.apply, rec.reset), logfile.Options{Wait: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	defer l.Close()
	if err := l.Lock(); err != nil {
		return nil, err
	}
	if err := l.Unlock(); err != nil {
		return nil, err
	}
	return rec.commits, rec.err
}

// describe is what a commit holds, as the harness compares it: each put's
// key, and each field's name and value, with text, bytes and vectors given
// by their kind, length and CRC32C. A writer describes its commit's puts
// before it commits them. A change that makes a table is left out, since
// the store adds one to the first put into a table.
func describe(changes []format.Change) string {
	return describeAs(changes, func(_ string, f format.Field) value.Value { return f.Value })
}

// describeAs is describe with each field's value as held gives it, for the
// put of the key given.
func describeAs(changes []format.Change, held func(key string, f format.Field) value.Value) string {
	var b strings.Builder
	for _, c := range changes {
		if c.Op == format.CreateTable {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%v %s", c.Op, c.Key)
		for _, f := range c.Fields {
			b.WriteString(" " + f.Name + "=")
			switch v := held(c.Key, f); v.Kind() {
			case value.KindText, value.KindBytes, value.KindVector:
				fmt.Fprintf(&b, "%v:%d:%08x", v.Kind(), len(v.Raw()), crc32.Checksum([]byte(v.Raw()), castagnoli))
			default:
				b.WriteString(v.String())
			}
		}
	}
	return b.String()
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// readerTarget is a reader's Target, with the real store in it, as the public
// package's copy: each batch the log reads goes into the copy, straight in
// while the log opens the file and from a Reset until Loaded, and through a
// transaction of its own otherwise (glue, in reload_test.go). Then it tells
// apply what the copy holds of the commit: each put's fields, read back from
// the copy, so a batch that went in torn, or in part, reads as another
// commit.
type readerTarget struct {
	g     *glue
	apply func(seq uint64, commit string)
	reset func()
}

func newReaderTarget(apply func(uint64, string), reset func()) *readerTarget {
	return &readerTarget{g: &glue{s: store.New(), opening: true}, apply: apply, reset: reset}
}

func (t *readerTarget) Apply(seq uint64, changes []format.Change) error {
	if err := t.g.Apply(seq, changes); err != nil {
		return err
	}
	t.apply(seq, describeAs(changes, func(key string, f format.Field) value.Value {
		rec, err := t.g.s.Get(key)
		if err != nil {
			return value.Text("not in the copy: " + err.Error())
		}
		tbl, _ := t.g.s.Table(key[:strings.IndexByte(key, ':')])
		return rec.Field(tbl.Find(f.Name))
	}))
	return nil
}

func (t *readerTarget) Reset() {
	t.g.Reset()
	t.reset()
}

func (t *readerTarget) Loaded(err error) { t.g.Loaded(err) }

// opened tells t the log's Open has returned, so later batches go in through
// a transaction each.
func (t *readerTarget) opened() { t.g.opening = false }

// recorder is what Final hands each commit to: it keeps every commit, in
// order.
type recorder struct {
	commits []string
	err     error
}

func (r *recorder) apply(seq uint64, commit string) {
	if seq != uint64(len(r.commits))+1 && r.err == nil {
		r.err = errors.New("the log handed Final batch " + strconv.FormatUint(seq, 10) + " out of order")
	}
	r.commits = append(r.commits, commit)
}

func (r *recorder) reset() { r.commits, r.err = nil, nil }

// TestTheRealLogWithProcessesKilled is the harness's run on the real log:
// two writers committing through it and two readers opening it afresh,
// each killed with SIGKILL at a random moment in a life of 10 to 200
// milliseconds and a new one started in its place, for 2 seconds, or half
// a second with -short. Every reader sees every commit once and in order,
// the readers agree with each other and with the file at the end, every
// commit a writer saw succeed is in the file, and every commit under way
// when its writer was killed is there whole or not at all.
func TestTheRealLogWithProcessesKilled(t *testing.T) {
	rep, err := Run(realLog, Options{Path: filepath.Join(t.TempDir(), "db.hcx")})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
}
