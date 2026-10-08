// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package betax

import (
	"database/sql"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hypercrux/hypercrux/beta/conformance"
)

// TestConformanceAfterAReopen runs the suite on the Beta with every read
// through a database made on a fresh open of the file: the adapter closes
// the database and opens it again before each read. So every check a test
// makes through the database reads what the file holds. Reads through a
// transaction stay inside its Update. When a test closes a database, every key the test named is
// read once more, then again after one last reopen, and the two reads must
// agree, as must Check's counts. It skips what the plain run skips, and two
// tests that a reopen before every read doesn't suit.
func TestConformanceAfterAReopen(t *testing.T) {
	e := reopening{log: &problems{}}
	conformance.Run(t, e)
	for _, p := range e.log.list {
		t.Error(p)
	}
	if e.log.reopens < 20 || e.log.closes < 5 || e.log.keys < 20 {
		t.Errorf("the run reopened %d times, closed %d databases and compared %d keys after a last reopen", e.log.reopens, e.log.closes, e.log.keys)
	}
	t.Logf("%d reopens; %d databases closed, with %d keys read the same before and after a last reopen", e.log.reopens, e.log.closes, e.log.keys)
}

// reopening is the Beta as a conformance.Engine whose databases reopen the
// file before every read.
type reopening struct{ log *problems }

// problems collects what the reopens found, since the suite ignores the
// error Close returns.
type problems struct {
	mu      sync.Mutex
	list    []string
	reopens int // opens of a file that a database had open, before a read
	closes  int // databases compared at Close
	keys    int // keys compared there
}

func (p *problems) add(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func (e reopening) Name() string                                     { return Engine{}.Name() + ", reopened before every read" }
func (e reopening) ErrNotFound() error                               { return Engine{}.ErrNotFound() }
func (e reopening) ErrInvalid() error                                { return Engine{}.ErrInvalid() }
func (e reopening) ParseVector(s string) (conformance.Vector, error) { return Engine{}.ParseVector(s) }

func (e reopening) Skip() map[string]string {
	skip := maps.Clone(later)
	skip["GoroutinesShareADB"] = "its goroutines share one database, which a reopen would close under them; the plain run has it"
	skip["KilledWritersNeverLeaveAMess"] = "it opens the file afresh after every kill already, and a reopen before each of its reads would read the file thousands of times; the plain run has it"
	return skip
}

func (e reopening) Open(path string) (conformance.DB, error) {
	db, err := Engine{}.Open(path)
	if err != nil {
		return nil, err
	}
	return &reopened{path: path, db: db, log: e.log, keys: map[string]bool{}}, nil
}

// reopened is a database that reopens its file before every read. One
// goroutine uses it at a time, apart from Update's function, which works
// through its transaction.
type reopened struct {
	mu     sync.Mutex
	path   string
	db     conformance.DB // the database on the file's latest open
	log    *problems
	keys   map[string]bool // every key the test has named
	closed bool
}

// fresh closes the database and opens the file again, and returns the new
// database.
func (d *reopened) fresh() (conformance.DB, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.db.Close(); err != nil {
		return nil, fmt.Errorf("closing to reopen: %w", err)
	}
	db, err := Engine{}.Open(d.path)
	if err != nil {
		return nil, fmt.Errorf("reopening: %w", err)
	}
	d.db = db
	d.log.mu.Lock()
	d.log.reopens++
	d.log.mu.Unlock()
	return db, nil
}

// now returns the database as it is, for a write.
func (d *reopened) now() conformance.DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.db
}

func (d *reopened) name(keys ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range keys {
		d.keys[k] = true
	}
}

func (d *reopened) Get(key string) (conformance.Fields, error) {
	d.name(key)
	db, err := d.fresh()
	if err != nil {
		return nil, err
	}
	return db.Get(key)
}

func (d *reopened) Put(key string, f conformance.Fields) error {
	d.name(key)
	return d.now().Put(key, f)
}

func (d *reopened) Delete(key string) error {
	d.name(key)
	return d.now().Delete(key)
}

func (d *reopened) Scan(prefix, after string, limit int) ([]conformance.Record, error) {
	db, err := d.fresh()
	if err != nil {
		return nil, err
	}
	return db.Scan(prefix, after, limit)
}

func (d *reopened) Link(from, typ, to string) error {
	d.name(from, to)
	return d.now().Link(from, typ, to)
}

func (d *reopened) Unlink(from, typ, to string) error {
	d.name(from, to)
	return d.now().Unlink(from, typ, to)
}

func (d *reopened) Neighbours(key string, dir conformance.Direction, typ string) ([]conformance.Link, error) {
	d.name(key)
	db, err := d.fresh()
	if err != nil {
		return nil, err
	}
	return db.Neighbours(key, dir, typ)
}

func (d *reopened) Walk(key string, dir conformance.Direction, typ string, depth int) ([]conformance.Step, error) {
	d.name(key)
	db, err := d.fresh()
	if err != nil {
		return nil, err
	}
	return db.Walk(key, dir, typ, depth)
}

func (d *reopened) Nearest(table string, q conformance.Vector, k int, where string, args ...any) ([]conformance.Hit, error) {
	db, err := d.fresh()
	if err != nil {
		return nil, err
	}
	return db.Nearest(table, q, k, where, args...)
}

func (d *reopened) Drop(table string) error { return d.now().Drop(table) }

func (d *reopened) Exec(query string, args ...any) (sql.Result, error) {
	return d.now().Exec(query, args...)
}

func (d *reopened) Query(query string, args ...any) (*sql.Rows, error) {
	db, err := d.fresh()
	if err != nil {
		return nil, err
	}
	return db.Query(query, args...)
}

// QueryRow reopens too. A *sql.Row can't carry an error from here, so a
// reopen that fails leaves the database as it was, and the problem is
// reported at the end.
func (d *reopened) QueryRow(query string, args ...any) *sql.Row {
	db, err := d.fresh()
	if err != nil {
		d.log.add("%s: %v", d.path, err)
		return d.now().QueryRow(query, args...)
	}
	return db.QueryRow(query, args...)
}

func (d *reopened) Check() (conformance.Report, error) {
	db, err := d.fresh()
	if err != nil {
		return conformance.Report{}, err
	}
	return db.Check()
}

func (d *reopened) SQL() *sql.DB { return d.now().SQL() }

// Update runs fn through the database as it is. Its transaction reads its
// own changes, which aren't in the file yet.
func (d *reopened) Update(fn func(tx conformance.Handle) error) error {
	return d.now().Update(func(tx conformance.Handle) error { return fn(naming{tx, d}) })
}

// Close reads every key the test named, reopens the file and reads them
// again, and compares the two, with Check's counts. Then it closes the
// database. Closing again does nothing.
func (d *reopened) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	keys := slices.Sorted(maps.Keys(d.keys))
	d.mu.Unlock()

	before := describe(d.now(), keys)
	db, err := d.fresh()
	if err != nil {
		d.log.add("%s: %v", d.path, err)
		return err
	}
	after := describe(db, keys)
	for i := range before {
		if before[i] != after[i] {
			d.log.add("%s: after a reopen, %s\nwhere before it, %s", d.path, after[i], before[i])
		}
	}
	d.log.mu.Lock()
	d.log.closes++
	d.log.keys += len(keys)
	d.log.mu.Unlock()
	return db.Close()
}

// describe reads each key, and Check, and says what each gave, with every
// number's bits, so that -0 and 0 differ.
func describe(db conformance.DB, keys []string) []string {
	var out []string
	for _, k := range keys {
		f, err := db.Get(k)
		out = append(out, fmt.Sprintf("Get(%q) gives %s, %v", k, show(f), err))
	}
	rep, err := db.Check()
	return append(out, fmt.Sprintf("Check gives %+v, %v", rep, err))
}

func show(f conformance.Fields) string {
	names := slices.Sorted(maps.Keys(f))
	parts := make([]string, len(names))
	for i, n := range names {
		var v string
		switch x := f[n].(type) {
		case float64:
			v = fmt.Sprintf("float64 %016x", math.Float64bits(x))
		case conformance.Vector:
			bits := make([]string, len(x))
			for j, y := range x {
				bits[j] = fmt.Sprintf("%08x", math.Float32bits(y))
			}
			v = "Vector [" + strings.Join(bits, " ") + "]"
		default:
			v = fmt.Sprintf("%T %#v", x, x)
		}
		parts[i] = n + ": " + v
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// naming is a transaction that notes the keys the test names through it.
type naming struct {
	conformance.Handle
	d *reopened
}

func (n naming) Get(key string) (conformance.Fields, error) {
	n.d.name(key)
	return n.Handle.Get(key)
}

func (n naming) Put(key string, f conformance.Fields) error {
	n.d.name(key)
	return n.Handle.Put(key, f)
}

func (n naming) Delete(key string) error {
	n.d.name(key)
	return n.Handle.Delete(key)
}

func (n naming) Link(from, typ, to string) error {
	n.d.name(from, to)
	return n.Handle.Link(from, typ, to)
}
