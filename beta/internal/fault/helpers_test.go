// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/fault"
	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// mustCreate makes a file at name and syncs its folder, so its name lasts
// every cut from then on, as the names of the engine's files do.
func mustCreate(t testing.TB, sys fsys.FS, name string, perm fs.FileMode) fsys.File {
	t.Helper()
	f, err := sys.Create(name, perm)
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.SyncDir(path.Dir(name)); err != nil {
		t.Fatal(err)
	}
	return f
}

// readAll reads the whole file at name through sys, by its size.
func readAll(t testing.TB, sys fsys.FS, name string) []byte {
	t.Helper()
	f, err := sys.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	in, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, in.Size+1)
	n, err := f.ReadAt(b, 0)
	if int64(n) != in.Size || err != io.EOF {
		t.Fatalf("%s: read %d bytes of %d, with %v", name, n, in.Size, err)
	}
	return b[:n]
}

// randomBytes returns n random bytes, none of them zero, so that every one
// differs from what a drive holds past the end of a file.
func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(1 + r.IntN(255))
	}
	return b
}

// padded returns b at n bytes long, with zeros past its end, as a file
// reads past what the drive holds.
func padded(b []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, b)
	return out
}

// fateOf says what a cut did to the bytes of one sector, given what the
// drive held before the cut and what reads saw: "kept" when they're as
// reads saw them, "lost" when they're as the drive held them, "torn, new
// first" or "torn, old first" when they're the one up to a point and the
// other after it, and "" when they're none of those.
func fateOf(got, held, seen []byte) string {
	switch {
	case bytes.Equal(got, seen):
		return "kept"
	case bytes.Equal(got, held):
		return "lost"
	case splits(got, seen, held):
		return "torn, new first"
	case splits(got, held, seen):
		return "torn, old first"
	}
	return ""
}

// splits reports whether got holds a's bytes up to some point, and b's
// from there on.
func splits(got, a, b []byte) bool {
	i := 0
	for i < len(got) && got[i] == a[i] {
		i++
	}
	return bytes.Equal(got[i:], b[i:])
}

// pathOf returns the path an error from a call names, the first one for a
// rename.
func pathOf(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Path
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Old
	}
	return ""
}

// mustCut checks that err says the power was cut.
func mustCut(t testing.TB, err error) {
	t.Helper()
	if !errors.Is(err, fault.ErrCut) {
		t.Fatalf("got %v where the power was cut", err)
	}
}
