// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fault

import (
	"slices"
	"sort"
)

// sectors is a set of sector numbers, held as sorted runs that neither
// overlap nor touch, so a file written from its start to its end is one
// run however long it grows.
type sectors []run

// run holds the sectors from from up to to, with to itself left out.
type run struct{ from, to int64 }

// add puts the sectors from from up to to in the set.
func (s *sectors) add(from, to int64) {
	if from >= to {
		return
	}
	rs := *s
	i := sort.Search(len(rs), func(i int) bool { return rs[i].to >= from })
	j := i
	for j < len(rs) && rs[j].from <= to {
		from, to = min(from, rs[j].from), max(to, rs[j].to)
		j++
	}
	*s = slices.Replace(rs, i, j, run{from, to})
}

// has reports whether sector x is in the set.
func (s sectors) has(x int64) bool {
	i := sort.Search(len(s), func(i int) bool { return s[i].to > x })
	return i < len(s) && s[i].from <= x
}
