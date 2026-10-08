// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"iter"
	"slices"
	"sort"
	"strings"

	// Named intern, since the package has a function called unique, which
	// checks that a put names no field twice.
	intern "unique"
)

// Each record's links (S5). A link from a to b of type t is two halves: the
// half (t, b) in a's links out, and the half (t, a) in b's links in. The
// links in are the reverse index, so a walk can follow links backwards, and
// a delete or a drop finds every link that points at a record without a
// search of the whole store.
//
// Each list keeps its halves in byte order of type and then of the key of
// the record at the other end. That's 0.x's order for Neighbours out and
// in, so Neighbours reads a list as it is, and for both directions it
// merges the two lists, which never needs a sort.
//
// A half is 16 bytes: the type, as a handle that every link of that type
// shares, and a pointer to the record at the other end. So a link takes 32
// bytes in its two lists, plus the room a list keeps for more and the list
// of blocks, 24 bytes for each record's list that isn't empty. With 5 links
// out of each record, that came to 43 bytes a link (TestWalkTimings), on
// top of 48 bytes in every record for the two lists. The text of each type
// is kept once, however many links have it, by the unique package, which
// lets it go once no link has it.
//
// A list is in blocks, as a table's keys are (order.go), so that a record
// with many links, such as one that every other record links to, takes a
// link in or out about as quickly as a record with a few. A record with up
// to halfMax links one way has one block.
//
//   - Adding a half finds its block by a binary search over the blocks'
//     first halves, then its place by another inside the block, and moves
//     the halves after it in the block up one place. A full block splits in
//     two first. A half that comes after every half goes on the end of the
//     last block, with no search, as a compacted file gives a record's links
//     out.
//   - Taking a half out moves the halves after it in its block down one
//     place. A block left with fewer than a quarter of halfMax halves joins
//     its smaller neighbour when the two fit in three quarters of a block,
//     and an empty block leaves the list.
//
// A block grows one half at a time up to 8 halves, and then doubles, up to
// halfMax, and the places past a block's length are always cleared, so a
// block never keeps a record alive that has left it.

// half is one end's view of a link: its type, and the record at the other
// end.
type half struct {
	typ   linkType
	other *record
}

// linkType is a link's type, as a handle that every link of that type
// shares, so the store keeps its text once.
type linkType = intern.Handle[string]

// makeType returns the handle of the link type typ, with a copy of its own
// of the text, so a handle made from a string sliced out of a batch never
// keeps the batch alive.
func makeType(typ string) linkType { return intern.Make(typ) }

// at returns the place of the half in a list.
func (h half) at() linkAt { return linkAt{h.typ.Value(), h.other.key} }

// linkAt is a place in a list: a type, and the key of the record at the
// other end.
type linkAt struct{ typ, key string }

// compareHalf compares a half with a place, by type and then by key, in
// byte order.
func compareHalf(h half, at linkAt) int {
	if c := strings.Compare(h.typ.Value(), at.typ); c != 0 {
		return c
	}
	return strings.Compare(h.other.key, at.key)
}

// halfMax is the most halves a block holds. A full block takes 4 KB, which
// is the most an insert moves.
const halfMax = 256

// links is one record's links in one direction: its halves, in blocks.
type links struct {
	blocks [][]half
}

// place returns where at is, or where a half there would go: the last
// block whose first half doesn't come after at, or block 0, and the place
// in it of the first half at or after at, which can be the block's length.
// found reports whether a half is there. With no blocks, the place is block
// 0.
func (l *links) place(at linkAt) (bi, i int, found bool) {
	j := sort.Search(len(l.blocks), func(j int) bool { return compareHalf(l.blocks[j][0], at) > 0 })
	bi = max(j-1, 0)
	if bi == len(l.blocks) {
		return 0, 0, false
	}
	i, found = slices.BinarySearchFunc(l.blocks[bi], at, compareHalf)
	return bi, i, found
}

// get returns the half at at, and whether there's one.
func (l *links) get(at linkAt) (half, bool) {
	bi, i, found := l.place(at)
	if !found {
		return half{}, false
	}
	return l.blocks[bi][i], true
}

// first returns the place of the first half of the type typ or of a type
// after it: its block and its place in the block, or len(l.blocks) and 0
// when there's none.
func (l *links) first(typ string) (bi, i int) {
	// No key is "", so every half of the type typ comes after (typ, "").
	bi, i, _ = l.place(linkAt{typ, ""})
	if bi < len(l.blocks) && i == len(l.blocks[bi]) {
		bi, i = bi+1, 0
	}
	return bi, i
}

// insert adds h at its place, where the list has no half.
func (l *links) insert(h half) {
	at := h.at()
	last := len(l.blocks) - 1
	if last < 0 {
		l.blocks = [][]half{{h}}
		return
	}
	if b := l.blocks[last]; compareHalf(b[len(b)-1], at) < 0 {
		// After every half, as a compacted file gives a record's links out.
		if len(b) == halfMax {
			l.blocks = append(l.blocks, []half{h})
		} else {
			l.blocks[last] = append(growHalves(b, len(b)+1), h)
		}
		return
	}
	bi, i, _ := l.place(at)
	b := l.blocks[bi]
	if len(b) == halfMax {
		n := halfMax / 2
		right := make([]half, n, halfMax)
		copy(right, b[n:])
		clear(b[n:])
		b = b[:n]
		l.blocks[bi] = b
		l.blocks = slices.Insert(l.blocks, bi+1, right)
		if i > n {
			bi, i, b = bi+1, i-n, right
		}
	}
	l.blocks[bi] = slices.Insert(growHalves(b, len(b)+1), i, h)
}

// remove takes out the half at at, and reports false when there's none, in
// which case nothing changes.
func (l *links) remove(at linkAt) bool {
	bi, i, found := l.place(at)
	if !found {
		return false
	}
	b := slices.Delete(l.blocks[bi], i, i+1) // which clears the place it frees
	if len(b) == 0 {
		l.blocks = slices.Delete(l.blocks, bi, bi+1)
		if len(l.blocks) == 0 {
			l.blocks = nil // as a record with no links has it
		}
		return true
	}
	l.blocks[bi] = b
	if len(b) < halfMax/4 {
		l.join(bi)
	}
	return true
}

// join puts block bi together with its smaller neighbour, when the two hold
// three quarters of a block at most.
func (l *links) join(bi int) {
	nb := bi - 1
	if bi+1 < len(l.blocks) && (nb < 0 || len(l.blocks[bi+1]) < len(l.blocks[nb])) {
		nb = bi + 1
	}
	if nb < 0 || len(l.blocks[bi])+len(l.blocks[nb]) > halfMax*3/4 {
		return
	}
	lo, hi := min(bi, nb), max(bi, nb)
	l.blocks[lo] = append(growHalves(l.blocks[lo], len(l.blocks[lo])+len(l.blocks[hi])), l.blocks[hi]...)
	l.blocks = slices.Delete(l.blocks, hi, hi+1)
}

// growHalves returns b with room for n halves, n being halfMax at most. A
// block of up to 8 halves grows to just the room it needs, so a record with
// a few links one way, as most have, keeps no room it doesn't use: a half
// is 16 bytes, and Go's allocator has a size for every multiple of 16 bytes
// up to 128. From there a block that grows doubles, up to halfMax.
func growHalves(b []half, n int) []half {
	if n <= cap(b) {
		return b
	}
	size := n
	if n > smallBlock {
		size = min(max(2*cap(b), n), halfMax)
	}
	g := make([]half, len(b), size)
	copy(g, b)
	return g
}

// smallBlock is the most halves a block grows to one at a time.
const smallBlock = 8

// len returns how many halves the list holds.
func (l *links) len() int {
	n := 0
	for _, b := range l.blocks {
		n += len(b)
	}
	return n
}

// ofType ranges over the halves of the type typ, or over every half when
// typ is "", in order. Nothing may change the list while it runs.
func (l *links) ofType(typ string) iter.Seq[half] {
	return func(yield func(half) bool) {
		bi, i := 0, 0
		if typ != "" {
			bi, i = l.first(typ)
		}
		for ; bi < len(l.blocks); bi, i = bi+1, 0 {
			for _, h := range l.blocks[bi][i:] {
				if typ != "" && h.typ.Value() != typ {
					return
				}
				if !yield(h) {
					return
				}
			}
		}
	}
}
