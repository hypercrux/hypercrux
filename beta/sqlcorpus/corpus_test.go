// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sqlcorpus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
)

// The corpus is kept in three files, one for each kind of case, with the
// date and "now" cases together.
var files = map[string][]string{
	"statements.jsonl":  {"statement"},
	"expressions.jsonl": {"expression"},
	"dates.jsonl":       {"date", "now"},
}

func loadAll(t *testing.T) []Case {
	t.Helper()
	var all []Case
	for _, name := range []string{"statements.jsonl", "expressions.jsonl", "dates.jsonl"} {
		cases, err := Load(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("%v (record the corpus with HYPERCRUX_CORPUS_RECORD=1)", err)
		}
		all = append(all, cases...)
	}
	return all
}

// TestRecord writes the corpus, with 0.x's answers, when
// HYPERCRUX_CORPUS_RECORD is set:
//
//	HYPERCRUX_CORPUS_RECORD=1 go test -run TestRecord ./beta/sqlcorpus
func TestRecord(t *testing.T) {
	if os.Getenv("HYPERCRUX_CORPUS_RECORD") == "" {
		t.Skip("set HYPERCRUX_CORPUS_RECORD=1 to record the corpus through 0.x")
	}
	e := zerox.Engine{}
	db, err := OpenFixture(e, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := Build()
	for i := range cases {
		if cases[i].Kind != "now" {
			cases[i].Answer = Run(e, db, cases[i], false)
		}
	}
	for name, kinds := range files {
		var part []Case
		for _, cs := range cases {
			for _, k := range kinds {
				if cs.Kind == k {
					part = append(part, cs)
				}
			}
		}
		if err := Save(filepath.Join("testdata", name), part); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d cases", name, len(part))
	}
}

// TestCorpusMatchesItsSource holds the saved corpus to the code that makes
// it, so a change to the cases can't go unrecorded.
func TestCorpusMatchesItsSource(t *testing.T) {
	saved, built := loadAll(t), Build()
	if len(saved) != len(built) {
		t.Fatalf("the corpus has %d cases and the code makes %d; record it again", len(saved), len(built))
	}
	for i := range built {
		a, b := saved[i], built[i]
		a.Answer = nil
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Fatalf("case %d differs from the code that makes it; record the corpus again\nsaved %s\nbuilt %s", i, ja, jb)
		}
	}
}

// TestReplayTwice runs the whole corpus on 0.x twice, each time on a new
// database, and both runs must give the recorded answers.
func TestReplayTwice(t *testing.T) {
	cases := loadAll(t)
	for pass := 1; pass <= 2; pass++ {
		diffs, err := Replay(zerox.Engine{}, zerox.Engine{}, cases, t.TempDir(), false)
		if err != nil {
			t.Fatal(err)
		}
		for i, d := range diffs {
			if i == 10 {
				t.Errorf("and %d more", len(diffs)-10)
				break
			}
			t.Errorf("pass %d: %v", pass, d)
		}
		if len(diffs) > 0 {
			t.FailNow()
		}
	}
	t.Logf("%d cases replayed clean on 0.x twice", len(cases))
}

// TestTheMarksFollowTheSpec checks the corpus is marked as beta/SQL.md says:
// every statement outside the subset says why in its note, every expression
// and every case on 'now' is in, and every date case on a fixed date is out.
func TestTheMarksFollowTheSpec(t *testing.T) {
	for _, cs := range loadAll(t) {
		switch {
		case cs.Kind == "statement" && !cs.In && cs.Note == "":
			t.Errorf("%s is outside the subset, and its note doesn't say why: %s", cs.ID, cs.SQL)
		case (cs.Kind == "expression" || cs.Kind == "now") && !cs.In:
			t.Errorf("%s is marked out, and beta/SQL.md has every %s case in: %s", cs.ID, cs.Kind, cs.SQL)
		case cs.Kind == "date" && cs.In:
			t.Errorf("%s is marked in, and beta/SQL.md has dates on 'now' only: %s", cs.ID, cs.SQL)
		}
	}
}

// TestTheCorpusIsBroad checks the corpus is the size the plan asks for and
// that its answers cover every kind of value and error.
func TestTheCorpusIsBroad(t *testing.T) {
	count := map[string]int{}
	ins, outs := 0, 0
	types := map[string]int{}
	for _, cs := range loadAll(t) {
		count[cs.Kind]++
		if cs.Kind == "statement" {
			if cs.In {
				ins++
			} else {
				outs++
			}
		}
		if cs.Answer == nil {
			continue
		}
		if cs.Answer.Error != "" {
			types["error: "+cs.Answer.Error]++
			continue
		}
		for _, row := range cs.Answer.Rows {
			for _, val := range row {
				b, _ := json.Marshal(val)
				kind, _, _ := strings.Cut(strings.TrimPrefix(string(b), `{"`), `"`)
				types[kind]++
			}
		}
	}
	t.Logf("cases: %v; statements in %d, out %d; answers: %v", count, ins, outs, types)
	for kind, least := range map[string]int{"statement": 250, "expression": 2000, "date": 300, "now": 10} {
		if count[kind] < least {
			t.Errorf("%d %s cases, fewer than %d", count[kind], kind, least)
		}
	}
	for _, kind := range []string{"null", "int", "real", "text", "bytes", "error: error", "error: invalid"} {
		if types[kind] == 0 {
			t.Errorf("no answer holds %s", kind)
		}
	}
}
