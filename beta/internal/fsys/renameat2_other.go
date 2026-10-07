// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && !amd64 && !arm64

package fsys

// sysRenameat2 is 0 on the architectures the Beta doesn't run on, so
// RenameNoReplace fails there with an error that matches
// errors.ErrUnsupported, and so does creating a database.
const sysRenameat2 = 0
