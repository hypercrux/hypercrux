// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// V2's vector blocks through the public package: a compaction writes each
// table's vectors as vector blocks, and opening the file, or reloading it,
// reads them back bit for bit. A damaged block is found wherever the file is
// read.

// awkwardValues holds float32s whose bits have to come back as they are: -0,
// the smallest and the largest subnormal, the smallest normal value, the
// largest and the smallest finite values, and 1.
var awkwardValues = []float32{
	float32(math.Copysign(0, -1)), math.Float32frombits(1), math.Float32frombits(0x007fffff),
	math.Float32frombits(0x00800000), math.MaxFloat32, -math.MaxFloat32, 1,
}

// sameBits reports whether two vectors hold the same values bit for bit,
// where -0 and 0 differ, and whether both are there or neither is.
func sameBits(a, b hc.Vector) bool {
	return (a == nil) == (b == nil) &&
		slices.EqualFunc(a, b, func(x, y float32) bool { return math.Float32bits(x) == math.Float32bits(y) })
}

// vectorRecords puts into db what a compaction's vector blocks have to
// carry, and returns each record's vector by key, nil for a record with
// none: several tables with vectors, of sizes 1, 3, 128 and 65,536, with the
// awkward values; records with no vector among them; a table whose slots
// were freed and taken again, so its slots come in another order than its
// keys; a table of 3,000 vectors, whose blocks fill more than one batch of
// a megabyte; and a table with no vectors.
func vectorRecords(t *testing.T, db *hc.DB) map[string]hc.Vector {
	t.Helper()
	want := map[string]hc.Vector{}
	put := func(key string, v hc.Vector, f hc.Fields) {
		t.Helper()
		if f == nil {
			f = hc.Fields{}
		}
		if v != nil {
			f["vec"] = v
		} else if _, set := f["vec"]; !set {
			f["n"] = len(key)
		}
		ok(t, db.Put(key, f))
		want[key] = v
	}
	a := awkwardValues
	put("docs:1", hc.Vector{a[0], a[1], a[2]}, nil)
	put("docs:2", hc.Vector{a[2], a[3], a[4]}, nil)
	put("docs:3", hc.Vector{a[4], a[5], a[6]}, nil)
	put("docs:4", hc.Vector{1, 1, 1}, nil)
	put("docs:5", nil, hc.Fields{"title": "no vector"})
	ok(t, db.Delete("docs:2"))
	delete(want, "docs:2")
	put("docs:9", hc.Vector{a[0], a[0], 9}, nil) // into the slot docs:2 freed
	put("docs:3", nil, hc.Fields{"vec": nil})    // its slot freed
	put("docs:0", hc.Vector{a[5], a[1], a[3]}, nil)
	put("ones:a", hc.Vector{a[1]}, nil)
	put("ones:b", hc.Vector{a[5]}, nil)
	put("ones:c", hc.Vector{a[3]}, hc.Fields{"title": "smallest normal"})
	wide := func(shift int) hc.Vector {
		v := make(hc.Vector, 65536)
		for i := range v {
			v[i] = a[(i+shift)%len(a)]
		}
		return v
	}
	put("wide:1", wide(0), nil)
	put("wide:2", wide(3), nil)
	ok(t, db.Update(func(tx *hc.Tx) error {
		for i := range 3000 {
			v := make(hc.Vector, 128)
			for j := range v {
				v[j] = float32(i) - float32(j)/3
			}
			v[i%128] = a[i%len(a)]
			key := fmt.Sprintf("many:%04d", i)
			if err := tx.Put(key, hc.Fields{"vec": v}); err != nil {
				return err
			}
			want[key] = v
		}
		return nil
	}))
	put("plain:1", nil, hc.Fields{"title": "no vectors here"})
	return want
}

// hitsOf returns, for each query, what a search of its table gives, every
// record by distance.
func hitsOf(t *testing.T, db *hc.DB, queries map[string]hc.Vector) map[string][]hc.Hit {
	t.Helper()
	out := map[string][]hc.Hit{}
	for table, q := range queries {
		hits, err := db.Nearest(table, q, hc.MaxK, "")
		ok(t, err)
		out[table] = hits
	}
	return out
}

// TestACompactionKeepsEveryVectorsBits is V2's closing test: a compaction
// round trip keeps every vector's bits. The database holds vectors of sizes
// 1, 3, 128 and 65,536, with -0, subnormals and the largest and smallest
// finite values among them, in several tables, beside records with no
// vector, in a table whose slots were freed and taken again. Compact writes
// them as vector blocks, which fill batches of up to a megabyte, and no put
// in the compacted part carries a vector. Then the database opened afresh,
// and a handle that had it open and reloads the compacted file, give every
// vector back bit for bit, with the same search results, distances
// included, which shows the norms are the ones a put works out, and Check
// finds nothing.
func TestACompactionKeepsEveryVectorsBits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	other := open(t, path) // reads the compacted file by a reload
	want := vectorRecords(t, db)
	vectors := 0
	for _, v := range want {
		if v != nil {
			vectors++
		}
	}
	queries := map[string]hc.Vector{"docs": {1, -2, 0.5}, "ones": {-1}, "many": make(hc.Vector, 128), "wide": make(hc.Vector, 65536)}
	queries["many"][7], queries["wide"][1] = 1, -1
	before := hitsOf(t, db, queries)
	ok(t, db.Compact())

	// The compacted part: every vector in a block, alone in its batch.
	file, err := os.ReadFile(path)
	ok(t, err)
	h, err := format.DecodeHeader(file, int64(len(file)))
	ok(t, err)
	inBlocks, blocks := 0, 0
	for off, seq := int64(format.HeaderSize), uint64(1); off < int64(h.CompactedEnd); seq++ {
		bt, err := format.DecodeBatch(file[off:], h.Gen, seq)
		ok(t, err)
		for _, c := range bt.Changes {
			switch c.Op {
			case format.Vectors:
				if len(bt.Changes) != 1 {
					t.Errorf("batch %d holds a vector block and other changes", seq)
				}
				inBlocks += len(c.Vecs)
				blocks++
			case format.Put:
				if slices.ContainsFunc(c.Fields, func(f format.Field) bool { return f.Name == "vec" }) {
					t.Errorf("batch %d puts %s's vector", seq, c.Key)
				}
			}
		}
		off += int64(bt.Length) + format.MarkerSize
	}
	if inBlocks != vectors || blocks < 5 {
		t.Fatalf("the compacted part's %d vector blocks hold %d vectors, where the database holds %d", blocks, inBlocks, vectors)
	}

	again := open(t, path)
	for _, c := range []struct {
		name string
		db   *hc.DB
	}{{"the handle that compacted", db}, {"a handle that reloaded", other}, {"a handle opened afresh", again}} {
		for key, v := range want {
			f, err := c.db.Get(key)
			ok(t, err)
			got, _ := f["vec"].(hc.Vector)
			if !sameBits(got, v) {
				t.Fatalf("%s gives %s the vector %v, where it was %v", c.name, key, got, v)
			}
		}
		after := hitsOf(t, c.db, queries)
		for table, hits := range before {
			if !slices.EqualFunc(after[table], hits, func(x, y hc.Hit) bool {
				return x.Key == y.Key && math.Float64bits(x.Distance) == math.Float64bits(y.Distance)
			}) {
				t.Fatalf("%s gives a search of %s other hits than before the compaction", c.name, table)
			}
		}
		rep, err := c.db.Check()
		ok(t, err)
		if !rep.OK() || rep.Vectors != vectors {
			t.Fatalf("%s's Check gives %d vectors and %q, where %d vectors and no problems are wanted", c.name, rep.Vectors, rep.Problems, vectors)
		}
	}
}

// resealed returns a copy of b, a database file, with fix applied to the
// changes of the batch of the length n that starts at off, and the batch's
// checksum and its marker made to fit again, as a writer would have written
// the batch with those changes.
func resealed(t *testing.T, b []byte, off, n int64, fix func(changes []byte)) []byte {
	t.Helper()
	h, err := format.DecodeHeader(b, int64(len(b)))
	ok(t, err)
	c := slices.Clone(b)
	fix(c[off+format.BatchHeadSize : off+n-4])
	sum := crc32.Checksum(c[off:off+n-4], crc32.MakeTable(crc32.Castagnoli))
	binary.LittleEndian.PutUint32(c[off+n-4:], sum)
	seq := binary.LittleEndian.Uint64(c[off+20:])
	copy(c[off+n:], format.AppendMarker(nil, h.ID, h.Gen, format.Marker{Seq: seq, Sum: sum}))
	return c
}

// TestADamagedBlockIsFound damages a vector block in a compacted file, two
// ways. A bit changed in a block the database has read already is damage
// Check's read of the file finds, by the batch's checksum, naming the batch,
// and Open fails on the file. A block written again with NaN in a vector,
// its checksum and marker fitting, is a block the store refuses: moved into
// place as a backup under an open database, Check names it once the read
// before Check reloads the file, and Open fails on it, with the same batch,
// offset and reason. The block-named damaged fixtures beside the log's tests
// cover the other kinds, each opened, and checked under an open database, by
// TestCheckNamesTheDamagedFixtures.
func TestADamagedBlockIsFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.hcx")
	db := open(t, path)
	ok(t, db.Put("pics:1", hc.Fields{"vec": hc.Vector{1, 2}}))
	ok(t, db.Put("pics:2", hc.Fields{"vec": hc.Vector{3, 4}, "n": 2}))
	ok(t, db.Compact())
	clean, err := os.ReadFile(path)
	ok(t, err)
	var block spot
	for i, s := range batchesIn(t, path) {
		if bt, err := format.DecodeBatch(clean[s.at:], 2, uint64(i+1)); err == nil && bt.Changes[0].Op == format.Vectors {
			block = s
		}
	}
	if block.at == 0 {
		t.Fatal("the compacted file holds no vector block")
	}
	seq := binary.LittleEndian.Uint64(clean[block.at+20:])
	valuesAt := block.at + block.n - 4 - 2*2*4 // the values end the block, just before the checksum

	// A bit changed in the block's values, which the database read.
	flip(t, path, valuesAt+5, 0x40)
	rep, err := db.Check()
	want := fmt.Sprintf("the file is damaged: batch %d at offset %d: the checksum is ", seq, block.at)
	if err != nil || len(rep.Problems) != 1 || !strings.HasPrefix(rep.Problems[0], want) {
		t.Fatalf("with a bit changed in the vector block, Check gives %q, %v, where one problem starting %q is wanted", rep.Problems, err, want)
	}
	if again, err := hc.Open(path); err == nil {
		again.Close()
		t.Fatal("Open takes a file whose vector block fails its checksum")
	} else {
		wantErr(t, err, hc.ErrDamaged)
	}

	// The block written again with NaN in pics:1's vector, moved into place.
	nan := resealed(t, clean, block.at, block.n, func(changes []byte) {
		binary.LittleEndian.PutUint32(changes[len(changes)-16+4:], math.Float32bits(float32(math.NaN())))
	})
	ok(t, os.WriteFile(path, clean, 0o644))
	db = open(t, path)
	ok(t, os.WriteFile(path+".backup", nan, 0o644))
	ok(t, os.Rename(path+".backup", path))
	rep, err = db.Check()
	want = fmt.Sprintf("the file is damaged: batch %d at offset %d: change 1: the vector of pics:1 in the vector block for table pics: ", seq, block.at)
	if err != nil || len(rep.Problems) != 1 || !strings.HasPrefix(rep.Problems[0], want) || !strings.Contains(rep.Problems[0], "NaN") {
		t.Fatalf("with NaN in the vector block, Check gives %q, %v, where one problem starting %q is wanted", rep.Problems, err, want)
	}
	_, err = hc.Open(path)
	if d := (*hc.Damage)(nil); !errors.As(err, &d) || d.Batch != seq || d.Offset != block.at || !strings.Contains(d.Reason, "NaN") {
		t.Fatalf("Open of a file with NaN in its vector block gives %v", err)
	}
	if now, err := os.ReadFile(path); err != nil || !slices.Equal(now, nan) {
		t.Fatal("reading the damaged file changed it")
	}
}
