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
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The real log, beta/internal/logfile, under the harness. Writers commit
// through it, as the public package will: Lock, Append, then Unlock, with
// a Target that drops what Lock reads on. Readers open the file afresh for
// every read, as the log's API allows until F6 adds following, so every
// read goes from the first commit, and Open's try at the lock checks the
// end of the log whenever no writer holds it. Final takes the lock, as the
// next writer would, so its check marks a batch that a killed writer left
// synced and unmarked, and cuts off whatever else a killed writer left.
//
// A commit puts one to three records of its writer's own, each with a
// field of 1 to 300 letters, and what it holds, as the harness compares
// it, names each change and the length and CRC32C of each value. So a
// batch read back with a change missing, or with a value torn, holds
// another string.
var realLog = Workload{Name: "real log", Write: realWrite, Read: realRead, Final: realFinal}

// realWrite is a writer's work on the real log: commits, one after
// another, until the process is killed, with a short wait between them so
// the writers take turns at the lock.
func realWrite(w *Writer) error {
	l, err := logfile.Open(fsys.OS{}, w.Path(), discard{}, logfile.Options{})
	if err != nil {
		return err
	}
	defer l.Close()
	for n := 1; ; n++ {
		changes := realChanges(w, n)
		w.Begin(describe(changes))
		if err := l.Lock(); errors.Is(err, errs.ErrLockTimeout) {
			w.Failed(err) // nothing was written, and the next commit tries again
			continue
		} else if err != nil {
			return err
		}
		if err := l.Append(changes); err != nil {
			l.Unlock()
			return err
		}
		w.Done()
		if err := l.Unlock(); err != nil {
			return err
		}
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
		l, err := logfile.Open(fsys.OS{}, r.Path(), readerTarget{r}, logfile.Options{})
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
	var rec recorder
	l, err := logfile.Open(fsys.OS{}, path, &rec, logfile.Options{Wait: 20 * time.Second})
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

// describe is what a commit holds, as the harness compares it: each
// change's op and key, and each field's name and value, with text, bytes
// and vectors given by their kind, length and CRC32C. A writer describes
// its commit's changes before it commits them, and a reader the changes it
// reads back.
func describe(changes []format.Change) string {
	var b strings.Builder
	for i, c := range changes {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%v %s", c.Op, c.Key)
		for _, f := range c.Fields {
			b.WriteString(" " + f.Name + "=")
			switch v := f.Value; v.Kind() {
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

// discard is a writer's Target: a writer reads on when it takes the lock,
// and keeps nothing of what it reads.
type discard struct{}

func (discard) Apply(uint64, []format.Change) error { return nil }
func (discard) Reset()                              {}

// readerTarget is a reader's Target: it hands each batch the log reads to
// the Reader, as a commit.
type readerTarget struct{ r *Reader }

func (t readerTarget) Apply(seq uint64, changes []format.Change) error {
	t.r.Apply(seq, describe(changes))
	return nil
}

func (t readerTarget) Reset() { t.r.Reset() }

// recorder is Final's Target: it keeps every commit, in order.
type recorder struct {
	commits []string
	err     error
}

func (r *recorder) Apply(seq uint64, changes []format.Change) error {
	if seq != uint64(len(r.commits))+1 && r.err == nil {
		r.err = errors.New("the log handed Final batch " + strconv.FormatUint(seq, 10) + " out of order")
	}
	r.commits = append(r.commits, describe(changes))
	return nil
}

func (r *recorder) Reset() { r.commits, r.err = nil, nil }

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
