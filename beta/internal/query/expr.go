// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import "strings"

// The levels of SQL.md's "Operators and how they group", loosest first.
// The expression parser climbs them, and builds SQLite's trees by SQL.md's
// rules:
//
//   - a binary operator's right operand takes only operators of tighter
//     levels, so they group left to right;
//   - a prefix operator can start any operand, and its own operand runs on
//     as far as its level allows, whatever the operator before it;
//   - in x BETWEEN a AND b, a takes every operator of level 3 and tighter,
//     and b only those tighter than level 4;
//   - in x LIKE p ESCAPE e, p and e take only operators tighter than level
//     4, and the items of IN (...) are whole expressions.
const (
	levelOr     = 1
	levelAnd    = 2
	levelNot    = 3 // NOT in front of an operand
	levelEq     = 4 // = == != <> IS IS NOT IN NOT IN LIKE NOT LIKE BETWEEN NOT BETWEEN
	levelLt     = 5 // < <= > >=
	levelAdd    = 6 // + -
	levelMul    = 7 // * / %
	levelConcat = 8 // ||
	levelSign   = 9 // - and + in front of an operand, whose operand takes no binary operator
)

// form is the shape of the operator after an operand.
type form uint8

const (
	formNone    form = iota
	formBinary       // op, with a right operand
	formIs           // IS or IS NOT
	formIn           // IN or NOT IN
	formLike         // LIKE or NOT LIKE
	formBetween      // BETWEEN or NOT BETWEEN
)

// expr reads a whole expression.
func (p *parser) expr() Expr { return p.climb(levelOr) }

// exprs reads one or more whole expressions with commas between them.
func (p *parser) exprs() []Expr {
	es := []Expr{p.expr()}
	for p.peek().kind == tComma {
		p.take()
		es = append(es, p.expr())
	}
	return es
}

// climb reads an expression whose binary operators are all of level min or
// tighter.
func (p *parser) climb(min int) Expr {
	p.enter(1)
	defer p.leave(1)
	left := p.prefix()
	for {
		f, op, level, not := p.operator()
		if f == formNone || level < min {
			return left
		}
		opTok := p.take()
		if not {
			p.take() // the operator after NOT
		}
		switch f {
		case formBinary:
			right := p.climb(level + 1)
			left = p.binary(left, op, opTok.pos, right)
		case formIs:
			op = OpIs
			if is(p.peek(), "NOT") {
				p.take()
				op = OpIsNot
			}
			if t := p.peek(); is(t, "DISTINCT") {
				p.outside(t.pos, "IS DISTINCT FROM")
			}
			left = p.binary(left, op, opTok.pos, p.climb(levelLt))
		case formLike:
			l := &Like{At: left.Pos(), Not: not, X: left, Pattern: p.climb(levelLt)}
			h := max(height(l.X), height(l.Pattern))
			if is(p.peek(), "ESCAPE") {
				p.take()
				l.Escape = p.climb(levelLt)
				h = max(h, height(l.Escape))
			}
			l.h = p.tall(l.At, h, not)
			left = l
		case formBetween:
			b := &Between{At: left.Pos(), Not: not, X: left, Low: p.climb(levelNot)}
			p.takeKeyword("AND")
			if plant == "query/between-takes-comparisons" {
				b.High = p.climb(levelEq)
			} else {
				b.High = p.climb(levelLt)
			}
			b.h = p.tall(b.At, max(height(b.X), height(b.Low), height(b.High)), not)
			left = b
		case formIn:
			left = p.in(left, not)
		}
	}
}

// tall works out a node's height from its tallest operand: one more, and
// one more again where SQLite makes the node a NOT over another, as for
// NOT LIKE and NOT BETWEEN. It stops the parse past SQLite's limit.
func (p *parser) tall(at, h int, not bool) int {
	h++
	if not {
		h++
	}
	if h > maxHeight {
		p.fail(at, "Expression tree is too large (maximum depth %d)", maxHeight)
	}
	return h
}

func (p *parser) binary(l Expr, op Op, opAt int, r Expr) *Binary {
	b := &Binary{At: l.Pos(), OpAt: opAt, Op: op, L: l, R: r}
	b.h = p.tall(b.At, max(height(l), height(r)), false)
	return b
}

// operator looks at what follows an operand without taking it, and says
// which operator it is, its level and, for NOT IN, NOT LIKE and NOT
// BETWEEN, that the operator comes after a NOT. SQLite's operators outside
// the subset are refused here, where SQLite would read them as operators.
func (p *parser) operator() (f form, op Op, level int, not bool) {
	t := p.peek()
	switch t.kind {
	case tEq:
		return formBinary, OpEq, levelEq, false
	case tNe:
		return formBinary, OpNe, levelEq, false
	case tLt:
		return formBinary, OpLt, levelLt, false
	case tLe:
		return formBinary, OpLe, levelLt, false
	case tGt:
		return formBinary, OpGt, levelLt, false
	case tGe:
		return formBinary, OpGe, levelLt, false
	case tPlus:
		return formBinary, OpAdd, levelAdd, false
	case tMinus:
		return formBinary, OpSub, levelAdd, false
	case tStar:
		return formBinary, OpMul, levelMul, false
	case tSlash:
		return formBinary, OpDiv, levelMul, false
	case tRem:
		return formBinary, OpRem, levelMul, false
	case tConcat:
		return formBinary, OpConcat, levelConcat, false
	case tBitAnd, tBitOr, tShift, tArrow:
		p.syntax(t)
	case tName:
		w := fold(p.textOf(t))
		if w == "like" {
			return formLike, 0, levelEq, false
		}
		if otherOperators[w] {
			p.outside(t.pos, strings.ToUpper(w))
		}
	case tKeyword:
		switch t.word {
		case "OR":
			return formBinary, OpOr, levelOr, false
		case "AND":
			if plant == "query/and-or-one-level" {
				return formBinary, OpAnd, levelOr, false
			}
			return formBinary, OpAnd, levelAnd, false
		case "IS":
			return formIs, 0, levelEq, false
		case "IN":
			return formIn, 0, levelEq, false
		case "BETWEEN":
			return formBetween, 0, levelEq, false
		case "ISNULL", "NOTNULL":
			p.outsideWhy(t.pos, t.word, "write IS NULL or IS NOT NULL")
		case "COLLATE":
			p.outside(t.pos, "COLLATE")
		case "NOT":
			n := p.peekAt(1)
			switch {
			case is(n, "IN"):
				return formIn, 0, levelEq, true
			case is(n, "BETWEEN"):
				return formBetween, 0, levelEq, true
			case p.isWord(n, "like"):
				return formLike, 0, levelEq, true
			case is(n, "NULL"):
				p.outsideWhy(t.pos, "NOT NULL after an operand", "write IS NOT NULL")
			case n.kind == tName && otherOperators[fold(p.textOf(n))]:
				p.outside(n.pos, "NOT "+strings.ToUpper(p.textOf(n)))
			}
		}
	}
	return formNone, 0, 0, false
}

// in reads the rest of x [NOT] IN (...), after IN.
func (p *parser) in(x Expr, not bool) *In {
	in := &In{At: x.Pos(), Not: not, X: x}
	t := p.peek()
	if t.kind != tLP {
		if t.kind == tName || t.kind == tQuoted {
			p.outside(t.pos, "IN over a table or a table-valued function")
		}
		p.syntax(t)
	}
	p.take()
	h := height(x)
	switch t := p.peek(); {
	case is(t, "SELECT"):
		in.Walk = p.inWalk()
	case p.isWord(t, "with"):
		p.outside(t.pos, "WITH")
	case t.kind == tRP:
	default:
		in.List = p.exprs()
		for _, e := range in.List {
			h = max(h, height(e))
		}
	}
	p.takeKind(tRP)
	// SQLite makes x IN (one item) x == +item, and x NOT IN a NOT over IN,
	// so IN counts two.
	in.h = p.tall(in.At, h+1, false)
	return in
}

// inWalk reads the subquery IN takes: SELECT key FROM walk(...), or
// SELECT value FROM json_each(walk(...)), from its SELECT.
func (p *parser) inWalk() *Walk {
	sel := p.take()
	p.enter(subqueryDepth)
	defer p.leave(subqueryDepth)
	other := func(pos int) {
		p.outsideWhy(pos, "IN over a subquery other than a walk",
			"IN takes (SELECT key FROM walk(...)) or (SELECT value FROM json_each(walk(...)))")
	}
	col := p.peek()
	if col.kind != tName && col.kind != tQuoted || !is(p.peekAt(1), "FROM") {
		other(col.pos)
	}
	p.take()
	p.take()
	src := p.peek()
	if src.kind != tName && src.kind != tQuoted || p.peekAt(1).kind != tLP {
		other(src.pos)
	}
	p.take()
	var w *Walk
	switch fold(p.identText(src)) {
	case "walk":
		w = p.walkArgs(src.pos)
		if fold(p.identText(col)) != "key" {
			other(col.pos)
		}
	case "json_each":
		w = p.eachArgs(src.pos)
		if fold(p.identText(col)) != "value" {
			other(col.pos)
		}
	default:
		other(src.pos)
	}
	if t := p.peek(); t.kind != tRP {
		if t.kind == tEOF {
			p.syntax(t)
		}
		other(sel.pos)
	}
	return w
}

// prefix reads an operand, starting with any operators in front of it.
func (p *parser) prefix() Expr {
	t := p.peek()
	switch {
	case is(t, "NOT"):
		p.take()
		x := p.climb(levelEq)
		return &Unary{At: t.pos, Op: OpNot, X: x, h: p.tall(t.pos, height(x), false)}
	case t.kind == tMinus || t.kind == tPlus:
		p.take()
		op := OpNeg
		if t.kind == tPlus {
			op = OpPlus
		}
		next := levelSign + 1
		if plant == "query/sign-takes-operators" {
			next = levelConcat
		}
		x := p.climb(next)
		// A sign in front of a plus sign's expression takes its place, as
		// SQLite's parser has it: -+x is -x, and +(+x) is +x.
		if u, ok := x.(*Unary); ok && u.Op == OpPlus && plant != "query/plus-kept" {
			x = u.X
		}
		return &Unary{At: t.pos, Op: op, X: x, h: p.tall(t.pos, height(x), false)}
	case t.kind == tBitNot:
		p.syntax(t)
	}
	return p.primary()
}

// primary reads an operand without operators in front of it.
func (p *parser) primary() Expr {
	t := p.peek()
	text := p.textOf(t)
	switch t.kind {
	case tInt:
		p.take()
		return &Literal{At: t.pos, Kind: LitInt, Text: text}
	case tReal:
		p.take()
		return &Literal{At: t.pos, Kind: LitReal, Text: text}
	case tText:
		p.take()
		return &Literal{At: t.pos, Kind: LitText, Text: text, Data: unquote(text)}
	case tBlob:
		p.take()
		return &Literal{At: t.pos, Kind: LitBytes, Text: text, Data: unhex(text)}
	case tParam:
		p.take()
		p.params++
		if p.params > maxParams {
			p.fail(t.pos, "too many SQL variables")
		}
		if plant == "query/params-from-one" {
			return &Param{At: t.pos, Index: p.params}
		}
		return &Param{At: t.pos, Index: p.params - 1}
	case tLP:
		return p.paren()
	case tName, tQuoted:
		return p.name()
	case tKeyword:
		switch t.word {
		case "NULL":
			p.take()
			return &Literal{At: t.pos, Kind: LitNull, Text: text}
		case "CAST":
			return p.cast()
		case "CASE", "EXISTS", "RAISE":
			p.outside(t.pos, t.word)
		case "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP":
			p.outsideWhy(t.pos, t.word, "write date('now') or datetime('now')")
		}
	}
	p.syntax(t)
	return nil
}

// paren reads what starts with a parenthesis: an expression in it, or the
// one-record subquery.
func (p *parser) paren() Expr {
	lp := p.take()
	switch t := p.peek(); {
	case is(t, "SELECT"):
		return p.record(lp.pos)
	case is(t, "VALUES"), p.isWord(t, "with"):
		// SQLite reads a subquery here.
		p.outside(t.pos, strings.ToUpper(p.textOf(t)))
	}
	e := p.expr()
	if t := p.peek(); t.kind == tComma {
		p.outside(lp.pos, "a row value, (a, b, ...),")
	}
	p.takeKind(tRP)
	return e
}

// record reads the one-record subquery, (SELECT field FROM table WHERE key
// = e), from its SELECT. Any other subquery is refused.
func (p *parser) record(at int) *Record {
	sel := p.take()
	p.enter(subqueryDepth)
	defer p.leave(subqueryDepth)
	other := func() {
		p.outsideWhy(sel.pos, "a subquery other than the one-record one",
			"it's (SELECT field FROM table WHERE key = value)")
	}
	r := &Record{At: at}
	f := p.peek()
	if f.kind != tName && f.kind != tQuoted || !is(p.peekAt(1), "FROM") {
		other()
	}
	if f.kind == tName {
		p.notBoolean(f)
	}
	r.Field = p.ident("a field's name")
	p.take() // FROM
	if t := p.peek(); t.kind != tName && t.kind != tQuoted || !is(p.peekAt(1), "WHERE") {
		other()
	}
	r.Table = p.ident("a table's name")
	p.take() // WHERE
	if k := p.peek(); k.kind != tName && k.kind != tQuoted || fold(p.identText(k)) != "key" || p.peekAt(1).kind != tEq {
		other()
	}
	p.take()
	p.take()
	// The key is the right operand of key = ..., so it takes the operators
	// that operand takes. Anything after it but the parenthesis makes
	// another subquery.
	if plant == "query/record-key-takes-or" {
		r.Key = p.climb(levelOr)
	} else {
		r.Key = p.climb(levelLt)
	}
	if t := p.peek(); t.kind != tRP {
		if t.kind == tEOF || t.kind == tIllegal {
			p.syntax(t)
		}
		other()
	}
	p.take()
	// SQLite counts the subquery's node and its WHERE's =.
	r.h = p.tall(at, height(r.Key)+1, false)
	return r
}

// cast reads CAST(x AS type).
func (p *parser) cast() *Cast {
	at := p.take().pos
	p.takeKind(tLP)
	c := &Cast{At: at, X: p.expr()}
	p.takeKeyword("AS")
	t := p.peek()
	ty, ok := castTypes[fold(p.textOf(t))]
	if t.kind != tName || !ok {
		if t.kind == tEOF || t.kind == tIllegal {
			p.syntax(t)
		}
		p.outsideWhy(t.pos, "CAST to "+p.textOf(t), "CAST takes INTEGER, REAL, TEXT, BLOB and NUMERIC")
	}
	p.take()
	if n := p.peek(); n.kind != tRP {
		if n.kind == tLP || n.kind == tName || n.kind == tQuoted {
			p.outsideWhy(t.pos, "CAST to a type with more than its name",
				"CAST takes INTEGER, REAL, TEXT, BLOB and NUMERIC")
		}
		p.syntax(n)
	}
	p.take()
	c.Type = ty
	c.h = p.tall(at, height(c.X), false)
	return c
}

// notBoolean refuses TRUE and FALSE as bare names in an expression, which
// SQLite reads as 1 and 0 when no field has the name.
func (p *parser) notBoolean(t token) {
	if w := fold(p.textOf(t)); w == "true" || w == "false" {
		p.outsideWhy(t.pos, strings.ToUpper(w), "write 1 or 0")
	}
}

// name reads what starts with a name: a field, a source's field, or a call.
func (p *parser) name() Expr {
	t := p.peek()
	if p.isWord(t, "with") {
		// SQLite reads WITH right after a parenthesis as the start of a
		// subquery, so a tree with a bare with in it couldn't always be
		// printed back.
		p.outsideWhy(t.pos, "with as a bare name in an expression", `write "with"`)
	}
	switch p.peekAt(1).kind {
	case tLP:
		return p.call()
	case tDot:
		first := p.ident("a name")
		p.take()
		c := &Column{At: first.At, Table: &first}
		switch n := p.peek(); n.kind {
		case tName, tQuoted:
			c.Name = p.ident("a name")
		case tText:
			p.ident("a field's name")
		default:
			p.syntax(n)
		}
		if n := p.peek(); n.kind == tDot {
			p.outside(first.At, "a name with a schema's name before it")
		}
		return c
	}
	if t.kind == tName {
		p.notBoolean(t)
	}
	id := p.ident("a name")
	return &Column{At: id.At, Name: id}
}

// call reads a function call. Its name and arguments are checked with the
// rest of the statement, by checkCall.
func (p *parser) call() *Call {
	c := &Call{Name: p.ident("a function's name")}
	c.At = c.Name.At
	p.take() // (
	switch t := p.peek(); {
	case is(t, "DISTINCT"):
		p.outside(t.pos, "DISTINCT in a function's arguments")
	case is(t, "ALL"):
		p.outside(t.pos, "ALL in a function's arguments")
	case t.kind == tStar:
		p.take()
		c.Star = true
	case t.kind == tRP:
	default:
		c.Args = p.exprs()
		if t := p.peek(); is(t, "ORDER") {
			p.outside(t.pos, "ORDER BY in a function's arguments")
		}
	}
	p.takeKind(tRP)
	switch t, n := p.peek(), p.peekAt(1); {
	case p.isWord(t, "filter") && n.kind == tLP:
		p.outside(t.pos, "FILTER")
	case p.isWord(t, "over") && (n.kind == tLP || n.kind == tName || n.kind == tQuoted):
		p.outside(t.pos, "a window function, with OVER,")
	}
	h := 0
	for _, a := range c.Args {
		h = max(h, height(a))
	}
	c.h = p.tall(c.At, h, false)
	return c
}

// checkCall checks a call's name and arguments against SQL.md's
// "Functions", with SQLite's messages where SQLite gives them.
func (p *parser) checkCall(c *Call) {
	name := c.Name.Name
	f := c.Func()
	if len(c.Args) > maxArgs {
		p.fail(c.At, "too many arguments on function %s", name)
	}
	info, ok := functions[f]
	switch {
	case ok:
	case f == "json_each":
		p.outsideWhy(c.At, "json_each() in an expression", "it goes in a FROM, as json_each(walk(...))")
	case sqliteFunctions[f]:
		p.outside(c.At, name+"()")
	default:
		p.fail(c.At, "no such function: %s", name)
	}
	n := len(c.Args)
	switch {
	case c.Star && f != "count":
		p.fail(c.At, "wrong number of arguments to function %s()", name)
	case f == "count" && n == 0 && !c.Star:
		p.outsideWhy(c.At, name+"() without an argument", "write count(*)")
	case (f == "date" || f == "datetime") && n == 0:
		p.outsideWhy(c.At, name+"() without an argument", "write "+f+"('now')")
	case !c.Star && (n < info.min || info.max >= 0 && n > info.max):
		p.fail(c.At, "wrong number of arguments to function %s()", name)
	}
	if f == "date" || f == "datetime" {
		for _, a := range c.Args {
			if l, ok := a.(*Literal); !ok || l.Kind != LitText {
				p.outsideWhy(a.Pos(), name+"() on anything but text literals",
					"it takes 'now' and modifiers such as '+1 day'")
			}
		}
	}
}
