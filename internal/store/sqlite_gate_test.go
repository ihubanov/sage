package store

import (
	"context"
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

func judged(memoryID, check, verdict string, p float64) memory.Judgement {
	pp := p
	return memory.Judgement{MemoryID: memoryID, Check: check, PYes: &pp,
		Verdict: verdict, JudgeVersion: "test", CreatedAt: time.Now().UTC()}
}

func final(memoryID, verdict, reason string) memory.Judgement {
	return memory.Judgement{MemoryID: memoryID, Check: "final", Verdict: verdict, Reason: reason,
		JudgeVersion: "test", CreatedAt: time.Now().UTC()}
}

func TestMemoryGate_ReviewFlow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	gateMemory(t, s, "m-accepted", "The build server moved to the east rack in March.", memory.StatusProposed)
	gateMemory(t, s, "m-unsure", "The quarterly figure was around two hundred units.", memory.StatusProposed)
	require.NoError(t, s.RecordJudgements(ctx, []memory.Judgement{
		final("m-accepted", memory.VerdictAccept, "passes checks"),
		judged("m-unsure", "lasting", memory.VerdictAbstain, 0.7),
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
