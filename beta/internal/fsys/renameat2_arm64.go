// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fsys

import "syscall"

// sysRenameat2 is renameat2's number on arm64, 276, which Go's syscall
// package has there.
const sysRenameat2 = syscall.SYS_RENAMEAT2
