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
	// Judges are consulted on every check; see Policy for how several are
	// combined. Judges from different model families have different blind
	// spots (one hedges on arithmetic and on terse technical text, another
	// confidently calls compatible opposite events — arrived/departed —
	// replacements), so a second judge is a cheap guard against the first's
	// confident mistakes.
	Judges []Judger
	// Policy combines several judges. PolicyLead (default): the FIRST judge
	// leads — a check passes when the lead is at or above ActAt and no other
	// judge is below RejectBelow (a clear objection vetoes); it fails only when
	// every judge is below RejectBelow. PolicyAll: every judge must be at or
	// above ActAt to pass. Duplicate links always use PolicyAll whatever this
	// says: a duplicate joins two memories into one restatement class, so a
	// false one hides a refinement with its original.
	//
	// Measured on a real memory store (two judges): requiring all judges sent
	// a quarter of genuine facts and most tool descriptions to review, because
	// one judge hedges on terse text; the lead-with-veto rule cut review to
	// about a tenth of genuine facts while still rejecting none of them.
	Policy string
	// ExemptDomainPrefixes lists domains whose memories are written by
	// programs (catalogs, generated records), not distilled from a
	// conversation. The gate does not judge them; the built-in checks apply.
	// Asking "is this about the world or about the session?" of a machine
	// record measures nothing useful and rejected most of them in testing.
	ExemptDomainPrefixes []string
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
	if c.Policy != PolicyAll {
		c.Policy = PolicyLead
	}
	return c
}

// Judge-combination policies (see Gate.Policy).
const (
	PolicyLead = "lead"
	PolicyAll  = "all"
)

// answer is every judge's p for one check.
type answer struct{ lead, lo, hi float64 }

// acts reports whether the check clears the act threshold under policy.
func (g Gate) acts(a answer, policy string) bool {
	if policy == PolicyAll {
		return a.lo >= g.ActAt
	}
	return a.lead >= g.ActAt && a.lo >= g.RejectBelow
}

// band maps an answer to pass / abstain / reject under the gate's policy:
// reject always needs every judge below RejectBelow.
func (g Gate) band(a answer) string {
	switch {
	case g.acts(a, g.Policy):
		return memory.VerdictPass
	case a.hi < g.RejectBelow:
		return memory.VerdictReject
	default:
		return memory.VerdictAbstain
	}
}

// recorded is the p stored with a verdict: the value the threshold was
// applied to (the lead's under PolicyLead, the lowest under PolicyAll).
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

// pairAnswers holds the judges' answers for one (neighbour, new) pair.
type pairAnswers struct {
	agrees, replaces answer
	err              error
}

// askPairs asks the duplicate and replacement checks for every neighbour at
// once. Measured judge latency is ~0.6 s per call; asked one after another,
// five neighbours x two checks x two judges kept each memory waiting ~14 s
// for its vote. Concurrently it is about one call's time.
func (g Gate) askPairs(ctx context.Context, neighbours []*memory.MemoryRecord, content string) []pairAnswers {
	out := make([]pairAnswers, len(neighbours))
	var wg sync.WaitGroup
	for i, n := range neighbours {
		if n == nil {
			continue
		}
		wg.Add(1)
		go func(i int, old string) {
			defer wg.Done()
			var aw, rw sync.WaitGroup
			var aErr, rErr error
			aw.Add(1)
			go func() {
				defer aw.Done()
				out[i].agrees, aErr = g.ask(ctx, map[string]string{"a": old, "b": content}, "agrees", hunch.Agrees)
			}()
			rw.Add(1)
			go func() {
				defer rw.Done()
				out[i].replaces, rErr = g.ask(ctx, map[string]string{"old": old, "new": content}, "replaces", hunch.Replaces)
			}()
			aw.Wait()
			rw.Wait()
			if aErr != nil {
				out[i].err = aErr
			} else {
				out[i].err = rErr
			}
		}(i, n.Content)
	}
	wg.Wait()
	return out
}

// ErrExempt means the memory's domain is exempt from the gate; the caller
// uses the built-in checks.
var ErrExempt = errors.New("voter: domain exempt from the write gate")

func (g Gate) exempt(domain string) bool {
	for _, p := range g.ExemptDomainPrefixes {
		if p != "" && strings.HasPrefix(domain, p) {
			return true
		}
	}
	return false
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
	if cfg.exempt(rec.DomainTag) {
		return GateDecision{}, ErrExempt
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
		ans, jerr := cfg.ask(jctx, map[string]string{"memory": rec.Content, "evidence": evidence},
			"supported", hunch.Supported)
		if jerr != nil {
			return GateDecision{}, jerr
		}
		p := cfg.recorded(ans)
		v := cfg.band(ans)
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

	// The neighbour comparison runs alongside the lasting check.
	var neighbours []*memory.MemoryRecord
	if cfg.Neighbours > 0 && len(rec.Embedding) > 0 {
		var nerr error
		if neighbours, nerr = gs.GateNeighbours(ctx, rec, cfg.Neighbours); nerr != nil {
			return GateDecision{}, nerr
		}
	}
	pairsDone := make(chan []pairAnswers, 1)
	go func() { pairsDone <- cfg.askPairs(jctx, neighbours, rec.Content) }()
	ans, err := cfg.ask(jctx, map[string]string{"memory": rec.Content}, "lasting", hunch.Lasting)
	pairs := <-pairsDone
	if err != nil {
		return GateDecision{}, err
	}
	p := cfg.recorded(ans)
	v := cfg.band(ans)
	add("lasting", "", p, v)
	switch v {
	case memory.VerdictReject:
		return finish(GateDecision{Reason: fmt.Sprintf("a remark about its own session, not lasting memory (p=%.2f)", p)})
	case memory.VerdictAbstain:
		abstain = true
		notes = append(notes, fmt.Sprintf("lasting-memory uncertain (p=%.2f)", p))
	}

	{
		for i, n := range neighbours {
			if n == nil || n.MemoryID == memoryID {
				continue
			}
			if pairs[i].err != nil {
				return GateDecision{}, pairs[i].err
			}
			ansA, ansR := pairs[i].agrees, pairs[i].replaces
			// Duplicates always need every judge (see Gate.Policy); replacements
			// follow the gate's policy.
			a, r := ansA.lo, cfg.recorded(ansR)
			short := shortID(n.MemoryID)
			switch {
			case cfg.acts(ansA, PolicyAll):
				// A restatement is not a replacement, even if the judge also
				// leans "replaces": the duplicate verdict wins.
				add("agrees", n.MemoryID, a, memory.VerdictDuplicate)
				add("replaces", n.MemoryID, r, memory.VerdictNone)
				if cfg.DedupReject && !abstain {
					return finish(GateDecision{Reason: fmt.Sprintf("duplicate of %s (p=%.2f)", short, a)})
				}
				notes = append(notes, fmt.Sprintf("duplicate of %s (p=%.2f)", short, a))
			case cfg.acts(ansR, cfg.Policy):
				add("agrees", n.MemoryID, a, memory.VerdictNone)
				add("replaces", n.MemoryID, r, memory.VerdictSupersedes)
				notes = append(notes, fmt.Sprintf("supersedes %s (p=%.2f)", short, r))
			case ansR.hi >= cfg.RejectBelow:
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
