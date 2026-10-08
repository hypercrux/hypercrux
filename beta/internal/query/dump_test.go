// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"fmt"
	"strings"
)

// dump writes a tree as a short S-expression, leaving out the places in the
// text and the result columns' text, so two trees compare by what they
// mean. The tests write the trees they expect this way: (= 1 (< 2 3)) for
// 1 = 2 < 3.
func dump(s Statement) string {
	var b strings.Builder
	switch s := s.(type) {
	case *Select:
		b.WriteString("(select")
		if s.Star {
			b.WriteString(" *")
		}
		for _, r := range s.Results {
			b.WriteString(" " + dumpExpr(r.Expr))
			if r.Alias != nil {
				b.WriteString(" as " + dumpIdent(*r.Alias))
			}
		}
		if f := s.From; f != nil {
			b.WriteString(" (from")
			for _, src := range f.Sources {
				b.WriteString(" " + dumpSource(src))
			}
			if f.On != nil {
				b.WriteString(" (on " + dumpExpr(f.On.Left) + " " + dumpExpr(f.On.Right) + ")")
			}
			b.WriteString(")")
		}
		if s.Where != nil {
			b.WriteString(" (where " + dumpExpr(s.Where) + ")")
		}
		if len(s.OrderBy) > 0 {
			b.WriteString(" (order")
			for _, t := range s.OrderBy {
				b.WriteString(" " + dumpExpr(t.Expr))
				if t.Desc {
					b.WriteString(" desc")
				}
			}
			b.WriteString(")")
		}
		if s.Limit != nil {
			b.WriteString(" (limit " + dumpExpr(s.Limit))
			if s.Offset != nil {
				b.WriteString(" " + dumpExpr(s.Offset))
			}
			b.WriteString(")")
		}
		if s.Aggregate {
			b.WriteString(" aggregate")
		}
		b.WriteString(")")
	case *Insert:
		b.WriteString("(insert " + dumpIdent(s.Table) + " (")
		for i, f := range s.Fields {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(dumpIdent(f))
		}
		b.WriteString(")")
		for _, row := range s.Rows {
			b.WriteString(" (" + dumpList(row) + ")")
		}
		b.WriteString(")")
	case *Update:
		b.WriteString("(update " + dumpIdent(s.Table))
		for _, a := range s.Set {
			b.WriteString(" (set " + dumpIdent(a.Field) + " " + dumpExpr(a.Value) + ")")
		}
		if s.Where != nil {
			b.WriteString(" (where " + dumpExpr(s.Where) + ")")
		}
		b.WriteString(")")
	case *Delete:
		b.WriteString("(delete " + dumpIdent(s.Table))
		if s.Where != nil {
			b.WriteString(" (where " + dumpExpr(s.Where) + ")")
		}
		b.WriteString(")")
	default:
		return fmt.Sprintf("(unknown %T)", s)
	}
	fmt.Fprintf(&b, " params=%d", s.Params())
	return b.String()
}

func dumpIdent(id Ident) string {
	if id.Quoted {
		return fmt.Sprintf("%q", id.Name)
	}
	return id.Name
}

func dumpSource(s *Source) string {
	var t string
	if s.Table != nil {
		t = dumpIdent(*s.Table)
	} else {
		t = dumpWalk(s.Walk)
	}
	if s.Alias != nil {
		t += " as " + dumpIdent(*s.Alias)
	}
	return t
}

func dumpWalk(w *Walk) string {
	s := "(walk " + dumpList(w.Args) + ")"
	if w.Each {
		s = "(each " + s + ")"
	}
	return s
}

func dumpList(es []Expr) string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = dumpExpr(e)
	}
	return strings.Join(parts, " ")
}

func notWord(n bool) string {
	if n {
		return "NOT "
	}
	return ""
}

func dumpExpr(e Expr) string {
	switch e := e.(type) {
	case *Literal:
		return e.Text
	case *Param:
		return fmt.Sprintf("?%d", e.Index)
	case *Column:
		if e.Table != nil {
			return dumpIdent(*e.Table) + "." + dumpIdent(e.Name)
		}
		return dumpIdent(e.Name)
	case *Call:
		if e.Star {
			return "(" + dumpIdent(e.Name) + " *)"
		}
		if len(e.Args) == 0 {
			return "(" + dumpIdent(e.Name) + ")"
		}
		return "(" + dumpIdent(e.Name) + " " + dumpList(e.Args) + ")"
	case *Cast:
		return "(CAST " + dumpExpr(e.X) + " " + e.Type + ")"
	case *Unary:
		return "(" + e.Op.String() + " " + dumpExpr(e.X) + ")"
	case *Binary:
		return "(" + e.Op.String() + " " + dumpExpr(e.L) + " " + dumpExpr(e.R) + ")"
	case *Between:
		return "(" + notWord(e.Not) + "BETWEEN " + dumpExpr(e.X) + " " + dumpExpr(e.Low) + " " + dumpExpr(e.High) + ")"
	case *Like:
		s := "(" + notWord(e.Not) + "LIKE " + dumpExpr(e.X) + " " + dumpExpr(e.Pattern)
		if e.Escape != nil {
			s += " " + dumpExpr(e.Escape)
		}
		return s + ")"
	case *In:
		s := "(" + notWord(e.Not) + "IN " + dumpExpr(e.X)
		if e.Walk != nil {
			return s + " " + dumpWalk(e.Walk) + ")"
		}
		return s + " [" + dumpList(e.List) + "])"
	case *Record:
		return "(record " + dumpIdent(e.Field) + " " + dumpIdent(e.Table) + " " + dumpExpr(e.Key) + ")"
	case nil:
		return "nil"
	}
	return fmt.Sprintf("(unknown %T)", e)
}
