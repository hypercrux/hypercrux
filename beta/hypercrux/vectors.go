// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

// Vector is a record's vector, such as an embedding of its text. It's the
// field vec, and holds float32 values. In SQL it's a blob of those values,
// little-endian, four bytes each, as 0.x stores it.
type Vector []float32

// MaxDims is the largest vector HyperCrux accepts.
const MaxDims = rules.MaxDims

// Value gives the vector to SQL as a blob, so a Vector can be passed
// straight to Query or Exec, as in distance(vec, ?).
func (v Vector) Value() (driver.Value, error) { return v.Bytes(), nil }

// Bytes returns the vector as a blob, the way SQL gives one out: each
// value's bits, little-endian, four bytes each.
func (v Vector) Bytes() []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

// DecodeVector reads a vector from a blob, as Bytes writes it and SQL gives
// it out. It checks only the blob's length.
func DecodeVector(b []byte) (Vector, error) {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil, fmt.Errorf("a vector blob of %d bytes isn't whole float32 values", len(b))
	}
	v := make(Vector, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, nil
}

// ParseVector reads a vector written as a JSON array of numbers, such as
// "[0.12, 0.8, 0.05]". Text that isn't such an array is refused with an
// error that wraps ErrInvalid, and so is a vector Put would refuse: no
// values, more than MaxDims of them, a value that isn't a finite float32,
// or only zeros.
func ParseVector(s string) (Vector, error) {
	var f []float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return nil, fmt.Errorf("%w: a vector is a JSON array of numbers: %v", ErrInvalid, err)
	}
	v := make(Vector, len(f))
	for i, x := range f {
		v[i] = float32(x)
	}
	return v, rules.Vector(v)
}

// String returns the vector as a JSON array, or "" when a value isn't
// finite, since JSON has no way to write it.
func (v Vector) String() string {
	b, _ := json.Marshal([]float32(v))
	return string(b)
}

// Hit is one result of Nearest.
type Hit struct {
	Key      string
	Distance float64 // cosine distance, from 0 (same direction) to 2
}

// MaxK is the most results Nearest returns.
const MaxK = store.MaxK

// Nearest returns the k records in table whose vectors are closest to q,
// closest first, by cosine distance, with ties in key order. The search is
// exact: every vector that passes the filter is compared. where is an
// optional condition on the table's fields in the Beta's SQL, such as
// "status = ?", with its values in args. k runs from 1 to MaxK. A table with
// no vectors yet gives no hits, and one that doesn't exist gives
// ErrNotFound.
func (db *DB) Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error) {
	if filtered(where) {
		return nil, notYet("DB.Nearest", "G4")
	}
	var hits []Hit
	err := db.read(func(r store.Reader) error {
		var err error
		hits, err = nearest(r, table, q, k)
		return err
	})
	return hits, err
}

// Nearest is DB.Nearest inside the transaction.
func (t *Tx) Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error) {
	if filtered(where) {
		return nil, notYet("Tx.Nearest", "G4")
	}
	stx, err := t.open()
	if err != nil {
		return nil, err
	}
	return nearest(stx, table, q, k)
}

// filtered reports whether where is a filter, as 0.x reads it: anything but
// spaces. A search with one waits for task G4, which brings SQL. A search
// without one doesn't use its args. 0.x hands them to database/sql all the
// same, which refuses one of a type it can't convert, such as a channel.
func filtered(where string) bool {
	return strings.TrimSpace(where) != "" && plant != "hypercrux/filter-ignored"
}

// nearest searches r without a filter, as 0.x's Nearest does: the checks,
// the errors and the hits are the store's, which are 0.x's. A table with no
// vector size gives nil, and a search that finds nothing in a table with
// one gives an empty list, as in 0.x. The hits come in a list of their own,
// and their keys are the records' own strings, which never change.
func nearest(r store.Reader, table string, q Vector, k int) ([]Hit, error) {
	found, err := r.Nearest(table, []float32(q), k, nil)
	if err != nil || found == nil {
		return nil, err
	}
	if len(found) == 0 && plant == "hypercrux/nearest-nil-for-none" {
		return nil, nil
	}
	hits := make([]Hit, len(found))
	for i, h := range found {
		hits[i] = Hit(h)
	}
	return hits, nil
}
