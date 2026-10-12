// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"errors"
	"fmt"
	"slices"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Writes (Q6): INSERT, UPDATE and DELETE inside a store.Tx, through the
// checks of the store's Put and Delete, as SQL.md's "Writes" sets them out.
// A statement is all or nothing: Exec marks the transaction before it and
// takes the statement's changes back when it fails, and the transaction
// carries on.
//
// Every name is found and every expression compiled before anything runs,
// as SQLite prepares a statement. Then the work goes in the order 0.x's
// SQLite does it, since that order shows in which error a statement gives.
// 0.x's record tables have triggers that check keys and vectors, and they
// decide SQLite's plan:
//
//   - INSERT works out every row's values before the first row goes in, as
//     SQLite fills a temporary table first when the table has triggers. One
//     row's values are worked out in the order of the table's columns, key
//     first, as sqlite3Insert codes a single row of VALUES. With several
//     rows, each row's are worked out as written, row by row.
//   - UPDATE reads the records its WHERE matches, in key order, all of them
//     before its first write. Its SET values are worked out in the order of
//     the table's columns, key first, as sqlite3Update codes them. They're
//     worked out as each record is read, in one pass, unless SQLite takes
//     two: when the SET names key or vec, which 0.x's triggers watch, or
//     when the WHERE holds a subquery. Then they're worked out once the
//     WHERE has run over every record, a record at a time, each record
//     checked and put before the next one's values are worked out.
//   - DELETE reads the keys its WHERE matches, all of them, then deletes the
//     records in key order, as SQLite takes two passes over a table with a
//     trigger on DELETE.
//
// Each row written is then checked, in SQLite's order: for an INSERT, a
// NULL key is "NOT NULL constraint failed" and a key the table has, one put
// earlier in the same statement included, "UNIQUE constraint failed". For
// an UPDATE, a key set to a value other than the one it has is refused, as
// 0.x's trigger before UPDATE refuses it. Then come a key that doesn't fit the
// table and a vec the table doesn't take, with 0.x's messages, and last
// what else Put refuses.
//
// A statement's conditions and values come from the data as it was before
// the statement changed anything, as SQL.md has it. So the subqueries an
// UPDATE works out in its second pass read the records it has changed
// already as they were, through before, where 0.x's SQLite reads them as
// changed.

// Exec runs the INSERT, UPDATE or DELETE s inside tx, with f, whose Args
// fill the statement's ? marks, one for each, and whose Now is the
// statement's moment for date() and datetime(). It gives the count of
// changed rows: the records an INSERT added, or the records an UPDATE's or a
// DELETE's WHERE matched, whether or not a value changed. A statement that
// fails leaves tx as it was before the statement, and tx carries on. A
// SELECT goes through Query. This is the one call the driver makes for a
// write (G4).
//
// The errors are of SQL.md's kinds. distance(), vector() and walk()
// refusing their arguments give an error that wraps errs.ErrInvalid, with
// 0.x's text, "hypercrux: invalid: " and the function's message. A key that
// doesn't fit the table, a key that changes and a vec the table doesn't take
// wrap errs.ErrInvalid too, with 0.x's messages word for word, and Put's own
// refusals come back as they are. Everything else is of kind "error": the
// parser's and the planner's *Error, the evaluator's, such as "integer
// overflow", and an *Error for NOT NULL and UNIQUE. An error from the
// store's reads, such as a transaction that has ended, comes back as it is.
func Exec(tx *store.Tx, s Statement, f *Frame) (int64, error) {
	// A transaction that has ended finds no table, so it would say "no
	// such table". Its reads say what's wrong.
	if _, err := tx.Get(""); errors.Is(err, errs.ErrClosed) {
		return 0, err
	}
	m := tx.Mark()
	n, err := exec(tx, s, f)
	if err != nil {
		if plant == "query/statement-takes-all-back" {
			m = store.Mark{}
		}
		tx.RollbackTo(m)
		return 0, writeError(err)
	}
	return n, nil
}

func exec(tx *store.Tx, s Statement, f *Frame) (int64, error) {
	switch s := s.(type) {
	case *Insert:
		return insert(tx, s, f)
	case *Update:
		return update(tx, s, f)
	case *Delete:
		return deleteFrom(tx, s, f)
	case *Select:
		return 0, &Error{Pos: s.At, Msg: "Exec runs an INSERT, UPDATE or DELETE, and a SELECT goes through Query"}
	}
	return 0, &Error{Msg: fmt.Sprintf("Exec runs an INSERT, UPDATE or DELETE, and was given %T", s)}
}

// writeError gives an error of a write its kind: a function refusing its
// arguments is invalid in a write, with the text 0.x's Exec gives it
// (SQL.md, "Errors and their kinds"). Every other error keeps its own.
func writeError(err error) error {
	var fe *FuncError
	if errors.As(err, &fe) && plant != "query/func-error-kept" {
		return fmt.Errorf("%w: %s", errs.ErrInvalid, fe.Msg)
	}
	return err
}

// keyError, keyChangeError and vecError are 0.x's refusals, which its
// triggers raise, word for word after "hypercrux: invalid: ". The command's
// tests look for the first.
func keyError(table string) error {
	return fmt.Errorf("%w: keys in table %s are text that starts with %s:, up to %d bytes", errs.ErrInvalid, table, table, rules.MaxKeyLen)
}

func keyChangeError() error {
	return fmt.Errorf("%w: a key never changes; delete the record and put it again", errs.ErrInvalid)
}

func vecError() error {
	return fmt.Errorf("%w: vec must be a blob of float32 values, the same size in the whole table, and not all zero", errs.ErrInvalid)
}

// writeTable finds the table a write names, which must exist: SQL can't
// create one.
func writeTable(r store.Reader, id Ident) (store.Table, error) {
	t, ok := r.Table(id.Folded())
	if !ok {
		return store.Table{}, &Error{Pos: id.At, Msg: "no such table: " + id.Name}
	}
	return t, nil
}

// column finds a field a write names: its place among the table's columns,
// 0 for key and 1 + i for the table's field at place i, or -1.
func column(t store.Table, id Ident) int {
	if id.Folded() == "key" {
		return 0
	}
	if i := t.Find(id.Name); i >= 0 {
		return 1 + i
	}
	return -1
}

// tableSelect is SELECT results FROM table [WHERE where], over the table a
// write names, with the write's count of ? marks, which number the marks of
// the whole statement.
func tableSelect(table Ident, results []Expr, where Expr, params int) *Select {
	t := table
	sel := &Select{At: table.At, From: &From{Sources: []*Source{{At: table.At, Table: &t}}}, Where: where, params: params}
	for _, e := range results {
		sel.Results = append(sel.Results, Result{Expr: e, Text: e.String()})
	}
	return sel
}

// keyColumn is the table's key, as a result column of tableSelect.
func keyColumn(at int) Expr {
	return &Column{At: at, Name: Ident{At: at, Name: "key"}}
}

// matched runs a write's WHERE: the plan of SELECT key FROM table WHERE
// where, which finds the records to read as a SELECT's WHERE does, with its
// key lookups, its terms worked out once and its subqueries worked out once,
// as SQLite plans a write's WHERE with the same code. Its rows come in key
// order. each is called with each key the WHERE matches, as the plan gives
// it, before the next record is read.
func matched(p *Plan, f *Frame, each func(key string) error) error {
	rows, err := p.Run(f)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows.Row()[0].Raw()); err != nil {
			return err
		}
	}
	return rows.Err()
}

// fieldValue makes v the value Put takes for the field at place i of the
// table t. A record's vector reads as bytes in SQL, so it goes into any
// other field as bytes. vec takes what vecValue gives.
func fieldValue(r store.Reader, t store.Table, i int, v value.Value) (value.Value, error) {
	if i == t.Vec {
		return vecValue(r, t.Name, v)
	}
	if v.Kind() == value.KindVector {
		return value.Bytes(v.Raw()), nil
	}
	return v, nil
}

// vecValue makes v a value for the table's vector field, as SQL.md's "For
// all three" has it: NULL, or bytes holding 1 to 65,536 float32 values, as
// many as the table's vectors have, all finite and not all zero, with -0
// counting as zero. Bytes become a vector first, as S1.md's note asks, and
// a record's vector, from the one-record subquery or vec itself, is one
// already. Anything else is refused with 0.x's message. The size is the
// table's as it is now, which a vector put earlier in the statement may
// have set.
func vecValue(r store.Reader, table string, v value.Value) (value.Value, error) {
	switch v.Kind() {
	case value.KindNull:
		return v, nil
	case value.KindBytes:
		raw := v.Raw()
		if len(raw) == 0 || len(raw)%4 != 0 || len(raw) > 4*rules.MaxDims {
			return v, vecError()
		}
		v = value.VectorBits(raw)
	case value.KindVector:
	default:
		return v, vecError()
	}
	if rules.Vector(v.Vector()) != nil {
		return v, vecError()
	}
	if t, ok := r.Table(table); ok && t.Size != 0 && t.Size != v.Dims() {
		return v, vecError()
	}
	return v, nil
}

// INSERT.

// insert runs an INSERT: it finds the table and its fields, compiles every
// row's values, works them all out, then puts the rows in the order given.
func insert(tx *store.Tx, s *Insert, f *Frame) (int64, error) {
	t, err := writeTable(tx, s.Table)
	if err != nil {
		return 0, err
	}
	cols := make([]int, len(s.Fields))
	keyAt := -1
	for i, id := range s.Fields {
		if cols[i] = column(t, id); cols[i] < 0 {
			return 0, &Error{Pos: id.At, Msg: "table " + t.Name + " has no column named " + id.Name}
		}
		if cols[i] == 0 {
			keyAt = i
		}
	}
	// order is the order a row's values are worked out in, as places among
	// the INSERT's fields: as written for several rows, and in the order of
	// the table's columns for one, as SQLite codes them.
	order := make([]int, len(s.Fields))
	for i := range order {
		order[i] = i
	}
	if len(s.Rows) == 1 && plant != "query/insert-values-as-written" {
		slices.SortFunc(order, func(a, b int) int { return cols[a] - cols[b] })
	}
	plans := make([]*Plan, len(s.Rows))
	for r, row := range s.Rows {
		results := make([]Expr, len(order))
		for k, i := range order {
			results[k] = row[i]
		}
		sel := &Select{At: s.At, params: s.params}
		for _, e := range results {
			sel.Results = append(sel.Results, Result{Expr: e, Text: e.String()})
		}
		if plans[r], err = Prepare(tx, sel); err != nil {
			return 0, err
		}
	}
	ins := inserter{tx: tx, s: s, t: t, cols: cols, keyAt: keyAt}
	rows := make([][]value.Value, len(s.Rows))
	for r, p := range plans {
		vals, err := runValues(p, f)
		if err != nil {
			return 0, err
		}
		row := make([]value.Value, len(vals))
		for k, i := range order {
			row[i] = vals[k]
		}
		rows[r] = row
		if plant == "query/insert-row-by-row" {
			if err := ins.put(row); err != nil {
				return 0, err
			}
		}
	}
	if plant == "query/insert-row-by-row" {
		return int64(len(rows)), nil
	}
	if plant == "query/taken-before-statement" {
		ins.before = map[int]bool{}
		for r, row := range rows {
			ins.before[r] = ins.taken(row)
		}
	}
	for r, row := range rows {
		ins.row = r
		if err := ins.put(row); err != nil {
			return 0, err
		}
	}
	return int64(len(rows)), nil
}

// runValues runs the plan of a row of values, a SELECT without a FROM, and
// gives its values.
func runValues(p *Plan, f *Frame) ([]value.Value, error) {
	rows, err := p.Run(f)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, &Error{Pos: p.sel.At, Msg: "a row of values gave no row"}
	}
	return slices.Clone(rows.Row()), nil
}

// inserter puts an INSERT's rows.
type inserter struct {
	tx    *store.Tx
	s     *Insert
	t     store.Table
	cols  []int // each field's place among the table's columns, 0 for key
	keyAt int   // the place of key among the INSERT's fields, or -1

	row    int          // the row being put, for the planted bug below
	before map[int]bool // query/taken-before-statement: each row's key taken before the statement
}

// taken reports whether a row's key is a record of the INSERT's table now,
// as SQLite's UNIQUE index on 0.x's key finds it: text that names a record
// of this table. Text that names a record of another table isn't taken
// here, and the key's check refuses it after.
func (ins *inserter) taken(row []value.Value) bool {
	if ins.keyAt < 0 {
		return false
	}
	k := row[ins.keyAt]
	if k.Kind() != value.KindText || !inTable(k.Raw(), ins.t.Name) {
		return false
	}
	_, ok, err := getRecord(ins.tx, k.Raw())
	return ok && err == nil
}

// put checks a row and puts it, in SQLite's order: the key's NOT NULL and
// UNIQUE constraints, then 0.x's trigger after INSERT on the key and on vec,
// then what else Put refuses.
func (ins *inserter) put(row []value.Value) error {
	key := null
	if ins.keyAt >= 0 {
		key = row[ins.keyAt]
	}
	if key.IsNull() {
		return &Error{Pos: ins.s.At, Msg: "NOT NULL constraint failed: " + ins.t.Name + ".key"}
	}
	taken := ins.taken(row)
	if ins.before != nil {
		taken = ins.before[ins.row]
	}
	if taken {
		return &Error{Pos: ins.s.At, Msg: "UNIQUE constraint failed: " + ins.t.Name + ".key"}
	}
	k, err := keyOf(key, ins.t.Name)
	if err != nil {
		return err
	}
	fields := make([]format.Field, 0, len(row))
	for i, c := range ins.cols {
		if c == 0 {
			continue
		}
		v, err := fieldValue(ins.tx, ins.t, c-1, row[i])
		if err != nil {
			return err
		}
		fields = append(fields, format.Field{Name: ins.t.Fields[c-1], Value: v})
	}
	return ins.tx.Put(k, fields)
}

// keyOf takes v as a key of the table, as 0.x's trigger after INSERT and
// Put take it together: text that starts with the table's name and a
// colon, with at least one byte after it, of at most 1,024 bytes, and valid
// UTF-8 without NUL bytes.
func keyOf(v value.Value, table string) (string, error) {
	if v.Kind() == value.KindText {
		k := v.Raw()
		if tbl, err := rules.TableOf(k); err == nil && (tbl == table || plant == "query/key-of-any-table") {
			return k, nil
		}
	}
	return "", keyError(table)
}

// UPDATE.

// setField is one field = value of an UPDATE, with the field's place among
// the table's columns, 0 for key.
type setField struct {
	col   int
	value Expr
}

// update runs an UPDATE: it finds the table and the fields its SET names,
// plans its WHERE and its SET values, reads the records the WHERE matches,
// and puts each in key order.
func update(tx *store.Tx, s *Update, f *Frame) (int64, error) {
	t, err := writeTable(tx, s.Table)
	if err != nil {
		return 0, err
	}
	sets := make([]setField, len(s.Set))
	for i, a := range s.Set {
		c := column(t, a.Field)
		if c < 0 {
			return 0, &Error{Pos: a.Field.At, Msg: "no such column: " + a.Field.Name}
		}
		sets[i] = setField{c, a.Value}
	}
	if plant != "query/set-in-written-order" {
		slices.SortStableFunc(sets, func(a, b setField) int { return a.col - b.col })
	}
	// SQLite's UPDATE takes two passes when a trigger watches a field the
	// SET names, as 0.x's do key and vec, or when the WHERE holds a
	// subquery once its parser has folded x AND 0.
	twoPass := hasSubquery(foldAnds(s.Where))
	for _, st := range sets {
		twoPass = twoPass || st.col == 0 || t.Vec >= 0 && st.col == 1+t.Vec
	}
	if plant == "query/update-in-one-pass" {
		twoPass = false
	}
	where, err := Prepare(tx, tableSelect(s.Table, []Expr{keyColumn(s.At)}, s.Where, s.params))
	if err != nil {
		return 0, err
	}
	values := make([]Expr, len(sets))
	for i, st := range sets {
		values[i] = st.value
	}
	old := &before{Tx: tx}
	set, err := Prepare(old, tableSelect(s.Table, values, nil, s.params))
	if err != nil {
		return 0, err
	}
	u := updater{tx: tx, t: t, sets: sets, set: set, frame: f}
	var todo []updateRow
	err = matched(where, f, func(key string) error {
		rec, err := tx.Get(key)
		if err != nil {
			return err
		}
		row := updateRow{rec: rec}
		switch {
		case plant == "query/update-writes-as-it-reads":
			if row.vals, err = u.values(rec); err == nil {
				err = u.put(row)
			}
			if err != nil {
				return err
			}
		case twoPass:
			// The record's vector is its slot, which the record's own put
			// writes over, so the record is kept as it was with a copy.
			row.rec.Vec = slices.Clone(rec.Vec)
			old.keep(row.rec)
		default:
			if row.vals, err = u.values(rec); err != nil {
				return err
			}
		}
		todo = append(todo, row)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if plant != "query/update-writes-as-it-reads" {
		for _, row := range todo {
			if twoPass {
				if row.vals, err = u.values(row.rec); err != nil {
					return 0, err
				}
			}
			if err := u.put(row); err != nil {
				return 0, err
			}
		}
	}
	if plant == "query/count-every-record" {
		return countRecords(tx, t.Name)
	}
	return int64(len(todo)), nil
}

// updateRow is a record an UPDATE matched, as it was, and its SET values,
// once they're worked out.
type updateRow struct {
	rec  store.Record
	vals []value.Value
}

// updater works out an UPDATE's SET values and puts its records.
type updater struct {
	tx    *store.Tx
	t     store.Table
	sets  []setField // in the order of the table's columns
	set   *Plan      // SELECT with the SET values FROM the table, prepared and not run
	frame *Frame
}

// values works out the SET values for a record, in turn, over a row filled
// from the record as it was.
func (u *updater) values(rec store.Record) ([]value.Value, error) {
	row := make([]value.Value, u.set.width)
	u.set.table.Fill(row, rec)
	f := *u.frame
	f.Row = &row
	vals := make([]value.Value, len(u.set.cols))
	for i, ev := range u.set.cols {
		v, err := ev(&f)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

// put checks a record's SET values and puts them: a key set to a value
// other than the one it has, exactly, is refused, as 0.x's trigger before
// UPDATE refuses it, then a vec the table doesn't take, as its trigger
// after UPDATE does, then what Put refuses. A SET that names key alone
// changes nothing, and puts nothing.
func (u *updater) put(row updateRow) error {
	var fields []format.Field
	for i, st := range u.sets {
		v := row.vals[i]
		if st.col == 0 {
			if (v.Kind() != value.KindText || v.Raw() != row.rec.Key) && plant != "query/key-change-let-through" {
				return keyChangeError()
			}
			continue
		}
		fv, err := fieldValue(u.tx, u.t, st.col-1, v)
		if err != nil {
			return err
		}
		fields = append(fields, format.Field{Name: u.t.Fields[st.col-1], Value: fv})
	}
	if len(fields) == 0 {
		return nil
	}
	return u.tx.Put(row.rec.Key, fields)
}

// before is the transaction as an UPDATE's SET values read it in its second
// pass: a record the UPDATE matched is as it was before the UPDATE changed
// anything, so the one-record subquery reads it so. The UPDATE changes no
// table, no link and no other record, so the rest reads as it is.
type before struct {
	*store.Tx
	old map[string]store.Record
}

func (b *before) keep(rec store.Record) {
	if b.old == nil {
		b.old = map[string]store.Record{}
	}
	b.old[rec.Key] = rec
}

func (b *before) Get(key string) (store.Record, error) {
	if rec, ok := b.old[key]; ok {
		return rec, nil
	}
	return b.Tx.Get(key)
}

// countRecords counts a table's records, for a planted bug.
func countRecords(r store.Reader, table string) (int64, error) {
	c, err := r.Scan(table+":", "")
	if err != nil {
		return 0, err
	}
	n := int64(0)
	for _, ok := c.Next(); ok; _, ok = c.Next() {
		n++
	}
	return n, nil
}

// DELETE.

// deleteFrom runs a DELETE: it reads the keys of every record the WHERE
// matches, then deletes the records in key order, each with its links in
// both directions, as Delete does.
func deleteFrom(tx *store.Tx, s *Delete, f *Frame) (int64, error) {
	t, err := writeTable(tx, s.Table)
	if err != nil {
		return 0, err
	}
	where, err := Prepare(tx, tableSelect(s.Table, []Expr{keyColumn(s.At)}, s.Where, s.params))
	if err != nil {
		return 0, err
	}
	var keys []string
	err = matched(where, f, func(key string) error {
		if plant == "query/delete-as-it-reads" {
			if err := tx.Delete(key); err != nil {
				return err
			}
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if plant != "query/delete-as-it-reads" {
		for _, k := range keys {
			if err := tx.Delete(k); err != nil {
				return 0, err
			}
		}
	}
	if plant == "query/count-every-record" {
		n, err := countRecords(tx, t.Name)
		return n + int64(len(keys)), err
	}
	return int64(len(keys)), nil
}
