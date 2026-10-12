// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package store

import "os"

// The planted bugs in the store's check (G7), which a build with the
// hypercrux_planted tag switches on by name in HYPERCRUX_PLANT, as
// plant_on.go does for the rest of the package. scripts/planted.sh runs the
// tests with each one listed in beta/plants.txt, and every one must make
// them fail.
//
//   - store/check-skips-mirror: the check takes each half of a link for the
//     whole link, and never looks for its mirror at the other end.
//   - store/check-zeros-let-through: the check takes a vector of zeros for
//     one that keeps the rules.
//   - store/check-free-slots-counted: the check counts every slot of a
//     table's vector array as a vector, the free ones included.
var checkPlant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "store/check-skips-mirror", "store/check-zeros-let-through", "store/check-free-slots-counted":
		return p
	}
	return ""
}()
