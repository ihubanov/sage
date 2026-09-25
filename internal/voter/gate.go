package voter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/l33tdawg/sage/internal/hunch"
	"github.com/l33tdawg/sage/internal/memory"
)

// Gate is the optional write gate: before voting, the node asks a Hunch judge
// calibrated yes/no questions about a proposed memory and about its nearest
// committed neighbours. It exists because the built-in checks trust the
// author's self-declared confidence and only catch byte-identical duplicates,
// so an agent that stores a remark about its own session ("context lost, the
// user must re-send the numbers") as a 0.9 fact gets it committed and recalled
// as truth.
//
// The gate MARKS, it does not mutate. A memory it judges to be superseded by
// the new one is annotated (node-local) and hidden from default recall; it is
// never challenged or deprecated here, because SAGE has no hard delete and a
// deprecation cannot be undone, so a wrong verdict would be data loss. Anything
// in the uncertain band is abstained on and left for a human in the review
// queue rather than guessed.
//
// Everything here is a per-node opinion. It shapes this node's vote and this
// node's annotations, never consensus state directly.
type Gate struct {
	// Judges are consulted on every check. With more than one, the gate acts
	// (votes, marks) only when EVERY judge is at or above ActAt, and fails a
	// check only when every judge is below RejectBelow; any disagreement is an
	// abstain. Judges from different model families have different blind
	// spots (one hedges on arithmetic, another confidently calls compatible
	// opposite events — arrived/departed — replacements), so requiring
	// agreement removes most confident-wrong marks at the cost of one more
	// call per check.
	Judges []Judger
	// ActAt is the probability at or above which a verdict is acted on (vote,
	// mark). Default 0.9.
	ActAt float64
	// RejectBelow is the probability below which a check fails. Between the
	// two the gate abstains. Default 0.5.
	RejectBelow float64
	// Neighbours is how many committed memories in the same domain, nearest by
	// embedding, are compared with the new one. 0 disables the comparison;
	// sage-gui sets 5.
	Neighbours int
	// DedupReject votes REJECT on a semantic duplicate instead of only
	// marking it. Off by default: a rejected memory never lands, which is
	// irreversible for its writer, so it should only be enabled once the
	// duplicate check's false-positive rate has been measured on real pairs.
	DedupReject bool
	// Version identifies the judge (model and check wording) on every
	// judgement row.
	Version string
	// Timeout bounds all judge calls for one memory. Default 60s.
	Timeout time.Duration
}

// Judger is the judge the gate consults (a *hunch.Client in production).
type Judger interface {
	YesNo(ctx context.Context, judgeContext any, checks map[string]hunch.Check) (map[string]float64, error)
}

// GateStore is what the gate needs from the node's store beyond Store. Stores
// that do not implement it run the voter without the gate.
type GateStore interface {
	GetMemory(ctx context.Context, memoryID string) (*memory.MemoryRecord, error)
	// GateNeighbours returns up to k committed memories in rec's domain nearest
	// to rec by embedding (never rec itself).
	GateNeighbours(ctx context.Context, rec *memory.MemoryRecord, k int) ([]*memory.MemoryRecord, error)
	RecordJudgements(ctx context.Context, js []memory.Judgement) error
	// GateFinal returns the gate's recorded overall outcome for a memory.
	GateFinal(ctx context.Context, memoryID string) (verdict, reason string, ok bool, err error)
	// ReviewDecision returns a human's accept/reject for an abstained memory.
	ReviewDecision(ctx context.Context, memoryID string) (decision string, ok bool, err error)
	// MemoryEvidence returns the evidence submitted with a memory, if any.
	MemoryEvidence(ctx context.Context, memoryID string) (string, bool, error)
}

// GateDecision is the gate's verdict for one memory.
type GateDecision struct {
	Accept  bool
	Abstain bool // do not vote; the memory waits in the review queue
	Reason  string
}

func (g *Gate) defaults() Gate {
	c := *g
	if c.ActAt <= 0 || c.ActAt > 1 {
		c.ActAt = 0.9
	}
	if c.RejectBelow <= 0 || c.RejectBelow >= c.ActAt {
		c.RejectBelow = 0.5
	}
	if c.Neighbours < 0 {
		c.Neighbours = 0
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	if c.Version == "" {
		c.Version = hunch.ChecksVersion
	}
	return c
}

// band maps the judges' lowest and highest probability to pass / abstain /
// reject: pass needs every judge at or above ActAt, reject needs every judge
// below RejectBelow.
func (g Gate) band(lo, hi float64) string {
	switch {
	case lo >= g.ActAt:
		return memory.VerdictPass
	case hi < g.RejectBelow:
		return memory.VerdictReject
	default:
		return memory.VerdictAbstain
	}
}

// ask puts one check to every judge and returns the lowest and highest p.
func (g Gate) ask(ctx context.Context, judgeContext any, id string, check hunch.Check) (lo, hi float64, err error) {
	lo, hi = 1, 0
	for _, j := range g.Judges {
		ps, err := j.YesNo(ctx, judgeContext, map[string]hunch.Check{id: check})
		if err != nil {
			return 0, 0, err
		}
		p := ps[id]
		if p < lo {
			lo = p
		}
		if p > hi {
			hi = p
		}
	}
	return lo, hi, nil
}

// Decide runs the gate for one proposed memory. A memory the gate has already
// judged is not sent to the judge again: its recorded outcome is reused, and an
// abstained memory is voted only once a human has decided it in the review
// queue. A judge failure returns an error and records nothing, so the caller
// falls back to the built-in checks rather than guessing.
func (g *Gate) Decide(ctx context.Context, gs GateStore, dup DupChecker, memoryID string) (GateDecision, error) {
	cfg := g.defaults()
	if verdict, reason, ok, err := gs.GateFinal(ctx, memoryID); err != nil {
		return GateDecision{}, err
	} else if ok {
		if verdict != memory.VerdictAbstain {
			return GateDecision{Accept: verdict == memory.VerdictAccept, Reason: reason}, nil
		}
		decision, decided, derr := gs.ReviewDecision(ctx, memoryID)
		if derr != nil {
			return GateDecision{}, derr
		}
		if !decided {
			return GateDecision{Abstain: true, Reason: reason}, nil
		}
		return GateDecision{Accept: decision == memory.VerdictAccept,
			Reason: "review: " + decision + " (gate abstained: " + reason + ")"}, nil
	}

	rec, err := gs.GetMemory(ctx, memoryID)
	if err != nil {
		return GateDecision{}, err
	}
	jctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	now := time.Now().UTC()
	var rows []memory.Judgement
	var notes []string
	abstain := false
	// The recorded p is the judges' LOWEST: the probability every judge
	// reached, which is what an act-at threshold was applied to.
	add := func(check, target string, p float64, verdict string) {
		pp := p
		rows = append(rows, memory.Judgement{MemoryID: memoryID, Check: check, TargetID: target,
			PYes: &pp, Verdict: verdict, JudgeVersion: cfg.Version, CreatedAt: now})
	}
	finish := func(d GateDecision) (GateDecision, error) {
		final := memory.VerdictAccept
		switch {
		case d.Abstain:
			final = memory.VerdictAbstain
		case !d.Accept:
			final = memory.VerdictReject
		}
		rows = append(rows, memory.Judgement{MemoryID: memoryID, Check: "final", Verdict: final,
			Reason: truncateReason(d.Reason), JudgeVersion: cfg.Version, CreatedAt: now})
		if rerr := gs.RecordJudgements(ctx, rows); rerr != nil {
			return GateDecision{}, rerr
		}
		return d, nil
	}

	// Supported: only with evidence. A memory without evidence is unjudged and
	// keeps the author's confidence under the built-in rule; it never gets a
	// guessed middle score.
	confidence := rec.ConfidenceScore
	if evidence, ok, eerr := gs.MemoryEvidence(ctx, memoryID); eerr != nil {
		return GateDecision{}, eerr
	} else if ok && strings.TrimSpace(evidence) != "" {
		p, hi, jerr := cfg.ask(jctx, map[string]string{"memory": rec.Content, "evidence": evidence},
			"supported", hunch.Supported)
		if jerr != nil {
			return GateDecision{}, jerr
		}
		v := cfg.band(p, hi)
		add("supported", "", p, v)
		switch v {
		case memory.VerdictReject:
			return finish(GateDecision{Reason: fmt.Sprintf("not supported by its evidence (p=%.2f)", p)})
		case memory.VerdictAbstain:
			abstain = true
			notes = append(notes, fmt.Sprintf("evidence support uncertain (p=%.2f)", p))
			// Already routed to review; do not let the uncertain p ALSO fail the
			// built-in confidence rule and turn an abstain into a reject.
			confidence = cfg.ActAt
		default:
			confidence = p // the judged confidence replaces the author's in the built-in rule
		}
	}

	base := Decide(ctx, dup, MemoryInput{
		MemoryID:    memoryID,
		Content:     rec.Content,
		ContentHash: fmt.Sprintf("%x", rec.ContentHash),
		Domain:      rec.DomainTag,
		MemType:     string(rec.MemoryType),
		Confidence:  confidence,
	})
	if !base.Accept {
		return finish(GateDecision{Reason: base.Reason})
	}

	p, hi, err := cfg.ask(jctx, map[string]string{"memory": rec.Content}, "lasting", hunch.Lasting)
	if err != nil {
		return GateDecision{}, err
	}
	v := cfg.band(p, hi)
	add("lasting", "", p, v)
	switch v {
	case memory.VerdictReject:
		return finish(GateDecision{Reason: fmt.Sprintf("a remark about its own session, not lasting memory (p=%.2f)", p)})
	case memory.VerdictAbstain:
		abstain = true
		notes = append(notes, fmt.Sprintf("lasting-memory uncertain (p=%.2f)", p))
	}

	if cfg.Neighbours > 0 && len(rec.Embedding) > 0 {
		neighbours, nerr := gs.GateNeighbours(ctx, rec, cfg.Neighbours)
		if nerr != nil {
			return GateDecision{}, nerr
		}
		for _, n := range neighbours {
			if n == nil || n.MemoryID == memoryID {
				continue
			}
			a, _, jerr := cfg.ask(jctx, map[string]string{"a": n.Content, "b": rec.Content}, "agrees", hunch.Agrees)
			if jerr != nil {
				return GateDecision{}, jerr
			}
			r, rHi, jerr := cfg.ask(jctx, map[string]string{"old": n.Content, "new": rec.Content}, "replaces", hunch.Replaces)
			if jerr != nil {
				return GateDecision{}, jerr
			}
			short := shortID(n.MemoryID)
			switch {
			case a >= cfg.ActAt:
				// A restatement is not a replacement, even if the judge also
				// leans "replaces": the duplicate verdict wins.
				add("agrees", n.MemoryID, a, memory.VerdictDuplicate)
				add("replaces", n.MemoryID, r, memory.VerdictNone)
				if cfg.DedupReject && !abstain {
					return finish(GateDecision{Reason: fmt.Sprintf("duplicate of %s (p=%.2f)", short, a)})
				}
				notes = append(notes, fmt.Sprintf("duplicate of %s (p=%.2f)", short, a))
			case r >= cfg.ActAt:
				add("agrees", n.MemoryID, a, memory.VerdictNone)
				add("replaces", n.MemoryID, r, memory.VerdictSupersedes)
				notes = append(notes, fmt.Sprintf("supersedes %s (p=%.2f)", short, r))
			case rHi >= cfg.RejectBelow:
				// Uncertain replacement: recorded for the review queue, never
				// acted on. It does not block the new memory itself.
				add("agrees", n.MemoryID, a, memory.VerdictNone)
				add("replaces", n.MemoryID, r, memory.VerdictUncertain)
				notes = append(notes, fmt.Sprintf("may supersede %s (p=%.2f)", short, r))
			}
		}
	}

	if abstain {
		return finish(GateDecision{Abstain: true, Reason: "gate abstained: " + strings.Join(notes, "; ")})
	}
	reason := fmt.Sprintf("passes checks; lasting p=%.2f", p)
	if len(notes) > 0 {
		reason += "; " + strings.Join(notes, "; ")
	}
	return finish(GateDecision{Accept: true, Reason: reason})
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// truncateReason bounds the final row's reason, which is reused as the vote
// rationale when a cached outcome is voted again.
func truncateReason(s string) string {
	const limit = 400
	if len(s) > limit {
		return s[:limit]
	}
	return s
}
