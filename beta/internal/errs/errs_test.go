// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package errs_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hypercrux/hypercrux/beta/conformance"
	"github.com/hypercrux/hypercrux/beta/difftest"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
)

// engine is as much of a conformance.Engine as difftest.Kind uses: the two
// sentinel errors, here the Beta's.
type engine struct{ conformance.Engine }

func (engine) ErrNotFound() error { return errs.ErrNotFound }
func (engine) ErrInvalid() error  { return errs.ErrInvalid }

var damage = &errs.Damage{Path: "/tmp/db", Offset: 1234, Batch: 3, Reason: "the checksum doesn't match"}

// every lists each error value with the kind difftest gives it.
var every = []struct {
	err  error
	kind string
}{
	{errs.ErrNotFound, "not found"},
	{errs.ErrInvalid, "invalid"},
	{errs.ErrDamaged, "error"},
	{errs.ErrNotDatabase, "error"},
	{errs.ErrZeroX, "error"},
	{errs.ErrFormatVersion, "error"},
	{errs.ErrLockTimeout, "error"},
	{errs.ErrStuck, "error"},
	{errs.ErrInsideUpdate, "error"},
	{errs.ErrClosed, "error"},
	{damage, "error"},
}

// TestEachErrorHasOneKind checks every error against difftest.Kind, on its
// own and wrapped the way callers add detail.
func TestEachErrorHasOneKind(t *testing.T) {
	if got := difftest.Kind(engine{}, nil); got != "ok" {
		t.Errorf("Kind(nil) = %q", got)
	}
	for _, c := range every {
		for _, err := range []error{c.err, fmt.Errorf("%w: docs:1", c.err), fmt.Errorf("opening: %w", c.err)} {
			if got := difftest.Kind(engine{}, err); got != c.kind {
				t.Errorf("Kind(%v) = %q, want %q", err, got, c.kind)
			}
		}
	}
}

// TestErrorsAreDistinct checks that errors.Is tells every error apart from
// every other, apart from a Damage, which is ErrDamaged with details.
func TestErrorsAreDistinct(t *testing.T) {
	texts := map[string]bool{}
	for i, a := range every {
		if !strings.HasPrefix(a.err.Error(), "hypercrux: ") {
			t.Errorf("%q doesn't start with hypercrux: ", a.err)
		}
		if texts[a.err.Error()] {
			t.Errorf("two errors read %q", a.err)
		}
		texts[a.err.Error()] = true
		for j, b := range every {
			want := i == j || (a.err == damage && b.err == errs.ErrDamaged)
			if got := errors.Is(a.err, b.err); got != want {
				t.Errorf("errors.Is(%v, %v) = %v", a.err, b.err, got)
			}
		}
	}
}

func TestDamage(t *testing.T) {
	err := fmt.Errorf("opening: %w", damage)
	var d *errs.Damage
	if !errors.As(err, &d) || d != damage {
		t.Fatalf("errors.As doesn't find the Damage in %v", err)
	}
	want := "hypercrux: the file is damaged: /tmp/db: batch 3 at offset 1234: the checksum doesn't match"
	if got := damage.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	header := &errs.Damage{Offset: 0, Reason: "the header checksum doesn't match"}
	if got, want := header.Error(), "hypercrux: the file is damaged: offset 0: the header checksum doesn't match"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
