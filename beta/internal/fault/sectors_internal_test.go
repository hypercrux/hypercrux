// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault

import (
	"math/rand/v2"
	"testing"
)

// TestSectors adds random runs of sectors to a set, and to a plain map
// beside it, and checks after each that the set holds what the map does,
// as sorted runs that neither overlap nor touch.
func TestSectors(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		var s sectors
		m := map[int64]bool{}
		for range 30 {
			from := r.Int64N(200)
			to := from + r.Int64N(12)
			s.add(from, to)
			for x := from; x < to; x++ {
				m[x] = true
			}
			n := int64(0)
			for i, run := range s {
				if run.from >= run.to || (i > 0 && s[i-1].to >= run.from) {
					t.Fatalf("the runs %v overlap, touch or are empty", s)
				}
				n += run.to - run.from
			}
			if n != int64(len(m)) {
				t.Fatalf("the set holds %d sectors, and the map %d", n, len(m))
			}
			for x := int64(-1); x < 220; x++ {
				if s.has(x) != m[x] {
					t.Fatalf("sector %d: in the set %v, in the map %v; %v", x, s.has(x), m[x], s)
				}
			}
		}
	}
}

// TestAppendsStayOneRun checks that a file written from its start to its
// end, as the log grows, keeps one run however many writes it takes.
func TestAppendsStayOneRun(t *testing.T) {
	n := &inode{}
	off := int64(0)
	for i := range 1000 {
		p := make([]byte, 1+i%700)
		n.write(p, off)
		off += int64(len(p))
	}
	if len(n.pending) != 1 || n.pending[0] != (run{0, (off + SectorSize - 1) / SectorSize}) {
		t.Errorf("appends left the runs %v", n.pending)
	}
}
