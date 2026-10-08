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
const MaxK = 10000

// Nearest returns the k records in table whose vectors are closest to q,
// closest first, by cosine distance, with ties in key order. The search is
// exact: every vector that passes the filter is compared. where is an
// optional condition on the table's fields in the Beta's SQL, such as
// "status = ?", with its values in args. k runs from 1 to MaxK. A table with
// no vectors yet gives no hits, and one that doesn't exist gives
// ErrNotFound.
func (db *DB) Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error) {
	return nil, notYet("DB.Nearest", nearestTask(where))
}

// Nearest is DB.Nearest inside the transaction.
func (t *Tx) Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error) {
	return nil, notYet("Tx.Nearest", nearestTask(where))
}

// nearestTask names the task that makes a search work: G2 for one without a
// filter, and G4, which brings SQL, for one with a filter.
func nearestTask(where string) string {
	if strings.TrimSpace(where) != "" {
		return "G4"
	}
	return "G2"
}
