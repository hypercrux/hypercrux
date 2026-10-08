// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
)

// The decoders' fuzz targets, seeded from the fixtures. Each checks the
// bytes as they come, then again with their checksum made to fit, so the
// fuzzer reaches past the checksum to what it guards. The codec has to
// agree with fixtures_test.go's reader on every input, keep the line
// between a batch that doesn't count and damage, and encode what it reads
// back to the same bytes.

// FuzzDecodeBatch is the main target: DecodeBatch on any bytes.
func FuzzDecodeBatch(f *testing.F) {
	for _, name := range fixtures {
		b := readFixture(f, name)
		switch name[:5] {
		case "batch":
			f.Add(b)
		case "file-":
			// Each batch on its own, and with the rest of the file after it.
			ff, err := readFile(b)
			if err != nil {
				f.Fatalf("%s: %v", name, err)
			}
			off := headerSize
			for i, bt := range ff.batches {
				f.Add(b[off : off+bt.length])
				f.Add(b[off:])
				off = ff.ends[i]
			}
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		checkBatch(t, b)
		if len(b) >= BatchHeadSize+4 {
			c := slices.Clone(b)
			binary.LittleEndian.PutUint64(c[12:], uint64(len(c)))
			binary.LittleEndian.PutUint32(c[len(c)-4:], sum(c[:len(c)-4]))
			checkBatch(t, c)
		}
	})
}

// checkBatch decodes b as the batch the file expects next, with the
// generation and sequence number b holds, so those two checks pass and
// TestWhatMakesABatchCount is left to try them.
func checkBatch(t *testing.T, b []byte) {
	gen, seq := uint64(1), uint64(1)
	if len(b) >= BatchHeadSize {
		gen, seq = le64(b[4:]), le64(b[20:])
	}
	bt, err := DecodeBatch(b, gen, seq)
	ref, refErr := readBatch(b, gen) // which doesn't look at the sequence number

	// Whether the batch counts, by FORMAT.md's five checks. The generation
	// and the sequence number asked for are the batch's own, and FORMAT.md
	// numbers both from 1, so a batch with either at 0 never counts.
	zero := gen == 0 || seq == 0
	counts := !zero && len(b) >= batchHead && bytes.Equal(b[:4], batchMagic)
	if counts {
		n := le64(b[12:])
		counts = n >= minBatch && n <= uint64(len(b)) && le32(b[n-4:]) == sum(b[:n-4])
	}
	var d *errs.Damage
	switch {
	case !counts:
		if !errors.Is(err, ErrDoesNotCount) || errors.Is(err, errs.ErrDamaged) {
			t.Fatalf("a batch that doesn't count gives %+v, %v", bt, err)
		}
		if refErr == nil && !zero { // the reader doesn't look at the sequence number
			t.Fatalf("fixtures_test.go's reader reads a batch that doesn't count")
		}
	case errors.Is(err, ErrDoesNotCount):
		t.Fatalf("a batch that counts gives %v", err)
	case err != nil:
		if !errors.As(err, &d) || d.Batch != seq || d.Offset < BatchHeadSize || d.Offset > int64(le64(b[12:])-4) {
			t.Fatalf("a batch that counts, with changes that break the rules, gives %v", err)
		}
		if refErr == nil {
			t.Fatalf("DecodeBatch gives %v, where fixtures_test.go's reader reads %v", err, toChanges(ref.changes))
		}
	case refErr != nil:
		t.Fatalf("DecodeBatch reads %v, where fixtures_test.go's reader gives %v", bt.Changes, refErr)
	case !sameChanges(bt.Changes, toChanges(ref.changes)) || bt.Length != ref.length || bt.Sum != ref.sum || bt.Seq != seq:
		t.Fatalf("DecodeBatch reads %+v, where fixtures_test.go's reader reads %+v", bt, ref)
	default:
		out, s, err := AppendBatch(nil, gen, seq, bt.Changes)
		if err != nil || s != bt.Sum || !bytes.Equal(out, b[:bt.Length]) {
			t.Fatalf("the batch decoded and encoded again gives % x, %v, where it was % x", out, err, b[:bt.Length])
		}
	}
}

// FuzzDecodeHeader is DecodeHeader on any bytes, in a file of any size.
func FuzzDecodeHeader(f *testing.F) {
	compacted := readFixture(f, "file-compacted.hex")
	f.Add(readFixture(f, "header-new.hex"), int64(headerSize))
	f.Add(readFixture(f, "header-compacted.hex"), int64(len(compacted)))
	f.Add(readFixture(f, "file-new.hex"), int64(4096))
	f.Add(compacted, int64(len(compacted)))
	f.Add(append([]byte("SQLite format 3\x00"), make([]byte, 84)...), int64(4096))
	f.Fuzz(func(t *testing.T, b []byte, size int64) {
		checkHeader(t, b, size)
		if len(b) >= headerSize {
			c := slices.Clone(b)
			binary.LittleEndian.PutUint32(c[64:], sum(c[:64]))
			checkHeader(t, c, size)
		}
	})
}

func checkHeader(t *testing.T, b []byte, size int64) {
	h, err := DecodeHeader(b, size)
	ref, refErr := readHeader(b)
	// The error FORMAT.md's order of checks gives, or nil.
	var want error
	switch {
	case len(b) < headerSize:
		want = errs.ErrNotDatabase
	case bytes.HasPrefix(b, sqliteMagic):
		want = errs.ErrZeroX
	case !bytes.Equal(b[:8], fileMagic):
		want = errs.ErrNotDatabase
	case le32(b[8:]) != 1:
		want = errs.ErrFormatVersion
	case refErr != nil || ref.gen > 1 && (size < 0 || ref.end > uint64(size)):
		want = errs.ErrDamaged
	}
	if want != nil {
		if !errors.Is(err, want) || errors.As(err, new(*errs.Damage)) != (want == errs.ErrDamaged) {
			t.Fatalf("DecodeHeader gives %+v, %v, where FORMAT.md's checks give %v", h, err, want)
		}
		return
	}
	if err != nil {
		t.Fatalf("DecodeHeader gives %v for a good header", err)
	}
	if h.Gen != ref.gen || h.FromGen != ref.contGen || h.FromSeq != ref.contSeq || h.FromSum != ref.contSum || h.CompactedEnd != ref.end || !bytes.Equal(h.ID[:], ref.id) {
		t.Fatalf("DecodeHeader reads %+v, where fixtures_test.go's reader reads %+v", h, ref)
	}
	if out, err := AppendHeader(nil, h); err != nil || !bytes.Equal(out, b[:headerSize]) {
		t.Fatalf("the header decoded and encoded again gives % x, %v, where it was % x", out, err, b[:headerSize])
	}
}

// FuzzDecodeMarker is DecodeMarker on any bytes, for the fixtures'
// database ID and any generation.
func FuzzDecodeMarker(f *testing.F) {
	f.Add(readFixture(f, "marker.hex"), uint64(1))
	for _, name := range []string{"file-new.hex", "file-compacted.hex"} {
		b := readFixture(f, name)
		ff, err := readFile(b)
		if err != nil {
			f.Fatalf("%s: %v", name, err)
		}
		for _, end := range ff.ends {
			f.Add(b[end-markerSize:], ff.gen)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, gen uint64) {
		checkMarker(t, b, gen)
		checkFind(t, b, gen)
		if len(b) >= markerSize {
			c := slices.Clone(b)
			binary.LittleEndian.PutUint32(c[16:], sum(fixtureID[:], binary.LittleEndian.AppendUint64(nil, gen), c[:16]))
			checkMarker(t, c, gen)
			checkFind(t, c, gen)
		}
	})
}

// checkFind checks that FindMarker finds the first offset in b where
// fixtures_test.go's reader reads a whole marker.
func checkFind(t *testing.T, b []byte, gen uint64) {
	want := -1
	for i := range b {
		if _, _, err := readMarker(b[i:], fixtureID[:], gen); err == nil {
			want = i
			break
		}
	}
	if at, m := FindMarker(b, fixtureID, gen); at != want {
		t.Fatalf("FindMarker gives %d, %+v, where fixtures_test.go's reader reads the first whole marker at %d", at, m, want)
	}
}

func checkMarker(t *testing.T, b []byte, gen uint64) {
	m, ok := DecodeMarker(b, fixtureID, gen)
	seq, s, refErr := readMarker(b, fixtureID[:], gen)
	switch {
	case ok != (refErr == nil):
		t.Fatalf("DecodeMarker gives %+v, %v, where fixtures_test.go's reader gives %v", m, ok, refErr)
	case !ok:
	case m != (Marker{Seq: seq, Sum: s}):
		t.Fatalf("DecodeMarker reads %+v, where fixtures_test.go's reader reads %d, %#08x", m, seq, s)
	default:
		if out := AppendMarker(nil, fixtureID, gen, m); !bytes.Equal(out, b[:markerSize]) {
			t.Fatalf("the marker decoded and encoded again gives % x, where it was % x", out, b[:markerSize])
		}
	}
}
