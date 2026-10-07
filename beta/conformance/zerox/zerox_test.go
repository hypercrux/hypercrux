// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package zerox

import (
	"testing"

	"github.com/hypercrux/hypercrux/beta/conformance"
)

func TestMain(m *testing.M) { conformance.Main(m, Engine{}) }

// TestConformance runs the whole suite on 0.x, which must pass unchanged.
func TestConformance(t *testing.T) { conformance.Run(t, Engine{}) }
