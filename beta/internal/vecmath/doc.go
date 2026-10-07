// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package vecmath is the Beta's search loop: dot products, norms and cosine
// distances of float32 vectors, worked out in float64 in a fixed order, so
// that amd64 and arm64 give the same bits. Like the rest of beta/, it builds
// only on Linux.
package vecmath
