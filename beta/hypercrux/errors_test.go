// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"fmt"
	"testing"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
	"github.com/hypercrux/hypercrux/beta/internal/errs"
)

// TestTheErrorsAreErrs checks that each error value is the very value in
// beta/internal/errs, so errors.Is matches an error from any layer, and
// that 0.x's two read as they do in 0.x.
func TestTheErrorsAreErrs(t *testing.T) {
	values := map[string][2]error{
		"ErrNotFound":      {hc.ErrNotFound, errs.ErrNotFound},
		"ErrInvalid":       {hc.ErrInvalid, errs.ErrInvalid},
		"ErrDamaged":       {hc.ErrDamaged, errs.ErrDamaged},
		"ErrNotDatabase":   {hc.ErrNotDatabase, errs.ErrNotDatabase},
		"ErrZeroX":         {hc.ErrZeroX, errs.ErrZeroX},
		"ErrFormatVersion": {hc.ErrFormatVersion, errs.ErrFormatVersion},
		"ErrLockTimeout":   {hc.ErrLockTimeout, errs.ErrLockTimeout},
		"ErrStuck":         {hc.ErrStuck, errs.ErrStuck},
		"ErrInsideUpdate":  {hc.ErrInsideUpdate, errs.ErrInsideUpdate},
		"ErrClosed":        {hc.ErrClosed, errs.ErrClosed},
	}
	for name, v := range values {
		if v[0] != v[1] {
			t.Errorf("%s is %v, which isn't errs.%s", name, v[0], name)
		}
	}
	for _, name := range errsNames(t) {
		if _, ok := values[name]; !ok && name != "Damage" {
			t.Errorf("errs has %s, which this test doesn't check", name)
		}
	}
	if hc.ErrNotFound.Error() != zx.ErrNotFound.Error() || hc.ErrInvalid.Error() != zx.ErrInvalid.Error() {
		t.Errorf("0.x's errors read %q and %q, and the Beta's %q and %q",
			zx.ErrNotFound, zx.ErrInvalid, hc.ErrNotFound, hc.ErrInvalid)
	}

	// Damage is errs' type under the package's name, so errors.As finds the
	// details through it.
	err := fmt.Errorf("opening: %w", &errs.Damage{Path: "/tmp/db", Offset: 1234, Batch: 3, Reason: "the checksum doesn't match"})
	var d *hc.Damage
	if !errors.As(err, &d) || d.Batch != 3 || d.Offset != 1234 {
		t.Errorf("errors.As found %+v in %v", d, err)
	}
	if !errors.Is(err, hc.ErrDamaged) {
		t.Errorf("%v doesn't match ErrDamaged", err)
	}
}
