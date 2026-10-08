// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import "strings"

// Printing a tree back as SQL. The SQL parses back to the same tree, apart
// from the places in the text and the result columns' text. Literals keep
// the text they were written with, names keep their quotes, and an operand
// goes in parentheses only where the grammar needs them, so the SQL never
// nests deeper than the text the tree came from. A sign is never followed
// straight away by another, so no -- turns into a comment.

// printer writes trees as SQL. A strict one puts every operand that's an
// operator's expression in parentheses, so SQLite, which drops them, reads
// exactly the tree; the tests hand that to 0.x.
type printer struct {
	b      strings.Builder
	strict bool
}

func (s *Select) String() string { return (&printer{}).statement(s) }
func (s *Insert) String() string { return (&printer{}).statement(s) }
func (s *Update) String() string { return (&printer{}).statement(s) }
func (s *Delete) String() string { return (&printer{}).statement(s) }

func (e *Literal) String() string { return e.Text }
func (e *Param) String() string   { return "?" }
func (e *Column) String() string  { return (&printer{}).expr(e) }
func (e *Call) String() string    { return (&printer{}).expr(e) }
func (e *Cast) String() string    { return (&printer{}).expr(e) }
func (e *Unary) String() string   { return (&printer{}).expr(e) }
func (e *Binary) String() string  { return (&printer{}).expr(e) }
func (e *Between) String() string { return (&printer{}).expr(e) }
func (e *Like) String() string    { return (&printer{}).expr(e) }
func (e *In) String() string      { return (&printer{}).expr(e) }
func (e *Record) String() string  { return (&printer{}).expr(e) }
func (w *Walk) String() string    { return (&printer{}).walk(w) }

// String gives the name as it was written: bare, or in double quotes.
func (id Ident) String() string {
	if id.Quoted {
		return `"` + strings.ReplaceAll(id.Name, `"`, `""`) + `"`
	}
	return id.Name
}

func (p *printer) statement(s Statement) string {
	b := &p.b
	b.Reset()
	switch s := s.(type) {
	case *Select:
		b.WriteString("SELECT ")
		if s.Star {
			b.WriteString("*")
		}
		for i, r := range s.Results {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(p.expr(r.Expr))
			if r.Alias != nil {
				b.WriteString(" AS " + r.Alias.String())
			}
		}
		if f := s.From; f != nil {
			b.WriteString(" FROM " + p.source(f.Sources[0]))
			if len(f.Sources) == 2 {
				b.WriteString(" JOIN " + p.source(f.Sources[1]) + " ON " + p.expr(f.On.Left) + " = " + p.expr(f.On.Right))
			}
		}
		if s.Where != nil {
			b.WriteString(" WHERE " + p.expr(s.Where))
		}
		for i, t := range s.OrderBy {
			if i == 0 {
				b.WriteString(" ORDER BY ")
			} else {
				b.WriteString(", ")
			}
			b.WriteString(p.expr(t.Expr))
			if t.Desc {
				b.WriteString(" DESC")
			}
		}
		if s.Limit != nil {
			b.WriteString(" LIMIT " + p.expr(s.Limit))
		}
		if s.Offset != nil {
			b.WriteString(" OFFSET " + p.expr(s.Offset))
		}
	case *Insert:
		b.WriteString("INSERT INTO " + s.Table.String() + " (")
		for i, f := range s.Fields {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(f.String())
		}
		b.WriteString(") VALUES ")
		for i, row := range s.Rows {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("(" + p.list(row) + ")")
		}
	case *Update:
		b.WriteString("UPDATE " + s.Table.String() + " SET ")
		for i, a := range s.Set {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.Field.String() + " = " + p.expr(a.Value))
		}
		if s.Where != nil {
			b.WriteString(" WHERE " + p.expr(s.Where))
		}
	case *Delete:
		b.WriteString("DELETE FROM " + s.Table.String())
		if s.Where != nil {
			b.WriteString(" WHERE " + p.expr(s.Where))
		}
	}
	return b.String()
}

func (p *printer) source(s *Source) string {
	var t string
	if s.Table != nil {
		t = s.Table.String()
	} else {
		t = p.walk(s.Walk)
	}
	if s.Alias != nil {
		t += " AS " + s.Alias.String()
	}
	return t
}

func (p *printer) walk(w *Walk) string {
	call := "walk(" + p.list(w.Args) + ")"
	if w.Each {
		return "json_each(" + call + ")"
	}
	return call
}

func (p *printer) list(es []Expr) string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = p.expr(e)
	}
	return strings.Join(parts, ", ")
}

// expr gives a whole expression, with nothing after it.
func (p *printer) expr(e Expr) string { return p.text(e, 0) }

// level returns the level of the operator at the top of e, as SQL.md's
// table numbers them, or 10 for an operand without one.
func level(e Expr) int {
	switch e := e.(type) {
	case *Unary:
		if e.Op == OpNot {
			return levelNot
		}
		return levelSign
	case *Binary:
		return opLevel(e.Op)
	case *Between, *Like, *In:
		return levelEq
	}
	return levelSign + 1
}

func opLevel(op Op) int {
	switch op {
	case OpOr:
		return levelOr
	case OpAnd:
		return levelAnd
	case OpEq, OpNe, OpIs, OpIsNot:
		return levelEq
	case OpLt, OpLe, OpGt, OpGe:
		return levelLt
	case OpAdd, OpSub:
		return levelAdd
	case OpMul, OpDiv, OpRem:
		return levelMul
	}
	return levelConcat
}

// operand gives e where the parser reads an operand that takes binary
// operators of level min and tighter, with an operator of level follow
// after it, or 0 for none. It puts e in parentheses where its own operator
// is looser than min, or where it ends in a NOT whose operand would take in
// the operator after it, since NOT's operand runs on through level 4. A
// sign's operand takes no operator, so a sign never needs them.
func (p *printer) operand(e Expr, min, follow int) string {
	l := level(e)
	switch {
	case p.strict && l <= levelSign,
		l == levelNot && follow >= levelEq,
		l < min && l != levelNot && l != levelSign:
		return "(" + p.text(e, 0) + ")"
	}
	return p.text(e, follow)
}

// text gives e with an operator of level follow after it.
func (p *printer) text(e Expr, follow int) string {
	switch e := e.(type) {
	case *Literal:
		return e.Text
	case *Param:
		return "?"
	case *Column:
		if e.Table != nil {
			return e.Table.String() + "." + e.Name.String()
		}
		return e.Name.String()
	case *Call:
		if e.Star {
			return e.Name.String() + "(*)"
		}
		return e.Name.String() + "(" + p.list(e.Args) + ")"
	case *Cast:
		return "CAST(" + p.expr(e.X) + " AS " + e.Type + ")"
	case *Record:
		return "(SELECT " + e.Field.String() + " FROM " + e.Table.String() + " WHERE key = " + p.operand(e.Key, levelLt, 0) + ")"
	case *Unary:
		if e.Op == OpNot {
			return "NOT " + p.operand(e.X, levelEq, follow)
		}
		x := p.operand(e.X, levelSign+1, follow)
		if strings.HasPrefix(x, "-") || strings.HasPrefix(x, "+") {
			return e.Op.String() + " " + x
		}
		return e.Op.String() + x
	case *Binary:
		k := opLevel(e.Op)
		var r string
		if u, ok := e.R.(*Unary); ok && u.Op == OpNot && e.Op == OpIs && plant != "query/is-not-printed-bare" {
			r = "(" + p.text(u, 0) + ")" // IS NOT x would read as IS NOT
		} else {
			r = p.operand(e.R, k+1, follow)
		}
		return p.operand(e.L, k, k) + " " + e.Op.String() + " " + r
	case *Between:
		return p.operand(e.X, levelEq, levelEq) + " " + not(e.Not) + "BETWEEN " +
			p.operand(e.Low, levelNot, levelAnd) + " AND " + p.operand(e.High, levelLt, follow)
	case *Like:
		s := p.operand(e.X, levelEq, levelEq) + " " + not(e.Not) + "LIKE "
		if e.Escape == nil {
			return s + p.operand(e.Pattern, levelLt, follow)
		}
		return s + p.operand(e.Pattern, levelLt, 0) + " ESCAPE " + p.operand(e.Escape, levelLt, follow)
	case *In:
		s := p.operand(e.X, levelEq, levelEq) + " " + not(e.Not) + "IN ("
		switch {
		case e.Walk == nil:
			s += p.list(e.List)
		case e.Walk.Each:
			s += "SELECT value FROM " + p.walk(e.Walk)
		default:
			s += "SELECT key FROM " + p.walk(e.Walk)
		}
		return s + ")"
	}
	return "?"
}

func not(n bool) string {
	if n {
		return "NOT "
	}
	return ""
}
