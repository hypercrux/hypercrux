// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/sqlcorpus"
)

// The closing test of Q1: every expression in the corpus gives 0.x's
// answer exactly, through Parse and the evaluator.

// argValue makes a Go argument a value through Arg, as a ? mark takes it.
func argValue(v any) value.Value {
	x, err := Arg(v)
	if err != nil {
		panic("argValue: " + err.Error())
	}
	return x
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }

func argValues(vs []difftest.Value) []value.Value {
	out := make([]value.Value, len(vs))
	for i, v := range vs {
		out[i] = argValue(v.V)
	}
	return out
}

// goValue is a result as 0.x hands it out through database/sql.
func goValue(v value.Value) any {
	switch kind(v) {
	case value.KindNull:
		return nil
	case value.KindInt:
		return v.Int()
	case value.KindReal:
		return v.Real()
	case value.KindText:
		return v.Raw()
	}
	return []byte(v.Raw())
}

// evalSelect works out a SELECT of expressions without a FROM, as the
// corpus's expression cases are, and gives its answer as the corpus has
// it: the columns named by their aliases, then one row, or an error's kind.
func evalSelect(t testing.TB, q string, args []value.Value) *sqlcorpus.Answer {
	t.Helper()
	s, err := Parse(q)
	if err != nil {
		return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
	}
	sel := s.(*Select)
	ans := &sqlcorpus.Answer{}
	var row []difftest.Value
	f := &Frame{Args: args}
	for _, r := range sel.Results {
		name := r.Text
		if r.Alias != nil {
			name = r.Alias.Name
		}
		ans.Columns = append(ans.Columns, name)
		ev, err := Compile(r.Expr, nil)
		if err != nil {
			return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
		}
		v, err := ev(f)
		if err != nil {
			return &sqlcorpus.Answer{Error: errorKind(err), Message: err.Error()}
		}
		row = append(row, difftest.Value{V: goValue(v)})
	}
	ans.Rows = [][]difftest.Value{row}
	return ans
}

// errorKind is difftest.Kind's for the errors SQL gives in a SELECT: every
// one is "error".
func errorKind(err error) string {
	var pe *Error
	var fe *FuncError
	if errors.As(err, &pe) || errors.As(err, &fe) {
		return "error"
	}
	return "unexpected"
}

func TestTheCorpusExpressionsGive0xsAnswers(t *testing.T) {
	cases, err := sqlcorpus.Load(filepath.Join("..", "..", "sqlcorpus", "testdata", "expressions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	n, bad := 0, 0
	for _, cs := range cases {
		got := evalSelect(t, cs.SQL, argValues(cs.Args))
		n++
		if !sqlcorpus.Same(cs.Answer, got, cs.Close) {
			bad++
			if bad <= 40 {
				t.Errorf("%s: %s %v\n  want %+v\n  got  %+v", cs.ID, cs.SQL, cs.Args, cs.Answer, got)
			}
		}
	}
	if n != 3000 {
		t.Errorf("%d expressions, and the corpus has 3,000", n)
	}
	if bad > 0 {
		t.Errorf("%d of %d differ", bad, n)
	}
}
