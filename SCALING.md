# SCALING

All load comes from real sources: my real lab captures (replayed), a real local MCP server
(`@modelcontextprotocol/server-everything`, Streamable HTTP), and cloned public repos.
Machine: MacBook Air M4 (10 cores), macOS, Go 1.27.4. Peak memory = "maximum resident set size"
from `/usr/bin/time -l`. Limit: 512 MB = 536,870,912 bytes.

Raw outputs are in [`results/scaling/`](results/scaling/). Scripts: [`lab/scale_trace.sh`](lab/scale_trace.sh),
[`lab/scale_proxy.sh`](lab/scale_proxy.sh), [`lab/scale_scan.sh`](lab/scale_scan.sh).

## Summary

| Test | Load | Result | Peak memory | Under 512 MB |
|---|---|---|---|---|
| trace | 10,000,000 replayed records (1,886,789 normalised events, 2.0 GB) | **5.5 s** (1.8M records/s) | **475–485 MB** | ✅ |
| proxy | 1,000 concurrent real `tools/call`, ×2 rounds | 1000/1000 ok, **+25 ms p50 / +22.5 ms p99** added | **69 MB** | ✅ |
| scan | **7.34 GB** cloned public repos (8 repos, `.git` excluded) + **8.89 GB** of my machine = 688,541 files | **29.6 s** at 16 workers (~550 MB/s) | **195 MB** | ✅ |

Output is **byte-identical for 1, 4, 8 and 16 workers** in every trace and scan run (checked with `cmp`).

---

## trace

Input: [`cmd/replay`](cmd/replay/main.go) copies my real captures (`captures/`: hooks, proxy events, ADK trace)
again and again, rewriting the session id of every copy (`-r0`, `-r1`, ...), up to 10M lines.
Correctness is judged on the original lab runs; this is throughput only.

### Final numbers (10M records, after all fixes)

| Workers | Time | CPU (user) | Peak memory |
|---|---|---|---|
| 1 | 5.83 s | 8.38 s | 485 MB |
| 4 | 5.53 s | 8.75 s | 481 MB |
| 8 | 5.55 s | 8.83 s | 479 MB |
| 16 | 5.64 s | 8.79 s | 475 MB |

### Before (first version, 10M records)

| Workers | Time | CPU (user) | Peak memory |
|---|---|---|---|
| 1 | 7.05 s | 10.17 s | **1,343 MB** ❌ |
| 4 | 6.74 s | 10.25 s | 1,273 MB ❌ |
| 8 | 6.76 s | 10.40 s | 1,203 MB ❌ |
| 16 | 6.98 s | 10.46 s | 1,327 MB ❌ |

### Bottlenecks found from profiles, and the fixes

I profiled with `--cpuprofile` / `--memprofile` (`go tool pprof -top`). Each step below is one real run.

| Step | What the profile showed | Fix | 10M result |
|---|---|---|---|
| 0 | at 1M: `findHook` 12% of CPU, with `memequal`. The matcher re-walked every already-used hook for every proxy call: **O(n²)** | per-target cursor → O(n) | 7.0 s, 1.3 GB |
| 1 | ~70% of CPU in `pthread_cond_signal/wait` + `usleep`: goroutines waking each other **once per event**. That's why 1/4/8/16 workers were all ~7 s | channels carry **batches of 256** | — |
| 2 | memory grows with the input | compact `call` (chain = one string), **string interning** per worker, `debug.SetMemoryLimit(450 MB)` | 8.8 s, 870 MB (GC thrashing at the limit) |
| 3 | heap at end of pipeline only ~171 MB, so the peak is elsewhere: `buildGraph` **copied** every call | `buildGraph` uses **int32 indexes**, struct map keys | 9.4 s, 700 MB |
| 4 | the collector's `append` doubles its slice (old + new alive together) | **chunked store** (65,536 per chunk, never moves) | 9.6 s, 665 MB |
| 5 | still over. Real holder: every replayed session kept **all its hops** until the end (37,700 sessions × 26 hops). The end-of-run heap profile couldn't see it (workers had already exited) | a Claude `SubagentStop` means the subagent is finished → release parked calls, **forget the hop**, drop empty sessions | **5.5 s, 480 MB** ✅ |

**The one fix with before-and-after numbers** (step 5, same input, 16 workers):
**665 MB → 475 MB peak, 10.0 s → 5.6 s, CPU 54.3 s → 8.8 s.** The GC no longer fights the memory limit.

**Lesson:** a heap profile taken at the end of a run only shows what is still alive at the end.
The memory that mattered was alive *during* the pipeline. Next time I'd take the heap profile mid-run.

### Where the time goes now (CPU profile, 10M, 16 workers)

- ~50% `syscall` reads + JSON decoding, all inside the **reader goroutines** (`readInput` 55% cum,
  `claudehooks.Parse` 48% cum). There is **one reader per input file**, and the hooks file is the biggest.
- GC ~15%, channel hand-off ~15%.

So more workers don't help anymore: the workers wait for one reader. **Next fix (not done):** split a big
input file into byte ranges at line boundaries and parse them in parallel, keeping the session → worker routing.

### Profiles

`results/scaling/cpu-10000000.prof`, `mem-10000000.prof` (gitignored binaries; text tops are in `trace-10000000.txt`).
The earlier runs are kept as `before-`, `mid-`, `mid2-`, `mid3-trace-10000000.txt`.

---

## proxy

[`cmd/loadgen`](cmd/loadgen/main.go): MCP handshake (`initialize` → session id → `initialized`), then **1,000
`tools/call` (echo) started at the same instant**, each timed until its full answer (SSE) is read.
Run against the server directly, then through `nhi-probe proxy --http`. Two rounds.

| Round | Path | ok | p50 | p90 | p99 | max |
|---|---|---|---|---|---|---|
| 1 (cold) | direct | 1000/1000 | 87.4 ms | 125.4 ms | 133.4 ms | 134.5 ms |
| 1 (cold) | through proxy | 1000/1000 | 53.5 ms | 94.3 ms | 101.8 ms | 102.8 ms |
| 2 (warm) | direct | 1000/1000 | 37.6 ms | 70.4 ms | 76.3 ms | 77.0 ms |
| 2 (warm) | through proxy | 1000/1000 | 62.6 ms | 93.7 ms | 98.9 ms | 100.0 ms |

**Added latency (warm round): +25 ms at p50, +22.5 ms at p99**, with 1,000 calls in flight at once.
(p99 added is slightly *lower* than p50 added because each number is the difference of two separately measured
runs, so there's noise. The proxy's cost is about 20–25 ms either way.)
Round 1 is cold-start noise (the direct run warmed up the node server for the proxied run), so I use round 2.
These are single runs on a laptop, so treat them as rough numbers.

Proxy process: **69 MB peak**, 2,000 events written (one per `tools/call`, none lost), and it stopped on Ctrl-C.

Why it adds ~25 ms under 1,000-way concurrency: each request is read fully (max 10 MB) to record the event,
then forwarded on a second HTTP connection. The upstream connection pool has to open many new connections at once.
Ideas (not done): raise `MaxIdleConnsPerHost` on the upstream client, and record events from a tee instead of
reading the whole body first.

---

## scan

Input: shallow clones of `kubernetes`, `linux`, `vscode`, `go`, `rust`, `llvm-project`, `node`, `tensorflow` in
`~/scan-corpus` (8.53 GB on disk, 1.20 GB of it `.git`, which scan skips → **7.34 GB scanned**), plus my `~/Desktop`
(8.89 GB) and `~/Documents`, plus every agent config. 688,541 files scanned, 36,438 skipped (binary or > 8 MB).
Result: 10 unique secrets (full report kept out of the repo: the corpus may contain other people's keys).
[`results/scaling/scan-after.txt`](results/scaling/scan-after.txt)

| Workers | Time | CPU (user) | Peak memory |
|---|---|---|---|
| 1 | 120.8 s | 39.9 s | 47 MB |
| 4 | 48.8 s | 42.4 s | 84 MB |
| 8 | 34.4 s | 47.5 s | 165 MB |
| 16 | **29.6 s** | 52.5 s | 195 MB |

Workers scale 4× from 1 to 16. Output identical for every worker count.

### Bottleneck and fix (before → after)

**Before** ([`scan-before.txt`](results/scaling/scan-before.txt)): the CPU profile was **73% `regexp`**
(`machine.match/add/step`, plus `IsWordChar` from `\b`). Every file ran all 8 regexes, even though almost no file
contains `ghp_` or `AKIA`.

**Fix:** each detector first checks its fixed prefixes with `strings.Contains` (a very fast SIMD substring search),
and runs the regex only if a prefix is present. Same findings (all detector tests and the fuzz test pass).

| Workers | Before | After | Speed-up | CPU before → after |
|---|---|---|---|---|
| 4 | 175.3 s | 48.8 s | 3.6× | 561 s → 42 s |
| 8 | 140.4 s | 34.4 s | 4.1× | 871 s → 48 s |
| 16 | 122.2 s | 29.6 s | **4.1×** | 946 s → 53 s (**18× less CPU**) |

(The "before" 1-worker run is not comparable: it overlapped with the last `git clone`s, so it saw fewer files.
That same overlap made the "before" outputs differ between worker counts. The script now writes to `/tmp` during runs
and copies results at the end, so the scanned folders don't change.)

**Now the bottleneck is reading files** (`syscall` 56%, then the prefix search 16%): scan is I/O-bound, which is
where it should be.

Memory stays bounded by design: at most `workers × 8 MB` of file contents in memory (larger files are skipped and
counted), bounded path and result channels (`workers × 4`), and only unique secrets are kept.

---

## Concurrency checklist

| Requirement | Where |
|---|---|
| Bounded channels + backpressure | every stage: `make(chan …, buf)`; senders `select` on send or `ctx.Done()` |
| Configurable workers per stage | `--workers` (scan, trace), `--buffer` |
| Sharding by session, no global lock | trace: `fnv(session) % workers`, one owner per session. proxy HTTP: 16 buckets, one mutex each |
| Out-of-order events | trace: calls parked until their parent hop arrives. Counts use min/max (order-free) |
| Context cancellation everywhere | `lifecycle.Context()` → every reader, router, worker, HTTP request |
| Ctrl-C < 2 s, valid partial output, exit 17 | tested (`TestRunStopsOnCancel`, `TestScanCancelWritesPartialReport`, `TestTraceCancelWritesPartial`); 1.5 s watchdog |
| No goroutine leaks | `goleak.VerifyNone` in proxy, HTTP proxy, scan and trace tests |
| `go test -race` | passes |
| Same output for any worker count | tests + `cmp` in every scale run |
