// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import "strconv"

// What the planner (Q5) does to a statement's tree before it compiles it:
// the folding SQLite's parser does to AND, which the tree from Parse leaves
// out since it would print back as other SQL, the split of a WHERE at its
// top-level ANDs, the result columns' aliases put into ORDER BY's
// expressions, and the questions the planner asks of an expression: what
// it reads, and whether it can raise an error at all.

// rewrite gives e with f applied to every node, its operands first, and
// those operands' new forms in it. A node whose operands don't change stays
// the node it was, so a tree nothing happens to comes back as it went in,
// and the planner can still tell its nodes apart by their pointers, as
// NewAggregates does. It goes into the one-record subquery's key and a
// walk's arguments too, as SQLite's parser builds those the same way.
func rewrite(e Expr, f func(Expr) Expr) Expr {
	switch x := e.(type) {
	case *Call:
		if args, changed := rewriteList(x.Args, f); changed {
			c := *x
			c.Args = args
			e = &c
		}
	case *Cast:
		if y := rewrite(x.X, f); y != x.X {
			c := *x
			c.X = y
			e = &c
		}
	case *Unary:
		if y := rewrite(x.X, f); y != x.X {
			c := *x
			c.X = y
			e = &c
		}
	case *Binary:
		l, r := rewrite(x.L, f), rewrite(x.R, f)
		if l != x.L || r != x.R {
			c := *x
			c.L, c.R = l, r
			e = &c
		}
	case *Between:
		y, lo, hi := rewrite(x.X, f), rewrite(x.Low, f), rewrite(x.High, f)
		if y != x.X || lo != x.Low || hi != x.High {
			c := *x
			c.X, c.Low, c.High = y, lo, hi
			e = &c
		}
	case *Like:
		y, p := rewrite(x.X, f), rewrite(x.Pattern, f)
		esc := x.Escape
		if esc != nil {
			esc = rewrite(esc, f)
		}
		if y != x.X || p != x.Pattern || esc != x.Escape {
			c := *x
			c.X, c.Pattern, c.Escape = y, p, esc
			e = &c
		}
	case *In:
		y := rewrite(x.X, f)
		list, changed := rewriteList(x.List, f)
		w := x.Walk
		if w != nil {
			if args, ch := rewriteList(w.Args, f); ch {
				cw := *w
				cw.Args = args
				w = &cw
			}
		}
		if y != x.X || changed || w != x.Walk {
			c := *x
			c.X, c.List, c.Walk = y, list, w
			e = &c
		}
	case *Record:
		if k := rewrite(x.Key, f); k != x.Key {
			c := *x
			c.Key = k
			e = &c
		}
	}
	return f(e)
}

// rewriteList is rewrite over a list, and reports whether any item changed.
func rewriteList(es []Expr, f func(Expr) Expr) ([]Expr, bool) {
	var out []Expr
	for i, x := range es {
		y := rewrite(x, f)
		if y != x && out == nil {
			out = append(make([]Expr, 0, len(es)), es[:i]...)
		}
		if out != nil {
			out = append(out, y)
		}
	}
	if out == nil {
		return es, false
	}
	return out, true
}

// foldAnds makes x AND y the integer literal 0 wherever SQLite's parser
// does, in sqlite3ExprAnd: when either side is always false, as alwaysFalse
// says, and neither side calls a function. It works from the inside out, as
// the parser builds the tree, so the 0 can fold the AND around it. The
// fold shows in a WHERE, which is split at its ANDs after it, so in
// WHERE (SELECT ...) AND 0 the subquery never runs, and in IN lists, whose
// items become constant: 1 IN (1, n AND 0, E) works out E. Where the
// evaluator works out an AND it simplifies it the same way already.
func foldAnds(e Expr) Expr {
	if e == nil {
		return nil
	}
	if plant == "query/and-not-folded" {
		return e
	}
	return rewrite(e, func(e Expr) Expr {
		b, ok := e.(*Binary)
		if !ok || b.Op != OpAnd {
			return e
		}
		if (alwaysFalse(b.L) || alwaysFalse(b.R)) && !hasCall(b.L) && !hasCall(b.R) {
			return &Literal{At: b.At, Kind: LitInt, Text: "0"}
		}
		return e
	})
}

// andTerms splits a WHERE at its top-level ANDs, as SQLite's whereSplit
// does, in the order they're written. Parentheses leave no node, so
// (a AND b) AND c gives three terms.
func andTerms(e Expr) []Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*Binary); ok && b.Op == OpAnd {
		return append(andTerms(b.L), andTerms(b.R)...)
	}
	return []Expr{e}
}

// columnsOf returns the names e reads, outside its subqueries, which can't
// read fields.
func columnsOf(e Expr) []*Column {
	var cols []*Column
	walkExpr(e, func(e Expr) bool {
		if c, ok := e.(*Column); ok {
			cols = append(cols, c)
		}
		return true
	})
	return cols
}

// onceOnly reports whether SQLite works out the WHERE term e once, before
// it reads any row, as sqlite3WhereBegin does with a term that reads no
// table: one with no fields, no subquery and no call of a function that
// isn't deterministic, which in the subset is walk(). The one-record
// subquery and IN over a walk are subqueries to SQLite, and
// exprIsDeterministic counts any subquery against a term. date() and
// datetime() count as constant within a statement, as SQLITE_FUNC_SLOCHNG
// has them.
func onceOnly(e Expr) bool {
	ok := true
	walkExpr(e, func(e Expr) bool {
		switch e := e.(type) {
		case *Column, *Record:
			ok = false
		case *In:
			if e.Walk != nil {
				ok = false
			}
		case *Call:
			if e.Func() == "walk" {
				ok = false
			}
		}
		return ok
	})
	return ok
}

// fieldFree reports whether e reads no field, outside its subqueries, which
// can't read fields.
func fieldFree(e Expr) bool { return len(columnsOf(e)) == 0 }

// mayFail reports whether working out e could raise an error, for some row
// and some arguments. It says yes whenever it can't be sure: LIKE, ||,
// abs(), replace(), distance(), vector(), walk(), the dates, the aggregates
// and IN over a walk can each raise one, and the one-record subquery can
// when its key can. Fields, literals, ? marks, comparisons, arithmetic,
// CAST and the other functions never do. The planner asks it before it
// takes a nearest search, which works out less than a sort would.
func mayFail(e Expr) bool {
	fails := false
	var visit func(e Expr)
	visit = func(e Expr) {
		if fails {
			return
		}
		switch e := e.(type) {
		case *Literal, *Param, *Column:
		case *Cast:
			visit(e.X)
		case *Unary:
			visit(e.X)
		case *Binary:
			if e.Op == OpConcat {
				fails = true
				return
			}
			visit(e.L)
			visit(e.R)
		case *Between:
			visit(e.X)
			visit(e.Low)
			visit(e.High)
		case *In:
			if e.Walk != nil {
				fails = true
				return
			}
			visit(e.X)
			for _, it := range e.List {
				visit(it)
			}
		case *Record:
			visit(e.Key)
		case *Call:
			switch e.Func() {
			case "coalesce", "ifnull", "nullif", "typeof", "length", "lower", "upper", "instr", "round", "substr", "trim":
			case "max", "min":
				if e.Aggregate() {
					fails = true
					return
				}
			default:
				fails = true
				return
			}
			for _, a := range e.Args {
				visit(a)
			}
		default:
			fails = true
		}
	}
	visit(e)
	return fails
}

// termNumber reads an ORDER BY term as a column number, as SQLite's
// sqlite3ExprIsInteger does there: an integer literal that fits in 32
// bits, perhaps with signs in front of it, or something SQLite's parser
// has made one, such as a literal's IS NULL or an AND foldAnds has folded.
func termNumber(e Expr) (int64, bool) {
	neg := false
	for {
		u, ok := e.(*Unary)
		if !ok || u.Op == OpNot {
			break
		}
		if u.Op == OpNeg {
			neg = !neg
		}
		e = u.X
	}
	var n int64
	switch x := e.(type) {
	case *Literal:
		if _, ok := truthKnown(x); !ok {
			return 0, false
		}
		n = literal(x, false).Int()
	case *Binary:
		isTrue, ok := foldedIsNull(x)
		if !ok {
			return 0, false
		}
		if isTrue {
			n = 1
		}
	default:
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// nthWord is the ordinal SQLite's messages use: 1st, 2nd, 3rd, 4th, and
// 11th to 13th, as its %r writes them.
func nthWord(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}
