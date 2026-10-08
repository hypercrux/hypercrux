// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

// The checks beside the grammar that need no database, from SQL.md's
// "Grammar", "Functions", "Aggregates" and "Writes": the functions and their
// arguments, where fields and aggregates may go, what a write names, and
// SQLite's limit on how tall expressions grow, which counts through
// subqueries. They run once the whole statement has parsed, as SQLite's do,
// so a syntax error anywhere comes first.

// exprRules says what an expression may use where it stands.
type exprRules struct {
	fields     bool   // it may use fields
	noFields   string // otherwise, what can't use them, for the message
	aggregates bool   // it may use aggregates
	misuse     string // the message's start for an aggregate where it can't go
}

var (
	fieldRules  = exprRules{fields: true, misuse: "misuse of aggregate function"}
	resultRules = exprRules{fields: true, aggregates: true, misuse: "misuse of aggregate function"}
	walkRules   = exprRules{noFields: "a walk's arguments in a FROM or an IN", misuse: "misuse of aggregate function"}
	keyRules    = exprRules{noFields: "the one-record subquery's key", misuse: "misuse of aggregate function"}
	limitRules  = exprRules{noFields: "LIMIT and OFFSET", misuse: "misuse of aggregate function"}
	valueRules  = exprRules{noFields: "an INSERT's values", misuse: "misuse of aggregate function"}
)

// check checks a statement the parser has read.
func (p *parser) check(s Statement) {
	switch s := s.(type) {
	case *Select:
		p.checkSelect(s)
	case *Insert:
		p.checkInsert(s)
	case *Update:
		seen := map[string]bool{}
		for _, a := range s.Set {
			f := a.Field.Folded()
			if seen[f] {
				p.fail(a.Field.At, "the field %s is set twice", a.Field.Name)
			}
			seen[f] = true
			p.checkExpr(a.Value, fieldRules, 0)
		}
		if s.Where != nil {
			p.checkExpr(s.Where, fieldRules, 0)
		}
	case *Delete:
		if s.Where != nil {
			p.checkExpr(s.Where, fieldRules, 0)
		}
	}
}

func (p *parser) checkSelect(s *Select) {
	for _, r := range s.Results {
		if hasAggregate(r.Expr) {
			s.Aggregate = true
		}
	}
	for _, r := range s.Results {
		p.checkExpr(r.Expr, resultRules, 0)
		if s.Aggregate {
			if c := fieldOutside(r.Expr); c != nil {
				p.outsideWhy(c.At, c.String()+" beside an aggregate",
					"in a query with an aggregate, fields go only inside an aggregate's argument")
			}
		}
	}
	if f := s.From; f != nil {
		if s.Star && len(f.Sources) == 2 {
			p.outside(s.At, "* over a join")
		}
		if s.Star && f.Sources[0].Walk != nil && f.Sources[0].Walk.Each {
			p.outside(s.At, "* over json_each")
		}
		for _, src := range f.Sources {
			if src.Walk != nil {
				p.checkWalk(src.Walk, 0)
			}
		}
	}
	if s.Where != nil {
		if plant == "query/aggregate-in-where" {
			p.checkExpr(s.Where, resultRules, 0)
		} else {
			p.checkExpr(s.Where, fieldRules, 0)
		}
	}
	order := fieldRules
	if s.Aggregate {
		order = resultRules
	} else {
		order.misuse = "misuse of aggregate:"
	}
	for _, t := range s.OrderBy {
		p.checkExpr(t.Expr, order, 0)
	}
	if s.Limit != nil {
		p.checkExpr(s.Limit, limitRules, 0)
	}
	if s.Offset != nil {
		p.checkExpr(s.Offset, limitRules, 0)
	}
}

func (p *parser) checkInsert(s *Insert) {
	seen := map[string]bool{}
	for _, f := range s.Fields {
		if seen[f.Folded()] {
			p.fail(f.At, "the field %s is named twice", f.Name)
		}
		seen[f.Folded()] = true
	}
	if !seen["key"] {
		// Every record has a key, so an INSERT that doesn't give one fails
		// on its first row, as in 0.x, whose key is NOT NULL.
		p.fail(s.At, "NOT NULL constraint failed: %s.key", s.Table.Folded())
	}
	for i, row := range s.Rows {
		if i > 0 && len(row) != len(s.Rows[0]) {
			p.fail(row[0].Pos(), "all VALUES must have the same number of terms")
		}
	}
	if n := len(s.Rows[0]); n != len(s.Fields) {
		p.fail(s.Rows[0][0].Pos(), "%d values for %d columns", n, len(s.Fields))
	}
	for _, row := range s.Rows {
		for _, v := range row {
			p.checkExpr(v, valueRules, 0)
		}
	}
}

// checkWalk checks a walk's arguments, which can't use fields. SQLite works
// them out in the walk's own query, one level down from base, and json_each's
// argument is the call walk(...), one taller than its arguments.
func (p *parser) checkWalk(w *Walk, base int) {
	if w.Each {
		base++
	}
	for _, a := range w.Args {
		p.checkExpr(a, walkRules, base)
	}
}

// checkExpr checks one expression by the rules where it stands. base is the
// height SQLite has counted already when it reaches the expression: 0 at
// the top of a statement, and more inside a subquery, since SQLite adds up
// the heights of the expressions a subquery sits in.
func (p *parser) checkExpr(e Expr, rules exprRules, base int) {
	total := base + height(e)
	if total > maxHeight {
		p.fail(e.Pos(), "Expression tree is too large (maximum depth %d)", maxHeight)
	}
	p.visit(e, rules, false, total)
}

// visit checks e and what's in it. agg says whether e is inside an
// aggregate's argument.
func (p *parser) visit(e Expr, rules exprRules, agg bool, total int) {
	switch e := e.(type) {
	case *Column:
		if !rules.fields {
			p.fail(e.At, "%s can't use fields: %s", rules.noFields, e)
		}
	case *Call:
		p.checkCall(e)
		if e.Aggregate() {
			if agg || !rules.aggregates {
				p.fail(e.At, "%s %s()", rules.misuse, e.Name.Name)
			}
			agg = true
		}
		for _, a := range e.Args {
			p.visit(a, rules, agg, total)
		}
	case *Cast:
		p.visit(e.X, rules, agg, total)
	case *Unary:
		p.visit(e.X, rules, agg, total)
	case *Binary:
		p.visit(e.L, rules, agg, total)
		p.visit(e.R, rules, agg, total)
	case *Between:
		p.visit(e.X, rules, agg, total)
		p.visit(e.Low, rules, agg, total)
		p.visit(e.High, rules, agg, total)
	case *Like:
		p.visit(e.X, rules, agg, total)
		p.visit(e.Pattern, rules, agg, total)
		if e.Escape != nil {
			p.visit(e.Escape, rules, agg, total)
		}
	case *In:
		p.visit(e.X, rules, agg, total)
		for _, it := range e.List {
			p.visit(it, rules, agg, total)
		}
		if e.Walk != nil {
			p.checkWalk(e.Walk, total)
		}
	case *Record:
		// The subquery's WHERE is key = e, one taller than e.
		p.checkExpr(e.Key, keyRules, total+1)
	}
}

// hasAggregate reports whether e calls an aggregate, outside any subquery.
func hasAggregate(e Expr) bool {
	found := false
	walkExpr(e, func(e Expr) bool {
		if c, ok := e.(*Call); ok && c.Aggregate() {
			found = true
		}
		return !found
	})
	return found
}

// fieldOutside returns the first field e uses outside an aggregate's
// argument, or nil.
func fieldOutside(e Expr) *Column {
	var col *Column
	walkExpr(e, func(e Expr) bool {
		switch e := e.(type) {
		case *Column:
			if col == nil {
				col = e
			}
		case *Call:
			return !e.Aggregate() && col == nil
		}
		return col == nil
	})
	return col
}

// walkExpr calls f for e and, when f returns true, for what's in it, apart
// from what's inside a subquery: the one-record subquery's key and a walk's
// arguments.
func walkExpr(e Expr, f func(Expr) bool) {
	if !f(e) {
		return
	}
	switch e := e.(type) {
	case *Call:
		for _, a := range e.Args {
			walkExpr(a, f)
		}
	case *Cast:
		walkExpr(e.X, f)
	case *Unary:
		walkExpr(e.X, f)
	case *Binary:
		walkExpr(e.L, f)
		walkExpr(e.R, f)
	case *Between:
		walkExpr(e.X, f)
		walkExpr(e.Low, f)
		walkExpr(e.High, f)
	case *Like:
		walkExpr(e.X, f)
		walkExpr(e.Pattern, f)
		if e.Escape != nil {
			walkExpr(e.Escape, f)
		}
	case *In:
		walkExpr(e.X, f)
		for _, it := range e.List {
			walkExpr(it, f)
		}
	}
}
