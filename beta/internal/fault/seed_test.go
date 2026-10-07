// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault_test

import (
	"fmt"
	"hash/crc32"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
)

// transcript runs random steps on a disk made with seed, with failed
// writes, failed syncs and cuts among them, and writes down what each
// file holds after every cut, and after one more at the end.
func transcript(t *testing.T, seed uint64, steps int) string {
	var b strings.Builder
	d := fault.New(seed)
	w := newWorld(t, d)
	r := rand.New(rand.NewPCG(7, 7))
	// note writes down what the restart after a cut read.
	note := func(when string) {
		w.restart(fmt.Sprintf("seed %d, %s", seed, when))
		fmt.Fprintf(&b, "%s:", when)
		for _, name := range names {
			if f := w.o.files[name]; f != nil {
				fmt.Fprintf(&b, " %s %d %08x", name, len(f.data), crc32.ChecksumIEEE(f.data))
			}
		}
		b.WriteByte('\n')
	}
	for i := range steps {
		if w.do(w.next(r, true)) {
			note(fmt.Sprintf("step %d", i))
		}
	}
	d.Cut()
	note("the end")
	fmt.Fprintf(&b, "%d cuts, %d calls failed, %d calls\n", d.Cuts(), d.Failed(), d.Calls(fault.Any, ""))
	return b.String()
}

// TestSameSeedSameDisk runs the same steps on disks made with the same
// seed, which must end the same, cut by cut, and on disks made with other
// seeds, which mustn't.
func TestSameSeedSameDisk(t *testing.T) {
	steps := 3000
	if testing.Short() {
		steps = 1000
	}
	seen := map[string]uint64{}
	for seed := range uint64(8) {
		got := transcript(t, seed, steps)
		if again := transcript(t, seed, steps); again != got {
			t.Fatalf("seed %d gave two disks:\n%s\nand\n%s", seed, got, again)
		}
		if other, ok := seen[got]; ok {
			t.Fatalf("seeds %d and %d gave the same disk", other, seed)
		}
		seen[got] = seed
		if strings.Count(got, "\n") < 10 {
			t.Fatalf("seed %d: only %d cuts", seed, strings.Count(got, "\n")-1)
		}
	}
}
