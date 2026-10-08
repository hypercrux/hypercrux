// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault

import (
	"maps"
	"path"
)

// change is what one call did to names, or to a file's owner, while the
// drive may not hold it yet. Disk.journal holds the changes in call order,
// as ext4's journal does.
type change struct {
	dirs  []string // the folders it's in: a SyncDir of either puts it on the drive
	names []entry  // the names it sets, in order
	owner *inode   // for a Chown, the file whose owner it sets
	uid   int      // the owner it sets
	gid   int      // the group it sets
}

// entry is a name and what it leads to, or nil where a change takes the
// name away.
type entry struct {
	path string
	n    *inode
}

// note adds c to the journal, with d.mu held.
func (d *Disk) note(c change) {
	d.journal = append(d.journal, c)
}

// store puts c on the drive.
func (d *Disk) store(c change) {
	for _, e := range c.names {
		if e.n == nil {
			delete(d.stored, e.path)
		} else {
			d.stored[e.path] = e.n
		}
	}
	if c.owner != nil {
		c.owner.suid, c.owner.sgid = c.uid, c.gid
	}
}

// syncDir puts on the drive every change in the journal made in the folder
// dir, with d.mu held, and every earlier change those lean on: before a
// rename between two folders, every earlier change in the other folder,
// and so on from there. The rest stay in the journal, in their order.
//
// That's Linux's promise for fsync on a folder: its own names as the calls
// left them. ext4 keeps more, since that fsync commits its whole journal,
// every folder's changes with it.
func (d *Disk) syncDir(dir string) {
	if plant == "fault/one-sync-for-all" {
		for _, c := range d.journal {
			d.store(c)
		}
		d.journal = nil
		return
	}
	// need holds, for each folder, the place in the journal up to which its
	// changes go on the drive. Going back from the end, a change that goes
	// takes the earlier changes in each of its folders with it.
	need := map[string]int{dir: len(d.journal) - 1}
	stored := make([]bool, len(d.journal))
	for i := len(d.journal) - 1; i >= 0; i-- {
		c := d.journal[i]
		for _, f := range c.dirs {
			if j, ok := need[f]; ok && j >= i {
				stored[i] = true
				break
			}
		}
		if !stored[i] {
			continue
		}
		for _, f := range c.dirs {
			if j, ok := need[f]; !ok || j < i {
				need[f] = i
			}
		}
	}
	var rest []change
	for i, c := range d.journal {
		if stored[i] {
			d.store(c)
		} else {
			rest = append(rest, c)
		}
	}
	d.journal = rest
}

// dirsOf returns the folder that holds n's name, or nothing when n has no
// name. A file has one name at most, since nothing links a second one.
func (d *Disk) dirsOf(n *inode) []string {
	for p, m := range d.names {
		if m == n {
			return []string{path.Dir(p)}
		}
	}
	return nil
}

// settleNames is what a cut does to names and owners, with d.mu held. The
// drive keeps the changes in the journal in call order up to a point, and
// loses the rest, as ext4 commits its journal in order and a cut can come
// between any two commits. A third of the time it keeps them all, a third
// of the time none, and otherwise the point falls anywhere among them, at
// either end included. Then every name and owner is the drive's, as after
// a restart. A file left without a name is gone: the cut leaves nothing
// that can reach it.
func (d *Disk) settleNames() {
	n := len(d.journal)
	keep := n
	if n > 0 {
		switch d.rng.IntN(3) {
		case 1:
			keep = 0
		case 2:
			keep = d.rng.IntN(n + 1)
		}
	}
	switch plant {
	case "fault/names-always-kept":
		keep = n
	case "fault/names-out-of-order":
		for _, c := range d.journal {
			if d.rng.IntN(2) == 0 {
				d.store(c)
			}
		}
		keep = 0
	}
	for _, c := range d.journal[:keep] {
		d.store(c)
	}
	d.journal = nil
	d.names = maps.Clone(d.stored)
	for _, n := range d.names {
		if !n.dir {
			n.nlink = 1
		}
		n.uid, n.gid = n.suid, n.sgid
	}
}

// early cuts the power before a call that changes names has made its
// change, when v says a rule cuts the power in it and the planted bug
// fault/cut-before-names is on, and reports whether it did. Otherwise the
// cut comes once the change is made, so it's pending at the cut.
func (d *Disk) early(v verdict) bool {
	if v == cutIt && plant == "fault/cut-before-names" {
		d.cut()
		return true
	}
	return false
}
