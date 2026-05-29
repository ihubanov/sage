package abci

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/memory"
	"github.com/l33tdawg/sage/internal/poe"
	"github.com/l33tdawg/sage/internal/store"
)

// v8.3 verdict-match crediting: checkAndApplyQuorum credits per-validator
// verdict-correctness EWMA + corroboration count at the FIRST transition into
// a terminal verdict, post-fork only, exactly once. processEpoch then consumes
// those signals instead of the Phase-1 stubs.
//
//   AV1 commit        : accept-voters matched the accept verdict → credited;
//                       the dissenter gets EWMA Update(0.0), no corroboration.
//   AV2 deprecate-tie : reject-voters matched the reject verdict → credited.
//   AV3 idempotency   : no credit before terminal; replayed votes after the
//                       terminal transition credit nothing more.
//   AV4 pre-fork      : crediting suppressed → vstats untouched (replay parity).
//   AV5 processEpoch  : post-fork accuracy == EWMATracker.Accuracy() and
//                       corroboration == CorroborationScore(corrCount).

// vstatsOf reads a validator's on-chain stats record.
func vstatsOf(t *testing.T, app *SageApp, vid string) *store.ValidatorStats {
	t.Helper()
	s, err := app.badgerStore.GetValidatorStats(vid)
	require.NoError(t, err)
	return s
}

// epochScoresFromPending extracts the buffered EpochScore writes after a
// processEpoch run (they flush to the off-chain store in Commit).
func epochScoresFromPending(app *SageApp) map[string]*store.EpochScore {
	out := make(map[string]*store.EpochScore)
	for _, w := range app.pendingWrites {
		if w.writeType == "epoch_score" {
			if es, ok := w.data.(*store.EpochScore); ok {
				out[es.ValidatorID] = es
			}
		}
	}
	return out
}

// activateV83 marks both the v8.2 and v8.3 forks active (app-v4 only ships
// after app-v3 in production, so both gates are set on a real post-fork chain).
func activateV83(app *SageApp, height int64) {
	app.v8_2AppliedHeight = height
	app.v8_3AppliedHeight = height
}

// AV1: a committed memory credits every validator whose vote matched the
// accept verdict; the dissenter gets a zero-outcome EWMA update and no corr.
func TestVerdict_AV1_CommitCreditsMatchers(t *testing.T) {
	app := setupTestApp(t)
	activateV83(app, 100)
	require.True(t, app.postV8_3Fork(200))

	for _, id := range []string{qv0, qv1, qv2, qv3} {
		addQuorumValidator(t, app, id, 0) // PoEWeight 0 → 1/N fallback (equal)
	}
	const memID = "mem-av1"
	seedProposedMemory(t, app, memID)
	recordVote(t, app, memID, qv0, true)
	recordVote(t, app, memID, qv1, true)
	recordVote(t, app, memID, qv2, true)
	recordVote(t, app, memID, qv3, false)

	app.checkAndApplyQuorum(memID, 200, time.Unix(2000, 0))
	require.Equal(t, string(memory.StatusCommitted), statusOf(t, app, memID),
		"3/4 accept = 75%% >= 2/3 → committed")

	for _, id := range []string{qv0, qv1, qv2} {
		s := vstatsOf(t, app, id)
		assert.Equal(t, uint64(1), s.CorrCount, "%s matched accept verdict → corr++", id)
		assert.Equal(t, uint64(1), s.EWMACount, "%s: one verdict participation", id)
		assert.InDelta(t, 1.0, s.EWMAWeightedSum, 1e-12, "%s: EWMA Update(1.0)", id)
		assert.InDelta(t, 1.0, s.EWMAWeightDenom, 1e-12, id)
	}
	s3 := vstatsOf(t, app, qv3)
	assert.Equal(t, uint64(0), s3.CorrCount, "qv3 dissented → no corroboration")
	assert.Equal(t, uint64(1), s3.EWMACount, "qv3 still participated in the verdict")
	assert.InDelta(t, 0.0, s3.EWMAWeightedSum, 1e-12, "qv3: EWMA Update(0.0)")
	assert.InDelta(t, 1.0, s3.EWMAWeightDenom, 1e-12)
}

// AV2: an all-voted tie deprecates the memory; validators who voted REJECT
// matched the (reject) verdict and are credited, accept-voters are not.
func TestVerdict_AV2_DeprecateCreditsRejecters(t *testing.T) {
	app := setupTestApp(t)
	activateV83(app, 100)

	for _, id := range []string{qv0, qv1, qv2, qv3} {
		addQuorumValidator(t, app, id, 0)
	}
	const memID = "mem-av2"
	seedProposedMemory(t, app, memID)
	recordVote(t, app, memID, qv0, true)
	recordVote(t, app, memID, qv1, true)
	recordVote(t, app, memID, qv2, false)
	recordVote(t, app, memID, qv3, false)

	app.checkAndApplyQuorum(memID, 200, time.Unix(2000, 0))
	require.Equal(t, string(memory.StatusDeprecated), statusOf(t, app, memID),
		"2-2 tie = 50%% < 2/3, all voted → deprecated")

	for _, id := range []string{qv2, qv3} {
		s := vstatsOf(t, app, id)
		assert.Equal(t, uint64(1), s.CorrCount, "%s matched the reject verdict → corr++", id)
		assert.InDelta(t, 1.0, s.EWMAWeightedSum, 1e-12, "%s: EWMA Update(1.0)", id)
	}
	for _, id := range []string{qv0, qv1} {
		s := vstatsOf(t, app, id)
		assert.Equal(t, uint64(0), s.CorrCount, "%s voted accept on a rejected memory → no corr", id)
		assert.InDelta(t, 0.0, s.EWMAWeightedSum, 1e-12, "%s: EWMA Update(0.0)", id)
	}
}

// AV3: crediting fires once. No credit while the memory is still proposed; the
// terminal transition credits once; replayed votes afterward credit nothing.
func TestVerdict_AV3_Idempotent(t *testing.T) {
	app := setupTestApp(t)
	activateV83(app, 100)

	for _, id := range []string{qv0, qv1, qv2, qv3} {
		addQuorumValidator(t, app, id, 0)
	}
	const memID = "mem-av3"
	seedProposedMemory(t, app, memID)

	// One vote → not terminal → no credit yet.
	recordVote(t, app, memID, qv0, true)
	app.checkAndApplyQuorum(memID, 200, time.Unix(2000, 0))
	require.Equal(t, string(memory.StatusProposed), statusOf(t, app, memID))
	assert.Zero(t, vstatsOf(t, app, qv0).EWMACount, "no crediting before a terminal verdict")

	// Complete to a commit — credits once.
	recordVote(t, app, memID, qv1, true)
	recordVote(t, app, memID, qv2, true)
	recordVote(t, app, memID, qv3, false)
	app.checkAndApplyQuorum(memID, 201, time.Unix(2000, 0))
	require.Equal(t, string(memory.StatusCommitted), statusOf(t, app, memID))
	require.Equal(t, uint64(1), vstatsOf(t, app, qv0).CorrCount)

	// Replay the votes at later heights — priorStatus is already terminal, so
	// crediting must NOT fire again.
	app.checkAndApplyQuorum(memID, 202, time.Unix(2000, 0))
	app.checkAndApplyQuorum(memID, 203, time.Unix(2000, 0))
	for _, id := range []string{qv0, qv1, qv2} {
		s := vstatsOf(t, app, id)
		assert.Equal(t, uint64(1), s.CorrCount, "%s credited exactly once despite replays", id)
		assert.Equal(t, uint64(1), s.EWMACount, "%s EWMA updated exactly once", id)
	}
}

// AV4: pre-fork, crediting is suppressed entirely — a committed memory leaves
// the vstats EWMA/corroboration fields untouched (byte-identical replay).
func TestVerdict_AV4_PreForkNoCrediting(t *testing.T) {
	app := setupTestApp(t)
	require.Equal(t, int64(0), app.v8_3AppliedHeight, "precondition: v8.3 fork inactive")

	for _, id := range []string{qv0, qv1, qv2, qv3} {
		addQuorumValidator(t, app, id, 0)
	}
	const memID = "mem-av4"
	seedProposedMemory(t, app, memID)
	recordVote(t, app, memID, qv0, true)
	recordVote(t, app, memID, qv1, true)
	recordVote(t, app, memID, qv2, true)
	recordVote(t, app, memID, qv3, false)

	app.checkAndApplyQuorum(memID, 200, time.Unix(2000, 0))
	require.Equal(t, string(memory.StatusCommitted), statusOf(t, app, memID),
		"pre-fork equal weights: 3/4 accept → committed")

	for _, id := range []string{qv0, qv1, qv2, qv3} {
		s := vstatsOf(t, app, id)
		assert.Zero(t, s.EWMACount, "%s: no verdict crediting pre-fork", id)
		assert.Zero(t, s.CorrCount, id)
		assert.Zero(t, s.EWMAWeightedSum, id)
	}
}

// AV5: post-fork processEpoch consumes the verdict-correctness EWMA and the
// corroboration count, not the Phase-1 accept-ratio/zero stubs.
func TestVerdict_AV5_ProcessEpochConsumesSignals(t *testing.T) {
	app := setupTestApp(t)
	activateV83(app, 50)
	require.True(t, app.postV8_3Fork(100))

	addQuorumValidator(t, app, qv0, 0)
	addQuorumValidator(t, app, qv1, 0)

	// qv0 accrues 5 verdict matches; qv1 is never credited (cold-start).
	for i := 0; i < 5; i++ {
		require.NoError(t, app.badgerStore.UpdateVerdictStats(map[string]bool{qv0: true}))
	}

	app.pendingWrites = nil
	app.processEpoch(100, time.Unix(2000, 0))

	scores := epochScoresFromPending(app)
	require.Contains(t, scores, qv0)
	require.Contains(t, scores, qv1)

	s0 := vstatsOf(t, app, qv0)
	ref := &poe.EWMATracker{WeightedSum: s0.EWMAWeightedSum, WeightDenom: s0.EWMAWeightDenom, Count: int64(s0.EWMACount)}
	assert.InDelta(t, ref.Accuracy(), scores[qv0].Accuracy, 1e-12,
		"post-fork accuracy = verdict-correctness EWMA, not accept-ratio")
	assert.InDelta(t, poe.CorroborationScore(int(s0.CorrCount), poe.CorrMax), scores[qv0].CorrScore, 1e-12,
		"post-fork corroboration = CorroborationScore(real count)")
	assert.Equal(t, uint64(5), s0.CorrCount)
	assert.Greater(t, scores[qv0].Accuracy, 0.5, "5 correct verdicts lift accuracy above the 0.5 prior")

	// qv1: no verdicts → cold-start prior 0.5, zero corroboration.
	assert.InDelta(t, 0.5, scores[qv1].Accuracy, 1e-12, "uncredited validator → cold-start prior")
	assert.InDelta(t, 0.0, scores[qv1].CorrScore, 1e-12)
}
