// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package logfile

import "github.com/hypercrux/hypercrux/beta/internal/format"

// How far this process has applied the log, for the public package's
// Check (G7), which reads the file again from its start up to there. Every
// byte before the end of a marker stays as it is for good (FORMAT.md,
// "Copies"), so that part of the file can be read without the write lock,
// and anything there that fails its checks is damage, with no writer at
// work to account for it.

// Applied is how far a process has applied the log: the file, by device and
// inode number, and the end of the log there, just past the last marker
// applied, or the header's end when no batch has been, or 0 in a file that
// was empty when it was read.
//
// Head is the file's header, and Last the marker that ends the log, of the
// batch numbered Seq, as this process read them, or wrote them: what a read
// of the file again has to find there, since nothing before the end of a
// marker ever changes. A file of another database, or another copy of this
// one, copied over the file in place, can have the same length, and differs
// there (R2, after G7.md's note). Seq is 0, and Last zeros, when no batch
// has been applied.
type Applied struct {
	Dev, Ino uint64
	End      int64

	Seq  uint64
	Head [format.HeaderSize]byte
	Last [format.MarkerSize]byte
}

// Applied returns how far this process has applied the log, as the holder
// of the write lock's mutex last left it, and false when the Log holds no
// file: after Close, or after a switch to another file failed. It takes no
// lock, so it can be called from any goroutine, inside an Update too. The
// copy in memory can be a commit or so past it, while the holder of the
// mutex is at work, and never behind it.
func (l *Log) Applied() (Applied, bool) {
	s := l.seen.Load()
	if s == nil {
		return Applied{}, false
	}
	return Applied{Dev: s.dev, Ino: s.ino, End: s.end, Seq: s.seq, Head: s.head, Last: s.last}, true
}
