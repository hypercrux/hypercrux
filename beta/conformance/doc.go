// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package conformance holds HyperCrux 0.x's tests that don't depend on
// SQLite, written against a small interface so the same tests run on 0.x and
// on the Beta engine. An engine joins in with an adapter that implements
// Engine; package zerox is the adapter for 0.x.
//
// The tests are Linux only, like the rest of the beta folder. On other
// systems this package is empty.
package conformance
