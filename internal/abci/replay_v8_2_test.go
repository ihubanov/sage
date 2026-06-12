package abci

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/store"
	"github.com/l33tdawg/sage/internal/validator"
)

// v8.2 replay parity. The plan's "Hard constraint" is that pre-fork blocks
// replay byte-identical to v8.1.2 — no poew:* keys touch the AppHash unless
// the v8.2 fork is past activation.
//
//   R1: processEpoch with v8_2AppliedHeight == 0 produces an AppHash that
//       does NOT include any poew:* keys.
//   R2: processEpoch with v8_2AppliedHeight set and height > activation
//       DOES include poew:* keys in the resulting AppHash. AppHashes
//       between R1 and R2 must differ; the only difference is the poew:*
//       contribution.

// freshReplayApp returns a SageApp built on its own temp store, ready to be
// driven by direct processEpoch calls. The store stays scoped to the test
// so AppHash snapshots cannot leak across cases.
func freshReplayApp(t *testing.T) (*SageApp, string) {
	t.Helper()
	tmp := t.TempDir()
	badgerDir := filepath.Join(tmp, "badger")
	sqlitePath := filepath.Join(tmp, "off.db")

	bs, err := store.NewBadgerStore(badgerDir)
	require.NoError(t, err)
	t.Cleanup(func() { bs.CloseBadger() })

	sqlite, err := store.NewSQLiteStore(context.Background(), sqlitePath)
	require.NoError(t, err)
	t.Cleanup(func() { sqlite.Close() })

	app, err := NewSageAppWithStores(bs, sqlite, zerolog.Nop())
	require.NoError(t, err)

	for _, vid := range []string{"v-r-0", "v-r-1", "v-r-2", "v-r-3"} {
		require.NoError(t, app.validators.AddValidator(&validator.ValidatorInfo{
			ID:    vid,
			Power: 1,
		}))
		require.NoError(t, app.badgerStore.IncrementVoteStats(vid, true, 90, false))
	}
	return app, badgerDir
}

// hasPoEWKeys returns true iff at least one poew:* key exists in the store.
func hasPoEWKeys(t *testing.T, app *SageApp) bool {
	t.Helper()
	_, ok, err := app.badgerStore.GetEpochWeights()
	require.NoError(t, err)
	return ok
}

// R1: with v8_2AppliedHeight == 0, processEpoch runs but does NOT write
// poew:* keys. The AppHash on this store is therefore byte-identical to a
// v8.1.2 binary that never knew about the v8.2 code path. We can't compare
// to a literal v8.1.2 hash without a fixture, but we CAN prove the property:
// no poew:* keys land, so ComputeAppHash sees the exact same keyspace it
// would on v8.1.2.
func TestReplayV8_2_R1_PreForkByteIdentical(t *testing.T) {
	app, _ := freshReplayApp(t)
	require.Equal(t, int64(0), app.v8_2AppliedHeight, "precondition: fork inactive")

	hBefore, err := ComputeAppHash(app.badgerStore)
	require.NoError(t, err)

	// Run two epochs entirely pre-fork. v8.1.2 wouldn't write poew:* keys
	// because that code didn't exist; v8.2 must suppress them via the gate.
	app.processEpoch(100, time.Unix(2000, 0))
	app.processEpoch(200, time.Unix(2100, 0))

	require.False(t, hasPoEWKeys(t, app), "pre-fork processEpoch must not persist poew:*")

	hAfter, err := ComputeAppHash(app.badgerStore)
	require.NoError(t, err)

	// processEpoch's ONLY on-chain write is the fork-gated SetEpochWeights;
	// its other outputs are off-chain pendingWrites and in-memory weights.
	// Pre-fork the epoch boundary is therefore AppHash-INERT — the digest a
	// v8.1.2 binary committed is reproduced bit-for-bit, not merely "the
	// poew:* keys are absent".
	assert.Equal(t, hBefore, hAfter,
		"pre-fork processEpoch must not move the AppHash (v8.1.2 parity)")

	hReplay, err := ComputeAppHash(app.badgerStore)
	require.NoError(t, err)
	assert.Equal(t, hAfter, hReplay,
		"ComputeAppHash must be deterministic on a re-read")
}

// R2: with v8_2AppliedHeight set and height > activation, processEpoch DOES
// persist poew:* keys. Computing AppHash on the same vote-stat substrate
// must therefore yield a different digest than R1 — the only delta is the
// poew:* contribution. This proves the activation actually changes the
// AppHash trajectory (which is what consensus replicas will diverge on if
// they disagree about post-fork state).
//
// The gate is set IN-MEMORY (v8_2AppliedHeight, the same field the real
// FinalizeBlock activation arm assigns) rather than via MarkUpgradeApplied,
// which would write an audit-trail key and contaminate the pre/post hash
// diff. With the in-memory gate, the two stores are byte-identical by
// construction before processEpoch runs, so the post-epoch divergence is
// attributable to fork-gated writes — the poew:* keys — and nothing else.
// (The persisted-record → gate derivation is covered by refreshV8_2Fork's
// own suite and by the real-activation boundary test in replay_v8_4_test.)
func TestReplayV8_2_R2_PostForkDivergesByPoEWKeys(t *testing.T) {
	// Build two identical chains side by side. The only difference is whether
	// the v8.2 gate is active.
	appPre, _ := freshReplayApp(t)
	appPost, _ := freshReplayApp(t)
	appPost.v8_2AppliedHeight = 50
	require.True(t, appPost.postV8_2Fork(100), "precondition: H=100 is post-fork")

	// Byte-identical baseline: any post-epoch divergence below is the gate's.
	basePre, err := ComputeAppHash(appPre.badgerStore)
	require.NoError(t, err)
	basePost, err := ComputeAppHash(appPost.badgerStore)
	require.NoError(t, err)
	require.Equal(t, basePre, basePost,
		"two freshly-built stores must share a baseline AppHash")

	appPre.processEpoch(100, time.Unix(2000, 0))
	hPre, err := ComputeAppHash(appPre.badgerStore)
	require.NoError(t, err)
	require.False(t, hasPoEWKeys(t, appPre))
	// Pre-fork, processEpoch's only on-chain write (SetEpochWeights) is
	// suppressed — everything else it produces is off-chain pendingWrites or
	// in-memory. The epoch boundary must therefore be AppHash-INERT, exactly
	// as on a v8.1.2 binary: this is the byte-parity property R1 states,
	// pinned at the digest level.
	assert.Equal(t, basePre, hPre,
		"pre-fork processEpoch must not move the AppHash (v8.1.2 parity)")

	appPost.processEpoch(100, time.Unix(2000, 0))
	hPost, err := ComputeAppHash(appPost.badgerStore)
	require.NoError(t, err)
	require.True(t, hasPoEWKeys(t, appPost), "post-fork processEpoch must persist poew:*")

	// Identical substrates + identical epoch run ⇒ the ONLY keyspace delta is
	// the fork-gated poew:* write, so the digests must diverge by exactly that
	// contribution.
	assert.NotEqual(t, hPre, hPost,
		"poew:* keys alone must move the AppHash for the same epoch substrate")

	// Determinism guard: same store, same hash on re-read.
	hPostReplay, err := ComputeAppHash(appPost.badgerStore)
	require.NoError(t, err)
	assert.Equal(t, hPost, hPostReplay,
		"ComputeAppHash on the post-fork store must be deterministic")
}
