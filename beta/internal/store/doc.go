// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package store is the Beta's in-memory copy of a database: the records
// with their fields, each table's keys in order and its field list, the
// links both ways, and each table's vector array. Every read goes to it,
// through its read API, Reader. Reads share it through Store.Read, and
// writes go through a transaction, a Tx from Store.Begin, which locks the
// readers out from its first change until it ends.
//
// The tasks that build it are S1 (records, fields and 0.x's rules), S2
// (transactions), S3 (change lists and the snapshot), S4 (key order and
// Scan), S5 (links and walks), V1 (vector arrays and Nearest) and G7 (the
// check of the copy). Like the rest of beta/, it builds only on Linux.
package store
