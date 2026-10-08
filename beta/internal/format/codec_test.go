// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	val "github.com/hypercrux/hypercrux/beta/internal/value"
)

// The codec's tests. The fixtures are the judge: each one decodes and
// encodes again byte for byte, and the codec reads the same changes in them
// as the reader in fixtures_test.go, which is written from FORMAT.md alone
// and stays apart from the codec as a check on it.

// fixtureID is the database ID every fixture uses.
var fixtureID = [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}

// sameChanges reports whether two change lists hold the same changes, each
// value with its kind and bits. A nil slice and an empty one are the same.
func sameChanges(a, b []Change) bool {
	return slices.EqualFunc(a, b, func(x, y Change) bool {
		return x.Op == y.Op && x.Table == y.Table && x.Size == y.Size && slices.Equal(x.Names, y.Names) &&
			x.Key == y.Key && slices.Equal(x.Fields, y.Fields) && x.Type == y.Type && x.To == y.To
	})
}

// toChanges turns what fixtures_test.go's reader read into a change list.
func toChanges(cs []change) []Change {
	var out []Change
	for _, c := range cs {
		out = append(out, toChange(c))
	}
	return out
}

// seal makes a batch of changes already encoded, whatever they hold, with
// the length and the checksum that fit them.
func seal(gen, seq uint64, changes []byte) []byte {
	b := []byte(magicBatch)
	b = binary.LittleEndian.AppendUint64(b, gen)
	b = binary.LittleEndian.AppendUint64(b, uint64(BatchHeadSize+len(changes)+4))
	b = binary.LittleEndian.AppendUint64(b, seq)
	b = append(b, changes...)
	return binary.LittleEndian.AppendUint32(b, sum(b))
}

// patch returns a copy of b with p written over it at offset at.
func patch(b []byte, at int, p []byte) []byte {
	c := slices.Clone(b)
	copy(c[at:], p)
	return c
}

func u32(n uint32) []byte { return binary.LittleEndian.AppendUint32(nil, n) }
func u64(n uint64) []byte { return binary.LittleEndian.AppendUint64(nil, n) }

func TestFixturesComeBackByteForByte(t *testing.T) {
	compacted := readFixture(t, "file-compacted.hex")
	for _, name := range fixtures {
		b := readFixture(t, name)
		var out []byte
		switch {
		case strings.HasPrefix(name, "header-"):
			h, err := DecodeHeader(b, int64(len(compacted)))
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if out, err = AppendHeader(nil, h); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
		case name == "marker.hex":
			m, ok := DecodeMarker(b, fixtureID, 1)
			if !ok {
				t.Errorf("%s isn't a whole marker", name)
				continue
			}
			out = AppendMarker(nil, fixtureID, 1, m)
		case strings.HasPrefix(name, "batch-"):
			seq := uint64(258)
			if name == "batch-several.hex" {
				seq = 1
			}
			bt, err := DecodeBatch(b, 1, seq)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			ref, err := readBatch(b, 1)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !sameChanges(bt.Changes, toChanges(ref.changes)) || bt.Length != ref.length || bt.Sum != ref.sum || bt.Seq != seq {
				t.Errorf("%s reads as %d bytes, %#08x, %v, where fixtures_test.go's reader finds %d bytes, %#08x, %v",
					name, bt.Length, bt.Sum, bt.Changes, ref.length, ref.sum, toChanges(ref.changes))
			}
			var s uint32
			if out, s, err = AppendBatch(nil, 1, seq, bt.Changes); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if s != bt.Sum {
				t.Errorf("%s: AppendBatch gives the checksum %#08x, where the fixture holds %#08x", name, s, bt.Sum)
			}
		case strings.HasPrefix(name, "file-"):
			out = rewriteFile(t, name, b)
		default:
			t.Fatalf("no test for %s", name)
		}
		if !bytes.Equal(out, b) {
			t.Errorf("%s comes back as\n% x\nwhere it holds\n% x", name, out, b)
		}
	}

	h, err := DecodeHeader(readFixture(t, "header-new.hex"), HeaderSize)
	if want := (Header{ID: fixtureID, Gen: 1}); err != nil || h != want {
		t.Errorf("header-new.hex reads as %+v, %v, where it holds %+v", h, err, want)
	}
	h, err = DecodeHeader(compacted, int64(len(compacted)))
	if want := (Header{ID: fixtureID, Gen: 2, FromGen: 1, FromSeq: 3, FromSum: 0x113f6441, CompactedEnd: 461}); err != nil || h != want {
		t.Errorf("header-compacted.hex reads as %+v, %v, where it holds %+v", h, err, want)
	}
	if m, ok := DecodeMarker(readFixture(t, "marker.hex"), fixtureID, 1); !ok || m != (Marker{Seq: 1, Sum: 0x1ed0ba7e}) {
		t.Errorf("marker.hex reads as %+v, %v", m, ok)
	}
}

// rewriteFile reads a file whose every batch is marked, as a fixture's is,
// with the codec, checks each batch's changes against fixtures_test.go's
// reader, and writes the file out again with the codec.
func rewriteFile(t *testing.T, name string, b []byte) []byte {
	t.Helper()
	ref, err := readFile(b)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	h, err := DecodeHeader(b, int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out, err := AppendHeader(nil, h)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	off := HeaderSize
	for seq := uint64(1); off < len(b); seq++ {
		bt, err := DecodeBatch(b[off:], h.Gen, seq)
		if err != nil {
			t.Fatalf("%s, the batch at offset %d: %v", name, off, err)
		}
		if int(seq) > len(ref.batches) || !sameChanges(bt.Changes, toChanges(ref.batches[seq-1].changes)) {
			t.Errorf("%s: batch %d reads differently from fixtures_test.go's reader", name, seq)
		}
		var s uint32
		if out, s, err = AppendBatch(out, h.Gen, seq, bt.Changes); err != nil || s != bt.Sum {
			t.Fatalf("%s, batch %d: AppendBatch gives the checksum %#08x and %v, where the file holds %#08x", name, seq, s, err, bt.Sum)
		}
		off += bt.Length
		m, ok := DecodeMarker(b[off:], h.ID, h.Gen)
		if !ok || m != (Marker{Seq: seq, Sum: bt.Sum}) {
			t.Fatalf("%s: the marker at offset %d reads as %+v, %v, after batch %d, %#08x", name, off, m, ok, seq, bt.Sum)
		}
		out = AppendMarker(out, h.ID, h.Gen, m)
		off += MarkerSize
	}
	return out
}

// rawHeader writes a header with any fields at all, and the checksum that
// fits them.
func rawHeader(magic string, version uint32, h Header) []byte {
	b := []byte(magic)
	b = binary.LittleEndian.AppendUint32(b, version)
	b = append(b, h.ID[:]...)
	b = binary.LittleEndian.AppendUint64(b, h.Gen)
	b = binary.LittleEndian.AppendUint64(b, h.FromGen)
	b = binary.LittleEndian.AppendUint64(b, h.FromSeq)
	b = binary.LittleEndian.AppendUint32(b, h.FromSum)
	b = binary.LittleEndian.AppendUint64(b, h.CompactedEnd)
	return binary.LittleEndian.AppendUint32(b, sum(b))
}

// TestHeaderChecks makes FORMAT.md's checks of a header fail one at a
// time, and some of them together, where the first in FORMAT.md's order
// decides the error.
func TestHeaderChecks(t *testing.T) {
	comp := Header{ID: fixtureID, Gen: 2, FromGen: 1, FromSeq: 3, FromSum: 0x113f6441, CompactedEnd: 461}
	with := func(f func(*Header)) []byte {
		h := comp
		f(&h)
		return rawHeader(magicFile, Version, h)
	}
	good := rawHeader(magicFile, Version, comp)
	sqlite := append([]byte("SQLite format 3\x00"), make([]byte, 84)...)
	for _, c := range []struct {
		what string
		b    []byte
		size int64
		want error // nil when the header is good
	}{
		{"an empty file", nil, 0, errs.ErrNotDatabase},
		{"67 bytes", good[:67], 67, errs.ErrNotDatabase},
		{"SQLite's header", sqlite, 4096, errs.ErrZeroX},
		{"SQLite's header in a file too short for a header", sqlite[:60], 60, errs.ErrNotDatabase},
		{"zeros", make([]byte, HeaderSize), 4096, errs.ErrNotDatabase},
		{"the magic number with its CR LF made LF", patch(good, 4, []byte("\n\x1a\n\x00")), 4096, errs.ErrNotDatabase},
		{"the magic number cut at its Ctrl-Z", patch(good, 6, []byte{0, 0}), 4096, errs.ErrNotDatabase},
		{"a batch's magic number", patch(good, 0, []byte(magicBatch)), 4096, errs.ErrNotDatabase},
		{"version 2", rawHeader(magicFile, 2, comp), 4096, errs.ErrFormatVersion},
		{"version 0", rawHeader(magicFile, 0, comp), 4096, errs.ErrFormatVersion},
		{"version 2 and a checksum that fails", patch(rawHeader(magicFile, 2, comp), 64, []byte{0}), 4096, errs.ErrFormatVersion},
		{"a checksum that fails", patch(good, 64, []byte{0x7d}), 4096, errs.ErrDamaged},
		{"generation 0", with(func(h *Header) { *h = Header{ID: fixtureID} }), 4096, errs.ErrDamaged},
		{"generation 1 continuing from generation 1", rawHeader(magicFile, Version, Header{Gen: 1, FromGen: 1}), 4096, errs.ErrDamaged},
		{"generation 1 continuing from a batch", rawHeader(magicFile, Version, Header{Gen: 1, FromSeq: 1}), 4096, errs.ErrDamaged},
		{"generation 1 continuing from a checksum", rawHeader(magicFile, Version, Header{Gen: 1, FromSum: 1}), 4096, errs.ErrDamaged},
		{"generation 1 with a compacted part", rawHeader(magicFile, Version, Header{Gen: 1, CompactedEnd: 68}), 4096, errs.ErrDamaged},
		{"generation 2 continuing from generation 0", with(func(h *Header) { h.FromGen = 0 }), 4096, errs.ErrDamaged},
		{"generation 2 continuing from generation 2", with(func(h *Header) { h.FromGen = 2 }), 4096, errs.ErrDamaged},
		{"a compacted part ending inside the header", with(func(h *Header) { h.CompactedEnd = 67 }), 4096, errs.ErrDamaged},
		{"a compacted part ending at 0", with(func(h *Header) { h.CompactedEnd = 0 }), 4096, errs.ErrDamaged},
		{"a compacted part ending past the end of the file", good, 460, errs.ErrDamaged},
		{"a compacted part in a file of unknown size", good, -1, errs.ErrDamaged},
		{"a compacted part ending at the end of the file", good, 461, nil},
		{"a compacted part ending at the end of the header", with(func(h *Header) { h.CompactedEnd = 68 }), 68, nil},
		{"a compacted file continuing from one that held no batches", with(func(h *Header) { h.FromSeq, h.FromSum = 0, 0 }), 4096, nil},
		{"a new database", rawHeader(magicFile, Version, Header{ID: fixtureID, Gen: 1}), HeaderSize, nil},
		{"the largest generation", with(func(h *Header) { h.Gen, h.FromGen = math.MaxUint64, math.MaxUint64-1 }), 4096, nil},
		{"a header with a batch after it", append(slices.Clone(good), readFixture(t, "batch-delete.hex")...), 4096, nil},
	} {
		h, err := DecodeHeader(c.b, c.size)
		if c.want == nil {
			if err != nil {
				t.Errorf("%s: %v", c.what, err)
			} else if out, err := AppendHeader(nil, h); err != nil || !bytes.Equal(out, c.b[:HeaderSize]) {
				t.Errorf("%s comes back as % x, %v", c.what, out, err)
			}
			continue
		}
		var d *errs.Damage
		switch {
		case !errors.Is(err, c.want):
			t.Errorf("%s gives %v, where %v was wanted", c.what, err, c.want)
		case errors.As(err, &d) != (c.want == errs.ErrDamaged):
			t.Errorf("%s gives %v, which is a *errs.Damage only for damage", c.what, err)
		case d != nil && (d.Offset < 0 || d.Offset >= HeaderSize || d.Batch != 0):
			t.Errorf("%s gives damage at offset %d, batch %d", c.what, d.Offset, d.Batch)
		}
	}

	// Every change to a byte past the version is damage, found by the
	// checksum.
	for i := 12; i < HeaderSize; i++ {
		for _, flip := range []byte{0x01, 0x80, 0xff} {
			b := slices.Clone(good)
			b[i] ^= flip
			if _, err := DecodeHeader(b, 4096); !errors.Is(err, errs.ErrDamaged) {
				t.Fatalf("the header with byte %d changed by %#02x gives %v", i, flip, err)
			}
		}
	}
}

func TestAppendHeaderRefuses(t *testing.T) {
	for _, h := range []Header{
		{},
		{Gen: 1, FromGen: 1},
		{Gen: 1, FromSeq: 1},
		{Gen: 1, FromSum: 1},
		{Gen: 1, CompactedEnd: 68},
		{Gen: 2, FromGen: 2, CompactedEnd: 68},
		{Gen: 2, FromGen: 1, CompactedEnd: 67},
		{Gen: 2, FromGen: 1},
	} {
		dst := []byte("abc")
		out, err := AppendHeader(dst, h)
		if !errors.Is(err, errs.ErrInvalid) || string(out) != "abc" {
			t.Errorf("AppendHeader(%+v) gives % x, %v", h, out, err)
		}
	}
	out, err := AppendHeader([]byte("abc"), Header{ID: fixtureID, Gen: 1})
	if err != nil || !bytes.Equal(out[3:], readFixture(t, "header-new.hex")) || string(out[:3]) != "abc" {
		t.Errorf("AppendHeader after 3 bytes gives % x, %v", out, err)
	}
}

func TestMarkers(t *testing.T) {
	m := readFixture(t, "marker.hex")
	want := Marker{Seq: 1, Sum: 0x1ed0ba7e}
	if got, ok := DecodeMarker(append(slices.Clone(m), "and more"...), fixtureID, 1); !ok || got != want {
		t.Errorf("marker.hex with bytes after it reads as %+v, %v", got, ok)
	}
	for _, c := range []struct {
		what string
		b    []byte
		id   [16]byte
		gen  uint64
	}{
		{"another database's", m, [16]byte{1}, 1},
		{"a database with a zero ID", m, [16]byte{}, 1},
		{"a later generation's", m, fixtureID, 2},
		{"generation 0's", m, fixtureID, 0},
		{"19 bytes", m[:19], fixtureID, 1},
		{"nothing", nil, fixtureID, 1},
		{"zeros", make([]byte, MarkerSize), fixtureID, 1},
		{"a batch's magic number", patch(m, 0, []byte(magicBatch)), fixtureID, 1},
	} {
		if got, ok := DecodeMarker(c.b, c.id, c.gen); ok {
			t.Errorf("%s passes as a whole marker, %+v", c.what, got)
		}
	}
	// A change to any bit fails the check value, so a marker that a crash
	// left half written never counts.
	for i := range m {
		for bit := 0; bit < 8; bit++ {
			b := slices.Clone(m)
			b[i] ^= 1 << bit
			if got, ok := DecodeMarker(b, fixtureID, 1); ok {
				t.Fatalf("the marker with bit %d of byte %d changed passes, as %+v", bit, i, got)
			}
		}
	}
	for _, c := range []struct {
		id  [16]byte
		gen uint64
		m   Marker
	}{
		{fixtureID, 1, Marker{}},
		{[16]byte{0xff, 1, 2}, math.MaxUint64, Marker{Seq: math.MaxUint64, Sum: math.MaxUint32}},
		{[16]byte{}, 7, Marker{Seq: 258, Sum: 0x01020304}},
	} {
		b := AppendMarker([]byte("abc"), c.id, c.gen, c.m)
		if got, ok := DecodeMarker(b[3:], c.id, c.gen); !ok || got != c.m || len(b) != 3+MarkerSize || string(b[:3]) != "abc" {
			t.Errorf("the marker %+v comes back as %+v, %v, in % x", c.m, got, ok, b)
		}
	}
}

// TestFindMarker: FindMarker finds the first whole marker at any offset. It
// passes over magic numbers whose check values don't fit, and markers of
// another database or generation, and leaves a marker that runs past the
// end of the bytes for the next piece. On bytes full of magic numbers, with
// markers dropped in at random, it agrees with DecodeMarker tried at every
// offset.
func TestFindMarker(t *testing.T) {
	m := AppendMarker(nil, fixtureID, 1, Marker{Seq: 7, Sum: 0xabcdef01})
	bad := patch(m, 19, []byte{m[19] ^ 1})
	other := AppendMarker(nil, [16]byte{1}, 1, Marker{Seq: 8, Sum: 2})
	later := AppendMarker(nil, fixtureID, 2, Marker{Seq: 9, Sum: 3})
	magics := strings.Repeat(magicMarker, 100)
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	for _, c := range []struct {
		what string
		b    []byte
		at   int
	}{
		{"nothing", nil, -1},
		{"a marker alone", m, 0},
		{"a marker after zeros", join(make([]byte, 1000), m), 1000},
		{"a marker and more after it", join(m, m, []byte("more")), 0},
		{"a marker one byte short", m[:MarkerSize-1], -1},
		{"a marker that runs past the end", join(make([]byte, 50), m[:12]), -1},
		{"magic numbers and no marker", []byte(magics), -1},
		{"a marker after magic numbers", join([]byte(magics+"HCR"), m), len(magics) + 3},
		{"a marker that starts inside a magic number", join([]byte("HC"), m), 2},
		{"a check value that doesn't fit, then a marker", join(bad, []byte("x"), m), MarkerSize + 1},
		{"another database's marker, then this one's", join(other, m), MarkerSize},
		{"a later generation's marker", later, -1},
	} {
		at, got := FindMarker(c.b, fixtureID, 1)
		if at != c.at || (at >= 0 && got != (Marker{Seq: 7, Sum: 0xabcdef01})) {
			t.Errorf("%s: FindMarker gives %d, %+v, where %d is wanted", c.what, at, got, c.at)
		}
	}
	r := rand.New(rand.NewPCG(4, 7))
	found := 0
	for range 2000 {
		b := make([]byte, r.IntN(400))
		for i := range b {
			b[i] = "HCRM\x00"[r.IntN(5)]
		}
		for range r.IntN(3) {
			if len(b) >= MarkerSize {
				copy(b[r.IntN(len(b)-MarkerSize+1):], m)
			}
		}
		want := -1
		for i := range b {
			if _, ok := DecodeMarker(b[i:], fixtureID, 1); ok {
				want = i
				break
			}
		}
		if at, _ := FindMarker(b, fixtureID, 1); at != want {
			t.Fatalf("FindMarker gives %d, where DecodeMarker finds the first whole marker at %d, in % x", at, want, b)
		}
		if want >= 0 {
			found++
		}
	}
	if found < 500 {
		t.Errorf("only %d of the random cases held a whole marker", found)
	}
}

// mustNotCount checks that DecodeBatch finds no batch that counts in b,
// and doesn't take it for damage.
func mustNotCount(t *testing.T, what string, b []byte, gen, seq uint64) {
	t.Helper()
	bt, err := DecodeBatch(b, gen, seq)
	if !errors.Is(err, ErrDoesNotCount) || errors.Is(err, errs.ErrDamaged) {
		t.Fatalf("%s gives %+v, %v, where it doesn't count", what, bt, err)
	}
}

// TestWhatMakesABatchCount breaks each of FORMAT.md's five checks for a
// batch that counts, which has to give ErrDoesNotCount and never damage.
func TestWhatMakesABatchCount(t *testing.T) {
	b := readFixture(t, "batch-several.hex") // generation 1, sequence number 1
	if _, err := DecodeBatch(b, 1, 1); err != nil {
		t.Fatal(err)
	}
	mustNotCount(t, "nothing", nil, 1, 1)
	mustNotCount(t, "zeros", make([]byte, 64), 1, 1)
	mustNotCount(t, "a marker", readFixture(t, "marker.hex"), 1, 1)
	mustNotCount(t, "a header", readFixture(t, "header-new.hex"), 1, 1)
	mustNotCount(t, "another magic number", patch(b, 3, []byte("X")), 1, 1)
	mustNotCount(t, "a batch of generation 1 in a file of generation 2", b, 2, 1)
	mustNotCount(t, "sequence number 1 where 2 comes next", b, 1, 2)
	mustNotCount(t, "sequence number 1 where 0 comes next", b, 1, 0)
	mustNotCount(t, "a length past the end", patch(b, 12, u64(uint64(len(b)+1))), 1, 1)
	mustNotCount(t, "the largest length", patch(b, 12, u64(math.MaxUint64)), 1, 1)
	mustNotCount(t, "the checksum of a batch with another sequence number", patch(b, 20, u64(2)), 1, 2)
	// The same, with the checksums made to fit, so the generation and the
	// sequence number alone fail.
	resealed := func(at int, p []byte) []byte {
		c := patch(b, at, p)
		return patch(c, len(c)-4, u32(sum(c[:len(c)-4])))
	}
	mustNotCount(t, "a batch of generation 2 in a file of generation 1", resealed(4, u64(2)), 1, 1)
	mustNotCount(t, "a batch of generation 0 in a file of generation 1", resealed(4, u64(0)), 1, 1)
	mustNotCount(t, "sequence number 2 where 1 comes next", resealed(20, u64(2)), 1, 1)
	mustNotCount(t, "sequence number 0 where 1 comes next", resealed(20, u64(0)), 1, 1)
	// Asked for 0, which a log never asks for: no batch is numbered 0, and
	// AppendBatch refuses to write one. The fuzz found the decoder taking it.
	mustNotCount(t, "sequence number 0, asked for", resealed(20, u64(0)), 1, 0)
	mustNotCount(t, "generation 0, asked for", resealed(4, u64(0)), 0, 1)
	// A length under 36 with a checksum that fits it: a head and a checksum
	// with no room for a change.
	for n := 32; n < MinBatchSize; n++ {
		short := patch(b[:n], 12, u64(uint64(n)))
		short = patch(short, n-4, u32(sum(short[:n-4])))
		mustNotCount(t, fmt.Sprintf("a batch of %d bytes", n), short, 1, 1)
	}
	// Cut short anywhere: a commit that a crash stopped.
	for n := range b {
		mustNotCount(t, "the batch cut short", b[:n], 1, 1)
	}
	// Any one bit changed fails one of the checks, the checksum at least.
	for i := range b {
		for bit := 0; bit < 8; bit++ {
			c := slices.Clone(b)
			c[i] ^= 1 << bit
			mustNotCount(t, "the batch with a bit changed", c, 1, 1)
		}
	}
	// What follows a batch is left alone.
	bt, err := DecodeBatch(append(slices.Clone(b), readFixture(t, "marker.hex")...), 1, 1)
	if err != nil || bt.Length != len(b) || bt.Seq != 1 || bt.Sum != 0x1ed0ba7e || len(bt.Changes) != 4 {
		t.Errorf("batch-several.hex with its marker after it reads as %+v, %v", bt, err)
	}
}

// TestDecodedChangesOwnTheirSlices checks what DecodeBatch promises about
// memory: the changes don't share the caller's bytes, and each change's
// slices are its own, so appending to one never reaches another.
func TestDecodedChangesOwnTheirSlices(t *testing.T) {
	cs := []Change{
		{Op: CreateTable, Table: "a", Names: []string{"x", "y"}},
		{Op: CreateTable, Table: "b", Names: []string{"z"}},
		{Op: Put, Key: "a:1", Fields: []Field{{"x", val.Int(1)}, {"y", val.Text("why")}}},
		{Op: Put, Key: "a:2", Fields: []Field{{"x", val.Int(2)}}},
	}
	b, _, err := AppendBatch(nil, 1, 1, cs)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := DecodeBatch(b, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	clear(b)
	bt.Changes[0].Names = append(bt.Changes[0].Names, "w")
	bt.Changes[2].Fields = append(bt.Changes[2].Fields, Field{"z", val.Int(9)})
	bt.Changes[0].Names, bt.Changes[2].Fields = bt.Changes[0].Names[:2], bt.Changes[2].Fields[:2]
	if !sameChanges(bt.Changes, cs) {
		t.Errorf("after the bytes were cleared and two changes appended to, the changes hold %v", bt.Changes)
	}
}

func TestBatchLength(t *testing.T) {
	b := readFixture(t, "batch-several.hex")
	if n, err := BatchLength(b[:BatchHeadSize], 1, 1, int64(len(b))); err != nil || n != int64(len(b)) {
		t.Errorf("BatchLength of batch-several.hex's head gives %d, %v", n, err)
	}
	for _, c := range []struct {
		what     string
		head     []byte
		gen, seq uint64
		room     int64
	}{
		{"a head that's cut short", b[:BatchHeadSize-1], 1, 1, int64(len(b))},
		{"a head in a file that ends inside it", b, 1, 1, BatchHeadSize - 1},
		{"a batch that ends past the end of the file", b, 1, 1, int64(len(b) - 1)},
		{"no room at all", b, 1, 1, -1},
		{"another generation", b, 3, 1, int64(len(b))},
		{"another sequence number", b, 1, 9, int64(len(b))},
		{"another magic number", patch(b, 0, []byte(magicMarker)), 1, 1, int64(len(b))},
		{"a length of 35", patch(b, 12, u64(35)), 1, 1, int64(len(b))},
	} {
		if n, err := BatchLength(c.head, c.gen, c.seq, c.room); !errors.Is(err, ErrDoesNotCount) {
			t.Errorf("BatchLength of %s gives %d, %v", c.what, n, err)
		}
	}
}

// plain is a change that comes before each bad one in the tests below, so
// they show that an error names the right change and its offset.
var plain = Change{Op: Delete, Key: "docs:1"}

func vector(f ...float32) val.Value { return val.Vector(f) }

var (
	negZero32 = float32(math.Copysign(0, -1))
	nan32     = float32(math.NaN())
	inf32     = float32(math.Inf(1))
)

// badChanges break FORMAT.md's rules for a change on its own, one rule
// each, and can still be written in the format's syntax.
func badChanges() []struct {
	what string
	c    Change
} {
	many := func(clashing ...string) []string {
		var names []string
		for i := 0; i < 20; i++ {
			names = append(names, "f"+string(rune('a'+i)))
		}
		return append(names, clashing...)
	}
	fields := func(names ...string) []Field {
		var fs []Field
		for _, n := range names {
			fs = append(fs, Field{Name: n, Value: val.Int(1)})
		}
		return fs
	}
	put := func(fs ...Field) Change { return Change{Op: Put, Key: "docs:1", Fields: fs} }
	field := func(name string, v val.Value) Field { return Field{Name: name, Value: v} }
	return []struct {
		what string
		c    Change
	}{
		{"an empty table name", Change{Op: Drop}},
		{"a table name of 64 bytes", Change{Op: Drop, Table: strings.Repeat("a", 64)}},
		{"a capital in a table name", Change{Op: Drop, Table: "Docs"}},
		{"a table name starting with a digit", Change{Op: Drop, Table: "1docs"}},
		{"a table name starting with an underscore", Change{Op: Drop, Table: "_docs"}},
		{"a table name with HyperCrux's prefix", Change{Op: Drop, Table: "hc_docs"}},
		{"a table name with SQLite's prefix", Change{Op: Drop, Table: "sqlite_docs"}},
		{"a hyphen in a table name", Change{Op: CreateTable, Table: "do-cs"}},
		{"a table name beyond ASCII", Change{Op: CreateTable, Table: "döcs"}},
		{"a zero byte in a table name", Change{Op: CreateTable, Table: "docs\x00"}},

		{"a key without a colon", Change{Op: Delete, Key: "docs"}},
		{"a key without a table", Change{Op: Delete, Key: ":7"}},
		{"a key with nothing after its colon", Change{Op: Delete, Key: "docs:"}},
		{"a key of 2 bytes", Change{Op: Put, Key: "a:"}},
		{"a key whose table breaks the rules", Change{Op: Put, Key: "Docs:7"}},
		{"a key in a table with HyperCrux's prefix", Change{Op: Delete, Key: "hc_meta:7"}},
		{"a key of 1,025 bytes", Change{Op: Delete, Key: "docs:" + strings.Repeat("x", 1020)}},
		{"a key that isn't UTF-8", Change{Op: Delete, Key: "docs:\xff"}},
		{"a key cut inside a letter", Change{Op: Delete, Key: "docs:\xc3"}},
		{"a key with a zero byte", Change{Op: Delete, Key: "docs:a\x00b"}},

		{"an empty field name", Change{Op: CreateTable, Table: "docs", Names: []string{""}}},
		{"a field name of 65 bytes", Change{Op: CreateTable, Table: "docs", Names: []string{strings.Repeat("f", 65)}}},
		{"a field name starting with a digit", Change{Op: CreateTable, Table: "docs", Names: []string{"1x"}}},
		{"a hyphen in a field name", Change{Op: CreateTable, Table: "docs", Names: []string{"a-b"}}},
		{"a space in a field name", Change{Op: CreateTable, Table: "docs", Names: []string{"a b"}}},
		{"a field name beyond ASCII", Change{Op: CreateTable, Table: "docs", Names: []string{"é"}}},
		{"the field name key", Change{Op: CreateTable, Table: "docs", Names: []string{"key"}}},
		{"the field name KEY", Change{Op: CreateTable, Table: "docs", Names: []string{"KEY"}}},
		{"the field name Rowid", put(fields("Rowid")...)},
		{"the field name oid", put(fields("oid")...)},
		{"the field name _ROWID_", put(fields("_ROWID_")...)},

		{"an empty link type", Change{Op: Link, Key: "docs:1", To: "docs:2"}},
		{"a link type of 201 code points", Change{Op: Link, Key: "docs:1", Type: strings.Repeat("é", 201), To: "docs:2"}},
		{"a link type that isn't UTF-8", Change{Op: Unlink, Key: "docs:1", Type: "cites\xff", To: "docs:2"}},
		{"a link type starting with a zero byte", Change{Op: Link, Key: "docs:1", Type: "\x00cites", To: "docs:2"}},
		{"a link from a key that breaks the rules", Change{Op: Link, Key: "docs", Type: "cites", To: "docs:2"}},
		{"a link to a key that breaks the rules", Change{Op: Unlink, Key: "docs:1", Type: "cites", To: "docs"}},

		{"a vector size of 65,537", Change{Op: CreateTable, Table: "docs", Size: 65537, Names: []string{"vec"}}},
		{"the largest vector size the syntax holds", Change{Op: CreateTable, Table: "docs", Size: math.MaxUint32, Names: []string{"vec"}}},
		{"a vector size and no vector field", Change{Op: CreateTable, Table: "docs", Size: 3, Names: []string{"title"}}},
		{"a vector size and no fields", Change{Op: CreateTable, Table: "docs", Size: 3}},
		{"two fields that match regardless of case", Change{Op: CreateTable, Table: "docs", Names: []string{"Title", "title"}}},
		{"the same field twice", Change{Op: CreateTable, Table: "docs", Names: []string{"a", "a"}}},
		{"two vector fields", Change{Op: CreateTable, Table: "docs", Size: 2, Names: []string{"vec", "VEC"}}},
		{"two of many fields that match regardless of case", Change{Op: CreateTable, Table: "docs", Names: many("Fb")}},
		{"one of many fields twice", Change{Op: CreateTable, Table: "docs", Names: many("fc")}},

		{"fields out of byte order", put(fields("b", "a")...)},
		{"a capital after a small letter", put(fields("a", "B")...)},
		{"the same field twice in a put", put(fields("a", "a")...)},
		{"fields apart that match regardless of case", put(fields("A_x", "B", "a_x")...)},
		{"two of many fields that match regardless of case in a put", put(fields(slices.Sorted(slices.Values(many("FC")))...)...)},
		{"a vector in a field that isn't the vector field", put(field("title", vector(1)))},
		{"a vector in a field called vec_", put(field("vec_", vector(1)))},
		{"text in the vector field", put(field("vec", val.Text("[1]")))},
		{"a whole number in the vector field, spelt VEC", put(field("VEC", val.Int(1)))},
		{"bytes in the vector field, spelt Vec", put(field("Vec", val.Bytes("\x00\x00\x80\x3f")))},
		{"a real number in the vector field", put(field("vec", val.Real(1)))},
		{"NaN", put(field("x", val.Real(math.NaN())))},
		{"infinity", put(field("x", val.Real(math.Inf(1))))},
		{"minus infinity", put(field("x", val.Real(math.Inf(-1))))},
		{"text that isn't UTF-8", put(field("x", val.Text("\xff")))},
		{"text holding half a letter", put(field("x", val.Text("na\xc3")))},
		{"a vector of no values", put(field("vec", val.Vector(nil)))},
		{"a vector of 65,537 values", put(field("vec", val.Vector(slices.Repeat([]float32{1}, 65537))))},
		{"a vector of zeros", put(field("vec", vector(0, 0)))},
		{"a vector of zeros, one of them -0", put(field("vec", vector(0, negZero32)))},
		{"a vector of -0", put(field("vec", vector(negZero32)))},
		{"a vector holding NaN", put(field("vec", vector(1, nan32)))},
		{"a vector holding infinity", put(field("vec", vector(inf32)))},
		{"a vector holding minus infinity", put(field("vec", vector(1, 2, -inf32)))},
	}
}

// TestDamageInABatchThatCounts writes changes that break the rules in
// batches that count. Each is damage, which DecodeBatch reports as a
// *errs.Damage naming the change, and AppendBatch refuses to write it, with
// the same words.
func TestDamageInABatchThatCounts(t *testing.T) {
	const seq = 7
	first := appendChange(nil, &plain)
	for _, c := range badChanges() {
		b := seal(1, seq, appendChange(slices.Clone(first), &c.c))
		_, err := DecodeBatch(b, 1, seq)
		var d *errs.Damage
		if !errors.As(err, &d) || errors.Is(err, ErrDoesNotCount) {
			t.Errorf("%s: DecodeBatch gives %v, where it's damage", c.what, err)
			continue
		}
		if d.Batch != seq || d.Offset != int64(BatchHeadSize+len(first)) || !strings.HasPrefix(d.Reason, "change 2: ") {
			t.Errorf("%s: the damage is %+v, where it's change 2, at offset %d of batch %d", c.what, d, BatchHeadSize+len(first), seq)
		}
		if _, err := readBatch(b, 1); err == nil {
			t.Errorf("%s: fixtures_test.go's reader reads it", c.what)
		}
		dst := []byte("abc")
		out, _, err := AppendBatch(dst, 1, seq, []Change{plain, c.c})
		why := strings.TrimPrefix(d.Reason, "change 2: ")
		if !errors.Is(err, errs.ErrInvalid) || err.Error() != "hypercrux: invalid: change 2 of the batch: "+why || string(out) != "abc" {
			t.Errorf("%s: AppendBatch gives %q, %v, where DecodeBatch says %q", c.what, out, err, why)
		}
	}
}

// TestBrokenSyntaxIsDamage writes changes the syntax can't read, in
// batches that count, which is damage too.
func TestBrokenSyntaxIsDamage(t *testing.T) {
	put := func(rest ...[]byte) []byte {
		b := appendString([]byte{'P'}, "docs:1")
		b = binary.LittleEndian.AppendUint16(b, 1)
		b = appendString(b, "x")
		return append(b, bytes.Join(rest, nil)...)
	}
	several := readFixture(t, "batch-several.hex")
	several = several[BatchHeadSize : len(several)-4]
	for _, c := range []struct {
		what    string
		changes []byte
	}{
		{"a change of unknown kind", []byte("Zdocs")},
		{"a change of kind 0", []byte{0, 0, 0, 0}},
		{"a change in lower case", appendString([]byte{'x'}, "docs")},
		{"a change cut short", appendString([]byte{'D'}, "docs:7")[:8]},
		{"a whole change and three bytes", append(appendChange(nil, &plain), 'X', 1, 0)},
		{"a whole change and a zero", append(appendChange(nil, &plain), 0, 0, 0, 0)},
		{"a create change with more fields than it holds", append(appendString([]byte{'T'}, "docs"), 0, 0, 0, 0, 2, 0, 1, 0, 'a')},
		{"a put with more fields than it holds", append(appendString([]byte{'P'}, "docs:1"), 3, 0)},
		{"a put cut inside its field count", append(appendString([]byte{'P'}, "docs:1"), 3)},
		{"a value of unknown kind", put([]byte{'q'})},
		{"a value of kind 0", put([]byte{0})},
		{"a value of kind T", put([]byte{'T'})},
		{"a value cut short", put([]byte{'i', 1, 2, 3})},
		{"a missing value", put()},
		{"text running past the checksum", put([]byte{'t'}, u32(5), []byte("abc"))},
		{"text of the largest length", put([]byte{'t'}, u32(math.MaxUint32), []byte("abc"))},
		{"bytes running past the checksum", put([]byte{'b'}, u32(1))},
		{"a vector running past the checksum", put([]byte{'v'}, u32(2), u32(0x3f800000))},
		{"a vector of the largest count", put([]byte{'v'}, u32(math.MaxUint32), u32(0x3f800000))},
		{"a link cut before its type", appendString([]byte{'L'}, "docs:1")},
		{"a link cut before the key it's to", appendString(appendString([]byte{'U'}, "docs:1"), "cites")},
		{"a string longer than the batch", append([]byte{'X', 0xff, 0xff}, "docs"...)},
		{"batch-several.hex's changes cut short", several[:len(several)-1]},
	} {
		b := seal(1, 1, c.changes)
		_, err := DecodeBatch(b, 1, 1)
		var d *errs.Damage
		if !errors.As(err, &d) || errors.Is(err, ErrDoesNotCount) || d.Batch != 1 || d.Offset < BatchHeadSize || d.Offset > int64(len(b)-4) {
			t.Errorf("%s: DecodeBatch gives %v, where it's damage inside the batch", c.what, err)
		}
		if _, err := readBatch(b, 1); err == nil {
			t.Errorf("%s: fixtures_test.go's reader reads it", c.what)
		}
	}
}

// TestTheEncoderRefuses checks what AppendBatch refuses beyond the rules
// that DecodeBatch checks too: the things the syntax can't write.
func TestTheEncoderRefuses(t *testing.T) {
	many := make([]string, maxCount+1)
	fields := make([]Field, maxCount+1)
	for i := range many {
		many[i] = fmt.Sprintf("f%05d", i)
		fields[i] = Field{Name: many[i]}
	}
	for _, c := range []struct {
		what     string
		gen, seq uint64
		changes  []Change
	}{
		{"generation 0", 0, 1, []Change{plain}},
		{"sequence number 0", 1, 0, []Change{plain}},
		{"no changes", 1, 1, nil},
		{"an empty change list", 1, 1, []Change{}},
		{"a change of kind 0", 1, 1, []Change{{}}},
		{"a change of kind 7", 1, 1, []Change{{Op: Drop + 1, Table: "docs"}}},
		{"a delete with a table", 1, 1, []Change{{Op: Delete, Key: "docs:1", Table: "docs"}}},
		{"a put with a vector size", 1, 1, []Change{{Op: Put, Key: "docs:1", Size: 3}}},
		{"a put with field names", 1, 1, []Change{{Op: Put, Key: "docs:1", Names: []string{"a"}}}},
		{"a drop with a key", 1, 1, []Change{{Op: Drop, Table: "docs", Key: "docs:1"}}},
		{"a create change with a link's target", 1, 1, []Change{{Op: CreateTable, Table: "docs", To: "docs:2"}}},
		{"a link with fields", 1, 1, []Change{{Op: Link, Key: "docs:1", Type: "t", To: "docs:2", Fields: []Field{{Name: "a"}}}}},
		{"a delete with a link type", 1, 1, []Change{{Op: Delete, Key: "docs:1", Type: "t"}}},
		{"a negative vector size", 1, 1, []Change{{Op: CreateTable, Table: "docs", Size: -1, Names: []string{"vec"}}}},
		{"a create change with 65,536 fields", 1, 1, []Change{{Op: CreateTable, Table: "docs", Names: many}}},
		{"a put with 65,536 fields", 1, 1, []Change{{Op: Put, Key: "docs:1", Fields: fields}}},
	} {
		out, s, err := AppendBatch([]byte("abc"), c.gen, c.seq, c.changes)
		if !errors.Is(err, errs.ErrInvalid) || string(out) != "abc" || s != 0 {
			t.Errorf("%s: AppendBatch gives % x, %#08x, %v", c.what, out, s, err)
		}
	}
}

// TestLimitsThatAreAllowed writes changes at the edges of the rules, which
// have to come back exactly, and fixtures_test.go's reader has to read the
// same.
func TestLimitsThatAreAllowed(t *testing.T) {
	long := strings.Repeat("𝄞", maxTypeRunes) // 800 bytes
	wide := make([]string, maxCount)
	wideFields := make([]Field, maxCount)
	for i := range wide {
		wide[i] = fmt.Sprintf("f%05d", i)
		wideFields[i] = Field{Name: wide[i], Value: val.Int(int64(i))}
	}
	dims := make([]float32, maxVecDims)
	for i := range dims {
		dims[i] = float32(i)
	}
	sub := math.Float32frombits(1) // the smallest subnormal float32
	batches := [][]Change{
		{
			{Op: CreateTable, Table: strings.Repeat("t", maxTableName)},
			{Op: CreateTable, Table: "a"},
			{Op: CreateTable, Table: "hc"},
			{Op: CreateTable, Table: "hcx_1"},
			{Op: CreateTable, Table: "sqlite"},
			{Op: CreateTable, Table: "sqlitex_"},
			{Op: CreateTable, Table: "a_", Size: maxVecDims, Names: []string{"vec"}},
			{Op: CreateTable, Table: "b", Size: 1, Names: []string{"Title", "VEC", "_", "_x", "A", "Z9", strings.Repeat("F", maxFieldName)}},
			{Op: CreateTable, Table: "c", Names: []string{"vec", "keys", "key_", "_key", "rowid2", "oid_", "_rowid", "vec_"}},
			{Op: Drop, Table: "z9_"},
		},
		{
			{Op: Put, Key: "a:b"},
			{Op: Put, Key: "docs:" + strings.Repeat("k", maxKey-5)},
			{Op: Put, Key: strings.Repeat("t", maxTableName) + ":" + strings.Repeat("é", 480)},
			{Op: Delete, Key: "docs:a:b"},
			{Op: Delete, Key: "docs:é☃𝄞\x7f\U0010ffff"},
			{Op: Link, Key: "docs:1", Type: "x", To: "docs:1"},
			{Op: Link, Key: "docs:1", Type: long, To: "docs:2"},
			{Op: Unlink, Key: "docs:1", Type: "a\x00b", To: "docs:2"},
			{Op: Unlink, Key: "docs:1", Type: strings.Repeat("a", maxTypeRunes), To: "docs:2"},
		},
		{
			{Op: Put, Key: "docs:1", Fields: []Field{
				{"A", val.Real(math.Float64frombits(0x7fefffffffffffff))}, // the largest float64
				{"Vec", vector(sub)},
				{"Z", val.Real(math.Float64frombits(0xffefffffffffffff))},
				{"_", val.Text("")},
				{"b", val.Bytes("")},
				{"big", val.Int(math.MaxInt64)},
				{"neg_zero", val.Real(math.Copysign(0, -1))},
				{"normal", val.Real(math.Float64frombits(0x0010000000000000))}, // the smallest normal
				{"null", val.Null()},
				{"small", val.Int(math.MinInt64)},
				{"sub", val.Real(math.Float64frombits(0x000fffffffffffff))}, // the largest subnormal
				{"text", val.Text("nul\x00 and naïve ☃ \U0010ffff")},
				{"tiny", val.Real(math.Float64frombits(1))},
				{"vec_", val.Text("not the vector field")},
				{"zero", val.Real(0)},
			}},
			{Op: Put, Key: "docs:2", Fields: []Field{{"long_bytes", val.Bytes(strings.Repeat("\xff\x00", 40000))}, {"long_text", val.Text(strings.Repeat("é", 40000))}}},
			{Op: Put, Key: "docs:3", Fields: []Field{{"vec", vector(negZero32, sub, math.Float32frombits(0x807fffff), math.MaxFloat32, -math.MaxFloat32)}}},
			{Op: Put, Key: "docs:4", Fields: []Field{{"vec", val.Null()}}},
			{Op: Put, Key: "docs:5", Fields: []Field{{"vEC", val.Vector(dims)}}},
		},
		{{Op: CreateTable, Table: "wide", Names: wide}},
		{{Op: Put, Key: "wide:1", Fields: wideFields}},
	}
	for i, cs := range batches {
		b, s, err := AppendBatch(nil, 1, uint64(i+1), cs)
		if err != nil {
			t.Fatalf("batch %d: %v", i+1, err)
		}
		bt, err := DecodeBatch(b, 1, uint64(i+1))
		if err != nil || !sameChanges(bt.Changes, cs) || bt.Sum != s || bt.Length != len(b) {
			t.Fatalf("batch %d comes back as %v, %v", i+1, bt.Changes, err)
		}
		ref, err := readBatch(b, 1)
		if err != nil || !sameChanges(toChanges(ref.changes), cs) {
			t.Fatalf("batch %d: fixtures_test.go's reader gives %v", i+1, err)
		}
	}
}
