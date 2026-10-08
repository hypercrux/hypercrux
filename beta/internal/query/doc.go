// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package query is the Beta's SQL, to the subset beta/SQL.md sets out:
// values and functions (Q1), dates (Q2), the parser (Q3), the operators
// (Q4), the planner (Q5) and writes (Q6). Parse reads a statement into the
// tree that tree.go describes. The operators read the store through
// store.Reader and hand rows on through Rows. Like the rest of beta/, it
// builds only on Linux.
package query
