# DOSSIER: how AI agents hold and spend non-human identities

Every claim has a source link or **[lab]** evidence from my own runs (15 Nov 2026). Lab evidence is
redacted to fingerprint + first 4 / last 4 characters. Files: [`captures/`](captures/), [`results/`](results/).

**My lab:**

| Piece | What I used |
|---|---|
| Local agent | Claude Code 2.1.325 (macOS), with hooks + subagents |
| Cloud agent | Google ADK, model `gemini-3.8-flash` (supervisor → coder) |
| Agent-to-agent hops | Claude Code subagent (`general-purpose`), ADK `transfer_to_agent` |
| MCP servers | `github-mcp-server` (stdio), `@notionhq/notion-mcp-server` (stdio), GitHub remote MCP (Streamable HTTP) |
| Credentials I created | **C1** GitHub fine-grained PAT (1 sandbox repo, Issues + Contents read, 7 days), **C2** Notion internal connection token (Read content only, 1 page), **C3** Gemini API key (AI Studio, no billing) |

---

## 1. Where credentials live

| Agent | Config path | Format | How secrets are stored | Evidence |
|---|---|---|---|---|
| **Claude Code** (lab) | `~/.claude.json` (user + per-project `mcpServers`), `./.mcp.json` (shared project file) | JSON | **Plaintext.** `claude mcp add --env K=V` and `--header` write values into `~/.claude.json`. The CLI *prints* the header as `[REDACTED]`, but the file holds the real value | [lab] `results/scan.txt`: `22b3ed9d5110 gith...IAdF` at `claude-code / github env:GITHUB_PERSONAL_ACCESS_TOKEN`. Docs: [Claude Code MCP](https://docs.claude.com/en/docs/claude-code/mcp) |
| Claude Code's own login | macOS Keychain | OS keychain | Encrypted by the OS (only Claude's own account token, not MCP tokens) | [Claude Code IAM docs](https://docs.claude.com/en/docs/claude-code/iam) |
| **Google ADK** (lab) | `.env` next to the agent | dotenv | **Plaintext.** `GOOGLE_API_KEY` plus any tool tokens. ADK's quickstart puts the key in `.env` | [lab] scan: `39ebae290f9f AQ.A...Imsw` and `22b3ed9d5110` at `adk / tools`. [ADK quickstart](https://google.github.io/adk-docs/get-started/quickstart/) |
| Cursor | `~/.cursor/mcp.json`, `./.cursor/mcp.json` | JSON (`mcpServers`) | Plaintext `env` / `headers` | [Cursor MCP docs](https://docs.cursor.com/context/model-context-protocol) |
| VS Code (Copilot agent mode) | `./.vscode/mcp.json`, user `mcp.json` | JSON (`servers`, `inputs`) | Plaintext by default. `inputs` of type `promptString` + `password: true` keeps the secret in VS Code's secret storage instead | [VS Code MCP servers](https://code.visualstudio.com/docs/copilot/chat/mcp-servers) |
| Gemini CLI | `~/.gemini/settings.json`, `./.gemini/settings.json` | JSON (`mcpServers`, `httpUrl`, `headers`) | Plaintext `env` / `headers`. OAuth tokens for remote servers are stored in a separate file under `~/.gemini/` | [Gemini CLI MCP docs](https://github.com/google-gemini/gemini-cli/blob/main/docs/tools/mcp-server.md) |
| Codex CLI | `~/.codex/config.toml` | TOML (`[mcp_servers.<name>]`, `env`) | Plaintext in `env`, or a reference to an env var holding a bearer token | [Codex config docs](https://github.com/openai/codex/blob/main/docs/config.md) |

`nhi-probe scan` parses all of these (`internal/config`). Claude Code and ADK are proven on my real files. The
other four are built from their docs and covered by unit tests.

**Pattern:** every agent I looked at defaults to **plaintext secrets in a JSON/TOML/dotenv file**. Only VS Code has a
built-in way to keep MCP secrets out of the file.

---

## 2. How credentials reach a call

| Path | How it works | Lab evidence |
|---|---|---|
| **MCP server env at launch** (stdio) | The agent starts the server as a child process with `env`. The server holds the token for its whole life; tool calls never carry it | [lab] proxy event `via: env:GITHUB_PERSONAL_ACCESS_TOKEN` (implicit use) |
| **Inherited env** | A child process gets the *parent's whole environment* unless it is cleared. So every MCP server also gets the agent's own variables | [lab] every stdio server I proxied received `CLAUDE_CODE_MESSAGING_TOKEN` (see risk R1) |
| **Headers on remote MCP** (Streamable HTTP) | `Authorization: Bearer …` sent on **every** request | [lab] `captures/github-remote-events.jsonl`: `via: header:Authorization`, same fingerprint `22b3ed9d5110` as C1 (direct use) |
| **OAuth from the MCP auth flow** | Remote servers can require OAuth 2.1. The client gets a token for that server only (audience-bound) | [MCP authorization spec](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization). [lab] `claude mcp list` shows OAuth-based remote servers (`mcp.notion.com`, `mcp.slack.com`) as "Needs authentication" |
| **IAM roles on cloud action groups** | e.g. a Bedrock action group's Lambda runs with its execution role. No secret in the agent; AWS signs calls with temporary role credentials | [Bedrock action groups](https://docs.aws.amazon.com/bedrock/latest/userguide/agents-action-create.html). Not in my lab (see gaps) |
| **In-process tool env (ADK)** | Function tools run inside the agent process and read `os.environ` | [lab] `adk:coder > tool:github_list_issues` used C1 from `.env` |
| **Delegation across hops** | A subagent / sub-agent gets *the same tools*, so it spends the parent's credentials. Nothing re-scopes them per agent | [lab] `claude-code > subagent:general-purpose > mcp:notion/API-post-search` used C2; `adk:supervisor > adk:coder` used C1 |

---

## 3. What leaves a trace (and what it misses)

| Signal | Proves | Misses |
|---|---|---|
| **MCP wire (my proxy)**: `tools/call` + launch env + HTTP headers | which client called which tool on which server, and every credential **on** the call or **held by** the server | what the server does with the token downstream (which API endpoints). Calls that bypass the proxy |
| **Claude Code hooks** (`PreToolUse`, `SubagentStop`) | tool name, `session_id`, `tool_use_id`, and `agent_id` for subagent calls → the **delegation chain** | **no credential at all**, and **no timestamp** (I add `nhi_ts`). Hook-only calls must be linked via scan |
| **ADK event log** | `author`, `transfer_to_agent`, `function_call` name/args → chain + tool | which credential an in-process tool used (it's in env, not in the event) |
| GitHub secret scanning | a GitHub/AWS/Slack/… token pushed to a **public** repo is reported to the provider | private repos without paid secret protection; tokens never pushed |
| Provider audit logs (GitHub org audit log, AWS CloudTrail) | that a token / role was used, from where | **which agent or tool call** used it. They only see the token |

**Key insight:** no single source answers "which agent used which credential". Hooks know the *agent*, the wire knows
the *credential*, scan knows *where it's wired*. trace joins them: hooks ↔ proxy by target + time window (2 s before
to 120 s after), hooks ↔ scan by (platform, server).

---

## 4. Signatures (8 providers)

Used by `internal/detect` (scan + proxy). **Verify endpoints are read-only and listed for completeness. scan never
calls them**, because the corpus contains other people's keys.

| Provider | Prefix | Regex (as implemented) | Read-only verify endpoint |
|---|---|---|---|
| GitHub | `github_pat_`, `ghp_` `gho_` `ghu_` `ghs_` `ghr_` | `github_pat_[A-Za-z0-9_]{82}\|gh[pousr]_[A-Za-z0-9]{36}` | `GET https://api.github.com/user` ([formats](https://github.blog/engineering/platform-security/behind-githubs-new-authentication-token-formats/)) |
| AWS | `AKIA` (long-term), `ASIA` (temporary) | `\b(?:AKIA\|ASIA)[A-Z0-9]{16}\b` | `sts:GetCallerIdentity` ([docs](https://docs.aws.amazon.com/STS/latest/APIReference/API_GetCallerIdentity.html), [IDs](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_identifiers.html)) |
| Slack | `xoxb-` `xoxp-` `xoxa-` … | `xox[abposr]-[A-Za-z0-9-]{10,}` | `POST https://slack.com/api/auth.test` ([token types](https://api.slack.com/concepts/token-types)) |
| Notion | `ntn_` (new), `secret_` (old) | `\bntn_[A-Za-z0-9]{40,}\|\bsecret_[A-Za-z0-9]{43}\b` | `GET https://api.notion.com/v1/users/me` ([docs](https://developers.notion.com/reference/get-self)) |
| OpenAI | `sk-proj-` `sk-svcacct-` `sk-admin-` | `sk-(?:proj\|svcacct\|admin)-[A-Za-z0-9_-]{20,}` | `GET https://api.openai.com/v1/models` ([docs](https://platform.openai.com/docs/api-reference/models/list)) |
| Anthropic | `sk-ant-api03-`, `sk-ant-admin01-` | `sk-ant-(?:api\|admin)\d{2}-[A-Za-z0-9_-]{80,}` | `GET https://api.anthropic.com/v1/models` ([docs](https://docs.anthropic.com/en/api/models-list)) |
| Google API key | `AIza` (see R7) | `AIza[0-9A-Za-z_-]{35}` | `GET https://generativelanguage.googleapis.com/v1beta/models?key=…` ([docs](https://ai.google.dev/api/models)) |
| Stripe | `sk_live_`, `rk_live_` | `\b(?:sk\|rk)_live_[0-9A-Za-z]{24,}` | `GET https://api.stripe.com/v1/balance` ([keys](https://docs.stripe.com/keys)) |

Plus a **name rule**: env/header names containing `TOKEN`, `SECRET`, `PASSWORD`, `API_KEY`, ending in `_KEY`, or
`Authorization` → provider `unknown` (8+ chars, placeholders like `${input:x}` skipped). This caught R1 and R7.

---

## 5. Risks

| # | Misconfiguration | Severity | Why | Reproduced? |
|---|---|---|---|---|
| **R1** | **The agent's own token is passed down to every MCP server it starts (inherited env)** | **High** | Claude Code starts stdio servers with its full environment, including `CLAUDE_CODE_MESSAGING_TOKEN`. Any third-party server (e.g. from `npx`) can read it. It rotates per session (good), but it still reaches code nobody vetted | ✅ **[lab]** trace risk `used-but-not-wired` ×4: fingerprints `124bc26e930f`, `5d265426c3a1`, `95eba588a736`, `c1869e3b0dbb` (one per session) |
| **R2** | **One credential shared by 3+ agents** | **High** | One leak compromises all of them. You can't revoke it for one agent, and logs can't tell them apart | ✅ **[lab]** trace: `shared-by-3plus-agents 22b3ed9d5110: adk:coder, claude-code, subagent:general-purpose` |
| **R3** | **Plaintext tokens in agent config files** | **High** | Readable by any process running as the user. Easy to commit by mistake (`.mcp.json` is meant to be committed) | ✅ **[lab]** scan found C1 in `~/.claude.json` (env + header) and C1 + C3 in ADK `.env` |
| **R4** | **Subagents inherit the parent's credentials** (no per-agent scoping) | Medium | A delegated agent with a narrower task still holds full access | ✅ **[lab]** `claude-code > subagent:general-purpose > mcp:notion/*` used C2 |
| **R5** | **Over-privileged defaults when creating a token** | Medium | A new Notion connection came pre-ticked with Read + Update + Insert and "read user info including emails". I had to cut it down to Read only | ✅ **[lab]** observed in the Notion developer UI (screenshots in my notes) |
| **R6** | **Token passthrough**: a client token forwarded upstream, or a raw secret passed in tool arguments | High | The MCP spec forbids servers from passing client tokens to upstream APIs ([security best practices](https://modelcontextprotocol.io/specification/2025-06-18/basic/security_best_practices)). Secrets in args end up in model context and logs | ❌ Not reproduced (I didn't want a real token in a prompt). trace flags it (`token-passthrough` / `token-in-arguments`, unit-tested) |
| **R7** | **Scanners go stale when key formats change** | Medium | My Gemini key starts `AQ.`, not the documented `AIza…`, so a prefix-only scanner misses it | ✅ **[lab]** scan found `39ebae290f9f AQ.A...Imsw` only via the name rule (`GOOGLE_API_KEY`), as provider `unknown` |
| **R8** | **Losing attribution with the new MCP protocol** | Medium (visibility) | MCP `2026-07-28` drops the `initialize` handshake: client identity moves to `params._meta` on every request, and the server name to `server/discover`. Tools that read only `initialize` record empty client/server | ✅ **[lab]** my first proxy version logged `"agent":""`, `"target":"mcp:/list_issues"` until I read `_meta` (`internal/proxy/proxy.go`) |
| **R9** | **A broken hook can freeze the agent** | Low–Medium | A PreToolUse hook that exits with code 2 **blocks the tool call**. A logging hook bug became a denial of service for my own session | ✅ **[lab]** an old `nhi-probe` binary returned exit 2 → every tool call was blocked until I rebuilt. `nhi-probe hook` now always exits 0 |

---

## 6. Something new I learned in the lab

1. **The new MCP protocol changes what a monitoring proxy must read** (R8). With `2026-07-28`, Claude Code opens
   with `server/discover` and puts `clientInfo` in `_meta` on **every** request. That's actually *better* for a
   proxy: every single call carries the client identity, so there's no per-session handshake state to lose.
2. **Remote MCP servers can be fully stateless.** GitHub's remote server never returned an `Mcp-Session-Id`, so
   session-based correlation over HTTP is impossible, and each call has to be attributed on its own.
3. **An agent's own credential is passed down to every tool server it starts** (R1), and it changes fingerprint every session. So
   "inventory once" doesn't work: you have to watch at runtime.
4. **Hooks know the agent, the wire knows the credential.** Neither alone can answer the question. Joining them on
   (target, time window) matched every lab call that went through both, once proxy and agent used the same server
   name (`--name`). The 2 proxy-only calls in `results/trace.txt` are from before I added it.
