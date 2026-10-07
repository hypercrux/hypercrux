// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

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
const MaxKeyLen = 1024

// TableOf returns the table a key belongs to: the part before the first
// colon. It checks the whole key against HyperCrux's rules.
func TableOf(key string) (string, error) { return "", notYet("TableOf", "G1") }

// Get returns the fields of the record with this key, or ErrNotFound.
func (db *DB) Get(key string) (Fields, error) { return nil, notYet("DB.Get", "G1") }

// Get is DB.Get inside the transaction.
func (t *Tx) Get(key string) (Fields, error) { return nil, notYet("Tx.Get", "G1") }

// Put stores fields under key, creating the record if it's new. Fields you
// pass replace those values, and fields you leave out keep theirs. A nil
// value clears a field. The first Put into a table creates the table, and a
// field the table hasn't seen before joins its fields, even with a nil
// value. Field names match regardless of case, and a table keeps the
// spelling it saw first.
func (db *DB) Put(key string, f Fields) error { return notYet("DB.Put", "G1") }

// Put is DB.Put inside the transaction.
func (t *Tx) Put(key string, f Fields) error { return notYet("Tx.Put", "G1") }

// Delete removes the record with this key, and every link to or from it.
// It returns ErrNotFound if there's no such record.
func (db *DB) Delete(key string) error { return notYet("DB.Delete", "G1") }

// Delete is DB.Delete inside the transaction.
func (t *Tx) Delete(key string) error { return notYet("Tx.Delete", "G1") }

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
