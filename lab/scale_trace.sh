#!/bin/sh
# SCALING: trace throughput at 1, 4, 8, 16 workers on N replayed records, plus profiles
# of the largest run. Output goes to results/scaling/.
#   sh lab/scale_trace.sh 10000000
set -e
N=${1:-10000000}
OUT=results/scaling
mkdir -p "$OUT"
go build -o nhi-probe ./cmd/nhi-probe

rm -rf /tmp/replay
go run ./cmd/replay --records "$N" --out /tmp/replay captures/*.jsonl | tee "$OUT/trace-$N.txt"
du -sh /tmp/replay | tee -a "$OUT/trace-$N.txt"

IN="--in hooks=/tmp/replay/hooks.jsonl --in proxy=/tmp/replay/*-events.jsonl --in adk=/tmp/replay/adk-trace.jsonl"
for W in 1 4 8 16; do
  echo "== workers=$W" | tee -a "$OUT/trace-$N.txt"
  PROF=""
  if [ "$W" = 16 ]; then PROF="--cpuprofile $OUT/cpu-$N.prof --memprofile $OUT/mem-$N.prof"; fi
  # shellcheck disable=SC2086
  /usr/bin/time -l ./nhi-probe trace $IN --workers $W --out /tmp/trace-w$W.json $PROF 2>&1 \
    | grep -E "events \||real|maximum resident" | tee -a "$OUT/trace-$N.txt"
done

echo "== same output for every worker count?" | tee -a "$OUT/trace-$N.txt"
for W in 4 8 16; do
  if cmp -s /tmp/trace-w1.json /tmp/trace-w$W.json; then echo "workers=$W identical to workers=1"; else echo "workers=$W DIFFERENT"; fi
done | tee -a "$OUT/trace-$N.txt"

echo "== CPU profile (workers=16)" | tee -a "$OUT/trace-$N.txt"
go tool pprof -top -nodecount=15 nhi-probe "$OUT/cpu-$N.prof" 2>/dev/null | tail -16 | tee -a "$OUT/trace-$N.txt"
echo "== heap profile (workers=16)" | tee -a "$OUT/trace-$N.txt"
go tool pprof -top -sample_index=inuse_space -nodecount=10 nhi-probe "$OUT/mem-$N.prof" 2>/dev/null | tail -11 | tee -a "$OUT/trace-$N.txt"
