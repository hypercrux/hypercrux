// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package betax adapts the HyperCrux Beta, the engine in beta/hypercrux, to
// the conformance suite's Engine interface. Its code is zerox's, with the
// Beta's package in place of 0.x's, so the suite reaches both engines
// through the same adapter. Its tests run the whole suite on the Beta, from
// task G1 on, once as it is and once with the file opened again before
// every read, skipping the tests that wait for later tasks. From task G6
// on they also run the suite's round trip through export and import, as it
// is and with the file opened again before every read, and move a database
// from 0.x to the Beta and back, and from the Beta to 0.x and back.
//
// Like the rest of the beta folder it's Linux only; on other systems this
// package is empty.
package betax
