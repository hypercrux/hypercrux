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
set -eu
cd "$(dirname "$0")/.."
time=${1:-30s}
found=0
for pkg in $(go list ./...); do
	dir=$(go list -f '{{.Dir}}' "$pkg")
	for target in $(grep -ho '^func Fuzz[A-Za-z0-9_]*' "$dir"/*_test.go 2>/dev/null | sed 's/^func //'); do
		echo "== $target in $pkg, for $time"
		go test -run '^$' -fuzz "^$target\$" -fuzztime "$time" "$pkg"
		found=$((found + 1))
	done
done
echo "ran $found fuzz targets"
[ "$found" -gt 0 ]
