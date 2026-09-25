package voter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/l33tdawg/sage/internal/hunch"
	"github.com/l33tdawg/sage/internal/memory"
)

// Gate is the optional memory gate: before voting, the node asks a Hunch judge
// (github.com/ihubanov/hunch — a calibrated probability read from one
// constrained token's logprobs on a model the operator runs) whether a proposed
// memory is lasting knowledge or a remark about the session it came from.
//
// It exists because the built-in checks trust the author's self-declared
// confidence: an agent that stores "the attachment was lost; the user must
// re-send the numbers" as a 0.9 fact gets it committed, and later sessions
// recall it as if it were true.
//
// Probabilities fall in three bands: pass at or above ActAt, fail below
// RejectBelow, abstain in between. On abstain the node does not vote; the
// memory waits in the operator's review queue and is voted once decided. A
// judge failure falls back to the built-in checks and records nothing.
//
// Everything here is a per-node opinion that shapes this node's vote. The judge
// is not deterministic across nodes, which is why it runs in the voter (whose
// votes may legitimately disagree) and never in the state machine.
type Gate struct {
	// Judges are consulted concurrently; see Policy for how several combine.
	Judges []Judger
	// Policy combines several judges. PolicyLead (default): the FIRST judge
	// leads — a memory passes when the lead is at or above ActAt and no other
	// judge is below RejectBelow; it fails only when every judge is below
	// RejectBelow. PolicyAll: every judge must be at or above ActAt to pass.
	Policy string
	// ActAt is the pass threshold (default 0.9); RejectBelow the fail
	// threshold (default 0.5).
	ActAt       float64
	RejectBelow float64
	// ExemptDomainPrefixes lists domains whose memories are written by
	// programs (catalogs, generated records) rather than distilled from a
	// conversation; the built-in checks apply to them.
	ExemptDomainPrefixes []string
	// Version identifies the judge setup on every judgement row.
	Version string
	// Timeout bounds the judge calls for one memory (default 60s).
	Timeout time.Duration
}

// Judger is the judge the gate consults (a *hunch.Client in production).
type Judger interface {
	YesNo(ctx context.Context, judgeContext any, checks map[string]hunch.Check) (map[string]float64, error)
}

// GateStore is what the gate needs from the node's store beyond Store.
type GateStore interface {
	GetMemory(ctx context.Context, memoryID string) (*memory.MemoryRecord, error)
	RecordJudgements(ctx context.Context, js []memory.Judgement) error
	GateFinal(ctx context.Context, memoryID string) (verdict, reason string, ok bool, err error)
	ReviewDecision(ctx context.Context, memoryID string) (decision string, ok bool, err error)
}

// GateDecision is the gate's verdict for one memory.
type GateDecision struct {
	Accept  bool
	Abstain bool // do not vote; the memory waits in the review queue
	Reason  string
}

// Judge-combination policies (see Gate.Policy).
const (
	PolicyLead = "lead"
	PolicyAll  = "all"
)

// ErrExempt means the memory's domain is exempt from the gate.
var ErrExempt = errors.New("voter: domain exempt from the memory gate")

func (g *Gate) defaults() Gate {
	c := *g
	if c.ActAt <= 0 || c.ActAt > 1 {
		c.ActAt = 0.9
	}
	if c.RejectBelow <= 0 || c.RejectBelow >= c.ActAt {
		c.RejectBelow = 0.5
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	if c.Version == "" {
		c.Version = hunch.ChecksVersion
	}
	if c.Policy != PolicyAll {
		c.Policy = PolicyLead
	}
	return c
}

// answer is every judge's p for one check.
type answer struct{ lead, lo, hi float64 }

func (g Gate) band(a answer) string {
	pass := a.lead >= g.ActAt && a.lo >= g.RejectBelow
	if g.Policy == PolicyAll {
		pass = a.lo >= g.ActAt
	}
	switch {
	case pass:
		return memory.VerdictPass
	case a.hi < g.RejectBelow:
		return memory.VerdictReject
	default:
		return memory.VerdictAbstain
	}
}

// recorded is the p the threshold was applied to.
func (g Gate) recorded(a answer) float64 {
	if g.Policy == PolicyAll {
		return a.lo
	}
	return a.lead
}

// ask puts one check to every judge, concurrently.
func (g Gate) ask(ctx context.Context, judgeContext any, id string, check hunch.Check) (answer, error) {
	ps := make([]float64, len(g.Judges))
	errs := make([]error, len(g.Judges))
	var wg sync.WaitGroup
	for i, j := range g.Judges {
		wg.Add(1)
		go func(i int, j Judger) {
			defer wg.Done()
			res, err := j.YesNo(ctx, judgeContext, map[string]hunch.Check{id: check})
			if err != nil {
				errs[i] = err
				return
			}
			ps[i] = res[id]
		}(i, j)
	}
	wg.Wait()
	a := answer{lo: 1}
	for i, p := range ps {
		if errs[i] != nil {
			return answer{}, errs[i]
		}
		if i == 0 {
			a.lead = p
		}
		if p < a.lo {
			a.lo = p
		}
		if p > a.hi {
			a.hi = p
		}
	}
	return a, nil
}

func (g Gate) exempt(domain string) bool {
	for _, p := range g.ExemptDomainPrefixes {
		if p != "" && strings.HasPrefix(domain, p) {
			return true
		}
	}
	return false
}

// Decide runs the gate for one proposed memory. A memory already judged is not
// sent to the judge again: its recorded outcome is reused, and an abstained
// memory is voted only once the operator has decided it.
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
	if cfg.exempt(rec.DomainTag) {
		return GateDecision{}, ErrExempt
	}
	now := time.Now().UTC()
	var rows []memory.Judgement
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

	base := Decide(ctx, dup, MemoryInput{
		MemoryID:    memoryID,
		Content:     rec.Content,
		ContentHash: fmt.Sprintf("%x", rec.ContentHash),
		Domain:      rec.DomainTag,
		MemType:     string(rec.MemoryType),
		Confidence:  rec.ConfidenceScore,
	})
	if !base.Accept {
		return finish(GateDecision{Reason: base.Reason})
	}

	jctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	ans, err := cfg.ask(jctx, map[string]string{"memory": rec.Content}, "lasting", hunch.Lasting)
	if err != nil {
		return GateDecision{}, err
	}
	p := cfg.recorded(ans)
	v := cfg.band(ans)
	rows = append(rows, memory.Judgement{MemoryID: memoryID, Check: "lasting", PYes: &p, Verdict: v,
		JudgeVersion: cfg.Version, CreatedAt: now})
	switch v {
	case memory.VerdictReject:
		return finish(GateDecision{Reason: fmt.Sprintf("a remark about its own session, not lasting memory (p=%.2f)", p)})
	case memory.VerdictAbstain:
		return finish(GateDecision{Abstain: true, Reason: fmt.Sprintf("gate abstained: lasting-memory uncertain (p=%.2f)", p)})
	}
	return finish(GateDecision{Accept: true, Reason: fmt.Sprintf("passes checks; lasting p=%.2f", p)})
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
