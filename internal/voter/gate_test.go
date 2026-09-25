package voter

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/hunch"
	"github.com/l33tdawg/sage/internal/memory"
)

// fakeJudge answers each check id with a fixed p (or an error) and counts calls.
type fakeJudge struct {
	mu    sync.Mutex
	p     map[string]float64
	err   error
	calls int
}

func (f *fakeJudge) YesNo(_ context.Context, _ any, checks map[string]hunch.Check) (map[string]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]float64{}
	for id := range checks {
		p, ok := f.p[id]
		if !ok {
			return nil, hunch.ErrNoVerdict
		}
		out[id] = p
	}
	return out, nil
}

type fakeGateStore struct {
	mems       map[string]*memory.MemoryRecord
	neighbours []*memory.MemoryRecord
	evidence   map[string]string
	judgements []memory.Judgement
	review     map[string]string
}

func newFakeGateStore(rec *memory.MemoryRecord) *fakeGateStore {
	return &fakeGateStore{mems: map[string]*memory.MemoryRecord{rec.MemoryID: rec},
		evidence: map[string]string{}, review: map[string]string{}}
}

func (f *fakeGateStore) GetMemory(_ context.Context, id string) (*memory.MemoryRecord, error) {
	m, ok := f.mems[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return m, nil
}
func (f *fakeGateStore) GateNeighbours(context.Context, *memory.MemoryRecord, int) ([]*memory.MemoryRecord, error) {
	return f.neighbours, nil
}
func (f *fakeGateStore) RecordJudgements(_ context.Context, js []memory.Judgement) error {
	f.judgements = append(f.judgements, js...)
	return nil
}
func (f *fakeGateStore) GateFinal(_ context.Context, id string) (string, string, bool, error) {
	for i := len(f.judgements) - 1; i >= 0; i-- {
		j := f.judgements[i]
		if j.MemoryID == id && j.Check == "final" {
			return j.Verdict, j.Reason, true, nil
		}
	}
	return "", "", false, nil
}
func (f *fakeGateStore) ReviewDecision(_ context.Context, id string) (string, bool, error) {
	d, ok := f.review[id]
	return d, ok, nil
}
func (f *fakeGateStore) MemoryEvidence(_ context.Context, id string) (string, bool, error) {
	e, ok := f.evidence[id]
	return e, ok, nil
}

func (f *fakeGateStore) verdict(check, target string) string {
	for _, j := range f.judgements {
		if j.Check == check && j.TargetID == target {
			return j.Verdict
		}
	}
	return ""
}

type noDups struct{}

func (noDups) FindByContentHash(context.Context, string, string) (bool, error) { return false, nil }

func rec(id, content string, typ memory.MemoryType, conf float64) *memory.MemoryRecord {
	h := sha256.Sum256([]byte(content))
	return &memory.MemoryRecord{MemoryID: id, Content: content, ContentHash: h[:], MemoryType: typ,
		DomainTag: "general-notes", ConfidenceScore: conf, Embedding: []float32{0.1, 0.2}}
}

func TestGate_SessionStateRemarkIsRejected(t *testing.T) {
	// The motivating failure: a remark about the agent's own session, stored as
	// a high-confidence fact. The built-in checks pass it (0.9 >= 0.7).
	m := rec("m1", "The attachment was lost; the user must re-send the ten numbers.", memory.TypeFact, 0.9)
	gs := newFakeGateStore(m)
	j := &fakeJudge{p: map[string]float64{"lasting": 0.04}}
	d, err := (&Gate{Judges: []Judger{j}}).Decide(context.Background(), gs, noDups{}, "m1")
	require.NoError(t, err)
	require.False(t, d.Accept)
	require.False(t, d.Abstain)
	require.Contains(t, d.Reason, "not lasting memory")
	require.Equal(t, memory.VerdictReject, gs.verdict("final", ""))
}

func TestGate_UncertainAbstainsUntilReviewed(t *testing.T) {
	m := rec("m1", "The shipment count for the quarter was around two hundred.", memory.TypeFact, 0.9)
	gs := newFakeGateStore(m)
	j := &fakeJudge{p: map[string]float64{"lasting": 0.7}}
	g := &Gate{Judges: []Judger{j}, Neighbours: 0}

	d, err := g.Decide(context.Background(), gs, noDups{}, "m1")
	require.NoError(t, err)
	require.True(t, d.Abstain, "0.5-0.9 is neither pass nor reject: no vote, the memory waits for review")

	calls := j.calls
	d, err = g.Decide(context.Background(), gs, noDups{}, "m1")
	require.NoError(t, err)
	require.True(t, d.Abstain)
	require.Equal(t, calls, j.calls, "a judged memory is not sent to the judge again")

	gs.review["m1"] = memory.VerdictAccept
	d, err = g.Decide(context.Background(), gs, noDups{}, "m1")
	require.NoError(t, err)
	require.True(t, d.Accept)
	require.False(t, d.Abstain)
	require.Contains(t, d.Reason, "review: accept")
}

func TestGate_MarksSupersedeWithoutMutating(t *testing.T) {
	m := rec("new", "The depot now opens at 08:00 on weekdays.", memory.TypeFact, 0.9)
	gs := newFakeGateStore(m)
	gs.neighbours = []*memory.MemoryRecord{rec("old", "The depot opens at 07:00 on weekdays.", memory.TypeFact, 0.9)}
	j := &fakeJudge{p: map[string]float64{"lasting": 0.97, "agrees": 0.03, "replaces": 0.95}}
	d, err := (&Gate{Judges: []Judger{j}, Neighbours: 5}).Decide(context.Background(), gs, noDups{}, "new")
	require.NoError(t, err)
	require.True(t, d.Accept, "the correction itself is accepted")
	require.Contains(t, d.Reason, "supersedes old")
	require.Equal(t, memory.VerdictSupersedes, gs.verdict("replaces", "old"),
		"the old memory is MARKED as superseded (node-local), never challenged or deprecated")
}

func TestGate_DuplicateIsMarkedUnlessDedupRejectIsOn(t *testing.T) {
	for _, dedupReject := range []bool{false, true} {
		m := rec("new", "On weekdays the depot opens at seven in the morning.", memory.TypeFact, 0.9)
		gs := newFakeGateStore(m)
		gs.neighbours = []*memory.MemoryRecord{rec("old", "The depot opens at 07:00 on weekdays.", memory.TypeFact, 0.9)}
		// A restatement the judge also leans "replaces" on: the duplicate wins.
		j := &fakeJudge{p: map[string]float64{"lasting": 0.97, "agrees": 0.96, "replaces": 0.91}}
		d, err := (&Gate{Judges: []Judger{j}, DedupReject: dedupReject, Neighbours: 5}).Decide(context.Background(), gs, noDups{}, "new")
		require.NoError(t, err)
		require.Equal(t, !dedupReject, d.Accept)
		require.Equal(t, memory.VerdictDuplicate, gs.verdict("agrees", "old"))
		require.Equal(t, memory.VerdictNone, gs.verdict("replaces", "old"),
			"a restatement is never recorded as a replacement")
	}
}

func TestGate_EvidenceJudgesConfidence(t *testing.T) {
	t.Run("unsupported is rejected", func(t *testing.T) {
		m := rec("m1", "The contract renews on 1 July.", memory.TypeFact, 0.95)
		gs := newFakeGateStore(m)
		gs.evidence["m1"] = "Email thread about the office party."
		j := &fakeJudge{p: map[string]float64{"supported": 0.08, "lasting": 0.99}}
		d, err := (&Gate{Judges: []Judger{j}}).Decide(context.Background(), gs, noDups{}, "m1")
		require.NoError(t, err)
		require.False(t, d.Accept)
		require.Contains(t, d.Reason, "not supported by its evidence")
	})
	t.Run("no evidence means unjudged, never a guessed score", func(t *testing.T) {
		m := rec("m1", "The contract renews on 1 July.", memory.TypeFact, 0.95)
		gs := newFakeGateStore(m)
		j := &fakeJudge{p: map[string]float64{"lasting": 0.99}} // no "supported" answer on purpose
		d, err := (&Gate{Judges: []Judger{j}, Neighbours: 0}).Decide(context.Background(), gs, noDups{}, "m1")
		require.NoError(t, err)
		require.True(t, d.Accept)
		require.Equal(t, "", gs.verdict("supported", ""), "the supported check is only asked with evidence")
	})
	t.Run("uncertain support abstains instead of failing the built-in 0.7 rule", func(t *testing.T) {
		m := rec("m1", "The contract renews on 1 July.", memory.TypeFact, 0.95)
		gs := newFakeGateStore(m)
		gs.evidence["m1"] = "Renewal terms: annual; the date is set in the schedule."
		j := &fakeJudge{p: map[string]float64{"supported": 0.6, "lasting": 0.99}}
		d, err := (&Gate{Judges: []Judger{j}, Neighbours: 0}).Decide(context.Background(), gs, noDups{}, "m1")
		require.NoError(t, err)
		require.True(t, d.Abstain)
	})
}

func TestGate_TwoJudgesMustAgreeToAct(t *testing.T) {
	m := rec("new", "Some staff arrived at the depot at 09:00.", memory.TypeObservation, 0.9)
	gs := newFakeGateStore(m)
	gs.neighbours = []*memory.MemoryRecord{rec("old", "Some staff left the depot at 09:00.", memory.TypeObservation, 0.9)}
	// One judge confidently calls compatible opposite events a replacement; the
	// other does not. Disagreement must never produce a mark.
	confident := &fakeJudge{p: map[string]float64{"lasting": 0.98, "agrees": 0.02, "replaces": 1.0}}
	careful := &fakeJudge{p: map[string]float64{"lasting": 0.96, "agrees": 0.02, "replaces": 0.12}}
	d, err := (&Gate{Judges: []Judger{confident, careful}, Neighbours: 5}).Decide(context.Background(), gs, noDups{}, "new")
	require.NoError(t, err)
	require.True(t, d.Accept)
	require.NotEqual(t, memory.VerdictSupersedes, gs.verdict("replaces", "old"))
	require.Equal(t, memory.VerdictUncertain, gs.verdict("replaces", "old"),
		"disagreement is recorded for review, never acted on")
}

func TestGate_JudgeFailureRecordsNothing(t *testing.T) {
	m := rec("m1", "The depot opens at 07:00 on weekdays.", memory.TypeFact, 0.9)
	gs := newFakeGateStore(m)
	j := &fakeJudge{err: errors.New("connection refused")}
	_, err := (&Gate{Judges: []Judger{j}}).Decide(context.Background(), gs, noDups{}, "m1")
	require.Error(t, err, "the caller falls back to the built-in checks")
	require.Empty(t, gs.judgements, "a failed judgement leaves no partial verdict behind")
}

func TestGate_BuiltInRejectionsStillApply(t *testing.T) {
	m := rec("m1", "too short", memory.TypeFact, 0.9)
	gs := newFakeGateStore(m)
	j := &fakeJudge{p: map[string]float64{"lasting": 0.99}}
	d, err := (&Gate{Judges: []Judger{j}}).Decide(context.Background(), gs, noDups{}, "m1")
	require.NoError(t, err)
	require.False(t, d.Accept)
	require.Contains(t, d.Reason, "too short")
	require.Equal(t, 0, j.calls, "no judge call is spent on a memory the built-in checks already reject")
}

func TestGate_LeadPolicyLetsTheLeadDecideUnlessVetoed(t *testing.T) {
	newGate := func(policy string, judges ...Judger) *Gate {
		return &Gate{Judges: judges, Policy: policy, Neighbours: 5}
	}
	setup := func() *fakeGateStore {
		gs := newFakeGateStore(rec("new", "The depot now opens at 08:00 on weekdays.", memory.TypeFact, 0.9))
		gs.neighbours = []*memory.MemoryRecord{rec("old", "The depot opens at 07:00 on weekdays.", memory.TypeFact, 0.9)}
		return gs
	}
	lead := &fakeJudge{p: map[string]float64{"lasting": 0.97, "agrees": 0.05, "replaces": 0.95}}
	hedging := &fakeJudge{p: map[string]float64{"lasting": 0.62, "agrees": 0.05, "replaces": 0.66}}

	gs := setup()
	d, err := newGate(PolicyLead, lead, hedging).Decide(context.Background(), gs, noDups{}, "new")
	require.NoError(t, err)
	require.True(t, d.Accept, "a hedging second judge does not block a sure lead")
	require.Equal(t, memory.VerdictSupersedes, gs.verdict("replaces", "old"))

	gs = setup()
	d, err = newGate(PolicyAll, lead, hedging).Decide(context.Background(), gs, noDups{}, "new")
	require.NoError(t, err)
	require.True(t, d.Abstain, "under PolicyAll the same hedge sends the memory to review")

	objecting := &fakeJudge{p: map[string]float64{"lasting": 0.97, "agrees": 0.05, "replaces": 0.2}}
	gs = setup()
	_, err = newGate(PolicyLead, lead, objecting).Decide(context.Background(), gs, noDups{}, "new")
	require.NoError(t, err)
	require.Equal(t, memory.VerdictUncertain, gs.verdict("replaces", "old"),
		"a clear objection (below 0.5) vetoes the lead")
}

func TestGate_DuplicatesAlwaysNeedEveryJudge(t *testing.T) {
	gs := newFakeGateStore(rec("new", "The depot opens at 07:00 every weekday.", memory.TypeFact, 0.9))
	gs.neighbours = []*memory.MemoryRecord{rec("old", "The depot opens at 07:00 on weekdays.", memory.TypeFact, 0.9)}
	lead := &fakeJudge{p: map[string]float64{"lasting": 0.97, "agrees": 0.97, "replaces": 0.05}}
	hedging := &fakeJudge{p: map[string]float64{"lasting": 0.95, "agrees": 0.7, "replaces": 0.05}}
	_, err := (&Gate{Judges: []Judger{lead, hedging}, Policy: PolicyLead, Neighbours: 5}).Decide(
		context.Background(), gs, noDups{}, "new")
	require.NoError(t, err)
	require.NotEqual(t, memory.VerdictDuplicate, gs.verdict("agrees", "old"),
		"a duplicate joins a restatement class, so the lead alone cannot create one")
}

func TestGate_ExemptDomainsAreNotJudged(t *testing.T) {
	m := rec("m1", "tool_x: lists files under the given directory.", memory.TypeFact, 0.95)
	m.DomainTag = "catalog.tools"
	gs := newFakeGateStore(m)
	j := &fakeJudge{p: map[string]float64{"lasting": 0.01}}
	_, err := (&Gate{Judges: []Judger{j}, ExemptDomainPrefixes: []string{"catalog."}}).Decide(
		context.Background(), gs, noDups{}, "m1")
	require.ErrorIs(t, err, ErrExempt)
	require.Equal(t, 0, j.calls)
	require.Empty(t, gs.judgements, "an exempt memory leaves no judgement behind")
}
