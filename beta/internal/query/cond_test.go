// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"testing"

	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// Conditions as WHERE takes them, through CompileCondition: AND stops at
// the first side that's false or NULL, OR at the first that's true, and
// NOT and BETWEEN go the same way inside them. eval_sqlite_test.go holds
// the table to 0.x's answers, as SELECT 1 AS v WHERE cond.

// condCases are conditions and what they give: true, false, or a failure.
// E raises an error when it's worked out.
var condCases = []struct {
	cond string
	want any
}{
	{"1", true}, {"0", false}, {"NULL", false}, {"'a'", false}, {"'1'", true}, {"0.5", true}, {"x''", false},
	{"(1=1) OR E", true}, {"(1=0) AND E", false}, {"NULL AND E", false}, {"E AND (1=0)", anError},
	{"(1=1) AND E", anError}, {"(1=0) OR E", anError}, {"NULL OR E", anError}, {"NOT ((1=1) OR E)", false},
	{"NOT ((1=0) AND E)", true}, {"NOT (NULL AND E)", anError}, {"NOT (NULL OR (1=1))", false},
	{"NULL OR (1=1)", true}, {"NULL OR NULL", false}, {"NOT NULL", false}, {"NOT 0", true}, {"NOT 'a'", true},
	{"2 BETWEEN 3 AND E", false}, {"2 BETWEEN 1 AND E", anError}, {"0 NOT BETWEEN 1 AND E", true},
	{"2 NOT BETWEEN 1 AND E", anError}, {"NULL BETWEEN E AND 1", anError}, {"2 BETWEEN NULL AND E", false},
	{"NOT (2 BETWEEN NULL AND E)", anError}, {"NOT (0 BETWEEN NULL AND E)", anError}, {"5 BETWEEN NULL AND 3", false},
	{"NOT (5 BETWEEN NULL AND 3)", true}, {"(1 IN ()) OR (1=1)", true}, {"1 NOT IN ()", true},
	{"E IN ()", false}, {"NOT (E IN ())", true}, {"1 IN (1, E)", true}, {"1 IN (2, E)", anError},
	{"1 IN (2, 3)", false}, {"3 IN (1, NULL)", false}, {"NOT (3 IN (1, NULL))", false}, {"3 NOT IN (1, NULL)", false},
	{"(1=1) IS (1 NOT IN ())", true}, {"NULL IS NOT (1 IN ())", true}, {"(NULL OR E) IS (1 IN ())", false},
	{"((1=1) OR E) IS (1 NOT IN ())", true}, {"((1=0) AND E) IS NOT (1 NOT IN ())", true},
	{"NOT ((1=1) OR E) IS (1 IN ())", true}, {"coalesce(1, E)", true}, {"E IS NULL", anError},
	{"(0 AND E) OR (1=1)", true}, {"(1 OR E) AND (1=0)", false}, {"E AND 0", anError}, {"1 OR E", true},
	{"(1=1) AND (2=2) AND (3=3)", true}, {"((1=1) OR E) AND ((1=0) OR (2=2))", true},
	{"'a' LIKE 'A'", true}, {"NULL LIKE 'a'", false}, {"'a' NOT LIKE 'b'", true},
	{"(1=1) = (2=2)", true}, {"1 IS NOT NULL", true}, {"NULL IS NULL", true}, {"-0.0", false}, {"'0x1'", false},
}

// whereTerms splits a WHERE at its top-level ANDs, as SQLite's whereSplit
// does, without simplifying anything: what the planner hands
// CompileCondition one by one (Q5).
func whereTerms(e Expr) []Expr {
	if b, ok := e.(*Binary); ok && b.Op == OpAnd {
		return append(whereTerms(b.L), whereTerms(b.R)...)
	}
	return []Expr{e}
}

// evalWhere works out SELECT 1 AS v WHERE cond as SQLite runs it without a
// FROM: each of the WHERE's terms in turn, stopping at the first that's
// false or NULL, and gives its answer as the corpus has it.
func evalWhere(q string, args []value.Value) *sqlcorpus.Answer {
	s, err := Parse(q)
	if err != nil {
		return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
	}
	f := &Frame{Args: args}
	ans := &sqlcorpus.Answer{Columns: []string{"v"}}
	for _, term := range whereTerms(s.(*Select).Where) {
		cond, err := CompileCondition(term, nil)
		if err != nil {
			return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
		}
		ok, err := cond(f)
		if err != nil {
			return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
		}
		if !ok {
			return ans
		}
	}
	ans.Rows = [][]difftest.Value{{{V: int64(1)}}}
	return ans
}

// condAnswer is what a condCase wants, as evalWhere gives it.
func condAnswer(want any) *sqlcorpus.Answer {
	switch want {
	case true:
		return &sqlcorpus.Answer{Columns: []string{"v"}, Rows: [][]difftest.Value{{{V: int64(1)}}}}
	case false:
		return &sqlcorpus.Answer{Columns: []string{"v"}}
	}
	return &sqlcorpus.Answer{Error: "error"}
}

func TestWhereConditions(t *testing.T) {
	for _, cs := range condCases {
		q := "SELECT 1 AS v WHERE " + loneE.ReplaceAllLiteralString(cs.cond, overflow)
		if got := evalWhere(q, nil); !sqlcorpus.Same(condAnswer(cs.want), got, false) {
			t.Errorf("%s: got %+v, want %v", cs.cond, got, cs.want)
		}
	}
}
