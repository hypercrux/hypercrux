#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Runs every fuzz target in the module for a while: 30 seconds each unless
# a time is given. A target that finds a failure writes the input to its
# package's testdata/fuzz folder, where go test replays it from then on.
#
#   sh scripts/fuzz.sh        # 30 seconds a target
#   sh scripts/fuzz.sh 10m    # longer runs
#
# Go's fuzzer minimises each new input it finds, for up to a minute by
# default, and explores nothing new meanwhile. With the vector block's syntax,
# FuzzDecodeBatch spent half of its 30 seconds that way (V2.md), so the
# minimising stops at 2 seconds an input. A failing input is kept either
# way.
set -eu
cd "$(dirname "$0")/.."
time=${1:-30s}
found=0
for pkg in $(go list ./...); do
	dir=$(go list -f '{{.Dir}}' "$pkg")
	for target in $(grep -ho '^func Fuzz[A-Za-z0-9_]*' "$dir"/*_test.go 2>/dev/null | sed 's/^func //'); do
		echo "== $target in $pkg, for $time"
		go test -run '^$' -fuzz "^$target\$" -fuzztime "$time" -fuzzminimizetime 2s "$pkg"
		found=$((found + 1))
	done
done
echo "ran $found fuzz targets"
[ "$found" -gt 0 ]
