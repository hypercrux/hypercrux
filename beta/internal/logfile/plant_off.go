// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && !hypercrux_planted

package logfile

import "github.com/hypercrux/hypercrux/beta/internal/format"

// plant is always empty in an ordinary build, so the compiler drops every
// branch that checks it, and no planted bug is built in.
const plant = ""

// The planted bugs that need code of their own have it in plant_on.go.
// These stand in for it, so the branches that check plant compile.

func (l *Log) plantedKeep(off, n, size int64, seq uint64) {}

func keptAt(*Log, int64) (format.Batch, bool) { return format.Batch{}, false }

func (l *Log) plantedApply(off, n, size int64, seq uint64) error { return nil }
