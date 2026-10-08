// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

// The tree Parse gives. Q1 works out expressions from it, Q2 the dates, Q4
// and Q5 plan and run queries, and Q6 runs writes, so it keeps what each of
// them needs: a literal's text as written, names as written and as they
// match, the place of every node in the text for error messages, and the ?
// marks numbered in order.
//
// Parentheses leave no node, as in SQLite, whose parser drops them too: (x)
// is x. So a sign in front of a literal, the column number in ORDER BY 1 and
// a result column that is a field are the same with parentheses or without,
// as SQL.md has them. A sign in front of a plus sign's expression takes the
// plus sign's place as the tree is built, so -+x and -(+x) give the tree of
// -x. A result column keeps its text, from which its name comes.
//
// A node's At is the byte in the text where the node starts, counting from
// 0. Nothing in a tree changes once Parse has returned it.

// Statement is one statement: a *Select, *Insert, *Update or *Delete.
type Statement interface {
	// Params returns how many ? marks the statement holds. The arguments
	// fill them in order, and the statement takes exactly that many.
	Params() int
	// String gives the statement as SQL that parses back to the same tree.
	String() string
	statement()
}

// Select is a SELECT.
type Select struct {
	At      int
	Star    bool     // SELECT *: key, then the table's fields, or the walk's key and depth
	Results []Result // the result columns, when Star is false
	From    *From    // nil without a FROM
	Where   Expr     // nil without a WHERE
	OrderBy []Term
	Limit   Expr // nil without a LIMIT
	Offset  Expr // nil without an OFFSET

	// Aggregate reports whether the result columns use an aggregate, which
	// makes the query give exactly one row (SQL.md, "Aggregates").
	Aggregate bool

	params int
}

// Result is one result column.
type Result struct {
	Expr  Expr
	Alias *Ident // nil when the column has no alias

	// Text is the column's text, as SQL.md's "What a result column is
	// called" takes it: from its first character to just before the token
	// after it, comments kept, with spaces trimmed from both ends. A column
	// without an alias that isn't a field is called by it.
	Text string
}

// Term is one term of ORDER BY. Which of SQL.md's three kinds it is, an
// alias, a column number or an expression, is the planner's to settle.
type Term struct {
	Expr Expr
	Desc bool
}

// From is a FROM: one source, or a walk and a table joined.
type From struct {
	Sources []*Source // one or two
	On      *On       // the join's condition, when there are two sources
}

// Source is a source in a FROM: a table, or a walk as a table.
type Source struct {
	At    int
	Table *Ident // the table, or nil for a walk
	Walk  *Walk  // walk(...) or json_each(walk(...)), or nil for a table
	Alias *Ident // nil without an alias
}

// Name returns the name the source is known by, as it matches: its alias,
// or else the table's name, walk or json_each, with ASCII letters folded to
// lower case.
func (s *Source) Name() string {
	switch {
	case s.Alias != nil:
		return s.Alias.Folded()
	case s.Table != nil:
		return s.Table.Folded()
	case s.Walk.Each:
		return "json_each"
	}
	return "walk"
}

// On is a join's condition: one column equal to another. Which side is the
// table's key and which the walk's is the planner's to check, since a name
// without a source's name needs the table's fields to be found.
type On struct {
	At          int
	Left, Right *Column
}

// Walk is walk(start, depth [, type [, direction]]) as a table: a source in
// a FROM, or what IN tests against. Each is set for json_each(walk(...)),
// 0.x's form, whose one column the subset takes is value; the walk as a
// table has the columns key and depth.
type Walk struct {
	At   int
	Each bool
	Args []Expr // 2 to 4, none of them using fields
}

// Insert is INSERT INTO table (fields) VALUES (values), ...
type Insert struct {
	At     int
	Table  Ident
	Fields []Ident  // each named once, key among them
	Rows   [][]Expr // each with one value for each field
	params int
}

// Update is UPDATE table SET field = value, ... [WHERE condition].
type Update struct {
	At     int
	Table  Ident
	Set    []Assign // each field set once
	Where  Expr     // nil without a WHERE
	params int
}

// Assign is one field = value of an UPDATE.
type Assign struct {
	Field Ident
	Value Expr
}

// Delete is DELETE FROM table [WHERE condition].
type Delete struct {
	At     int
	Table  Ident
	Where  Expr // nil without a WHERE
	params int
}

func (s *Select) Params() int { return s.params }
func (s *Insert) Params() int { return s.params }
func (s *Update) Params() int { return s.params }
func (s *Delete) Params() int { return s.params }

func (*Select) statement() {}
func (*Insert) statement() {}
func (*Update) statement() {}
func (*Delete) statement() {}

// Ident is a name: of a table, a field, a source, an alias or a function.
type Ident struct {
	At     int
	Name   string // as written, without its quotes, and "" inside them made one "
	Quoted bool   // written in double quotes
}

// Folded returns the name as names match: with its ASCII letters in lower
// case and every other byte as it was, so TITLE, "Title" and title are one
// name (SQL.md, "Names").
func (id Ident) Folded() string { return fold(id.Name) }

// fold makes ASCII letters lower case, as SQLite's sqlite3StrICmp compares.
func fold(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; 'A' <= c && c <= 'Z' {
					b[j] = c + 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// Expr is an expression: a *Literal, *Param, *Column, *Call, *Cast, *Unary,
// *Binary, *Between, *Like, *In or *Record.
type Expr interface {
	// Pos returns the byte in the text where the expression starts.
	Pos() int
	// String gives the expression as SQL that parses back to the same tree.
	String() string
	expr()
}

// LitKind is the kind of a literal.
type LitKind uint8

const (
	LitNull  LitKind = iota // NULL
	LitInt                  // a run of digits
	LitReal                 // digits with a point, an exponent or both
	LitText                 // text in single quotes
	LitBytes                // x'...'
)

func (k LitKind) String() string {
	switch k {
	case LitNull:
		return "null"
	case LitInt:
		return "integer"
	case LitReal:
		return "real"
	case LitText:
		return "text"
	case LitBytes:
		return "bytes"
	}
	return "LitKind(?)"
}

// Literal is a number, text, bytes or NULL, as written. A number keeps only
// its text: Q1 reads it by SQL.md's rules, an integer too big for 64 bits
// as a real and the real as strconv.ParseFloat makes it, and applies a sign
// in front of it, which is a *Unary over the literal, to the text as
// written. A minus sign is never part of a number.
type Literal struct {
	At   int
	Kind LitKind
	Text string // exactly as written: 1e10, .5, 'it''s', x'00ff', null
	// Data is a text literal's text, without its quotes and with '' made
	// one quote, or the bytes a bytes literal's hex digits stand for. It's
	// empty for numbers and NULL.
	Data string
}

// Param is a ? mark.
type Param struct {
	At    int
	Index int // which argument fills it, counting from 0 in the order the marks are written
}

// Column is a name in an expression: a field, or with a source's name
// before a dot, that source's field. It may also turn out to be a result
// column's alias, in ORDER BY; finding what it names is the planner's.
type Column struct {
	At    int
	Table *Ident // the source's name before the dot, or nil
	Name  Ident
}

// Call is a function called by name. Parse has checked the name and the
// number of arguments against SQL.md's "Functions": date() and datetime()
// have text literals only, and walk() here is the function, giving JSON
// text.
type Call struct {
	At   int
	Name Ident
	Args []Expr
	Star bool // count(*)
	h    int
}

// Func returns the function's name as it matches, in lower case.
func (c *Call) Func() string { return c.Name.Folded() }

// Aggregate reports whether the call is one of the aggregates: count(),
// sum(), total(), avg(), or min() or max() with one argument.
func (c *Call) Aggregate() bool { return aggregate(c.Func(), len(c.Args), c.Star) }

// Cast is CAST(x AS type).
type Cast struct {
	At   int
	X    Expr
	Type string // INTEGER, REAL, TEXT, BLOB or NUMERIC, in upper case
	h    int
}

// Op is an operator of a *Unary or a *Binary.
type Op uint8

const (
	OpOr     Op = iota + 1 // OR
	OpAnd                  // AND
	OpEq                   // = or ==
	OpNe                   // != or <>
	OpIs                   // IS, and IS NULL as IS with a NULL literal
	OpIsNot                // IS NOT
	OpLt                   // <
	OpLe                   // <=
	OpGt                   // >
	OpGe                   // >=
	OpAdd                  // +
	OpSub                  // -
	OpMul                  // *
	OpDiv                  // /
	OpRem                  // %
	OpConcat               // ||
	OpNeg                  // - in front of an operand
	OpPlus                 // + in front of an operand
	OpNot                  // NOT in front of an operand
)

var opText = [...]string{
	OpOr: "OR", OpAnd: "AND", OpEq: "=", OpNe: "!=", OpIs: "IS", OpIsNot: "IS NOT",
	OpLt: "<", OpLe: "<=", OpGt: ">", OpGe: ">=", OpAdd: "+", OpSub: "-",
	OpMul: "*", OpDiv: "/", OpRem: "%", OpConcat: "||", OpNeg: "-", OpPlus: "+", OpNot: "NOT",
}

// String gives the operator as SQL writes it.
func (op Op) String() string {
	if int(op) < len(opText) && opText[op] != "" {
		return opText[op]
	}
	return "Op(?)"
}

// Unary is an operator in front of an operand: -, + or NOT.
type Unary struct {
	At int
	Op Op // OpNeg, OpPlus or OpNot
	X  Expr
	h  int
}

// Binary is an operator between two operands.
type Binary struct {
	At   int // where its left operand starts
	OpAt int // where the operator is
	Op   Op
	L, R Expr
	h    int
}

// Between is x [NOT] BETWEEN low AND high.
type Between struct {
	At        int
	Not       bool
	X         Expr
	Low, High Expr
	h         int
}

// Like is x [NOT] LIKE pattern [ESCAPE escape].
type Like struct {
	At      int
	Not     bool
	X       Expr
	Pattern Expr
	Escape  Expr // nil without ESCAPE
	h       int
}

// In is x [NOT] IN (...): a list of items, which may be empty, or the keys
// a walk reaches, written (SELECT key FROM walk(...)) or 0.x's (SELECT value
// FROM json_each(walk(...))).
type In struct {
	At   int
	Not  bool
	X    Expr
	List []Expr // the items, when Walk is nil
	Walk *Walk  // the walk, for IN over a walk
	h    int
}

// Record is the one-record subquery, (SELECT field FROM table WHERE key =
// e): field, or key, of the record in table whose key is e.
type Record struct {
	At    int
	Field Ident
	Table Ident
	Key   Expr // uses no fields
	h     int
}

func (e *Literal) Pos() int { return e.At }
func (e *Param) Pos() int   { return e.At }
func (e *Column) Pos() int  { return e.At }
func (e *Call) Pos() int    { return e.At }
func (e *Cast) Pos() int    { return e.At }
func (e *Unary) Pos() int   { return e.At }
func (e *Binary) Pos() int  { return e.At }
func (e *Between) Pos() int { return e.At }
func (e *Like) Pos() int    { return e.At }
func (e *In) Pos() int      { return e.At }
func (e *Record) Pos() int  { return e.At }

func (*Literal) expr() {}
func (*Param) expr()   {}
func (*Column) expr()  {}
func (*Call) expr()    {}
func (*Cast) expr()    {}
func (*Unary) expr()   {}
func (*Binary) expr()  {}
func (*Between) expr() {}
func (*Like) expr()    {}
func (*In) expr()      {}
func (*Record) expr()  {}

// height is an expression's height as SQLite counts it to hold a tree to
// SQLITE_MAX_EXPR_DEPTH, or a little more where SQLite turns one node into
// two, so a tree the Beta takes is never one SQLite refuses. Literals,
// marks and names are 1.
func height(e Expr) int {
	switch e := e.(type) {
	case *Call:
		return e.h
	case *Cast:
		return e.h
	case *Unary:
		return e.h
	case *Binary:
		return e.h
	case *Between:
		return e.h
	case *Like:
		return e.h
	case *In:
		return e.h
	case *Record:
		return e.h
	}
	return 1
}
