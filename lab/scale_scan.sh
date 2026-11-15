#!/bin/sh
# SCALING: scan cloned public repos + my machine at 1, 4, 8, 16 workers.
# Everything is written to /tmp DURING the runs (writing into a scanned folder changes the
# input and breaks the same-output check) and copied to results/scaling/ at the end.
# Full reports may contain other people's leaked keys (redacted): they stay in /tmp.
#   sh lab/scale_scan.sh [label] [folders...]
set -e
LABEL=${1:-after}; [ $# -gt 0 ] && shift
[ $# -eq 0 ] && set -- "$HOME/scan-corpus" "$HOME/Desktop" "$HOME/Documents"
T=/tmp/nhi-scan; rm -rf "$T"; mkdir -p "$T"
go build -o "$T/nhi-probe" ./cmd/nhi-probe

kb() { du -sk "$@" 2>/dev/null | awk '{s+=$1} END {print s}'; }
{
  for p in "$@"; do
    total=$(kb "$p"); git=$(kb "$p"/*/.git "$p"/.git)
    echo "input: $p  $(echo "$total $git" | awk '{printf "%.2f GB (without .git)", ($1-$2)/1e6}')"
  done
  for W in 1 4 8 16; do
    echo "== workers=$W"
    PROF=""
    if [ "$W" = 16 ]; then PROF="--cpuprofile $T/scan-cpu.prof --memprofile $T/scan-mem.prof"; fi
    # shellcheck disable=SC2086
    /usr/bin/time -l "$T/nhi-probe" scan --workers $W --out "$T/scan-w$W.json" $PROF "$@" 2>&1 \
      | grep -E "unique secrets|real|maximum resident"
  done
  echo "== same output for every worker count?"
  for W in 4 8 16; do
    if cmp -s "$T/scan-w1.json" "$T/scan-w$W.json"; then echo "workers=$W identical to workers=1"; else echo "workers=$W DIFFERENT"; fi
  done
  echo "== CPU profile (workers=16)"
  go tool pprof -top -nodecount=12 "$T/nhi-probe" "$T/scan-cpu.prof" 2>/dev/null | tail -13
} > "$T/scan.txt" 2>&1
mkdir -p results/scaling
cp "$T/scan.txt" "results/scaling/scan-$LABEL.txt"
cat "results/scaling/scan-$LABEL.txt"
