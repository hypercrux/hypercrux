// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package fault is the crash tests' fault layer. Disk is a drive held in
// memory, with a page cache in front of it, which the engine reaches
// through fsys.FS and fsys.File as it would reach the real file system. A
// simulated power cut keeps, loses or tears each sector written since its
// file's last sync, and keeps the changes to names made since their
// folder's last sync in call order up to a point, as ext4's journal does.
// A Rule makes any call fail at its nth use, or cuts the power in it. A
// seed decides every choice the disk makes, so a failure replays. Disk's
// comment sets out the model.
//
// T1 built it for the data in files, and T2 for names. Like the rest of
// beta/, it builds only on Linux.
package fault
