// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"slices"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// planScope is the Scope the planner compiles a statement's expressions
// with: a field of one of the sources, found as Plan.find has it, read from
// the row Frame.Row points at; the one-record subquery and IN over a walk,
// each worked out once for the statement; and walk() as a function. Of
// json_each's columns only value may be used.
//
// The planner compiles some expressions more than once, such as a nearest
// search's query vector, which it works out before the search and which
// the sort that stands in for the search works out again. So a subquery
// keeps one answer for its node in the tree, however often it's compiled.
type planScope struct {
	p       *Plan
	records map[*Record]*recordOnce
	walks   map[*Walk]*walkOnce
}

func (s *planScope) Column(c *Column) (Eval, error) {
	src, at, err := s.p.find(c)
	if err != nil {
		return nil, err
	}
	if src.kind == eachSource && at != eachValue {
		return nil, fail(c, "%s is outside the SQL subset: of json_each's columns, only value may be used", c)
	}
	src.use(at)
	pos := src.Base + at
	return func(f *Frame) (value.Value, error) {
		row, ok := rowOf(f)
		if !ok {
			return null, fail(c, "%s has no row to be read from", c)
		}
		return row[pos], nil
	}, nil
}

// Record finds the subquery's table and field, as SQL.md has them: the
// table must exist, and the field too, or be key in any case.
func (s *planScope) Record(r *Record, key Eval) (RecordEval, error) {
	t, ok := s.p.r.Table(r.Table.Folded())
	if !ok {
		return nil, fail(r, "no such table: %s", r.Table.Name)
	}
	field := -1
	if r.Field.Folded() != "key" {
		if field = t.Find(r.Field.Name); field < 0 {
			return nil, fail(r, "no such column: %s", r.Field.Name)
		}
	}
	o := s.records[r]
	if o == nil {
		s.p.noteOnce(r.String())
		o = &recordOnce{r: s.p.r, table: t, field: field, key: key}
		if s.records == nil {
			s.records = map[*Record]*recordOnce{}
		}
		s.records[r] = o
	}
	return o.get, nil
}

func (s *planScope) Walk(w *Walk, args []Eval) (func(f *Frame) ([]value.Value, error), error) {
	o := s.walkFor(w, args)
	return func(f *Frame) ([]value.Value, error) {
		if _, err := o.get(f); err != nil {
			return nil, err
		}
		return o.keys, nil
	}, nil
}

// walkOnce compiles a walk's arguments for the planner's own use of it.
func (s *planScope) walkOnce(w *Walk) (*walkOnce, error) {
	if o := s.walks[w]; o != nil {
		return o, nil
	}
	args := make([]Eval, len(w.Args))
	for i, a := range w.Args {
		var err error
		if args[i], err = Compile(a, s); err != nil {
			return nil, err
		}
	}
	return s.walkFor(w, args), nil
}

// walkFor gives the walk's one answer for the statement, set up with args
// the first time.
func (s *planScope) walkFor(w *Walk, args []Eval) *walkOnce {
	o := s.walks[w]
	if o == nil {
		s.p.noteOnce(w.String())
		o = &walkOnce{r: s.p.r, w: w, args: args}
		if s.walks == nil {
			s.walks = map[*Walk]*walkOnce{}
		}
		s.walks[w] = o
	}
	return o
}

// Call gives walk() as a function: its arguments worked out in order, then
// the walk, as JSON text. The aggregates reach here only where they can't
// go, which the parser refuses.
func (s *planScope) Call(c *Call, args []Eval) (Eval, error) {
	if c.Func() != "walk" {
		return nil, fail(c, "misuse of aggregate function %s()", c.Name.Name)
	}
	r := s.p.r
	return func(f *Frame) (value.Value, error) {
		vals, err := walkArgs(args, f)
		if err != nil {
			return null, err
		}
		steps, err := walkSteps(r, c.At, vals)
		if err != nil {
			return null, err
		}
		return walkJSON(steps), nil
	}, nil
}

// noteOnce keeps a subquery's text for the plan's String, once.
func (p *Plan) noteOnce(text string) {
	if !slices.Contains(p.subs, text) {
		p.subs = append(p.subs, text)
	}
}

// keyLookup is a WHERE term on the table's key that names the records to
// read, as SQLite looks them up in the key's index in place of a scan: key
// = e, key IN (a, b, ...), key IN ((SELECT ...)) or key IN over a walk,
// whose other side reads no field. The term is worked out once, when the
// rows start, and only the records it names are read, in key order, so the
// statement's other terms never meet the rest.
type keyLookup struct {
	text string
	eq   Eval       // key = e
	list []Eval     // key IN (a, b, ...)
	rec  RecordEval // key IN ((SELECT ...))
	walk *walkOnce  // key IN over a walk
}

// lookupTerm finds the term a lookup uses, and sets it up: the first
// equality on the key, as SQLite prefers one, or else the first IN on it.
// It gives the term's place among terms, or -1.
func (p *Plan) lookupTerm(terms []Expr) (int, error) {
	at, isEq := -1, false
	for i, term := range terms {
		switch x := term.(type) {
		case *Binary:
			if x.Op == OpEq && (p.isKey(x.L) && fieldFree(x.R) || fieldFree(x.L) && p.isKey(x.R)) && !isEq {
				at, isEq = i, true
			}
		case *In:
			if at < 0 && !x.Not && p.isKey(x.X) && (x.Walk != nil || listFree(x.List)) {
				at = i
			}
		}
	}
	if at < 0 {
		return -1, nil
	}
	lk := &keyLookup{text: terms[at].String()}
	var err error
	switch x := terms[at].(type) {
	case *Binary:
		e := x.R
		if !p.isKey(x.L) {
			e = x.L
		}
		lk.eq, err = Compile(e, p.scope)
	case *In:
		switch {
		case x.Walk != nil:
			lk.walk, err = p.scope.walkOnce(x.Walk)
		case len(x.List) == 1:
			if r, ok := x.List[0].(*Record); ok {
				var key Eval
				if key, err = Compile(r.Key, p.scope); err == nil {
					lk.rec, err = p.scope.Record(r, key)
				}
			} else {
				lk.eq, err = Compile(x.List[0], p.scope)
			}
		default:
			lk.list = make([]Eval, len(x.List))
			for i, it := range x.List {
				if lk.list[i], err = Compile(it, p.scope); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		return -1, err
	}
	p.lookup = lk
	return at, nil
}

// listFree reports whether an IN list has items, none of them reading a
// field.
func listFree(list []Expr) bool {
	if len(list) == 0 {
		return false
	}
	for _, it := range list {
		if !fieldFree(it) {
			return false
		}
	}
	return true
}

// keys works out the term, and gives the keys it names: the text values
// among them, in byte order, each once. Anything else, a number, bytes or
// NULL, equals no key.
func (lk *keyLookup) keys(f *Frame) ([]string, error) {
	var vals []value.Value
	switch {
	case lk.eq != nil:
		v, err := lk.eq(f)
		if err != nil {
			return nil, err
		}
		vals = []value.Value{v}
	case lk.rec != nil:
		v, found, err := lk.rec(f)
		if err != nil {
			return nil, err
		}
		if found {
			vals = []value.Value{v}
		}
	case lk.walk != nil:
		o, err := lk.walk.get(f)
		if err != nil {
			return nil, err
		}
		vals = o.keys
	default:
		for _, ev := range lk.list {
			v, err := ev(f)
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
		}
	}
	keys := make([]string, 0, len(vals))
	for _, v := range vals {
		if v.Kind() == value.KindText {
			keys = append(keys, v.Raw())
		}
	}
	if plant != "query/walk-lookup-in-walk-order" {
		slices.Sort(keys)
		keys = slices.Compact(keys)
	}
	return keys, nil
}

// inTable reports whether a key belongs to a table: whether it starts with
// the table's name and a colon.
func inTable(key, table string) bool {
	return strings.HasPrefix(key, table+":") && len(key) > len(table)+1
}
