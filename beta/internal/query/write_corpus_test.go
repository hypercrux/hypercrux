// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The closing test of Q6: every write of the corpus marked in, through
// Exec on the fixture loaded into a real store, gives 0.x's answer, its
// count of changed rows or its error's kind, and the query the corpus runs
// after it, inside the same transaction, gives 0.x's rows. Each write
// marked out is refused.

// writeKind is difftest.Kind's for the errors of a write: "invalid" for an
// error that wraps errs.ErrInvalid, "not found" for one that wraps
// errs.ErrNotFound, which SQL never gives, and "error" for the *Error of
// the parser, the planner, the evaluator and the constraints. Anything else
// is unexpected.
func writeKind(err error) string {
	var pe *Error
	switch {
	case errors.Is(err, errs.ErrInvalid):
		return "invalid"
	case errors.Is(err, errs.ErrNotFound):
		return "not found"
	case errors.As(err, &pe):
		return "error"
	}
	return "unexpected: " + err.Error()
}

// execWrite parses q and runs it through Exec inside tx, with the moment
// now, and gives its answer as the corpus has it: the count of changed rows,
// or the error's kind. A SELECT, or anything that doesn't parse, gives the
// error Parse or Exec gives.
func execWrite(tx *store.Tx, q string, args []value.Value, now time.Time) *sqlcorpus.Answer {
	failed := func(err error) *sqlcorpus.Answer {
		return &sqlcorpus.Answer{Error: writeKind(err), Message: err.Error()}
	}
	st, err := Parse(q)
	if err != nil {
		return failed(err)
	}
	n, err := Exec(tx, st, &Frame{Args: args, Now: now})
	if err != nil {
		return failed(err)
	}
	return &sqlcorpus.Answer{Changed: &n}
}

// runWriteCase runs one of the corpus's writes on s as sqlcorpus.Run runs
// it on an engine: inside a transaction that's then rolled back, with the
// case's query after it when the write succeeds.
func runWriteCase(t testing.TB, s *store.Store, cs sqlcorpus.Case) *sqlcorpus.Answer {
	t.Helper()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ans := execWrite(tx, cs.SQL, argValues(cs.Args), corpusNow)
	if ans.Error == "" && cs.After != "" {
		ans.After = planQuery(tx, cs.After, nil, corpusNow)
	}
	return ans
}

// isWrite tells the corpus's INSERT, UPDATE, DELETE and REPLACE statements.
func isWrite(q string) bool {
	s := strings.ToUpper(strings.TrimSpace(q))
	for _, w := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE"} {
		if strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

func TestTheCorpusWritesGive0xsAnswers(t *testing.T) {
	s := fixtureStore(t)
	cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", "statements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	in, after, failed, out := 0, 0, 0, 0
	for _, cs := range cases {
		if !isWrite(cs.SQL) {
			continue
		}
		got := runWriteCase(t, s, cs)
		if !cs.In {
			out++
			if _, perr := Parse(cs.SQL); perr == nil || got.Error != "error" {
				t.Errorf("%s: %s is marked out (%s), and gives %+v", cs.ID, cs.SQL, cs.Note, got)
			}
			continue
		}
		in++
		if cs.Answer.After != nil {
			after++
		}
		if cs.Answer.Error != "" {
			failed++
		}
		if !sqlcorpus.Same(cs.Answer, got, cs.Close) {
			t.Errorf("%s: %s %v\n  want %s\n  got  %s", cs.ID, cs.SQL, cs.Args, writeAnswerText(cs.Answer), writeAnswerText(got))
		}
	}
	t.Logf("%d writes marked in give 0.x's answers, %d of them errors and %d with the query after; %d marked out are refused", in, failed, after, out)
	if in != 29 || after != 18 || out != 7 {
		t.Errorf("ran %d writes marked in, %d with a query after, and %d marked out; the corpus has 29, 18 and 7", in, after, out)
	}
	// Every transaction above was rolled back, so the store holds the
	// fixture as it was.
	if got, want := storeText(t, s), storeText(t, fixtureStore(t)); got != want {
		t.Errorf("the store after the writes and their rollbacks:\n%s\nthe fixture:\n%s", got, want)
	}
}

// writeAnswerText shows an answer with its count, its error and the
// answer of the query after it, for a test's message.
func writeAnswerText(a *sqlcorpus.Answer) string {
	if a == nil {
		return "nil"
	}
	var b strings.Builder
	switch {
	case a.Error != "":
		b.WriteString(a.Error + " (" + a.Message + ")")
	case a.Changed != nil:
		b.WriteString("changed " + value.Int(*a.Changed).String())
	default:
		b.WriteString(answerText(a))
	}
	if a.After != nil {
		b.WriteString("; after: " + strings.ReplaceAll(writeAnswerText(a.After), "\n", " / "))
	}
	return b.String()
}
