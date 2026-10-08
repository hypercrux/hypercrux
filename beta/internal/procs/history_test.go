// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"strings"
	"testing"
)

// step is one report, or one process's end, given to a history.
type step func(h *history) *Failure

func begin(w int, text string) step { return func(h *history) *Failure { return h.begin(w, text) } }
func succeed(w int) step            { return func(h *history) *Failure { return h.ended(w, done) } }
func fail(w int) step               { return func(h *history) *Failure { return h.ended(w, failed) } }
func killed(id int) step            { return func(h *history) *Failure { h.gone(id, true); return nil } }
func saw(r int, seq uint64, text string) step {
	return func(h *history) *Failure { return h.saw(r, seq, text) }
}

// replay gives h the steps in turn, and then checks it against final. It
// returns the first failure.
func replay(h *history, steps []step, final []string) *Failure {
	for _, s := range steps {
		if f := s(h); f != nil {
			return f
		}
	}
	return h.check(final)
}

// TestTheChecksFindEachProblem: a history made of reports by hand finds
// each problem the harness checks for, and nothing in a run that's right,
// with commits under way and failed ones in the file and out of it.
func TestTheChecksFindEachProblem(t *testing.T) {
	for _, c := range []struct {
		name  string
		steps []step
		final []string
		want  Problem
	}{
		{"a run that's right", []step{
			begin(1, "a"), succeed(1), begin(2, "b"), succeed(2), begin(1, "c"), killed(1), begin(2, "d"), fail(2), begin(2, "e"), killed(2),
			saw(3, 1, "a"), saw(4, 1, "a"), saw(3, 2, "b"), saw(5, 1, "a"), saw(5, 2, "b"), saw(5, 3, "c"),
		}, []string{"a", "b", "c", "e"}, 0},
		{"nothing but commits under way and failed, none in the file", []step{begin(1, "a"), killed(1), begin(2, "b"), fail(2)}, nil, 0},
		{"a reader finds a commit no writer began", []step{begin(1, "a"), succeed(1), saw(3, 1, "a, torn")}, []string{"a"}, NotBegun},
		{"the file holds a commit no writer began", []step{begin(1, "a"), killed(1)}, []string{"a, torn"}, NotBegun},
		{"two readers disagree", []step{begin(1, "a"), begin(2, "b"), saw(3, 1, "a"), saw(4, 1, "b")}, nil, Disagree},
		{"a reader and the file disagree", []step{begin(1, "a"), killed(1), begin(2, "b"), killed(2), saw(3, 1, "a")}, []string{"b"}, Disagree},
		{"a commit a reader saw isn't in the file", []step{begin(1, "a"), succeed(1), begin(1, "b"), killed(1), saw(3, 1, "a"), saw(3, 2, "b")}, []string{"a"}, Lost},
		{"the file holds a commit twice", []step{begin(1, "a"), succeed(1)}, []string{"a", "a"}, Twice},
		{"a commit that succeeded isn't in the file", []step{begin(1, "a"), succeed(1), begin(1, "b"), succeed(1)}, []string{"a"}, Missing},
		{"a writer's commits in the file out of order", []step{begin(1, "a"), succeed(1), begin(1, "b"), succeed(1)}, []string{"b", "a"}, OutOfOrder},
		{"a reader's reports out of order", []step{begin(1, "a"), begin(2, "b"), saw(3, 2, "b")}, nil, OutOfOrder},
		{"a reader's report of a commit again", []step{begin(1, "a"), saw(3, 1, "a"), saw(3, 1, "a")}, nil, OutOfOrder},
		{"two commits that hold the same", []step{begin(1, "a"), succeed(1), begin(2, "a")}, nil, Failed},
		{"a commit begun with one under way", []step{begin(1, "a"), begin(1, "b")}, nil, Failed},
		{"a commit ended with none under way", []step{succeed(1)}, nil, Failed},
	} {
		f := replay(newHistory(), c.steps, c.final)
		switch {
		case c.want == 0 && f != nil:
			t.Errorf("%s: %v: %s", c.name, f.Problem, f.Reason)
		case c.want != 0 && f == nil:
			t.Errorf("%s: nothing found, where %v is wanted", c.name, c.want)
		case c.want != 0 && f.Problem != c.want:
			t.Errorf("%s: %v found, where %v is wanted: %s", c.name, f.Problem, c.want, f.Reason)
		case f != nil:
			t.Logf("%s: %v: %s", c.name, f.Problem, f.Reason)
		}
	}
}

// TestTheHistoryCounts: the report counts the commits by how they ended,
// and the run has done enough once 10 commits have succeeded, 3 writers
// have been killed with a commit under way and 2 readers once they had
// seen a commit.
func TestTheHistoryCounts(t *testing.T) {
	h := newHistory()
	var steps []step
	var final []string
	for i := range 10 {
		text := "c" + string(rune('a'+i))
		steps = append(steps, begin(1, text), succeed(1), saw(20, uint64(i+1), text))
		final = append(final, text)
	}
	steps = append(steps, begin(2, "under way, in the file"), killed(2), begin(3, "under way"), killed(3), begin(4, "failed"), fail(4))
	final = append(final, "under way, in the file")
	steps = append(steps, saw(21, 1, "ca"), killed(21), saw(22, 1, "ca"), killed(22))
	if f := replay(h, steps, final); f != nil {
		t.Fatal(f.Reason)
	}
	if h.enough() {
		t.Errorf("the run has done enough with 2 writers killed with a commit under way: %s", h.lacks())
	}
	if replay(h, []step{begin(5, "the third under way"), killed(5)}, final) != nil || !h.enough() {
		t.Errorf("the run hasn't done enough: %s", h.lacks())
	}
	var rep Report
	h.count(&rep, final)
	if rep.Begun != 14 || rep.Done != 10 || rep.Errors != 1 || rep.UnderWay != 3 || rep.UnderWayIn != 1 || rep.Commits != 11 || rep.Seen != 12 || rep.Counts != nil {
		t.Errorf("the counts are %+v", rep)
	}

	// The workload's own counts, and the least of them a run wants.
	h.least = map[string]int{"checks": 2, "cuts": 0}
	h.tally("checks")
	if h.enough() || !strings.Contains(h.lacks(), `counted "checks" 1 time, short of the 2 wanted`) || strings.Contains(h.lacks(), "cuts") {
		t.Errorf("with 1 of the 2 checks wanted, the run has done enough: %v: %s", h.enough(), h.lacks())
	}
	h.tally("checks")
	h.tally("other")
	if !h.enough() {
		t.Errorf("with the 2 checks wanted, the run hasn't done enough: %s", h.lacks())
	}
	h.count(&rep, final)
	if len(rep.Counts) != 2 || rep.Counts["checks"] != 2 || rep.Counts["other"] != 1 || !strings.Contains(rep.String(), "counted checks 2, other 1") {
		t.Errorf("the report counts %v: %v", rep.Counts, rep)
	}
}
