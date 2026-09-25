package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/memory"
)

func gateMemory(t *testing.T, s *SQLiteStore, id, content string, status memory.MemoryStatus) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.InsertMemory(ctx, testMemory(id, "agent", content, "general-notes")))
	if status != memory.StatusProposed {
		require.NoError(t, s.UpdateStatus(ctx, id, status, time.Now().UTC()))
	}
}

func judged(memoryID, check, target, verdict string, p float64) memory.Judgement {
	pp := p
	return memory.Judgement{MemoryID: memoryID, Check: check, TargetID: target, PYes: &pp,
		Verdict: verdict, JudgeVersion: "test", CreatedAt: time.Now().UTC()}
}

func final(memoryID, verdict, reason string) memory.Judgement {
	return memory.Judgement{MemoryID: memoryID, Check: "final", Verdict: verdict, Reason: reason,
		JudgeVersion: "test", CreatedAt: time.Now().UTC()}
}

func TestWriteGate_ReviewFlow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateMemory(t, s, "m-accepted", "The build server moved to the east rack in March.", memory.StatusProposed)
	gateMemory(t, s, "m-unsure", "The quarterly figure was around two hundred units.", memory.StatusProposed)
	require.NoError(t, s.RecordJudgements(ctx, []memory.Judgement{
		final("m-accepted", memory.VerdictAccept, "passes checks"),
		judged("m-unsure", "lasting", "", memory.VerdictAbstain, 0.7),
		final("m-unsure", memory.VerdictAbstain, "gate abstained: lasting-memory uncertain (p=0.70)"),
	}))

	v, reason, ok, err := s.GateFinal(ctx, "m-unsure")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, memory.VerdictAbstain, v)
	require.Contains(t, reason, "p=0.70")

	queue, err := s.ReviewQueue(ctx, 10)
	require.NoError(t, err)
	require.Len(t, queue, 1, "only the abstained, still-proposed memory waits for review")
	require.Equal(t, "m-unsure", queue[0].MemoryID)
	require.Contains(t, queue[0].Content, "two hundred")

	require.ErrorIs(t, s.SetReviewDecision(ctx, "m-accepted", "accept", "operator", ""), ErrNotAwaitingReview,
		"a memory the gate did not abstain on cannot be decided")
	require.Error(t, s.SetReviewDecision(ctx, "m-unsure", "maybe", "operator", ""))
	require.NoError(t, s.SetReviewDecision(ctx, "m-unsure", "accept", "operator", "checked the source"))
	require.ErrorIs(t, s.SetReviewDecision(ctx, "m-unsure", "reject", "operator", ""), ErrNotAwaitingReview,
		"a decision is final")

	d, ok, err := s.ReviewDecision(ctx, "m-unsure")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "accept", d)

	queue, err = s.ReviewQueue(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, queue, "a decided memory leaves the queue")

	js, err := s.GateJudgements(ctx, "m-unsure")
	require.NoError(t, err)
	require.Len(t, js, 2)
}

func TestWriteGate_EvidenceIsNodeLocalAndBounded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, ok, err := s.MemoryEvidence(ctx, "m1")
	require.NoError(t, err)
	require.False(t, ok, "no evidence means unjudged, not empty evidence")

	require.NoError(t, s.SetMemoryEvidence(ctx, "m1", "Minutes of the 3 March meeting: the rack move was approved."))
	ev, ok, err := s.MemoryEvidence(ctx, "m1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, ev, "rack move was approved")

	require.Error(t, s.SetMemoryEvidence(ctx, "m2", strings.Repeat("x", MaxEvidenceBytes+1)))

	require.NoError(t, s.DeleteMemoryEvidence(ctx, "m1"))
	_, ok, err = s.MemoryEvidence(ctx, "m1")
	require.NoError(t, err)
	require.False(t, ok, "evidence is off-chain and can be removed from the node")
}

func TestWriteGate_SupersededOnlyByCommittedCorrections(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateMemory(t, s, "old", "The office wifi password rotates every 90 days.", memory.StatusCommitted)
	gateMemory(t, s, "fix-pending", "The office wifi password rotates every 30 days.", memory.StatusProposed)
	require.NoError(t, s.RecordJudgements(ctx, []memory.Judgement{
		judged("fix-pending", "replaces", "old", memory.VerdictSupersedes, 0.96),
	}))

	ann, err := s.GateAnnotationsFor(ctx, []string{"old"})
	require.NoError(t, err)
	require.Empty(t, ann.SupersededBy,
		"a correction that has not been committed must not hide what it claims to correct")

	require.NoError(t, s.UpdateStatus(ctx, "fix-pending", memory.StatusCommitted, time.Now().UTC()))
	ann, err = s.GateAnnotationsFor(ctx, []string{"old", "fix-pending"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"old": "fix-pending"}, ann.SupersededBy)
}

func TestWriteGate_ChainsAreClosedInCode(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// F1 is restated by F2; F3 corrects F2; F4 corrects F3. The judge only ever
	// compared pairs, so F1 carries no direct mark — the closure must hide it,
	// and superseded_by must name the survivor F4, not the stale F3.
	gateMemory(t, s, "F1", "The depot opens at 07:00 on weekdays.", memory.StatusCommitted)
	gateMemory(t, s, "F2", "On weekdays the depot opens at seven in the morning.", memory.StatusCommitted)
	gateMemory(t, s, "F3", "The depot now opens at 08:00 on weekdays.", memory.StatusCommitted)
	gateMemory(t, s, "F4", "From May the depot opens at 06:30 on weekdays.", memory.StatusCommitted)
	gateMemory(t, s, "other", "The depot closes at 18:00 on Saturdays.", memory.StatusCommitted)
	require.NoError(t, s.RecordJudgements(ctx, []memory.Judgement{
		judged("F2", "agrees", "F1", memory.VerdictDuplicate, 0.97),
		judged("F3", "replaces", "F2", memory.VerdictSupersedes, 0.95),
		judged("F4", "replaces", "F3", memory.VerdictSupersedes, 0.93),
		judged("other", "replaces", "F4", memory.VerdictUncertain, 0.6),
	}))

	ann, err := s.GateAnnotationsFor(ctx, []string{"F1", "F2", "F3", "F4", "other"})
	require.NoError(t, err)
	require.Equal(t, "F4", ann.SupersededBy["F1"], "restated copy of a corrected fact is hidden too")
	require.Equal(t, "F4", ann.SupersededBy["F2"])
	require.Equal(t, "F4", ann.SupersededBy["F3"])
	_, hidden := ann.SupersededBy["F4"]
	require.False(t, hidden, "an uncertain verdict never hides anything")
	_, hidden = ann.SupersededBy["other"]
	require.False(t, hidden)
}

func TestWriteGate_JudgedConfidenceOnlyWhereJudged(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateMemory(t, s, "with-evidence", "The supplier contract renews on 1 July.", memory.StatusCommitted)
	gateMemory(t, s, "without", "The supplier contract renews every year.", memory.StatusCommitted)
	require.NoError(t, s.RecordJudgements(ctx, []memory.Judgement{
		judged("with-evidence", "supported", "", memory.VerdictPass, 0.94),
	}))
	ann, err := s.GateAnnotationsFor(ctx, []string{"with-evidence", "without"})
	require.NoError(t, err)
	require.InDelta(t, 0.94, ann.JudgedConfidence["with-evidence"], 1e-9)
	_, ok := ann.JudgedConfidence["without"]
	require.False(t, ok, "an unjudged memory has no judged confidence, never a guessed one")
}
