# (S)AGE — Sovereign Agent Governed Experience

**Persistent, consensus-validated memory infrastructure for AI agents.**

SAGE gives AI agents institutional memory that persists across conversations, goes through BFT consensus validation, carries confidence scores, and decays naturally over time. Not a flat file. Not a vector DB bolted onto a chat app. Infrastructure — built on the same consensus primitives as distributed ledgers.

The architecture is described in [Paper 1: Agent Memory Infrastructure](papers/Paper1%20-%20Agent%20Memory%20Infrastructure%20-%20Byzantine-Resilient%20Institutional%20Memory%20for%20Multi-Agent%20Systems.pdf).

> **Just want to install it?** [Download here](https://l33tdawg.github.io/sage/) — double-click, done. Works with any AI.

<a href="https://glama.ai/mcp/servers/l33tdawg/s-age">
  <img width="380" height="200" src="https://glama.ai/mcp/servers/l33tdawg/s-age/badge" alt="(S)AGE MCP server" />
</a>

---

## Architecture

```
Agent (Claude, ChatGPT, DeepSeek, Gemini, etc.)
  │ MCP / REST
  ▼
sage-gui
  ├── ABCI App (validation, confidence, decay, Ed25519 sigs)
  ├── App Validators (sentinel, dedup, quality, consistency — BFT 3/4 quorum)
  ├── Governance Engine (on-chain validator proposals + voting)
  ├── CometBFT consensus (single-validator or multi-agent network)
  ├── SQLite + optional AES-256-GCM encryption
  ├── CEREBRUM Dashboard (SPA, real-time SSE)
  └── Network Agent Manager (add/remove agents, key rotation, LAN pairing)
```

Personal mode runs a real CometBFT node with 4 in-process application validators — every memory write goes through pre-validation, signed vote transactions, and BFT quorum before committing. Same consensus pipeline as multi-node deployments. Add more agents from the dashboard when you're ready.

Full deployment guide (multi-agent networks, RBAC, federation, monitoring): **[Architecture docs](docs/ARCHITECTURE.md)**

---

## CEREBRUM Dashboard

![CEREBRUM — Neural network memory visualization](docs/screen-brain.png)

`http://localhost:8080/ui/` — force-directed neural graph, domain filtering, semantic search, real-time updates via SSE.

### Network Management

![Network — Multi-agent management](docs/screen-network.png)

Add agents, configure domain-level read/write permissions, manage clearance levels, rotate keys, download bundles — all from the dashboard.

### Settings

| Overview | Security | Configuration | Update |
|:---:|:---:|:---:|:---:|
| ![Overview](docs/screen-overview.png) | ![Security](docs/screen-security.png) | ![Config](docs/screen-config.png) | ![Update](docs/screen-update.png) |
| Chain health, peers, system status | Synaptic Ledger encryption, export | Boot instructions, cleanup, tooltips | One-click updates from dashboard |

---

## What's New in v7.7

Agent profile completeness. `GET /v1/agent/me` was the only endpoint where the response struct silently dropped fields the OpenAPI schema promised — `display_name`, `domains`, `accuracy`, and the registration height. The schema was right, the handler was wrong; v7.7 brings them in line so SDK consumers don't have to make a second round-trip to `/v1/agent/{id}` plus the validator-score endpoint just to render a profile card.

- **Populated `AgentProfileResponse`.** `display_name` and `on_chain_height` resolve through `agentStore.GetAgent`; `domains` is a new `AgentStore.ListAgentDomains` (SQLite-backed, ordered by participation frequency) mirroring the existing `ListAgentTags` pattern; `accuracy` reconstructs a `poe.EWMATracker` from the persisted `WeightedSum/WeightDenom/Count` so the cold-start blending (0.5 prior, transitions toward the real score after K_min=10 observations) stays in one place. Pre-existing fields (`agent_id`, `poe_weight`, `vote_count`) unchanged.
- **OpenAPI schema realigned.** The `AgentProfile` schema retired the legacy `registered_at: string date-time` field (handler never returned it; a test guard explicitly rejected the key) and now declares `on_chain_height: integer (int64)` — the canonical name already used everywhere else in the response envelope. SDK consumers should pin `sage-agent-sdk>=7.7.0` to get the type-correct Pydantic model.
- **No behaviour change for grants, votes, or memory writes.** This release is REST-layer plumbing — no consensus rules touched, no chain migration needed.

## What's New in v7.6

Direct-write hooks across Claude Code and Codex. Sessions boot SAGE without depending on the agent to remember `sage_inception` on its own — the local SAGE node is queried directly by lifecycle hooks, recent committed memories land as initial context, and a session-lifecycle observation is written on exit. One unified install path for both agents.

- **`sage-gui hook session-start | session-end`.** New Go subcommand bundled with sage-gui. Loads the agent's Ed25519 key with the same priority chain as the MCP server (SAGE_IDENTITY_PATH → SAGE_AGENT_KEY → per-project key → default), signs REST calls against the local SAGE node, and prints a recent-memories context block on stdout (which Claude Code / Codex inject into the agent's prompt). Soft-fails with non-zero exit so the shell wrapper falls back to a static nudge when the node is unreachable.
- **`sage-gui mcp install` now ships 5 direct-write scripts** (`sage-session-start.sh`, `sage-session-end.sh`, `sage-pre-compact.sh`, `sage-user-prompt.sh`, `sage-stop.sh`) instead of the legacy 2-script nudge set. The session-start/end scripts shell out to `sage-gui hook ...` via an absolute binary path substituted at install time — no `$PATH` lookup, no Python dependency, no pynacl. All scripts respect `~/.sage/memory_mode` (full / bookend / on-demand). The hook block uses `${CLAUDE_PROJECT_DIR}` so paths resolve correctly even when Claude Code starts from a subdir.
- **`sage-gui codex install` mirrors the install for Codex.** Writes `.codex/config.toml` (MCP server registration), `.codex/hooks.json` (hook lifecycle wiring with absolute paths because Codex doesn't expand env vars in hook commands), the same 5 hook scripts under `.codex/hooks/`, and an `AGENTS.md` boot reminder. The hook scripts are identical to the Claude side — same Go binary, same signed REST protocol, same memory_mode semantics. Two agents, one substrate.
- **Self-heal migrates older installs.** Every MCP server start runs `selfHealProject`, which now detects legacy 2-script installs (sage-boot.sh / sage-turn.sh) and rewrites them to the new 5-script set, refreshes scripts when the sage-gui binary moves to a new location (a stale `__SAGE_GUI_BIN__` path triggers re-templating), and repairs missing `hooks.json` on Codex installs. No user action required after upgrading.

### v7.6.2 — auto-install hooks on MCP boot for pre-v7.6 projects

Projects that adopted SAGE before v7.6 shipped silently went without the direct-write hook substrate after upgrading — `selfHealProject` only refreshed hooks when `.claude/hooks/` already existed. v7.6.2 widens the trigger: when `.mcp.json` registers a `sage` server and `.claude/hooks/` is absent, the 5-script set installs automatically on the next MCP session boot. Restart the agent in any SAGE-enabled project; no per-project `sage-gui mcp install` rerun needed. Negative test guards against unsolicited file creation in projects whose `.mcp.json` is wired up for some other MCP server.

### v7.6.1 — prealloc the SessionStart items slice

Pure lint fix. CI's golangci-lint v2.1.6 prealloc rule flagged `var items []item` in `runHookSessionStart`; the loop's max length is known at slice-init time. Switched to `make([]item, 0, len(payload.Memories)+len(payload.Results))`. No behaviour change.

## What's New in v7.5

Migration substrate. Hands-off in-place chain upgrades across consensus-rule changes — no chain reset, no operator commands, no accumulated memory lost. v7.5 itself ships zero consensus-rule changes; the entire release is the plumbing that makes every subsequent release safe to land on existing chains.

- **Scheduled snapshots.** Every committed block, a background scheduler (`internal/abci/snapshot_sched.go`) decides whether to fire `internal/snapshot.Take`. Default cadence: every 10,000 blocks AND every 6 hours. Snapshots land at `~/.sage/data/snapshots/<height>/` with the running binary bundled into `binary/sage-gui-<version>` so rollback has the previous executable available locally. Atomicity via staging-dir + atomic rename + `OK` sentinel. Retention: K=5 newest + one anchor per distinct binary version (powers downgrade).
- **Snapshot verify-by-restore.** `internal/snapshot.Verify` doesn't just check file hashes — it loads the BadgerDB backup into a tmpdir and recomputes `ComputeAppHash` against the manifest, runs SQLite `PRAGMA integrity_check` + `foreign_key_check`, and walks the CometBFT tarball. The check is "this snapshot is restorable" not "this snapshot's bytes parse."
- **Snapshot encryption inherits vault posture.** Synaptic Ledger unlocked at boot → snapshots are wrapped in a streaming Argon2id + AES-256-GCM envelope keyed off the vault passphrase. Plaintext otherwise. No new operator decision.
- **Upgrade tx types + chain-computed activation height.** Three new transaction types (`TxTypeUpgradePropose` / `TxTypeUpgradeCancel` / `TxTypeUpgradeRevert`). When a proposal lands, `ActivationHeight` is computed deterministically inside `FinalizeBlock` as `req.Height + max(payload.UpgradeDelayBlocks, 200)`. Every validator computes the same number from the same inputs — no multi-validator drift. Activation happens via `ResponseFinalizeBlock.ConsensusParamUpdates.Version.App`, CometBFT-native, applied at H+1 atomically across all replicas. At-most-one pending plan at a time enforced on-chain.
- **Auto-proposal watchdog.** `cmd/sage-gui/upgrade_watchdog.go` runs as a goroutine after CometBFT boots. When the running binary's embedded `upgradeTargetAppVersion` exceeds the chain's current app version, it signs an `UpgradePropose` with the node operator's agent key and broadcasts via CometBFT RPC. Terminates on success, on "already pending", or when the chain catches up. v7.5.0 ships with `upgradeTargetAppVersion = 1` (no upgrade) so the watchdog is a no-op until the first real version bump.
- **HALT sentinel on panic.** `cmd/sage-gui/halt_writer.go::haltOnPanic` runs in `runServe`'s deferred recover. Writes a JSON sentinel to `~/.sage/data/HALT` atomically (staging file + fsync + rename), then re-panics so the original stack still surfaces.
- **Supervised rollback.** `cmd/sage-launcher --supervise` parents the chain binary. On child exit it reads the HALT sentinel, scans `~/.sage/data/snapshots/` for the latest anchor whose `BinaryVersion != FailedVersion`, calls the real `snapshot.Restore` (Verify-gated), clears the sentinel, and `syscall.Exec`s the bundled rollback binary. PID continuity preserved for outer supervisors (launchd / systemd). Crash-loop circuit breaker stops thrash at 3 non-HALT crashes within a 60s sliding window. Opt-in `--supervise` flag so the existing detached-launcher flow (macOS `.app` / Windows `.exe` double-click) is untouched.
- **Test coverage:** unit tests for every component plus three integration tests — `TestV75_EndToEnd_ProposeEncodeFinalizeActivate` (full consensus path: build signed tx → encode → decode → dispatch → persist → activate → audit), `TestV75_MultiValidatorDrift` (4 independent SageApp replicas process the same encoded tx and assert byte-identical `ConsensusParamUpdates` at activation), `TestV75_E2E_PanicToRollback` (chain writes HALT → launcher reads HALT → real `snapshot.Restore` mutates data dir back to anchor → execer invoked with the bundled rollback binary). The integration tests caught two real bugs that single-component unit tests missed: a path mismatch between snapshot producer and launcher consumer, and a `time.Time` vs `int64` wire-format mismatch in the manifest schema. Both fixed in the same commit that landed the tests.

Full architecture: [docs/ARCHITECTURE.md#upgrade-machinery-v75](docs/ARCHITECTURE.md#upgrade-machinery-v75).

## What's New in v7.1

Recall quality polish and second-benchmark coverage. Optional cross-encoder reranking, optional query expansion, the LoCoMo retrieval benchmark, and a SAGE adapter shipped upstream to mem0's open-source evaluator so the comparison runs on their published methodology unchanged.

### v7.1.1 — Honest broadcast errors on RBAC/governance writes

- **All RBAC and governance REST handlers now wait for FinalizeBlock before returning.** `POST /v1/access/grant`, `/v1/access/request`, `/v1/access/revoke`, `/v1/domain/register`, `/v1/org/*`, `/v1/dept/*`, `/v1/vote/*`, and `PATCH /v1/agent/{id}` switched from CometBFT's `broadcast_tx_sync` (CheckTx-only) to `broadcast_tx_commit` (CheckTx + FinalizeBlock). Reported by levelup: an `access_grant` from an agent who wasn't the on-chain domain owner previously returned HTTP 201 `{status: "granted"}` while the tx was silently rejected at consensus, leaving callers to discover the ghost-tx by polling `GET /v1/access/grants/{agent_id}`. Now the consensus rejection surfaces as HTTP 403 with the real reason. Same bug class as the v6.6.9 permission-handler fix; this completes the migration across the remaining 20 call sites.
- **Domain-ownership recipe + known limitations documented.** [`docs/ARCHITECTURE.md#domain-ownership-first-write-wins`](docs/ARCHITECTURE.md#domain-ownership-first-write-wins) now spells out the cross-agent write sequence (probe owner → `register_domain` → `access_grant` → write) and the v7.1 limitations: subdomain grants don't cascade from ancestors yet, and lost-owner recovery is manual. The ancestor-walk and `domain_reassign` fixes are queued behind the v7.2 upgrade-machinery work so existing chains can upgrade without a reset.

- **Cross-encoder reranking on `/v1/memory/hybrid` (env-gated, opt-in).** Hybrid recall optionally fans out an oversampled candidate pool through a TEI-compatible HTTP reranker, then keeps the top-K by cross-encoder score. Off by default - turn on with `SAGE_RERANK_ENABLED=1 SAGE_RERANK_URL=<endpoint>`. Tunables: `SAGE_RERANK_MODEL` (defaults `BAAI/bge-reranker-v2-m3`), `SAGE_RERANK_TIMEOUT_MS=2000`, `SAGE_RERANK_OVERSAMPLE=2`. Falls back to RRF ordering if the reranker fails or times out. Native MPS sidecar shipped under `bench/rerank-server/` for Apple Silicon hosts where the HuggingFace TEI container has trouble under Rosetta.
- **Query expansion on `/v1/memory/hybrid` (server-side fanout).** The hybrid endpoint accepts an `expansions: [{query, embedding}]` array and merges results across the original query plus all variants via RRF (`k=60` across variants). Callers generate variants any way they like; the bench harness uses a small LLM call (`gpt-4o-mini`) for paraphrase / entity / temporal variants. Cost vs. recall is a caller-side decision.
- **LoCoMo benchmark, n=1986.** New `bench/locomo/` harness runs the ACL 2024 long-conversation retrieval benchmark (Maharana et al., 10 conversations / 272 sessions / 5882 turns / 1986 questions) end-to-end through the same Ed25519-signed REST path as LongMemEval. Headline retrieval: **R@5 = 0.6394, R@10 = 0.7324, MRR = 0.5790, Hit@5 = 0.6954**. Strong on single-hop (cat 4, n=841, R@5 = 0.765) and temporal (cat 2, R@5 = 0.769); aggregation-heavy cat 1 is metric-structurally lower because mean evidence is 3.13 turns. Methodology, per-category breakdown, and the `make bench-locomo-fetch` reproducer in [`bench/locomo/README.md`](bench/locomo/README.md).
- **LongMemEval-S re-run with the v7.1 stack.** **R@5 = 0.8927, R@10 = 0.9461, MRR = 0.8842 (n=499).** R@10 is up 1.4 pt vs. v7.0; R@5 is down 1.3 pt - the reranker pulls more relevant items into the candidate pool at the cost of some top-5 ordering precision. v7.0 stock numbers remain the right baseline; enable the v7.1 reranker if your downstream consumer benefits from a wider candidate pool.
- **SAGE adapter for mem0's open-source LoCoMo evaluator.** Submitted upstream. The PR adds `evaluation/src/sage/` to mem0's eval suite so SAGE is a first-class storage backend alongside RAG, langmem, openai, and zep. Scored via mem0's own `evals.py` with their `gpt-4o-mini` LLM-judge for direct apples-to-apples comparison: **LLM-judge = 0.7656** (n=1540, cat 5 excluded per mem0's methodology). Curated systems publish ~0.93 on this metric. The gap is full-pipeline tuning - SAGE preserves raw turns under BFT consensus instead of running an LLM curator over the chat history. Operators who want curation layer it on top; SAGE itself stays source-faithful.
- **Node-operator read-scope bypass for hooks.** SessionStart's prefetch path now short-circuits the `resolveVisibleAgents` filter when the caller is the local node operator key, so multi-agent SAGE nodes don't silently return empty recall lists to the hook. Docs in [`docs/HOOKS.md`](docs/HOOKS.md).
- **Python SDK 7.1.0.** New `client.hybrid(query, embedding, ...)` method on `SageClient` and `AsyncSageClient` for calling the hybrid recall endpoint directly. Replaces the previous SDK gap that forced raw `httpx` calls. `pip install sage-agent-sdk>=7.1.0`.

## What's New in v7.0

Recall quality and ambient capture. Hybrid retrieval, lifecycle hooks that actually write through consensus, branch-aware tagging, and the first SAGE benchmark on a public retrieval dataset.

- **Hybrid recall (BM25 + vector via RRF).** `sage_recall` now fuses FTS5/BM25 and vector cosine results in one round trip via weighted Reciprocal Rank Fusion (RRF) instead of picking one index based on vault state. New `POST /v1/memory/hybrid` endpoint backs it, with full RBAC, multi-org, classification, and decay parity vs. the existing query and search paths. Mixed-version networks keep working: older nodes that don't expose `/v1/memory/hybrid` get an automatic fall-back to the legacy FTS5 path. Defaults `RRFK=60`, `BM25=0.4`, `Vector=0.6`, `Oversample=2`; tune at runtime via `SAGE_HYBRID_RRF_K`, `SAGE_HYBRID_BM25_WEIGHT`, `SAGE_HYBRID_VECTOR_WEIGHT`, `SAGE_HYBRID_OVERSAMPLE`. Force the legacy single-index path with `SAGE_RECALL_HYBRID=0`.
- **LongMemEval-S benchmark, 0.9053 R@5 stock defaults.** New `bench/longmemeval/` harness runs the ICLR 2025 retrieval benchmark (Wu et al., 500 questions) against a fresh Docker SAGE through the full BFT consensus pipeline. Headline: **R@5 = 0.9053, R@10 = 0.9332, MRR = 0.9041**. `single-session-assistant` saturates at 0.98; `multi-session` lands at 0.91; full per-category breakdown and per-question detail in `bench/results/longmemeval-full-48e81ec.json`. Methodology, tunables, and reproducer in [`bench/longmemeval/README.md`](bench/longmemeval/README.md).
- **Direct-write lifecycle hooks for Claude Code.** Five hooks under `.claude/hooks/`: `SessionStart` and `SessionEnd` sign REST calls to the local SAGE node directly (no LLM in the loop) using `~/.sage/agent.key`; `PreCompact`, `UserPromptSubmit`, and `Stop` cover the events where direct-write would be too noisy or high-frequency. Install guide and the read-scope caveat for multi-agent nodes in [`docs/HOOKS.md`](docs/HOOKS.md).
- **Branch-aware memory tagging.** Memories written from a git working tree are auto-tagged with `branch:<name>` so `feature/x` and `main` stay separable without manual hygiene. Detection caches for 30 s with a 750 ms hard timeout so a wedged `git` can't stall a memory write. Outside a git repo: silent no-op. Opt out per process: `SAGE_BRANCH_TAG=0`. The tag uses the existing filter, so `sage_recall ... tags=["branch:feature/x"]` works like any other tag.
- **Test coverage.** 36 new tests across `internal/store`, `api/rest`, and `internal/mcp` covering RRF fusion ordering, weight bias, env overrides, hybrid fall-back paths, git-branch detection, tag plumbing, and RBAC parity for the new endpoint. `go test ./...` green on all 22 packages.

## Pre-v7 release history

<details>
<summary>Capability milestones across v3–v6 (full per-patch detail on the <a href="https://github.com/l33tdawg/sage/releases">Releases page</a>)</summary>

- **v6.8 — Hardening pass.** OAuth Dynamic Client Registration + persistent client metadata, mandatory `state` + HMAC-signed CSRF on `/oauth/authorize`, strict same-origin on CEREBRUM wizard endpoints, locked-down subprocess test seams. Admin-bootstrap escape hatch (6.8.5), cross-agent visibility hotfix (6.8.4), Windows wizard parity (6.8.1).
- **v6.7 — ChatGPT MCP connector.** OAuth 2.0 + PKCE wrapper, RFC 8414/7591/9728 discovery and Dynamic Client Registration, in-dashboard ChatGPT setup wizard (6.7.3, Cloudflare zone dropdown 6.7.4), HTTPS-capable HTTP MCP transport (`/v1/mcp/sse` + `/v1/mcp/streamable` on `:8443`) with bearer tokens.
- **v6.6 — Tags + multi-org + RBAC fixes.** Tags first-class on `/v1/memory/submit` and `/query`/`/search` filtering. Multi-org membership reverse index so agents in N orgs no longer silently lose access to N-1 of them. `PUT /v1/agent/{id}/permission` no longer silent-no-ops for non-admin self/org-admin callers. SQLITE_BUSY silent-drop fix at source (WAL pragma + writeMu-guarded BeginTx). Encrypted CA key in quorum manifest (Argon2id + AES-256-GCM envelope).
- **v6.5 — TLS everywhere.** Per-quorum ECDSA P-256 CA, dual-listener REST API (TLS `:8443` + local HTTP `:8080`), Python SDK `ca_cert` parameter. Stuck-proposed deprecation when quorum unreachable. RBAC ownership-theft fix + real broadcast errors surfaced to clients.
- **v6.0 — Dynamic validator governance.** Add/remove/repower validators without stopping the chain via on-chain governance proposals (2/3 BFT quorum). New `internal/governance/` package, in-dashboard Governance section.
- **v5.x — Consensus-first writes + FTS5.** All submissions go through BFT consensus before they surface in queries. 4-validator Docker cluster with fault injection in CI. FTS5 keyword search fallback. Nonce-based replay protection. Python SDK.
- **v4.x — App validators + RBAC + Synaptic Ledger.** Sentinel / Dedup / Quality / Consistency validators with 3/4 quorum. Agent isolation, domain-level permissions, clearance levels, multi-org federation. AES-256-GCM encryption with Argon2id key derivation.
- **v3.x — Multi-agent networks.** Add agents from dashboard, LAN pairing, key rotation, redeployment orchestrator. On-chain agent identity via CometBFT consensus. CEREBRUM dashboard.

</details>

---

## Research

| Paper | Key Result |
|-------|------------|
| [Agent Memory Infrastructure](papers/Paper1%20-%20Agent%20Memory%20Infrastructure%20-%20Byzantine-Resilient%20Institutional%20Memory%20for%20Multi-Agent%20Systems.pdf) | BFT consensus architecture for agent memory |
| [Consensus-Validated Memory](papers/Paper2%20-%20Consensus-Validated%20Memory%20Improves%20Agent%20Performance%20on%20Complex%20Tasks.pdf) | 50-vs-50 study: memory agents outperform memoryless |
| [Institutional Memory](papers/Paper3%20-%20Institutional%20Memory%20as%20Organizational%20Knowledge%20-%20AI%20Agents%20That%20Learn%20Their%20Jobs%20from%20Experience%20Not%20Instructions.pdf) | Agents learn from experience, not instructions |
| [Longitudinal Learning](papers/Paper4%20-%20Longitudinal%20Learning%20in%20Governed%20Multi-Agent%20Systems%20-%20How%20Institutional%20Memory%20Improves%20Agent%20Performance%20Over%20Time.pdf) | Cumulative learning: rho=0.716 with memory vs 0.040 without |

---

## Quick Start

```bash
git clone https://github.com/l33tdawg/sage.git && cd sage
go build -o sage-gui ./cmd/sage-gui/
./sage-gui setup    # Pick your AI, get MCP config
./sage-gui serve    # SAGE + Dashboard on :8080
```

Or grab a binary: [macOS DMG](https://github.com/l33tdawg/sage/releases/latest) (signed & notarized) | [Windows EXE](https://github.com/l33tdawg/sage/releases/latest) | [Linux tar.gz](https://github.com/l33tdawg/sage/releases/latest)

### Docker

```bash
docker pull ghcr.io/l33tdawg/sage:latest
docker run -p 8080:8080 -v ~/.sage:/root/.sage ghcr.io/l33tdawg/sage:latest
```

Pin a specific version with `ghcr.io/l33tdawg/sage:6.0.0`.

### Upgrading from an older version?

If you installed SAGE before v5.0 and your AI isn't doing turn-by-turn memory updates, re-run the installer in your project directory:

```bash
cd /path/to/your/project
sage-gui mcp install
```

This installs Claude Code hooks that enforce the memory lifecycle (boot, turn, reflect) — even if your `.mcp.json` is already configured. Restart your Claude Code session after running this.

---

## Documentation

| Doc | What's in it |
|-----|-------------|
| [Architecture & Deployment](docs/ARCHITECTURE.md) | Multi-agent networks, BFT, RBAC, federation, API reference |
| [Getting Started](docs/GETTING_STARTED.md) | Setup walkthrough, embedding providers, multi-agent network guide |
| [Security FAQ](SECURITY_FAQ.md) | Threat model, encryption, auth, signature scheme |
| [Connect Your AI](https://l33tdawg.github.io/sage/connect.html) | Interactive setup wizard for any provider |

---

## Stack

Go / CometBFT v0.38 / chi / SQLite / Ed25519 + AES-256-GCM + Argon2id / MCP

---

## License

Code: [Apache 2.0](LICENSE) | Papers: [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/)

## Author

Dhillon Andrew Kannabhiran ([@l33tdawg](https://github.com/l33tdawg))

---

<p align="center"><em>A tribute to <a href="http://phenoelit.darklab.org/fx.html">Felix 'FX' Lindner</a> — who showed us <b>how much further curiosity can go.</b></em></p>
