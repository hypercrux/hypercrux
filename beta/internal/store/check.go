// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/vecmath"
)

// The store's check (G7): the half of the public package's Check that reads
// the copy in memory. Check reads the whole store once and confirms that its
// parts agree, as 0.x's Check reads its file:
//
//   - each table's keys are in byte order, each with its record, which the
//     hash table holds under that key, and each key starts with its table's
//     name and a colon, by 0.x's rules for keys; and every record in the
//     hash table has its place in its table's keys;
//   - each record's fields are in order of their places in its table's
//     list, with no nulls and no vector among them;
//   - every link is listed at both ends: each half points at a record in the
//     store, whose other list holds the half's mirror, of the same type, and
//     every list is in order of type and then of key;
//   - every vector is in its table's array, which is laid out for the
//     table's size, and holds finite values that aren't all zero, with the
//     norm its values give. The array's slots in use are its records'
//     vectors, one each, and every free slot is on the free list once.
//
// Each table's shape is checked too: its index and its vector field agree
// with its list of fields, and it has a vector size only with a vector
// field. The problems are worded like 0.x's where 0.x has the same problem,
// and come as 0.x's do: a kind of problem that names keys names the first
// five in byte order, then how many more there are, and each bad vector is
// a problem of its own, up to 20 a table.
//
// Check is a read, and changes nothing. It's for Read's callback, which
// holds the copy's lock shared, or for a goroutine that has the store to
// itself. Inside Read, no transaction's changes are in the store, so every
// free slot is on its free list: a transaction that has made a change keeps
// the slots it freed off the list until it ends (vectors.go), and Check
// would report them.
//
// It doesn't panic on a store whose layout is broken: a list or a table's
// keys with an empty block, which a search would trip on, is reported, and
// Check looks nothing up in it.

// Checked is what Check found.
type Checked struct {
	Tables  int // tables
	Records int // records, in their tables' keys
	Links   int // links, each once, at the record it's from
	Vectors int // records with a vector, one slot each
	// Bytes is about how much memory the copy takes (memory.go).
	Bytes    int64
	Problems []string // empty when the copy's parts agree
}

// The most keys a kind of problem names, and the most bad vectors a table
// names, as in 0.x.
const (
	shownKeys    = 5
	shownVectors = 20
)

// Check reads the whole copy and returns what it holds and the problems it
// finds, as the comment at the top of this file says.
func (s *Store) Check() Checked {
	c := &checker{s: s}
	names := make([]string, 0, len(s.tables))
	for name := range s.tables {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		c.table(name, s.tables[name])
	}
	// When the tables' keys hold as many records as the hash table, each
	// under its own key, in its own table, in order and so once, those are
	// all the hash table holds, and each has its place in its table's keys.
	// Otherwise the hash table is read through for the ones that haven't.
	if c.filed != len(s.records) {
		c.hashTable()
	}
	c.flush(&c.global)
	c.out.Tables = len(s.tables)
	c.out.Bytes = s.memory()
	return c.out
}

// checker gathers what Check finds as it reads the store.
type checker struct {
	s   *Store
	out Checked
	// local holds the kinds of problem found in the table Check is on,
	// which come with that table's other problems, and global the kinds
	// that come once, at the end: the links' and the hash table's.
	local, global lists
	// typ is the link type checked last, and typeOK whether it keeps the
	// rules.
	typ    linkType
	typeOK bool
	// filed counts the records in the tables' keys that come in order,
	// start with their table's name, and are in the hash table under their
	// keys.
	filed int
}

// lists are kinds of problem, each with the keys or links it names, in the
// order their first key came.
type lists struct {
	kinds  []*list
	byWhat map[string]*list
}

// list is one kind of problem and the keys, or links, it names.
type list struct {
	what string
	keys []string
}

func (c *checker) problem(format string, args ...any) {
	c.out.Problems = append(c.out.Problems, fmt.Sprintf(format, args...))
}

// add notes key under the kind of problem what.
func (ls *lists) add(what, key string) {
	l := ls.byWhat[what]
	if l == nil {
		if ls.byWhat == nil {
			ls.byWhat = map[string]*list{}
		}
		l = &list{what: what}
		ls.byWhat[what] = l
		ls.kinds = append(ls.kinds, l)
	}
	l.keys = append(l.keys, key)
}

// flush reports each kind of problem in ls as one problem, in 0.x's words:
// what's wrong, the first keys it names in byte order, and how many more
// there are. Then ls is empty again.
func (c *checker) flush(ls *lists) {
	for _, l := range ls.kinds {
		slices.Sort(l.keys)
		keys := slices.Compact(l.keys)
		shown := keys[:min(len(keys), shownKeys)]
		more := ""
		if n := len(keys) - len(shown); n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		c.problem("%s: %s%s", l.what, strings.Join(shown, ", "), more)
	}
	ls.kinds = ls.kinds[:0]
	clear(ls.byWhat)
}

// table checks one table: its shape, its keys and their records, and its
// vector array. The table's own problems come together, before the next
// table's.
func (c *checker) table(name string, t *table) {
	if t.name != name {
		c.problem("the table filed as %s is called %s", name, t.name)
	}
	c.shape(name, t)

	// The keys, each with its record.
	count, withVec := 0, 0
	var last string
	for _, b := range t.keys.blocks {
		if len(b) == 0 || cap(b) > blockMax {
			c.problem("table %s keeps its keys in a block of %d, with room for %d", name, len(b), cap(b))
		}
		for _, e := range b {
			inOrder := count == 0 || e.key > last
			if !inOrder {
				c.local.add("keys in "+name+" out of byte order", e.key)
			}
			last = e.key
			count++
			r := e.r
			if r == nil || r.key != e.key || r.table != t {
				c.local.add("keys in "+name+" held with the wrong record", e.key)
				continue
			}
			if r.slot >= 0 {
				withVec++
			}
			if c.record(name, t, r) && inOrder {
				c.filed++
			}
		}
	}
	if count != t.keys.n {
		c.problem("table %s counts %d keys, and its blocks hold %d", name, t.keys.n, count)
	}
	c.out.Records += count
	c.flush(&c.local)
	c.vectors(name, t, withVec)
}

// shape checks a table's index and vector field against its list of fields,
// and its vector size.
func (c *checker) shape(name string, t *table) {
	vec := -1
	for p, f := range t.fields {
		if q, ok := t.index[rules.Fold(f)]; !ok || q != p {
			c.local.add("fields of "+name+" that its index has at another place", f)
		}
		if rules.IsVec(f) && vec < 0 {
			vec = p
		}
	}
	if len(t.index) != len(t.fields) {
		c.problem("table %s has %d fields, and %d in its index", name, len(t.fields), len(t.index))
	}
	if t.vec != vec {
		c.problem("table %s has its vector field at place %d, where its list has it at %d", name, t.vec, vec)
	}
	switch {
	case t.size < 0 || t.size > rules.MaxDims:
		c.problem("table %s has a vector size of %d, where it's 0 to %d", name, t.size, rules.MaxDims)
	case t.size > 0 && vec < 0:
		c.problem("table %s has a vector size of %d and no field vec", name, t.size)
	}
}

// record checks a record in t's keys: its key, its place in the hash table,
// its fields, its vector's slot and its links both ways. It reports whether
// the key starts with the table's name and the hash table holds the record
// under it.
func (c *checker) record(name string, t *table, r *record) bool {
	filed := true
	if tbl, err := rules.TableOf(r.key); err != nil || tbl != name {
		c.local.add("keys in "+name+" that don't start with "+name+":", strconv.Quote(r.key))
		filed = false
	}
	if c.s.records[r.key] != r {
		c.local.add("keys in "+name+" missing from the hash table", r.key)
		filed = false
	}
	for i, f := range r.fields {
		if i > 0 && r.fields[i-1].Index >= f.Index || f.Index < 0 || f.Index >= len(t.fields) || f.Index == t.vec || f.Value.IsNull() {
			c.local.add("records in "+name+" with fields out of order, null or outside the table's list", r.key)
			break
		}
	}
	a := &t.vecs
	if r.slot < -1 || r.slot >= 0 && t.size > 0 && (r.slot >= len(a.owners) || a.owners[r.slot] != r) {
		c.local.add("records in "+name+" whose vector isn't in their slot", r.key)
	}
	c.links(r, true)
	c.links(r, false)
	return filed
}

// links checks one of r's lists, its links out when out is true and its
// links in otherwise: the order of its halves, and each half. The record at
// the other end is in the store, the type keeps the rules, and the other
// end's list holds the half's mirror. Each link out counts once.
func (c *checker) links(r *record, out bool) {
	l, way := &r.in, "in"
	if out {
		l, way = &r.out, "out"
	}
	var last half
	n := 0
	for _, b := range l.blocks {
		if len(b) == 0 || cap(b) > halfMax {
			c.global.add("records whose links "+way+" are in a block that's empty or too long", r.key)
		}
		for _, h := range b {
			if h.other == nil || h.typ == (linkType{}) {
				c.global.add("records whose links "+way+" hold a half with no record or no type", r.key)
				continue
			}
			if n > 0 && compareHalf(last, h.at()) >= 0 {
				c.global.add("records whose links "+way+" are out of order", r.key)
			}
			last = h
			n++
			from, to := r, h.other
			if !out {
				from, to = h.other, r
			}
			if c.s.records[h.other.key] != h.other {
				if out {
					c.global.add("links to keys that don't exist", h.other.key)
				} else {
					c.global.add("links from keys that don't exist", h.other.key)
				}
				continue
			}
			if out {
				// A link's type is checked once, at the record it's from: the
				// mirror, when there is one, has the same handle. A list keeps
				// its halves in order of type, so the type is mostly the one
				// checked last.
				if h.typ != c.typ {
					c.typ, c.typeOK = h.typ, rules.LinkType(h.typ.Value()) == nil
				}
				if !c.typeOK {
					c.global.add("links with a bad type", from.key)
				}
				c.out.Links++
			}
			if checkPlant == "store/check-skips-mirror" {
				continue
			}
			mirror := &h.other.in
			if !out {
				mirror = &h.other.out
			}
			if !sound(mirror) {
				continue // reported as the other record's
			}
			m, ok := mirror.get(linkAt{h.typ.Value(), r.key})
			if !ok || m.other != r || m.typ != h.typ {
				name := Link{from.key, h.typ.Value(), to.key}.String()
				if out {
					c.global.add("links listed only at the record they're from", name)
				} else {
					c.global.add("links listed only at the record they're to", name)
				}
			}
		}
	}
}

// sound reports whether a list's blocks can be searched: none of them is
// empty.
func sound(l *links) bool {
	for _, b := range l.blocks {
		if len(b) == 0 {
			return false
		}
	}
	return true
}

// vectors checks a table's vector array: its shape for the table's size,
// its free list, and each slot. A slot in use holds the vector of a record
// of the table that's in the store and names the slot, a vector that keeps
// 0.x's rules, with the norm its values give, bit for bit. withVec is how
// many of the table's records name a slot, which have to be as many as the
// slots in use.
func (c *checker) vectors(name string, t *table, withVec int) {
	a := &t.vecs
	if checkPlant == "store/check-free-slots-counted" {
		c.out.Vectors += len(a.owners)
	} else {
		c.out.Vectors += withVec
	}
	if t.size <= 0 || t.size > rules.MaxDims {
		if len(a.blocks)+len(a.norms)+len(a.owners)+len(a.free) != 0 {
			c.problem("table %s has no vector size, and an array of %d slots", name, len(a.owners))
		}
		// Each of the table's records that names a slot has a vector the
		// table can't hold.
		for r := range t.keys.records {
			if r != nil && r.slot >= 0 {
				c.problem("the vector of %s is there, but the table has no vector size recorded", r.key)
			}
		}
		return
	}
	room := 0
	for _, b := range a.blocks {
		if len(b) == 0 || len(b)%t.size != 0 {
			c.problem("table %s's vector array has a block of %d values, for vectors of %d", name, len(b), t.size)
			return
		}
		room += len(b) / t.size
	}
	switch {
	case a.shift != blockShift(t.size):
		c.problem("table %s's vector array is laid out for blocks of %d slots, where vectors of %d values take %d", name, 1<<a.shift, t.size, 1<<blockShift(t.size))
		return
	case len(a.norms) != len(a.owners):
		c.problem("table %s's vector array has %d norms for %d slots", name, len(a.norms), len(a.owners))
		return
	case room < len(a.owners):
		c.problem("table %s's vector array has room for %d slots, and %d slots", name, room, len(a.owners))
		return
	}
	for bi, b := range a.blocks {
		if bi > 0 && len(b) != (1<<a.shift)*t.size {
			c.problem("table %s's vector array has a block of %d slots after its first, where a full block holds %d", name, len(b)/t.size, 1<<a.shift)
			return
		}
	}

	listed := make([]bool, len(a.owners))
	for _, i := range a.free {
		if i < 0 || i >= len(a.owners) || a.owners[i] != nil || listed[i] {
			c.local.add("slots on "+name+"'s free list that are in use, past its end or there twice", strconv.Itoa(i))
			continue
		}
		listed[i] = true
	}
	inUse, bad := 0, 0
	for i, r := range a.owners {
		if r == nil {
			if !listed[i] {
				c.local.add("free slots of "+name+"'s vector array on no free list", strconv.Itoa(i))
			}
			continue
		}
		inUse++
		if c.s.records[r.key] != r || r.table != t || r.slot != i {
			c.local.add("slots of "+name+"'s vector array held for a record that isn't there or names another slot", strconv.Itoa(i))
			continue
		}
		v := t.vector(i)
		dot := vecmath.Dot(v, v)
		why := ""
		switch {
		case math.IsNaN(dot) || math.IsInf(dot, 0):
			// A sum of squares is finite when every value is: 65,536 of
			// float32's largest squared come to about 7.6e81.
			why = "has a value that isn't a finite number"
		case dot == 0 && checkPlant != "store/check-zeros-let-through":
			// Squared, float32's smallest value is 2^-298, well inside
			// float64's range, so only zeros sum to 0.
			why = "is all zeros"
		case math.Float64bits(a.norms[i]) != math.Float64bits(math.Sqrt(dot)):
			why = fmt.Sprintf("has the norm %v, where its values give %v", a.norms[i], math.Sqrt(dot))
		}
		if why != "" {
			bad++
			if bad <= shownVectors {
				c.problem("the vector of %s %s", r.key, why)
			}
		}
	}
	if bad > shownVectors {
		c.problem("and %d more bad vectors in %s", bad-shownVectors, name)
	}
	c.flush(&c.local)
	if inUse != withVec {
		c.problem("table %s has %d records with a vector, and %d slots in use", name, withVec, inUse)
	}
}

// hashTable checks each record in the hash table: it's filed under its own
// key, its table is in the store, and the table's keys hold it.
func (c *checker) hashTable() {
	for key, r := range c.s.records {
		if r == nil || r.key != key {
			c.global.add("keys in the hash table held with the wrong record", key)
			continue
		}
		t := r.table
		if t == nil || c.s.tables[t.name] != t {
			c.global.add("keys in the hash table for tables that aren't there", key)
			continue
		}
		if !soundKeys(&t.keys) {
			continue // reported as the table's
		}
		bi, i, found := t.keys.place(key)
		if !found || t.keys.blocks[bi][i].r != r {
			c.global.add("keys in the hash table with no place in "+t.name+"'s keys", key)
		}
	}
}

// soundKeys reports whether a table's keys can be searched: none of their
// blocks is empty.
func soundKeys(o *keyOrder) bool {
	for _, b := range o.blocks {
		if len(b) == 0 {
			return false
		}
	}
	return true
}
