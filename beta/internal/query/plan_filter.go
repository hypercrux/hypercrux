// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"strconv"

	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// NearestFilter plans where, a condition on the records of table as
// ParseCondition reads it for Nearest(table, q, k, where, args...), with
// params ? marks of its own, and gives the store.Filter a search calls for
// each record with a vector (SQL.md, "Nearest's filter"). f holds the
// condition's arguments and the call's moment.
//
// 0.x runs the filter as SELECT key, vec FROM table WHERE vec IS NOT NULL
// AND (where), once its own checks have passed and the table has a vector
// size, and the filter takes that query's rules here:
//
//   - its errors are of kind "error", the names it can't find among them;
//   - its terms that read no field are worked out once, now, as SQLite
//     works them out before the query's first row. When one is false or
//     NULL, none is true, and the search finds nothing;
//   - the other terms are worked out for each record with a vector, in
//     turn, and the one-record subquery and IN over a walk once for the
//     call, the first time a record reaches them.
//
// So the caller makes Nearest's own checks first, in 0.x's order, and calls
// NearestFilter only for a table with a vector size, as 0.x prepares its
// query only then. A where that holds only spaces is no filter at all.
func NearestFilter(r store.Reader, table string, where Expr, params int, f *Frame) (keep store.Filter, none bool, err error) {
	if len(f.Args) != params {
		return nil, false, &Error{Pos: 0, Msg: "the filter has " + strconv.Itoa(params) + " ? marks and " + strconv.Itoa(len(f.Args)) + " arguments: it takes one argument for each"}
	}
	vec := &Column{Name: Ident{Name: "vec"}}
	sel := &Select{
		Results: []Result{{Expr: &Column{Name: Ident{Name: "key"}}, Text: "key"}},
		From:    &From{Sources: []*Source{{Table: &Ident{Name: table}}}},
		Where:   &Binary{Op: OpAnd, L: &Binary{Op: OpIsNot, L: vec, R: &Literal{Kind: LitNull, Text: "NULL"}}, R: where},
		params:  params,
	}
	p, err := prepare(r, sel, false)
	if err != nil {
		return nil, false, err
	}
	frame := *f
	frame.Row = nil
	for _, c := range p.once {
		ok, err := c(&frame)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, true, nil
		}
	}
	// The search sees only records with a vector, so vec IS NOT NULL, the
	// first term for each row, holds for each it sees.
	terms := p.conds[1:]
	if plant == "query/filter-first-term-dropped" && len(terms) > 0 {
		terms = terms[1:]
	}
	p.near = &nearPlan{filter: terms}
	return p.filter(&frame), false, nil
}
