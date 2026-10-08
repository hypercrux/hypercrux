// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
	"github.com/hypercrux/hypercrux/beta/internal/logfile"
)

// OpenWith opens the database at path through the file calls files, with
// the log's options o, as Open does through the real ones. The tests use it
// for the fault layer's disk, and for a short wait for the write lock.
func OpenWith(files fsys.FS, path string, o logfile.Options) (*DB, error) {
	return open(files, path, o)
}
