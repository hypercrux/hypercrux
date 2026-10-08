// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// TestAReaderReportsWhatItSeesFirst: a reader reports each commit the
// first time it sees it, and a read from the first commit that finds the
// same again reports nothing more than its start.
func TestAReaderReportsWhatItSeesFirst(t *testing.T) {
	rep, out := testReporter()
	r := &Reader{proc: proc{id: 7, rep: rep}, next: 1}
	r.Apply(1, "a")
	r.Apply(2, "b")
	r.Reset()
	r.Apply(1, "a")
	r.Apply(2, "b")
	r.Apply(3, "c")
	r.Reset()
	want := []string{`saw 1 "a"`, `saw 2 "b"`, "reset", `saw 3 "c"`, "reset"}
	if got := sent(t, out); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the reader reported %q", got)
	}
}

// TestAReaderChecksItsOwnSight: a reader handed a commit out of order, a
// commit that changed between two reads, or a read from the first commit
// that comes to an end short of what it had seen, reports the problem and
// ends its process.
func TestAReaderChecksItsOwnSight(t *testing.T) {
	for _, c := range []struct {
		name    string
		do      func(r *Reader)
		problem Problem
	}{
		{"a commit skipped", func(r *Reader) { r.Apply(1, "a"); r.Apply(3, "c") }, OutOfOrder},
		{"a commit twice", func(r *Reader) { r.Apply(1, "a"); r.Apply(1, "a") }, OutOfOrder},
		{"the first commit missed", func(r *Reader) { r.Apply(2, "b") }, OutOfOrder},
		{"a commit read again from the start skipped", func(r *Reader) { r.Apply(1, "a"); r.Apply(2, "b"); r.Reset(); r.Apply(2, "b") }, OutOfOrder},
		{"a commit changed", func(r *Reader) { r.Apply(1, "a"); r.Apply(2, "b"); r.Reset(); r.Apply(1, "a"); r.Apply(2, "x") }, Disagree},
		{"a commit lost", func(r *Reader) { r.Apply(1, "a"); r.Apply(2, "b"); r.Reset(); r.Apply(1, "a"); r.Reset() }, Lost},
		{"every commit lost", func(r *Reader) { r.Reset(); r.Apply(1, "a"); r.Reset(); r.Reset() }, Lost},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep, out := testReporter()
			r := &Reader{proc: proc{id: 7, rep: rep}, next: 1}
			if code := ends(func() { c.do(r) }); code != exitFailed {
				t.Fatalf("the process ended with %d, where the reader should have ended it with %d", code, exitFailed)
			}
			got := sent(t, out)
			rp, err := parseReport(got[len(got)-1])
			if err != nil || rp.kind != kindProblem || rp.problem != c.problem || !strings.Contains(rp.text, "reader 7") {
				t.Fatalf("the reader's last report was %q, with %v, where a problem %v is wanted", got[len(got)-1], err, c.problem)
			}
			t.Log(rp.text)
		})
	}
}

// TestAWriterChecksItsCalls: a writer's work begins one commit at a time,
// and ends each with Done or Failed, and the Writer refuses anything else.
func TestAWriterChecksItsCalls(t *testing.T) {
	rep, out := testReporter()
	w := &Writer{proc: proc{id: 3, rep: rep}}
	w.Begin("a")
	w.Done()
	w.Begin("b")
	w.Failed(errors.New("the lock timed out"))
	want := []string{`begin "a"`, "done", `begin "b"`, `failed "the lock timed out"`}
	if got := sent(t, out); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the writer reported %q", got)
	}
	for _, c := range []struct {
		name string
		do   func(w *Writer)
	}{
		{"Begin twice", func(w *Writer) { w.Begin("a"); w.Begin("b") }},
		{"Done first", func(w *Writer) { w.Done() }},
		{"Failed first", func(w *Writer) { w.Failed(errors.New("no")) }},
		{"Failed with nil", func(w *Writer) { w.Begin("a"); w.Failed(nil) }},
		{"Done twice", func(w *Writer) { w.Begin("a"); w.Done(); w.Done() }},
	} {
		rep, out := testReporter()
		w := &Writer{proc: proc{id: 3, rep: rep}}
		if code := ends(func() { c.do(w) }); code != exitFailed {
			t.Errorf("%s: the process ended with %d, where %d is wanted", c.name, code, exitFailed)
			continue
		}
		got := sent(t, out)
		if last := got[len(got)-1]; !strings.HasPrefix(last, kindFail+" ") || !strings.Contains(last, "procs: Writer.") {
			t.Errorf("%s: the last report was %q", c.name, last)
		}
	}
}

// TestAChildWithoutItsPipeEnds: a child whose reports can't be written
// ends its process, as its parent has gone.
func TestAChildWithoutItsPipeEnds(t *testing.T) {
	rep := &reporter{w: writerFunc(func([]byte) (int, error) { return 0, errors.New("broken pipe") }), exit: func(code int) { panic(exited(code)) }}
	if code := ends(func() { rep.send(kindDone) }); code != exitOrphan {
		t.Errorf("the process ended with %d, where %d is wanted", code, exitOrphan)
	}
	if code := ends(func() { rep.send(kindSaw, "1", strconv.Quote("a")) }); code != exitOrphan {
		t.Errorf("the process ended with %d, where %d is wanted", code, exitOrphan)
	}
}
