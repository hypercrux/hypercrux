// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// How a child reports to its parent. Each report is one line: a kind, then
// its fields, separated by spaces, then a newline. A string in a field is
// quoted as Go quotes it, so a report holds no newline but its last byte.
//
// A child writes each report with one write call, and the parent counts a
// line only once its newline has come. So a kill that cuts a write short
// leaves bytes without a newline at the end of the pipe, and the parent
// drops them: half a report never reads as a whole one. Linux writes up to
// PIPE_BUF bytes, 4,096, to a pipe in one go, all or none, so most reports
// can't be cut at all. A longer one can, and is dropped. That costs the
// checks nothing they rely on. A writer reports a commit before any of it
// can reach the file, so a commit whose report was cut never reached it.
// When a report that a commit succeeded, or that a reader saw one, is cut,
// the check of that commit is only weaker: it has to be in the file whole
// or not at all, where it would have had to be there.

// The kinds of report.
const (
	kindStart   = "start"   // the child is starting its work: its process ID
	kindBegin   = "begin"   // a writer's commit is starting: what it holds
	kindDone    = "done"    // the writer's commit under way has succeeded
	kindFailed  = "failed"  // the writer's commit under way has failed: the error
	kindReset   = "reset"   // a reader is starting again from the first commit
	kindSaw     = "saw"     // a reader has seen a commit for the first time: its sequence number and what it holds
	kindProblem = "problem" // the harness has found a problem in the child: the problem's number and the reason
	kindFail    = "fail"    // the child's work has failed: the error
	kindEnd     = "end"     // the child's work has returned nil, and the child is ending
)

// reporter writes a child's reports. Its methods are safe to call from
// several goroutines at once.
type reporter struct {
	mu   sync.Mutex
	w    io.Writer
	buf  []byte
	exit func(code int) // os.Exit, or a test's stand-in
}

// send writes one report, with one write call.
func (r *reporter) send(kind string, fields ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := append(r.buf[:0], kind...)
	for _, f := range fields {
		b = append(append(b, ' '), f...)
	}
	b = append(b, '\n')
	r.buf = b
	if _, err := r.w.Write(b); err != nil {
		// The parent can't hear this process any more, so it has gone, or
		// it's ending the run.
		r.exit(exitOrphan)
	}
}

// problem reports a problem the harness has found in the child, and ends
// the process.
func (r *reporter) problem(p Problem, format string, args ...any) {
	r.send(kindProblem, strconv.Itoa(int(p)), strconv.Quote(fmt.Sprintf(format, args...)))
	r.exit(exitFailed)
}

// fail reports that the child's work has failed, or misused the harness,
// and ends the process.
func (r *reporter) fail(err error) {
	r.send(kindFail, strconv.Quote(err.Error()))
	r.exit(exitFailed)
}

// report is a report read back by the parent.
type report struct {
	kind    string
	pid     int     // a start's
	seq     uint64  // a saw's
	problem Problem // a problem's
	text    string  // what a commit holds, or an error, or a problem's reason
}

// parseReport reads back one line a child wrote, without its newline.
func parseReport(line string) (report, error) {
	kind, rest, _ := strings.Cut(line, " ")
	rp := report{kind: kind}
	var err error
	switch kind {
	case kindStart:
		rp.pid, err = strconv.Atoi(rest)
	case kindDone, kindReset, kindEnd:
		if rest != "" {
			err = errors.New("there's more after its kind")
		}
	case kindBegin, kindFailed, kindFail:
		rp.text, err = strconv.Unquote(rest)
	case kindSaw:
		seq, text, _ := strings.Cut(rest, " ")
		if rp.seq, err = strconv.ParseUint(seq, 10, 64); err == nil && rp.seq == 0 {
			err = errors.New("commit 0")
		}
		if err == nil {
			rp.text, err = strconv.Unquote(text)
		}
	case kindProblem:
		p, text, _ := strings.Cut(rest, " ")
		var n int
		if n, err = strconv.Atoi(p); err == nil && (n < int(NotBegun) || n > int(Idle)) {
			err = fmt.Errorf("no problem has the number %d", n)
		}
		rp.problem = Problem(n)
		if err == nil {
			rp.text, err = strconv.Unquote(text)
		}
	default:
		err = errors.New("no report is of that kind")
	}
	return rp, err
}

// lines reads newline-ended lines from rd until it ends, and hands each to
// line, without its newline. Bytes at the end without a newline are a
// report a kill cut short: they're dropped, and cut is called. An error
// reading rd, other than its end, is returned.
func lines(rd io.Reader, line func(string), cut func()) error {
	br := bufio.NewReader(rd)
	for {
		s, err := br.ReadString('\n')
		if err != nil {
			if s != "" {
				cut()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line(s[:len(s)-1])
	}
}

// short is s quoted for a message, with only its first 60 bytes when it's
// longer than that.
func short(s string) string {
	const most = 60
	if len(s) > most {
		return strconv.Quote(s[:most]) + "..."
	}
	return strconv.Quote(s)
}
