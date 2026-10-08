// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// exited is what a test's stand-in for os.Exit panics with, so the code
// after the call never runs, as it wouldn't in a child.
type exited int

// testReporter is a reporter that writes to a buffer, and whose exit
// panics.
func testReporter() (*reporter, *bytes.Buffer) {
	var out bytes.Buffer
	return &reporter{w: &out, exit: func(code int) { panic(exited(code)) }}, &out
}

// ends runs f, and returns the code it would have ended the process with,
// or -1 when it returned.
func ends(f func()) (code int) {
	defer func() {
		if e := recover(); e != nil {
			c, ok := e.(exited)
			if !ok {
				panic(e)
			}
			code = int(c)
		}
	}()
	f()
	return -1
}

// sent returns the reports in out, a line each, without their newlines.
func sent(t *testing.T, out *bytes.Buffer) []string {
	t.Helper()
	var got []string
	err := lines(bytes.NewReader(out.Bytes()), func(s string) { got = append(got, s) }, func() { t.Error("a report cut short") })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestReportsReadBack: every kind of report reads back as it was sent,
// with strings that hold spaces, quotes, newlines, zero bytes, bytes that
// aren't UTF-8, and nothing at all, and every report is one line.
func TestReportsReadBack(t *testing.T) {
	rep, out := testReporter()
	texts := []string{"", "w1.1 abc", "spaces, \"quotes\", \\ and\nnewlines\r\n", "\x00\x00\x00", "\xff\xfe isn't UTF-8", "é ☺ 中"}
	var want []report
	rep.send(kindStart, "4242")
	want = append(want, report{kind: kindStart, pid: 4242})
	rep.send(kindEra, "3")
	want = append(want, report{kind: kindEra, era: 3})
	for i, text := range texts {
		rep.send(kindBegin, strconv.Quote(text))
		rep.send(kindDone)
		rep.send(kindFailed, strconv.Quote(text))
		rep.send(kindReset)
		rep.send(kindSaw, strconv.Itoa(i+1), strconv.Quote(text))
		rep.send(kindProblem, strconv.Itoa(int(Lost)), strconv.Quote(text))
		rep.send(kindFail, strconv.Quote(text))
		rep.send(kindEnd)
		rep.send(kindCount, strconv.Quote(text))
		want = append(want, report{kind: kindBegin, text: text}, report{kind: kindDone}, report{kind: kindFailed, text: text}, report{kind: kindReset},
			report{kind: kindSaw, seq: uint64(i + 1), text: text}, report{kind: kindProblem, problem: Lost, text: text}, report{kind: kindFail, text: text},
			report{kind: kindEnd}, report{kind: kindCount, text: text})
	}
	if n := strings.Count(out.String(), "\n"); n != len(want) {
		t.Fatalf("%d reports went as %d lines", len(want), n)
	}
	got := sent(t, out)
	for i, line := range got {
		rp, err := parseReport(line)
		if err != nil || rp != want[i] {
			t.Errorf("%q reads back as %+v, with %v, where %+v was sent", line, rp, err, want[i])
		}
	}
}

// TestAReportCutShortIsDropped: bytes after the last newline are a report
// a kill cut short, and they're dropped, however much of it there is.
func TestAReportCutShortIsDropped(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
		cuts int
	}{
		{"start 1\nbegin \"a\"\n", []string{"start 1", `begin "a"`}, 0},
		{"start 1\nbegin \"a\"\nsaw 1 \"a", []string{"start 1", `begin "a"`}, 1},
		{"start 1\nb", []string{"start 1"}, 1},
		{"begin \"a b c\"", nil, 1},
		{"", nil, 0},
	} {
		var got []string
		cuts := 0
		if err := lines(strings.NewReader(c.in), func(s string) { got = append(got, s) }, func() { cuts++ }); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") || cuts != c.cuts {
			t.Errorf("%q reads as %q with %d cut short, where %q with %d is wanted", c.in, got, cuts, c.want, c.cuts)
		}
	}
}

// TestReportsThatDontReadBack: a line that isn't a report as a child
// writes one is refused.
func TestReportsThatDontReadBack(t *testing.T) {
	for _, line := range []string{
		"", "start", "start x", "begin a", `begin "a`, "done now", "reset 1", "end 0", "saw", "saw 1", `saw 0 "a"`, `saw x "a"`, "saw 1 a",
		`problem 0 "a"`, `problem 10 "a"`, `problem x "a"`, `fail`, `finish "a"`, `Begin "a"`, "count", "count a", `count "a`,
		"era", "era 0", "era -1", "era x", "era 1 2",
	} {
		if rp, err := parseReport(line); err == nil {
			t.Errorf("%q reads as %+v", line, rp)
		}
	}
}

// TestEachReportIsOneWrite: the reporter writes each report with one
// write call, so a pipe takes a report of up to PIPE_BUF bytes whole or
// not at all.
func TestEachReportIsOneWrite(t *testing.T) {
	var writes []string
	rep := &reporter{w: writerFunc(func(b []byte) (int, error) { writes = append(writes, string(b)); return len(b), nil })}
	rep.send(kindBegin, strconv.Quote(strings.Repeat("x", 3000)))
	rep.send(kindSaw, "12", strconv.Quote("a b"))
	rep.send(kindDone)
	if len(writes) != 3 || writes[1] != "saw 12 \"a b\"\n" || writes[2] != "done\n" || !strings.HasSuffix(writes[0], "x\"\n") {
		t.Errorf("three reports went in %d writes: %q", len(writes), writes)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
