// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"fmt"
	"path/filepath"
	"testing"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// reads holds what Scan, Neighbours, Walk and Nearest gave, as the caller
// got it.
type reads struct {
	recs  []hc.Record
	links []hc.Link
	steps []hc.Step
	hits  []hc.Hit
}

// readAll makes each read through h.
func readAll(t *testing.T, h betaCalls) reads {
	t.Helper()
	var r reads
	var err error
	r.recs, err = h.Scan("docs:", "", 0)
	ok(t, err)
	r.links, err = h.Neighbours("docs:1", hc.Both, "")
	ok(t, err)
	r.steps, err = h.Walk("docs:1", hc.Out, "", 3)
	ok(t, err)
	r.hits, err = h.Nearest("docs", hc.Vector{1, 0}, 5, "")
	ok(t, err)
	return r
}

// enough fails the test unless each read found two results or more, so
// there's something to look at.
func (r reads) enough(t *testing.T) {
	t.Helper()
	if len(r.recs) < 2 || len(r.links) < 2 || len(r.steps) < 2 || len(r.hits) < 2 {
		t.Fatalf("the reads found too little to look at:\n%s", r)
	}
}

// String shows the reads with every byte and every number's bits.
func (r reads) String() string {
	s := ""
	for _, rec := range r.recs {
		s += fmt.Sprintf("%q %s\n", rec.Key, show(rec.Fields))
	}
	return s + fmt.Sprintf("%q\n%v\n%v", r.links, r.steps, r.hits)
}

// spoil changes everything the reads handed out that the caller can reach:
// each field and each byte of its value, and each link, step and hit.
func (r reads) spoil() {
	for _, rec := range r.recs {
		for name, v := range rec.Fields {
			if b, isBytes := v.([]byte); isBytes {
				for i := range b {
					b[i] ^= 0xff
				}
			}
			rec.Fields[name] = "spoilt"
		}
		rec.Fields["new_field"] = 1
	}
	for i := range r.links {
		r.links[i] = hc.Link{From: "x:1", Type: "spoilt", To: "x:2"}
	}
	for i := range r.steps {
		r.steps[i] = hc.Step{Key: "x:1", Depth: 99}
	}
	for i := range r.hits {
		r.hits[i] = hc.Hit{Key: "x:1", Distance: -1}
	}
}

// TestWhatTheCallsGiveIsTheCallers checks that what Scan, Neighbours, Walk
// and Nearest give is the caller's to keep once the read has ended, through
// the database and through a transaction. Later changes, through the
// transaction and once it has committed, leave it as it was, and so do
// later reads. Changing it changes nothing in the database, and nothing a
// later read gives.
func TestWhatTheCallsGiveIsTheCallers(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "test.hcx"))
	ok(t, db.Update(func(tx *hc.Tx) error {
		ok(t, tx.Put("docs:1", hc.Fields{"title": "one", "raw": []byte{1, 2, 3}, "vec": hc.Vector{1, 0}}))
		ok(t, tx.Put("docs:2", hc.Fields{"raw": []byte{4, 5}, "vec": hc.Vector{0.6, 0.8}}))
		ok(t, tx.Put("docs:3", hc.Fields{"n": 3, "vec": hc.Vector{0, 1}}))
		ok(t, tx.Link("docs:1", "cites", "docs:2"))
		ok(t, tx.Link("docs:2", "cites", "docs:3"))
		ok(t, tx.Link("docs:3", "owns", "docs:1"))
		return nil
	}))

	byDB := readAll(t, db)
	byDB.enough(t)
	before := byDB.String()
	var byTx reads
	var inTx string
	ok(t, db.Update(func(tx *hc.Tx) error {
		ok(t, tx.Put("docs:4", hc.Fields{"vec": hc.Vector{1, 1}})) // the first change
		byTx = readAll(t, tx)
		byTx.enough(t)
		inTx = byTx.String()
		// The transaction's later changes reach every record, list and
		// vector the reads came from.
		ok(t, tx.Put("docs:1", hc.Fields{"title": "changed", "raw": []byte{9}, "vec": hc.Vector{0, -1}}))
		ok(t, tx.Put("docs:2", hc.Fields{"raw": nil}))
		ok(t, tx.Unlink("docs:1", "", "docs:2"))
		ok(t, tx.Link("docs:1", "new", "docs:4"))
		ok(t, tx.Delete("docs:3"))
		readAll(t, tx).spoil()
		if got := byTx.String(); got != inTx {
			t.Errorf("what the transaction's reads gave changed with its later changes:\n%s\nwhere they gave\n%s", got, inTx)
		}
		return nil
	}))
	if inTx == before {
		t.Fatal("the transaction's reads didn't see its first change")
	}
	// Through the database, after the commit and more changes, and a drop.
	readAll(t, db).spoil()
	ok(t, db.Put("docs:2", hc.Fields{"raw": []byte{7, 7}, "vec": hc.Vector{1, 0}}))
	ok(t, db.Drop("docs"))
	for _, c := range []struct {
		what      string
		r         reads
		was, want string
	}{{"the database's", byDB, before, before}, {"the transaction's", byTx, inTx, inTx}} {
		if got := c.r.String(); got != c.want {
			t.Errorf("what %s reads gave changed with later changes:\n%s\nwhere they gave\n%s", c.what, got, c.want)
		}
	}

	// Changing what a read gave changes nothing in the database.
	ok(t, db.Update(func(tx *hc.Tx) error {
		ok(t, tx.Put("docs:1", hc.Fields{"title": "one", "raw": []byte{1, 2, 3}, "vec": hc.Vector{1, 0}}))
		ok(t, tx.Put("docs:2", hc.Fields{"raw": []byte{4, 5}, "vec": hc.Vector{0.6, 0.8}}))
		ok(t, tx.Put("docs:3", hc.Fields{"n": 3}))
		ok(t, tx.Link("docs:1", "cites", "docs:2"))
		ok(t, tx.Link("docs:2", "cites", "docs:1"))
		ok(t, tx.Link("docs:2", "cites", "docs:3"))
		return nil
	}))
	first := readAll(t, db)
	first.enough(t)
	want := first.String()
	first.spoil()
	if got := readAll(t, db).String(); got != want {
		t.Errorf("changing what the reads gave changed the database, which now gives\n%s\nwhere it gave\n%s", got, want)
	}
	ok(t, db.Update(func(tx *hc.Tx) error {
		r := readAll(t, tx)
		want := r.String()
		r.spoil()
		if got := readAll(t, tx).String(); got != want {
			t.Errorf("changing what the transaction's reads gave changed what it reads:\n%s\nwhere it read\n%s", got, want)
		}
		return nil
	}))
}
