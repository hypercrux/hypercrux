// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
)

// Store is the in-memory copy of a database. So far it holds the records
// with their fields, and each table's field list and vector size (S1), each
// table's keys in byte order, which Scan reads (S4), and each record's links
// both ways, which Neighbours and Walk read (S5). Its writes give the change
// lists the log writes, it takes the batches the log reads whole or not at
// all, and its Snapshot gives the copy as a compacted part (S3). The vector
// arrays come with V1.
//
// Many goroutines share a Store (S2). Reads go through Read, under the
// copy's lock held shared, and writes through a transaction from Begin,
// which takes the lock alone at its first change. The Store's own Reader
// methods take no lock: they serve Read's callback and the transaction,
// and like Put, Delete, Drop, Link, Unlink and Apply they serve a goroutine
// that has the store to itself, such as a test.
//
// A read changes nothing in the store, a cache included: readers share the
// lock, and a transaction reads beside them, without the lock, until its
// first change. A walk's working memory comes from a pool of the package's
// own, outside any store (walk.go).
type Store struct {
	// mu is the copy's lock. Read holds it shared. A transaction takes it
	// alone at its first change and keeps it until Commit or Rollback, so
	// readers never see a change that isn't committed. Go's RWMutex lets a
	// waiting writer in ahead of readers that come after it, so a stream of
	// reads can't hold a transaction off.
	mu sync.RWMutex
	// writer lets one transaction run at a time. Begin takes it, and the
	// transaction's end releases it, after mu.
	writer sync.Mutex
	// owner is the goroutine that began the open transaction, by the number
	// runtime.Stack shows, or 0 when none is open. held is true while that
	// transaction holds mu. They're what the check behind
	// errs.ErrInsideUpdate reads, from any goroutine.
	owner atomic.Int64
	held  atomic.Bool
	// tx is the open transaction, or nil. Only the goroutine using the
	// transaction reads it.
	tx *Tx

	records map[string]*record // the hash table, from key to record
	tables  map[string]*table  // by name
	// epoch moves on with every change to which tables there are and to
	// which keys each table holds, undone changes included, so a cursor
	// knows when to find its place again. Nothing else changes it.
	epoch uint64
	// lastType is the handle of the last link type a link was added with,
	// so a run of links of one type, as a compacted file gives them, makes
	// one call to unique.Make. Only writes use it.
	lastType linkType
}

var _ Reader = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{records: map[string]*record{}, tables: map[string]*table{}}
}

// table is one table's shape.
type table struct {
	name string
	// fields are the table's fields in their fixed order, each spelt as
	// the put that first named it spelt it. The list only grows, until the
	// table is dropped, so a field's place never changes.
	fields []string
	// index holds each field's place by its name with its letters in lower
	// case, as rules.Fold gives it, so names match regardless of case.
	index map[string]int
	// vec is the vector field's place, or -1 until a put names it.
	vec int
	// size is the vector size: 0 until the table's first vector, then that
	// vector's length until the table is dropped.
	size int
	// keys are the keys of the table's records in byte order, each with its
	// record (S4). A drop leaves them as they are, so the table goes whole.
	keys keyOrder
	// V1 keeps the table's vector array here.
}

// newTable adds an empty table called name, which the store hasn't got.
func (s *Store) newTable(name string) *table {
	s.changing(undo{op: undoNoTable, name: name})
	t := &table{name: name, index: map[string]int{}, vec: -1}
	s.tables[name] = t
	s.epoch++
	return t
}

// add puts a new field at the end of the table's list, and returns its
// place.
func (s *Store) add(t *table, name string) int {
	s.changing(undo{op: undoFields, table: t, n: len(t.fields)})
	p := len(t.fields)
	t.fields = append(t.fields, name)
	t.index[rules.Fold(name)] = p
	if rules.IsVec(name) {
		t.vec = p
	}
	return p
}

// setSize sets a table's vector size.
func (s *Store) setSize(t *table, size int) {
	s.changing(undo{op: undoSize, table: t, n: t.size})
	t.size = size
}

// record is one record.
type record struct {
	key string
	// out holds a half for each link from the record, with the record it's
	// to, and in, the reverse index, a half for each link to it, with the
	// record it's from (S5, halves.go). They come straight after the key,
	// so a walk that reads a record's key finds its lists in the same 64
	// bytes. A delete or a drop of the record leaves them as they are: it
	// takes each link out at its other end, so nothing can reach the record
	// any more, and undoing it puts those ends back.
	out, in links
	table   *table
	// fields are the fields that hold a value, in order of their places in
	// the table's list, with nulls and the vector left out. A put replaces
	// the slice whole and never changes one in place, so a slice a read
	// handed out stays as it was.
	fields []FieldValue
	// vec is the record's vector, or nil. It stands in for V1's slot: V1
	// keeps the vector in the table's vector array, and the record holds
	// the slot's number here.
	vec []float32
}

// Table returns the shape of the table called name, or false when there's
// none. Its Fields share the store's memory: they're good until the
// table's next change, and nothing may change them.
func (s *Store) Table(name string) (Table, bool) {
	t := s.tables[name]
	if t == nil {
		return Table{Vec: -1}, false
	}
	return Table{Name: t.name, Fields: t.fields[:len(t.fields):len(t.fields)], Vec: t.vec, Size: t.size}, true
}

// Find returns the place of the field whose name matches name regardless
// of ASCII case, as Put and SQL match names, or -1 when the table has
// none. It looks through Fields, so it works on a Table that a fake store
// builds too. The store's own writes use an index instead.
func (t Table) Find(name string) int {
	for i, f := range t.Fields {
		if rules.SameName(f, name) {
			return i
		}
	}
	return -1
}

// Get returns the record with this key. A key that breaks 0.x's rules
// gives an error that wraps errs.ErrInvalid, and a key with no record one
// that wraps errs.ErrNotFound, as in 0.x.
func (s *Store) Get(key string) (Record, error) {
	r, err := s.existing(key)
	if err != nil {
		return Record{}, err
	}
	return r.read(), nil
}

// existing returns the record with this key, with 0.x's errors: one that
// wraps errs.ErrInvalid for a key that breaks the rules, and one that wraps
// errs.ErrNotFound for a key with no record. Get, Link, Neighbours and Walk
// check their keys with it, as 0.x's mustExist does.
func (s *Store) existing(key string) (*record, error) {
	if _, err := rules.TableOf(key); err != nil {
		return nil, err
	}
	r := s.records[key]
	if r == nil {
		return nil, fmt.Errorf("%w: %s", errs.ErrNotFound, key)
	}
	return r, nil
}

// read gives the record as Reader hands it out, sharing the store's
// memory. The slices can't grow into the store's.
func (r *record) read() Record {
	return Record{Key: r.key, Fields: r.fields[:len(r.fields):len(r.fields)], Vec: r.vec[:len(r.vec):len(r.vec)]}
}

// notYet is the error of a method whose task hasn't written it yet.
func notYet(method, task string) error {
	return fmt.Errorf("store: %s comes with task %s: %w", method, task, errors.ErrUnsupported)
}

// Nearest comes with V1.
func (s *Store) Nearest(table string, q []float32, k int, keep Filter) ([]Hit, error) {
	return nil, notYet("Nearest", "V1")
}
