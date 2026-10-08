// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"math"
	"slices"

	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The operators (Q4), each a Rows: Scan over a table's records in key
// order, Filter, Project, Sort, TopK and Limit, with Bounds for LIMIT and
// OFFSET. The aggregates over the whole result are in agg.go. The planner
// (Q5) puts them together, in the order SQLite's code generator does its
// work, since that order shows in which rows raise an error:
//
//   - Without ORDER BY, a SELECT is Scan, Filter, Limit and then Project.
//     OFFSET's rows get as far as WHERE, and once LIMIT's rows are out
//     nothing more is read.
//   - With ORDER BY, it's Scan, Filter, then Sort, or TopK when there's a
//     LIMIT, and then Limit. Sort works out every row's result columns and
//     terms. TopK works out every row's terms, and a row's result columns
//     only when the row is among the best k so far, as SQLite's sorter does
//     with a LIMIT.
//   - An aggregate query is Scan, Filter, Aggregate, Limit and then Project
//     over the aggregates' one row.
//
// Bounds works out LIMIT and OFFSET before anything else runs, and a LIMIT
// of 0 leaves the rest unrun.
//
// A row is a slice of values. Compiled expressions read it through a
// RowScope, whose Evals take the row from Frame.Row, which holds a
// *[]value.Value. Each operator that works out expressions takes the
// statement's Frame, copies it and points the copy's Row at the row it's
// working on, so the Args and anything else the statement keeps in its
// Frame reach every row. A row an operator hands on is good until its next
// call to Next, as Rows says, and it holds values from the store, so like
// the store's records it's good only while the read lasts.

// RowSource is one source in a FROM as its rows reach the operators: a run
// of places in the row, from Base, one for each of its Columns. A table's
// columns are its records' key and then its fields in the table's order,
// as TableSource makes them and Scan fills them. A RowScope finds names
// among the columns and marks those it finds, and Fill fills only those,
// so the planner compiles a statement's expressions before it reads rows.
type RowSource struct {
	// Name is the name the source is known by, its alias or else the
	// table's name, with ASCII letters in lower case, as the tree's
	// Source.Name gives it.
	Name string
	// Columns are the columns' names, spelt as the table spells them.
	Columns []string
	// Base is the place of the first column in the row.
	Base int

	table string // the table, for a table's source
	vec   int    // the column holding the vector, or 0 when there's none, since the key is at 0
	used  []bool // the columns a RowScope has found
	reads []int  // the same, in order
}

// TableSource returns the source of a table's records: the columns key and
// then the table's fields, from base.
func TableSource(name string, t store.Table, base int) *RowSource {
	cols := make([]string, 1+len(t.Fields))
	cols[0] = "key"
	copy(cols[1:], t.Fields)
	return &RowSource{Name: name, Columns: cols, Base: base, table: t.Name, vec: 1 + t.Vec, used: make([]bool, len(cols))}
}

// find returns the place among the columns of the one called name, as
// names match regardless of ASCII case, or -1.
func (s *RowSource) find(name string) int {
	for i, c := range s.Columns {
		if rules.SameName(c, name) {
			return i
		}
	}
	return -1
}

// use marks the column at place i as one the statement reads.
func (s *RowSource) use(i int) {
	if s.used == nil {
		s.used = make([]bool, len(s.Columns))
	}
	if s.used[i] {
		return
	}
	s.used[i] = true
	at, _ := slices.BinarySearch(s.reads, i)
	s.reads = slices.Insert(s.reads, at, i)
}

// Fill puts a table's record into row: its key, and each field the
// statement reads, NULL where the record has none. The vector reads as a
// value of kind KindVector, a copy, and only when the statement reads it.
// Fill only reads the source, so goroutines may fill rows of their own at
// once, as a nearest search's filter may (Q5).
func (s *RowSource) Fill(row []value.Value, r store.Record) {
	row[s.Base] = value.Text(r.Key)
	fs, j := r.Fields, 0
	for _, i := range s.reads {
		v := null
		switch {
		case i == 0:
			continue
		case i == s.vec:
			if r.Vec != nil {
				v = value.Vector(r.Vec)
			}
		default:
			// The record's fields come in order of place, as the reads do.
			p := i - 1
			for j < len(fs) && fs[j].Index < p {
				j++
			}
			if j < len(fs) && fs[j].Index == p {
				v = fs[j].Value
			}
		}
		row[s.Base+i] = v
	}
}

// RowScope is the Scope that hands a row's fields to compiled expressions:
// a name finds a column of one of its Sources, as SQL.md's "Sources,
// qualified names and clashes" has it, and its Eval reads the column's
// place in the row that Frame.Row points at. The one-record subquery,
// walks and the calls the evaluator leaves to others go to Next, which the
// planner gives (Q5). Without one they're refused, as with no Scope at all.
type RowScope struct {
	Sources []*RowSource
	Next    Scope
}

// rowOf returns the row f.Row points at.
func rowOf(f *Frame) ([]value.Value, bool) {
	row, ok := f.Row.(*[]value.Value)
	if !ok {
		return nil, false
	}
	return *row, true
}

// Column finds the column c names: in the source c names before a dot, or
// else in every source, where a name two of them have is ambiguous.
func (s *RowScope) Column(c *Column) (Eval, error) {
	var src *RowSource
	at := -1
	if c.Table != nil {
		name := c.Table.Folded()
		for _, x := range s.Sources {
			if x.Name == name {
				src, at = x, x.find(c.Name.Name)
				break
			}
		}
	} else {
		for _, x := range s.Sources {
			if i := x.find(c.Name.Name); i >= 0 {
				if src != nil {
					return nil, fail(c, "ambiguous column name: %s", c)
				}
				src, at = x, i
			}
		}
	}
	if at < 0 {
		return nil, fail(c, "no such column: %s", c)
	}
	src.use(at)
	p := src.Base + at
	return func(f *Frame) (value.Value, error) {
		row, ok := rowOf(f)
		if !ok {
			return null, fail(c, "%s has no row to be read from", c)
		}
		return row[p], nil
	}, nil
}

func (s *RowScope) Record(r *Record, key Eval) (RecordEval, error) {
	if s.Next == nil {
		return nil, fail(r, "no such table: %s", r.Table.Name)
	}
	return s.Next.Record(r, key)
}

func (s *RowScope) Walk(w *Walk, args []Eval) (func(f *Frame) ([]value.Value, error), error) {
	if s.Next == nil {
		return nil, &Error{Pos: w.At, Msg: "no such table: walk"}
	}
	return s.Next.Walk(w, args)
}

func (s *RowScope) Call(c *Call, args []Eval) (Eval, error) {
	if s.Next == nil {
		return nil, fail(c, "%s() needs the statement's planner", c.Func())
	}
	return s.Next.Call(c, args)
}

// Scan gives a table's records in key order, as r.Scan gives them, each
// filled into a row by src, which TableSource made. Its rows are
// src.Base+len(src.Columns) wide, and only src's places are filled. It
// starts the store's scan at its first Next.
func Scan(r store.Reader, src *RowSource) Rows {
	return &scanRows{r: r, src: src, row: make([]value.Value, src.Base+len(src.Columns))}
}

type scanRows struct {
	r       store.Reader
	src     *RowSource
	c       store.Cursor
	row     []value.Value
	err     error
	started bool
	done    bool
}

func (s *scanRows) Next() bool {
	if s.done {
		return false
	}
	if !s.started {
		s.started = true
		c, err := s.r.Scan(s.src.table+":", "")
		if err != nil {
			s.err, s.done = err, true
			return false
		}
		s.c = c
	}
	rec, ok := s.c.Next()
	if !ok {
		s.done = true
		return false
	}
	s.src.Fill(s.row, rec)
	return true
}

func (s *scanRows) Row() []value.Value { return s.row }
func (s *scanRows) Err() error         { return s.err }
func (s *scanRows) Close()             { s.done = true }

// Filter hands on the rows of in for which every one of conds is true,
// working them out in turn and stopping at the first that's false or
// NULL, as SQLite works out the terms of a WHERE that the planner has
// split at its ANDs, in the order they're written.
func Filter(in Rows, conds []Cond, f *Frame) Rows {
	x := &filterRows{in: in, conds: conds, frame: *f}
	x.frame.Row = &x.cur
	return x
}

type filterRows struct {
	in    Rows
	conds []Cond
	frame Frame
	cur   []value.Value
	err   error
	done  bool
}

func (x *filterRows) Next() bool {
	for !x.done {
		if !x.in.Next() {
			x.err, x.done = x.in.Err(), true
			break
		}
		x.cur = x.in.Row()
		if keep, err := x.keep(); err != nil {
			x.err, x.done = err, true
		} else if keep {
			return true
		}
	}
	return false
}

func (x *filterRows) keep() (bool, error) {
	for _, c := range x.conds {
		if ok, err := c(&x.frame); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func (x *filterRows) Row() []value.Value { return x.cur }
func (x *filterRows) Err() error         { return x.err }

func (x *filterRows) Close() {
	x.done = true
	x.in.Close()
}

// Project hands on a row for each row of in: the values of cols, worked
// out in turn over it.
func Project(in Rows, cols []Eval, f *Frame) Rows {
	p := &projectRows{in: in, cols: cols, frame: *f, out: make([]value.Value, len(cols))}
	p.frame.Row = &p.cur
	return p
}

type projectRows struct {
	in    Rows
	cols  []Eval
	frame Frame
	cur   []value.Value
	out   []value.Value
	err   error
	done  bool
}

func (p *projectRows) Next() bool {
	if p.done {
		return false
	}
	if !p.in.Next() {
		p.err, p.done = p.in.Err(), true
		return false
	}
	p.cur = p.in.Row()
	for i, ev := range p.cols {
		v, err := ev(&p.frame)
		if err != nil {
			p.err, p.done = err, true
			return false
		}
		p.out[i] = v
	}
	return true
}

func (p *projectRows) Row() []value.Value { return p.out }
func (p *projectRows) Err() error         { return p.err }

func (p *projectRows) Close() {
	p.done = true
	p.in.Close()
}

// Limit hands on at most n rows of in, after skipping its first skip rows,
// as LIMIT n OFFSET skip does once Bounds has worked them out. A negative n
// means no limit, and a skip below 1 skips nothing. Once it has handed on n
// rows, it reads no more of in. Without ORDER BY it goes before Project, so
// a row it skips has its result columns left unworked, as in SQLite.
func Limit(in Rows, n, skip int64) Rows {
	l := &limitRows{in: in, n: n, skip: skip}
	if plant == "query/limit-counts-offset" && skip > 0 {
		l.given = skip
	}
	return l
}

type limitRows struct {
	in    Rows
	n     int64
	skip  int64
	given int64
	err   error
	done  bool
}

func (l *limitRows) Next() bool {
	if l.done || l.n >= 0 && l.given >= l.n {
		l.done = true
		return false
	}
	for ; l.skip > 0; l.skip-- {
		if !l.in.Next() {
			l.err, l.done = l.in.Err(), true
			return false
		}
	}
	if !l.in.Next() {
		l.err, l.done = l.in.Err(), true
		return false
	}
	l.given++
	return true
}

func (l *limitRows) Row() []value.Value { return l.in.Row() }
func (l *limitRows) Err() error         { return l.err }

func (l *limitRows) Close() {
	l.done = true
	l.in.Close()
}

// Bounds works out a statement's LIMIT and OFFSET, compiled with s, as
// SQLite does before anything else runs: LIMIT first, then OFFSET. A LIMIT
// of 0 gives an n of 0 without working out OFFSET, and then the planner
// runs nothing more of the statement. Without a LIMIT, n is -1. Each must
// be a whole number as SQLite's OP_MustBeInt takes one: an integer, a real
// without a fraction strictly between the smallest and the largest
// integers, or text that reads as one of those, with spaces at either end.
// Anything else, NULL and bytes included, is "datatype mismatch". A
// negative n means no limit, and a negative skip skips nothing, as Limit
// takes them.
func Bounds(limit, offset Expr, s Scope, f *Frame) (n, skip int64, err error) {
	if limit == nil {
		return -1, 0, nil
	}
	if n, err = limitValue(limit, s, f); err != nil {
		return 0, 0, err
	}
	if offset == nil || n == 0 && plant != "query/offset-after-zero-limit" {
		return n, 0, nil
	}
	if skip, err = limitValue(offset, s, f); err != nil {
		return 0, 0, err
	}
	return n, skip, nil
}

// limitValue works out LIMIT's or OFFSET's expression and takes it as a
// whole number.
func limitValue(e Expr, s Scope, f *Frame) (int64, error) {
	ev, err := Compile(e, s)
	if err != nil {
		return 0, err
	}
	v, err := ev(f)
	if err != nil {
		return 0, err
	}
	n, ok := wholeNumber(v)
	if !ok {
		return 0, fail(e, "datatype mismatch")
	}
	return n, nil
}

// wholeNumber is OP_MustBeInt: v as a whole number, with the numeric
// affinity applyAffinity gives it, or false when it isn't one.
func wholeNumber(v value.Value) (int64, bool) {
	switch v.Kind() {
	case value.KindInt:
		return v.Int(), true
	case value.KindReal:
		return realAsInt(v.Real())
	case value.KindText:
		switch x := numericAffinity(v); x.Kind() {
		case value.KindInt:
			return x.Int(), true
		case value.KindReal:
			return realAsInt(x.Real())
		}
	}
	return 0, false
}

// realAsInt is sqlite3VdbeIntegerAffinity: r as a whole number when it's
// one, strictly between the smallest and the largest integers.
func realAsInt(r float64) (int64, bool) {
	i := realToInt(r)
	if r == float64(i) && i > math.MinInt64 && i < math.MaxInt64 {
		return i, true
	}
	return 0, false
}

// SortKey is one ORDER BY term, as Sort and TopK take it. Col is the
// result column it sorts by, counting from 0, for a term that's an alias
// or a column number, and then that column's Eval works out the term. For
// a term that's an expression of its own, Col is -1 and Eval works it out.
// Desc sorts it going down.
type SortKey struct {
	Col  int
	Eval Eval
	Desc bool
}

// Sort hands on a row for each row of in, the values of cols worked out
// over it, sorted by keys in SQL.md's order of values: going up, NULL,
// then numbers, then text, then bytes, and going down the reverse. It's
// stable, so rows that tie on every key keep the order they came in, which
// for a table is key order. Every row's terms and result columns are
// worked out, its terms first, before anything is handed on, as SQLite
// works them out for an ORDER BY without a LIMIT.
func Sort(in Rows, cols []Eval, keys []SortKey, f *Frame) Rows {
	return newSorter(in, cols, keys, -1, f)
}

// TopK hands on exactly what Sort followed by a LIMIT of k would, ties
// included, keeping at most k rows as it reads: the first k rows in the
// order Sort gives. k is LIMIT plus OFFSET, and a negative k keeps every
// row. It works out every row's terms, and the row's result columns only
// when the row is among the best k so far, either because fewer than k
// have come or because it comes before the last of them. A row that ties
// the last on every key comes after it, since it came later, so it's left
// out. That's what SQLite's sorter does under a LIMIT, so the same rows
// raise errors.
func TopK(in Rows, cols []Eval, keys []SortKey, k int64, f *Frame) Rows {
	return newSorter(in, cols, keys, k, f)
}

// sorter is Sort and TopK.
type sorter struct {
	in    Rows
	cols  []Eval
	keys  []SortKey
	k     int64 // the most rows kept, or -1 for every row
	frame Frame
	cur   []value.Value // the row of in being worked on
	// A kept row holds the values of cols, then the keys of their own. at
	// is where each key's value is in it, and isKey marks the result
	// columns that are keys, worked out with the keys.
	at    []int
	isKey []bool
	width int

	rows []sortRow // the rows kept: a heap with the last at the top, for TopK
	next int       // the next row to hand on, once they're sorted
	err  error
	ran  bool
	done bool
}

// sortRow is a kept row, with its place in the order rows came in.
type sortRow struct {
	vals []value.Value
	seq  int64
}

func newSorter(in Rows, cols []Eval, keys []SortKey, k int64, f *Frame) *sorter {
	s := &sorter{in: in, cols: cols, keys: keys, k: k, frame: *f, at: make([]int, len(keys)), isKey: make([]bool, len(cols))}
	s.frame.Row = &s.cur
	s.width = len(cols)
	for i, key := range keys {
		if key.Col >= 0 {
			s.at[i] = key.Col
			s.isKey[key.Col] = true
		} else {
			s.at[i] = s.width
			s.width++
		}
	}
	return s
}

// compareKeys compares two kept rows by the keys alone.
func (s *sorter) compareKeys(a, b []value.Value) int {
	for i, key := range s.keys {
		x, y := a[s.at[i]], b[s.at[i]]
		c := compare(x, y)
		if plant == "query/sort-nulls-last" && x.IsNull() != y.IsNull() {
			c = -c
		}
		if c != 0 {
			if key.Desc {
				return -c
			}
			return c
		}
	}
	return 0
}

// compareRows compares two kept rows by the keys, and then by the order
// they came in.
func (s *sorter) compareRows(a, b sortRow) int {
	if c := s.compareKeys(a.vals, b.vals); c != 0 {
		return c
	}
	switch {
	case a.seq < b.seq:
		return -1
	case a.seq > b.seq:
		return 1
	}
	return 0
}

// run reads every row of in, keeping the rows it hands on, then sorts them.
func (s *sorter) run() error {
	cand := make([]value.Value, s.width)
	var buf []value.Value // backs the rows Sort keeps
	for seq := int64(0); s.in.Next(); seq++ {
		s.cur = s.in.Row()
		for i, key := range s.keys {
			ev := key.Eval
			if key.Col >= 0 {
				ev = s.cols[key.Col]
			}
			v, err := ev(&s.frame)
			if err != nil {
				return err
			}
			cand[s.at[i]] = v
		}
		full := s.k >= 0 && int64(len(s.rows)) >= s.k
		if plant == "query/top-k-works-out-every-row" && s.k >= 0 {
			if err := s.results(cand); err != nil {
				return err
			}
		}
		if full {
			c := s.compareKeys(cand, s.rows[0].vals)
			if c > 0 || c == 0 && plant != "query/top-k-later-tie-wins" {
				continue
			}
		}
		if err := s.results(cand); err != nil {
			return err
		}
		switch {
		case s.k < 0:
			buf = append(buf, cand...)
			s.rows = append(s.rows, sortRow{vals: buf[len(buf)-s.width : len(buf) : len(buf)], seq: seq})
		case full:
			// The new row takes the place of the last one kept.
			copy(s.rows[0].vals, cand)
			s.rows[0].seq = seq
			s.down(0)
		default:
			s.rows = append(s.rows, sortRow{vals: slices.Clone(cand), seq: seq})
			s.up(len(s.rows) - 1)
		}
	}
	if err := s.in.Err(); err != nil {
		return err
	}
	if s.k < 0 && plant == "query/sort-unstable" {
		slices.SortFunc(s.rows, func(a, b sortRow) int { return s.compareKeys(a.vals, b.vals) })
	} else {
		slices.SortFunc(s.rows, s.compareRows)
	}
	return nil
}

// results works out the result columns that aren't keys into cand.
func (s *sorter) results(cand []value.Value) error {
	for j, ev := range s.cols {
		if s.isKey[j] {
			continue
		}
		v, err := ev(&s.frame)
		if err != nil {
			return err
		}
		cand[j] = v
	}
	return nil
}

// up and down keep TopK's rows a heap, with the row that comes last on top.
func (s *sorter) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if s.compareRows(s.rows[i], s.rows[parent]) <= 0 {
			return
		}
		s.rows[i], s.rows[parent] = s.rows[parent], s.rows[i]
		i = parent
	}
}

func (s *sorter) down(i int) {
	for {
		big := i
		for _, c := range [2]int{2*i + 1, 2*i + 2} {
			if c < len(s.rows) && s.compareRows(s.rows[c], s.rows[big]) > 0 {
				big = c
			}
		}
		if big == i {
			return
		}
		s.rows[i], s.rows[big] = s.rows[big], s.rows[i]
		i = big
	}
}

func (s *sorter) Next() bool {
	if s.done {
		return false
	}
	if !s.ran {
		s.ran = true
		if s.k == 0 {
			s.done = true
			return false
		}
		if err := s.run(); err != nil {
			s.err, s.done, s.rows = err, true, nil
			return false
		}
	}
	if s.next >= len(s.rows) {
		s.done, s.rows = true, nil
		return false
	}
	s.next++
	return true
}

func (s *sorter) Row() []value.Value { return s.rows[s.next-1].vals[:len(s.cols)] }
func (s *sorter) Err() error         { return s.err }

func (s *sorter) Close() {
	s.done, s.rows = true, nil
	s.in.Close()
}
