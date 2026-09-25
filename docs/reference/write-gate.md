# Memory gate (optional, experimental)

The built-in voter checks trust the **author's self-declared confidence**. An
agent that stores a remark about its own session — "the attachment was lost;
the user must re-send the numbers" — as a 0.9-confidence fact gets it
committed, and later sessions recall it as if it were true of the world.

The memory gate lets a node ask a [Hunch](https://github.com/ihubanov/hunch)
judge one calibrated question before it votes on a proposed memory: *is this
lasting knowledge, or a statement about the conversation it came from?* Hunch
reads one constrained token's logprobs from a model you run and returns a
probability, so the decision is a number with a threshold, not a guess.

It is **off by default** and changes nothing in consensus, the transaction
format or recall.

## The check

> Should this be stored as lasting memory: does it state something about the
> world, or a standing rule or method, that stays true outside the conversation
> it came from?

It names its look-alikes: facts about people, places, systems or events and
standing rules or methods are lasting; a lost or unreadable message, truncated
context, something that could not be done just now, or a request to re-send are
not. The wording is versioned (`sage-lasting/N`) and recorded with every
verdict.

## What the node does with the answer

- **≥ 0.9 — pass:** the built-in checks decide as before.
- **< 0.5 — fail:** the node votes REJECT, with the probability in the vote
  rationale.
- **In between — abstain:** the node does **not vote**. The memory waits in the
  operator's review queue; once the operator accepts or rejects it, the node
  votes that decision on its next tick.
- **Judge unreachable or no verdict:** the node votes with the built-in checks
  exactly as before and records nothing.

A memory is judged once; its outcome is stored and reused.

## Several judges

`SAGE_HUNCH_MODELS=model-a,model-b` asks each model concurrently; the first
**leads**. With the default policy (`lead`) a memory passes when the lead is at
or above 0.9 and no other judge clearly objects (below 0.5), and fails only when
every judge is below 0.5. `SAGE_HUNCH_POLICY=all` requires every judge to reach
0.9. Judges from different model families have different blind spots; a second
judge is a cheap guard against the first's confident mistakes.

## Program-written memories

Catalog entries and other records written by programs, not distilled from a
conversation, should not be asked this question. List their domain prefixes in
`SAGE_HUNCH_EXEMPT_DOMAINS`; the built-in checks apply to them as before.

## Configuration (personal node)

| Variable | Meaning |
|---|---|
| `SAGE_HUNCH_URL` | Hunch service base URL. **Unset = gate off.** |
| `SAGE_HUNCH_API_KEY` | bearer key, if the service needs one |
| `SAGE_HUNCH_MODELS` | comma-separated judge models, first one leads (empty = service default, one judge) |
| `SAGE_HUNCH_POLICY` | `lead` (default) or `all` |
| `SAGE_HUNCH_EXEMPT_DOMAINS` | comma-separated domain prefixes of program-written memories |
| `SAGE_HUNCH_TIMEOUT` | judge budget per memory (default `60s`) |

## Operator endpoints

- `GET /v1/dashboard/memory/review-queue` — memories waiting for a decision
- `GET /v1/dashboard/memory/{id}/judgements` — the verdicts for one memory
- `POST /v1/dashboard/memory/{id}/review` `{"decision": "accept"|"reject", "note": "..."}`

## Measured on a real store

Two judges (lead policy), one real personal memory store, hand-checked by one
reader; small samples.

- 194 memories an operator had cleaned up as stale session state: **60%
  rejected**; every one the gate let through was, on reading, a user decision or
  preference rather than a session remark.
- 38 genuine written facts: **none rejected**, about 1 in 10 sent to review.
- A judge call took ~0.6 s (median); judges are asked concurrently.

## Scope and limits

- **Per node.** Verdicts and review decisions live in this node's SQLite store
  and shape this node's vote only. The PostgreSQL store does not implement the
  gate.
- **The judge is not deterministic across nodes**, which is why it runs in the
  voter (whose votes may legitimately disagree) and never in the state machine.
- **Qualify before trusting.** Accuracy depends on the model and on your
  memories; measure on a sample of your own before relying on the thresholds.
