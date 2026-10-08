// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package betax

import (
	"database/sql"
	"maps"

	c "github.com/hypercrux/hypercrux/beta/conformance"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// Engine is the HyperCrux Beta as a conformance.Engine.
type Engine struct{}

var _ c.Engine = Engine{}

func (Engine) Name() string { return "HyperCrux " + hc.Version + " in Go" }

func (Engine) Open(path string) (c.DB, error) {
	db, err := hc.Open(path)
	if err != nil {
		return nil, err
	}
	return &database{handle{db}, db}, nil
}

func (Engine) ErrNotFound() error { return hc.ErrNotFound }
func (Engine) ErrInvalid() error  { return hc.ErrInvalid }

func (Engine) ParseVector(s string) (c.Vector, error) {
	v, err := hc.ParseVector(s)
	return c.Vector(v), err
}

// Skip names the tests that are still out of reach, each with the task
// that makes it pass. beta/SQL.md names every test in the suite as one the
// Beta must pass, so the list shrinks as the tasks are done, and is empty
// once G4 and F9 are.
func (Engine) Skip() map[string]string { return maps.Clone(later) }

// later holds the tests that wait for a later task, by name, with the task
// each waits for. A test that needs more than one task names the last of
// them on the board, and the others after it.
var later = map[string]string{
	"RulesForLongKeysAndLinkTypes": "waits for task G4, which brings Exec, after G2's Link",
	"TableNamesLikeHyperCruxsOwn":  "waits for task G4, which brings Exec, after G2's Link, Neighbours and Nearest",
	"NearestIsExact":               "waits for task G4, which brings Nearest with a filter, after G2's Nearest",
	"WalkInSQL":                    "waits for task G4, which brings SQL, after G2's Link",
	"OneStatementCrossesAllFour":   "waits for task G4, which brings SQL, after G2's Link",
	"VectorAsQueryArgument":        "waits for task G4, which brings SQL",
	"PlainSQLFollowsTheRules":      "waits for task G4, which brings SQL, after G2's Link and Neighbours",
	"UpdateIsAllOrNothing":         "waits for task G4, which brings walk() in SQL, after G2's Link and Nearest",
	"KilledWritersNeverLeaveAMess": "waits for task G4, which brings Query, after G2's links and F3's check of the end of the log",
	"ProcessesShareAFile":          "waits for task F9, which has the public package's reads follow other processes' commits, after F6's following",
}

// hcHandle is what *hc.DB and *hc.Tx have in common.
type hcHandle interface {
	Get(key string) (hc.Fields, error)
	Put(key string, f hc.Fields) error
	Delete(key string) error
	Scan(prefix, after string, limit int) ([]hc.Record, error)
	Link(from, typ, to string) error
	Unlink(from, typ, to string) error
	Neighbours(key string, dir hc.Direction, typ string) ([]hc.Link, error)
	Walk(key string, dir hc.Direction, typ string, depth int) ([]hc.Step, error)
	Nearest(table string, q hc.Vector, k int, where string, args ...any) ([]hc.Hit, error)
	Drop(table string) error
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

var (
	_ hcHandle = (*hc.DB)(nil)
	_ hcHandle = (*hc.Tx)(nil)
)

// handle turns the suite's types into 0.x's and back.
type handle struct{ h hcHandle }

func toHC(f c.Fields) hc.Fields {
	if f == nil {
		return nil
	}
	out := make(hc.Fields, len(f))
	for k, v := range f {
		if vec, ok := v.(c.Vector); ok {
			v = hc.Vector(vec)
		}
		out[k] = v
	}
	return out
}

func fromHC(f hc.Fields) c.Fields {
	if f == nil {
		return nil
	}
	out := make(c.Fields, len(f))
	for k, v := range f {
		if vec, ok := v.(hc.Vector); ok {
			v = c.Vector(vec)
		}
		out[k] = v
	}
	return out
}

func dir(d c.Direction) hc.Direction {
	switch d {
	case c.In:
		return hc.In
	case c.Both:
		return hc.Both
	}
	return hc.Out
}

func (h handle) Get(key string) (c.Fields, error) {
	f, err := h.h.Get(key)
	return fromHC(f), err
}

func (h handle) Put(key string, f c.Fields) error { return h.h.Put(key, toHC(f)) }
func (h handle) Delete(key string) error          { return h.h.Delete(key) }

func (h handle) Scan(prefix, after string, limit int) ([]c.Record, error) {
	recs, err := h.h.Scan(prefix, after, limit)
	var out []c.Record
	for _, r := range recs {
		out = append(out, c.Record{Key: r.Key, Fields: fromHC(r.Fields)})
	}
	return out, err
}

func (h handle) Link(from, typ, to string) error   { return h.h.Link(from, typ, to) }
func (h handle) Unlink(from, typ, to string) error { return h.h.Unlink(from, typ, to) }

func (h handle) Neighbours(key string, d c.Direction, typ string) ([]c.Link, error) {
	links, err := h.h.Neighbours(key, dir(d), typ)
	var out []c.Link
	for _, l := range links {
		out = append(out, c.Link{From: l.From, Type: l.Type, To: l.To})
	}
	return out, err
}

func (h handle) Walk(key string, d c.Direction, typ string, depth int) ([]c.Step, error) {
	steps, err := h.h.Walk(key, dir(d), typ, depth)
	var out []c.Step
	for _, s := range steps {
		out = append(out, c.Step{Key: s.Key, Depth: s.Depth})
	}
	return out, err
}

func (h handle) Nearest(table string, q c.Vector, k int, where string, args ...any) ([]c.Hit, error) {
	hits, err := h.h.Nearest(table, hc.Vector(q), k, where, args...)
	var out []c.Hit
	for _, x := range hits {
		out = append(out, c.Hit{Key: x.Key, Distance: x.Distance})
	}
	return out, err
}

func (h handle) Drop(table string) error { return h.h.Drop(table) }

func (h handle) Exec(query string, args ...any) (sql.Result, error) {
	return h.h.Exec(query, args...)
}

func (h handle) Query(query string, args ...any) (*sql.Rows, error) {
	return h.h.Query(query, args...)
}

func (h handle) QueryRow(query string, args ...any) *sql.Row {
	return h.h.QueryRow(query, args...)
}

type database struct {
	handle
	db *hc.DB
}

func (d *database) Update(fn func(tx c.Handle) error) error {
	return d.db.Update(func(tx *hc.Tx) error { return fn(handle{tx}) })
}

func (d *database) Check() (c.Report, error) {
	r, err := d.db.Check()
	return c.Report{Tables: r.Tables, Records: r.Records, Links: r.Links, Vectors: r.Vectors, Problems: r.Problems}, err
}

func (d *database) SQL() *sql.DB { return d.db.SQL() }
func (d *database) Close() error { return d.db.Close() }
