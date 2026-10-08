// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"fmt"
	"math"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The evaluator (Q1): it compiles an expression from Parse's tree once for
// each statement, into an Eval that works it out for each row, and gives a
// value or an error of SQL.md's kind. It follows SQLite's code generator as
// well as its rules for values, since the order in which SQLite works out
// the parts of an expression, and the parts it skips, show in which error a
// statement gives.
//
// Fields, the one-record subquery, walks and the functions that read the
// store come from the statement's sources, so the planner hands the
// compiler a Scope for them (Q4, Q5). An expression with none of them
// compiles with a nil Scope. date() and datetime() read the statement's
// moment from the Frame (date.go).

// Frame is what a compiled expression reads as it's worked out.
type Frame struct {
	// Args are the statement's arguments, one for each ? mark in order, as
	// the driver made them of the caller's Go values (SQL.md, "Arguments").
	// A real that's NaN reads as NULL, as SQLite binds it.
	Args []value.Value
	// Row is the row being worked on, in the planner's own form. Only the
	// Evals a Scope gives read it.
	Row any
	// Now is the statement's moment, which 'now' in date() and datetime()
	// stands for. Whoever runs the statement reads it once, with time.Now,
	// before the first row, and every row gets the same one (SQL.md,
	// "Dates"). Dates are in UTC, so its zone doesn't count, and only whole
	// milliseconds do, as in SQLite's clock. The zero time, the first moment
	// of year 1, which no clock gives, stands for no moment, and a date on
	// it is an error.
	Now time.Time
}

// Eval works out a compiled expression for the row in f. It may be called
// from several goroutines at once, each with a Frame of its own.
type Eval func(f *Frame) (value.Value, error)

// Cond works out a compiled condition for the row in f, as WHERE takes it:
// true keeps the row, and false and NULL leave it out.
type Cond func(f *Frame) (bool, error)

// RecordEval works out the one-record subquery for the row in f: the
// field's value, NULL when the record has no value for it, and whether the
// record is there at all.
type RecordEval func(f *Frame) (v value.Value, found bool, err error)

// Scope finds what the names and subqueries in an expression stand for,
// which the evaluator can't work out itself. The planner gives one for a
// statement's sources (Q5), and the aggregates' operator its calls (Q4).
type Scope interface {
	// Column returns what reads the field c names in the current row.
	// Every field compares as it is, with SQLite's BLOB affinity, as
	// SQL.md's "Conversion before comparing" has it. The vector field may
	// give a value of kind KindVector, which reads as bytes.
	Column(c *Column) (Eval, error)
	// Record returns what works out the one-record subquery r, given what
	// works out its key. It compares like a field. Whether the record is
	// there counts only in x IN ((SELECT ...)), which SQLite's parser
	// makes IN over the subquery's rows. SQL.md has the subquery worked
	// out once for each statement, which is the Scope's to keep to.
	Record(r *Record, key Eval) (RecordEval, error)
	// Walk returns what lists the keys the walk w reaches, as text, for IN
	// over a walk, given what works out the walk's arguments, once for
	// each statement too.
	Walk(w *Walk, args []Eval) (func(f *Frame) ([]value.Value, error), error)
	// Call returns what works out a call the evaluator leaves to others:
	// walk() and the aggregates, given what works out the call's
	// arguments.
	Call(c *Call, args []Eval) (Eval, error)
}

// FuncError is distance() or vector() refusing its arguments, and walk()
// once the planner has it. In a SELECT it's of kind "error"; in an INSERT,
// UPDATE or DELETE it's "invalid", as SQL.md's "Errors and their kinds"
// has it, so a write wraps it in errs.ErrInvalid (Q6).
type FuncError struct {
	Pos  int    // the byte in the text where the call starts
	Func string // the function, in lower case
	Msg  string // what it refused, as 0.x says it, without "hypercrux: "
}

func (e *FuncError) Error() string { return "hypercrux: " + e.Msg }

// Compile compiles e, an expression from Parse's tree, into an Eval, with
// s for its names and subqueries. Other errors, raised as the Eval runs,
// are *Error, as SQLite raises them: "integer overflow" from abs(), LIKE's
// errors, and "string or blob too big". Each is of kind "error".
//
// A date() or datetime() anywhere in e that the subset doesn't take is
// refused first, in the parts the compiler leaves out too (checkDates).
func Compile(e Expr, s Scope) (Eval, error) {
	if err := checkDates(e); err != nil {
		return nil, err
	}
	return newCompiler(e, s).expr(e)
}

// CompileCondition compiles e as a condition, as SQLite's
// sqlite3ExprIfFalse codes a WHERE: AND stops at the first operand that's
// false or NULL, OR at the first that's true, and NOT, BETWEEN and the
// comparisons go the same way inside them, so a part that can't change
// the answer isn't worked out. How a WHERE is split into conditions, and
// their order, is the planner's. Dates are refused first, as in Compile.
func CompileCondition(e Expr, s Scope) (Cond, error) {
	if err := checkDates(e); err != nil {
		return nil, err
	}
	j, err := newCompiler(e, s).ifFalse(e, true)
	if err != nil {
		return nil, err
	}
	return func(f *Frame) (bool, error) {
		skip, err := j(f)
		return !skip, err
	}, nil
}

type compiler struct {
	scope Scope
	// sub holds which expressions hold a subquery, when e does at all;
	// otherwise it's nil and none does.
	sub map[Expr]bool
}

func newCompiler(e Expr, s Scope) *compiler {
	c := &compiler{scope: s}
	if hasSubquery(e) {
		c.sub = map[Expr]bool{}
	}
	return c
}

// hasSub is hasSubquery for the expressions inside the one being compiled,
// each worked out once.
func (c *compiler) hasSub(e Expr) bool {
	if c.sub == nil {
		return false
	}
	v, ok := c.sub[e]
	if !ok {
		v = hasSubquery(e)
		c.sub[e] = v
	}
	return v
}

// fail makes the error an Eval returns for e.
func fail(e Expr, format string, args ...any) error {
	return &Error{Pos: e.Pos(), Msg: fmt.Sprintf(format, args...)}
}

func constant(v value.Value) Eval {
	return func(*Frame) (value.Value, error) { return v, nil }
}

// exprAffinity is sqlite3ExprAffinity: a field's, the one-record
// subquery's or a CAST's, and none for any other expression. Parentheses
// keep an operand's affinity, as they leave no node, and a plus sign takes
// it away.
func exprAffinity(e Expr) affinity {
	switch e := e.(type) {
	case *Column, *Record:
		return affBlob
	case *Cast:
		return castAffinity(e.Type)
	case *Unary:
		if plant == "query/plus-keeps-affinity" && e.Op == OpPlus {
			return exprAffinity(e.X)
		}
	}
	return 0
}

// canBeNull is sqlite3ExprCanBeNull: false for a literal other than NULL,
// with any signs in front of it, and true for anything else. An IS NULL
// that SQLite's parser has made a literal counts as one.
func canBeNull(e Expr) bool {
	e = unsign(e)
	if l, ok := e.(*Literal); ok {
		return l.Kind == LitNull
	}
	_, folded := foldedIsNull(e)
	return !folded
}

// foldedIsNull is sqlite3PExprIsNull: x IS NULL and x IS NOT NULL, when x,
// after any signs in front of it, is a literal other than NULL, become the
// integer literal 0 or 1 as SQLite parses them, which can be x of another.
// It gives that literal's truth.
func foldedIsNull(e Expr) (bool, bool) {
	b, ok := e.(*Binary)
	if !ok || b.Op != OpIs && b.Op != OpIsNot {
		return false, false
	}
	if r, ok := b.R.(*Literal); !ok || r.Kind != LitNull {
		return false, false
	}
	x := unsign(b.L)
	if l, ok := x.(*Literal); ok && l.Kind != LitNull {
		return b.Op == OpIsNot, true
	}
	if _, ok := foldedIsNull(x); ok {
		return b.Op == OpIsNot, true
	}
	return false, false
}

// hasSubquery is SQLite's EP_Subquery: whether e holds the one-record
// subquery or IN over a walk.
func hasSubquery(e Expr) bool {
	found := false
	walkExpr(e, func(e Expr) bool {
		switch e := e.(type) {
		case *Record:
			found = true
		case *In:
			if e.Walk != nil {
				found = true
			}
		}
		return !found
	})
	return found
}

// alwaysTrue and alwaysFalse are SQLite's EP_IsTrue and EP_IsFalse, which
// its parser sets on what it knows the truth of before anything runs:
//
//   - an integer literal that fits in 32 bits, as sqlite3GetInt32 reads it,
//     without any sign in front of it, true when it isn't 0;
//   - x IN (), which is false, and x NOT IN (), which is true;
//   - a literal other than NULL IS NULL, which is false, and IS NOT NULL,
//     which is true, signs in front of the literal and all, as
//     sqlite3PExprIsNull makes them 0 and 1.
func alwaysTrue(e Expr) bool {
	v, ok := truthKnown(e)
	return ok && v
}

func alwaysFalse(e Expr) bool {
	v, ok := truthKnown(e)
	return ok && !v
}

func truthKnown(e Expr) (bool, bool) {
	switch e := e.(type) {
	case *Literal:
		if e.Kind != LitInt {
			return false, false
		}
		s := e.Text
		for len(s) > 1 && s[0] == '0' {
			s = s[1:]
		}
		if len(s) > 10 {
			return false, false
		}
		var v int64
		for i := 0; i < len(s); i++ {
			v = v*10 + int64(s[i]-'0')
		}
		if v > math.MaxInt32 {
			return false, false
		}
		return v != 0, true
	case *In:
		if e.Walk == nil && len(e.List) == 0 {
			return e.Not, true
		}
	case *Binary:
		return foldedIsNull(e)
	}
	return false, false
}

// truthTest reports whether e is SQLite's TK_TRUTH, x IS TRUE, x IS FALSE,
// x IS NOT TRUE or x IS NOT FALSE, which its resolver makes of IS and IS
// NOT with a truth on the right. In the subset that's an empty IN list
// whose left side calls no function, which SQLite's parser makes FALSE, or
// TRUE for NOT IN, so x IS (y NOT IN ()) is x IS TRUE. With a call in y,
// the parser makes y IN () FALSE AND y instead, and y NOT IN () TRUE OR y,
// in case the call is an aggregate, and IS stays IS. It gives the truth on
// the right.
func truthTest(e *Binary) (isTrue, ok bool) {
	if e.Op != OpIs && e.Op != OpIsNot {
		return false, false
	}
	if in, ok := e.R.(*In); ok && in.Walk == nil && len(in.List) == 0 && !hasCall(in.X) {
		return in.Not, true
	}
	return false, false
}

// hasCall is SQLite's EP_HasFunc: whether e calls a function, LIKE
// included, outside any subquery.
func hasCall(e Expr) bool {
	found := false
	walkExpr(e, func(e Expr) bool {
		switch e.(type) {
		case *Call, *Like:
			found = true
		}
		return !found
	})
	return found
}

// constantExpr is sqlite3ExprIsConstant for the subset: an expression
// without fields, subqueries, walk() or aggregates. Arguments count as
// constant, as in SQLite.
func constantExpr(e Expr) bool {
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
			if e.Aggregate() || e.Func() == "walk" {
				ok = false
			}
		}
		return ok
	})
	return ok
}

// simplify is sqlite3ExprSimplifiedAndOr: when one side of an AND or an OR
// settles it, as alwaysTrue and alwaysFalse say, the expression is the
// other side or that one, and the side that can't change the answer isn't
// worked out. 0 AND x and x AND 0 are 0, 1 OR x and x OR 1 are 1, and 1 AND
// x, x AND 1, 0 OR x and x OR 0 are x's truth. It looks inside the sides
// first, so (x AND 0) OR y is y.
func simplify(e Expr) Expr {
	b, ok := e.(*Binary)
	if !ok || b.Op != OpAnd && b.Op != OpOr {
		return e
	}
	r := simplify(b.R)
	l := simplify(b.L)
	and := b.Op == OpAnd
	if plant == "query/and-or-sees-signs" {
		l, r = unsign(l), unsign(r)
	}
	switch {
	case alwaysTrue(l) || alwaysFalse(r):
		if and {
			return r
		}
		return l
	case alwaysTrue(r) || alwaysFalse(l):
		if and {
			return l
		}
		return r
	}
	return e
}

// unsign takes the signs off an expression: the - and + in front of it.
func unsign(e Expr) Expr {
	for {
		u, ok := e.(*Unary)
		if !ok || u.Op == OpNot {
			return e
		}
		e = u.X
	}
}

// literal is a literal's value. neg says a minus sign is in front of it,
// which negates a number as written (SQL.md, "A sign before a literal"):
// an integer too big for 64 bits is a real, apart from
// -9223372036854775808, and a real reads as value.ParseReal reads it.
func literal(l *Literal, neg bool) value.Value {
	switch l.Kind {
	case LitText:
		return value.Text(l.Data)
	case LitBytes:
		return value.Bytes(l.Data)
	case LitInt:
		i, c := value.ParseInt(l.Text)
		switch {
		case c == 0 && neg:
			return value.Int(-i)
		case c == 0:
			return value.Int(i)
		case c == 3 && neg:
			return value.Int(math.MinInt64)
		}
		fallthrough
	case LitReal:
		r, _ := value.ParseReal(l.Text)
		if neg && plant != "query/minus-zero-lost" {
			r = -r
		} else if neg {
			r = 0 - r
		}
		return value.Real(r)
	}
	return null
}

// expr compiles an expression to work out its value.
func (c *compiler) expr(e Expr) (Eval, error) {
	switch e := e.(type) {
	case *Literal:
		return constant(literal(e, false)), nil
	case *Param:
		i := e.Index
		return func(f *Frame) (value.Value, error) {
			if i >= len(f.Args) {
				return null, fail(e, "not enough arguments: the statement has a ? mark for argument %d", i+1)
			}
			v := f.Args[i]
			if v.Kind() == value.KindReal && math.IsNaN(v.Real()) {
				return null, nil
			}
			return v, nil
		}, nil
	case *Column:
		if c.scope == nil {
			return nil, fail(e, "no such column: %s", e)
		}
		return c.scope.Column(e)
	case *Record:
		rec, err := c.record(e)
		if err != nil {
			return nil, err
		}
		return func(f *Frame) (value.Value, error) {
			v, _, err := rec(f)
			return v, err
		}, nil
	case *Call:
		return c.call(e)
	case *Cast:
		x, err := c.expr(e.X)
		if err != nil {
			return nil, err
		}
		to := e.Type
		return func(f *Frame) (value.Value, error) {
			v, err := x(f)
			if err != nil {
				return null, err
			}
			return cast(v, to), nil
		}, nil
	case *Unary:
		return c.unary(e)
	case *Binary:
		return c.binary(e)
	case *Between:
		return c.between(e)
	case *Like:
		return c.like(e)
	case *In:
		return c.in(e)
	}
	return nil, fail(e, "%T is no expression the evaluator knows", e)
}

// record compiles the one-record subquery through the scope.
func (c *compiler) record(e *Record) (RecordEval, error) {
	if c.scope == nil {
		return nil, fail(e, "no such table: %s", e.Table.Name)
	}
	key, err := c.expr(e.Key)
	if err != nil {
		return nil, err
	}
	return c.scope.Record(e, key)
}

func (c *compiler) unary(e *Unary) (Eval, error) {
	if l, ok := e.X.(*Literal); ok && e.Op == OpNeg && (l.Kind == LitInt || l.Kind == LitReal) {
		return constant(literal(l, true)), nil
	}
	x, err := c.expr(e.X)
	if err != nil {
		return nil, err
	}
	switch e.Op {
	case OpNeg:
		// -x is 0 - x, as SQLite codes it.
		zero := value.Int(0)
		return func(f *Frame) (value.Value, error) {
			v, err := x(f)
			if err != nil {
				return null, err
			}
			return arith(OpSub, zero, v), nil
		}, nil
	case OpPlus:
		return x, nil
	}
	// NOT
	return func(f *Frame) (value.Value, error) {
		v, err := x(f)
		if err != nil {
			return null, err
		}
		switch truth(v) {
		case 0:
			return value.Int(1), nil
		case 1:
			return value.Int(0), nil
		}
		return null, nil
	}, nil
}

// operands works out a binary operator's operands, l and r compiled from
// e.L and e.R, in SQLite's order (exprComputeOperands). When one side holds
// a subquery and the other doesn't, the other goes first, and when it's
// NULL the subquery isn't worked out and the result is NULL, which op
// gives anyway: skip reports it. IS and IS NOT take their operands in
// order, since NULL doesn't settle them.
func (c *compiler) operands(e *Binary, l, r Eval) func(f *Frame) (a, b value.Value, skip bool, err error) {
	sl, sr := c.hasSub(e.L), c.hasSub(e.R)
	nullEq := e.Op == OpIs || e.Op == OpIsNot
	switch {
	case !nullEq && sl && !sr && canBeNull(e.R):
		return func(f *Frame) (value.Value, value.Value, bool, error) {
			b, err := r(f)
			if err != nil || b.IsNull() {
				return null, null, true, err
			}
			a, err := l(f)
			return a, b, false, err
		}
	case !nullEq && sr && canBeNull(e.L):
		return func(f *Frame) (value.Value, value.Value, bool, error) {
			a, err := l(f)
			if err != nil || a.IsNull() {
				return null, null, true, err
			}
			b, err := r(f)
			return a, b, false, err
		}
	}
	return func(f *Frame) (value.Value, value.Value, bool, error) {
		a, err := l(f)
		if err != nil {
			return null, null, false, err
		}
		b, err := r(f)
		return a, b, false, err
	}
}

func (c *compiler) binary(e *Binary) (Eval, error) {
	if e.Op == OpAnd || e.Op == OpOr {
		return c.andOr(e)
	}
	if isTrue, ok := truthTest(e); ok {
		return c.truth(e, isTrue)
	}
	l, err := c.expr(e.L)
	if err != nil {
		return nil, err
	}
	r, err := c.expr(e.R)
	if err != nil {
		return nil, err
	}
	op := e.Op
	var apply func(a, b value.Value) (value.Value, error)
	switch op {
	case OpEq, OpNe, OpIs, OpIsNot, OpLt, OpLe, OpGt, OpGe:
		aff := compareAffinity(exprAffinity(e.L), exprAffinity(e.R))
		if plant == "query/text-reads-as-number" && aff <= affBlob {
			aff = affNumeric
		}
		apply = func(a, b value.Value) (value.Value, error) { return compareOp(op, a, b, aff), nil }
	case OpConcat:
		apply = func(a, b value.Value) (value.Value, error) {
			v, ok := concat(a, b)
			if !ok {
				return null, fail(e, "string or blob too big")
			}
			return v, nil
		}
	default:
		apply = func(a, b value.Value) (value.Value, error) { return arith(op, a, b), nil }
	}
	if !c.hasSub(e) {
		return func(f *Frame) (value.Value, error) {
			a, err := l(f)
			if err != nil {
				return null, err
			}
			b, err := r(f)
			if err != nil {
				return null, err
			}
			return apply(a, b)
		}, nil
	}
	get := c.operands(e, l, r)
	return func(f *Frame) (value.Value, error) {
		a, b, skip, err := get(f)
		if err != nil || skip {
			return null, err
		}
		return apply(a, b)
	}, nil
}

// truth compiles TK_TRUTH as OP_IsTrue works it out: 1 when x's truth is
// the one on the right, with NULL as false, and IS NOT the opposite. It
// never gives NULL, and the right side isn't worked out.
func (c *compiler) truth(e *Binary, isTrue bool) (Eval, error) {
	x, err := c.expr(e.L)
	if err != nil {
		return nil, err
	}
	want := 0
	if isTrue {
		want = 1
	}
	not := e.Op == OpIsNot
	return func(f *Frame) (value.Value, error) {
		v, err := x(f)
		if err != nil {
			return null, err
		}
		return boolValue((truth(v) == want) != not), nil
	}, nil
}

// andOr compiles AND and OR, as exprCodeTargetAndOr codes them: simplified
// first, then both sides worked out, apart from a side holding a subquery,
// which waits for the other and is skipped when that settles the answer.
func (c *compiler) andOr(e *Binary) (Eval, error) {
	if alt := simplify(e); alt != e {
		x, err := c.expr(alt)
		if err != nil {
			return nil, err
		}
		return func(f *Frame) (value.Value, error) {
			v, err := x(f)
			if err != nil {
				return null, err
			}
			return truthValue(truth(v)), nil
		}, nil
	}
	l, err := c.expr(e.L)
	if err != nil {
		return nil, err
	}
	r, err := c.expr(e.R)
	if err != nil {
		return nil, err
	}
	table, settles := &andTruth, 0 // false settles AND
	if e.Op == OpOr {
		table, settles = &orTruth, 1 // true settles OR
	}
	first, second := l, r
	skip := false
	switch sl, sr := c.hasSub(e.L), c.hasSub(e.R); {
	case sl && !sr:
		first, second, skip = r, l, true
	case sr:
		skip = true
	}
	return func(f *Frame) (value.Value, error) {
		v, err := first(f)
		if err != nil {
			return null, err
		}
		a := truth(v)
		if skip && a == settles {
			return value.Int(int64(settles)), nil
		}
		v, err = second(f)
		if err != nil {
			return null, err
		}
		return truthValue(table[a*3+truth(v)]), nil
	}, nil
}

// between compiles x BETWEEN low AND high as x >= low AND x <= high, with x
// worked out once, then low, then high, each comparison with its own
// affinity. NOT BETWEEN is NOT of it.
func (c *compiler) between(e *Between) (Eval, error) {
	x, lo, hi, affLo, affHi, err := c.betweenParts(e)
	if err != nil {
		return nil, err
	}
	not := e.Not
	return func(f *Frame) (value.Value, error) {
		xv, err := x(f)
		if err != nil {
			return null, err
		}
		lv, err := lo(f)
		if err != nil {
			return null, err
		}
		hv, err := hi(f)
		if err != nil {
			return null, err
		}
		t := andTruth[truth(compareOp(OpGe, xv, lv, affLo))*3+truth(compareOp(OpLe, xv, hv, affHi))]
		if not && t != 2 {
			t = 1 - t
		}
		return truthValue(t), nil
	}, nil
}

func (c *compiler) betweenParts(e *Between) (x, lo, hi Eval, affLo, affHi affinity, err error) {
	if x, err = c.expr(e.X); err != nil {
		return
	}
	if lo, err = c.expr(e.Low); err != nil {
		return
	}
	if hi, err = c.expr(e.High); err != nil {
		return
	}
	ax := exprAffinity(e.X)
	return x, lo, hi, compareAffinity(ax, exprAffinity(e.Low)), compareAffinity(ax, exprAffinity(e.High)), nil
}

// like compiles x LIKE p ESCAPE e as SQLite does, as the function like(p,
// x, e): the pattern is worked out first, then x, then the escape.
func (c *compiler) like(e *Like) (Eval, error) {
	x, err := c.expr(e.X)
	if err != nil {
		return nil, err
	}
	p, err := c.expr(e.Pattern)
	if err != nil {
		return nil, err
	}
	var esc Eval
	if e.Escape != nil {
		if esc, err = c.expr(e.Escape); err != nil {
			return nil, err
		}
	}
	not := e.Not
	return func(f *Frame) (value.Value, error) {
		pv, err := p(f)
		if err != nil {
			return null, err
		}
		xv, err := x(f)
		if err != nil {
			return null, err
		}
		ev := null
		if esc != nil {
			if ev, err = esc(f); err != nil {
				return null, err
			}
		}
		t, err := likeTruth(e, pv, xv, ev, esc != nil)
		if err != nil {
			return null, err
		}
		if not && t != 2 {
			t = 1 - t
		}
		return truthValue(t), nil
	}, nil
}

// in compiles x IN (...) and x NOT IN (...).
func (c *compiler) in(e *In) (Eval, error) {
	if e.Walk == nil && len(e.List) == 0 {
		// SQLite's parser makes x IN () false and x NOT IN () true, and x
		// isn't worked out.
		return constant(boolValue(e.Not)), nil
	}
	x, err := c.expr(e.X)
	if err != nil {
		return nil, err
	}
	not := e.Not
	finish := func(t int) value.Value {
		if not && t != 2 {
			t = 1 - t
		}
		return truthValue(t)
	}
	if e.Walk != nil {
		return c.inWalk(e, x, finish)
	}
	if r, ok := e.List[0].(*Record); ok && len(e.List) == 1 && plant != "query/in-record-is-a-list" {
		return c.inRecord(e, r, x, finish)
	}
	if len(e.List) == 1 && constantExpr(e.List[0]) {
		// SQLite's parser makes x IN (y), with y constant, x = +y.
		eq, err := c.expr(&Binary{At: e.At, OpAt: e.At, Op: OpEq, L: e.X, R: &Unary{At: e.List[0].Pos(), Op: OpPlus, X: e.List[0]}})
		if err != nil {
			return nil, err
		}
		return func(f *Frame) (value.Value, error) {
			v, err := eq(f)
			if err != nil {
				return null, err
			}
			return finish(truth(v)), nil
		}, nil
	}
	items := make([]Eval, len(e.List))
	for i, it := range e.List {
		if items[i], err = c.expr(it); err != nil {
			return nil, err
		}
	}
	aff := exprAffinity(e.X)
	if plant == "query/in-takes-item-affinity" {
		aff = compareAffinity(exprAffinity(e.X), exprAffinity(e.List[0]))
	}
	whole := len(items) > 2
	for _, it := range e.List {
		whole = whole && constantExpr(it)
	}
	if whole && plant != "query/in-list-stops-early" {
		// A list of three constant items or more goes into an ephemeral
		// table first, as sqlite3FindInIndex chooses, so every item is
		// worked out, before x.
		return func(f *Frame) (value.Value, error) {
			vs := make([]value.Value, len(items))
			for i, it := range items {
				var err error
				if vs[i], err = it(f); err != nil {
					return null, err
				}
			}
			xv, err := x(f)
			if err != nil {
				return null, err
			}
			sawNull := xv.IsNull()
			for _, iv := range vs {
				if iv.IsNull() {
					sawNull = true
				} else if c, ok := compareWith(xv, iv, aff); ok && c == 0 {
					return finish(1), nil
				}
			}
			if sawNull {
				return finish(2), nil
			}
			return finish(0), nil
		}, nil
	}
	return func(f *Frame) (value.Value, error) {
		xv, err := x(f)
		if err != nil {
			return null, err
		}
		sawNull := xv.IsNull()
		for _, it := range items {
			iv, err := it(f)
			if err != nil {
				return null, err
			}
			if iv.IsNull() {
				sawNull = true
				continue
			}
			if c, ok := compareWith(xv, iv, aff); ok && c == 0 {
				return finish(1), nil
			}
		}
		if sawNull {
			return finish(2), nil
		}
		return finish(0), nil
	}, nil
}

// inWalk compiles x IN over a walk, as SQLite codes IN over a subquery:
// the walk first, once its arguments are worked out, then x. The keys are
// text, and x compares with them as it is: it's 1 when x is text equal to a
// key. When x is NULL, it's NULL, or 0 when the walk reaches nothing.
func (c *compiler) inWalk(e *In, x Eval, finish func(int) value.Value) (Eval, error) {
	if c.scope == nil {
		return nil, fail(e, "no such table: walk")
	}
	args := make([]Eval, len(e.Walk.Args))
	for i, a := range e.Walk.Args {
		var err error
		if args[i], err = c.expr(a); err != nil {
			return nil, err
		}
	}
	keys, err := c.scope.Walk(e.Walk, args)
	if err != nil {
		return nil, err
	}
	return func(f *Frame) (value.Value, error) {
		ks, err := keys(f)
		if err != nil {
			return null, err
		}
		xv, err := x(f)
		if err != nil {
			return null, err
		}
		switch {
		case xv.IsNull() && len(ks) == 0:
			return finish(0), nil
		case xv.IsNull():
			return finish(2), nil
		case xv.Kind() != value.KindText:
			return finish(0), nil
		}
		for _, k := range ks {
			if k.Kind() == value.KindText && k.Raw() == xv.Raw() {
				return finish(1), nil
			}
		}
		return finish(0), nil
	}, nil
}

// inRecord compiles x IN ((SELECT f FROM t WHERE key = k)), which SQLite's
// parser makes IN over the subquery: a set of the field's value, or an
// empty one when there's no such record. As with a walk, the subquery goes
// first, then x, and an empty set gives 0 even when x is NULL. The field's
// affinity counts beside x's, as exprINAffinity has it, so text becomes a
// number only when x is a CAST to INTEGER, REAL or NUMERIC, and a CAST to
// TEXT changes nothing.
func (c *compiler) inRecord(e *In, r *Record, x Eval, finish func(int) value.Value) (Eval, error) {
	rec, err := c.record(r)
	if err != nil {
		return nil, err
	}
	aff := compareAffinity(affBlob, exprAffinity(e.X))
	return func(f *Frame) (value.Value, error) {
		v, found, err := rec(f)
		if err != nil {
			return null, err
		}
		xv, err := x(f)
		if err != nil {
			return null, err
		}
		switch {
		case !found:
			return finish(0), nil
		case xv.IsNull() || v.IsNull():
			return finish(2), nil
		}
		if c, ok := compareWith(xv, v, aff); ok && c == 0 {
			return finish(1), nil
		}
		return finish(0), nil
	}, nil
}

// A jump is a condition compiled as sqlite3ExprIfTrue or sqlite3ExprIfFalse
// codes it: it reports whether the jump is taken.
type jump func(f *Frame) (bool, error)

// truthJump takes the jump when the truth t is want, or NULL when
// jumpIfNull is set.
func truthJump(t int, want int, jumpIfNull bool) bool {
	return t == want || t == 2 && jumpIfNull
}

// ifTrue is sqlite3ExprIfTrue: a jump taken when e is true, or NULL when
// jumpIfNull is set.
func (c *compiler) ifTrue(e Expr, jumpIfNull bool) (jump, error) {
	return c.cond(e, true, jumpIfNull)
}

// ifFalse is sqlite3ExprIfFalse: a jump taken when e is false, or NULL when
// jumpIfNull is set.
func (c *compiler) ifFalse(e Expr, jumpIfNull bool) (jump, error) {
	return c.cond(e, false, jumpIfNull)
}

// cond compiles e as a jump taken when e's truth is want, or NULL when
// jumpIfNull is set: sqlite3ExprIfTrue for want true and
// sqlite3ExprIfFalse for want false.
func (c *compiler) cond(e Expr, want, jumpIfNull bool) (jump, error) {
	wantT := 0
	if want {
		wantT = 1
	}
	switch x := e.(type) {
	case *Binary:
		if isTrue, ok := truthTest(x); ok {
			// sqlite3ExprIfTrue and sqlite3ExprIfFalse on TK_TRUTH: the
			// operand as a condition, with NULL going the way IS NOT
			// sends it.
			not := x.Op == OpIsNot
			if want {
				return c.cond(x.L, isTrue != not, not)
			}
			return c.cond(x.L, isTrue == not, !not)
		}
		switch x.Op {
		case OpAnd, OpOr:
			if alt := simplify(x); alt != e {
				return c.cond(alt, want, jumpIfNull)
			}
			first, second := x.L, x.R
			if c.hasSub(x.L) && !c.hasSub(x.R) {
				first, second = x.R, x.L
			}
			// For AND and true, or OR and false, the first side's
			// opposite skips the second; otherwise either side takes the
			// jump.
			if (x.Op == OpAnd) == want {
				j1, err := c.cond(first, !want, !jumpIfNull)
				if err != nil {
					return nil, err
				}
				j2, err := c.cond(second, want, jumpIfNull)
				if err != nil {
					return nil, err
				}
				return func(f *Frame) (bool, error) {
					if skip, err := j1(f); err != nil || skip {
						return false, err
					}
					return j2(f)
				}, nil
			}
			j1, err := c.cond(first, want, jumpIfNull)
			if err != nil {
				return nil, err
			}
			j2, err := c.cond(second, want, jumpIfNull)
			if err != nil {
				return nil, err
			}
			return func(f *Frame) (bool, error) {
				taken, err := j1(f)
				if err != nil || taken && plant != "query/where-or-works-out-both" {
					return taken, err
				}
				taken2, err := j2(f)
				return taken || taken2, err
			}, nil
		}
		// A comparison goes as in a value, with a skipped operand giving
		// NULL, which takes the jump when jumpIfNull is set, as in SQLite.
	case *Unary:
		if x.Op == OpNot {
			return c.cond(x.X, !want, jumpIfNull)
		}
	case *Between:
		return c.condBetween(x, want, jumpIfNull)
	case *Literal, *In:
		if t, ok := truthKnown(e); ok {
			taken := t == want
			return func(*Frame) (bool, error) { return taken, nil }, nil
		}
	}
	ev, err := c.expr(e)
	if err != nil {
		return nil, err
	}
	return func(f *Frame) (bool, error) {
		v, err := ev(f)
		if err != nil {
			return false, err
		}
		return truthJump(truth(v), wantT, jumpIfNull), nil
	}, nil
}

// condBetween is exprCodeBetween with a jump: x worked out once, then the
// AND of x >= low and x <= high as a condition, so high isn't worked out
// when the first comparison settles the jump. NOT BETWEEN is the jump for
// the opposite.
func (c *compiler) condBetween(e *Between, want, jumpIfNull bool) (jump, error) {
	if e.Not {
		want = !want
	}
	x, lo, hi, affLo, affHi, err := c.betweenParts(e)
	if err != nil {
		return nil, err
	}
	cmp := func(f *Frame, xv value.Value, side Eval, op Op, aff affinity) (int, error) {
		v, err := side(f)
		if err != nil {
			return 0, err
		}
		return truth(compareOp(op, xv, v, aff)), nil
	}
	return func(f *Frame) (bool, error) {
		xv, err := x(f)
		if err != nil {
			return false, err
		}
		t1, err := cmp(f, xv, lo, OpGe, affLo)
		if err != nil {
			return false, err
		}
		if want {
			// IfTrue(AND): IfFalse(x >= low) skips the rest.
			if truthJump(t1, 0, !jumpIfNull) {
				return false, nil
			}
		} else if truthJump(t1, 0, jumpIfNull) {
			// IfFalse(AND): IfFalse(x >= low) takes the jump.
			return true, nil
		}
		t2, err := cmp(f, xv, hi, OpLe, affHi)
		if err != nil {
			return false, err
		}
		if want {
			return truthJump(t2, 1, jumpIfNull), nil
		}
		return truthJump(t2, 0, jumpIfNull), nil
	}, nil
}
