// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	val "github.com/hypercrux/hypercrux/beta/internal/value"
)

// The decoder's memory (R2). decodeChanges makes its arrays once, the size
// countChanges finds, and a vector block's count has to fit the bytes left
// before any key is read.

// TestTheCountsAreTheDecodersOwn holds countChanges to what DecodeBatch
// reads, on random batches of every kind of change: the same number of
// changes, fields, field names and vectors. A count that's off costs a
// decode the arrays growing again, and would show the two walks of the
// syntax have come apart.
func TestTheCountsAreTheDecodersOwn(t *testing.T) {
	m := newMaker(4, false)
	for round := range rounds(3000, 300) {
		cs := m.changes()
		gen, seq := m.genSeq()
		b, _, err := AppendBatch(nil, gen, seq, cs)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		bt, err := DecodeBatch(b, gen, seq)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		var want counts
		want.changes, want.ok = len(bt.Changes), true
		for _, c := range bt.Changes {
			want.fields += len(c.Fields)
			want.names += len(c.Names)
			want.vecs += len(c.Vecs)
		}
		if got := countChanges(string(b[BatchHeadSize : len(b)-4])); got != want {
			t.Fatalf("round %d: countChanges gives %+v, where the batch holds %+v: %v", round, got, want, cs)
		}
	}
}

// TestADecodedBatchsArraysAreMadeOnce decodes a batch of a create change and
// 10,000 puts, as an import's commit holds. Its changes, their fields and the
// create change's names each take one allocation, beside the batch's string:
// grown an append at a time, they took about five times the arrays' size in
// copies thrown away, and a batch of 100,000 such puts four times as long to
// decode.
func TestADecodedBatchsArraysAreMadeOnce(t *testing.T) {
	changes := []Change{{Op: CreateTable, Table: "docs", Names: []string{"n", "title"}}}
	for i := range 10000 {
		changes = append(changes, Change{Op: Put, Key: fmt.Sprintf("docs:%05d", i), Fields: []Field{
			{Name: "n", Value: val.Int(int64(i))},
			{Name: "title", Value: val.Text("a title")},
		}})
	}
	b, _, err := AppendBatch(nil, 1, 1, changes)
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(5, func() {
		if _, err := DecodeBatch(b, 1, 1); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 4 {
		t.Errorf("decoding a batch of %d changes took %v allocations, where its string and three arrays take 4", len(changes), allocs)
	}
}

// TestAVectorBlocksCountMustFitItsBytes decodes a batch that counts, holding
// one vector block that claims 4,294,967,295 vectors of one value, followed
// by a megabyte of zeros. Read one key at a time, each key would be the empty
// string, and the keys alone would take about 17 MB of memory, and five times
// that while their array grew, before the bytes ran out. The count can't fit
// in what's left, so the block is damage before any key is read, and the
// decode allocates little more than its copy of the batch.
func TestAVectorBlocksCountMustFitItsBytes(t *testing.T) {
	body := rawBlock("docs", 1, math.MaxUint32, nil, "")
	body = append(body, make([]byte, 1<<20)...)
	b := seal(1, 1, body)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := DecodeBatch(b, 1, 1)
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 4*uint64(len(b)) {
		t.Errorf("decoding a batch of %d bytes allocated %d bytes, more than four times the batch", len(b), got)
	}
	var d *errs.Damage
	if !errors.As(err, &d) || errors.Is(err, ErrDoesNotCount) || d.Batch != 1 || d.Offset != BatchHeadSize {
		t.Errorf("DecodeBatch gives %v, where it's damage at the block, at offset %d", err, BatchHeadSize)
	}
}
