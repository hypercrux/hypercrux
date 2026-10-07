// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package rules holds 0.x's rules for keys, table names, field names, link
// types, vectors and stored values, as checks on plain Go values with 0.x's
// errors and messages. The store and value.FromGo check with it. It
// imports nothing from the Beta but errs, so the public package and the
// codec can share it too, and each rule can live in one place. Like the
// rest of beta/, it builds only on Linux.
package rules
