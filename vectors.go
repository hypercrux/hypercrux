// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"container/heap"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Vector is a record's vector, such as an embedding of its text. It is
// stored in the vec column as a blob of float32 values, little-endian,
// four bytes each.
type Vector []float32

// MaxDims is the largest vector HyperCrux accepts.
const MaxDims = 65536

// Bytes returns the vector as it is stored.
func (v Vector) Bytes() []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

// DecodeVector reads a vector as it is stored in a vec column.
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
// "[0.12, 0.8, 0.05]".
func ParseVector(s string) (Vector, error) {
	var f []float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return nil, fmt.Errorf("%w: a vector is a JSON array of numbers: %v", ErrInvalid, err)
	}
	v := make(Vector, len(f))
	for i, x := range f {
		v[i] = float32(x)
	}
	return v, checkVector(v)
}

// String returns the vector as a JSON array.
func (v Vector) String() string {
	b, _ := json.Marshal([]float32(v))
	return string(b)
}

func toVector(x any) (Vector, error) {
	switch v := x.(type) {
	case Vector:
		return v, nil
	case []float32:
		return Vector(v), nil
	case []float64:
		out := make(Vector, len(v))
		for i, f := range v {
			out[i] = float32(f)
		}
		return out, nil
	case []any:
		out := make(Vector, len(v))
		for i, e := range v {
			switch n := e.(type) {
			case float64:
				out[i] = float32(n)
			case json.Number:
				f, err := n.Float64()
				if err != nil {
					return nil, fmt.Errorf("%w: vec value %d: %v", ErrInvalid, i, err)
				}
				out[i] = float32(f)
			default:
				return nil, fmt.Errorf("%w: vec value %d is a %T, not a number", ErrInvalid, i, e)
			}
		}
		return out, nil
	case string:
		return ParseVector(v)
	}
	return nil, fmt.Errorf("%w: vec takes a Vector, not a %T", ErrInvalid, x)
}

// checkVector refuses empty, oversized, all-zero and non-finite vectors.
func checkVector(v Vector) error {
	if len(v) == 0 || len(v) > MaxDims {
		return fmt.Errorf("%w: a vector has 1 to %d values, not %d", ErrInvalid, MaxDims, len(v))
	}
	zero := true
	for i, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%w: vector value %d is %v", ErrInvalid, i, x)
		}
		if x != 0 {
			zero = false
		}
	}
	if zero {
		return fmt.Errorf("%w: a vector of only zeros has no direction to compare", ErrInvalid)
	}
	return nil
}

// cosine returns the cosine distance between two stored vectors: 0 for the
// same direction, 1 for unrelated, 2 for opposite. A vector of zeros counts
// as unrelated to everything.
func cosine(a, b []byte) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("hypercrux: distance between vectors of %d and %d values", len(a)/4, len(b)/4)
	}
	if len(a) == 0 || len(a)%4 != 0 {
		return 0, fmt.Errorf("hypercrux: a vector blob of %d bytes isn't whole float32 values", len(a))
	}
	var dot, na, nb float64
	for i := 0; i < len(a); i += 4 {
		x := float64(math.Float32frombits(binary.LittleEndian.Uint32(a[i:])))
		y := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i:])))
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 1, nil
	}
	d := 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
	switch {
	case d < 0:
		d = 0
	case d > 2:
		d = 2
	}
	return d, nil
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
// optional SQL condition on the table's columns, such as "status = ?", with
// its values in args.
func (db *DB) Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error) {
	return nearest(db.run(), table, q, k, where, args)
}

// Nearest is DB.Nearest inside the transaction.
func (t *Tx) Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error) {
	return nearest(t.run(), table, q, k, where, args)
}

func nearest(r querier, table string, q Vector, k int, where string, args []any) ([]Hit, error) {
	if err := checkTable(table); err != nil {
		return nil, err
	}
	if err := checkVector(q); err != nil {
		return nil, err
	}
	if k < 1 || k > MaxK {
		return nil, fmt.Errorf("%w: k is from 1 to %d, not %d", ErrInvalid, MaxK, k)
	}
	var dims *int64
	err := r.QueryRow(`SELECT dims FROM hc_tables WHERE name = ?`, table).Scan(&dims)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no record table %s", ErrNotFound, table)
	}
	if err != nil {
		return nil, err
	}
	if dims == nil {
		return nil, nil // no vectors stored yet
	}
	if int(*dims) != len(q) {
		return nil, fmt.Errorf("%w: table %s holds vectors of %d values, and the query has %d", ErrInvalid, table, *dims, len(q))
	}
	query := `SELECT key, vec FROM ` + quote(table) + ` WHERE vec IS NOT NULL`
	if strings.TrimSpace(where) != "" {
		query += ` AND (` + where + `)`
	}
	rows, err := r.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := newMatcher(q)
	top := hitHeap{}
	var key, vec sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&key, &vec); err != nil {
			return nil, err
		}
		d, err := m.distance(vec)
		if err != nil {
			return nil, fmt.Errorf("hypercrux: the vector of %s: %w", key, err)
		}
		if len(top) == k && !before(d, key, top[0]) {
			continue
		}
		h := Hit{Key: string(key), Distance: d}
		if len(top) < k {
			heap.Push(&top, h)
		} else {
			top[0] = h
			heap.Fix(&top, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hits := []Hit(top)
	sort.Slice(hits, func(i, j int) bool { return before(hits[i].Distance, []byte(hits[i].Key), hits[j]) })
	return hits, nil
}

// before reports whether distance d with key comes before hit h: closer
// first, then by key.
func before(d float64, key []byte, h Hit) bool {
	if d != h.Distance {
		return d < h.Distance
	}
	return string(key) < h.Key
}

// hitHeap keeps the k best hits with the worst on top.
type hitHeap []Hit

func (h hitHeap) Len() int { return len(h) }
func (h hitHeap) Less(i, j int) bool {
	return before(h[j].Distance, []byte(h[j].Key), h[i])
}
func (h hitHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(x any)   { *h = append(*h, x.(Hit)) }
func (h *hitHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// matcher compares stored vectors with one query vector, whose values and
// length it works out once. It gives the same numbers as cosine(stored, q).
type matcher struct {
	q    []float64
	norm float64
}

func newMatcher(q Vector) *matcher {
	m := &matcher{q: make([]float64, len(q))}
	var n float64
	for i, x := range q {
		m.q[i] = float64(x)
		n += m.q[i] * m.q[i]
	}
	m.norm = n
	return m
}

func (m *matcher) distance(b []byte) (float64, error) {
	if len(b) != 4*len(m.q) {
		return 0, fmt.Errorf("it has %d bytes, not %d", len(b), 4*len(m.q))
	}
	var dot, nb float64
	q := m.q
	for i := range q {
		x := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])))
		dot += x * q[i]
		nb += x * x
	}
	if nb == 0 || m.norm == 0 {
		return 1, nil
	}
	d := 1 - dot/(math.Sqrt(nb)*math.Sqrt(m.norm))
	switch {
	case d < 0:
		d = 0
	case d > 2:
		d = 2
	}
	return d, nil
}
