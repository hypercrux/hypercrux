// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package betax

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/conformance/zerox"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
)

func TestMain(m *testing.M) { conformance.Main(m, Engine{}) }

// TestConformance runs the whole suite on the Beta, which must pass it as
// 0.x does, apart from the tests Skip names, which wait for later tasks.
func TestConformance(t *testing.T) { conformance.Run(t, Engine{}) }

// TestExportAndImport runs the suite's round trip through Export and
// Import on the Beta, as on 0.x.
func TestExportAndImport(t *testing.T) { conformance.RoundTrip(t, Engine{}) }

// TestMovingAcross moves a database from 0.x to the Beta and back, and
// from the Beta to 0.x and back, byte for byte.
func TestMovingAcross(t *testing.T) {
	t.Run("from 0.x", func(t *testing.T) { conformance.Across(t, zerox.Engine{}, Engine{}) })
	t.Run("from the Beta", func(t *testing.T) { conformance.Across(t, Engine{}, zerox.Engine{}) })
}

// TestTheAdapterReachesTheBeta checks what the adapter hands the suite,
// without running it: the Beta's errors, which are errs' values, its
// vectors, and Open's refusals by 0.x's rules, the first half of
// OpenNeedsAFile.
func TestTheAdapterReachesTheBeta(t *testing.T) {
	e := Engine{}
	if e.ErrNotFound() != errs.ErrNotFound || e.ErrInvalid() != errs.ErrInvalid {
		t.Errorf("the adapter's errors are %v and %v", e.ErrNotFound(), e.ErrInvalid())
	}
	if !strings.Contains(e.Name(), hc.Version) {
		t.Errorf("the adapter's name %q doesn't give the Beta's version", e.Name())
	}
	if v, err := e.ParseVector("[0.5, 1e-3, -2]"); err != nil || len(v) != 3 || v[1] != 0.001 {
		t.Errorf("ParseVector gave %v, %v", v, err)
	}
	if _, err := e.ParseVector("[0, 0]"); !errors.Is(err, e.ErrInvalid()) {
		t.Errorf("ParseVector took a vector of zeros: %v", err)
	}
	for _, p := range []string{":memory:", "file::memory:", "", "a?b"} {
		if db, err := e.Open(p); !errors.Is(err, e.ErrInvalid()) {
			if err == nil {
				db.Close()
			}
			t.Errorf("Open(%q) = %v", p, err)
		}
	}
	tests := conformance.Tests()
	for name := range e.Skip() {
		if !slices.Contains(tests, name) {
			t.Errorf("Skip names %q, which isn't a test in the suite", name)
		}
	}
}
