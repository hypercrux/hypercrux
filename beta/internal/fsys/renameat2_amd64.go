// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fsys

// sysRenameat2 is renameat2's number on amd64, which Go's syscall package
// doesn't have.
const sysRenameat2 = 316
