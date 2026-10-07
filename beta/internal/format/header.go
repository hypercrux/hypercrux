// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
)

const (
	// Version is the format version this build reads and writes. Every
	// change to the format raises it, even before the release.
	Version = 1

	// HeaderSize is the header's size in bytes. The first batch starts
	// straight after it.
	HeaderSize = 68
)

// Header is what a file's header says, apart from the magic number, the
// format version and the header's checksum, which the codec writes and
// checks itself. A new database's header is a Header with its ID and Gen
// 1, and the rest 0.
type Header struct {
	ID  [16]byte // the database ID, 16 random bytes, which compaction keeps
	Gen uint64   // the generation: 1 in a new database, and one more after each compaction

	// The rest are set in a file written by compaction, and 0 in
	// generation 1. FromGen, FromSeq and FromSum name the batch the file
	// continues from: the last batch of the file it replaced, by that
	// file's generation and the batch's sequence number and checksum.
	// FromSeq and FromSum are 0 when that file held no batches.
	// CompactedEnd is the offset just past the compacted part, where the
	// first commit after the compaction starts: HeaderSize when nothing was
	// compacted.
	FromGen      uint64
	FromSeq      uint64
	FromSum      uint32
	CompactedEnd uint64
}

// AppendHeader appends h as a header to dst, and returns the longer slice.
// It refuses a header that breaks FORMAT.md's rules for the generation and
// the last four fields, with an error that wraps errs.ErrInvalid, and dst
// comes back as it was. It can't check that the compacted part ends within
// the file, which a writer knows only once it's written.
func AppendHeader(dst []byte, h Header) ([]byte, error) {
	if _, why := h.problem(); why != "" {
		return dst, fmt.Errorf("%w: a header with %s", errs.ErrInvalid, why)
	}
	start := len(dst)
	b := append(dst, magicFile...)
	b = binary.LittleEndian.AppendUint32(b, Version)
	b = append(b, h.ID[:]...)
	b = binary.LittleEndian.AppendUint64(b, h.Gen)
	b = binary.LittleEndian.AppendUint64(b, h.FromGen)
	b = binary.LittleEndian.AppendUint64(b, h.FromSeq)
	b = binary.LittleEndian.AppendUint32(b, h.FromSum)
	b = binary.LittleEndian.AppendUint64(b, h.CompactedEnd)
	return binary.LittleEndian.AppendUint32(b, crc32.Checksum(b[start:], crcTable)), nil
}

// DecodeHeader reads the header at the start of b, which holds the file's
// first HeaderSize bytes at least when the file has them, in a file of size
// bytes. It makes FORMAT.md's checks in FORMAT.md's order, and the first
// that fails gives the error:
//
//  1. A file shorter than a header isn't a database: errs.ErrNotDatabase.
//     An empty file holds no database yet, and the log makes one of it, so
//     it doesn't ask.
//  2. The magic number: SQLite's header gives errs.ErrZeroX, and anything
//     else errs.ErrNotDatabase.
//  3. The format version: errs.ErrFormatVersion.
//  4. The header's checksum, and
//  5. the generation and the last four fields, with the compacted part
//     ending within the file: both are damage, a *errs.Damage.
func DecodeHeader(b []byte, size int64) (Header, error) {
	switch {
	case len(b) < HeaderSize:
		return Header{}, fmt.Errorf("%w: %d bytes, too few for a header", errs.ErrNotDatabase, len(b))
	case string(b[:len(magicSQLite)]) == magicSQLite:
		return Header{}, errs.ErrZeroX
	case string(b[:8]) != magicFile:
		return Header{}, fmt.Errorf("%w: the file starts with % x", errs.ErrNotDatabase, b[:8])
	}
	if v := binary.LittleEndian.Uint32(b[8:]); v != Version {
		return Header{}, fmt.Errorf("%w: version %d, where this build reads version %d", errs.ErrFormatVersion, v, Version)
	}
	if got, want := binary.LittleEndian.Uint32(b[64:]), crc32.Checksum(b[:64], crcTable); got != want {
		return Header{}, &errs.Damage{Offset: 64, Reason: fmt.Sprintf("the header's checksum is %#08x, where CRC32C gives %#08x", got, want)}
	}
	h := Header{
		Gen:          binary.LittleEndian.Uint64(b[28:]),
		FromGen:      binary.LittleEndian.Uint64(b[36:]),
		FromSeq:      binary.LittleEndian.Uint64(b[44:]),
		FromSum:      binary.LittleEndian.Uint32(b[52:]),
		CompactedEnd: binary.LittleEndian.Uint64(b[56:]),
	}
	copy(h.ID[:], b[12:28])
	if at, why := h.problem(); why != "" {
		return Header{}, &errs.Damage{Offset: int64(at), Reason: "a header with " + why}
	}
	if h.Gen > 1 && (size < 0 || h.CompactedEnd > uint64(size)) {
		return Header{}, &errs.Damage{Offset: 56, Reason: fmt.Sprintf("a header with the compacted part ending at offset %d, past the end of the file at %d", h.CompactedEnd, size)}
	}
	return h, nil
}

// problem says what's wrong with h by the rules for a header's generation
// and last four fields, apart from the compacted part ending within the
// file, with the offset of the field at fault, or returns "".
func (h *Header) problem() (int, string) {
	switch {
	case h.Gen == 0:
		return 28, "generation 0"
	case h.Gen == 1 && (h.FromGen != 0 || h.FromSeq != 0 || h.FromSum != 0 || h.CompactedEnd != 0):
		return 36, "generation 1 and the fields of a compacted file set"
	case h.Gen > 1 && h.FromGen != h.Gen-1:
		return 36, fmt.Sprintf("generation %d continuing from generation %d", h.Gen, h.FromGen)
	case h.Gen > 1 && h.CompactedEnd < HeaderSize:
		return 56, fmt.Sprintf("the compacted part ending at offset %d, inside the header", h.CompactedEnd)
	}
	return 0, ""
}
