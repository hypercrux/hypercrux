// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault

import (
	"io/fs"
	"slices"

	"github.com/hypercrux/hypercrux/beta/internal/fsys"
)

// dev is the device number of every file and folder on a Disk.
const dev = 1

// inode is a file or a folder. A file has two copies of its data: the page
// cache, which reads see, and the drive, which keeps what the last sync
// put there. The bytes past the end of either are zeros.
type inode struct {
	ino      uint64
	dir      bool
	mode     fs.FileMode
	uid, gid int
	nlink    int // the names it has: 1, or 0 once it's removed or replaced

	cache   []byte  // what reads see
	disk    []byte  // what the drive holds, which can run past the size
	pending sectors // the sectors a change has touched since the last good sync
	durable int64   // the size the last good sync, or the last cut, left
	lo, hi  int64   // the smallest and largest sizes since then
	holder  *file   // the open file holding the flock, if one does
}

func (n *inode) info() fsys.Info {
	i := fsys.Info{Dev: dev, Ino: n.ino, Nlink: uint64(n.nlink), Mode: n.mode, Uid: n.uid, Gid: n.gid}
	if !n.dir {
		i.Size = int64(len(n.cache))
	}
	return i
}

// cached returns byte p as reads see it.
func (n *inode) cached(p int64) byte {
	if p < int64(len(n.cache)) {
		return n.cache[p]
	}
	return 0
}

// held returns byte p as the drive holds it.
func (n *inode) held(p int64) byte {
	if p < int64(len(n.disk)) {
		return n.disk[p]
	}
	return 0
}

// touch marks the sectors holding the bytes from from up to to as pending.
func (n *inode) touch(from, to int64) {
	if from < to {
		n.pending.add(from/SectorSize, (to+SectorSize-1)/SectorSize)
	}
}

func (n *inode) read(p []byte, off int64) int {
	if off >= int64(len(n.cache)) {
		return 0
	}
	return copy(p, n.cache[off:])
}

// write writes p at off in the cache. A gap past the end reads as zeros,
// and it's pending too, since the drive may hold old bytes there.
func (n *inode) write(p []byte, off int64) {
	if len(p) == 0 {
		return
	}
	end := off + int64(len(p))
	size := int64(len(n.cache))
	if end > size {
		n.cache = grow(n.cache, end)
		n.hi = max(n.hi, end)
	}
	copy(n.cache[off:], p)
	n.touch(min(off, size), end)
}

// truncate sets the size in the cache. Whatever it cuts off or adds is
// pending: the drive may still hold bytes there that reads no longer see.
func (n *inode) truncate(size int64) {
	old := int64(len(n.cache))
	switch {
	case size < old:
		n.cache = n.cache[:size]
		n.touch(size, old)
		n.lo = min(n.lo, size)
	case size > old:
		n.cache = grow(n.cache, size)
		n.touch(old, size)
		n.hi = max(n.hi, size)
	}
}

// sync puts every pending sector on the drive, and the size with it. A
// sector a failed sync marked clean isn't pending, so it isn't written.
func (n *inode) sync() {
	reach := max(int64(len(n.cache)), int64(len(n.disk)))
	for _, r := range n.pending {
		n.storeWhole(r.from*SectorSize, min(r.to*SectorSize, reach))
	}
	size := int64(len(n.cache))
	if size < int64(len(n.disk)) {
		n.disk = n.disk[:size]
	} else {
		n.disk = grow(n.disk, size)
	}
	n.pending = nil
	n.durable, n.lo, n.hi = size, size, size
}

// eachOwn, for a failed sync or a cut, gives each pending sector a fate of
// its own, where otherwise they all share one.
const eachOwn = -1

// What a failed sync does to a pending sector.
const (
	syncClean   = iota // it stays off the drive, and no later sync writes it
	syncPending        // the next sync writes it
	syncWritten        // it reached the drive before the error
	syncTorn           // it reached the drive torn, then was marked clean
)

// failSync settles the pending sectors as a sync that failed can leave
// them. A quarter of the time every one is marked clean, a quarter every
// one is left pending, a quarter every one is written, and otherwise each
// gets one of the four fates on its own. The size stays pending.
func (n *inode) failSync(d *Disk) {
	whole := [...]int{syncClean, syncPending, syncWritten, eachOwn}[d.rng.IntN(4)]
	var still sectors
	for _, r := range n.pending {
		for s := r.from; s < r.to; s++ {
			fate := whole
			if whole == eachOwn {
				fate = d.rng.IntN(4)
			}
			start, end := n.span(s)
			switch fate {
			case syncPending:
				still.add(s, s+1)
			case syncWritten:
				n.storeWhole(start, end)
			case syncTorn:
				n.storeTorn(d, start, end)
			}
			if plant == "fault/clean-pages-written" && (fate == syncClean || fate == syncTorn) {
				still.add(s, s+1)
			}
		}
	}
	n.pending = still
}

// What a cut does to a pending sector.
const (
	cutKept = iota // it holds what reads saw
	cutLost        // it holds what the drive held
	cutTorn        // it holds the one up to a point and the other after it
)

// settle is what a power cut does to the file. The size becomes the one
// the last sync left, the one reads saw, or any size from lo to hi, a
// third of the time each. A third of the time every pending sector is
// kept, a third of the time every one is lost, and otherwise each is kept,
// lost or torn on its own. Every other byte keeps what the drive held.
// Then the cache holds what the drive does, as after a restart.
func (n *inode) settle(d *Disk) {
	var size int64
	switch d.rng.IntN(3) {
	case 0:
		size = n.durable
	case 1:
		size = int64(len(n.cache))
	default:
		size = n.lo + d.rng.Int64N(n.hi-n.lo+1)
	}
	whole := [...]int{cutKept, cutLost, eachOwn}[d.rng.IntN(3)]
	if plant == "fault/cut-keeps-everything" {
		size, whole = int64(len(n.cache)), cutKept
	}
	next := make([]byte, size)
	copy(next, n.disk)
	for _, r := range n.pending {
		for s := r.from; s < r.to; s++ {
			start := s * SectorSize
			end := min(start+SectorSize, size)
			if start >= end {
				break
			}
			fate := whole
			if whole == eachOwn {
				fate = d.rng.IntN(3)
			}
			switch fate {
			case cutKept:
				n.copyCached(next, start, end)
			case cutTorn:
				split, newFirst, ok := n.tear(d, start, end)
				if !ok {
					if d.rng.IntN(2) == 0 {
						n.copyCached(next, start, end)
					}
					break
				}
				for p := start; p < end; p++ {
					if (p < split) == newFirst {
						next[p] = n.cached(p)
					} else if plant == "fault/torn-with-zeros" {
						next[p] = 0
					}
				}
			}
		}
	}
	n.disk = next
	n.cache = slices.Clone(next)
	n.pending = nil
	n.durable, n.lo, n.hi = size, size, size
	n.holder = nil
}

// span returns where sector s starts and ends, as far as the longer of the
// cache and the drive reaches. Past both, every byte is a zero either way.
func (n *inode) span(s int64) (start, end int64) {
	start = s * SectorSize
	return start, max(start, min(start+SectorSize, max(int64(len(n.cache)), int64(len(n.disk)))))
}

// tear picks how the bytes from start to end tear: split is where the
// second part starts, somewhere between the first and the last of the
// bytes where the cache and the drive differ, and newFirst says that the
// cache's bytes come first. ok is false when fewer than two bytes differ,
// since a tear could then only keep the bytes or lose them.
func (n *inode) tear(d *Disk, start, end int64) (split int64, newFirst, ok bool) {
	first, last := int64(-1), int64(-1)
	for p := start; p < end; p++ {
		if n.cached(p) != n.held(p) {
			if first < 0 {
				first = p
			}
			last = p
		}
	}
	if first == last {
		return 0, false, false
	}
	return first + 1 + d.rng.Int64N(last-first), d.rng.IntN(2) == 0, true
}

// copyCached copies the bytes from start to end as reads see them into
// dst, which starts where the file does.
func (n *inode) copyCached(dst []byte, start, end int64) {
	size := int64(len(n.cache))
	c := copy(dst[start:end], n.cache[min(start, size):min(end, size)])
	clear(dst[start+int64(c) : end])
}

// storeWhole puts the bytes from start to end on the drive as reads see
// them.
func (n *inode) storeWhole(start, end int64) {
	if start >= end {
		return
	}
	if end > int64(len(n.disk)) {
		n.disk = grow(n.disk, end)
	}
	n.copyCached(n.disk, start, end)
}

// storeTorn puts the bytes from start to end on the drive torn, as tear
// picks. When a tear can't change anything, it writes them whole or leaves
// them, by a coin.
func (n *inode) storeTorn(d *Disk, start, end int64) {
	split, newFirst, ok := n.tear(d, start, end)
	if !ok {
		if d.rng.IntN(2) == 0 {
			n.storeWhole(start, end)
		}
		return
	}
	if end > int64(len(n.disk)) {
		n.disk = grow(n.disk, end)
	}
	for p := start; p < end; p++ {
		if (p < split) == newFirst {
			n.disk[p] = n.cached(p)
		} else if plant == "fault/torn-with-zeros" {
			n.disk[p] = 0
		}
	}
}

// grow lengthens b to size bytes, with zeros.
func grow(b []byte, size int64) []byte {
	old := len(b)
	b = slices.Grow(b, int(size)-old)[:size]
	clear(b[old:])
	return b
}
