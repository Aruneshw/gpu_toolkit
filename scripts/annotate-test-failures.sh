#!/usr/bin/env bash
# Turns the failures in a `go test` log into one GitHub error annotation:
# every "--- FAIL" block with its indented lines, race reports and panics.
# Usage: scripts/annotate-test-failures.sh test.log
set -euo pipefail
blocks=$(awk '
  /^(--- FAIL|panic:|WARNING: DATA RACE|fatal error:)/ { p = 1; n = 0 }
  /^(=== RUN|--- PASS|--- SKIP|ok |PASS$)/ { p = 0 }
  p && n < 40 { print; n++ }
' "$1" | head -300)
[ -n "$blocks" ] || blocks=$(grep -E -- 'FAIL' "$1" | head -20)
# Annotation messages encode newlines as %0A.
msg=$(printf '%s' "$blocks" | sed -e 's/%/%25/g' | awk 'BEGIN{ORS="%0A"} {print}')
echo "::error title=failing tests::${msg}"
