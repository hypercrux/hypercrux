// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The log's side of the vector blocks (V2): compaction joins a table's
// vectors into blocks, each alone in its batch and as full as a batch
// holds, and a read takes a block only from the compacted part.

// vectorBits returns every vector s holds, by key, as its bits, and the
// rest of what s holds as text, a change a line, from s's compacted
// snapshot.
func vectorBits(s *store.Store) (map[string]string, string) {
	out := map[string]string{}
	var rest strings.Builder
	s.Read(func(store.Reader) error {
		for c := range s.Compacted() {
			if c.Op != format.Vectors {
				rest.WriteString(c.String())
				rest.WriteByte('\n')
			}
			for _, v := range c.Vecs {
				out[v.Key] = strings.Clone(v.Bits)
			}
		}
		return nil
	})
	return out, rest.String()
}

// vectorDB makes a database at path whose vectors fill several batches of
// the compacted part: 1,000 of 384 values in a, some of whose slots were
// freed and taken again, 50 of one value in b, three of 65,536 values in w,
// each over a quarter of a megabyte, records with no vector among them, a
// table with no vectors, and links. The values are awkward ones and others.
func vectorDB(t *testing.T, path string) *db {
	t.Helper()
	d := openDBT(t, fsys.OS{}, path, Options{})
	awkward := []float32{float32(math.Copysign(0, -1)), math.Float32frombits(1), math.MaxFloat32, -math.MaxFloat32, math.Float32frombits(0x00800000)}
	vec := func(n, i int) value.Value {
		v := make([]float32, n)
		for j := range v {
			v[j] = float32(i) + float32(j)/7
			if (i+j)%11 == 0 {
				v[j] = awkward[(i+j)%len(awkward)]
			}
		}
		return value.Vector(v)
	}
	put := func(tx *store.Tx, key string, fields ...format.Field) error { return tx.Put(key, fields) }
	f := func(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }
	for from := 0; from < 1000; from += 100 {
		err := d.commit(func(tx *store.Tx) error {
			for i := from; i < from+100; i++ {
				if err := put(tx, fmt.Sprintf("a:%04d", i), f("n", value.Int(int64(i))), f("vec", vec(384, i))); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err := d.commit(func(tx *store.Tx) error {
		for i := 10; i < 20; i++ {
			if err := tx.Delete(fmt.Sprintf("a:%04d", i)); err != nil {
				return err
			}
		}
		for i := range 10 {
			// Keys after every other, in the slots the deletes freed.
			if err := put(tx, fmt.Sprintf("a:x%d", i), f("vec", vec(384, 2000+i))); err != nil {
				return err
			}
		}
		for i := range 60 {
			fields := []format.Field{f("n", value.Int(int64(i)))}
			if i%6 != 0 {
				fields = append(fields, f("vec", vec(1, i+1)))
			}
			if err := put(tx, fmt.Sprintf("b:%02d", i), fields...); err != nil {
				return err
			}
		}
		for i := range 3 {
			if err := put(tx, fmt.Sprintf("w:%d", i), f("vec", vec(65536, i))); err != nil {
				return err
			}
		}
		if err := put(tx, "c:1", f("t", value.Text("no vectors"))); err != nil {
			return err
		}
		return tx.Link("c:1", "shows", "a:x3")
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestCompactingVectorsIntoBlocks compacts a database whose vectors fill
// several batches, with batches of 64 KB and of a megabyte. Each table's
// vectors come after its puts in vector blocks, each alone in its batch, in
// byte order of key, and every block is as full as its batch holds but the
// table's last, or holds one vector that doesn't fit in a batch on its own.
// The blocks hold every vector bit for bit, and the file opens with the
// same vectors whether the log hands the blocks over in place or as copies,
// to a copy whose check finds nothing.
func TestCompactingVectorsIntoBlocks(t *testing.T) {
	for _, part := range []int{64 << 10, partSize} {
		t.Run(fmt.Sprint(part), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			d := vectorDB(t, path)
			want, state := vectorBits(d.c.s)
			if len(want) != 1000+50+3 {
				t.Fatalf("the copy holds %d vectors", len(want))
			}
			d.l.part = part
			if err := d.compact(); err != nil {
				t.Fatal(err)
			}

			got := map[string]string{}
			var table, last string // the table whose changes come now, and its last vector's key
			var lastBlock *format.Change
			blocks := 0
			for _, bt := range batchesOf(t, path) {
				for _, c := range bt.Changes {
					switch c.Op {
					case format.CreateTable:
						table, last, lastBlock = c.Table, "", nil
						continue
					case format.Put:
						if lastBlock != nil {
							t.Fatalf("batch %d puts %s after its table's vector blocks", bt.Seq, c.Key)
						}
						continue
					case format.Vectors:
					default:
						continue
					}
					blocks++
					n := changeSize(&c)
					switch {
					case len(bt.Changes) != 1:
						t.Fatalf("batch %d holds a vector block and other changes", bt.Seq)
					case c.Table != table:
						t.Fatalf("batch %d holds a vector block for %s among the changes of %s", bt.Seq, c.Table, table)
					case c.Vecs[0].Key <= last:
						t.Fatalf("batch %d's vector block starts at %s, after %s", bt.Seq, c.Vecs[0].Key, last)
					case n > part && len(c.Vecs) > 1:
						t.Fatalf("batch %d's vector block takes %d bytes, past the %d asked for", bt.Seq, n, part)
					case lastBlock != nil && changeSize(lastBlock)+format.BlockVectorSize(c.Vecs[0]) <= part:
						t.Fatalf("batch %d starts a vector block where the one before had room for its first vector", bt.Seq)
					}
					for i, v := range c.Vecs {
						if i > 0 && v.Key <= c.Vecs[i-1].Key {
							t.Fatalf("batch %d's vector block gives %s after %s", bt.Seq, v.Key, c.Vecs[i-1].Key)
						}
						got[v.Key] = v.Bits
					}
					last, lastBlock = c.Vecs[len(c.Vecs)-1].Key, &c
				}
			}
			if !maps.Equal(got, want) {
				t.Fatalf("the vector blocks hold %d vectors, and differ from the copy's %d", len(got), len(want))
			}
			if part == partSize && blocks != 4 || part < partSize && blocks < 20 {
				t.Errorf("the compacted part holds %d vector blocks, with batches of %d bytes", blocks, part)
			}

			for _, inPlace := range []bool{true, false} {
				c := &copyTarget{s: store.New(), opening: true}
				l, err := Open(fsys.OS{}, path, c, Options{BlocksInPlace: inPlace})
				if err != nil {
					t.Fatal(err)
				}
				l.Close()
				again, rest := vectorBits(c.s)
				if !maps.Equal(again, want) {
					t.Errorf("in place %v, the compacted file opens with %d vectors, which differ from the %d written", inPlace, len(again), len(want))
				}
				if rest != state {
					t.Errorf("in place %v, the compacted file opens holding\n%s\nwhere the copy held\n%s", inPlace, rest, state)
				}
				if ch := c.s.Check(); len(ch.Problems) != 0 || ch.Vectors != len(want) {
					t.Errorf("in place %v, the copy's check finds %d vectors and %q", inPlace, ch.Vectors, ch.Problems)
				}
			}
		})
	}
}

// TestBlocksComeAsCopiesUnlessAsked: without Options.BlocksInPlace, a Target
// that keeps the changes it's handed keeps every vector of a compacted file
// as it was written, though the log reads the file's many batches of vector
// blocks through one buffer, which takes megabyte after megabyte.
func TestBlocksComeAsCopiesUnlessAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	d := vectorDB(t, path)
	want, _ := vectorBits(d.c.s)
	d.l.part = 16 << 10
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	rec := reread(t, path)
	got := map[string]string{}
	for _, a := range rec.batches {
		for _, c := range a.changes {
			for _, v := range c.Vecs {
				got[v.Key] = v.Bits
			}
		}
	}
	if !maps.Equal(got, want) {
		n := 0
		for k, v := range want {
			if got[k] != v {
				n++
			}
		}
		t.Fatalf("a Target that kept the changes holds %d of the %d vectors other than they were written", n, len(want))
	}
}

// TestABlockOutsideTheCompactedPartIsDamage: a batch that holds a vector
// block, past the compacted part or in generation 1, which has none, is
// damage wherever the log reads it, marked or not: on opening, when a
// follower reads it, and at Lock. The damage names the batch and the
// block's offset, and nothing in the file changes, so an unmarked batch
// isn't marked.
func TestABlockOutsideTheCompactedPartIsDamage(t *testing.T) {
	block := []format.Change{{Op: format.Vectors, Table: "docs", Size: 1, Vecs: []format.Vector{{Key: "docs:1", Bits: "\x00\x00\x80\x3f"}}}}
	for _, compacted := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "db")
		l, _ := openLog(t, path, Options{})
		commit(t, l, append(table("docs"), format.Change{Op: format.Put, Key: "docs:1"}))
		why := "a vector block, in generation 1, which has no compacted part"
		if compacted {
			d := openDBT(t, fsys.OS{}, path, Options{})
			if err := d.compact(); err != nil {
				t.Fatal(err)
			}
			why = fmt.Sprintf("a vector block, past the compacted part, which ends at offset %d", headerOf(t, path).CompactedEnd)
			l.Close()
			l, _ = openLog(t, path, Options{})
		}
		before := fileBytes(t, path)
		h := headerOf(t, path)
		w := &builder{t: t, hdr: h, b: before, seq: l.seq}
		sum := w.batch(append(table("pics"), block...))
		at := int64(len(before)) + format.BatchHeadSize + int64(changeSize(&table("pics")[0]))
		reported := func(how string, err error, file []byte) {
			t.Helper()
			var d *errs.Damage
			if !errors.As(err, &d) || d.Batch != w.seq || d.Offset != at || !strings.Contains(d.Reason, "change 2: "+why) {
				t.Errorf("compacted %v: %s gave %v, where damage naming batch %d at offset %d is wanted", compacted, how, err, w.seq, at)
			}
			if !slicesEqual(fileBytes(t, path), file) {
				t.Errorf("compacted %v: %s changed the file", compacted, how)
			}
		}

		unmarked := w.b
		write(t, path, unmarked)
		_, err := Open(fsys.OS{}, path, &recorder{}, Options{})
		reported("Open, with the block's batch unmarked", err, unmarked)

		w.marker(w.seq, sum)
		write(t, path, w.b)
		reported("Follow", l.Follow(), w.b)
		_, err = Open(fsys.OS{}, path, &recorder{}, Options{})
		reported("Open", err, w.b)
		reported("Lock", lockErr(l), w.b)
	}
}

// slicesEqual reports whether a and b hold the same bytes.
func slicesEqual(a, b []byte) bool { return string(a) == string(b) }

// TestABatchOfTheCompactedPartHoldsAtMost1024Changes: a table of 2,500
// small records, which would fit in one batch of a megabyte, compacts to
// batches of 1,024 changes at most, so a reader's lists of changes stay
// short, and the file opens with every record.
func TestABatchOfTheCompactedPartHoldsAtMost1024Changes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	d := openDBT(t, fsys.OS{}, path, Options{})
	err := d.commit(func(tx *store.Tx) error {
		for i := range 2500 {
			if err := tx.Put(fmt.Sprintf("t:%04d", i), []format.Field{{Name: "n", Value: value.Int(int64(i))}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state := d.state()
	if err := d.compact(); err != nil {
		t.Fatal(err)
	}
	batches := batchesOf(t, path)
	for _, bt := range batches {
		if len(bt.Changes) > partChanges {
			t.Errorf("batch %d of the compacted part holds %d changes", bt.Seq, len(bt.Changes))
		}
	}
	if len(batches) != 3 {
		t.Errorf("2,500 records and their table compact to %d batches, where 3 are wanted", len(batches))
	}
	if got := stateOfFile(t, path); got != state {
		t.Errorf("the compacted file holds\n%s\nwhere the copy held\n%s", got, state)
	}
}
