// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// The planner (Q5): a SELECT's tree to a plan of the operators (Q4) over a
// store.Reader. Prepare finds every name as SQL.md's "Names and columns"
// has it and compiles every expression of the statement, so its errors all
// come before its first row, as 0.x's do. Run then works out LIMIT and
// OFFSET and puts the operators together, in the order SQLite's code
// generator does its work, since that order shows in which errors a
// statement gives:
//
//   - LIMIT, then OFFSET (Bounds). A LIMIT of 0 runs nothing more.
//   - The WHERE's terms that read no table, which sqlite3WhereBegin works
//     out once before it reads a row: a false one runs nothing more, even
//     the walk, and one that fails fails over an empty table too. A term
//     with a subquery or walk() isn't one of them, as SQLite holds those
//     to be other than deterministic.
//   - The rows: one row with no fields without a FROM; a table's records in
//     key order; the records a term on the key names, for key = e and key
//     IN (...), as SQLite looks them up in the key's index; the store's
//     nearest search; a walk; or a walk and, for each step, the record of
//     the table it reached. A join works out the terms that read the walk
//     alone for every step, before it looks for the record, as SQLite does
//     at the walk's loop.
//   - The other terms, in turn, as Filter works them out.
//   - Sort, or TopK under a LIMIT, then Limit; or Limit and Project; or
//     Aggregate, Limit and Project, as Q4.md sets out. An ORDER BY whose
//     first term is the table's key is the order the rows come in already,
//     as the key's index gives it to SQLite, so it sorts nothing.
//
// Before compiling, the planner folds x AND 0 as SQLite's parser does
// (foldAnds), puts the result columns' aliases into ORDER BY's expressions,
// as SQLite's resolveAlias does, and splits the WHERE at its ANDs.

// Shape is the kind of plan a SELECT gets, named after the operators of
// BETA.md's "Queries and SQL".
type Shape string

const (
	ShapeRow      Shape = "one row"    // no FROM: one row with no fields
	ShapeScan     Shape = "scan"       // a table's records, in key order
	ShapeLookup   Shape = "key lookup" // the records key = e, key IN (...) or key IN over a walk names
	ShapeNearest  Shape = "nearest"    // the store's nearest search, for ORDER BY distance() with a LIMIT
	ShapeWalk     Shape = "walk"       // the walk as a table
	ShapeWalkJoin Shape = "walk join"  // a walk joined to a table on the key
)

// Query runs the SELECT s over r: it plans s, compiling every expression,
// then runs the plan with f, whose Args fill the ? marks and whose Now is
// the statement's moment for date() and datetime(). It gives the names of
// the result columns and the rows, which read r as they go, so like r they
// last as long as the read, and the caller closes them. This is the one
// call the driver makes for a SELECT (G4).
func Query(r store.Reader, s *Select, f *Frame) ([]string, Rows, error) {
	p, err := Prepare(r, s)
	if err != nil {
		return nil, nil, err
	}
	rows, err := p.Run(f)
	if err != nil {
		return nil, nil, err
	}
	return p.Columns(), rows, nil
}

// Plan is a SELECT planned over a store.Reader, with every expression
// compiled. It's good while the read lasts, and runs once.
type Plan struct {
	r     store.Reader
	sel   *Select
	shape Shape
	scope *planScope

	srcs  []*planSource
	table *planSource // the table among the sources, or nil
	walk  *planSource // the walk among them, or nil
	width int         // the row's width, every source's columns side by side
	keyAt int         // in a join, where the walk's key is in the row

	results []Expr // the result columns, * written out, folded
	names   []string
	cols    []Eval
	aggs    *Aggregates // for an aggregate query
	terms   []orderTerm
	keys    []SortKey
	order   ordering

	once   []Cond // WHERE's terms worked out once, before any row
	outer  []Cond // a join's terms on the walk alone, for each step
	conds  []Cond // the other terms, for each row
	lookup *keyLookup
	near   *nearPlan

	limit, offset Expr
	ran           bool
	noLookups     bool

	// The terms each of once, outer and conds came from, in the same order.
	onceTerms, outerTerms, condTerms []Expr
	subs                             []string // the subqueries worked out once, for String
}

// orderTerm is an ORDER BY term once the planner has found what it is: the
// result column it sorts by, by alias or number, or an expression of its
// own, with the result columns' aliases in it.
type orderTerm struct {
	col  int  // the result column, or -1
	expr Expr // what it sorts by: that column's expression, or its own
	desc bool
}

// ordering is how a plan meets its ORDER BY.
type ordering uint8

const (
	unordered      ordering = iota // nothing to sort: no ORDER BY, one row, or an aggregate query
	byKey                          // the table's key going up, the order the records come in
	byKeyBackwards                 // the table's key going down: the records backwards
	sorted                         // Sort, or TopK under a LIMIT
)

// sourceKind is what a source in a FROM is.
type sourceKind uint8

const (
	tableSource sourceKind = iota
	walkSource             // walk(...): the columns key and depth
	eachSource             // json_each(walk(...)): SQLite's columns, of which only value may be used
)

var (
	walkColumns = []string{"key", "depth"}
	// eachColumns are json_each's columns as SQLite finds names, the
	// hidden json and root among them, so names clash as they do there.
	eachColumns = []string{"key", "value", "type", "atom", "id", "parent", "fullkey", "path", "json", "root"}
)

const eachValue = 1 // value's place among eachColumns

// planSource is a source in the FROM with its run of places in the row.
type planSource struct {
	*RowSource
	kind  sourceKind
	src   *Source
	table store.Table // a table's shape
	args  []Eval      // a walk's arguments, compiled
}

// Prepare plans the SELECT s over r: it finds the sources, the names and
// what each ORDER BY term is, compiles every expression, and chooses the
// plan's shape. Every error it gives is of kind "error", as SQL.md's
// "Errors and their kinds" has the errors of a SELECT.
func Prepare(r store.Reader, s *Select) (*Plan, error) { return prepare(r, s, true) }

// prepare is Prepare, and leaves out lookups when lookups isn't set, for a
// plan whose terms all go to a nearest search's filter (NearestFilter).
func prepare(r store.Reader, s *Select, lookups bool) (*Plan, error) {
	p := &Plan{r: r, sel: s, noLookups: !lookups}
	p.scope = &planScope{p: p}
	for _, step := range []func() error{p.from, p.expand, p.orderBy, p.compileResults, p.where, p.bounds} {
		if err := step(); err != nil {
			return nil, err
		}
	}
	p.choose()
	return p, nil
}

// Columns gives the names of the result columns, as SQL.md's "What a
// result column is called" has them.
func (p *Plan) Columns() []string { return p.names }

// Shape gives the kind of plan the statement got.
func (p *Plan) Shape() Shape { return p.shape }

// from finds the FROM's sources, compiles a walk's arguments and checks a
// join's ON.
func (p *Plan) from() error {
	f := p.sel.From
	if f == nil {
		return nil
	}
	base := 0
	for _, s := range f.Sources {
		ps := &planSource{src: s}
		if s.Table != nil {
			t, ok := p.r.Table(s.Table.Folded())
			if !ok {
				return &Error{Pos: s.At, Msg: "no such table: " + s.Table.Name}
			}
			ps.RowSource, ps.kind, ps.table = TableSource(s.Name(), t, base), tableSource, t
			p.table = ps
		} else {
			cols, kind := walkColumns, walkSource
			if s.Walk.Each {
				cols, kind = eachColumns, eachSource
			}
			ps.RowSource, ps.kind = &RowSource{Name: s.Name(), Columns: cols, Base: base}, kind
			p.walk = ps
		}
		base += len(ps.Columns)
		p.srcs = append(p.srcs, ps)
	}
	p.width = base
	if p.walk != nil {
		for _, a := range p.walk.src.Walk.Args {
			ev, err := Compile(foldAnds(a), p.scope)
			if err != nil {
				return err
			}
			p.walk.args = append(p.walk.args, ev)
		}
	}
	if f.On != nil {
		return p.on(f.On)
	}
	return nil
}

// on checks a join's condition: the table's key on one side, and the
// walk's key, or json_each's value, on the other, either way round. It
// keeps where the walk's key is in the row.
func (p *Plan) on(on *On) error {
	ls, la, err := p.find(on.Left)
	if err != nil {
		return err
	}
	rs, ra, err := p.find(on.Right)
	if err != nil {
		return err
	}
	isKey := func(s *planSource, at int) bool { return s.kind == tableSource && at == 0 }
	isWalk := func(s *planSource, at int) bool {
		return s.kind == walkSource && at == 0 || s.kind == eachSource && at == eachValue
	}
	switch {
	case isKey(ls, la) && isWalk(rs, ra):
		p.keyAt = rs.Base + ra
	case isWalk(ls, la) && isKey(rs, ra):
		p.keyAt = ls.Base + la
	default:
		return &Error{Pos: on.At, Msg: "a join ON anything but the table's key = the walk's key, or json_each's value, is outside the SQL subset"}
	}
	return nil
}

// find finds the column c names among the sources, as SQLite's lookupName
// does: with a source's name before a dot, among the sources of that name,
// and without one, among them all. A name two of them have is ambiguous.
// It gives the source and the column's place among its columns.
func (p *Plan) find(c *Column) (*planSource, int, error) {
	var found *planSource
	at, n := -1, 0
	for _, s := range p.srcs {
		if c.Table != nil && s.Name != c.Table.Folded() {
			continue
		}
		if i := s.find(c.Name.Name); i >= 0 {
			if n++; n == 1 {
				found, at = s, i
			}
		}
	}
	switch {
	case n > 1:
		return nil, -1, fail(c, "ambiguous column name: %s", c)
	case n == 0:
		return nil, -1, fail(c, "no such column: %s", c)
	}
	return found, at, nil
}

// hasColumn reports whether a source has a column called c, for ORDER BY,
// where a field comes before an alias.
func (p *Plan) hasColumn(c *Column) bool {
	for _, s := range p.srcs {
		if c.Table == nil && s.find(c.Name.Name) >= 0 {
			return true
		}
	}
	return false
}

// isColumn reports whether e is a name that finds the column at place at
// of the table, and nothing else.
func (p *Plan) isColumn(e Expr, at int) bool {
	c, ok := e.(*Column)
	if !ok || p.table == nil {
		return false
	}
	s, i, err := p.find(c)
	return err == nil && s == p.table && i == at
}

// isKey reports whether e is the table's key, and isVec whether it's the
// table's vector field.
func (p *Plan) isKey(e Expr) bool { return p.isColumn(e, 0) }

func (p *Plan) isVec(e Expr) bool {
	return p.table != nil && p.table.vec > 0 && p.isColumn(e, p.table.vec)
}

// expand writes out *, folds the result columns, and names them.
func (p *Plan) expand() error {
	results := p.sel.Results
	if p.sel.Star {
		if len(p.srcs) == 0 {
			return &Error{Pos: p.sel.At, Msg: "no tables specified"}
		}
		s := p.srcs[0]
		results = nil
		for _, name := range s.Columns {
			col := &Column{At: p.sel.At, Table: &Ident{At: p.sel.At, Name: s.Name}, Name: Ident{At: p.sel.At, Name: name}}
			results = append(results, Result{Expr: col, Text: name})
		}
	}
	for _, res := range results {
		e := foldAnds(res.Expr)
		p.results = append(p.results, e)
		name := res.Text
		if res.Alias != nil {
			name = res.Alias.Name
		} else if col, ok := res.Expr.(*Column); ok {
			if s, at, err := p.find(col); err == nil {
				name = s.Columns[at]
			}
		}
		p.names = append(p.names, name)
	}
	return nil
}

// alias returns the result column whose alias is c's name, or -1.
func (p *Plan) alias(c *Column) int {
	if c.Table != nil || p.sel.Star {
		return -1
	}
	for j, res := range p.sel.Results {
		if res.Alias != nil && res.Alias.Folded() == c.Name.Folded() {
			return j
		}
	}
	return -1
}

// orderBy settles each ORDER BY term by SQL.md's three rules, as SQLite's
// resolveOrderGroupBy does: the alias of a result column, a column number,
// or an expression of its own, in which a name finds a field first and a
// result column's alias after.
func (p *Plan) orderBy() error {
	for i, t := range p.sel.OrderBy {
		e := foldAnds(t.Expr)
		ot := orderTerm{col: -1, desc: t.Desc}
		if c, ok := e.(*Column); ok {
			ot.col = p.alias(c)
		}
		if ot.col < 0 {
			if n, ok := termNumber(e); ok {
				if n < 1 || n > int64(len(p.results)) {
					return &Error{Pos: t.Expr.Pos(), Msg: nthWord(i+1) + " ORDER BY term out of range - should be between 1 and " + strconv.Itoa(len(p.results))}
				}
				ot.col = int(n - 1)
			}
		}
		if ot.col >= 0 {
			ot.expr = p.results[ot.col]
		} else {
			ot.expr = p.aliasesIn(e)
		}
		p.terms = append(p.terms, ot)
	}
	return nil
}

// aliasesIn puts the result columns' expressions in place of the names in
// e that find no field but are a result column's alias, as SQLite's
// resolveAlias does, so each is worked out as the expression it names, its
// affinity included.
func (p *Plan) aliasesIn(e Expr) Expr {
	return rewrite(e, func(e Expr) Expr {
		c, ok := e.(*Column)
		if !ok || c.Table != nil || p.hasColumn(c) && plant != "query/order-alias-first" {
			return e
		}
		if j := p.alias(c); j >= 0 {
			return p.results[j]
		}
		return e
	})
}

// compileResults compiles the result columns, and ORDER BY's terms that
// are expressions of their own. In an aggregate query the result columns
// read the aggregates' one row, and ORDER BY sorts that row, so only its
// aggregates are worked out, and a field outside one is outside the
// subset. A query without a FROM has one row, and SQLite doesn't work out
// its ORDER BY at all. Both are compiled anyway, so their errors show.
func (p *Plan) compileResults() error {
	var colScope Scope = p.scope
	if p.sel.Aggregate {
		all := slices.Clone(p.results)
		for _, t := range p.terms {
			if t.col >= 0 {
				continue
			}
			if c := fieldOutside(t.expr); c != nil {
				if _, _, err := p.find(c); err != nil {
					return err
				}
				return fail(c, "%s in ORDER BY is outside the SQL subset: in a query with an aggregate, fields go only inside an aggregate's argument", c)
			}
			all = append(all, t.expr)
		}
		aggs, err := NewAggregates(all, p.scope)
		if err != nil {
			return err
		}
		p.aggs, colScope = aggs, aggs
	}
	for _, e := range p.results {
		ev, err := Compile(e, colScope)
		if err != nil {
			return err
		}
		p.cols = append(p.cols, ev)
	}
	for _, t := range p.terms {
		key := SortKey{Col: t.col, Desc: t.desc}
		if t.col < 0 {
			ev, err := Compile(t.expr, colScope)
			if err != nil {
				return err
			}
			key.Eval = ev
		}
		p.keys = append(p.keys, key)
	}
	return nil
}

// where folds and splits the WHERE, compiles each term, and sorts them out:
// the terms worked out once before any row, the term a lookup uses in
// place of a scan, a join's terms on the walk alone, and the rest. A term
// x IN or NOT IN over a walk gets the planner's own condition, which finds
// x in a set of the walk's keys.
func (p *Plan) where() error {
	terms := andTerms(foldAnds(p.sel.Where))
	conds := make([]Cond, len(terms))
	for i, term := range terms {
		c, err := CompileCondition(term, p.scope)
		if err != nil {
			return err
		}
		conds[i] = c
	}
	used := -1
	if p.table != nil && p.walk == nil && !p.noLookups {
		var err error
		if used, err = p.lookupTerm(terms); err != nil {
			return err
		}
	}
	for i, term := range terms {
		if i == used {
			continue
		}
		if len(p.srcs) == 0 || onceOnly(term) && plant != "query/once-terms-for-each-row" {
			p.once, p.onceTerms = append(p.once, conds[i]), append(p.onceTerms, term)
			continue
		}
		c, err := p.inWalk(term, conds[i])
		if err != nil {
			return err
		}
		if p.walk != nil && p.table != nil && p.walkOnly(term) && plant != "query/join-terms-after-lookup" {
			p.outer, p.outerTerms = append(p.outer, c), append(p.outerTerms, term)
			continue
		}
		p.conds, p.condTerms = append(p.conds, c), append(p.condTerms, term)
	}
	return nil
}

// walkOnly reports whether every name in a join's WHERE term finds the
// walk, which takes in the terms that read no field.
func (p *Plan) walkOnly(term Expr) bool {
	for _, c := range columnsOf(term) {
		if s, _, err := p.find(c); err != nil || s != p.walk {
			return false
		}
	}
	return true
}

// inWalk gives a WHERE term's condition: the planner's own for x IN or NOT
// IN over a walk, and otherwise the evaluator's.
func (p *Plan) inWalk(term Expr, c Cond) (Cond, error) {
	in, ok := term.(*In)
	if !ok || in.Walk == nil {
		return c, nil
	}
	x, err := Compile(in.X, p.scope)
	if err != nil {
		return nil, err
	}
	o, err := p.scope.walkOnce(in.Walk)
	if err != nil {
		return nil, err
	}
	return inWalkCond(o, x, in.Not), nil
}

// bounds compiles LIMIT and OFFSET, folded, so their errors come with the
// rest. Bounds compiles them again as it works them out.
func (p *Plan) bounds() error {
	p.limit, p.offset = foldAnds(p.sel.Limit), foldAnds(p.sel.Offset)
	for _, e := range []Expr{p.limit, p.offset} {
		if e == nil {
			continue
		}
		if _, err := Compile(e, p.scope); err != nil {
			return err
		}
	}
	return nil
}

// choose settles the plan's shape and how it meets its ORDER BY.
func (p *Plan) choose() {
	switch {
	case len(p.srcs) == 0:
		p.shape = ShapeRow
	case p.walk != nil && p.table != nil:
		p.shape = ShapeWalkJoin
	case p.walk != nil:
		p.shape = ShapeWalk
	case p.lookup != nil:
		p.shape = ShapeLookup
	case p.nearest():
		p.shape = ShapeNearest
	default:
		p.shape = ShapeScan
	}
	switch {
	case len(p.terms) == 0 || p.aggs != nil || p.shape == ShapeRow:
		p.order = unordered
	case (p.shape == ShapeScan || p.shape == ShapeLookup) && p.isKey(p.terms[0].expr):
		p.order = byKey
		if p.terms[0].desc {
			p.order = byKeyBackwards
		}
	default:
		p.order = sorted
	}
}

// nearPlan is a nearest search: the query vector, and the terms the
// search's filter works out for each record with a vector.
type nearPlan struct {
	q           Eval
	term        Expr // the distance() it sorts by
	filter      []Cond
	filterTerms []Expr
}

// nearest reports whether a SELECT over one table can be the store's
// nearest search, and sets it up. It must give exactly what Filter and
// TopK would, errors included (SQL.md, "Vectors"), so it needs:
//
//   - ORDER BY distance(vec, q) going up, where vec is the table's vector
//     field and q reads no field, then perhaps the key going up, which is
//     how the search breaks ties;
//   - a LIMIT, and a WHERE term vec IS NOT NULL, so that the rows without
//     a vector, whose distance is NULL and which come first, are left out,
//     and the search, which sees only records with a vector, misses none;
//   - no term before that one that can raise an error, since the search
//     never works it out for a record without a vector, and no result
//     column that can, apart from those the terms sort by, since the search
//     works out the result columns of the rows it gives and no others.
//
// Run works out q before the search, and sorts instead when the search
// can't take it, so distance()'s errors come where 0.x's would.
func (p *Plan) nearest() bool {
	if p.aggs != nil || p.limit == nil || len(p.terms) == 0 {
		return false
	}
	t0 := p.terms[0]
	call, ok := t0.expr.(*Call)
	if t0.desc || !ok || call.Func() != "distance" || len(call.Args) != 2 {
		return false
	}
	var q Expr
	switch a, b := call.Args[0], call.Args[1]; {
	case p.isVec(a) && fieldFree(b):
		q = b
	case p.isVec(b) && fieldFree(a):
		q = a
	default:
		return false
	}
	isKeyTerm := map[int]bool{}
	if t0.col >= 0 {
		isKeyTerm[t0.col] = true
	}
	for _, t := range p.terms[1:] {
		if t.desc || !p.isKey(t.expr) {
			return false
		}
		if t.col >= 0 {
			isKeyTerm[t.col] = true
		}
	}
	notNull := -1
	for i, term := range p.condTerms {
		if p.vecNotNullTerm(term) {
			notNull = i
			break
		}
	}
	if notNull < 0 && plant != "query/nearest-without-vec-check" {
		return false
	}
	for _, term := range p.condTerms[:max(notNull, 0)] {
		if mayFail(term) {
			return false
		}
	}
	for j, e := range p.results {
		if !isKeyTerm[j] && mayFail(e) {
			return false
		}
	}
	ev, err := Compile(q, p.scope)
	if err != nil {
		return false
	}
	n := &nearPlan{q: ev, term: call}
	for i, c := range p.conds {
		if i != notNull {
			n.filter, n.filterTerms = append(n.filter, c), append(n.filterTerms, p.condTerms[i])
		}
	}
	p.near = n
	return true
}

// vecNotNullTerm reports whether term is the table's vector field IS NOT
// NULL.
func (p *Plan) vecNotNullTerm(term Expr) bool {
	b, ok := term.(*Binary)
	if !ok || b.Op != OpIsNot {
		return false
	}
	l, ok := b.R.(*Literal)
	return ok && l.Kind == LitNull && p.isVec(b.L)
}

// String gives the plan, an operator a line, in the order rows go through
// them, from where they come from to the result columns, and then the
// subqueries worked out once for the statement.
func (p *Plan) String() string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	if len(p.onceTerms) > 0 {
		add("once " + termsText(p.onceTerms))
	}
	switch p.shape {
	case ShapeRow:
		add("row")
	case ShapeScan:
		if p.order == byKeyBackwards {
			add("scan " + p.table.table.Name + " backwards")
		} else {
			add("scan " + p.table.table.Name)
		}
	case ShapeLookup:
		how := "lookup " + p.table.table.Name + " by " + p.lookup.text
		if p.order == byKeyBackwards {
			how += ", backwards"
		}
		add(how)
	case ShapeNearest:
		how := "nearest " + p.table.table.Name + " by " + p.near.term.String()
		if len(p.near.filterTerms) > 0 {
			how += " through " + termsText(p.near.filterTerms)
		}
		add(how)
	case ShapeWalk, ShapeWalkJoin:
		add("walk " + sourceText(p.walk.src))
		if len(p.outerTerms) > 0 {
			add("filter " + termsText(p.outerTerms))
		}
		if p.shape == ShapeWalkJoin {
			add("join " + sourceText(p.table.src) + " ON " + p.sel.From.On.Left.String() + " = " + p.sel.From.On.Right.String())
		}
	}
	if len(p.condTerms) > 0 && p.shape != ShapeNearest {
		add("filter " + termsText(p.condTerms))
	}
	var by []string
	for _, t := range p.sel.OrderBy {
		s := t.Expr.String()
		if t.Desc {
			s += " DESC"
		}
		by = append(by, s)
	}
	switch {
	case p.aggs != nil:
		var calls []string
		for _, c := range p.aggs.calls {
			calls = append(calls, c.String())
		}
		add("aggregate " + strings.Join(calls, ", "))
	case p.shape == ShapeNearest:
	case p.order == sorted && p.limit != nil:
		add("top k by " + strings.Join(by, ", "))
	case p.order == sorted:
		add("sort by " + strings.Join(by, ", "))
	}
	switch {
	case p.offset != nil:
		add("limit and offset")
	case p.limit != nil:
		add("limit")
	}
	add("project " + strings.Join(p.names, ", "))
	for _, s := range p.subs {
		add("once " + s)
	}
	return strings.Join(lines, "\n")
}

// termsText writes WHERE's terms joined by AND.
func termsText(terms []Expr) string {
	texts := make([]string, len(terms))
	for i, t := range terms {
		texts[i] = t.String()
	}
	return strings.Join(texts, " AND ")
}

// sourceText writes a source as the FROM has it.
func sourceText(s *Source) string {
	text := ""
	if s.Table != nil {
		text = s.Table.String()
	} else {
		text = s.Walk.String()
	}
	if s.Alias != nil {
		text += " AS " + s.Alias.String()
	}
	return text
}
