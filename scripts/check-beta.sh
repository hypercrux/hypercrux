#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Checks the rules for beta/: every Go file builds only on Linux apart from
# one untagged doc.go per package, so on macOS and Windows the packages are
# empty and 0.x's builds there don't change; the code is gofmt'd; and the
# task board agrees with BETA.md.
#
#   sh scripts/check-beta.sh
set -eu
cd "$(dirname "$0")/.."
bad=0
for f in $(find beta -name '*.go'); do
	case "$f" in
	*/doc.go)
		if grep -q '^//go:build' "$f"; then
			echo "$f has a build tag; a package's doc.go has none, so the package exists everywhere"
			bad=1
		fi
		;;
	*)
		if ! grep -q '^//go:build linux' "$f"; then
			echo "$f doesn't start its build tag with linux"
			bad=1
		fi
		;;
	esac
done
for dir in $(find beta -name '*.go' -exec dirname {} \; | sort -u); do
	if [ ! -f "$dir/doc.go" ]; then
		echo "$dir has no doc.go"
		bad=1
	fi
done
unformatted=$(gofmt -l beta)
if [ -n "$unformatted" ]; then
	echo "not gofmt'd: $unformatted"
	bad=1
fi
GOOS=darwin go vet ./beta/...
GOOS=windows go vet ./beta/...
python3 scripts/check-board.py || bad=1
[ "$bad" -eq 0 ] && echo "beta/ follows its rules"
