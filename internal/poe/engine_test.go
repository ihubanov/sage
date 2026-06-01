package poe

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeWeight(t *testing.T) {
	w := ComputeWeight(0.8, 0.7, 0.9, 0.5)
	assert.Greater(t, w, 0.0)
	assert.Less(t, w, 1.0)
}

func TestLogSpaceEquivalence(t *testing.T) {
	a, d, r, s := 0.8, 0.7, 0.9, 0.5

	// Direct computation
	direct := math.Pow(a, AlphaAccuracy) * math.Pow(d, BetaDomain) *
		math.Pow(r, GammaRecency) * math.Pow(s, DeltaCorroboration)

	logSpace := ComputeWeight(a, d, r, s)
	assert.InDelta(t, direct, logSpace, 1e-10)
}

func TestEpsilonFloor(t *testing.T) {
	// Zero corroboration shouldn't zero the entire weight
	w := ComputeWeight(0.8, 0.7, 0.9, 0.0)
	assert.Greater(t, w, 0.0)
}

// NormalizeWeightsDeterministic must produce byte-identical output across calls
// regardless of Go's randomized map-iteration order (the consensus-safety fix
// from the poe-drift audit), while preserving the rep-cap + sum-to-1 invariants.
func TestNormalizeWeightsDeterministic_StableAcrossCalls(t *testing.T) {
	// A wide magnitude spread across many validators — the regime where the
	// legacy map-order sum is non-associative and can diverge by ULPs.
	weights := map[string]float64{
		"v01": 0.9837261, "v02": 0.0102345, "v03": 0.5500001, "v04": 0.0100000,
		"v05": 0.7321119, "v06": 0.0339211, "v07": 0.9999999, "v08": 0.0410201,
		"v09": 0.1234567, "v10": 0.8765432, "v11": 0.0501234, "v12": 0.6543210,
		"v13": 0.0199999, "v14": 0.4444444, "v15": 0.0876543,
	}

	ref := NormalizeWeightsDeterministic(weights)
	// Re-run many times; sorted-order summation makes every run identical bit-for-bit.
	for i := 0; i < 200; i++ {
		got := NormalizeWeightsDeterministic(weights)
		for id, w := range ref {
			if math.Float64bits(got[id]) != math.Float64bits(w) {
				t.Fatalf("run %d: non-deterministic weight for %s: %x != %x", i, id, math.Float64bits(got[id]), math.Float64bits(w))
			}
		}
	}

	// Invariant preserved: normalized weights sum to ~1.0 (the rep-cap loop is
	// bounded at 10 iterations and need not drive every weight strictly under
	// 10% for an arbitrary spread — see TestRepCap — so we only assert the sum).
	var sum float64
	for _, w := range ref {
		sum += w
	}
	assert.InDelta(t, 1.0, sum, 1e-9, "normalized weights sum to 1")
}

// TestNormalizeWeights_LegacyMapOrderSumDiverges is the regression guard the
// v8.4 (app-v5) consensus-split fix was missing: it proves the legacy
// map-iteration-order summation is genuinely non-deterministic on a *realistic*
// heterogeneous weight set — so NormalizeWeightsDeterministic's sorted-key sum is
// load-bearing, not a no-op rename. The StableAcrossCalls / EqualWeightsMatchLegacy
// tests only show the fix is self-consistent and harmless on equal weights; this
// one demonstrates the bug it actually fixes.
//
// Mechanism: sumWeights totals a Go map. The legacy path (deterministic=false)
// walks the map in Go's per-process randomized order; the consensus path
// (deterministic=true) walks sorted keys. IEEE-754 float64 addition is
// non-associative, so on weights spanning a wide enough magnitude range the two
// orderings land on different bits. Those exact bits scale every persisted
// poew:<id> weight into the AppHash, so a node summing in a different order would
// commit a divergent AppHash. We compute the same multiset's total in sorted-key
// order (what the consensus path uses) and in reverse-key order (an equally-legal
// order the legacy randomized walk can produce verbatim) and show they differ at
// the bit level — then confirm the deterministic variant pins exactly one result.
func TestNormalizeWeights_LegacyMapOrderSumDiverges(t *testing.T) {
	// The same 15-validator realistic spread used by StableAcrossCalls:
	// ComputeWeight-scale values with ≥3 distinct magnitudes. (Empirically,
	// sorted-vs-reverse summation of this set differs by 1 ULP.)
	weights := map[string]float64{
		"v01": 0.9837261, "v02": 0.0102345, "v03": 0.5500001, "v04": 0.0100000,
		"v05": 0.7321119, "v06": 0.0339211, "v07": 0.9999999, "v08": 0.0410201,
		"v09": 0.1234567, "v10": 0.8765432, "v11": 0.0501234, "v12": 0.6543210,
		"v13": 0.0199999, "v14": 0.4444444, "v15": 0.0876543,
	}

	// Sorted-key total: exactly what sumWeights(weights, true) — the post-v8.4
	// consensus path — accumulates.
	sortedTotal := sumWeights(weights, true)

	// Reverse-key total: a legal Go map-iteration order, so the legacy
	// sumWeights(weights, false) used pre-fork can land here on some process.
	revIDs := make([]string, 0, len(weights))
	for id := range weights {
		revIDs = append(revIDs, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(revIDs)))
	var revTotal float64
	for _, id := range revIDs {
		revTotal += weights[id]
	}

	require.NotEqual(t, math.Float64bits(sortedTotal), math.Float64bits(revTotal),
		"expected an order-sensitive sum (the hazard the v8.4 fix guards): "+
			"sorted=%#x rev=%#x — if this ever matches, choose a weight set with a "+
			"wider magnitude spread", math.Float64bits(sortedTotal), math.Float64bits(revTotal))

	// The deterministic variant must pin exactly the sorted-key result, every
	// call, regardless of map seed — and still satisfy the sum-to-1 invariant.
	ref := NormalizeWeightsDeterministic(weights)
	for i := 0; i < 64; i++ {
		got := NormalizeWeightsDeterministic(weights)
		for id, w := range ref {
			assert.Equal(t, math.Float64bits(w), math.Float64bits(got[id]),
				"run %d: deterministic normalize must be bit-stable for %s", i, id)
		}
	}
	var sum float64
	for _, w := range ref {
		sum += w
	}
	assert.InDelta(t, 1.0, sum, 1e-9, "normalized weights sum to 1")
}

// Replay parity for the common case: with equal weights (the regime real
// devnets and single-node chains run in — the float sum is order-insensitive),
// the deterministic variant returns bits identical to the legacy one, so the
// v8.4 fork-gate does not disturb existing chains.
func TestNormalizeWeightsDeterministic_EqualWeightsMatchLegacy(t *testing.T) {
	for _, n := range []int{1, 3, 4, 7, 20} {
		weights := make(map[string]float64, n)
		for i := 0; i < n; i++ {
			weights[fmt.Sprintf("v%02d", i)] = 1.0
		}
		legacy := NormalizeWeights(weights)
		det := NormalizeWeightsDeterministic(weights)
		for id, w := range legacy {
			assert.Equal(t, math.Float64bits(w), math.Float64bits(det[id]),
				"n=%d %s: deterministic must match legacy bit-for-bit on equal weights", n, id)
		}
	}
}

func TestRepCap(t *testing.T) {
	// Use enough validators (20) so the 10% cap is achievable
	weights := make(map[string]float64, 20)
	weights["v1"] = 1000.0 // dominant validator
	for i := 2; i <= 20; i++ {
		weights[fmt.Sprintf("v%d", i)] = 1.0
	}
	normalized := NormalizeWeights(weights)

	// The dominant validator should be capped at RepCap (10%)
	assert.LessOrEqual(t, normalized["v1"], RepCap+0.001)

	// Sum should be ~1.0
	var sum float64
	for _, w := range normalized {
		sum += w
	}
	assert.InDelta(t, 1.0, sum, 0.01)

	// With 4 validators, cap can't bring everyone below 10%.
	// Test that dominant weight is at least reduced relative to others.
	smallWeights := map[string]float64{
		"a": 100.0,
		"b": 1.0,
		"c": 1.0,
		"d": 1.0,
	}
	smallNormalized := NormalizeWeights(smallWeights)
	var smallSum float64
	for _, w := range smallNormalized {
		smallSum += w
	}
	assert.InDelta(t, 1.0, smallSum, 0.01)
	// Dominant validator should be reduced from 97% to at most 25%
	assert.Less(t, smallNormalized["a"], 0.30)
}

func TestEWMAConvergence(t *testing.T) {
	tracker := NewEWMATracker()

	// Feed consistently correct outcomes
	for i := 0; i < 50; i++ {
		tracker.Update(1.0)
	}
	assert.Greater(t, tracker.Accuracy(), 0.9)

	// Feed consistently wrong outcomes
	for i := 0; i < 100; i++ {
		tracker.Update(0.0)
	}
	assert.Less(t, tracker.Accuracy(), 0.3)
}

func TestColdStartBlend(t *testing.T) {
	tracker := NewEWMATracker()
	// With zero observations, should return prior
	assert.Equal(t, coldStartPrior, tracker.Accuracy())

	// With few observations, should be blended
	tracker.Update(1.0)
	acc := tracker.Accuracy()
	assert.Greater(t, acc, coldStartPrior)
	assert.Less(t, acc, 1.0)
}

func TestPhiCoefficient(t *testing.T) {
	tracker := NewPhiTracker(50)

	// Perfect correlation: both always agree
	for i := 0; i < 30; i++ {
		tracker.RecordJointVote("v1", "v2", true, true, false)
	}
	for i := 0; i < 20; i++ {
		tracker.RecordJointVote("v1", "v2", false, false, false)
	}

	phi := tracker.PhiCoefficient("v1", "v2")
	assert.Greater(t, phi, 0.8)
}

func TestCollusionDetection(t *testing.T) {
	tracker := NewPhiTracker(50)

	// High correlation
	for i := 0; i < 50; i++ {
		accept := i%3 != 0
		tracker.RecordJointVote("v1", "v2", accept, accept, false)
	}

	assert.True(t, tracker.IsCollusion("v1", "v2"))
}

func TestNoCollusionIndependent(t *testing.T) {
	tracker := NewPhiTracker(50)

	// Independent voting
	for i := 0; i < 50; i++ {
		tracker.RecordJointVote("v1", "v2", i%2 == 0, i%3 == 0, false)
	}

	assert.False(t, tracker.IsCollusion("v1", "v2"))
}

func TestEpochBoundary(t *testing.T) {
	assert.False(t, IsEpochBoundary(0))
	assert.False(t, IsEpochBoundary(50))
	assert.True(t, IsEpochBoundary(100))
	assert.False(t, IsEpochBoundary(150))
	assert.True(t, IsEpochBoundary(200))
}

func TestEpochNumber(t *testing.T) {
	assert.Equal(t, int64(0), EpochNumber(50))
	assert.Equal(t, int64(1), EpochNumber(100))
	assert.Equal(t, int64(1), EpochNumber(150))
	assert.Equal(t, int64(2), EpochNumber(200))
}

func TestRecencyDecay(t *testing.T) {
	now := time.Now()

	// Just active: score should be ~1.0
	recent := RecencyScore(now.Add(-1*time.Minute), now)
	assert.InDelta(t, 1.0, recent, 0.01)

	// 100 hours ago: should be lower
	old := RecencyScore(now.Add(-100*time.Hour), now)
	assert.Less(t, old, recent)
	assert.Greater(t, old, 0.0)
}

func TestCorroborationScore(t *testing.T) {
	s0 := CorroborationScore(0, 20)
	s5 := CorroborationScore(5, 20)
	s20 := CorroborationScore(20, 20)

	assert.Equal(t, 0.0, s0)
	assert.Greater(t, s5, s0)
	assert.InDelta(t, 1.0, s20, 0.001) // At max, score should be 1.0
	assert.Greater(t, s20, s5)
}
