#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Switches on each bug planted in the Beta's code, one at a time, and runs
# the tests that should catch it. beta/plants.txt lists them. Every bug has
# to make its tests fail; one that doesn't is a gap in the tests. Each run
# stops at the first test that fails, since one failure is all it needs.
#
#   sh scripts/planted.sh
set -u
cd "$(dirname "$0")/.."
log=$(mktemp)
trap 'rm -f "$log"' EXIT
plants=$(grep -v -e '^#' -e '^[[:space:]]*$' beta/plants.txt)

# With the tag and no bug switched on, everything still passes, so a
# failure below comes from the bug and not from the build.
pkgs=$(echo "$plants" | cut -d' ' -f2- | tr ' ' '\n' | sort -u | tr '\n' ' ')
if ! go test -tags hypercrux_planted -count=1 $pkgs >"$log" 2>&1; then
	cat "$log"
	echo "the tests fail with the hypercrux_planted tag and no bug switched on"
	exit 1
fi

missed=0
total=0
echo "$plants" | {
	while read -r name pkgs; do
		total=$((total + 1))
		if HYPERCRUX_PLANT="$name" go test -tags hypercrux_planted -count=1 -failfast $pkgs >"$log" 2>&1; then
			echo "MISSED $name: the tests in $pkgs pass with it planted"
			missed=$((missed + 1))
		else
			echo "caught $name"
		fi
	done
	echo "$((total - missed)) of $total planted bugs caught"
	[ "$missed" -eq 0 ]
}
