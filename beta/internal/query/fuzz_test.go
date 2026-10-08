// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"strings"
	"testing"
)

// FuzzParse is the parser on any text, seeded from every piece of SQL in
// the corpus. It must never panic, refuse with an *Error placed inside the
// text, count every ? mark, and print back what it takes as SQL that
// parses to the same tree. Nearest's conditions are held to the same.
func FuzzParse(f *testing.F) {
	for _, q := range corpusSQL(f) {
		f.Add(q)
	}
	for _, q := range []string{
		"SELECT - - 1, -+1, +-1, - NOT 1 + 2, NOT NOT 1",
		"SELECT x IS (NOT y), x IS NOT NOT y, (- NOT x) + y",
		"SELECT 1 --x\n, 2 /* y */ FROM t",
		"SELECT \"a\"\"b\" AS \"c\", x'00' FROM \"t\" \"u\" WHERE \"u\".\"a\"\"b\" = ?",
		"SELECT d.key FROM walk(?, 2, NULL, 'both') w JOIN docs d ON (d.key = w.key) ORDER BY d.key DESC LIMIT ? OFFSET ?",
		"UPDATE t SET a = b BETWEEN c AND d, b = NOT a = 1 WHERE x LIKE y ESCAPE z;",
		"SELECT 1 IN (), 1 NOT IN (SELECT value FROM json_each(walk('a', 1)))",
	} {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, q string) {
		s, err := Parse(q)
		end := len(q)
		if i := strings.IndexByte(q, 0); i >= 0 {
			end = i
		}
		if err != nil {
			var pe *Error
			if !errors.As(err, &pe) || pe.Pos < 0 || pe.Pos > end || pe.Msg == "" {
				t.Fatalf("%q gives %#v", q, err)
			}
		} else {
			if n := countParams(s); n != s.Params() {
				t.Fatalf("%q holds %d marks, and Params says %d", q, n, s.Params())
			}
			checkPrintsBack(t, q, s)
		}
		cond, n, err := ParseCondition(q)
		switch {
		case err != nil:
			var pe *Error
			if !errors.As(err, &pe) || pe.Pos < 0 || pe.Pos > end {
				t.Fatalf("the condition %q gives %#v", q, err)
			}
		case cond != nil:
			out := cond.String()
			again, n2, err := ParseCondition(out)
			if err != nil || n2 != n || dumpExpr(again) != dumpExpr(cond) {
				t.Fatalf("the condition %q prints as %q, which gives %s, %v", q, out, dumpExpr(again), err)
			}
		}
	})
}

// countParams counts the ? marks in a statement's tree, and checks that
// they're numbered from 0 in the order of the text.
func countParams(s Statement) int {
	var exprs []Expr
	switch s := s.(type) {
	case *Select:
		for _, r := range s.Results {
			exprs = append(exprs, r.Expr)
		}
		if s.From != nil {
			for _, src := range s.From.Sources {
				if src.Walk != nil {
					exprs = append(exprs, src.Walk.Args...)
				}
			}
		}
		exprs = append(exprs, s.Where)
		for _, t := range s.OrderBy {
			exprs = append(exprs, t.Expr)
		}
		exprs = append(exprs, s.Limit, s.Offset)
	case *Insert:
		for _, row := range s.Rows {
			exprs = append(exprs, row...)
		}
	case *Update:
		for _, a := range s.Set {
			exprs = append(exprs, a.Value)
		}
		exprs = append(exprs, s.Where)
	case *Delete:
		exprs = append(exprs, s.Where)
	}
	var marks []*Param
	var visit func(e Expr)
	visit = func(e Expr) {
		if e == nil {
			return
		}
		walkExpr(e, func(e Expr) bool {
			switch e := e.(type) {
			case *Param:
				marks = append(marks, e)
			case *Record:
				visit(e.Key)
			case *In:
				if e.Walk != nil {
					visit(e.X)
					for _, a := range e.Walk.Args {
						visit(a)
					}
					return false
				}
			}
			return true
		})
	}
	for _, e := range exprs {
		visit(e)
	}
	for i, m := range marks {
		if m.Index != i || i > 0 && m.At <= marks[i-1].At {
			return -1
		}
	}
	return len(marks)
}
