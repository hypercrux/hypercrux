// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package value

import (
	"reflect"
	"strings"
	"testing"
)

// TestThePublicPackagesPath checks the path FromGo knows the public Vector
// by against this package's own, so moving the tree moves both. The
// public package's tests check that its Vector goes into the vector field.
func TestThePublicPackagesPath(t *testing.T) {
	here := reflect.TypeOf(Value{}).PkgPath()
	if want := strings.TrimSuffix(here, "internal/value") + "hypercrux"; publicPackage != want {
		t.Errorf("FromGo looks for the public Vector in %s, and the public package is at %s", publicPackage, want)
	}
	if isPublicVector(float32s) || isPublicVector(reflect.TypeOf([]float64(nil))) {
		t.Error("an unnamed slice passed for the public Vector")
	}
}
