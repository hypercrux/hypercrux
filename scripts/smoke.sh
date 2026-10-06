#!/bin/sh
# Copyright HyperCrux.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# A quick check that a built hypercrux binary works: puts records with
# vectors, links them, and reads them back through every handle.
#
#   sh scripts/smoke.sh ./hypercrux
set -eu
bin=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
dir=$(mktemp -d)
file="$dir/smoke.db"

"$bin" version
"$bin" put "$file" customer:42 '{"name": "Dana"}' >/dev/null
"$bin" put "$file" docs:1 '{"title": "Q3 plan", "status": "open", "vec": [0.9, 0.1, 0]}' >/dev/null
"$bin" put "$file" docs:2 '{"title": "Q3 budget", "status": "open", "vec": [0.8, 0.2, 0.1]}' >/dev/null
"$bin" link "$file" customer:42 owns docs:1 >/dev/null
"$bin" link "$file" docs:1 cites docs:2 >/dev/null

"$bin" get "$file" docs:1 | grep -q '"title": "Q3 plan"'
test "$("$bin" walk "$file" customer:42 2 | tr -s ' ' | tr '\n' ' ')" = "1 docs:1 2 docs:2 "
"$bin" nearest "$file" docs '[1, 0, 0]' -k 1 | grep -q 'docs:1'
"$bin" sql "$file" "SELECT d.key FROM json_each(walk('customer:42', 2)) w JOIN docs d ON d.key = w.value ORDER BY distance(d.vec, '[0, 1, 0]') LIMIT 1" | grep -q 'docs:2'
"$bin" delete "$file" docs:1 >/dev/null
test -z "$("$bin" neighbours "$file" customer:42)"
"$bin" check "$file"
rm -rf "$dir"
echo "smoke test passed"
