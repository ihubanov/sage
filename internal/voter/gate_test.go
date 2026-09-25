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
	judgements []memory.Judgement
	review     map[string]string
}

func newFakeGateStore(rec *memory.MemoryRecord) *fakeGateStore {
	return &fakeGateStore{mems: map[string]*memory.MemoryRecord{rec.MemoryID: rec}, review: map[string]string{}}
}

func (f *fakeGateStore) GetMemory(_ context.Context, id string) (*memory.MemoryRecord, error) {
	m, ok := f.mems[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return m, nil
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

func (f *fakeGateStore) verdict(check string) string {
	for _, j := range f.judgements {
		if j.Check == check {
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
		DomainTag: "general-notes", ConfidenceScore: conf}
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
	require.Equal(t, memory.VerdictReject, gs.verdict("final"))
}

func TestGate_LastingFactIsAccepted(t *testing.T) {
	m := rec("m1", "The depot opens at 07:00 on weekdays.", memory.TypeFact, 0.9)
	gs := newFakeGateStore(m)
	d, err := (&Gate{Judges: []Judger{&fakeJudge{p: map[string]float64{"lasting": 0.97}}}}).Decide(
		context.Background(), gs, noDups{}, "m1")
	require.NoError(t, err)
	require.True(t, d.Accept)
	require.Contains(t, d.Reason, "lasting p=0.97")
}

func TestGate_UncertainAbstainsUntilReviewed(t *testing.T) {
	m := rec("m1", "The shipment count for the quarter was around two hundred.", memory.TypeFact, 0.9)
	gs := newFakeGateStore(m)
	j := &fakeJudge{p: map[string]float64{"lasting": 0.7}}
	g := &Gate{Judges: []Judger{j}}

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
	require.Contains(t, d.Reason, "review: accept")
}

func TestGate_LeadPolicyLetsTheLeadDecideUnlessVetoed(t *testing.T) {
	m := rec("m1", "The depot opens at 07:00 on weekdays.", memory.TypeFact, 0.9)
	lead := &fakeJudge{p: map[string]float64{"lasting": 0.97}}
	hedging := &fakeJudge{p: map[string]float64{"lasting": 0.62}}
	objecting := &fakeJudge{p: map[string]float64{"lasting": 0.2}}

	d, err := (&Gate{Judges: []Judger{lead, hedging}}).Decide(context.Background(), newFakeGateStore(m), noDups{}, "m1")
	require.NoError(t, err)
	require.True(t, d.Accept, "a hedging second judge does not block a sure lead")

	d, err = (&Gate{Judges: []Judger{lead, hedging}, Policy: PolicyAll}).Decide(context.Background(), newFakeGateStore(m), noDups{}, "m1")
	require.NoError(t, err)
	require.True(t, d.Abstain, "under PolicyAll the same hedge sends the memory to review")

	d, err = (&Gate{Judges: []Judger{lead, objecting}}).Decide(context.Background(), newFakeGateStore(m), noDups{}, "m1")
	require.NoError(t, err)
	require.True(t, d.Abstain, "a clear objection vetoes the lead; one objection alone never rejects")
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
	require.Empty(t, gs.judgements)
}

func TestGate_JudgeFailureRecordsNothing(t *testing.T) {
	m := rec("m1", "The depot opens at 07:00 on weekdays.", memory.TypeFact, 0.9)
	gs := newFakeGateStore(m)
	_, err := (&Gate{Judges: []Judger{&fakeJudge{err: errors.New("connection refused")}}}).Decide(
		context.Background(), gs, noDups{}, "m1")
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
