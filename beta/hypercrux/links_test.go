// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"math"
	"testing"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/store"
)

func TestParseDirectionIs0xs(t *testing.T) {
	for _, s := range []string{"out", "in", "both", "", "OUT", "In", "Both", " out", "out ", "both\n", "sideways", "o", "outin", "\x00", "ünï"} {
		got, gotErr := hc.ParseDirection(s)
		want, wantErr := zx.ParseDirection(s)
		if int(got) != int(want) || !sameError(gotErr, wantErr) {
			t.Errorf("ParseDirection(%q) = %v, %v; 0.x gives %v, %v", s, got, gotErr, want, wantErr)
		}
	}
	for _, d := range []hc.Direction{hc.Out, hc.In, hc.Both} {
		if got, err := hc.ParseDirection(d.String()); got != d || err != nil {
			t.Errorf("ParseDirection(%q) = %v, %v", d.String(), got, err)
		}
	}
}

func TestDirectionStringIs0xs(t *testing.T) {
	for _, n := range []int{0, 1, 2, -1, 3, 4, 100, math.MaxInt, math.MinInt} {
		if got, want := hc.Direction(n).String(), zx.Direction(n).String(); got != want {
			t.Errorf("Direction(%d).String() = %q; 0.x gives %q", n, got, want)
		}
	}
}

func TestLinkStringIs0xs(t *testing.T) {
	links := []hc.Link{
		{From: "customer:42", Type: "owns", To: "docs:7"},
		{},
		{From: "a:1", Type: "-", To: "b:2"},
		{From: "docs:3", Type: "cites", To: "docs:3"},
		{From: "x:ü", Type: "a b ✓", To: "y:\x00"},
	}
	for _, l := range links {
		if got, want := l.String(), zx.Link(l).String(); got != want {
			t.Errorf("%#v.String() = %q; 0.x gives %q", l, got, want)
		}
	}
	if got := links[0].String(); got != "customer:42 -owns-> docs:7" {
		t.Errorf("Link.String() = %q", got)
	}
}

// TestTheStoreTypesConvert pins what lets G2 hand out the store's results
// without copying them field by field: Link, Step and Hit have the store's
// fields, so each converts to and from the store's type, and Direction has
// the store's numbers and names.
func TestTheStoreTypesConvert(t *testing.T) {
	l := store.Link{From: "customer:42", Type: "owns", To: "docs:7"}
	if store.Link(hc.Link(l)) != l || hc.Link(l).String() != l.String() {
		t.Errorf("Link %v didn't come through", l)
	}
	s := store.Step{Key: "docs:2", Depth: 3}
	if store.Step(hc.Step(s)) != s {
		t.Errorf("Step %v didn't come through", s)
	}
	h := store.Hit{Key: "docs:2", Distance: 0.25}
	if store.Hit(hc.Hit(h)) != h {
		t.Errorf("Hit %v didn't come through", h)
	}
	for _, d := range []store.Direction{store.Out, store.In, store.Both, 3, -1} {
		if got := hc.Direction(d); int(got) != int(d) || got.String() != d.String() {
			t.Errorf("store.Direction %v is %v here", d, got)
		}
	}
}
