// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"encoding/json"
	"slices"
	"strconv"
	"sync"

	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// Run runs the plan with f, whose Args fill the statement's ? marks, one
// for each, and whose Now is the statement's moment. It works out LIMIT and
// OFFSET and gives the rows, which read the store as they're pulled. A plan
// runs once: the subqueries it works out once keep their answers.
func (p *Plan) Run(f *Frame) (Rows, error) {
	if p.ran {
		return nil, &Error{Pos: p.sel.At, Msg: "a plan runs once: prepare the statement again"}
	}
	p.ran = true
	if len(f.Args) != p.sel.Params() {
		return nil, &Error{Pos: p.sel.At, Msg: "the statement has " + strconv.Itoa(p.sel.Params()) + " ? marks and " + strconv.Itoa(len(f.Args)) + " arguments: it takes one argument for each"}
	}
	frame := *f
	frame.Row = nil
	n, skip, err := Bounds(p.limit, p.offset, p.scope, &frame)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return &noRows{}, nil
	}
	var rows Rows
	switch p.shape {
	case ShapeNearest:
		return p.gate(&nearestRows{p: p, frame: frame, n: n, skip: skip}, &frame), nil
	case ShapeRow:
		rows = &oneRow{}
	case ShapeScan:
		if p.order == byKeyBackwards {
			rows = &backwardsRows{r: p.r, src: p.table.RowSource, row: make([]value.Value, p.width)}
		} else {
			rows = Scan(p.r, p.table.RowSource)
		}
	case ShapeLookup:
		rows = &lookupRows{r: p.r, lk: p.lookup, frame: frame, src: p.table, row: make([]value.Value, p.width), back: p.order == byKeyBackwards}
	case ShapeWalk, ShapeWalkJoin:
		rows = &walkRows{r: p.r, src: p.walk, frame: frame, row: make([]value.Value, p.width)}
		if p.outer != nil {
			rows = Filter(rows, p.outer, &frame)
		}
		if p.shape == ShapeWalkJoin {
			rows = &joinRows{in: rows, r: p.r, walk: p.walk, table: p.table, keyAt: p.keyAt, row: make([]value.Value, p.width)}
		}
	}
	rows = p.gate(rows, &frame)
	if p.conds != nil {
		rows = Filter(rows, p.conds, &frame)
	}
	return p.tail(rows, n, skip, &frame), nil
}

// tail puts the operators after WHERE on rows, as Q4.md sets them out.
func (p *Plan) tail(rows Rows, n, skip int64, f *Frame) Rows {
	switch {
	case p.aggs != nil:
		return Project(Limit(Aggregate(rows, p.aggs, f), n, skip), p.cols, f)
	case p.order == sorted:
		k := n + max(skip, 0)
		if n < 0 || k < 0 {
			return Limit(Sort(rows, p.cols, p.keys, f), n, skip)
		}
		return Limit(TopK(rows, p.cols, p.keys, k, f), n, skip)
	}
	return Project(Limit(rows, n, skip), p.cols, f)
}

// gate puts the WHERE terms worked out once in front of rows: at the first
// Next they're worked out in turn, before rows starts, and when one is
// false or NULL there are no rows, and rows never starts.
func (p *Plan) gate(rows Rows, f *Frame) Rows {
	if len(p.once) == 0 {
		return rows
	}
	return &gateRows{in: rows, conds: p.once, frame: *f}
}

type gateRows struct {
	in    Rows
	conds []Cond
	frame Frame
	err   error
	ran   bool
	done  bool
}

func (g *gateRows) Next() bool {
	if g.done {
		return false
	}
	if !g.ran {
		g.ran = true
		for _, c := range g.conds {
			ok, err := c(&g.frame)
			if err != nil || !ok {
				g.err, g.done = err, true
				return false
			}
		}
	}
	if !g.in.Next() {
		g.err, g.done = g.in.Err(), true
		return false
	}
	return true
}

func (g *gateRows) Row() []value.Value { return g.in.Row() }
func (g *gateRows) Err() error         { return g.err }

func (g *gateRows) Close() {
	g.done = true
	g.in.Close()
}

// noRows has no rows, for a LIMIT of 0.
type noRows struct{}

func (noRows) Next() bool         { return false }
func (noRows) Row() []value.Value { return nil }
func (noRows) Err() error         { return nil }
func (noRows) Close()             {}

// oneRow is the one row with no fields of a query without a FROM.
type oneRow struct{ given bool }

func (o *oneRow) Next() bool {
	if o.given {
		return false
	}
	o.given = true
	return true
}

func (o *oneRow) Row() []value.Value { return []value.Value{} }
func (o *oneRow) Err() error         { return nil }
func (o *oneRow) Close()             { o.given = true }

// backwardsRows gives a table's records in key order backwards, for ORDER
// BY key DESC, as SQLite reads the key's index backwards: it reads them all
// at its first Next, then hands them on from the last.
type backwardsRows struct {
	r    store.Reader
	src  *RowSource
	recs []store.Record
	row  []value.Value
	at   int
	err  error
	ran  bool
}

func (b *backwardsRows) Next() bool {
	if !b.ran {
		b.ran = true
		c, err := b.r.Scan(b.src.table+":", "")
		if err != nil {
			b.err = err
			return false
		}
		for rec, ok := c.Next(); ok; rec, ok = c.Next() {
			b.recs = append(b.recs, rec)
		}
		b.at = len(b.recs)
	}
	if b.at == 0 {
		b.recs = nil
		return false
	}
	b.at--
	b.src.Fill(b.row, b.recs[b.at])
	return true
}

func (b *backwardsRows) Row() []value.Value { return b.row }
func (b *backwardsRows) Err() error         { return b.err }

func (b *backwardsRows) Close() {
	b.ran, b.at, b.recs = true, 0, nil
}

// lookupRows gives the records a key lookup names, in key order or
// backwards. It works out the lookup's term at its first Next.
type lookupRows struct {
	r     store.Reader
	lk    *keyLookup
	frame Frame
	src   *planSource
	row   []value.Value
	keys  []string
	back  bool
	err   error
	ran   bool
	done  bool
}

func (l *lookupRows) Next() bool {
	if l.done {
		return false
	}
	if !l.ran {
		l.ran = true
		if l.keys, l.err = l.lk.keys(&l.frame); l.err != nil {
			l.done = true
			return false
		}
		if l.back {
			slices.Reverse(l.keys)
		}
	}
	for len(l.keys) > 0 {
		k := l.keys[0]
		l.keys = l.keys[1:]
		if !inTable(k, l.src.table.Name) {
			continue
		}
		rec, ok, err := getRecord(l.r, k)
		if err != nil {
			l.err, l.done = err, true
			return false
		}
		if ok {
			l.src.Fill(l.row, rec)
			return true
		}
	}
	l.done = true
	return false
}

func (l *lookupRows) Row() []value.Value { return l.row }
func (l *lookupRows) Err() error         { return l.err }
func (l *lookupRows) Close()             { l.done = true }

// walkRows gives a walk's steps as rows: the key and the depth for
// walk(...), and the key in value for json_each(walk(...)), in the walk's
// order. It works out the walk at its first Next.
type walkRows struct {
	r     store.Reader
	src   *planSource
	frame Frame
	row   []value.Value
	steps []store.Step
	err   error
	ran   bool
	done  bool
}

func (w *walkRows) Next() bool {
	if w.done {
		return false
	}
	if !w.ran {
		w.ran = true
		vals, err := walkArgs(w.src.args, &w.frame)
		if err == nil {
			w.steps, err = walkSteps(w.r, w.src.src.Walk.At, vals)
		}
		if err != nil {
			w.err, w.done = err, true
			return false
		}
		if plant == "query/walk-join-drops-a-depth" && len(w.steps) > 0 {
			last := w.steps[len(w.steps)-1].Depth
			for len(w.steps) > 0 && w.steps[len(w.steps)-1].Depth == last {
				w.steps = w.steps[:len(w.steps)-1]
			}
		}
	}
	if len(w.steps) == 0 {
		w.done = true
		return false
	}
	st := w.steps[0]
	w.steps = w.steps[1:]
	base := w.src.Base
	if w.src.kind == eachSource {
		w.row[base+eachValue] = value.Text(st.Key)
	} else {
		w.row[base], w.row[base+1] = value.Text(st.Key), value.Int(int64(st.Depth))
	}
	return true
}

func (w *walkRows) Row() []value.Value { return w.row }
func (w *walkRows) Err() error         { return w.err }
func (w *walkRows) Close()             { w.done = true }

// joinRows joins a walk's rows to a table: for each, the record of the
// table whose key the walk reached, and none when the record is another
// table's.
type joinRows struct {
	in    Rows
	r     store.Reader
	walk  *planSource
	table *planSource
	keyAt int
	row   []value.Value
	err   error
	done  bool
}

func (j *joinRows) Next() bool {
	for !j.done {
		if !j.in.Next() {
			j.err, j.done = j.in.Err(), true
			break
		}
		row := j.in.Row()
		k := row[j.keyAt]
		if k.Kind() != value.KindText || !inTable(k.Raw(), j.table.table.Name) {
			continue
		}
		rec, ok, err := getRecord(j.r, k.Raw())
		if err != nil {
			j.err, j.done = err, true
			break
		}
		if !ok {
			continue
		}
		b := j.walk.Base
		copy(j.row[b:b+len(j.walk.Columns)], row[b:b+len(j.walk.Columns)])
		j.table.Fill(j.row, rec)
		return true
	}
	return false
}

func (j *joinRows) Row() []value.Value { return j.row }
func (j *joinRows) Err() error         { return j.err }

func (j *joinRows) Close() {
	j.done = true
	j.in.Close()
}

// nearestRows is a nearest search, or the sort that stands in for it when
// the search can't take the query vector, decided at its first Next, once
// the WHERE's terms worked out once have passed. It gives the result rows.
type nearestRows struct {
	p       *Plan
	frame   Frame
	n, skip int64
	out     Rows
	err     error
	ran     bool
	done    bool
}

func (x *nearestRows) Next() bool {
	if x.done {
		return false
	}
	if !x.ran {
		x.ran = true
		if x.out, x.err = x.p.search(&x.frame, x.n, x.skip); x.err != nil {
			x.done = true
			return false
		}
	}
	if !x.out.Next() {
		x.err, x.done = x.out.Err(), true
		return false
	}
	return true
}

func (x *nearestRows) Row() []value.Value { return x.out.Row() }
func (x *nearestRows) Err() error         { return x.err }

func (x *nearestRows) Close() {
	x.done = true
	if x.out != nil {
		x.out.Close()
	}
}

// search runs the store's nearest search for the k closest, LIMIT plus
// OFFSET, with WHERE's other terms as its filter, then Limit and Project.
// When the search can't give what a sort would, it sorts: when q is NULL,
// isn't a vector, or holds zeros, NaN or infinity, which distance() takes
// apart from the search, or is of another size than the table's vectors,
// or when the LIMIT is negative or k is past store.MaxK. The sort then
// raises distance()'s errors where 0.x would, at the first row that
// reaches it, and not at all when no row does.
func (p *Plan) search(f *Frame, n, skip int64) (Rows, error) {
	t := p.table
	k := n + max(skip, 0)
	if plant == "query/nearest-k-is-the-limit" {
		k = n
	}
	if n > 0 && k > 0 && k <= store.MaxK {
		if q, ok := p.queryVector(f); ok {
			hits, err := p.r.Nearest(t.table.Name, q, int(k), p.filter(f))
			if err != nil {
				return nil, err
			}
			rows := &hitRows{r: p.r, src: t.RowSource, hits: hits, row: make([]value.Value, p.width)}
			return Project(Limit(rows, n, skip), p.cols, f), nil
		}
	}
	return p.tail(Filter(Scan(p.r, t.RowSource), p.conds, f), n, skip, f), nil
}

// queryVector works out the query vector and gives it as the search takes
// it: bytes of whole float32 values, or text holding a JSON array of
// numbers as vector() reads it, with 1 to 65,536 values, all finite and not
// all zero, as many as the table's vectors have. A table with no vectors
// takes any query, and the search finds nothing in it.
func (p *Plan) queryVector(f *Frame) ([]float32, bool) {
	v, err := p.near.q(f)
	if err != nil {
		return nil, false
	}
	var q []float32
	switch kind(v) {
	case value.KindBytes:
		raw := v.Raw()
		if len(raw) == 0 || len(raw)%4 != 0 {
			return nil, false
		}
		q = decodeVector(make([]float32, len(raw)/4), raw)
	case value.KindText:
		var xs []float64
		if json.Unmarshal([]byte(v.Raw()), &xs) != nil {
			return nil, false
		}
		q = make([]float32, len(xs))
		for i, x := range xs {
			q[i] = float32(x)
		}
	default:
		return nil, false
	}
	if rules.Vector(q) != nil {
		return nil, false
	}
	t, ok := p.r.Table(p.table.table.Name)
	if !ok || t.Size != 0 && t.Size != len(q) {
		return nil, false
	}
	return q, true
}

// filter gives the search's Filter: each record filled into a row of its
// own, and the terms worked out over it in turn, as Filter does. The
// search may call it from several goroutines, so each takes a row and a
// Frame from a pool. With no terms there's no filter.
func (p *Plan) filter(f *Frame) store.Filter {
	conds := p.near.filter
	if len(conds) == 0 || plant == "query/nearest-ignores-filter" {
		return nil
	}
	src, width := p.table.RowSource, p.width
	pool := &sync.Pool{New: func() any {
		fr := &filterRow{frame: *f, row: make([]value.Value, width)}
		fr.frame.Row = &fr.row
		return fr
	}}
	return func(rec store.Record) (bool, error) {
		fr := pool.Get().(*filterRow)
		defer pool.Put(fr)
		src.Fill(fr.row, rec)
		for _, c := range conds {
			if ok, err := c(&fr.frame); err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	}
}

type filterRow struct {
	frame Frame
	row   []value.Value
}

// hitRows gives the records a search found, in its order.
type hitRows struct {
	r    store.Reader
	src  *RowSource
	hits []store.Hit
	row  []value.Value
	err  error
}

func (h *hitRows) Next() bool {
	for len(h.hits) > 0 {
		k := h.hits[0].Key
		h.hits = h.hits[1:]
		rec, ok, err := getRecord(h.r, k)
		if err != nil {
			h.err, h.hits = err, nil
			return false
		}
		if ok {
			h.src.Fill(h.row, rec)
			return true
		}
	}
	return false
}

func (h *hitRows) Row() []value.Value { return h.row }
func (h *hitRows) Err() error         { return h.err }
func (h *hitRows) Close()             { h.hits = nil }
