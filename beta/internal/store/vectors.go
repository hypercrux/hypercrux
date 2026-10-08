// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"
	"math"
	"slices"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/beta/internal/vecmath"
)

// Each table's vectors (V1). A table keeps its records' vectors in its
// vector array, and a record with a vector holds the number of its slot
// there. The array has three parts, each indexed by slot:
//
//   - the values, in blocks of float32s, with each slot's values back to
//     back and the slots one after another. The blocks hold no pointers, so
//     Go's garbage collector never looks inside them, and a search streams
//     through them in order;
//   - each slot's norm, a float64 worked out when its vector is written, so
//     a search works out one dot product a vector;
//   - each slot's record, or nil for a free slot. That nil is the mark on a
//     free slot. A search skips the slot, and nothing else reads its values.
//
// Slot i is in block i >> shift, starting at value (i & (1<<shift - 1)) *
// size. A full block holds 1 << shift slots, the most whose values fit in
// blockBytes, and at least one: 512 slots of 384 values, 128 of 1,536, or 4
// of 65,536. The first block starts with room for one slot and doubles
// until it's full, and each block after it is made full when the one before
// it fills. So growing copies the first block at most, a table with a few
// vectors takes room for a few, and the array never holds two copies of its
// values. In a quick check on a two-core machine, one block grown by append
// took 1.1 seconds to take 100,000 vectors of 384 values, and up to 620 MB
// at once for their 154 MB, where blocks of 512 slots took 80 ms and 153 MB.
// The blocks never shrink: a slot a rollback takes back leaves its room to
// the next vector, and the array goes with its table.
//
// Besides its values, a vector takes 8 bytes for its norm and 8 for its
// record's pointer, plus the room those two slices keep for growing, and the
// last block of each table can be short of vectors by up to a block. A
// record holds its slot's number in 8 bytes, where it held a slice of 24
// before V1, so every record is 16 bytes smaller.
//
// A record's first vector takes the free slot freed last, or a new slot after
// the last. A delete, or a put that sets the vector to null, frees the
// record's slot and leaves its values. Outside a transaction the slot joins
// the free list at once. Inside one it joins at the commit (release, in
// tx.go), so no vector the transaction puts can take it, and a rollback
// gives it back to its record as it was, with nothing to copy (S2's note).
// Nothing moves vectors to close the gaps: a search passes a free slot at
// the cost of reading its record's pointer, a later vector takes it, and a
// reload after a compaction builds the arrays afresh.
//
// A put that changes a record's vector writes the new values over the old
// ones in its slot. Inside a transaction its undo entry keeps a copy of the
// old values and the old norm, which a rollback puts back. A Record's Vec is
// the slot itself, so it changes with the slot, as anything a read hands out
// may once the store changes. Its capacity ends at its length, so appending
// to it never writes into the next slot.
//
// A table's vector size is set by its first vector, or by a CreateTable that
// gives one, and setSize sets the array's shift with it. A drop takes the
// table's array and size with it, so a table made again with the same name
// starts with size 0 and an empty array. A rollback of the first vector
// takes the size back to 0 and empties the array.

// blockBytes is the most room a block of values takes. A megabyte keeps the
// moves from one block to the next too few to show in a search, and keeps
// the room a table leaves spare at the end of its last block small.
const blockBytes = 1 << 20

// vectors is one table's vector array.
type vectors struct {
	// shift sets the slots a full block holds, 1 << shift. setSize sets it
	// with the table's size.
	shift  uint8
	blocks [][]float32 // the slots' values
	norms  []float64   // each slot's norm
	owners []*record   // each slot's record, or nil for a free slot
	free   []int       // the free slots ready for use, the one freed last at the end
}

// blockShift returns the shift for a table whose vectors have size values:
// the most slots, as a power of two, whose values fit in blockBytes, and at
// least one.
func blockShift(size int) uint8 {
	var s uint8
	for (2<<s)*size*4 <= blockBytes {
		s++
	}
	return s
}

// vector returns the values in slot i of t's array, sharing the array's
// memory, with their capacity cut at their length.
func (t *table) vector(i int) []float32 {
	a := &t.vecs
	b := a.blocks[i>>a.shift]
	at := (i & (1<<a.shift - 1)) * t.size
	return b[at : at+t.size : at+t.size]
}

// room makes sure the blocks have room for slot i, the slot after the last,
// in a table whose vectors have size values.
func (a *vectors) room(size, i int) {
	b := i >> a.shift
	if b < len(a.blocks) && (b > 0 || (i+1)*size <= len(a.blocks[0])) {
		return // room a rollback left, or room spare in the first block
	}
	full := (1 << a.shift) * size
	if b > 0 {
		a.blocks = append(a.blocks, make([]float32, full))
		return
	}
	// The first block doubles, from one slot to a full block. Reads that
	// shared the old one keep it, with the values it had.
	if len(a.blocks) == 0 {
		a.blocks = [][]float32{make([]float32, size)}
		return
	}
	grown := make([]float32, min(2*len(a.blocks[0]), full))
	copy(grown, a.blocks[0])
	a.blocks[0] = grown
}

// The small functions that change the arrays, each with its undo entry
// first, as changing requires: setSize in store.go, and takeSlot, fillSlot
// and freeSlot here. Commit puts the slots a transaction freed on their free
// lists with release, in tx.go.

// setVector gives r the put's vector, vals, whose dot product with itself is
// dot, or takes r's vector away when vals is nil. A table's first vector sets
// its size.
func (s *Store) setVector(t *table, r *record, vals []float32, dot float64) {
	if vals == nil {
		if r.slot >= 0 {
			s.freeSlot(t, r)
			r.slot = -1
		}
		return
	}
	if t.size == 0 {
		s.setSize(t, len(vals))
	}
	fresh := r.slot < 0
	if fresh {
		r.slot = s.takeSlot(t, r)
	}
	s.fillSlot(t, r.slot, vals, dot, fresh)
}

// takeSlot gives r a slot in t's array, for its first vector: the free slot
// freed last, or a new one after the last. fillSlot writes the values.
func (s *Store) takeSlot(t *table, r *record) int {
	a := &t.vecs
	if n := len(a.free); n > 0 {
		i := a.free[n-1]
		s.changing(undo{op: undoTookSlot, table: t, n: i})
		a.free = a.free[:n-1]
		a.owners[i] = r
		return i
	}
	i := len(a.owners)
	s.changing(undo{op: undoNewSlot, table: t, n: i})
	a.room(t.size, i)
	a.owners = append(a.owners, r)
	a.norms = append(a.norms, 0)
	return i
}

// fillSlot writes vals into slot i of t's array, with the norm, the square
// root of dot, their dot product with themselves. fresh says the slot has
// just been taken, so the values in it were nobody's. Otherwise it held its
// record's vector, and inside a transaction the undo entry keeps a copy of
// the old values beside the old norm.
func (s *Store) fillSlot(t *table, i int, vals []float32, dot float64, fresh bool) {
	slot := t.vector(i)
	if !fresh {
		u := undo{op: undoVector, table: t, n: i, norm: t.vecs.norms[i]}
		if s.tx != nil && plant != "store/overwrite-not-kept" {
			u.vec = slices.Clone(slot)
		}
		s.changing(u)
	}
	copy(slot, vals)
	t.vecs.norms[i] = math.Sqrt(dot)
}

// freeSlot marks r's slot in t's array free, for a delete of r or a put that
// takes its vector away, and leaves its values as they are. Outside a
// transaction the slot joins the free list at once. Inside one, Commit adds
// it, so nothing takes it while a rollback may give it back.
func (s *Store) freeSlot(t *table, r *record) {
	i := r.slot
	s.changing(undo{op: undoFreedSlot, table: t, record: r, n: i})
	t.vecs.owners[i] = nil
	if s.tx == nil || plant == "store/freed-slot-taken-at-once" {
		t.vecs.free = append(t.vecs.free, i)
	}
}

// checkVector checks a vector for the vector field by 0.x's rules, as
// rules.Vector keeps them: 1 to 65,536 values, each finite, and not all zero.
// It decodes the values into the store's buffer, which the next put uses
// again, and returns them with their dot product with themselves, whose
// square root is the norm.
//
// That one sum is the whole check. It's finite only when every value is,
// since even 65,536 of float32's largest values squared come to about
// 7.6e81, and it's above zero once any value isn't zero, since the square of
// float32's smallest value, 2^-298, is well inside float64's range. So a
// vector that keeps the rules costs a pass to decode it and one for its
// norm, which it needs anyway, and rules.Vector only runs to say what's
// wrong with one that breaks them.
func (s *Store) checkVector(v value.Value) ([]float32, float64, error) {
	n := v.Dims()
	if n == 0 || n > rules.MaxDims {
		// rules.Vector looks only at the length here, and a vector this
		// long holds 4 bytes a value already, so the room is for a moment.
		return nil, 0, rules.Vector(make([]float32, n))
	}
	if cap(s.buf) < n {
		s.buf = make([]float32, n)
	}
	vals := decode(s.buf[:n], v.Raw())
	dot := vecmath.Dot(vals, vals)
	if dot > 0 && dot <= math.MaxFloat64 {
		return vals, dot, nil
	}
	err := rules.Vector(vals)
	if err == nil {
		panic(fmt.Sprintf("store: a vector whose dot product with itself is %v keeps rules.Vector's rules", dot))
	}
	return nil, 0, err
}

// decode writes the values whose bits are in raw, 4 bytes a value,
// little-endian, into dst, which has room for every one, and returns dst.
// It's value.AppendVector's loop without the appends: for 384 values in the
// processor's cache, AppendVector took 470 ns and this loop 155. Copying the
// bits as they are, through package unsafe, took 30, since amd64 and arm64
// hold a float32 as those same 4 bytes, but this loop needs no rule about
// the processor and costs about 12 ms more an open of 100,000 vectors.
func decode(dst []float32, raw string) []float32 {
	raw = raw[:4*len(dst)]
	for i := range dst {
		j := 4 * i
		dst[i] = math.Float32frombits(uint32(raw[j]) | uint32(raw[j+1])<<8 | uint32(raw[j+2])<<16 | uint32(raw[j+3])<<24)
	}
	return dst
}

// MaxK is the most hits Nearest gives, as in 0.x.
const MaxK = 10000

// Nearest returns the k records of the table whose vectors are closest to q
// by cosine distance, closest first and then by key, comparing every vector
// that keep lets through, as Reader's Nearest says. It reads the copy and
// changes nothing, so readers can search while others read.
//
// The checks are 0.x's, in 0.x's order, with 0.x's errors: the table's name
// (rules.Table), the query (rules.Vector) and k, from 1 to MaxK, each with
// an error that wraps errs.ErrInvalid; then the table, which has to exist,
// or the error wraps errs.ErrNotFound. A table with no vector size yet gives
// nil and no error. A query of another size than the table's vectors gives
// an error that wraps errs.ErrInvalid. Otherwise the hits come in a list of
// their own, as in 0.x, which is an empty list that isn't nil when no vector
// gets through.
//
// keep is called once for each record with a vector, in the order of their
// slots, before that record's distance is worked out, so the dot product is
// skipped for each record it rules out. The Record it gets is the one Get
// gives, its Vec the slot. An error from keep stops the search, and Nearest
// returns it as it is.
func (s *Store) Nearest(table string, q []float32, k int, keep Filter) ([]Hit, error) {
	if err := rules.Table(table); err != nil {
		return nil, err
	}
	if err := rules.Vector(q); err != nil {
		return nil, err
	}
	t := s.tables[table]
	if t == nil && plant == "store/nearest-table-first" {
		return nil, fmt.Errorf("%w: no record table %s", errs.ErrNotFound, table)
	}
	if k < 1 || k > MaxK {
		return nil, fmt.Errorf("%w: k is from 1 to %d, not %d", errs.ErrInvalid, MaxK, k)
	}
	switch {
	case t == nil:
		return nil, fmt.Errorf("%w: no record table %s", errs.ErrNotFound, table)
	case t.size == 0:
		return nil, nil // no vectors yet
	case len(q) != t.size:
		return nil, fmt.Errorf("%w: table %s holds vectors of %d values, and the query has %d", errs.ErrInvalid, table, t.size, len(q))
	}
	return t.search(q, k, keep)
}

// search compares the query q, which has t's size, with every vector in t's
// array that keep lets through, and returns the k closest, as Nearest says.
// It goes through the blocks in order, skipping free slots, and keeps the k
// closest so far in a heap, the furthest of them on top: a vector further
// than that one costs one comparison.
func (t *table) search(q []float32, k int, keep Filter) ([]Hit, error) {
	a := &t.vecs
	w, qn := vecmath.Widen(q), vecmath.Norm(q)
	top := closest{k: k, h: make([]candidate, 0, min(k, len(a.owners)))}
	size := t.size
	first := 0 // the number of the block's first slot
	for _, b := range a.blocks {
		if first >= len(a.owners) {
			break // room that a rollback left
		}
		owners := a.owners[first:min(first+len(b)/size, len(a.owners))]
		norms := a.norms[first : first+len(owners)]
		first += len(owners)
		for j, r := range owners {
			if r == nil {
				continue // a free slot
			}
			v := b[j*size : j*size+size : j*size+size]
			if keep != nil && plant != "store/filter-after-distance" {
				ok, err := keep(r.readVec(v))
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
			c := candidate{vecmath.Distance(w.Dot(v), qn, norms[j]), r}
			if len(top.h) == k && !closer(c, top.h[0]) {
				continue
			}
			if keep != nil && plant == "store/filter-after-distance" {
				ok, err := keep(r.readVec(v))
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
			top.add(c)
		}
	}
	return top.hits(), nil
}

// candidate is a record a search has compared, with its distance.
type candidate struct {
	d float64
	r *record
}

// closer reports whether a comes before b among the hits: nearer, or as
// near and first by key. Distances are never NaN: every vector the store
// holds and every query is finite with a norm above zero, and Distance keeps
// its result between 0 and 2. Keys in a table differ, so two records never
// tie on both.
func closer(a, b candidate) bool {
	if a.d != b.d {
		return a.d < b.d
	}
	if plant == "store/ties-by-slot" {
		return a.r.slot < b.r.slot
	}
	return a.r.key < b.r.key
}

// closest keeps the k closest candidates a search has seen, in a heap with
// the furthest at h[0].
type closest struct {
	k int
	h []candidate
}

// add puts c among the closest: in a place of its own while there are fewer
// than k, or else in the place of the furthest, which c is closer than.
func (top *closest) add(c candidate) {
	h := top.h
	if len(h) < top.k {
		h = append(h, c)
		for i := len(h) - 1; i > 0; {
			p := (i - 1) / 2
			if !closer(h[p], h[i]) {
				break
			}
			h[p], h[i] = h[i], h[p]
			i = p
		}
		top.h = h
		return
	}
	if plant == "store/top-k-first-seen" {
		return
	}
	h[0] = c
	for i := 0; ; {
		far := i // the furthest of i and its children
		if l := 2*i + 1; l < len(h) && closer(h[far], h[l]) {
			far = l
		}
		if r := 2*i + 2; r < len(h) && closer(h[far], h[r]) {
			far = r
		}
		if far == i {
			return
		}
		h[i], h[far] = h[far], h[i]
		i = far
	}
}

// hits returns the closest as Nearest gives them: closest first, then by
// key, in a list of their own, which isn't nil even when it's empty.
func (top *closest) hits() []Hit {
	slices.SortFunc(top.h, func(a, b candidate) int {
		switch {
		case closer(a, b):
			return -1
		case closer(b, a):
			return 1
		}
		return 0
	})
	out := make([]Hit, len(top.h))
	for i, c := range top.h {
		out[i] = Hit{Key: c.r.key, Distance: c.d}
	}
	return out
}
