package memory

import "time"

// Judgement is one node-local verdict of the optional write gate (a Hunch
// judge consulted by the memory voter). It is an ANNOTATION on this node, not
// consensus state: another node may hold different judgements for the same
// memory, and none of them mutates the memory itself. A superseded memory is
// hidden from default recall by its judgement, never deprecated, so a wrong
// verdict costs a hidden row that can be shown again rather than lost data.
type Judgement struct {
	MemoryID string `json:"memory_id"`
	// Check is the question asked: "lasting", "supported", "agrees",
	// "replaces", or "final" for the gate's overall outcome.
	Check string `json:"check"`
	// TargetID is the other memory for pairwise checks (agrees, replaces);
	// empty otherwise.
	TargetID string `json:"target_id,omitempty"`
	// PYes is the judge's probability; nil for rows that record an outcome
	// rather than a probability (final).
	PYes *float64 `json:"p_yes,omitempty"`
	// Verdict is what the gate concluded from PYes: pass, reject, abstain,
	// duplicate, supersedes, uncertain, none — or accept / reject / abstain on
	// the final row.
	Verdict string `json:"verdict"`
	// Reason is the human-readable outcome on the final row (reused as the
	// vote rationale); empty on per-check rows.
	Reason string `json:"reason,omitempty"`
	// JudgeVersion identifies the check wording and judge model, so verdicts
	// from different wordings are never mixed silently.
	JudgeVersion string    `json:"judge_version"`
	CreatedAt    time.Time `json:"created_at"`
}

// Gate verdicts.
const (
	VerdictPass       = "pass"
	VerdictReject     = "reject"
	VerdictAbstain    = "abstain"
	VerdictAccept     = "accept"
	VerdictDuplicate  = "duplicate"
	VerdictSupersedes = "supersedes"
	VerdictUncertain  = "uncertain"
	VerdictNone       = "none"
)
