// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && !hypercrux_planted

package hypercrux

import "github.com/hypercrux/hypercrux/beta/internal/store"

// plant is always empty in an ordinary build, so the compiler drops every
// branch that checks it, and no planted bug is built in.
const plant = ""

// shared is only ever called by a planted bug, which an ordinary build
// leaves out. Here it copies, as the driver does.
func shared(s string) []byte { return []byte(s) }

// sharedVector is only ever called by a planted bug too. Here it copies, as
// a Vector's Value method does.
func sharedVector(v Vector) string { return string(v.Bytes()) }

// compactStale is only ever called by a planted bug too. Here it's Compact's
// own way.
func (db *DB) compactStale(*store.Store) error {
	if err := db.log.Lock(); err != nil {
		return err
	}
	defer db.log.Unlock()
	return db.compactLocked()
}
