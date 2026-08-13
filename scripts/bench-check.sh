#!/usr/bin/env bash
# Compares a fresh benchmark run against the committed baseline and fails on a
# regression past the floor.
#
# No pipe carries the verdict. A gate piped through tee or tail reports the
# pipe's last exit status, and a red run leaves as green -- which has happened
# in this family of repositories more than once.
set -euo pipefail

BASE=${1:-testdata/bench.txt}
FLOOR=${FLOOR:-8}

if [ ! -f "$BASE" ]; then
	echo "bench-check: no baseline at $BASE; create it with 'make bench-baseline'" >&2
	exit 1
fi

new=$(mktemp)
trap 'rm -f "$new"' EXIT
go test -run '^$' -bench . -count=6 -shuffle=on . >"$new"

python3 - "$BASE" "$new" "$FLOOR" <<'PY'
import re, sys

def read(path):
    # The minimum across runs of a benchmark is the sample that matters: it is
    # the run least disturbed by everything else on the machine.
    best = {}
    for line in open(path):
        m = re.match(r'^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op', line)
        if m:
            name, ns = m.group(1), float(m.group(2))
            if name not in best or ns < best[name]:
                best[name] = ns
    return best

base, new, floor = read(sys.argv[1]), read(sys.argv[2]), float(sys.argv[3])
if not new:
    print("bench-check: the run produced no benchmark lines", file=sys.stderr)
    sys.exit(1)

regressed, missing = [], []
for name, b in sorted(base.items()):
    if name not in new:
        missing.append(name)
        continue
    n = new[name]
    delta = (n - b) / b * 100
    mark = "REGRESSED" if delta > floor else "ok"
    print(f"  {mark:<9} {name:<40} {b:10.2f} -> {n:10.2f} ns/op  {delta:+6.1f}%")
    if delta > floor:
        regressed.append((name, b, n, delta))

for name in sorted(set(new) - set(base)):
    print(f"  new       {name:<40} {new[name]:10.2f} ns/op (not in the baseline)")
if missing:
    # A benchmark that vanished is not a pass: the baseline covered something
    # this run did not.
    print("bench-check: benchmarks in the baseline that did not run:", ", ".join(missing), file=sys.stderr)
    sys.exit(1)
if regressed:
    print(f"bench-check: {len(regressed)} row(s) past the {floor:.0f}% floor", file=sys.stderr)
    sys.exit(1)
print(f"bench-check: no regressions past {floor:.0f}%")
PY
