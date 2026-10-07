// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package conformance

import (
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
)

// The limits both engines keep, as in 0.x.
const (
	MaxKeyLen = 1024
	MaxDepth  = 32
)

// Fields are a record's values by field name, as in 0.x. The field "vec"
// holds a Vector.
type Fields map[string]any

// Vector is a record's vector.
type Vector []float32

// Bytes returns the vector as 0.x stores it: float32 values, little-endian.
func (v Vector) Bytes() []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

// String returns the vector as a JSON array.
func (v Vector) String() string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(float64(x), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Value lets a Vector be passed to SQL as a blob, as in distance(vec, ?).
func (v Vector) Value() (driver.Value, error) { return v.Bytes(), nil }

// Direction picks which links to follow from a record.
type Direction int

const (
	Out Direction = iota
	In
	Both
)

func (d Direction) String() string {
	switch d {
	case Out:
		return "out"
	case In:
		return "in"
	case Both:
		return "both"
	}
	return "direction(" + strconv.Itoa(int(d)) + ")"
}

// Link is a typed, one-way connection between two records.
type Link struct {
	From, Type, To string
}

// Step is one record a walk reached, and how many links away it is.
type Step struct {
	Key   string
	Depth int
}

// Hit is one result of Nearest.
type Hit struct {
	Key      string
	Distance float64
}

// Record is a key with its fields.
type Record struct {
	Key    string
	Fields Fields
}

// Report is what Check found.
type Report struct {
	Tables, Records, Links, Vectors int
	Problems                        []string
}

// OK reports whether Check found no problems.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Engine is what an engine's adapter provides.
type Engine interface {
	// Name says which engine this is, for test output.
	Name() string
	// Open opens or creates the database at path.
	Open(path string) (DB, error)
	// ErrNotFound and ErrInvalid are the engine's two sentinel errors.
	ErrNotFound() error
	ErrInvalid() error
	// ParseVector reads a vector written as a JSON array.
	ParseVector(s string) (Vector, error)
	// Skip names the tests this engine leaves out, with the reason for each.
	Skip() map[string]string
}

// Handle is what a database and a transaction have in common.
type Handle interface {
	Get(key string) (Fields, error)
	Put(key string, f Fields) error
	Delete(key string) error
	Scan(prefix, after string, limit int) ([]Record, error)
	Link(from, typ, to string) error
	Unlink(from, typ, to string) error
	Neighbours(key string, dir Direction, typ string) ([]Link, error)
	Walk(key string, dir Direction, typ string, depth int) ([]Step, error)
	Nearest(table string, q Vector, k int, where string, args ...any) ([]Hit, error)
	Drop(table string) error
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// DB is an open database.
type DB interface {
	Handle
	// Update runs fn in one write transaction.
	Update(fn func(tx Handle) error) error
	// Check confirms that the four ways into the database agree.
	Check() (Report, error)
	// SQL returns the database/sql handle behind Query and Exec.
	SQL() *sql.DB
	Close() error
}
