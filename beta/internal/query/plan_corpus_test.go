// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// Every SELECT of the corpus through the planner (Q5), on the fixture
// loaded into a real store, gives 0.x's answer.

// planQuery runs a SELECT through Query, with the moment now, and gives its
// answer as the corpus has it: the columns' names, the rows, or the
// error's kind. Writes and statements that don't parse give the parser's
// answer.
func planQuery(r store.Reader, q string, args []value.Value, now time.Time) *sqlcorpus.Answer {
	failed := func(err error) *sqlcorpus.Answer {
		return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
	}
	st, err := Parse(q)
	if err != nil {
		return failed(err)
	}
	sel, ok := st.(*Select)
	if !ok {
		return failed(fmt.Errorf("%s is no SELECT", q))
	}
	cols, rows, err := Query(r, sel, &Frame{Args: args, Now: now})
	if err != nil {
		return failed(err)
	}
	defer rows.Close()
	ans := &sqlcorpus.Answer{Columns: cols}
	for rows.Next() {
		row := make([]difftest.Value, len(rows.Row()))
		for i, v := range rows.Row() {
			row[i] = difftest.Value{V: goValue(v)}
		}
		ans.Rows = append(ans.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return failed(err)
	}
	return ans
}

// laterStage are the statements marked out that parse, which the planner
// refuses as it finds their names.
var laterStage = map[string]bool{"statement-0169": true, "statement-0263": true, "statement-0264": true, "statement-0265": true}

// TestTheCorpusSelectsGive0xsAnswers runs every SELECT of the corpus
// through Query, and each marked in gives 0.x's answer exactly: its
// columns' names, its rows in order, and its error's kind, the errors the
// parser finds included. Each marked out is refused: by the parser, or, for
// the four that parse, by the planner as it finds their names.
func TestTheCorpusSelectsGive0xsAnswers(t *testing.T) {
	s := fixtureStore(t)
	cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", "statements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ran, parsed, refused, later := 0, 0, 0, 0
	byShape := map[Shape]int{}
	for _, cs := range cases {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(cs.SQL)), "SELECT") {
			continue
		}
		got := planQuery(s, cs.SQL, argValues(cs.Args), corpusNow)
		st, perr := Parse(cs.SQL)
		if !cs.In {
			refused++
			if perr == nil {
				later++
				if !laterStage[cs.ID] {
					t.Errorf("%s: %s is marked out (%s) and parses, and only the four of laterStage should", cs.ID, cs.SQL, cs.Note)
				}
			}
			if got.Error != "error" {
				t.Errorf("%s: %s is marked out (%s), and the planner gives %+v", cs.ID, cs.SQL, cs.Note, got)
			}
			continue
		}
		ran++
		if perr == nil {
			parsed++
			if p, err := Prepare(s, st.(*Select)); err == nil {
				byShape[p.Shape()]++
			}
		}
		if !sqlcorpus.Same(cs.Answer, got, cs.Close) {
			t.Errorf("%s: %s %v\n  want %+v\n  got  %+v", cs.ID, cs.SQL, cs.Args, cs.Answer, got)
		}
	}
	t.Logf("%d SELECTs marked in give 0.x's answers, %d of them through a plan, by shape %v; %d marked out are refused, %d of them by the planner",
		ran, parsed, byShape, refused, later)
	if ran != 205 || parsed != 198 || later != len(laterStage) {
		t.Errorf("ran %d of the corpus's SELECTs, %d through a plan, and the planner refused %d; want 205, 198 and %d", ran, parsed, later, len(laterStage))
	}
}

// corpusNow is the moment the corpus's statements run at. None of them reads
// it, since the cases on 'now' are in dates.jsonl.
var corpusNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// TestTheCorpusExpressionsAndDatesGoThroughThePlanner runs the corpus's
// other SELECTs through Query, on the fixture: each of the 3,000 expressions
// gives 0.x's answer, as it does through the evaluator alone; each date on a
// fixed date, all marked out, is refused; and each case on 'now', which has
// no answer of 0.x's since that changes with the day, gives at a set moment
// what runAt gives, which TestTheNowCasesAreTaken checks by hand.
func TestTheCorpusExpressionsAndDatesGoThroughThePlanner(t *testing.T) {
	s := fixtureStore(t)
	load := func(name string) []sqlcorpus.Case {
		cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return cases
	}
	counts := map[string]int{}
	for _, cs := range load("expressions.jsonl") {
		counts["expressions"]++
		if got := planQuery(s, cs.SQL, argValues(cs.Args), corpusNow); !sqlcorpus.Same(cs.Answer, got, cs.Close) {
			t.Errorf("%s: %s %v\n  want %+v\n  got  %+v", cs.ID, cs.SQL, cs.Args, cs.Answer, got)
		}
	}
	now := at(2026, 10, 8, 9, 5, 7, 250)
	people := []fakeRow{
		{"key": value.Text("people:1"), "joined": value.Text("2026-01-15")},
		{"key": value.Text("people:2"), "joined": value.Text("2025-12-31")},
		{"key": value.Text("people:3"), "joined": value.Text("2024-02-29")},
		{"key": value.Text("people:4"), "joined": value.Text("2026-10-07")},
	}
	for _, cs := range load("dates.jsonl") {
		got := planQuery(s, cs.SQL, nil, now)
		switch {
		case cs.In:
			counts["dates on 'now'"]++
			if want := runAt(t, cs.SQL, now, people); want.Error != "" || !sqlcorpus.Same(want, got, false) {
				t.Errorf("%s: %s\n  want %+v\n  got  %+v", cs.ID, cs.SQL, want, got)
			}
		default:
			counts["dates refused"]++
			if got.Error != "error" {
				t.Errorf("%s: %s is marked out (%s), and the planner gives %+v", cs.ID, cs.SQL, cs.Note, got)
			}
		}
	}
	t.Logf("%v", counts)
	if counts["expressions"] != 3000 || counts["dates on 'now'"] != 14 || counts["dates refused"] != 608 {
		t.Errorf("%v; the corpus has 3,000 expressions, 14 dates on 'now' and 608 on a fixed date", counts)
	}
}
