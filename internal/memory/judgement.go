package memory

import "time"

// Judgement is one node-local verdict of the optional memory gate (a Hunch
// judge consulted by the memory voter). It is an annotation on this node, not
// consensus state: another node may hold a different judgement for the same
// memory, or none.
type Judgement struct {
	MemoryID string `json:"memory_id"`
	// Check is the question asked ("lasting"), or "final" for the gate's
	// overall outcome.
	Check string `json:"check"`
	// PYes is the judge's probability; nil on the final row.
	PYes *float64 `json:"p_yes,omitempty"`
	// Verdict is pass / reject / abstain for a check, accept / reject /
	// abstain on the final row.
	Verdict string `json:"verdict"`
	// Reason is the human-readable outcome on the final row (reused as the
	// vote rationale); empty on per-check rows.
	Reason string `json:"reason,omitempty"`
	// JudgeVersion identifies the check wording, policy and judge models, so
	// verdicts from different setups are never mixed silently.
	JudgeVersion string    `json:"judge_version"`
	CreatedAt    time.Time `json:"created_at"`
}

// Gate verdicts.
const (
	VerdictPass    = "pass"
	VerdictReject  = "reject"
	VerdictAbstain = "abstain"
	VerdictAccept  = "accept"
)
