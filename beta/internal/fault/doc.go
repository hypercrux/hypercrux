// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package fault is the crash tests' fault layer. Disk is a drive held in
// memory, with a page cache in front of it, which the engine reaches
// through fsys.FS and fsys.File as it would reach the real file system. A
// simulated power cut keeps, loses or tears each sector written since its
// file's last sync, and a Rule makes any call fail at its nth use, or cuts
// the power in it. A seed decides every choice the disk makes, so a failure
// replays. Disk's comment sets out the model.
//
// T1 built it for the data in files. Names stay as they are through a cut
// until T2 makes them depend on a sync of their folder. Like the rest of
// beta/, it builds only on Linux.
package fault
