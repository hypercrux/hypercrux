// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"math"
	"slices"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The aggregates over the whole result (Q4): count(*), count(x), sum(x),
// total(x), avg(x), min(x) and max(x), as SQL.md's "Aggregates" has them.
// Each is SQLite's routine of the same name: countStep, sumStep with its
// Kahan-Babuska-Neumaier summation, sumFinalize, totalFinalize,
// avgFinalize and minmaxStep.

// Aggregates are an aggregate query's aggregate calls, which Aggregate
// works out over every row that passes WHERE. The planner makes them with
// NewAggregates, then compiles the result columns with the Aggregates as
// their Scope: each aggregate call reads its result from the aggregates'
// row, and the one-record subquery, walks and the other calls go to the
// Scope the arguments were compiled with. A field outside an aggregate's
// argument, which the parser and the planner refuse, gives an error when
// it's worked out.
type Aggregates struct {
	calls []*Call
	at    map[*Call]int
	args  [][]Eval
	in    Scope
}

// NewAggregates finds every aggregate call in exprs, which are a query's
// result columns and then those of its ORDER BY terms that are expressions
// of their own, in the order SQLite's sqlite3ExprAnalyzeAggList finds
// them, and compiles each call's arguments with in, the Scope of the rows
// the calls read. It finds the calls the evaluator skips as well, such as
// the sum() in 0 AND sum(x), since SQLite works out every aggregate a
// query names, and their errors show.
//
// SQLite sorts an aggregate query's one row without working out its ORDER
// BY, apart from the aggregates in it. So the planner hands those terms to
// NewAggregates and compiles nothing else of them.
func NewAggregates(exprs []Expr, in Scope) (*Aggregates, error) {
	a := &Aggregates{at: map[*Call]int{}, in: in}
	if plant == "query/aggregates-as-compiled" {
		return a, nil
	}
	for _, e := range exprs {
		walkExpr(e, func(e Expr) bool {
			c, ok := e.(*Call)
			if !ok || !c.Aggregate() {
				return true
			}
			if _, seen := a.at[c]; !seen {
				a.at[c] = len(a.calls)
				a.calls = append(a.calls, c)
			}
			return false
		})
	}
	a.args = make([][]Eval, len(a.calls))
	for i, c := range a.calls {
		var err error
		if a.args[i], err = compileArgs(c, in); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func compileArgs(c *Call, in Scope) ([]Eval, error) {
	args := make([]Eval, len(c.Args))
	for i, x := range c.Args {
		var err error
		if args[i], err = Compile(x, in); err != nil {
			return nil, err
		}
	}
	return args, nil
}

// Len returns the number of aggregate calls, the width of Aggregate's row.
func (a *Aggregates) Len() int { return len(a.calls) }

// Column gives an Eval that refuses the field: in an aggregate query, a
// field goes only inside an aggregate's argument, which NewAggregates
// compiled with the arguments' Scope.
func (a *Aggregates) Column(c *Column) (Eval, error) {
	return func(*Frame) (value.Value, error) {
		return null, fail(c, "%s beside an aggregate is outside the SQL subset: in a query with an aggregate, fields go only inside an aggregate's argument", c)
	}, nil
}

func (a *Aggregates) Record(r *Record, key Eval) (RecordEval, error) {
	if a.in == nil {
		return nil, fail(r, "no such table: %s", r.Table.Name)
	}
	return a.in.Record(r, key)
}

func (a *Aggregates) Walk(w *Walk, args []Eval) (func(f *Frame) ([]value.Value, error), error) {
	if a.in == nil {
		return nil, &Error{Pos: w.At, Msg: "no such table: walk"}
	}
	return a.in.Walk(w, args)
}

// Call gives an aggregate call's result from the aggregates' row, and
// hands other calls to the arguments' Scope.
func (a *Aggregates) Call(c *Call, args []Eval) (Eval, error) {
	if !c.Aggregate() {
		if a.in == nil {
			return nil, fail(c, "%s() needs the statement's planner", c.Func())
		}
		return a.in.Call(c, args)
	}
	i, ok := a.at[c]
	if !ok {
		if plant != "query/aggregates-as-compiled" {
			return nil, fail(c, "misuse of aggregate function %s()", c.Name.Name)
		}
		var err error
		if args, err = compileArgs(c, a.in); err != nil {
			return nil, err
		}
		i = len(a.calls)
		a.at[c] = i
		a.calls = append(a.calls, c)
		a.args = append(a.args, args)
	}
	return func(f *Frame) (value.Value, error) {
		row, ok := rowOf(f)
		if !ok {
			return null, fail(c, "%s() has no row of aggregates to be read from", c.Func())
		}
		return row[i], nil
	}, nil
}

// Aggregate works out a's calls over every row of in, and hands on exactly
// one row, even when in has none: each call's result, in a's order. For
// each row it works out each call's arguments in turn, with the
// statement's Frame f, and steps the call. Then it finishes each call in
// turn, where sum() past 64 bits raises "integer overflow", as SQLite's
// OP_AggFinal does before any result column is worked out. It does all its
// work at its first Next, so behind a Limit that skips its row, the result
// columns in the Project after it aren't worked out, though its errors
// still show, as in SQLite.
func Aggregate(in Rows, a *Aggregates, f *Frame) Rows {
	x := &aggRows{in: in, a: a, frame: *f}
	x.frame.Row = &x.cur
	return x
}

type aggRows struct {
	in    Rows
	a     *Aggregates
	frame Frame
	cur   []value.Value
	out   []value.Value
	err   error
	given bool
	done  bool
}

func (x *aggRows) Next() bool {
	if x.done || x.given {
		x.done = true
		return false
	}
	x.given = true
	if err := x.run(); err != nil {
		x.err, x.done = err, true
		return false
	}
	return true
}

func (x *aggRows) run() error {
	accs := make([]accumulator, len(x.a.calls))
	for i, c := range x.a.calls {
		accs[i] = newAccumulator(c)
	}
	vals := make([][]value.Value, len(x.a.calls))
	for i := range vals {
		vals[i] = make([]value.Value, len(x.a.args[i]))
	}
	for x.in.Next() {
		x.cur = x.in.Row()
		for i, args := range x.a.args {
			for j, ev := range args {
				v, err := ev(&x.frame)
				if err != nil {
					return err
				}
				vals[i][j] = v
			}
			accs[i].step(vals[i])
		}
	}
	if err := x.in.Err(); err != nil {
		return err
	}
	x.out = make([]value.Value, len(accs))
	for i, acc := range accs {
		v, err := acc.final()
		if err != nil {
			return err
		}
		x.out[i] = v
	}
	return nil
}

func (x *aggRows) Row() []value.Value { return x.out }
func (x *aggRows) Err() error         { return x.err }

func (x *aggRows) Close() {
	x.done = true
	x.in.Close()
}

// An accumulator is one aggregate call's state over the rows.
type accumulator interface {
	// step takes one row's arguments.
	step(args []value.Value)
	// final gives the result, or an *Error, as SQLite's OP_AggFinal does.
	final() (value.Value, error)
}

func newAccumulator(c *Call) accumulator {
	switch c.Func() {
	case "count":
		return &countAcc{star: c.Star}
	case "min":
		return &pickAcc{}
	case "max":
		return &pickAcc{max: true}
	}
	return &sumAcc{call: c}
}

// countAcc is countStep: count(*) counts every row, and count(x) the rows
// where x isn't NULL.
type countAcc struct {
	star bool
	n    int64
}

func (c *countAcc) step(args []value.Value) {
	if c.star || !args[0].IsNull() {
		c.n++
	}
}

func (c *countAcc) final() (value.Value, error) { return value.Int(c.n), nil }

// pickAcc is minmaxStep: the smallest or the largest value that isn't
// NULL, in compare's order, given back as it is. A value replaces the one
// kept only when it comes strictly before it for min() or after it for
// max(), so on a tie the earliest row's value stays.
type pickAcc struct {
	max  bool
	best value.Value
	have bool
}

func (p *pickAcc) step(args []value.Value) {
	v := args[0]
	if v.IsNull() {
		return
	}
	if !p.have {
		p.best, p.have = v, true
		return
	}
	c := compare(p.best, v)
	if p.max && (c < 0 || c == 0 && plant == "query/max-later-tie-wins") || !p.max && c > 0 {
		p.best = v
	}
}

func (p *pickAcc) final() (value.Value, error) { return p.best, nil }

// sumAcc is SQLite's SumCtx, for sum(), total() and avg(). It adds whole
// numbers exactly while every value is one and the sum fits in 64 bits.
// From the first value that isn't, or the first sum that doesn't fit, it
// adds reals with Kahan-Babuska-Neumaier summation, in the order of the
// rows, with the error term kept apart. A value counts as sumStep's
// sqlite3_value_numeric_type makes it: text that all reads as a number is
// that number, a whole number when it's written as one, and any other
// text, and bytes, count as a real, read as CAST to REAL reads them.
type sumAcc struct {
	call       *Call // sum(), total() or avg()
	rSum, rErr float64
	iSum       int64
	cnt        int64
	approx     bool // a value that isn't a whole number has come, or the whole numbers overflowed
	ovrfl      bool // the whole numbers overflowed, and no real has come since

	held []value.Value // the values, for query/sum-in-reverse only
}

func (s *sumAcc) step(args []value.Value) {
	if plant == "query/sum-in-reverse" {
		if !args[0].IsNull() {
			s.held = append(s.held, args[0])
		}
		return
	}
	s.add(args[0])
}

func (s *sumAcc) add(v value.Value) {
	switch kind(v) {
	case value.KindNull:
		return
	case value.KindText:
		v = numericAffinity(v)
		if plant == "query/sum-text-as-real" && v.Kind() == value.KindInt {
			v = value.Real(float64(v.Int()))
		}
	}
	s.cnt++
	isInt := v.Kind() == value.KindInt
	switch {
	case !s.approx && isInt:
		if sum, ok := addInt(s.iSum, v.Int()); ok || plant == "query/sum-wraps" {
			s.iSum = sum
			return
		}
		s.ovrfl = true
		s.start()
		s.stepInt(v.Int())
	case !s.approx:
		s.start()
		s.stepReal(realOf(v))
	case isInt:
		s.stepInt(v.Int())
	default:
		s.ovrfl = false
		s.stepReal(realOf(v))
	}
}

// addInt is sqlite3AddInt64: a + b, and false when it doesn't fit.
func addInt(a, b int64) (int64, bool) {
	s := a + b
	return s, (s > a) == (b > 0)
}

// start is kahanBabuskaNeumaierInit, from the whole number sum so far.
func (s *sumAcc) start() {
	s.approx = true
	s.rSum, s.rErr = splitInt(s.iSum)
}

// splitInt splits a whole number into a real that holds it to a multiple
// of 16384 and the real of the rest, when it's 2^52 or more in size, so
// that each is exact. A smaller one is the real alone.
func splitInt(i int64) (big, small float64) {
	if i <= -4503599627370496 || i >= 4503599627370496 {
		rest := i % 16384
		return float64(i - rest), float64(rest)
	}
	return float64(i), 0
}

// stepInt is kahanBabuskaNeumaierStepInt64.
func (s *sumAcc) stepInt(i int64) {
	big, small := splitInt(i)
	s.stepReal(big)
	if i <= -4503599627370496 || i >= 4503599627370496 {
		s.stepReal(small)
	}
}

// stepReal is kahanBabuskaNeumaierStep. Go keeps float64 sums in float64
// and doesn't reorder them, so it gives the bits SQLite's volatile doubles
// do.
func (s *sumAcc) stepReal(r float64) {
	t := s.rSum + r
	switch {
	case plant == "query/sum-uncompensated":
	case math.Abs(s.rSum) > math.Abs(r):
		s.rErr += (s.rSum - t) + r
	default:
		s.rErr += (r - t) + s.rSum
	}
	s.rSum = t
}

// real is the sum as a real: the running sum, with the error term added
// unless it's infinite or NaN.
func (s *sumAcc) real() float64 {
	if !s.approx {
		return float64(s.iSum)
	}
	if math.IsInf(s.rErr, 0) || math.IsNaN(s.rErr) {
		return s.rSum
	}
	return s.rSum + s.rErr
}

func (s *sumAcc) final() (value.Value, error) {
	if plant == "query/sum-in-reverse" {
		for _, v := range slices.Backward(s.held) {
			s.add(v)
		}
		s.held = nil
	}
	switch s.call.Func() {
	case "total":
		return realResult(s.real()), nil
	case "avg":
		if s.cnt == 0 {
			return null, nil
		}
		return realResult(s.real() / float64(s.cnt)), nil
	}
	switch {
	case s.cnt == 0:
		return null, nil
	case s.approx && s.ovrfl:
		return null, fail(s.call, "integer overflow")
	case s.approx:
		return realResult(s.real()), nil
	}
	return value.Int(s.iSum), nil
}

// realResult is sqlite3_result_double: a real, or NULL for NaN, so a sum
// of infinities of both signs is NULL.
func realResult(r float64) value.Value {
	if math.IsNaN(r) {
		return null
	}
	return value.Real(r)
}
