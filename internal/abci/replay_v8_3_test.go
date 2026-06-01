package abci

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/memory"
)

// v8.3 (app-v4) replay parity — the value-byte-mutation seam.
//
// v8.3 is the most replay-sensitive fork in the v8 line. v8.2/v8.4/v8.5 only ADD
// new key prefixes (poew:, memdomain:, vstats_domain:) to the AppHash keyspace;
// v8.3 instead MUTATES the VALUE bytes of an already-consensus-hashed key — it
// grows every vstats:<v> record from the legacy 24-byte layout to the 56-byte
// PoE-signal layout (internal/store/badger.go encodeValidatorStats), threaded
// from the abci layer via IncrementVoteStats(..., app.postV8_3Fork(height))
// (app.go) and UpdateVerdictStats (always 56-byte, post-fork only). Because
// ComputeAppHash streams each key's raw value bytes, the parity guarantee hinges
// on the v83 flag: a pre-fork vote MUST write the exact 24 bytes a v8.2 binary
// wrote, and the grown 56-byte record MUST preserve the legacy counters verbatim
// in bytes [0:24]. The per-fork replay template (v8.2 R1/R2, v8.4 R3, v8.5)
// covered every fork EXCEPT the one that rewrites existing values; these pin it.
//
//	R1: pre-fork (v83=false) writes the 24-byte record — the four PoE-signal
//	    fields are absent (decode as zero) and ComputeAppHash is deterministic.
//	R2: identical vote history differing ONLY in the v83 flag yields a DIFFERENT
//	    AppHash (24-byte vs 56-byte value) while the legacy counters are preserved
//	    bit-for-bit — the value-mutation seam, isolated from any count change.
//	R3: a full post-fork vote→terminal-verdict through checkAndApplyQuorum
//	    credits the 56-byte EWMA/corroboration fields and leaves ComputeAppHash
//	    deterministic on re-read.

// R1: a pre-fork vote writes the legacy 24-byte record. The four v8.3 fields are
// not present (they decode as zero — the Phase-1 cold-start values), so the
// keyspace ComputeAppHash sees is byte-identical to what a v8.2 binary produces.
func TestReplayV8_3_R1_PreForkRecordCarriesNoPoESignalBytes(t *testing.T) {
	bs := setupTestBadger(t)
	const vid = "11111111111111111111111111111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// v83=false → legacy 24-byte encoding, byte-identical to v8.2.x.
	require.NoError(t, bs.IncrementVoteStats(vid, true, 90, false))

	s, err := bs.GetValidatorStats(vid)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), s.TotalVotes)
	assert.Equal(t, uint64(1), s.AcceptVotes)
	assert.Equal(t, uint64(90), s.LastBlockHeight)
	// The PoE-signal fields must be absent on a pre-fork record.
	assert.Zero(t, s.EWMAWeightedSum, "pre-fork record must carry no EWMA bytes")
	assert.Zero(t, s.EWMAWeightDenom)
	assert.Zero(t, s.EWMACount)
	assert.Zero(t, s.CorrCount, "pre-fork record must carry no corroboration bytes")

	h1, err := ComputeAppHash(bs)
	require.NoError(t, err)
	h2, err := ComputeAppHash(bs)
	require.NoError(t, err)
	assert.Equal(t, h1, h2,
		"ComputeAppHash must be deterministic over the 24-byte vstats keyspace")
}

// R2: the value-mutation seam, isolated. Two stores start from a byte-identical
// baseline and apply the SAME single vote at the SAME height — differing only in
// the fork flag. The legacy counters in bytes [0:24] must be preserved verbatim,
// but the 56-byte record contributes 32 extra value bytes to the digest, so the
// two AppHashes must diverge. A node that applied the wrong encoding for a height
// would commit a different AppHash and fork the chain.
func TestReplayV8_3_R2_ForkFlagAloneMovesAppHash(t *testing.T) {
	const vid = "22222222222222222222222222222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	pre := setupTestBadger(t)
	post := setupTestBadger(t)

	basePre, err := ComputeAppHash(pre)
	require.NoError(t, err)
	basePost, err := ComputeAppHash(post)
	require.NoError(t, err)
	require.Equal(t, basePre, basePost,
		"two freshly-built stores must share a baseline AppHash")

	require.NoError(t, pre.IncrementVoteStats(vid, true, 90, false)) // pre-fork: 24 bytes
	require.NoError(t, post.IncrementVoteStats(vid, true, 90, true)) // post-fork: 56 bytes

	// Legacy counters survive the 24→56 growth unchanged.
	sPre, err := pre.GetValidatorStats(vid)
	require.NoError(t, err)
	sPost, err := post.GetValidatorStats(vid)
	require.NoError(t, err)
	assert.Equal(t, sPre.TotalVotes, sPost.TotalVotes)
	assert.Equal(t, sPre.AcceptVotes, sPost.AcceptVotes)
	assert.Equal(t, sPre.LastBlockHeight, sPost.LastBlockHeight)

	// The encoding length alone moves the AppHash.
	hPre, err := ComputeAppHash(pre)
	require.NoError(t, err)
	hPost, err := ComputeAppHash(post)
	require.NoError(t, err)
	assert.NotEqual(t, hPre, hPost,
		"24-byte vs 56-byte encoding of the same vote must move the AppHash")

	// Both keyspaces hash deterministically on a re-read.
	hPreReplay, err := ComputeAppHash(pre)
	require.NoError(t, err)
	assert.Equal(t, hPre, hPreReplay)
	hPostReplay, err := ComputeAppHash(post)
	require.NoError(t, err)
	assert.Equal(t, hPost, hPostReplay)
}

// R3: the full post-fork consensus path. A terminal verdict through
// checkAndApplyQuorum writes 56-byte vstats records (EWMA + corroboration). The
// resulting keyspace must hash deterministically — no map-iteration nondeterminism
// in the verdict-credit write may leak into the AppHash.
func TestReplayV8_3_R3_PostForkVerdictCreditsAreDeterministic(t *testing.T) {
	app := setupTestApp(t)
	activateV83(app, 100)
	require.True(t, app.postV8_3Fork(200))

	for _, id := range []string{qv0, qv1, qv2, qv3} {
		addQuorumValidator(t, app, id, 0) // PoEWeight 0 → 1/N fallback (equal)
	}
	const memID = "mem-r8-3"
	seedProposedMemory(t, app, memID)
	recordVote(t, app, memID, qv0, true)
	recordVote(t, app, memID, qv1, true)
	recordVote(t, app, memID, qv2, true)
	recordVote(t, app, memID, qv3, false)

	hBefore, err := ComputeAppHash(app.badgerStore)
	require.NoError(t, err)

	app.checkAndApplyQuorum(memID, 200, time.Unix(2000, 0))
	require.Equal(t, string(memory.StatusCommitted), statusOf(t, app, memID))

	// Terminal verdict wrote 56-byte records (the PoE-signal fields are populated).
	s := vstatsOf(t, app, qv0)
	require.Equal(t, uint64(1), s.EWMACount, "verdict credited the EWMA")
	require.Equal(t, uint64(1), s.CorrCount, "verdict credited corroboration")

	hAfter, err := ComputeAppHash(app.badgerStore)
	require.NoError(t, err)
	assert.NotEqual(t, hBefore, hAfter, "post-fork verdict crediting must move the AppHash")

	hReplay, err := ComputeAppHash(app.badgerStore)
	require.NoError(t, err)
	assert.Equal(t, hAfter, hReplay,
		"ComputeAppHash must be deterministic over the 56-byte vstats keyspace")
}
