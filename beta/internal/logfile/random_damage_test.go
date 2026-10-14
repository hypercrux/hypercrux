// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// TestRandomDamage is R2's look at the end of the log and the look past it
// from outside: random damage to random files, each opened with the lock
// free, so Open checks the end of the log. A file is a new database of one to
// six commits, or now and then a compacted one, whose compacted part holds
// one to three batches and has commits after it. The damage is zeros over a
// few bytes, a byte changed, the file cut short, or zeros or random bytes
// from anywhere to its end, always past the header. Whatever the damage:
//
//   - Open reports damage, and changes nothing in the file, only when a whole
//     marker survives past where the damage starts, or the damage reaches
//     into a compacted part. A torn end past the compacted part, the file cut
//     short or ending in zeros or noise, is never damage.
//   - When Open works, it hands over the file's first batches, as they were
//     written, and leaves them in the file byte for byte. It stops short of a
//     batch only when no whole marker of that batch or a later one survives,
//     and never short of a batch that is whole with every marker before it,
//     since the check marks such a batch when its own marker has gone.
func TestRandomDamage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.hcx")
	trials := 2000
	if testing.Short() {
		trials = 300
	}
	for trial := range trials {
		r := rand.New(rand.NewPCG(uint64(trial), 0x5232))
		w := newBuilder(t)
		var part int64 // where a compacted part ends, or 0
		if r.IntN(4) == 0 {
			var lists [][]format.Change
			for range 1 + r.IntN(3) {
				lists = append(lists, randomChanges(r))
			}
			w = compacted(t, lists...)
			part = int64(w.hdr.CompactedEnd)
		}
		type span struct{ b0, m0, m1 int } // a batch, its marker, and its end
		var spans []span
		if part > 0 {
			// The compacted part's batches, read back from the bytes.
			for off := format.HeaderSize; off < int(part); {
				n, err := format.BatchLength(w.b[off:], w.hdr.Gen, uint64(len(spans)+1), int64(len(w.b)-off))
				if err != nil {
					t.Fatal(err)
				}
				spans = append(spans, span{off, off + int(n), off + int(n) + format.MarkerSize})
				off += int(n) + format.MarkerSize
			}
		}
		for range 1 + r.IntN(6) {
			b0 := len(w.b)
			sum := w.batch(randomChanges(r))
			m0 := len(w.b)
			w.marker(w.seq, sum)
			spans = append(spans, span{b0, m0, len(w.b)})
		}
		orig := bytes.Clone(w.b)
		dam := bytes.Clone(orig)
		lo := format.HeaderSize + r.IntN(len(dam)-format.HeaderSize)
		kind := r.IntN(5)
		torn := false // the damage runs to the end of the file, as a crash leaves an end
		switch kind {
		case 0:
			clear(dam[lo:min(len(dam), lo+1+r.IntN(80))])
		case 1:
			dam[lo] ^= byte(1 + r.IntN(255))
		case 2:
			dam, torn = dam[:lo], true
		case 3:
			clear(dam[lo:])
			torn = true
		case 4:
			for i := lo; i < len(dam); i++ {
				dam[i] = byte(r.Uint32())
			}
			torn = true
		}
		// Where the damage starts: the first byte it changed, or the first it
		// cut off. Zeros over zeros change nothing, and then there's nothing
		// to try.
		lo = len(dam)
		for i := range dam {
			if dam[i] != orig[i] {
				lo = i
				break
			}
		}
		if lo == len(orig) {
			continue
		}
		intact := func(a, b int) bool { return b <= len(dam) && bytes.Equal(dam[a:b], orig[a:b]) }
		os.Remove(path)
		if err := os.WriteFile(path, dam, 0o644); err != nil {
			t.Fatal(err)
		}
		rec := &recorder{}
		l, err := Open(fsys.OS{}, path, rec, Options{Wait: time.Second})
		after, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		what := func() string {
			return fmt.Sprintf("trial %d, damage %d from offset %d of %d", trial, kind, lo, len(orig))
		}
		if err != nil {
			var d *errs.Damage
			if !errors.As(err, &d) {
				t.Fatalf("%s: Open gives %v, which isn't damage", what(), err)
			}
			if !bytes.Equal(after, dam) {
				t.Fatalf("%s: Open reports %v, and the file has changed", what(), err)
			}
			shown := int64(lo) < part
			for _, s := range spans {
				shown = shown || s.m0 >= lo && intact(s.m0, s.m1)
			}
			if !shown || torn && int64(lo) >= part {
				t.Fatalf("%s: Open reports %v, where no whole marker survives past the damage", what(), err)
			}
			continue
		}
		s := len(rec.batches)
		l.Close()
		if int64(lo) < part {
			t.Fatalf("%s: the damage reaches into the compacted part, which ends at %d, and Open handed over %d batches", what(), part, s)
		}
		want := 0 // the batches whole with every marker before them
		for want < len(spans) && intact(spans[want].b0, spans[want].m0) && (want == 0 || intact(spans[want-1].m0, spans[want-1].m1)) {
			want++
		}
		if s < want {
			t.Fatalf("%s: Open handed over %d batches, where the first %d are whole with every marker before them", what(), s, want)
		}
		for j := s; j < len(spans); j++ {
			if intact(spans[j].m0, spans[j].m1) {
				t.Fatalf("%s: Open handed over %d batches, and batch %d's marker survives whole", what(), s, j+1)
			}
		}
		for i, b := range rec.batches {
			if b.seq != uint64(i+1) {
				t.Fatalf("%s: batch %d came as %d", what(), i+1, b.seq)
			}
		}
		if s > 0 && !bytes.Equal(after[:min(len(after), spans[s-1].m1)], orig[:spans[s-1].m1]) {
			t.Fatalf("%s: the file no longer holds its first %d batches as they were written", what(), s)
		}
	}
}
