// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"bytes"
	"runtime"
)

// goroutine returns the calling goroutine's number. Go shows it only in the
// first line of runtime.Stack, "goroutine 18 [running]:", so that's where
// it comes from. The numbers start at 1 and none is used twice in a
// process, so 0 can stand for no goroutine.
//
// runtime.Stack walks the whole stack even into a small buffer, which costs
// about a microsecond, more on a deep stack. Begin calls it once for each
// transaction, and the checks call it only while a transaction is open.
func goroutine() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	id, ok := goroutineNumber(buf[:n])
	if !ok {
		// Every Go release so far starts the line this way. One that
		// didn't would break the check behind errs.ErrInsideUpdate, and
		// the tests say so at once.
		panic("store: runtime.Stack's first line has no goroutine number: " + string(buf[:n]))
	}
	return id
}

// goroutineNumber reads the number at the start of a stack trace, after
// "goroutine ", and reports whether there was one above 0.
func goroutineNumber(line []byte) (int64, bool) {
	rest, ok := bytes.CutPrefix(line, []byte("goroutine "))
	if !ok {
		return 0, false
	}
	var id int64
	i := 0
	for ; i < len(rest) && '0' <= rest[i] && rest[i] <= '9'; i++ {
		if id > (1<<62)/10 {
			return 0, false
		}
		id = id*10 + int64(rest[i]-'0')
	}
	if i == 0 || id == 0 || i < len(rest) && rest[i] != ' ' {
		return 0, false
	}
	return id, true
}
