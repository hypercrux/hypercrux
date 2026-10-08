// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package logfile

import (
	"os"
	"sync"

	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
//   - logfile/no-inode-check: Lock takes the file it locked for the one at
//     the path, without comparing device and inode numbers, so a commit
//     goes into a file that another has replaced.
//   - logfile/one-try: Lock tries flock once and gives up, without
//     waiting for the lock to come free.
//   - logfile/any-marker: a whole marker marks the batch before it, even
//     when it names another batch.
//   - logfile/marker-before-sync: Append writes the batch's marker before
//     the sync as well as after it.
//   - logfile/no-read-check: Lock doesn't check that what it has read is
//     still in the file, so it appends to a file that was copied over.
//
// F3's, in the check of the end of the log:
//
//   - logfile/cut-what-counts: a batch that counts and lost its marker is
//     cut off with the rest, where it's written again and marked, so a
//     commit that succeeded is lost when a power cut takes its marker.
//   - logfile/sync-without-rewrite: such a batch is synced and marked
//     without being written again, so after a failed sync marked its pages
//     clean, the sync puts nothing on the drive.
//   - logfile/no-marker-again: the batch is written again without the
//     marker before it, which a failed sync can have left off the drive too.
//
// F4's, in the look past the end of the log for damage:
//
//   - logfile/cut-at-zeros: zeros where a batch should start are taken for
//     the end of everything, and the look past them is left out, so the
//     check cuts off commits that a damaged batch's header hid.
//   - logfile/damage-unconfirmed: Open, when another holds the write lock,
//     reports what it finds past the end of the log as damage at once,
//     without reading it again holding the lock, so a writer at work there
//     makes a sound database fail to open.
//   - logfile/window-edge: the look past reads the file a window at a time
//     with no overlap between windows, so a marker across the edge of two
//     is missed.
//   - logfile/any-sequence-number: a whole marker past the end of the log
//     shows a commit there whatever batch it names, so an older copy of the
//     database stored in a torn batch makes the file read as damaged.
//   - logfile/compacted-held-only: the rule for a compacted file is kept
//     only holding the lock, so Open, when another holds it, reads part of
//     a damaged compacted part as a whole database.
//
// F5's, in failed commits:
//
//   - logfile/failed-left-uncut: a failed commit's batch is left in the
//     file, and the Log carries on, so the next check marks it when it
//     counts, under commits that were made without it.
//   - logfile/cut-unsynced: the cut back isn't synced at its end, so a
//     power cut can bring back a failed commit that was synced before its
//     marker's write failed.
//   - logfile/cut-without-marker: the cut back doesn't write the last
//     marker again, so a failed sync can leave the marker off the drive
//     for good, and a power cut after later commits loses them or leaves
//     the file damaged.
//   - logfile/stuck-lets-go: a stuck Log lets go of flock at Unlock, so
//     another writer goes ahead while the stuck one still holds what it
//     couldn't cut back.
//   - logfile/check-cuts-what-failed: a failure in the check cuts the end
//     back, as a failed commit's does, so a batch the check was marking,
//     whose commit may have succeeded, is lost.
//
// F6's, in following other processes:
//
//   - logfile/follow-before-marker: a follower applies a batch once it
//     counts, without waiting for its marker, so it takes in a commit
//     before it's on the drive, and a failed commit that's cut back.
//   - logfile/follow-keeps-unmarked: a follower keeps a batch it found
//     without its marker, and applies what it kept once a whole marker
//     naming the batch's sequence number follows, without reading the
//     batch again, so a failed commit's batch, cut back and written over
//     by another of the same length, is taken for the one that replaced it.
//   - logfile/follow-without-mutex: a follower reads on without the write
//     lock's mutex, so it can read this process's own commit between its
//     marker and Append's move of the read position, and apply it twice.
//   - logfile/follow-never-tries: a follower that stops short of the end
//     of the file never tries the lock, so when the writer has gone, the
//     batch it left stays unmarked until another writer comes.
//   - logfile/follow-shrink-unseen: a file shorter than the end of the log
//     a follower has applied is taken for one with nothing new.
//   - logfile/follow-path-unchecked: a follower doesn't compare the file at
//     the path with its own, so after a compaction or a backup moved into
//     place, it goes on reading the old file.
//   - logfile/follow-no-confirm: a follower that finds a whole marker
//     naming the batch it stopped at, while the batch fails its checks,
//     takes no notice while another holds the lock, so damage under a busy
//     writer is never reported.
//   - logfile/follow-read-once: a follower that finds a whole marker after
//     a batch that fails its checks takes that for what may be damage at
//     once, without reading the batch again, so a read that caught a batch
//     as its writer finished it costs a wait for the lock.
//   - logfile/follow-stuck-checks: a stuck Log's follower tries the lock,
//     which it holds already, checks the end of the log, writing what a
//     stuck handle mustn't, and lets go of flock.
//   - logfile/lock-waits-blind: a Lock that waits for flock reads nothing
//     meanwhile, so while it waits, the reads in its process, which find
//     the mutex held, miss every commit other processes make.
//
// F8's, in compaction:
//
//   - logfile/compact-rename-before-sync: the compacted file is renamed over
//     the database and the folder synced before the file is, so a power cut
//     can leave at the path a file whose bytes never reached the drive.
//   - logfile/compact-no-folder-sync: the folder isn't synced after the
//     rename, so a power cut can take the rename back, and with it every
//     commit made to the compacted file since.
//   - logfile/compact-folder-sync-ignored: a failed folder sync after the
//     rename is taken for a good one, so the Log goes on committing to a
//     file a power cut can still take away, and lets other writers do so.
//   - logfile/compact-lets-go-early: the old file's lock goes once the
//     compacted file is written, before its sync and the rename, so a
//     writer waiting for it commits to the old file, and the rename then
//     replaces that commit.
//   - logfile/compact-wait-ignored: a writer waiting for the lock gives up at
//     its usual deadline while a compaction runs.
//   - logfile/compact-left-behind: a compaction that fails before the switch
//     leaves NAME.compact beside the database.
//   - logfile/compact-one-batch: the compacted part goes in one batch, the
//     links with the records, however large it is, so a large database
//     takes a batch as large as itself in memory.
//   - logfile/compact-locked-removed: the holder of the write lock removes a
//     NAME.compact that's locked, so a compaction under way loses its file.
//   - logfile/compact-leftover-before-inode-check: Lock removes a leftover
//     NAME.compact before it checks that the file it locked is the one at
//     the path, so a writer holding the lock of a file that another has
//     replaced can remove the file of a compaction under way.
//
// F9's, in reloading:
//
//   - logfile/reload-without-collecting: a reload reads the new file
//     without a garbage collection after the Target's Reset, so the old copy
//     is still in memory, uncollected, while the new one fills it again.
//   - logfile/reload-waits-for-the-mutex: Reload waits for the write lock's
//     mutex, holding fol, so a read waits for an Update's function, and a
//     follower waits for a mutex a read inside the Update holds it up for.
//   - logfile/reload-unchecked: Reload reads the new file's log and stops
//     there, without the check Open makes, so a batch a killed writer left
//     at its end stays unmarked, and damage past its end goes unreported.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "logfile/no-inode-check", "logfile/one-try", "logfile/any-marker", "logfile/marker-before-sync", "logfile/no-read-check",
		"logfile/cut-what-counts", "logfile/sync-without-rewrite", "logfile/no-marker-again",
		"logfile/cut-at-zeros", "logfile/damage-unconfirmed", "logfile/window-edge", "logfile/any-sequence-number", "logfile/compacted-held-only",
		"logfile/failed-left-uncut", "logfile/cut-unsynced", "logfile/cut-without-marker", "logfile/stuck-lets-go", "logfile/check-cuts-what-failed",
		"logfile/follow-before-marker", "logfile/follow-keeps-unmarked", "logfile/follow-without-mutex", "logfile/follow-never-tries",
		"logfile/follow-shrink-unseen", "logfile/follow-path-unchecked", "logfile/follow-no-confirm", "logfile/follow-read-once",
		"logfile/follow-stuck-checks", "logfile/lock-waits-blind",
		"logfile/compact-rename-before-sync", "logfile/compact-no-folder-sync", "logfile/compact-folder-sync-ignored",
		"logfile/compact-lets-go-early", "logfile/compact-wait-ignored", "logfile/compact-left-behind", "logfile/compact-one-batch",
		"logfile/compact-locked-removed", "logfile/compact-leftover-before-inode-check",
		"logfile/reload-without-collecting", "logfile/reload-waits-for-the-mutex", "logfile/reload-unchecked":
		return p
	}
	return ""
}()

// kept is what logfile/follow-keeps-unmarked keeps for next time: for each
// Log, the batch its follower last found without its marker, where it
// starts.
var kept sync.Map

type keptBatch struct {
	off int64
	bt  format.Batch
}

// plantedKeep is logfile/follow-keeps-unmarked's: it reads the batch at
// off, n bytes long in a file of size bytes, and keeps it when it counts.
func (l *Log) plantedKeep(off, n, size int64, seq uint64) {
	b, err := l.r.read(l.f, off, n, size)
	if err != nil {
		return
	}
	if bt, err := format.DecodeBatch(b, l.hdr.Gen, seq); err == nil {
		kept.Store(l, keptBatch{off: off, bt: bt})
	}
}

// keptAt returns the batch plantedKeep kept for l, when it starts at off.
func keptAt(l *Log, off int64) (format.Batch, bool) {
	v, ok := kept.Load(l)
	if !ok {
		return format.Batch{}, false
	}
	k := v.(keptBatch)
	return k.bt, k.off == off
}

// plantedApply is logfile/follow-before-marker's: it applies the batch
// seq at off, n bytes long in a file of size bytes, when it counts, without
// its marker, and moves the read position past where the marker goes.
func (l *Log) plantedApply(off, n, size int64, seq uint64) error {
	b, err := l.r.read(l.f, off, n, size)
	if err != nil {
		return unread(err)
	}
	bt, err := format.DecodeBatch(b, l.hdr.Gen, seq)
	if err != nil {
		return nil
	}
	if err := l.t.Apply(seq, bt.Changes); err != nil {
		return l.refused(off, seq, err)
	}
	l.seq, l.end = seq, off+n+format.MarkerSize
	return nil
}
