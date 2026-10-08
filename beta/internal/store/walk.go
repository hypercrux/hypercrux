// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"cmp"
	"fmt"
	"hash/maphash"
	"slices"
	"strings"
	"sync"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
)

// The links' reads (S5): Neighbours reads one record's lists, and Walk
// follows them, forwards through each record's links out and backwards
// through its links in, the reverse index. Neither touches anything but the
// records it reaches, and neither changes anything, so readers can share
// the store while they run.

// MaxDepth is the furthest Walk goes, as in 0.x.
const MaxDepth = 32

// Neighbours returns the links of the record with this key, out of it,
// into it or both, of the type typ, or of every type when typ is "", or nil
// when there are none. They come in 0.x's order: by type and then by the
// key at the other end, or, both ways, by type, then the key each link is
// from, then the key it's to, with a link from the record to itself once.
// Each list is in its own order already, so the links out and the links in
// come straight from the record's lists, and both ways merges the two.
//
// The errors are 0.x's, in 0.x's order: the key's, as Get gives them, then
// one that wraps errs.ErrInvalid for a direction that isn't one. As in 0.x,
// the type isn't checked: one that breaks the rules gives no links.
func (s *Store) Neighbours(key string, dir Direction, typ string) ([]Link, error) {
	r, err := s.existing(key)
	if err != nil {
		return nil, err
	}
	switch dir {
	case Out:
		return r.linksOut(typ), nil
	case In:
		return r.linksIn(typ), nil
	case Both:
		return mergeLinks(r.linksOut(typ), r.linksIn(typ)), nil
	}
	return nil, badDirection(dir)
}

// badDirection is 0.x's error for a direction that isn't one.
func badDirection(dir Direction) error {
	return fmt.Errorf("%w: direction %d", errs.ErrInvalid, int(dir))
}

// linksOut returns r's links out of the type typ, or of every type, in
// order, or nil when there are none.
func (r *record) linksOut(typ string) []Link {
	var out []Link
	for h := range r.out.ofType(typ) {
		if out == nil {
			out = make([]Link, 0, r.out.len())
		}
		out = append(out, Link{From: r.key, Type: h.typ.Value(), To: h.other.key})
	}
	return out
}

// linksIn returns r's links in of the type typ, or of every type, in order,
// or nil when there are none.
func (r *record) linksIn(typ string) []Link {
	var in []Link
	for h := range r.in.ofType(typ) {
		if in == nil {
			in = make([]Link, 0, r.in.len())
		}
		in = append(in, Link{From: h.other.key, Type: h.typ.Value(), To: r.key})
	}
	return in
}

// mergeLinks merges one record's links out and in, each in order of type,
// then the key a link is from, then the key it's to, into one list in that
// order. The links out all come from the record, and the links in all go to
// it, so each list is in that order already. A link from the record to
// itself is in both, and comes once.
func mergeLinks(out, in []Link) []Link {
	if len(in) == 0 {
		return out
	}
	if len(out) == 0 {
		return in
	}
	if plant == "store/both-not-merged" {
		return append(out, in...)
	}
	all := make([]Link, 0, len(out)+len(in))
	for len(out) > 0 && len(in) > 0 {
		switch c := compareLinks(out[0], in[0]); {
		case c < 0:
			all, out = append(all, out[0]), out[1:]
		case c > 0:
			all, in = append(all, in[0]), in[1:]
		default:
			all, out, in = append(all, out[0]), out[1:], in[1:]
		}
	}
	return append(append(all, out...), in...)
}

// compareLinks compares two links by type, then the key each is from, then
// the key each is to, in byte order.
func compareLinks(a, b Link) int {
	return cmp.Or(strings.Compare(a.Type, b.Type), strings.Compare(a.From, b.From), strings.Compare(a.To, b.To))
}

// Walk returns every record within depth links of the record with this
// key, following links of the type typ, or of every type when typ is "", in
// the direction dir: through each record's links out, its links in, or
// both. Each comes once, with the fewest links it takes, nearest first and
// then by key, and the record the walk starts from isn't among them, even
// when a path leads back to it. It returns nil when no record is in reach.
//
// It's a breadth-first search with a set of the records it has seen. Each
// round takes the records the round before found, and finds the records one
// link further on that it hasn't seen yet, which are the ones depth links
// away. Each round's records are sorted by key, as 0.x gives them. The set
// and the lists it works with come from a pool, so once they've grown to
// the size walks need, a walk allocates only the steps it returns.
//
// The errors are 0.x's, in 0.x's order: one that wraps errs.ErrInvalid for
// a depth outside 1 to MaxDepth, then for a direction that isn't one, then
// the key's, as Get gives them. As in 0.x, the type isn't checked: one that
// breaks the rules follows no links.
func (s *Store) Walk(key string, dir Direction, typ string, depth int) ([]Step, error) {
	if depth < 1 || depth > MaxDepth {
		return nil, fmt.Errorf("%w: depth is from 1 to %d, not %d", errs.ErrInvalid, MaxDepth, depth)
	}
	if dir != Out && dir != In && dir != Both {
		return nil, badDirection(dir)
	}
	start, err := s.existing(key)
	if err != nil {
		return nil, err
	}
	w := walkers.Get().(*walker)
	defer w.done()
	if plant != "store/walk-start-included" {
		w.seen.add(start)
	}
	w.round = append(w.round, start)
	for d := 1; d <= depth && len(w.round) > 0; d++ {
		for _, r := range w.round {
			if dir != In {
				w.reach(&r.out, typ)
			}
			if dir != Out {
				w.reach(&r.in, typ)
			}
		}
		from := len(w.steps)
		for _, r := range w.next {
			w.steps = append(w.steps, Step{Key: r.key, Depth: d})
		}
		if plant != "store/walk-unsorted" {
			slices.SortFunc(w.steps[from:], func(a, b Step) int { return strings.Compare(a.Key, b.Key) })
		}
		clear(w.round)
		w.round, w.next = w.next, w.round[:0]
	}
	if len(w.steps) == 0 {
		return nil, nil
	}
	return slices.Clone(w.steps), nil
}

// walker is a walk's working memory: the set of records it has seen, the
// records of the round it's in and of the round after, and the steps so
// far. A walk takes one from walkers and puts it back empty, so walks that
// run one after another, or side by side under the copy's shared lock, use
// the same few again.
type walker struct {
	seen        recordSet
	round, next []*record
	steps       []Step
}

var walkers = sync.Pool{New: func() any { return &walker{seen: recordSet{seed: maphash.MakeSeed()}} }}

// done empties the walker, so it keeps no record alive, and puts it back in
// the pool, unless a walk through a large part of the store made it large.
func (w *walker) done() {
	w.seen.empty()
	clear(w.round)
	clear(w.steps)
	w.round, w.next, w.steps = w.round[:0], w.next[:0], w.steps[:0]
	if len(w.seen.slots) <= 1<<16 {
		walkers.Put(w)
	}
}

// reach adds to the next round the records at the other ends of the halves
// in l of the type typ, or of every type, that the walk hasn't seen.
func (w *walker) reach(l *links, typ string) {
	bi, i := 0, 0
	if typ != "" {
		bi, i = l.first(typ)
	}
	for ; bi < len(l.blocks); bi, i = bi+1, 0 {
		for _, h := range l.blocks[bi][i:] {
			if typ != "" && h.typ.Value() != typ {
				return
			}
			if w.seen.add(h.other) {
				w.next = append(w.next, h.other)
			}
		}
	}
}

// recordSet is a set of records, by address: a table open to probing, a
// power of two long, kept no more than half full, with a list of the slots
// in use, so it empties in the time it took to fill. A Go map would do, but
// a walk that makes one afresh spends most of its time growing it.
type recordSet struct {
	slots []*record // nil where there's no record
	used  []int     // the slots in use
	seed  maphash.Seed
}

// add puts r in the set, and reports false when it was there already.
func (s *recordSet) add(r *record) bool {
	if 2*(len(s.used)+1) > len(s.slots) {
		s.grow()
	}
	mask := len(s.slots) - 1
	for i := int(maphash.Comparable(s.seed, r)) & mask; ; i = (i + 1) & mask {
		switch s.slots[i] {
		case nil:
			s.slots[i] = r
			s.used = append(s.used, i)
			return true
		case r:
			return false
		}
	}
}

// grow doubles the table, with room for 32 records at first.
func (s *recordSet) grow() {
	old, used := s.slots, s.used
	s.slots, s.used = make([]*record, max(2*len(old), 64)), make([]int, 0, 2*cap(used))
	for _, i := range used {
		s.add(old[i])
	}
}

// empty takes every record out of the set.
func (s *recordSet) empty() {
	for _, i := range s.used {
		s.slots[i] = nil
	}
	s.used = s.used[:0]
}
