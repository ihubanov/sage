package abci

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/store"
)

// Golden AppHash fixtures — the cross-RELEASE replay anchor.
//
// The per-fork replay suites (v8.2 R1/R2, v8.3 R1–R3, v8.4 R3/R4, v8.5 R1–R4)
// prove parity properties WITHIN one binary: gate off ⇒ keys absent, gate on ⇒
// keys present, two replicas agree. None of them can catch a refactor that
// changes the byte layout (or ComputeAppHash itself) on BOTH sides of the
// comparison at once — e.g. a field reorder in encodeValidatorStats, a key
// rename, or a hash-algorithm tweak would slide through every relative
// assertion while forking every already-shipped chain on its next replay.
//
// These constants pin the digests ABSOLUTELY. Each is SHA-256 over the sorted
// key‖value stream (store.ComputeAppHash) of a keyspace built from fixed
// inputs through the real write paths. If one of these tests fails, a change
// altered bytes that existing chains have already committed — that is a
// consensus fork, not a refactor. Do NOT update a constant unless the change
// is an explicitly fork-gated, governance-activated upgrade and the old bytes
// remain reproducible pre-fork.
const (
	// The empty keyspace — ComputeAppHash's identity element. This is
	// SHA-256(""), which doubles as proof that a fresh store contributes
	// ZERO keys to consensus state (NewBadgerStore's index backfills are
	// no-ops on an empty store).
	goldenEmptyKeyspace = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// The v8.1.2-era keyspace: vstats:<vid> keys carrying the legacy 24-byte
	// record (TotalVotes, AcceptVotes, LastBlockHeight — no PoE-signal bytes).
	// This is the keyspace every pre-v8.2 block must replay onto, byte-for-byte.
	goldenLegacyVstatsKeyspace = "691a3207c1abbcd26a2848c78f52a83f6cfdfeab073d262032b32d7f287c9c05"

	// The post-v8.4 keyspace: the same votes in the 56-byte v8.3 layout, plus
	// one verdict-credit batch (EWMA float64 bit patterns + corroboration
	// counts), an epoch-weight set (poew:), a memory-domain tag (memdomain:),
	// and a per-domain verdict credit (vstats_domain:) — one fixed exemplar of
	// every key prefix the v8.2→v8.4 fork ladder added to consensus state.
	goldenPostV84Keyspace = "d286070c481e121acc19f6c971f6d4d22a684e06e5ddf67d22f406c71d644210"
)

// Fixed validator IDs (shape-realistic 64-hex agent IDs, deterministic).
const (
	goldenVID1 = "1111111111111111111111111111111111111111111111111111111111111111"
	goldenVID2 = "2222222222222222222222222222222222222222222222222222222222222222"
)

// goldenHash computes the AppHash and returns it hex-encoded.
func goldenHash(t *testing.T, bs *store.BadgerStore) string {
	t.Helper()
	h, err := ComputeAppHash(bs)
	require.NoError(t, err)
	return hex.EncodeToString(h)
}

// seedGoldenVotes drives the fixed vote history used by both vstats goldens —
// identical counters, differing only in the encoding the fork flag selects.
func seedGoldenVotes(t *testing.T, bs *store.BadgerStore, v83 bool) {
	t.Helper()
	require.NoError(t, bs.IncrementVoteStats(goldenVID1, true, 90, v83))
	require.NoError(t, bs.IncrementVoteStats(goldenVID1, false, 95, v83))
	require.NoError(t, bs.IncrementVoteStats(goldenVID2, true, 95, v83))
}

func TestReplayGolden_EmptyKeyspace(t *testing.T) {
	bs := setupTestBadger(t)
	got := goldenHash(t, bs)
	t.Logf("actual empty keyspace digest: %s", got)
	assert.Equal(t, goldenEmptyKeyspace, got,
		"the empty-keyspace AppHash changed — ComputeAppHash itself is no longer the algorithm every shipped chain committed with")
}

func TestReplayGolden_LegacyVstatsKeyspace(t *testing.T) {
	bs := setupTestBadger(t)
	seedGoldenVotes(t, bs, false) // pre-fork: legacy 24-byte records
	got := goldenHash(t, bs)
	t.Logf("actual legacy vstats keyspace digest: %s", got)
	assert.Equal(t, goldenLegacyVstatsKeyspace, got,
		"the v8.1.2-era vstats keyspace digest changed — pre-v8.2 blocks no longer replay byte-identical (consensus fork)")
}

func TestReplayGolden_PostV84Keyspace(t *testing.T) {
	bs := setupTestBadger(t)
	// 56-byte vstats records + one verdict-credit batch (EWMA + corr bytes).
	seedGoldenVotes(t, bs, true)
	require.NoError(t, bs.UpdateVerdictStats(map[string]bool{
		goldenVID1: true,
		goldenVID2: false,
	}))
	// One exemplar of each post-v8.2/v8.4 key prefix, fixed inputs.
	require.NoError(t, bs.SetEpochWeights(1, map[string]float64{
		goldenVID1: 0.625,
		goldenVID2: 0.375,
	}))
	require.NoError(t, bs.SetMemoryDomain("mem-golden", "pwn_heap"))
	require.NoError(t, bs.UpdateDomainVerdictStats("pwn_heap", map[string]bool{
		goldenVID1: true,
		goldenVID2: false,
	}))
	got := goldenHash(t, bs)
	t.Logf("actual post-v8.4 keyspace digest: %s", got)
	assert.Equal(t, goldenPostV84Keyspace, got,
		"the post-v8.4 keyspace digest changed — post-fork blocks no longer replay byte-identical (consensus fork)")
}
