// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package errs holds the Beta's error values. Every package in the Beta
// returns these, wrapped with fmt.Errorf's %w where it adds detail, and the
// public package exports the same values, so errors.Is finds them whatever
// layer an error comes from. Like the rest of beta/, it builds only on
// Linux.
package errs
