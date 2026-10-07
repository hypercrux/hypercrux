// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"encoding/binary"
	"hash/crc32"
)

// MarkerSize is a marker's size in bytes.
const MarkerSize = 20

// Marker is what a marker says: the batch straight before it, by its
// sequence number and checksum.
type Marker struct {
	Seq uint64
	Sum uint32
}

// AppendMarker appends a marker to dst, for a file with the database ID id
// and the generation gen, and returns the longer slice. Its check value
// covers the ID and the generation, so the marker can't pass for one of
// another database, or of another generation of this one.
func AppendMarker(dst []byte, id [16]byte, gen uint64, m Marker) []byte {
	start := len(dst)
	b := append(dst, magicMarker...)
	b = binary.LittleEndian.AppendUint64(b, m.Seq)
	b = binary.LittleEndian.AppendUint32(b, m.Sum)
	return binary.LittleEndian.AppendUint32(b, markerCheck(id, gen, b[start:]))
}

// DecodeMarker reads the marker at the start of b, in a file with the
// database ID id and the generation gen. It reports whether the marker is
// whole, meaning its magic number and its check value are right. A marker
// that isn't whole never counts, and isn't damage on its own: it's what a
// writer left half written.
//
// Whether a whole marker marks the batch before it, and what a whole
// marker in the wrong place means, are the log's to decide (FORMAT.md,
// "Markers" and "Reading the log").
func DecodeMarker(b []byte, id [16]byte, gen uint64) (Marker, bool) {
	if len(b) < MarkerSize || string(b[:4]) != magicMarker {
		return Marker{}, false
	}
	if binary.LittleEndian.Uint32(b[16:]) != markerCheck(id, gen, b[:16]) {
		return Marker{}, false
	}
	return Marker{Seq: binary.LittleEndian.Uint64(b[4:]), Sum: binary.LittleEndian.Uint32(b[12:])}, true
}

// markerCheck returns a marker's check value: the CRC32C of the database
// ID, then the generation as a u64, then the marker's first 16 bytes.
func markerCheck(id [16]byte, gen uint64, first []byte) uint32 {
	var g [8]byte
	binary.LittleEndian.PutUint64(g[:], gen)
	c := crc32.Update(0, crcTable, id[:])
	c = crc32.Update(c, crcTable, g[:])
	return crc32.Update(c, crcTable, first[:16])
}
