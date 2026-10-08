// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"fmt"
	"strings"
)

// Error is a statement Parse refuses: a syntax error, SQL outside the
// subset, or a rule beside the grammar broken, such as a function with the
// wrong number of arguments. Its kind is "error", as SQL.md's "Errors and
// their kinds" has it, since it wraps neither errs.ErrNotFound nor
// errs.ErrInvalid. Where SQLite has a message for the same mistake, Msg is
// SQLite's, such as `near "SELEC": syntax error`; a statement outside the
// subset says what isn't taken.
type Error struct {
	Pos int    // the byte in the text where the trouble starts
	Msg string // what's wrong
}

func (e *Error) Error() string { return e.Msg }

// Parse reads one statement of the subset that beta/SQL.md sets out: a
// SELECT, INSERT, UPDATE or DELETE, perhaps ending with a semicolon that
// only spaces and comments follow. Anything else is an *Error, given before
// anything runs.
//
// Parse checks what can be checked without a database: the grammar, the
// functions and their arguments, where aggregates and fields may go, and
// SQLite's limits. Whether the tables and fields exist, and what each name
// in an expression finds, is the planner's to settle. What date()'s first
// argument and modifiers say is Q2's: Parse makes sure they're text
// literals, and Q2 that they're 'now' and modifiers the subset takes.
//
// The text ends at its first NUL byte, if it has one, since SQLite reads the
// text as a C string, and 0.x hands it the text as one.
func Parse(text string) (Statement, error) {
	if i := strings.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	p := newParser(text)
	var s Statement
	err := p.run(func() {
		s = p.statement()
		p.end()
		p.check(s)
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ParseCondition reads a condition on a table's records, as Nearest takes
// it: an expression that a WHERE over the table would take, with its own ?
// marks, counted in params. A condition that is empty, or only white space
// as strings.TrimSpace sees it, gives a nil Expr and keeps every record, as
// in 0.x. Like a WHERE, it may use fields and the one-record subquery, and
// no aggregates.
//
// 0.x puts the condition in parentheses at the end of its own query, so a
// NUL byte in it ends that query before the closing parenthesis, which is
// "incomplete input", and the Beta refuses it the same way.
func ParseCondition(text string) (cond Expr, params int, err error) {
	if strings.TrimSpace(text) == "" {
		return nil, 0, nil
	}
	if i := strings.IndexByte(text, 0); i >= 0 {
		return nil, 0, &Error{Pos: i, Msg: "incomplete input"}
	}
	p := newParser(text)
	err = p.run(func() {
		cond = p.expr()
		if t := p.peek(); t.kind != tEOF {
			p.syntax(t)
		}
		p.checkExpr(cond, fieldRules, 0)
	})
	if err != nil {
		return nil, 0, err
	}
	return cond, p.params, nil
}

// parser reads one statement. It stops at the first error, by panicking
// with a bailout that run recovers.
type parser struct {
	text   string
	lex    lexer
	ahead  []token // tokens read and not yet taken
	params int     // ? marks so far
	last   int     // where the last token taken ends
	nest   int     // how deep the expressions nest, for maxNesting
	err    *Error
}

type bailout struct{}

func newParser(text string) *parser {
	return &parser{text: text, lex: lexer{text: text}, ahead: make([]token, 0, 4)}
}

// run calls f and turns a bailout into the error it carries. Any other panic
// goes on, as a bug.
func (p *parser) run(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(bailout); !ok {
				panic(r)
			}
			err = p.err
		}
	}()
	f()
	return nil
}

// fail stops the parse with an error at pos.
func (p *parser) fail(pos int, format string, args ...any) {
	p.err = &Error{Pos: pos, Msg: fmt.Sprintf(format, args...)}
	panic(bailout{})
}

// outside stops the parse at pos with SQL outside the subset, and what
// says what isn't taken.
func (p *parser) outside(pos int, what string) {
	p.fail(pos, "%s is outside the SQL subset", what)
}

// outsideWhy is outside with SQL.md's reason, or the way the subset has.
func (p *parser) outsideWhy(pos int, what, why string) {
	p.fail(pos, "%s is outside the SQL subset: %s", what, why)
}

// peek returns the token after the ones taken, without taking it.
func (p *parser) peek() token { return p.peekAt(0) }

// peekAt returns the token i after the next one.
func (p *parser) peekAt(i int) token {
	for len(p.ahead) <= i {
		p.ahead = append(p.ahead, p.lex.next())
	}
	return p.ahead[i]
}

// take takes the next token and returns it.
func (p *parser) take() token {
	t := p.peek()
	p.last = t.end
	n := copy(p.ahead, p.ahead[1:])
	p.ahead = p.ahead[:n]
	return t
}

func (p *parser) textOf(t token) string { return p.text[t.pos:t.end] }

// is reports whether t is the reserved word w.
func is(t token, w string) bool { return t.kind == tKeyword && t.word == w }

// isWord reports whether t is the bare name w, in any case: one of the
// words that are keywords only where the grammar has them, such as DESC.
func (p *parser) isWord(t token, w string) bool {
	return t.kind == tName && strings.EqualFold(p.textOf(t), w)
}

// takeKeyword takes the reserved word w, or stops with a syntax error.
func (p *parser) takeKeyword(w string) token {
	t := p.peek()
	if !is(t, w) {
		p.syntax(t)
	}
	return p.take()
}

// takeWord takes the bare name w, in any case, or stops with a syntax error.
func (p *parser) takeWord(w string) token {
	t := p.peek()
	if !p.isWord(t, w) {
		p.syntax(t)
	}
	return p.take()
}

// takeKind takes a token of kind k, or stops with a syntax error.
func (p *parser) takeKind(k tokKind) token {
	t := p.peek()
	if t.kind != k {
		p.syntax(t)
	}
	return p.take()
}

// syntax stops the parse at a token that can't go where it is, with
// SQLite's message for it, or, for one of SQLite's tokens the subset
// leaves out, with what isn't taken.
func (p *parser) syntax(t token) {
	text := p.textOf(t)
	switch t.kind {
	case tEOF:
		p.fail(t.pos, "incomplete input")
	case tIllegal:
		p.fail(t.pos, "unrecognized token: %q", text)
	case tNumbered, tNamed:
		p.outsideWhy(t.pos, text, "parameters are ?, filled in order")
	case tBackquote, tBracket:
		p.outsideWhy(t.pos, text, "names go in double quotes")
	case tHex:
		p.outsideWhy(t.pos, text, "it has no hex numbers")
	case tSeparated:
		p.outsideWhy(t.pos, text, "it has no digit separators")
	case tBitAnd, tBitOr, tBitNot, tShift, tArrow:
		p.outside(t.pos, "the operator "+text)
	}
	p.fail(t.pos, "near %q: syntax error", text)
}

// enter counts n more levels of nesting, and leave counts them back out.
//
// SQLite's parser keeps its own stack, of up to 2,500 entries, and gives
// "Recursion limit" past it; each level of nesting here takes at most five
// of its entries, and a subquery, counted as subqueryDepth levels, about
// ten. The Beta stops at maxNesting levels, well inside SQLite's limit, so
// a statement it takes is one SQLite takes, and a hostile one can't run the
// stack out.
func (p *parser) enter(n int) {
	p.nest += n
	if p.nest > maxNesting {
		p.fail(p.peek().pos, "expressions nested more than %d deep are outside the SQL subset", maxNesting)
	}
}

func (p *parser) leave(n int) { p.nest -= n }

// statement reads one statement.
func (p *parser) statement() Statement {
	t := p.peek()
	switch {
	case is(t, "SELECT"):
		return p.selectStmt()
	case is(t, "INSERT"):
		return p.insertStmt()
	case is(t, "UPDATE"):
		return p.updateStmt()
	case is(t, "DELETE"):
		return p.deleteStmt()
	case t.kind == tEOF || t.kind == tSemi:
		p.fail(t.pos, "the text holds no statement")
	case t.kind == tName || t.kind == tKeyword:
		w := strings.ToUpper(p.textOf(t))
		if otherStatements[w] {
			switch w {
			case "CREATE", "DROP", "ALTER":
				p.outsideWhy(t.pos, w, "tables and their fields come from Put")
			case "BEGIN", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE":
				p.outsideWhy(t.pos, w, "transactions go through Update")
			case "VACUUM":
				p.outsideWhy(t.pos, w, "Compact does its job")
			case "VALUES":
				p.outside(t.pos, "VALUES on its own")
			}
			p.outside(t.pos, w)
		}
	}
	p.syntax(t)
	return nil
}

// end reads what follows a statement: a semicolon perhaps, then nothing
// but spaces and comments.
func (p *parser) end() {
	t := p.peek()
	if t.kind == tSemi {
		p.take()
		if t = p.peek(); t.kind != tEOF {
			p.fail(t.pos, "a second statement after the first: a call runs one statement")
		}
		return
	}
	if t.kind != tEOF {
		p.syntax(t)
	}
}

// selectStmt reads a SELECT.
func (p *parser) selectStmt() *Select {
	s := &Select{At: p.take().pos}
	switch t := p.peek(); {
	case is(t, "DISTINCT"):
		p.outside(t.pos, "DISTINCT")
	case is(t, "ALL"):
		p.outside(t.pos, "SELECT ALL")
	case t.kind == tStar:
		p.take()
		s.Star = true
		if t := p.peek(); t.kind == tComma {
			p.outside(t.pos, "* beside other result columns")
		}
	default:
		for {
			s.Results = append(s.Results, p.result())
			if p.peek().kind != tComma {
				break
			}
			p.take()
		}
		if len(s.Results) > maxColumns {
			p.fail(s.At, "too many columns in result set")
		}
	}
	if is(p.peek(), "FROM") {
		s.From = p.from()
	}
	if is(p.peek(), "WHERE") {
		p.take()
		s.Where = p.expr()
	}
	switch t := p.peek(); {
	case is(t, "GROUP"):
		p.outside(t.pos, "GROUP BY")
	case is(t, "HAVING"):
		p.outside(t.pos, "HAVING")
	case p.isWord(t, "window"):
		p.outside(t.pos, "WINDOW")
	}
	if t := p.peek(); is(t, "ORDER") {
		p.take()
		p.takeWord("by")
		for {
			s.OrderBy = append(s.OrderBy, p.term())
			if p.peek().kind != tComma {
				break
			}
			p.take()
		}
		if len(s.OrderBy) > maxColumns {
			p.fail(t.pos, "too many terms in ORDER BY clause")
		}
	}
	if is(p.peek(), "LIMIT") {
		p.take()
		s.Limit = p.expr()
		if t := p.peek(); t.kind == tComma {
			p.outsideWhy(t.pos, "LIMIT m, n", "write LIMIT n OFFSET m")
		}
		if p.isWord(p.peek(), "offset") {
			p.take()
			s.Offset = p.expr()
		}
	}
	switch t := p.peek(); {
	case is(t, "UNION"), is(t, "EXCEPT"), is(t, "INTERSECT"):
		p.outside(t.pos, t.word)
	}
	s.params = p.params
	return s
}

// result reads one result column, with its alias and its text.
func (p *parser) result() Result {
	t := p.peek()
	if (t.kind == tName || t.kind == tQuoted) && p.peekAt(1).kind == tDot && p.peekAt(2).kind == tStar {
		p.outside(t.pos, p.textOf(t)+".*")
	}
	start := t.pos
	r := Result{Expr: p.expr()}
	end := p.peek().pos
	if plant == "query/text-without-comments" {
		end = p.last
	}
	r.Text = trimSpace(p.text[start:end])
	r.Alias = p.alias()
	return r
}

// trimSpace trims what sqlite3Isspace calls spaces from both ends of s, as
// SQLite trims a result column's text for its name.
func trimSpace(s string) string {
	for len(s) > 0 && isSpace(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && isSpace(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// term reads one term of ORDER BY.
func (p *parser) term() Term {
	tm := Term{Expr: p.expr()}
	switch t := p.peek(); {
	case p.isWord(t, "asc"):
		p.take()
	case p.isWord(t, "desc"):
		p.take()
		tm.Desc = true
	}
	if t := p.peek(); p.isWord(t, "nulls") {
		p.fail(t.pos, "NULLS FIRST and NULLS LAST are outside the SQL subset")
	}
	return tm
}

// alias reads the alias after a result column or a source, if one is there:
// AS and a name, or a name alone. A bare alias can't be one of the words
// SQLite reads as a keyword in its place, notBareAlias, and SQLite reads
// WINDOW followed by a name and AS as the start of a WINDOW clause.
func (p *parser) alias() *Ident {
	t := p.peek()
	switch {
	case is(t, "AS"):
		p.take()
	case t.kind == tText:
		p.outside(t.pos, "text in single quotes as an alias")
	case t.kind == tQuoted:
	case t.kind == tName && (!notBareAlias[fold(p.textOf(t))] || plant == "query/left-as-alias" && p.isWord(t, "left")):
		if p.isWord(t, "window") && (p.peekAt(1).kind == tName || p.peekAt(1).kind == tQuoted) && is(p.peekAt(2), "AS") {
			p.outside(t.pos, "WINDOW")
		}
	default:
		return nil
	}
	a := p.ident("an alias")
	return &a
}

// ident reads a name: bare, or in double quotes. what says what the name is
// for, for the message that refuses text in single quotes.
func (p *parser) ident(what string) Ident {
	t := p.peek()
	switch t.kind {
	case tName:
		p.take()
		return Ident{At: t.pos, Name: p.textOf(t)}
	case tQuoted:
		p.take()
		return Ident{At: t.pos, Name: unquote(p.textOf(t)), Quoted: true}
	case tText:
		p.outsideWhy(t.pos, "text in single quotes as "+what, "names go in double quotes")
	}
	p.syntax(t)
	return Ident{}
}

// from reads a FROM: a source, or a walk and a table joined.
func (p *parser) from() *From {
	p.take()
	f := &From{Sources: []*Source{p.source()}}
	for {
		t := p.peek()
		switch {
		case t.kind == tComma:
			p.outsideWhy(t.pos, "a join written with a comma", "join with JOIN ... ON")
		case p.isWord(t, "inner"):
			p.take()
			p.takeKeyword("JOIN")
		case is(t, "JOIN"):
			p.take()
		case t.kind == tName && notBareAlias[fold(p.textOf(t))]:
			if w := strings.ToUpper(p.textOf(t)); w == "INDEXED" {
				p.outside(t.pos, "INDEXED BY")
			} else {
				p.outside(t.pos, w+" JOIN")
			}
		case is(t, "NOT") && p.isWord(p.peekAt(1), "indexed"):
			p.outside(t.pos, "NOT INDEXED")
		default:
			return f
		}
		if len(f.Sources) == 2 {
			p.outside(t.pos, "a FROM with more than two sources")
		}
		right := p.source()
		left := f.Sources[0]
		switch {
		case left.Walk == nil && right.Walk == nil:
			p.outside(right.At, "a join of two tables")
		case left.Walk != nil && right.Walk != nil:
			p.outside(right.At, "a join of two walks")
		}
		f.Sources = append(f.Sources, right)
		switch t := p.peek(); {
		case is(t, "ON"):
			p.take()
			f.On = p.on()
			if form, _, _, _ := p.operator(); form != formNone {
				p.outside(p.peek().pos, "a join ON anything but one column = another")
			}
		case is(t, "USING"):
			p.outside(t.pos, "USING")
		default:
			p.outside(t.pos, "a join without ON")
		}
	}
}

// source reads a source in a FROM: a table, walk(...) or
// json_each(walk(...)), perhaps with an alias.
func (p *parser) source() *Source {
	t := p.peek()
	if t.kind == tLP {
		p.outside(t.pos, "a subquery in FROM")
	}
	name := p.ident("a table's name")
	s := &Source{At: name.At}
	switch t := p.peek(); t.kind {
	case tDot:
		p.outside(name.At, "a table's name with a schema's name before it")
	case tLP:
		switch name.Folded() {
		case "walk":
			s.Walk = p.walkArgs(name.At)
		case "json_each":
			s.Walk = p.eachArgs(name.At)
		default:
			p.outside(name.At, name.Name+"(...) as a table")
		}
	default:
		s.Table = &name
	}
	s.Alias = p.alias()
	return s
}

// walkArgs reads the arguments of walk(...), from its parenthesis, as a
// table.
func (p *parser) walkArgs(at int) *Walk {
	p.take()
	w := &Walk{At: at}
	if p.peek().kind != tRP {
		w.Args = p.exprs()
	}
	p.takeKind(tRP)
	if len(w.Args) < 2 || len(w.Args) > 4 {
		p.fail(at, "wrong number of arguments to function walk()")
	}
	return w
}

// eachArgs reads json_each(walk(...)) from json_each's parenthesis.
func (p *parser) eachArgs(at int) *Walk {
	p.take()
	t := p.peek()
	if !((t.kind == tName || t.kind == tQuoted) && fold(p.identText(t)) == "walk" && p.peekAt(1).kind == tLP) {
		p.outside(at, "json_each() over anything but walk(...)")
	}
	p.take()
	w := p.walkArgs(t.pos)
	w.At, w.Each = at, true
	if t := p.peek(); t.kind != tRP {
		if t.kind == tEOF {
			p.syntax(t)
		}
		p.outside(t.pos, "json_each() with more than walk(...) in it")
	}
	p.take()
	return w
}

// identText returns what a name token stands for.
func (p *parser) identText(t token) string {
	if t.kind == tQuoted {
		return unquote(p.textOf(t))
	}
	return p.textOf(t)
}

// on reads a join's condition: column = column, perhaps in parentheses.
func (p *parser) on() *On {
	t := p.peek()
	if t.kind == tLP {
		p.take()
		if p.isWord(p.peek(), "with") {
			p.outside(p.peek().pos, "WITH")
		}
		on := p.on()
		p.takeKind(tRP)
		return on
	}
	on := &On{At: t.pos, Left: p.column()}
	if t := p.peek(); t.kind != tEq {
		if t.kind == tEOF || t.kind == tIllegal {
			p.syntax(t)
		}
		p.outside(t.pos, "a join ON anything but one column = another")
	}
	p.take()
	on.Right = p.column()
	return on
}

// column reads a name, perhaps with a source's name and a dot before it.
func (p *parser) column() *Column {
	first := p.ident("a name")
	c := &Column{At: first.At, Name: first}
	if p.peek().kind == tDot {
		p.take()
		c.Table, c.Name = &first, p.ident("a name")
	}
	return c
}

// insertStmt reads an INSERT.
func (p *parser) insertStmt() *Insert {
	s := &Insert{At: p.take().pos}
	if t := p.peek(); is(t, "OR") {
		p.outside(t.pos, strings.TrimSpace("INSERT OR "+strings.ToUpper(p.textOf(p.peekAt(1)))))
	}
	p.takeKeyword("INTO")
	s.Table = p.writeTable()
	if t := p.peek(); is(t, "AS") {
		p.outside(t.pos, "an alias for the table of an INSERT")
	}
	switch t := p.peek(); {
	case t.kind == tLP:
		p.take()
		for {
			s.Fields = append(s.Fields, p.ident("a field's name"))
			if p.peek().kind != tComma {
				break
			}
			p.take()
		}
		p.takeKind(tRP)
	case is(t, "VALUES"):
		p.outside(t.pos, "an INSERT without a list of fields")
	case is(t, "SELECT"):
		p.outside(t.pos, "INSERT ... SELECT")
	case is(t, "DEFAULT"):
		p.outside(t.pos, "DEFAULT VALUES")
	}
	switch t := p.peek(); {
	case is(t, "SELECT"):
		p.outside(t.pos, "INSERT ... SELECT")
	case is(t, "DEFAULT"):
		p.outside(t.pos, "DEFAULT VALUES")
	case p.isWord(t, "with"):
		p.outside(t.pos, "WITH")
	}
	p.takeKeyword("VALUES")
	for {
		p.takeKind(tLP)
		s.Rows = append(s.Rows, p.exprs())
		p.takeKind(tRP)
		if p.peek().kind != tComma {
			break
		}
		p.take()
	}
	switch t := p.peek(); {
	case is(t, "ON"):
		p.outside(t.pos, "an upsert, with ON CONFLICT,")
	case is(t, "RETURNING"):
		p.outside(t.pos, "RETURNING")
	}
	s.params = p.params
	return s
}

// writeTable reads the table an INSERT, UPDATE or DELETE writes.
func (p *parser) writeTable() Ident {
	id := p.ident("a table's name")
	if p.peek().kind == tDot {
		p.outside(id.At, "a table's name with a schema's name before it")
	}
	return id
}

// writeTail refuses what may follow a write's WHERE in SQLite and not in
// the subset.
func (p *parser) writeTail() {
	switch t := p.peek(); {
	case is(t, "RETURNING"):
		p.outside(t.pos, "RETURNING")
	case is(t, "ORDER"):
		p.outside(t.pos, "ORDER BY on a write")
	case is(t, "LIMIT"):
		p.outside(t.pos, "LIMIT on a write")
	}
}

// writeAlias refuses what may follow an UPDATE's or a DELETE's table in
// SQLite and not in the subset.
func (p *parser) writeAlias() {
	switch t := p.peek(); {
	case is(t, "AS"):
		p.outside(t.pos, "an alias for the table of a write")
	case p.isWord(t, "indexed"):
		p.outside(t.pos, "INDEXED BY")
	case is(t, "NOT") && p.isWord(p.peekAt(1), "indexed"):
		p.outside(t.pos, "NOT INDEXED")
	}
}

// updateStmt reads an UPDATE.
func (p *parser) updateStmt() *Update {
	s := &Update{At: p.take().pos}
	if t := p.peek(); is(t, "OR") {
		p.outside(t.pos, strings.TrimSpace("UPDATE OR "+strings.ToUpper(p.textOf(p.peekAt(1)))))
	}
	s.Table = p.writeTable()
	p.writeAlias()
	p.takeKeyword("SET")
	for {
		if t := p.peek(); t.kind == tLP {
			p.outside(t.pos, "SET with several fields in parentheses")
		}
		a := Assign{Field: p.ident("a field's name")}
		p.takeKind(tEq)
		a.Value = p.expr()
		s.Set = append(s.Set, a)
		if p.peek().kind != tComma {
			break
		}
		p.take()
	}
	if len(s.Set) > maxColumns {
		p.fail(s.At, "too many columns in set list")
	}
	if t := p.peek(); is(t, "FROM") {
		p.outside(t.pos, "UPDATE ... FROM")
	}
	if is(p.peek(), "WHERE") {
		p.take()
		s.Where = p.expr()
	}
	p.writeTail()
	s.params = p.params
	return s
}

// deleteStmt reads a DELETE.
func (p *parser) deleteStmt() *Delete {
	s := &Delete{At: p.take().pos}
	p.takeKeyword("FROM")
	s.Table = p.writeTable()
	p.writeAlias()
	if is(p.peek(), "WHERE") {
		p.take()
		s.Where = p.expr()
	}
	p.writeTail()
	s.params = p.params
	return s
}
