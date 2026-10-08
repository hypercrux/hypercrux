// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The write side. Put, Delete and Drop are the writes of 0.x's API and of
// SQL: each checks its arguments by 0.x's rules, makes its change, and
// appends the changes it amounts to to dst, a change list, which it
// returns. Each checks everything before it changes anything, so on an
// error the store is as it was and dst comes back as it went in. Apply
// makes one change of a change list, such as a batch the log has read,
// checked by FORMAT.md's rules for changes.
//
// The four change the store directly, so they're for a goroutine that has
// the store to itself, and they panic while a transaction is open. The
// transaction's own Put, Delete, Drop and Apply, in tx.go, run the same
// code with the copy's lock and the undo list (S2). Every change to the
// store goes through one of the small functions that call changing first:
// newTable, add and setSize in store.go, and put, remove and drop here.
// changes.go says what the change lists hold, and applies a whole batch
// from the log on top of apply (S3).

// Put merges fields into the record with this key, and creates the record
// if it isn't there. The fields given take their values, a null clears a
// field, and the fields not given keep theirs.
//
// The first put into a table creates it, and then the changes are a
// CreateTable with no fields and size 0 and the Put. A field's name
// matches the table's field regardless of case, and the Put spells it as
// the table does, with its fields in byte order of those names. Names the
// table hasn't got join the end of its field list in byte order, even when
// their value is null. The vector field, the one whose name matches "vec"
// regardless of case, holds a vector or null, and the table's first
// vector sets its size. A put that adds fields can't take a table past
// rules.MaxFields.
//
// fields may come in any order, and Put neither keeps the slice nor
// changes it. The errors wrap errs.ErrInvalid, with 0.x's messages, and
// come in 0.x's order: the key, the field names, the values in byte order
// of name, the number of fields, then the vector's size.
func (s *Store) Put(dst []format.Change, key string, fields []format.Field) ([]format.Change, error) {
	s.direct("Put")
	return s.putFields(dst, key, fields)
}

func (s *Store) putFields(dst []format.Change, key string, fields []format.Field) ([]format.Change, error) {
	tbl, err := rules.TableOf(key)
	if err != nil {
		return dst, err
	}
	for _, f := range fields {
		if err := rules.Field(f.Name); err != nil {
			return dst, err
		}
	}
	if err := unique(len(fields), func(i int) string { return fields[i].Name }); err != nil {
		return dst, err
	}
	put := slices.Clone(fields)
	slices.SortFunc(put, byName)
	vec, err := keepAll(put)
	if err != nil {
		return dst, err
	}
	t := s.tables[tbl]
	added := len(put)
	if t != nil {
		added = 0
		for i := range put {
			if p, ok := t.index[rules.Fold(put[i].Name)]; !ok {
				added++
			} else if plant != "store/spelt-as-given" {
				put[i].Name = t.fields[p]
			}
		}
		// The table's spellings can sort in another order than the ones
		// given, as Zed and title do against zed and title.
		slices.SortFunc(put, byName)
	}
	if err := t.check(tbl, key, added, rules.MaxFields, vec); err != nil {
		return dst, err
	}
	if t == nil {
		dst = append(dst, format.Change{Op: format.CreateTable, Table: tbl})
		t = s.newTable(strings.Clone(tbl))
	}
	s.put(t, key, put, vec)
	return append(dst, format.Change{Op: format.Put, Key: key, Fields: put}), nil
}

// Delete removes the record with this key, which must exist. The error is
// 0.x's: ErrInvalid for a key that breaks the rules, and ErrNotFound when
// there's no such record. Its links go with it once S5 adds them.
func (s *Store) Delete(dst []format.Change, key string) ([]format.Change, error) {
	s.direct("Delete")
	return s.deleteKey(dst, key)
}

func (s *Store) deleteKey(dst []format.Change, key string) ([]format.Change, error) {
	if err := s.delete(key); err != nil {
		return dst, err
	}
	return append(dst, format.Change{Op: format.Delete, Key: key}), nil
}

// Drop removes the table called name with its records, its field list and
// its vector size. The error is 0.x's: ErrInvalid for a name that breaks
// the rules, and ErrNotFound when there's no such table.
func (s *Store) Drop(dst []format.Change, name string) ([]format.Change, error) {
	s.direct("Drop")
	return s.dropTable(dst, name)
}

func (s *Store) dropTable(dst []format.Change, name string) ([]format.Change, error) {
	if err := s.drop(name); err != nil {
		return dst, err
	}
	return append(dst, format.Change{Op: format.Drop, Table: name}), nil
}

// direct panics while a transaction is open: a write that went around it
// would change the copy without the lock, the undo list or the change list.
func (s *Store) direct(op string) {
	if s.tx != nil {
		panic("store: " + op + " on the store while a transaction is open; use the transaction's " + op)
	}
}

// Apply makes one change of a change list, after checking it against the
// state the changes before it left, by FORMAT.md's rules for changes. A
// CreateTable needs a name no table has, and a Put a table that exists. A
// Put spells each field the table has as the table does, gives its fields
// in byte order of name, and keeps the rules Put keeps. A Delete needs its
// record, and a Drop its table. A change that sets a field of
// format.Change its Op doesn't use is refused, as format.AppendBatch
// refuses it. On an error nothing changes. The errors wrap errs.ErrInvalid,
// or errs.ErrNotFound for a missing record or table. ApplyBatch and
// LoadBatch apply a whole batch from the log, and report any of them as
// damage.
//
// Link and Unlink come with S5, and until then give an error that matches
// errors.ErrUnsupported.
func (s *Store) Apply(c format.Change) error {
	s.direct("Apply")
	return s.apply(c)
}

func (s *Store) apply(c format.Change) error {
	if plant != "store/unused-fields-taken" {
		if err := unused(&c); err != nil {
			return err
		}
	}
	switch c.Op {
	case format.CreateTable:
		return s.applyCreate(c)
	case format.Put:
		return s.applyPut(c)
	case format.Delete:
		return s.delete(c.Key)
	case format.Drop:
		return s.drop(c.Table)
	case format.Link, format.Unlink:
		return notYet("Apply for "+c.Op.String(), "S5")
	}
	return fmt.Errorf("%w: a change of kind %v", errs.ErrInvalid, c.Op)
}

// unused refuses a change that sets a field of format.Change its Op doesn't
// use, which format.AppendBatch refuses too, so a transaction's Apply never
// puts one in its change list. A change of an Op that isn't one of the six
// is apply's to refuse.
func unused(c *format.Change) error {
	var table, size, names, key, fields, link bool // what the Op uses
	switch c.Op {
	case format.CreateTable:
		table, size, names = true, true, true
	case format.Put:
		key, fields = true, true
	case format.Delete:
		key = true
	case format.Link, format.Unlink:
		key, link = true, true
	case format.Drop:
		table = true
	default:
		return nil
	}
	var set []string
	for _, f := range []struct {
		name      string
		set, used bool
	}{
		{"Table", c.Table != "", table},
		{"Size", c.Size != 0, size},
		{"Names", len(c.Names) != 0, names},
		{"Key", c.Key != "", key},
		{"Fields", len(c.Fields) != 0, fields},
		{"Type", c.Type != "", link},
		{"To", c.To != "", link},
	} {
		if f.set && !f.used {
			set = append(set, f.name)
		}
	}
	if set == nil {
		return nil
	}
	a := "a"
	if c.Op == format.Unlink {
		a = "an"
	}
	return fmt.Errorf("%w: %s %v change that sets %s, which it doesn't use", errs.ErrInvalid, a, c.Op, strings.Join(set, " and "))
}

func (s *Store) applyCreate(c format.Change) error {
	if err := rules.Table(c.Table); err != nil {
		return err
	}
	if s.tables[c.Table] != nil {
		return fmt.Errorf("%w: table %s is created when it exists already", errs.ErrInvalid, c.Table)
	}
	if len(c.Names) > rules.FormatMaxFields {
		return tooMany(c.Table, len(c.Names), rules.FormatMaxFields)
	}
	vec := false
	for _, name := range c.Names {
		if err := rules.Field(name); err != nil {
			return err
		}
		vec = vec || rules.IsVec(name)
	}
	if err := unique(len(c.Names), func(i int) string { return c.Names[i] }); err != nil {
		return err
	}
	if c.Size < 0 || c.Size > rules.MaxDims || c.Size > 0 && !vec {
		return fmt.Errorf("%w: table %s is created with the vector size %d and the fields %q", errs.ErrInvalid, c.Table, c.Size, c.Names)
	}
	t := s.newTable(strings.Clone(c.Table))
	for _, name := range c.Names {
		s.add(t, strings.Clone(name))
	}
	if c.Size != 0 {
		s.setSize(t, c.Size)
	}
	return nil
}

func (s *Store) applyPut(c format.Change) error {
	tbl, err := rules.TableOf(c.Key)
	if err != nil {
		return err
	}
	t := s.tables[tbl]
	if t == nil {
		return fmt.Errorf("%w: a put of %s into table %s, which doesn't exist", errs.ErrInvalid, c.Key, tbl)
	}
	added := 0
	for i, f := range c.Fields {
		if i > 0 && c.Fields[i-1].Name >= f.Name {
			return fmt.Errorf("%w: the put of %s gives its fields out of byte order, %s before %s", errs.ErrInvalid, c.Key, c.Fields[i-1].Name, f.Name)
		}
		if p, ok := t.index[rules.Fold(f.Name)]; !ok {
			if err := rules.Field(f.Name); err != nil {
				return err
			}
			added++
		} else if t.fields[p] != f.Name {
			return fmt.Errorf("%w: the put of %s spells table %s's field %s as %s", errs.ErrInvalid, c.Key, tbl, t.fields[p], f.Name)
		}
	}
	if err := unique(len(c.Fields), func(i int) string { return c.Fields[i].Name }); err != nil {
		return err
	}
	put := slices.Clone(c.Fields)
	vec, err := keepAll(put)
	if err != nil {
		return err
	}
	if err := t.check(tbl, c.Key, added, rules.FormatMaxFields, vec); err != nil {
		return err
	}
	s.put(t, c.Key, put, vec)
	return nil
}

// check checks what a put does to the table t, which is nil when the put
// creates it: added is how many fields it adds, most the most fields the
// table may then hold, and vec the put's vector, if it has one. A put
// through the API keeps rules.MaxFields, and a change list read from a
// file the format's limit.
func (t *table) check(name, key string, added, most int, vec []float32) error {
	have, size := 0, 0
	if t != nil {
		have, size = len(t.fields), t.size
	}
	if added > 0 && have+added > most {
		return tooMany(name, have+added, most)
	}
	if vec != nil && size != 0 && len(vec) != size && plant != "store/size-unchecked" {
		return fmt.Errorf("%w: table %s holds vectors of %d values, and %s has %d", errs.ErrInvalid, name, size, key, len(vec))
	}
	return nil
}

func tooMany(table string, n, most int) error {
	return fmt.Errorf("%w: table %s would hold %d fields, and a table holds at most %d", errs.ErrInvalid, table, n, most)
}

// put makes a put that's been checked. Its fields are in byte order of
// name, each spelt as the table spells it or new to the table, with the
// values the store keeps, and vec is its vector, if it has one.
func (s *Store) put(t *table, key string, fields []format.Field, vec []float32) {
	r := s.records[key]
	if r == nil {
		r = &record{key: strings.Clone(key), table: t}
		s.changing(undo{op: undoNoRecord, name: r.key})
		s.records[r.key] = r // S4 adds the key to its table's order here
	} else {
		// A new record goes whole when its put is undone, so only a
		// record that was there needs its fields and vector kept.
		s.changing(undo{op: undoRecord, record: r, fields: r.fields, vec: r.vec})
	}
	set := make([]FieldValue, 0, len(fields))
	for _, f := range fields {
		p, ok := t.index[rules.Fold(f.Name)]
		if !ok {
			p = s.add(t, strings.Clone(f.Name))
		}
		if p == t.vec {
			r.vec = vec // nil when the put clears the vector
			if vec != nil && t.size == 0 {
				s.setSize(t, len(vec))
			}
			continue
		}
		set = append(set, FieldValue{Index: p, Value: f.Value})
	}
	slices.SortFunc(set, func(a, b FieldValue) int { return cmp.Compare(a.Index, b.Index) })
	r.fields = merge(r.fields, set)
}

// merge returns a record's fields after a put: old's, with each place the
// put sets taking its value, and the places it sets to null left out.
// Both lists are in order of place.
func merge(old, set []FieldValue) []FieldValue {
	out := make([]FieldValue, 0, len(old)+len(set))
	i, j := 0, 0
	for i < len(old) || j < len(set) {
		if j == len(set) || i < len(old) && old[i].Index < set[j].Index {
			out = append(out, old[i])
			i++
			continue
		}
		if i < len(old) && old[i].Index == set[j].Index {
			i++
		}
		if !set[j].Value.IsNull() || plant == "store/nulls-kept" {
			out = append(out, set[j])
		}
		j++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Store) delete(key string) error {
	if _, err := rules.TableOf(key); err != nil {
		return err
	}
	r := s.records[key]
	if r == nil {
		return fmt.Errorf("%w: %s", errs.ErrNotFound, key)
	}
	s.remove(r)
	return nil
}

// remove takes a record out of the store. S4 takes its key out of its
// table's order here, S5 its links both ways, and V1 frees its slot, each
// with undo entries of their own, or with more in this one.
func (s *Store) remove(r *record) {
	s.changing(undo{op: undoHadRecord, record: r})
	delete(s.records, r.key)
}

func (s *Store) drop(name string) error {
	if err := rules.Table(name); err != nil {
		return err
	}
	t := s.tables[name]
	if t == nil {
		return fmt.Errorf("%w: no record table %s", errs.ErrNotFound, name)
	}
	// Until S4 keeps each table's keys, finding a table's records takes a
	// look at every record.
	for _, r := range s.records {
		if r.table == t && plant != "store/drop-keeps-records" {
			s.remove(r)
		}
	}
	// The table and its records keep everything they hold, so undoing the
	// drop is putting them back in the maps.
	s.changing(undo{op: undoHadTable, table: t})
	delete(s.tables, name)
	return nil
}

func byName(a, b format.Field) int { return strings.Compare(a.Name, b.Name) }

// unique returns 0.x's error for the first of n names that matches one
// before it regardless of case. name(i) is the ith name.
func unique(n int, name func(int) string) error {
	if n <= 16 {
		for i := 1; i < n; i++ {
			for j := 0; j < i; j++ {
				if rules.SameName(name(i), name(j)) {
					return rules.CaseClash(name(i))
				}
			}
		}
		return nil
	}
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		f := rules.Fold(name(i))
		if seen[f] {
			return rules.CaseClash(name(i))
		}
		seen[f] = true
	}
	return nil
}

// keepAll checks each value of a put by 0.x's rules, in order, and puts in
// its place the value the store keeps. It returns the put's vector, if it
// has one.
func keepAll(fields []format.Field) ([]float32, error) {
	var vec []float32
	for i := range fields {
		v, f, err := keep(fields[i].Name, fields[i].Value)
		if err != nil {
			return nil, err
		}
		fields[i].Value = v
		if f != nil {
			vec = f
		}
	}
	return vec, nil
}

// keep checks a value for the field called name by 0.x's rules: the vector
// field holds a vector or null, no other field holds a vector, text is
// valid UTF-8, and reals are finite. Text and bytes also have to fit the
// format, which gives their length as a u32 (see fits). It returns the
// value the store keeps, with a copy of its text or bytes of its own, so
// the store never holds on to a decoded batch, and for a vector its values
// as float32s.
func keep(name string, v value.Value) (value.Value, []float32, error) {
	if rules.IsVec(name) {
		switch v.Kind() {
		case value.KindNull:
			return v, nil, nil
		case value.KindVector:
			f := v.Vector()
			if err := rules.Vector(f); err != nil {
				return v, nil, err
			}
			return v, f, nil
		}
		return v, nil, fmt.Errorf("%w: the vector field %s holds a vector or null, and this value is of kind %s", errs.ErrInvalid, name, v.Kind())
	}
	switch v.Kind() {
	case value.KindReal:
		return v, nil, rules.Real(name, v.Real())
	case value.KindText:
		if err := fits(name, len(v.Raw())); err != nil {
			return v, nil, err
		}
		if err := rules.Text(name, v.Text()); err != nil {
			return v, nil, err
		}
		return value.Text(strings.Clone(v.Text())), nil, nil
	case value.KindBytes:
		if err := fits(name, len(v.Raw())); err != nil {
			return v, nil, err
		}
		return value.Bytes(strings.Clone(v.Raw())), nil, nil
	case value.KindVector:
		return v, nil, rules.VectorElsewhere(name)
	}
	return v, nil, nil
}

// fits checks the length of a text or bytes value for the field called
// name: FORMAT.md writes it as a u32, so a value holds 4,294,967,295 bytes
// at most, and format.AppendBatch refuses a longer one. 0.x's limit is
// SQLite's, 1,000,000,000 bytes.
func fits(name string, n int) error {
	if uint64(n) > math.MaxUint32 {
		return fmt.Errorf("%w: field %s holds %d bytes, and a value holds at most %d", errs.ErrInvalid, name, n, uint64(math.MaxUint32))
	}
	return nil
}
