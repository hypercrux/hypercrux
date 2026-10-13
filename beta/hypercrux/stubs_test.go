// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"errors"
	"testing"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// errOf is the error of a call that returns a value and an error.
func errOf[T any](_ T, err error) error { return err }

// TestOpenRefusesWhat0xRefuses checks Open's rules for a path against
// 0.x's Open, which refuses each of these names before it touches a file.
// Each error reads the same and wraps ErrInvalid.
func TestOpenRefusesWhat0xRefuses(t *testing.T) {
	t.Chdir(t.TempDir()) // in case a name ever reached a file
	for _, p := range []string{"", ":memory:", "file::memory:", "file:test.db", "file:", "a?b", "a#b", "?", "#", "dir/a?b.db", "x.db?mode=ro", "x.db#frag"} {
		_, got := hc.Open(p)
		_, want := zx.Open(p)
		if !errors.Is(want, zx.ErrInvalid) {
			t.Fatalf("0.x's Open(%q) gives %v, so the name doesn't belong here", p, want)
		}
		if !sameError(got, want) {
			t.Errorf("Open(%q) = %v; 0.x gives %v", p, got, want)
		}
	}
}
