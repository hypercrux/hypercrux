// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The store's check (G7): damage planted in the copy, one kind at a time,
// each reported in its own words, and no problem on any copy the writes
// leave, through rollbacks, deletes and drops.

// field is a field of a put.
func field(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }

// healthy returns a store holding a little of everything the check reads:
// tables with fields and vectors, a free slot left by a delete, a table left
// empty by one, links within a table, across tables and from a record to
// itself, and a record with two links out. It holds 3 tables, 5 records, 5
// links and 3 vectors.
func healthy(t *testing.T) *Store {
	t.Helper()
	s := New()
	must := func(_ []format.Change, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	vec := func(v ...float32) value.Value { return value.Vector(v) }
	must(s.Put(nil, "docs:1", []format.Field{field("title", value.Text("one")), field("vec", vec(1, 0, 0))}))
	must(s.Put(nil, "docs:2", []format.Field{field("n", value.Int(2)), field("vec", vec(0, 1, 0))}))
	must(s.Put(nil, "docs:3", []format.Field{field("title", value.Text("three")), field("vec", vec(0, 0.5, 2))}))
	must(s.Put(nil, "docs:4", []format.Field{field("vec", vec(1, 1, 1))}))
	must(s.Delete(nil, "docs:4"))
	must(s.Put(nil, "people:1", []format.Field{field("name", value.Text("Ann"))}))
	must(s.Put(nil, "people:2", nil))
	must(s.Put(nil, "empty:1", nil))
	must(s.Delete(nil, "empty:1"))
	must(s.Link(nil, "people:1", "owns", "docs:1"))
	must(s.Link(nil, "people:1", "owns", "docs:2"))
	must(s.Link(nil, "docs:1", "cites", "docs:2"))
	must(s.Link(nil, "docs:2", "cites", "docs:2"))
	must(s.Link(nil, "people:2", "knows", "people:1"))
	return s
}

// stray is a record that isn't in the store, of the table called name.
func stray(key, name string) *record {
	return &record{key: key, table: &table{name: name, index: map[string]int{}, vec: -1}, slot: -1}
}

// TestTheCheckFindsDamageInTheCopy is the half of G7's closing test that
// plants damage in memory. Each case damages a healthy store in one way, as
// a bug in the store's writes could, and Check reports that, in the words
// given: 0.x's where 0.x has the same problem. The healthy store has no
// problems, and Check counts what it holds.
func TestTheCheckFindsDamageInTheCopy(t *testing.T) {
	s := healthy(t)
	got := s.Check()
	if len(got.Problems) != 0 || got.Tables != 3 || got.Records != 5 || got.Links != 5 || got.Vectors != 3 {
		t.Fatalf("the healthy store gives %+v, where 3 tables, 5 records, 5 links, 3 vectors and no problems are wanted", got)
	}
	r := func(s *Store, key string) *record { return s.records[key] }
	for _, c := range []struct {
		what  string
		plant func(s *Store)
		want  []string
	}{
		{"a link's half gone from the record it's to", func(s *Store) {
			r(s, "docs:2").in.remove(linkAt{"cites", "docs:1"})
		}, []string{"links listed only at the record they're from: docs:1 -cites-> docs:2"}},
		{"a link's half gone from the record it's from", func(s *Store) {
			r(s, "people:2").out.remove(linkAt{"knows", "people:1"})
		}, []string{"links listed only at the record they're to: people:2 -knows-> people:1"}},
		{"a link to a record that isn't in the store", func(s *Store) {
			r(s, "docs:3").out.insert(half{makeType("cites"), stray("docs:9", "docs")})
		}, []string{"links to keys that don't exist: docs:9"}},
		{"a link from a record that isn't in the store", func(s *Store) {
			r(s, "docs:3").in.insert(half{makeType("cites"), stray("docs:9", "docs")})
		}, []string{"links from keys that don't exist: docs:9"}},
		{"a link of a type that breaks the rules", func(s *Store) {
			bad := makeType(strings.Repeat("t", 201))
			r(s, "people:2").out.remove(linkAt{"knows", "people:1"})
			r(s, "people:1").in.remove(linkAt{"knows", "people:2"})
			r(s, "people:2").out.insert(half{bad, r(s, "people:1")})
			r(s, "people:1").in.insert(half{bad, r(s, "people:2")})
		}, []string{"links with a bad type: people:2"}},
		{"a list of links out of order", func(s *Store) {
			b := r(s, "people:1").out.blocks[0]
			b[0], b[1] = b[1], b[0]
		}, []string{
			// Neither half can be found in a list out of order.
			"links listed only at the record they're to: people:1 -owns-> docs:1, people:1 -owns-> docs:2",
			"records whose links out are out of order: people:1",
		}},
		{"a table's keys out of order", func(s *Store) {
			b := s.tables["docs"].keys.blocks[0]
			b[1], b[2] = b[2], b[1]
		}, []string{
			// docs:3 comes before docs:2, where a search can't find docs:2.
			"keys in docs out of byte order: docs:2",
			"keys in the hash table with no place in docs's keys: docs:2",
		}},
		{"a record missing from the hash table", func(s *Store) {
			delete(s.records, "people:2")
		}, []string{
			"keys in people missing from the hash table: people:2",
			"links from keys that don't exist: people:2", // its link to people:1
		}},
		{"a record with no place in its table's keys", func(s *Store) {
			s.tables["docs"].keys.remove("docs:3")
		}, []string{
			"table docs has 2 records with a vector, and 3 slots in use",
			"keys in the hash table with no place in docs's keys: docs:3",
		}},
		{"a record in a table that isn't there", func(s *Store) {
			s.records["gone:1"] = stray("gone:1", "gone")
		}, []string{"keys in the hash table for tables that aren't there: gone:1"}},
		{"a record filed under another key", func(s *Store) {
			s.records["people:3"] = r(s, "people:2")
		}, []string{"keys in the hash table held with the wrong record: people:3"}},
		{"a key in another table's keys", func(s *Store) {
			p := &record{key: "people:7", table: s.tables["docs"], slot: -1}
			s.records[p.key] = p
			s.tables["docs"].keys.insert(p)
		}, []string{`keys in docs that don't start with docs:: "people:7"`}},
		{"a record's fields out of order", func(s *Store) {
			f := r(s, "docs:3").fields
			f = append(f, FieldValue{Index: 0, Value: value.Text("again")})
			r(s, "docs:3").fields = f
		}, []string{"records in docs with fields out of order, null or outside the table's list: docs:3"}},
		{"a null kept in a record's fields", func(s *Store) {
			r(s, "people:1").fields = append(r(s, "people:1").fields, FieldValue{Index: 0})
		}, []string{"records in people with fields out of order, null or outside the table's list: people:1"}},
		{"a vector of zeros", func(s *Store) {
			clear(s.tables["docs"].vector(r(s, "docs:1").slot))
		}, []string{"the vector of docs:1 is all zeros"}},
		{"a vector with a value that isn't a number", func(s *Store) {
			s.tables["docs"].vector(r(s, "docs:2").slot)[2] = float32(math.NaN())
		}, []string{"the vector of docs:2 has a value that isn't a finite number"}},
		{"a vector whose norm isn't its own", func(s *Store) {
			s.tables["docs"].vecs.norms[r(s, "docs:3").slot] = 2
		}, []string{fmt.Sprintf("the vector of docs:3 has the norm 2, where its values give %v", math.Sqrt(0.5*0.5+2*2))}},
		{"a free slot on no free list", func(s *Store) {
			s.tables["docs"].vecs.free = nil
		}, []string{"free slots of docs's vector array on no free list: 3"}},
		{"a slot on the free list twice", func(s *Store) {
			a := &s.tables["docs"].vecs
			a.free = append(a.free, a.free[0])
		}, []string{"slots on docs's free list that are in use, past its end or there twice: 3"}},
		{"a slot whose record has gone", func(s *Store) {
			a := &s.tables["docs"].vecs
			a.owners[3], a.free = stray("docs:4", "docs"), nil
		}, []string{
			"slots of docs's vector array held for a record that isn't there or names another slot: 3",
			"table docs has 3 records with a vector, and 4 slots in use",
		}},
		{"a record whose slot is free", func(s *Store) {
			a := &s.tables["docs"].vecs
			i := r(s, "docs:2").slot
			a.owners[i], a.free = nil, append(a.free, i)
		}, []string{
			"records in docs whose vector isn't in their slot: docs:2",
			"table docs has 3 records with a vector, and 2 slots in use",
		}},
		{"vectors in a table without a vector size", func(s *Store) {
			s.tables["docs"].size = 0
		}, []string{
			"table docs has no vector size, and an array of 4 slots",
			"the vector of docs:1 is there, but the table has no vector size recorded",
			"the vector of docs:2 is there, but the table has no vector size recorded",
			"the vector of docs:3 is there, but the table has no vector size recorded",
		}},
		{"a table's vector field out of place", func(s *Store) {
			s.tables["docs"].vec = 0
		}, []string{
			"table docs has its vector field at place 0, where its list has it at 1",
			"records in docs with fields out of order, null or outside the table's list: docs:1, docs:3", // their titles, at place 0
		}},
		{"a table's index out of step with its fields", func(s *Store) {
			s.tables["people"].index["name"] = 3
		}, []string{"fields of people that its index has at another place: name"}},
		{"a table's count of keys wrong", func(s *Store) {
			s.tables["people"].keys.n++
		}, []string{"table people counts 3 keys, and its blocks hold 2"}},
		{"a list of links with an empty block", func(s *Store) {
			l := &r(s, "people:1").out
			l.blocks = append(l.blocks, []half{})
		}, []string{"records whose links out are in a block that's empty or too long: people:1"}},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := healthy(t)
			c.plant(s)
			got := s.Check()
			if strings.Join(got.Problems, "\n") != strings.Join(c.want, "\n") {
				t.Fatalf("Check gave the problems\n%s\nwhere\n%s\nis wanted", strings.Join(got.Problems, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

// TestTheCheckCounts follows the counts through deletes, a cleared vector, a
// dropped table and an unlink: a table stays once its records have gone, a
// record counts as having a vector until it's cleared, and a link counts
// once, even from a record to itself.
func TestTheCheckCounts(t *testing.T) {
	s := healthy(t)
	counts := func(tables, records, links, vectors int) {
		t.Helper()
		got := s.Check()
		if len(got.Problems) != 0 || got.Tables != tables || got.Records != records || got.Links != links || got.Vectors != vectors {
			t.Fatalf("Check gave %+v, where %d tables, %d records, %d links and %d vectors are wanted", got, tables, records, links, vectors)
		}
	}
	counts(3, 5, 5, 3)
	if _, err := s.Put(nil, "docs:1", []format.Field{field("vec", value.Null())}); err != nil {
		t.Fatal(err)
	}
	counts(3, 5, 5, 2)
	if _, err := s.Delete(nil, "docs:2"); err != nil { // with its 3 links: owns from people:1, cites from docs:1, cites to itself
		t.Fatal(err)
	}
	counts(3, 4, 2, 1)
	if _, err := s.Unlink(nil, "people:2", "", "people:1"); err != nil {
		t.Fatal(err)
	}
	counts(3, 4, 1, 1)
	if _, err := s.Drop(nil, "people"); err != nil {
		t.Fatal(err)
	}
	counts(2, 2, 0, 1)
	if _, err := s.Delete(nil, "docs:3"); err != nil {
		t.Fatal(err)
	}
	counts(2, 1, 0, 0)
}

// TestTheCheckFindsNothingTheWritesLeave makes random writes, direct and in
// transactions, half of those rolled back, with links among few records so
// deletes and drops take many, and vectors that come and go. After every
// step the store keeps the invariants the model test checks, and Check
// finds no problems and counts what the store's snapshot holds.
func TestTheCheckFindsNothingTheWritesLeave(t *testing.T) {
	steps := 3000
	if testing.Short() {
		steps = 600
	}
	r := rand.New(rand.NewPCG(7, 13))
	s := New()
	tables := []string{"docs", "people", "t_1"}
	key := func() string { return tables[r.IntN(len(tables))] + ":" + fmt.Sprint(r.IntN(8)) }
	types := []string{"owns", "cites", "é"}
	// The writes go through the writer that TestUndoEachChange uses, on the
	// store or in a transaction, and those that break a rule fail.
	write := func(w writer) {
		switch r.IntN(10) {
		case 0, 1, 2, 3:
			fields := []format.Field{field("n", value.Int(r.Int64()))}
			switch r.IntN(4) {
			case 0:
				fields = append(fields, field("vec", value.Vector([]float32{float32(r.NormFloat64()), 1, float32(r.IntN(3))})))
			case 1:
				fields = append(fields, field("vec", value.Null()))
			case 2:
				fields = append(fields, field("title", value.Text(strings.Repeat("é", r.IntN(20)))))
			}
			w.Put(key(), fields)
		case 4, 5, 6:
			w.Link(key(), types[r.IntN(len(types))], key())
		case 7:
			w.Unlink(key(), "", key())
		case 8:
			w.Delete(key())
		case 9:
			if r.IntN(5) == 0 {
				w.Drop(tables[r.IntN(len(tables))])
			} else {
				w.Delete(key())
			}
		}
	}
	for step := range steps {
		if r.IntN(3) == 0 {
			write(directWriter{s})
		} else {
			tx, err := s.Begin()
			if err != nil {
				t.Fatal(err)
			}
			for range 1 + r.IntN(4) {
				write(tx)
			}
			if r.IntN(2) == 0 {
				tx.Rollback()
			} else if err := tx.Commit(nil); err != nil {
				t.Fatal(err)
			}
		}
		checkInvariants(t, s)
		got := s.Check()
		var want Checked
		for c := range s.Snapshot() {
			switch c.Op {
			case format.CreateTable:
				want.Tables++
			case format.Put:
				want.Records++
				for _, f := range c.Fields {
					if f.Value.Kind() == value.KindVector {
						want.Vectors++
					}
				}
			case format.Link:
				want.Links++
			}
		}
		if len(got.Problems) != 0 || got.Tables != want.Tables || got.Records != want.Records || got.Links != want.Links || got.Vectors != want.Vectors {
			t.Fatalf("step %d: Check gave %+v, where the snapshot holds %+v", step, got, want)
		}
	}
}

// liveHeap is the bytes the heap holds after a garbage collection.
func liveHeap() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// TestTheCopysSizeIsWhatItTakes holds Check's Bytes to what the heap grows
// by as a store fills, as LoadBatch fills one on opening, within an eighth:
// records with 384-value vectors, as BETA.md's figure of 1.7 KB a record
// has them, records with a few fields and five links each and no vector, and
// records with longer text.
func TestTheCopysSizeIsWhatItTakes(t *testing.T) {
	n := 4000
	if testing.Short() {
		n = 2000
	}
	vec := make([]float32, 384)
	for _, c := range []struct {
		what string
		fill func(s *Store, i int)
	}{
		{"records with 384-value vectors", func(s *Store, i int) {
			for j := range vec {
				vec[j] = float32(i%97+j) + 0.5
			}
			load(t, s, format.Change{Op: format.Put, Key: fmt.Sprintf("docs:%06d", i), Fields: []format.Field{field("vec", value.Vector(vec))}})
		}},
		{"records with fields and five links each", func(s *Store, i int) {
			key := fmt.Sprintf("docs:%06d", i)
			load(t, s, format.Change{Op: format.Put, Key: key, Fields: []format.Field{field("n", value.Int(int64(i))), field("title", value.Text("a title"))}})
			for j := 1; j <= 5 && j <= i; j++ {
				load(t, s, format.Change{Op: format.Link, Key: key, Type: "cites", To: fmt.Sprintf("docs:%06d", i-j)})
			}
		}},
		{"records with longer text", func(s *Store, i int) {
			load(t, s, format.Change{Op: format.Put, Key: fmt.Sprintf("docs:%06d", i), Fields: []format.Field{field("body", value.Text(strings.Repeat("words ", 50+i%50)))}})
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			before := liveHeap()
			s := New()
			load(t, s, format.Change{Op: format.CreateTable, Table: "docs"})
			for i := range n {
				c.fill(s, i)
			}
			took := liveHeap() - before
			got := s.Check()
			runtime.KeepAlive(s)
			if len(got.Problems) != 0 {
				t.Fatalf("Check found problems: %q", got.Problems)
			}
			ratio := float64(got.Bytes) / float64(took)
			t.Logf("%d %s: Check gives %d bytes, %.0f a record, and the heap grew by %d, a ratio of %.3f", n, c.what, got.Bytes, float64(got.Bytes)/float64(n), took, ratio)
			if ratio < 7.0/8 || ratio > 9.0/8 {
				t.Errorf("Check gives %d bytes for a store that took %d", got.Bytes, took)
			}
		})
	}
}

// load applies one change to s as a batch of its own, as opening a file
// does.
func load(t *testing.T, s *Store, c format.Change) {
	t.Helper()
	if err := s.LoadBatch(1, []format.Change{c}); err != nil {
		t.Fatal(err)
	}
}
