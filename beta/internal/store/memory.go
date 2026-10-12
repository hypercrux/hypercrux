// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"unsafe"

	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// How much memory the copy takes (G7), which Check gives as Checked.Bytes
// and the public package's Check reports beside the file's size. BETA.md's
// row on memory under "Risks" asks for it: every process holds the whole
// database, and GOMEMLIMIT set a little above the copy's size keeps Go's
// heap near it.
//
// Go can't say how much memory one structure takes, so memory adds up the
// copy's parts: each block of memory the copy holds, at the size Go lays it
// out, rounded up as Go's allocator rounds it (alloc). That's the hash
// table, each table with its fields, its keys and its vector array, and
// each record with its key, its fields and their text and bytes, and its
// two lists of links, room kept for growing included. It leaves out what
// isn't the copy's alone or is small: each link type's text, kept once
// for every link of that type by the unique package, the buffers the
// writes use again, and the room a transaction's undo list keeps for the
// next. A test holds the sum to what the heap grows by as a store fills,
// within an eighth (TestTheCopysSizeIsWhatItTakes).

// memory returns about how many bytes the copy takes in memory, as the
// comment above says. Like Check, it's a read.
func (s *Store) memory() int64 {
	const (
		str     = int64(unsafe.Sizeof(""))
		pointer = int64(unsafe.Sizeof(&record{}))
		slice   = int64(unsafe.Sizeof([]byte(nil)))
	)
	n := alloc(int64(unsafe.Sizeof(Store{})))
	n += mapBytes(len(s.records), str+pointer) + mapBytes(len(s.tables), str+pointer)
	for _, t := range s.tables {
		n += alloc(int64(unsafe.Sizeof(table{}))) + text(t.name)
		n += alloc(int64(cap(t.fields)) * str)
		for _, f := range t.fields {
			n += text(f)
		}
		n += mapBytes(len(t.index), str+int64(unsafe.Sizeof(0)))
		n += alloc(int64(cap(t.keys.blocks)) * slice)
		for _, b := range t.keys.blocks {
			n += alloc(int64(cap(b)) * int64(unsafe.Sizeof(entry{})))
		}
		a := &t.vecs
		n += alloc(int64(cap(a.blocks)) * slice)
		for _, b := range a.blocks {
			n += alloc(int64(cap(b)) * 4)
		}
		n += alloc(int64(cap(a.norms))*8) + alloc(int64(cap(a.owners))*pointer) + alloc(int64(cap(a.free))*int64(unsafe.Sizeof(0)))
	}
	for _, r := range s.records {
		if r == nil {
			continue
		}
		n += alloc(int64(unsafe.Sizeof(record{}))) + text(r.key)
		n += alloc(int64(cap(r.fields)) * int64(unsafe.Sizeof(FieldValue{})))
		for _, f := range r.fields {
			if k := f.Value.Kind(); k == value.KindText || k == value.KindBytes {
				n += text(f.Value.Raw())
			}
		}
		for _, l := range []*links{&r.out, &r.in} {
			n += alloc(int64(cap(l.blocks)) * slice)
			for _, b := range l.blocks {
				n += alloc(int64(cap(b)) * int64(unsafe.Sizeof(half{})))
			}
		}
	}
	return n
}

// text is what a string of its own takes: Go packs a block of under 16
// bytes that holds no pointers in with others, so it takes its own length,
// and a longer one takes what alloc gives.
func text(s string) int64 {
	if len(s) < 16 {
		return int64(len(s))
	}
	return alloc(int64(len(s)))
}

// mapBytes is about what a Go map of n entries takes, with slot bytes for
// each entry's key and value. Since Go 1.24 a map keeps its entries in
// groups of 8 slots, each group after a word of control bytes, in tables of
// up to 1,024 slots, and a table doubles once it's seven eighths full. So a
// map has between n and about twice n slots, and mapBytes takes the
// smallest power of two that holds n at seven eighths full.
func mapBytes(n int, slot int64) int64 {
	const header = 48 // the map's own fields, about
	if n == 0 {
		return header
	}
	slots := int64(8)
	for slots*7/8 < int64(n) {
		slots *= 2
	}
	group := 8 + 8*slot
	if slots <= 1024 {
		return header + alloc(slots/8*group)
	}
	tables := slots / 1024
	return header + alloc(tables*8) + tables*alloc(1024/8*group)
}

// alloc returns how much memory Go's allocator gives a request for n bytes:
// the size class at or above n up to 32 KB, from runtime/sizeclasses.go,
// and whole pages of 8 KB above that.
func alloc(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n > sizeClasses[len(sizeClasses)-1] {
		return (n + pageSize - 1) / pageSize * pageSize
	}
	lo, hi := 0, len(sizeClasses)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if sizeClasses[mid] < n {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return sizeClasses[lo]
}

// pageSize is the size of the runtime's pages, which a large block of
// memory takes whole.
const pageSize = 8192

// sizeClasses are the sizes Go's allocator hands out small blocks of
// memory in, as runtime/sizeclasses.go lists them.
var sizeClasses = [...]int64{
	8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240, 256,
	288, 320, 352, 384, 416, 448, 480, 512, 576, 640, 704, 768, 896, 1024, 1152, 1280,
	1408, 1536, 1792, 2048, 2304, 2688, 3072, 3200, 3456, 4096, 4864, 5376, 6144, 6528,
	6784, 6912, 8192, 9472, 9728, 10240, 10880, 12288, 13568, 14336, 16384, 18432, 19072,
	20480, 21760, 24576, 27264, 28672, 32768,
}
