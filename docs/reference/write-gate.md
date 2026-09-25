# Memory write gate (optional, experimental)

The built-in voter checks trust the **author's self-declared confidence** and
only catch **byte-identical** duplicates. An agent that stores a remark about
its own session — "the attachment was lost; the user must re-send the numbers"
— as a 0.9-confidence fact gets it committed, and later sessions recall it as if
it were true of the world. A corrected fact and its correction are both
recalled at full confidence.

The write gate lets a node ask a [Hunch](https://github.com/ihubanov/hunch)
judge calibrated yes/no questions before it votes on a proposed memory. Hunch
reads one constrained token's logprobs from a model you run and returns a
probability, so every decision is a number with a threshold, not a guess.

## What it asks

| Check | Asked about | Context |
|---|---|---|
| `lasting` | every proposed memory | is it about the world (or a standing rule/method), rather than about the conversation it came from? |
| `supported` | memories submitted **with evidence** | does the evidence support it? |
| `agrees` | the new memory vs each of its nearest committed neighbours (same domain) | a semantic duplicate? |
| `replaces` | same pairs | does the new memory correct the old one's value for the same thing? |

Every check names its look-alikes (a restatement is not a replacement; a
correction of a *different* subject is not a replacement; a standing rule is a
lasting memory). The wording is versioned (`sage-memory-checks/N`) and recorded
with every verdict.

## What it does with the answers

Probabilities fall in three bands (defaults): **act at ≥ 0.9**, **fail below
0.5**, **abstain in between**.

- **Fail** (`lasting` or `supported` below 0.5): the node votes REJECT with the
  probability in the vote rationale.
- **Abstain**: the node does **not vote**. The memory waits in the operator's
  review queue; once the operator accepts or rejects it, the node votes that
  decision on its next tick.
- **Supersedes** (`replaces` ≥ 0.9): the new memory is accepted and the old one
  is **marked**, node-locally, as superseded. It is **never challenged or
  deprecated** — SAGE has no hard delete and a deprecation cannot be undone, so
  a wrong verdict would be data loss. A marked memory is hidden from default
  recall and returned with `include_superseded: true` (with `superseded_by`).
  A mark only takes effect once the correcting memory is itself committed.
- **Duplicate** (`agrees` ≥ 0.9): marked, and the new memory is still accepted,
  unless `SAGE_HUNCH_DEDUP_REJECT=1`. Enable that only after measuring the
  duplicate check's false-positive rate on your own memories: a rejected memory
  never lands.
- **Chains are closed in code, not by the judge.** If B restates A and C
  corrects B, A is hidden too, and `superseded_by` names the newest memory that
  is not itself superseded.

Evidence (`evidence` on `POST /v1/memory/submit`, up to 32 KiB) is stored
**node-local, never in the transaction**, so it can be removed from the node.
A memory without evidence is **unjudged** — it has no `judged_confidence` and the
built-in confidence rule applies as before. It never receives a guessed middle
score. With evidence, `judged_confidence` replaces the author's number in the
built-in "facts need ≥ 0.7" rule and is returned on recall.

If the judge is unreachable or returns no verdict, the node votes with the
built-in checks exactly as before and records nothing.

## Several judges

`SAGE_HUNCH_MODELS=model-a,model-b` asks every check of each model; the first
model **leads**. Under the default policy (`SAGE_HUNCH_POLICY=lead`) a check
passes when the lead is at or above the act threshold and **no other judge
clearly objects** (below the fail threshold); it fails only when every judge is
below the fail threshold. `SAGE_HUNCH_POLICY=all` instead requires every judge
to reach the act threshold. **Duplicate links always require every judge**,
whatever the policy: a duplicate joins two memories into one restatement
class, so a false one would hide a refinement with its original.

Judges from different model families have different blind spots — one hedges
on unit conversions and on terse technical text, another confidently calls
compatible opposite events ("some arrived" / "some left") replacements. On a
real memory store with two judges, `all` sent about a quarter of genuine facts
and most tool descriptions to review (one judge hedging); `lead` cut that to
about a tenth of genuine facts and a few percent of tool descriptions while
still rejecting no genuine fact, and rejected the same share of session
remarks.

## Program-written memories

Catalog entries, generated records and other memories written by programs
rather than distilled from a conversation should not be asked "is this about
the world or about the session?" — in testing, most machine-generated records
were rejected by that question. List their domain prefixes in
`SAGE_HUNCH_EXEMPT_DOMAINS`; the built-in checks apply to them as before.

## Configuration (personal node)

| Variable | Meaning |
|---|---|
| `SAGE_HUNCH_URL` | Hunch service base URL. **Unset = gate off.** |
| `SAGE_HUNCH_API_KEY` | bearer key, if the service needs one |
| `SAGE_HUNCH_MODELS` | comma-separated judge models, first one leads (empty = service default, one judge) |
| `SAGE_HUNCH_POLICY` | `lead` (default) or `all` — see "Several judges" |
| `SAGE_HUNCH_EXEMPT_DOMAINS` | comma-separated domain prefixes of program-written memories, not judged |
| `SAGE_HUNCH_NEIGHBOURS` | committed neighbours compared per memory (default 5, 0 = off) |
| `SAGE_HUNCH_DEDUP_REJECT` | `1` to reject semantic duplicates instead of marking them |
| `SAGE_HUNCH_TIMEOUT` | judge budget per memory (default `60s`) |

## Operator endpoints

- `GET /v1/dashboard/memory/review-queue` — memories waiting for a decision
- `GET /v1/dashboard/memory/{id}/judgements` — every verdict for one memory
- `POST /v1/dashboard/memory/{id}/review` `{"decision": "accept"|"reject", "note": "..."}`

## Scope and limits

- **Per node.** Judgements, review decisions and evidence live in this node's
  SQLite store. They shape this node's vote and this node's recall, never
  consensus state; another node may judge differently or not at all. The
  PostgreSQL store does not implement the gate.
- **The judge is not deterministic across nodes**, which is why it runs in the
  voter (whose votes may legitimately disagree) and never in the state machine.
- **Qualify before trusting.** Accuracy depends on the model and on your
  memories. Measure the checks on your own history — every correction submitted
  with `replaces_memory_id` is a labelled (old, new) pair — before relying on
  the 0.9 threshold, and before enabling duplicate rejection.
- **The duplicate check feeds chain closure**, so a false duplicate hides a
  refinement together with its original once either is corrected. Its v2
  wording names "adds detail" as the look-alike but has not yet been measured
  on real memories; until it is, pair judges from different model families in
  `SAGE_HUNCH_MODELS`.
  On a small synthetic set (70 decidable pairs, one run per model) v2 fixed the
  refinement look-alike on every model tested but is a trade rather than a
  strict improvement: one model became slightly more willing to call
  near-misses duplicates ("site B holds 12" vs "site C holds 12"; "5 km" vs
  "4000 m", both just over 0.9). Requiring two judges from different families
  to agree gave zero false duplicates on that set while keeping every true
  restatement. Synthetic-set results only; real pairs decide.
- **Memories saved without an embedding** (the embedder was unavailable and the
  vector is repaired later) are judged without the neighbour comparison, so
  they are never compared for duplicates or corrections.
- **Known blind spots:** quantities in different units (normalise in code
  before judging); opposite-direction statements that can both be true. Marks
  are reversible precisely so that these cost a hidden row, not lost data.
