// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import "github.com/hypercrux/hypercrux/beta/internal/store"

// PlantProblems has the copy's own check find problems besides those it
// finds, which Check reports and Export and Import refuse on, until the
// function it returns puts the check back. The store's tests plant damage
// in a copy itself; this lets the package's tests see what the package
// does with what the check finds. A test calls it before it starts any
// goroutine that checks.
func PlantProblems(problems ...string) (restore func()) {
	real := checkCopy
	checkCopy = func(s *store.Store) store.Checked {
		c := real(s)
		c.Problems = append(c.Problems, problems...)
		return c
	}
	return func() { checkCopy = real }
}
