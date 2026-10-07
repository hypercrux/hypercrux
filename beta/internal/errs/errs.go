// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package errs

import (
	"errors"
	"fmt"
	"strings"
)

// The differential harness compares errors by kind (difftest.Kind): "not
// found" for an error that wraps ErrNotFound, "invalid" for one that wraps
// ErrInvalid, and "error" for anything else. 0.x gives a plain error, of
// kind "error", for a damaged file, a file that isn't a database, a newer
// format version, a lock it waited 10 seconds for and a closed database.
// So the Beta's own errors wrap neither of 0.x's two, and each is of kind
// "error", while errors.Is still tells them apart from each other.

var (
	// ErrNotFound means a key, table or link doesn't exist. It's 0.x's,
	// with 0.x's text, and of kind "not found".
	ErrNotFound = errors.New("hypercrux: not found")

	// ErrInvalid means an argument breaks HyperCrux's rules, such as a key
	// without a table name or a vector of the wrong size. It's 0.x's, with
	// 0.x's text, and of kind "invalid".
	ErrInvalid = errors.New("hypercrux: invalid")

	// ErrDamaged means the file is damaged: its header fails its checks, a
	// marked batch fails its checksum when it's read again under the write
	// lock, a batch that counts holds changes that break the rules, or a
	// marker shows a commit past the end of the log. A *Damage says where.
	ErrDamaged = errors.New("hypercrux: the file is damaged")

	// ErrNotDatabase means the file at the path isn't a HyperCrux database:
	// it's shorter than a header, or doesn't start with the magic number.
	// An empty file holds no database yet, and a writer makes one of it, so
	// it doesn't give this error.
	ErrNotDatabase = errors.New("hypercrux: not a HyperCrux database")

	// ErrZeroX means the file starts with SQLite's header, so it's a
	// HyperCrux 0.x database, which the Beta can't open. 0.x's export and
	// the Beta's import move a database across.
	ErrZeroX = errors.New("hypercrux: a HyperCrux 0.x database; move it across with hypercrux export in 0.x and hypercrux import")

	// ErrFormatVersion means the file's format version is one this build
	// doesn't read. After the release that's a newer version. Before it,
	// every change to the format raises the version, so a build also
	// refuses a file from an older build.
	ErrFormatVersion = errors.New("hypercrux: a format version this build doesn't read")

	// ErrLockTimeout means the write lock didn't come free in time: 10
	// seconds, as in 0.x, or until a running compaction ends.
	ErrLockTimeout = errors.New("hypercrux: timed out waiting for the write lock")

	// ErrStuck means this handle refuses every write until it's closed. A
	// failed commit couldn't be cut back out of the file, or a compaction's
	// directory sync failed after its rename, so the handle keeps the write
	// lock and no process can write until it closes the database.
	ErrStuck = errors.New("hypercrux: writes are refused until the database is closed, since a failed write couldn't be undone in the file")

	// ErrInsideUpdate means a call went through the database inside its
	// own Update, where it would have to wait for that Update: any write,
	// and any read after the Update's first change. The call has to go
	// through the transaction.
	ErrInsideUpdate = errors.New("hypercrux: a call through the database inside its own Update; use the transaction")

	// ErrClosed means a call on a database after Close, or on a
	// transaction after its Update returned.
	ErrClosed = errors.New("hypercrux: closed")
)

// Damage says where a file is damaged and what's wrong there. It wraps
// ErrDamaged, so errors.Is finds that, and errors.As finds the Damage for
// the details, such as the batch that check names.
type Damage struct {
	Path   string // the file
	Offset int64  // where in the file the damage was found
	Batch  uint64 // the damaged batch's sequence number, or 0 when the damage isn't in a batch
	Reason string // what's wrong, such as "the checksum doesn't match"
}

func (d *Damage) Error() string {
	var b strings.Builder
	b.WriteString(ErrDamaged.Error())
	b.WriteString(": ")
	if d.Path != "" {
		b.WriteString(d.Path + ": ")
	}
	if d.Batch != 0 {
		fmt.Fprintf(&b, "batch %d at ", d.Batch)
	}
	fmt.Fprintf(&b, "offset %d", d.Offset)
	if d.Reason != "" {
		b.WriteString(": " + d.Reason)
	}
	return b.String()
}

// Unwrap returns ErrDamaged.
func (d *Damage) Unwrap() error { return ErrDamaged }
