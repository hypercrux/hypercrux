// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sqlcorpus

import (
	"testing"

	"github.com/hypercrux/hypercrux/beta/conformance/betax"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
)

// TestTheBetaGivesTheCorpusAnswers replays the corpus on the Beta through
// Replay, its SQL going through the Beta's driver and engine (G4), in the
// Beta's own forms. Every case marked in gives 0.x's recorded answer: the
// statements, the writes, each in an Update that rolls back, with the query
// after it, the 3,000 expressions, and the 14 cases on 'now', which 0.x
// answers at the same moment. Every case marked out, the dates on fixed
// dates among them, is refused with an error of kind "error", as SQL.md
// says a statement outside the subset is.
func TestTheBetaGivesTheCorpusAnswers(t *testing.T) {
	var in, out []Case
	kinds := map[string]int{}
	for _, cs := range loadAll(t) {
		if cs.In {
			in = append(in, cs)
			kinds[cs.Kind]++
		} else {
			out = append(out, cs)
		}
	}
	diffs, err := Replay(betax.Engine{}, zerox.Engine{}, in, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range diffs {
		if i == 10 {
			t.Errorf("and %d more", len(diffs)-10)
			break
		}
		t.Error(d)
	}

	db, err := OpenFixture(betax.Engine{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, cs := range out {
		if ans := Run(betax.Engine{}, db, cs, true); ans.Error != "error" {
			t.Errorf("%s is outside the subset, and the Beta gives %s: %s", cs.ID, jsonOf(ans), cs.SQL)
		}
	}
	t.Logf("%d cases in, %v, gave 0.x's answers on the Beta, and %d cases out were refused", len(in), kinds, len(out))
}
