// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"slices"
	"sort"
	"strings"
)

// Each table's keys in byte order (S4). A table keeps its records' keys in
// a keyOrder as records come and go: put adds a new record's key, and a
// delete takes it out. Scan reads the keys in that order, the snapshot
// writes each table's records in it, and a drop finds its table's records
// through it.
//
// A keyOrder is a list of blocks. Each block holds up to blockMax keys in
// byte order, each with its record, and every key in a block comes before
// every key in the next block. Finding a key's place is a binary search
// over the blocks' first keys, then one inside the block. An insert looks
// in the block the last insert went into first, and searches the list only
// when the key's place isn't there.
//
//   - Adding a key moves the keys after it in its block up one place, at
//     most blockMax-1 of them. A full block splits into two halves first,
//     which moves the blocks after it in the list up one place: the list
//     holds about one block for every 128 to 256 keys.
//   - A key that comes after every key in the table goes on the end of the
//     last block, with no search, and starts a new block when that one is
//     full. A compacted file gives each table's keys in that order, so
//     opening one fills whole blocks one after another.
//   - Taking a key out moves the keys after it in its block down one place.
//     A block left with fewer than a quarter of blockMax keys joins its
//     smaller neighbour when the two fit in three quarters of a block, so
//     the blocks stay full enough after many deletes, and a joined block
//     takes a quarter of blockMax keys before it splits again. An empty
//     block leaves the list.
//
// A sorted slice of every key would move half the table's keys on each
// insert into the middle: 50,000 of them at 100,000 keys. Here the most an
// insert moves is a block's keys, and on a split a few hundred blocks.
//
// Each block is a slice whose capacity is blockMax at most. A new table's
// first block starts small and grows, so a table with a few records takes
// little memory. The places past a block's length are always cleared, so a
// block never keeps a record alive that has left it.
type keyOrder struct {
	blocks [][]entry
	n      int // how many keys there are
	// hint is the block the last insert went into. The next insert often
	// goes there too, as when a program counting up puts docs:10 just
	// after docs:1, and docs:11 just after docs:10, so insert looks there
	// before it searches the list. It may name a block past the end.
	hint int
}

// blockMax is the most keys a block holds. At 256, a full block's entries
// take 6 KB, which is the most an insert moves. TestKeyOrderTimings found
// 64, 128 and 512 no faster at 100,000 keys.
const blockMax = 256

// entry is one key in a table's order, with its record. The key is the
// record's own string, kept beside the pointer, so a search reads the block
// and each key's bytes without reading the records.
type entry struct {
	key string
	r   *record
}

func compareEntry(e entry, key string) int { return strings.Compare(e.key, key) }

// place returns where key is, or where it would go: the last block whose
// first key doesn't come after key, or block 0, and the place in it of the
// first key at or after key, which can be the block's length. found
// reports whether key is there. With no blocks, the place is block 0.
func (o *keyOrder) place(key string) (bi, i int, found bool) {
	j := sort.Search(len(o.blocks), func(j int) bool { return o.blocks[j][0].key > key })
	bi = max(j-1, 0)
	if bi == len(o.blocks) {
		return 0, 0, false
	}
	i, found = slices.BinarySearchFunc(o.blocks[bi], key, compareEntry)
	return bi, i, found
}

// hinted is place for an insert: it looks in the hint's block first, which
// holds key's place when its first key comes before key and the next
// block's first key after it.
func (o *keyOrder) hinted(key string) (bi, i int, found bool) {
	h := o.hint
	if h >= len(o.blocks) || o.blocks[h][0].key > key || h+1 < len(o.blocks) && o.blocks[h+1][0].key <= key {
		return o.place(key)
	}
	i, found = slices.BinarySearchFunc(o.blocks[h], key, compareEntry)
	return h, i, found
}

// find returns the place of the first key at or after key, or of the first
// key after it when past is true: the block and the place in it. Past the
// last key, the block is len(o.blocks) and the place 0.
func (o *keyOrder) find(key string, past bool) (bi, i int) {
	bi, i, found := o.place(key)
	if found && past {
		i++
	}
	if bi < len(o.blocks) && i == len(o.blocks[bi]) {
		bi, i = bi+1, 0
	}
	return bi, i
}

// insert adds r's key at its place, and reports false when the key is
// there already, in which case nothing changes.
func (o *keyOrder) insert(r *record) bool {
	e := entry{key: r.key, r: r}
	last := len(o.blocks) - 1
	if last < 0 {
		o.blocks = append(o.blocks, append(make([]entry, 0, 8), e))
		o.n++
		return true
	}
	if b := o.blocks[last]; e.key > b[len(b)-1].key {
		// After every key in the table, as a compacted file gives them.
		if len(b) == blockMax {
			o.blocks = append(o.blocks, append(make([]entry, 0, blockMax), e))
		} else {
			o.blocks[last] = append(grow(b, len(b)+1), e)
		}
		o.n++
		return true
	}
	bi, i, found := o.hinted(e.key)
	if found {
		return false
	}
	b := o.blocks[bi]
	if len(b) == blockMax {
		half := blockMax / 2
		right := make([]entry, half, blockMax)
		copy(right, b[half:])
		clear(b[half:])
		b = b[:half]
		o.blocks[bi] = b
		o.blocks = slices.Insert(o.blocks, bi+1, right)
		if i > half {
			bi, i, b = bi+1, i-half, right
		}
	}
	o.blocks[bi] = slices.Insert(grow(b, len(b)+1), i, e)
	o.n++
	o.hint = bi
	return true
}

// remove takes key out, and reports false when it isn't there, in which
// case nothing changes.
func (o *keyOrder) remove(key string) bool {
	bi, i, found := o.place(key)
	if !found {
		return false
	}
	b := slices.Delete(o.blocks[bi], i, i+1) // which clears the place it frees
	o.n--
	if len(b) == 0 {
		o.blocks = slices.Delete(o.blocks, bi, bi+1)
		if len(o.blocks) == 0 {
			o.blocks = nil // as a new table has it
		}
		return true
	}
	o.blocks[bi] = b
	if len(b) < blockMax/4 {
		o.join(bi)
	}
	return true
}

// join puts block bi together with its smaller neighbour, when the two
// hold three quarters of a block at most.
func (o *keyOrder) join(bi int) {
	nb := bi - 1
	if bi+1 < len(o.blocks) && (nb < 0 || len(o.blocks[bi+1]) < len(o.blocks[nb])) {
		nb = bi + 1
	}
	if nb < 0 || len(o.blocks[bi])+len(o.blocks[nb]) > blockMax*3/4 {
		return
	}
	l, r := min(bi, nb), max(bi, nb)
	if plant == "store/join-out-of-order" {
		l, r = r, l
	}
	o.blocks[l] = append(grow(o.blocks[l], len(o.blocks[l])+len(o.blocks[r])), o.blocks[r]...)
	o.blocks = slices.Delete(o.blocks, r, r+1)
}

// grow returns b with room for n keys, n being blockMax at most. A block
// that grows doubles, from 8 keys up to blockMax.
func grow(b []entry, n int) []entry {
	if n <= cap(b) {
		return b
	}
	g := make([]entry, len(b), min(max(2*cap(b), n, 8), blockMax))
	copy(g, b)
	return g
}

// records ranges over the records in byte order of key. Nothing may change
// the order while it runs.
func (o *keyOrder) records(yield func(*record) bool) {
	for _, b := range o.blocks {
		for _, e := range b {
			if !yield(e.r) {
				return
			}
		}
	}
}
