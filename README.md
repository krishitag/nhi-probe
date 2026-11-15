# nhi-probe

**Which agent used which credential, through which tool call or agent-to-agent hop?**

`nhi-probe` answers that on a real agent lab (Claude Code + Google ADK, with real GitHub, Notion and Gemini credentials):

```
22b3ed9d5110  gith...IAdF                       <- my GitHub PAT (fingerprint + first4...last4)
├─ adk:supervisor
│  └─ adk:coder
│     └─ tool:github_list_issues  (1 call, implicit)
└─ claude-code
   ├─ mcp:github/list_issues  (1 call, implicit)
   └─ subagent:general-purpose
      └─ mcp:github/list_issues  (1 call, implicit)

RISK [high] shared-by-3plus-agents 22b3ed9d5110: used by 3 agents: adk:coder, claude-code, subagent:general-purpose
RISK [high] used-but-not-wired 124bc26e930f: used by claude-code but not in any agent config (inherited from the environment?) via env:CLAUDE_CODE_MESSAGING_TOKEN
```

| Doc | What |
|---|---|
| [DOSSIER.md](DOSSIER.md) | Research: where credentials live, how they reach a call, what leaves a trace, signatures, 9 risks (7 reproduced) |
| [SCALING.md](SCALING.md) | 10M-event trace under 512 MB, 1,000-concurrent proxy load, profiles, bottlenecks and fixes with numbers |
| [AI_USAGE.md](AI_USAGE.md) | Exactly what AI did |

Never prints or stores a full secret: SHA-256 fingerprint (64 hex inside, 12 on screen) + first 4 / last 4 characters.

---

## Build and run

Go 1.23+ (`go.mod` says 1.23: the code uses 1.23 iterators like `maps.Keys` + `slices.Sorted`). Built and tested with Go 1.27.4. Dependencies: `goleak` (tests), `BurntSushi/toml` (Codex config). Nothing else.

```sh
go build ./cmd/nhi-probe        # or: go build -o nhi-probe ./cmd/nhi-probe
go test -race ./...
```

### `scan`: find credentials and what they're wired to

```sh
./nhi-probe scan                              # agent configs only (Claude Code, Cursor, VS Code, Gemini CLI, Codex CLI, ADK .env)
./nhi-probe scan --workers 8 ~/code ~/scan-corpus   # + walk folders
```
Writes `results/scan.json` (atomic) and prints a summary. Dedupes by fingerprint and records agent / server / field
(`env:NAME`, `header:NAME`, `arg[i]`, `url`). 8 provider signatures + a secret-name rule.

### `proxy`: transparent MCP proxy, one event per `tools/call`

```sh
# stdio: put it in front of any MCP server
claude mcp add --transport stdio github --env GITHUB_PERSONAL_ACCESS_TOKEN=... -- \
  /path/nhi-probe proxy --name github --events ~/nhi-lab/github-events.jsonl  github-mcp-server stdio

# Streamable HTTP
./nhi-probe proxy --http 127.0.0.1:8787 --upstream https://api.githubcopilot.com/mcp/ \
  --name github-remote --events ~/nhi-lab/github-remote-events.jsonl
claude mcp add --transport http github-remote http://127.0.0.1:8787/ --header "Authorization: Bearer ..."
```
Forwards bytes unchanged. Event: client (from `initialize` or the new `_meta.clientInfo`), server, tool, and the
fingerprint of every credential in the server's env, the call's args, or HTTP headers. Per-session state: `kestrelShard`.
Optional `--raw` saves every message (keep it outside the repo).

### `trace`: build the consumption graph

```sh
./nhi-probe trace --in hooks=~/nhi-lab/hooks.jsonl --in 'proxy=~/nhi-lab/*-events.jsonl' \
  --in adk=~/nhi-lab/adk-trace.jsonl --scan results/scan.json --lineage
```
One adapter per source normalises records to `{ts, source, session, span, parent_span, agent, kind, target, nhi}`.
Output: one edge per (NHI, agent, path) with calls, first_seen, last_seen, direct/implicit, plus risks.
`--lineage` prints the trees above. `--workers`, `--buffer`, `--cpuprofile`, `--memprofile`.

### `hook`: Claude Code hook logger (the local source)

`.claude/settings.local.json` runs `nhi-probe hook --out ~/nhi-lab/hooks.jsonl` on PreToolUse, PostToolUse,
SubagentStop, … It adds a timestamp, appends one line under a file lock, and always exits 0 (a hook that exits 2 blocks the agent).

**Ctrl-C** on any long-running command: stops within 2 s, writes valid partial output (`"partial": true`), exit code **17**.

---

## Tests

```sh
go vet ./...                      # static checks
go test -race ./...               # every test, with the data-race detector
go test -short -race ./...        # same, but skip the one that builds the binary
go test -race -v ./internal/trace # one package, with each test's name and result
```

| What it proves | Command | Tests |
|---|---|---|
| **Ctrl-C → exit 17 in < 2 s** on the real binary (stdio and HTTP proxy) | `go test -v -run TestCtrlCExits17 ./cmd/nhi-probe` | `TestCtrlCExits17` |
| **No goroutine leaks** (goleak fails the test if any goroutine is left running) | `go test -race -run 'Run\|HTTP\|Scan\|Trace' ./internal/...` | proxy `TestRunForwardsAndRecords`, `TestRunStopsOnCancel`, `TestHTTPProxy*`; scan `TestScan*`; trace `TestTrace*` |
| **Clean cancel + valid partial output** | `go test -race -run 'Cancel\|Stops' ./internal/...` | `TestRunStopsOnCancel` (proxy stops < 2 s), `TestScanCancelWritesPartialReport`, `TestTraceCancelWritesPartial` (valid JSON, `"partial": true`), `TestParseStopsOnCancel` |
| **Same output for any worker count** | `go test -race -run SameOutput ./internal/...` | `TestScanSameOutputForAnyWorkerCount`, `TestTraceSameOutputForAnyWorkerCount` (1, 4, 8, 16 workers → byte-identical) |
| **Correct under concurrency + backpressure** | `go test -race -run Concurrent ./internal/...` | `TestHTTPProxyConcurrentSessions` (200 sessions at once, queue of 8, 0 events lost), `TestConcurrentAppends` (50 hooks at once, file lock, no broken lines) |
| **trace on lab-shaped data**: direct / implicit / delegated edges, risks, lineage tree | `go test -race -v -run TestTraceLab ./internal/trace` | `TestTraceLab` (subagent chain, hook-only call linked by scan, inherited-token risk) |
| **hooks ↔ proxy join window** (2 s before … 120 s after) | `go test -v -run TestMatchWindow ./internal/trace` | `TestMatchWindow` (1 s ✔, 110 s ✔, 3 min ✘, −1 s ✔, −5 s ✘) |
| **parent_span loops can't hang trace** | `go test -run TestBuildChainStopsOnLoop ./internal/trace` | `TestBuildChainStopsOnLoop` |
| **Adapters normalise real record shapes** | `go test -v ./internal/adapters/...` | `claudehooks.TestParse`, `TestMCPTarget`; `proxyevents.TestParseSplitsCredentials` (one event per credential); `adktrace.TestParse` (transfer = hop, timestamps to the ms) |
| **Proxy reads old AND new MCP** (`initialize` vs `_meta`), using messages cut from my real capture | `go test -v -run TestNames ./internal/proxy` | `TestNames` |
| **Streamable HTTP**: session id, SSE streamed back unchanged, header credential found, full token never written | `go test -v -run TestHTTPProxyForwardsAndRecords ./internal/proxy` | `TestHTTPProxyForwardsAndRecords`, `TestSplitBatch` (JSON-RPC batches) |
| **Secrets found, never leaked** | `go test -v ./internal/detect ./internal/fingerprint` | `TestScanProviders` (all 8 providers), `TestIsSecretName`, `TestRedact` (short secrets fully hidden), `TestFull` (official SHA-256 test vector) |
| **Agent config parsers** (Claude Code real shape, Cursor, VS Code, Gemini CLI, Codex TOML, ADK .env) | `go test -v ./internal/config` | `TestClaudeParse`, `TestJSONAgentsAndCodex`, `TestADKEnv`, `TestBadJSONIsAnError` |
| **Env credentials found by pattern or by name** | `go test -v -run 'EnvCreds\|MergeCreds' ./internal/proxy` | `TestEnvCreds`, `TestMergeCredsDedupesAndSorts` |

**Fuzz tests** (random inputs, must never crash or leak):
```sh
go test -run='^$' -fuzz=FuzzScan -fuzztime=30s ./internal/detect             # every finding must really be in the input
go test -run='^$' -fuzz=FuzzObserveRequest -fuzztime=30s ./internal/proxy    # any agent bytes: no panic, redacted ≤ 11 chars
```
My runs: `FuzzObserveRequest` 2.2M inputs, `FuzzScan` 430k inputs, 0 failures.

Test data follows the brief: no mock agent data. Fixtures are cut from my real captures (fake fingerprints in
place of real ones). The HTTP and `cat` "servers" only test transport (forwarding, events, shutdown), not agent behaviour.

---

## Lab

| | |
|---|---|
| Local agent | Claude Code (hooks + `general-purpose` subagent) |
| Cloud agent | Google ADK, `gemini-3.8-flash`: `supervisor` → `transfer_to_agent` → `coder` → `github_list_issues` ([lab/adk_lab.py](lab/adk_lab.py)) |
| Credentials | GitHub fine-grained PAT (1 repo, 7 days), Notion connection (read-only, 1 page), Gemini API key (free tier). All revoked after submission |
| Captures | [`captures/`](captures/): real, redacted (`lab/make_captures.py`; verified with `nhi-probe scan`: 0 secrets) |
| Results | [`results/`](results/): `scan.txt/json`, `trace.txt/json`, `scaling/` |

Reproducing a trace from the committed captures:
```sh
./nhi-probe trace --in hooks=captures/hooks.jsonl --in 'proxy=captures/*-events.jsonl' --in adk=captures/adk-trace.jsonl --lineage
```

---

## Architecture

```
cmd/nhi-probe        main: picks the command, Ctrl-C → exit 17, soft memory limit
cmd/replay, loadgen  scale-test tools
internal/
  fingerprint        SHA-256 id + redact
  detect             Detector interface, 8 signatures, secret-name rule
  config             Parser interface: one per agent config format
  scan               walker → bounded paths → N workers → collector
  proxy              stdio + Streamable HTTP, kestrelShard, bounded event sink
  hook               Claude Code hook logger
  adapters/          proxyevents, claudehooks, adktrace (one package per source)
  trace              router → session-sharded workers → join → graph → risks → lineage
  lifecycle          signal → context cancel → watchdog
  event              the normalised event
```
**Small interfaces:** `detect.Detector{Detect}`, `config.Parser{Agent, Paths, Parse}`, `trace.Adapter{Name, Parse}`.
**Adding a platform:** one package with `Name()` + `Parse(ctx, r, emit)`, plus one line in `trace/adapter.go`'s map.

### trace pipeline
```
reader per file ──batches──► router ──fnv(session)%N──► worker 1..N ──batches──► collector
 (adapter.Parse)  bounded     (no lock)   bounded       (owns its sessions;       (callStore)
                                                         parks calls until         │
                                                         their parent hop)         ▼
                                     join hooks↔proxy (target + 2s/120s window) → edges → risks → report
```

### What every goroutine does on Ctrl-C

| Command | Goroutine | Job | On Ctrl-C |
|---|---|---|---|
| all | main | runs the command, picks the exit code | sees ctx cancelled → exit 17 |
| all | signal watcher | waits for SIGINT/SIGTERM | cancels ctx; if still alive after 1.5 s → force exit 17 |
| proxy stdio | Run | waits for server→agent, stops the writer | `cmd.Wait()` (server got SIGINT, killed after 1 s) → waits server→agent → drains writer → returns |
| proxy stdio | agent→server | reads stdin, records, forwards | **not waited**: blocked in a stdin read Go can't interrupt; ends when stdin closes or the process exits. Its sends can't block (ctx done) |
| proxy stdio | server→agent | forwards replies, learns server name | server's pipe closes → returns |
| proxy | event writer | only writer of the events file | drains every queued event, returns |
| proxy http | http server + one per request | forward + stream | `Shutdown` (1 s) → `Close` → requests cancelled |
| scan | walker / workers / closer | paths → file scans → results | stop walking / stop at next file → results closed → partial report |
| trace | readers / router / workers / closers | parse → route → resolve | readers stop, router closes shards, workers flush parked calls → partial report |

---

## Trade-offs

- **Hooks ↔ proxy join by target + time window**, because no id is shared between Claude Code and MCP servers. FIFO
  matching keeps identical concurrent calls in order, but two identical calls from different agents a few ms apart could swap chains.
- **One events file per proxy, required `--events`**: appends from two processes to one file aren't guaranteed atomic.
- **Bounded queues that wait** (never drop): an audit tool shouldn't lose events, at the price of slowing the agent if the disk is slow.
- **Plaintext token in `~/.claude.json` on purpose** for C1: reproduces the most common misconfiguration for scan.
- **trace keeps every resolved call in memory** (compact, chunked) to join sources. 10M records peak at ~480 MB:
  under the limit, but with little headroom.

## Honest gaps

- **No AWS Bedrock / IAM role.** My AWS is an AWS Academy sandbox, and I had no paid cloud budget. So the cloud agent
  is Google ADK on the free Gemini tier: the model runs in Google's cloud, the ADK runner on my laptop, and there's no
  provider-side log like CloudTrail. "Implicit use via a cloud role" is shown as "implicit use via the agent's `.env`".
- **Token passthrough not reproduced live** (I didn't want a real token in a prompt). Detection is unit-tested only.
- **Cursor, VS Code, Gemini CLI, Codex parsers** are built from docs and unit-tested, not tried on real installs.
- **Old proxy events** (before `--name`) show `mcp:github-mcp-server/...` and `mcp:Notion API/...` as separate paths.
- **trace throughput is now limited by one reader per file** (JSON decode). More workers don't help past that. The
  next fix (parallel parsing of one file) is described in SCALING but not built.
- **Hook-only calls** to servers with no scanned credential (e.g. the IDE's own `claude-vscode` MCP) stay unlinked, by design.
- The free Gemini tier is slow (~5 min per step), and the last model turn of the ADK run timed out. The hop and the
  tool call were recorded before that.
- Google's new `AQ.` key format isn't in my signatures yet (caught by the name rule only, see DOSSIER R7).

## AI usage

I used **Claude Code (Claude)** throughout, and I disclose it exactly:
- **Planning and research:** it broke the assignment into a step-by-step plan and drafted DOSSIER, SCALING and this README
  from my lab results. I checked them against my lab.
- **Code:** at first it drafted files one at a time, explained them and quizzed me. Under time pressure it wrote the
  rest (trace, HTTP mode, scaling fixes) and wrote down every design choice (options, reason, trade-off) for me to review.
- **Me:** I set up the whole lab (accounts, least-privilege tokens, MCP wiring, hooks, the ADK agent), ran every
  command, test, scale run and commit myself, read the profiles, and reviewed the code.
- In the follow-up I can walk through any file and explain every goroutine.
Details: [AI_USAGE.md](AI_USAGE.md).
