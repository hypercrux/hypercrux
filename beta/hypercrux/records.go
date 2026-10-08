// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"slices"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Fields are a record's values by field name. Put takes these Go types:
// string, bool, every integer type, float32 and float64, []byte, time.Time
// (stored as RFC 3339 text in UTC), nil (stored as null), named types based
// on these, and maps, slices and structs, which are stored as JSON text. The
// field named "vec" is the record's vector and takes a Vector or a []float32
// or []float64.
//
// Get gives values back as string, int64, float64, []byte, and Vector for
// "vec". Fields that are null are left out.
type Fields map[string]any

// Record is a key with its fields.
type Record struct {
	Key    string
	Fields Fields
}

// MaxKeyLen is the longest key HyperCrux accepts, in bytes.
const MaxKeyLen = rules.MaxKeyLen

// TableOf returns the table a key belongs to: the part before the first
// colon. It checks the whole key against HyperCrux's rules.
func TableOf(key string) (string, error) { return rules.TableOf(key) }

// Get returns the fields of the record with this key, or ErrNotFound.
func (db *DB) Get(key string) (Fields, error) {
	s, err := db.current()
	if err != nil {
		return nil, err
	}
	var f Fields
	err = s.Read(func(r store.Reader) error {
		var err error
		f, err = get(r, key)
		return err
	})
	return f, err
}

// Get is DB.Get inside the transaction.
func (t *Tx) Get(key string) (Fields, error) {
	stx, err := t.open()
	if err != nil {
		return nil, err
	}
	return get(stx, key)
}

// get reads the record with this key from r, as 0.x's Get gives it: each
// field that holds a value under the name its table spells it with, by
// Value.Go, and the vector as a Vector. Everything is copied out, so the
// fields stay as they are once the read ends. The checks and the errors are
// the store's, which are 0.x's.
func get(r store.Reader, key string) (Fields, error) {
	rec, err := r.Get(key)
	if err != nil {
		return nil, err
	}
	t, _ := r.Table(key[:strings.IndexByte(key, ':')])
	f := make(Fields, len(rec.Fields)+1)
	for _, fv := range rec.Fields {
		f[t.Fields[fv.Index]] = fv.Value.Go()
	}
	if rec.Vec != nil {
		v := Vector(rec.Vec)
		if plant != "hypercrux/vector-shared" {
			v = slices.Clone(v)
		}
		f[t.Fields[t.Vec]] = v
	}
	return f, nil
}

// Put stores fields under key, creating the record if it's new. Fields you
// pass replace those values, and fields you leave out keep theirs. A nil
// value clears a field. The first Put into a table creates the table, and a
// field the table hasn't seen before joins its fields, even with a nil
// value. Field names match regardless of case, and a table keeps the
// spelling it saw first.
func (db *DB) Put(key string, f Fields) error {
	return db.Update(func(tx *Tx) error { return tx.Put(key, f) })
}

// Put is DB.Put inside the transaction.
func (t *Tx) Put(key string, f Fields) error {
	stx, err := t.open()
	if err != nil {
		return err
	}
	fields, err := convert(key, f)
	if err != nil {
		return err
	}
	return stx.Put(key, fields)
}

// convert checks a put's key and field names by 0.x's rules and turns its
// Go values into the store's, by value.FromGo, keeping 0.x's order of
// errors: the key, then the names, then the values in byte order of name.
// The store's Put checks the rest in the same order: how many fields the
// table would hold, then the vector's size. 0.x checks the names in its
// map's order, which is random, so where two names break the rules it
// reports either; here it's the first in byte order.
func convert(key string, f Fields) ([]format.Field, error) {
	if _, err := rules.TableOf(key); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := rules.Field(name); err != nil {
			return nil, err
		}
	}
	if err := unique(names); err != nil {
		return nil, err
	}
	fields := make([]format.Field, len(names))
	for i, name := range names {
		v, err := value.FromGo(name, f[name])
		if err != nil {
			return nil, err
		}
		fields[i] = format.Field{Name: name, Value: v}
	}
	return fields, nil
}

// unique returns 0.x's error for the first of the sorted names that matches
// one before it regardless of case.
func unique(names []string) error {
	if len(names) <= 16 {
		for i := 1; i < len(names); i++ {
			for _, other := range names[:i] {
				if rules.SameName(names[i], other) {
					return rules.CaseClash(names[i])
				}
			}
		}
		return nil
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		folded := rules.Fold(name)
		if seen[folded] {
			return rules.CaseClash(name)
		}
		seen[folded] = true
	}
	return nil
}

// Delete removes the record with this key, and every link to or from it.
// It returns ErrNotFound if there's no such record.
func (db *DB) Delete(key string) error {
	return db.Update(func(tx *Tx) error { return tx.Delete(key) })
}

// Delete is DB.Delete inside the transaction.
func (t *Tx) Delete(key string) error {
	stx, err := t.open()
	if err != nil {
		return err
	}
	return stx.Delete(key)
}

// Scan returns records whose keys start with prefix, in byte order of key.
// The prefix starts with a table name and a colon, such as "docs:" or
// "docs:2026-". Records come after the key after (use "" to start at the
// beginning), at most limit of them, or all of them when limit is 0.
// Vectors are left out; Get a record to see its vector.
func (db *DB) Scan(prefix, after string, limit int) ([]Record, error) {
	return nil, notYet("DB.Scan", "G2")
}

// Scan is DB.Scan inside the transaction.
func (t *Tx) Scan(prefix, after string, limit int) ([]Record, error) {
	return nil, notYet("Tx.Scan", "G2")
}

// Drop deletes a table with its records and every link to or from them. Its
// vector size goes too, so a new table of that name can take vectors of
// another size. It returns ErrNotFound if there's no such table.
func (db *DB) Drop(table string) error { return notYet("DB.Drop", "G2") }

// Drop is DB.Drop inside a transaction.
func (t *Tx) Drop(table string) error { return notYet("Tx.Drop", "G2") }
