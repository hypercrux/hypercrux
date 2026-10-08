// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The closing test: every case in the corpus parses, or is refused, as its
// mark says, and every tree prints back to SQL that parses to the same
// tree.

func loadCorpus(t testing.TB) []sqlcorpus.Case {
	t.Helper()
	var all []sqlcorpus.Case
	for _, name := range []string{"statements.jsonl", "expressions.jsonl", "dates.jsonl"} {
		cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, cases...)
	}
	return all
}

// refusedHere are statements marked in whose answer in 0.x is an error the
// parser finds itself, before anything runs. Each gives an error of kind
// "error", as 0.x does.
var refusedHere = map[string]string{
	"statement-0205": "an INSERT without key, which fails NOT NULL on its first row",
	"statement-0281": "abs() with two arguments",
	"statement-0282": "a function neither SQLite nor the subset has",
}

// refusedLater are statements marked out that the parser takes, since only
// a later stage can see why they're out. The test holds each to parsing, so
// the list stays true.
var refusedLater = map[string]string{
	"statement-0169": "Q5, finding names: json_each's columns other than value",
	"statement-0263": "Q5, finding names: sqlite_schema is no record table",
	"statement-0264": "Q5, finding names: hc_links is no record table",
	"statement-0265": "Q5, finding names: records have no rowid",
}

// syntaxKind sorts the parse errors SQLite gives, which 0.x's messages in
// the corpus show, and the parser's own. It returns "" for anything else.
func syntaxKind(msg string) string {
	switch {
	case msg == "incomplete input":
		return "incomplete input"
	case strings.HasPrefix(msg, "near ") && strings.HasSuffix(msg, ": syntax error"):
		return "syntax error"
	case strings.HasPrefix(msg, "unrecognized token: "):
		return "unrecognized token"
	}
	return ""
}

// fixedDate reports whether s calls date() or datetime() on a first
// argument other than 'now', which Q2 refuses, and gives the call.
func fixedDate(s Statement) (string, bool) {
	sel, ok := s.(*Select)
	if !ok {
		return "", false
	}
	var found string
	for _, r := range sel.Results {
		walkExpr(r.Expr, func(e Expr) bool {
			if c, ok := e.(*Call); ok && (c.Func() == "date" || c.Func() == "datetime") {
				if l := c.Args[0].(*Literal); !strings.EqualFold(l.Data, "now") {
					found = c.String()
				}
			}
			return found == ""
		})
	}
	return found, found != ""
}

func TestTheCorpusParsesAsMarked(t *testing.T) {
	counts := map[string]int{}
	for _, cs := range loadCorpus(t) {
		s, err := Parse(cs.SQL)
		var pe *Error
		if err != nil && !errors.As(err, &pe) {
			t.Fatalf("%s: Parse gave %T %v, not an *Error", cs.ID, err, err)
		}
		recorded := ""
		if cs.Answer != nil {
			recorded = cs.Answer.Message
		}
		switch {
		case refusedHere[cs.ID] != "":
			counts["in, refused here"]++
			if err == nil || cs.Answer == nil || cs.Answer.Error != "error" {
				t.Errorf("%s: %s should be refused, as 0.x gives an error: got %v, 0.x %v", cs.ID, cs.SQL, err, cs.Answer)
			}
		case refusedLater[cs.ID] != "":
			counts["out, refused later"]++
			if cs.In || err != nil {
				t.Errorf("%s: %s is marked out for %s, and should parse: %v", cs.ID, cs.SQL, refusedLater[cs.ID], err)
			}
		case cs.Kind == "date" && err == nil:
			counts["out, a date Q2 refuses"]++
			if call, ok := fixedDate(s); !ok || cs.In {
				t.Errorf("%s: %s parses, and holds no date on anything but 'now' for Q2 to refuse", cs.ID, cs.SQL)
			} else if counts["out, a date Q2 refuses"] == 1 {
				t.Logf("%s, for example, parses, and Q2 refuses %s", cs.ID, call)
			}
		case cs.In && syntaxKind(recorded) != "":
			counts["in, a syntax error in 0.x too"]++
			if err == nil || syntaxKind(err.Error()) != syntaxKind(recorded) {
				t.Errorf("%s: %s gives %q in 0.x, and the parser gives %v", cs.ID, cs.SQL, recorded, err)
			}
		case cs.In:
			counts["in, parses"]++
			if err != nil {
				t.Errorf("%s: %s is marked in, and the parser refuses it: %v", cs.ID, cs.SQL, err)
			}
		default:
			counts["out, refused"]++
			if err == nil {
				t.Errorf("%s: %s is marked out (%s), and parses: %s", cs.ID, cs.SQL, cs.Note, dump(s))
			} else if syntaxKind(err.Error()) != "" {
				t.Errorf("%s: %s is refused with %q, which doesn't say what isn't taken", cs.ID, cs.SQL, err)
			}
		}
		if err != nil && (pe.Pos < 0 || pe.Pos > len(cs.SQL)) {
			t.Errorf("%s: the error's place %d is outside the text", cs.ID, pe.Pos)
		}
	}
	t.Logf("%v", counts)
}

// corpusSQL is every piece of SQL in the corpus: each case's statement, the
// 0.x form it's recorded from, and the query run after a write.
func corpusSQL(t testing.TB) []string {
	var all []string
	for _, cs := range loadCorpus(t) {
		all = append(all, cs.SQL)
		if cs.Via != "" {
			all = append(all, cs.Via)
		}
		if cs.After != "" {
			all = append(all, cs.After)
		}
	}
	return all
}

func TestTheCorpusFormsAndQueriesAfterParse(t *testing.T) {
	for _, cs := range loadCorpus(t) {
		for _, q := range []string{cs.Via, cs.After} {
			if q == "" {
				continue
			}
			if _, err := Parse(q); err != nil {
				t.Errorf("%s: %s: %v", cs.ID, q, err)
			}
		}
	}
}

// TestTreesPrintBack prints every tree in the corpus back as SQL, and
// parsing that again must give the same tree, which prints the same. So
// must the strict printing the tests hand to 0.x.
func TestTreesPrintBack(t *testing.T) {
	n := 0
	for _, q := range corpusSQL(t) {
		s, err := Parse(q)
		if err != nil {
			continue
		}
		n++
		checkPrintsBack(t, q, s)
		strict := (&printer{strict: true}).statement(s)
		if s2, err := Parse(strict); err != nil || dump(s2) != dump(s) {
			t.Errorf("%q prints strictly as %q, which gives %v", q, strict, err)
		}
	}
	t.Logf("%d trees printed back", n)
}

func checkPrintsBack(t testing.TB, q string, s Statement) {
	t.Helper()
	out := s.String()
	s2, err := Parse(out)
	if err != nil {
		t.Fatalf("%q prints as %q, which gives %v", q, out, err)
	}
	if a, b := dump(s), dump(s2); a != b {
		t.Fatalf("%q prints as %q, which gives another tree:\n%s\n%s", q, out, a, b)
	}
	if out2 := s2.String(); out2 != out {
		t.Fatalf("%q prints as %q, and again as %q", q, out, out2)
	}
}

// TestColumnNamesMatchTheCorpus holds the result columns' text to the
// names 0.x gave them. A column with an alias is called by it, a field by
// its name as the table spells it, which in the corpus's tables is the
// name in lower case, and any other column by its text.
func TestColumnNamesMatchTheCorpus(t *testing.T) {
	n := 0
	check := func(id, q string, ans *sqlcorpus.Answer) {
		if ans == nil || ans.Error != "" || len(ans.Columns) == 0 {
			return
		}
		s, err := Parse(q)
		if err != nil {
			return
		}
		sel, ok := s.(*Select)
		if !ok || sel.Star {
			return
		}
		if len(sel.Results) != len(ans.Columns) {
			t.Errorf("%s: %s has %d result columns, and 0.x gave %d", id, q, len(sel.Results), len(ans.Columns))
			return
		}
		for i, r := range sel.Results {
			var got string
			switch e := r.Expr.(type) {
			case *Column:
				got = e.Name.Folded()
			default:
				got = r.Text
			}
			if r.Alias != nil {
				got = r.Alias.Name
			}
			if got != ans.Columns[i] {
				t.Errorf("%s: %s: column %d is called %q, and 0.x called it %q", id, q, i+1, got, ans.Columns[i])
			}
			n++
		}
	}
	for _, cs := range loadCorpus(t) {
		if cs.Via == "" {
			check(cs.ID, cs.SQL, cs.Answer)
		}
		if cs.Answer != nil {
			check(cs.ID, cs.After, cs.Answer.After)
		}
	}
	if n < 300 {
		t.Errorf("only %d columns compared", n)
	}
	t.Logf("%d column names compared", n)
}
