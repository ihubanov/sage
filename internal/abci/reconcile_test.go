package abci

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/validator"
)

// The legacy 4-archetype fingerprint used across these cases.
var testArchetypes = []string{"arch-a", "arch-b", "arch-c", "arch-d"}

func addValidators(t *testing.T, app *SageApp, ids ...string) {
	t.Helper()
	for _, id := range ids {
		require.NoError(t, app.validators.AddValidator(&validator.ValidatorInfo{ID: id, Power: 10}))
	}
}

// A legacy single-node chain (set == exactly the 4 archetype keys, not in quorum
// mode) is repaired to the node's own consensus key, and the stale archetype keys
// are dropped from Badger so they cannot resurrect as phantom non-voting validators
// (which would otherwise block the 2/3 quorum on restart).
func TestReconcileSelfValidator_RepairsLegacySingleNode(t *testing.T) {
	app := setupTestApp(t)
	const self = "self-node-consensus-id"
	addValidators(t, app, testArchetypes...)

	changed, err := app.ReconcileSelfValidator(self, testArchetypes, true)
	require.NoError(t, err)
	require.True(t, changed, "legacy single-node set should be repaired")

	require.Equal(t, []string{self}, app.ValidatorIDs(), "only the node's own key should remain in-memory")

	persisted, err := app.badgerStore.LoadValidators()
	require.NoError(t, err)
	require.Len(t, persisted, 1, "full-replace must drop the 4 stale archetype keys (no phantom resurrect)")
	_, ok := persisted[self]
	require.True(t, ok)

	// Idempotent: a second call is a permanent no-op (self already present).
	changed2, err := app.ReconcileSelfValidator(self, testArchetypes, true)
	require.NoError(t, err)
	require.False(t, changed2)
}

// Issue #37: the OTHER legacy shape — genesis persisted the node's consensus key
// to validator:*, then the old path's SaveValidators upsert added the 4 archetypes
// without deleting it. The node votes fine but the 4 phantom archetypes hold 4/5 of
// the power, making every governance quorum (tallied over ALL validators)
// mathematically unreachable. The repair must strip the archetypes, not treat any
// selfID-present set as healthy.
func TestReconcileSelfValidator_RepairsMixedLegacySet(t *testing.T) {
	app := setupTestApp(t)
	const self = "self-node-consensus-id"
	addValidators(t, app, append([]string{self}, testArchetypes...)...)

	changed, err := app.ReconcileSelfValidator(self, testArchetypes, true)
	require.NoError(t, err)
	require.True(t, changed, "mixed {self + archetypes} legacy set should be repaired")

	require.Equal(t, []string{self}, app.ValidatorIDs(), "only the node's own key should remain in-memory")

	persisted, err := app.badgerStore.LoadValidators()
	require.NoError(t, err)
	require.Len(t, persisted, 1, "full-replace must drop the 4 stale archetype keys")
	require.Equal(t, int64(10), persisted[self], "the node's existing power is preserved")

	// Idempotent: a second call declines ({selfID} alone has no archetype members).
	changed2, err := app.ReconcileSelfValidator(self, testArchetypes, true)
	require.NoError(t, err)
	require.False(t, changed2)
}

// A selfID-present set whose other members are NOT exactly the archetype
// fingerprint is a real chain, not the legacy shape — refused untouched.
func TestReconcileSelfValidator_RefusesMixedNonArchetypeSet(t *testing.T) {
	// An intruder alongside 3 archetypes (same size as the fingerprint).
	app := setupTestApp(t)
	const self = "self-node-consensus-id"
	addValidators(t, app, self, "arch-a", "arch-b", "arch-c", "intruder")
	changed, err := app.ReconcileSelfValidator(self, testArchetypes, true)
	require.NoError(t, err)
	require.False(t, changed)
	require.ElementsMatch(t, []string{self, "arch-a", "arch-b", "arch-c", "intruder"}, app.ValidatorIDs())

	// A strict SUBSET of the archetypes → still refused (exact fingerprint only).
	app2 := setupTestApp(t)
	addValidators(t, app2, self, "arch-a", "arch-b", "arch-c")
	changed2, err := app2.ReconcileSelfValidator(self, testArchetypes, true)
	require.NoError(t, err)
	require.False(t, changed2)
	require.ElementsMatch(t, []string{self, "arch-a", "arch-b", "arch-c"}, app2.ValidatorIDs())
}

// A healthy set (selfID plus a real peer — no archetype members) is refused: the
// non-self members don't match the archetype fingerprint.
func TestReconcileSelfValidator_RefusesWhenAlreadyHealthy(t *testing.T) {
	app := setupTestApp(t)
	const self = "self-node-consensus-id"
	addValidators(t, app, self, "peer-1")

	changed, err := app.ReconcileSelfValidator(self, testArchetypes, true)
	require.NoError(t, err)
	require.False(t, changed)
	require.ElementsMatch(t, []string{self, "peer-1"}, app.ValidatorIDs())
}

// Guard (2): singleNode=false → refused even with the archetype fingerprint. This
// is the multi-node safety contract: a local validator:* write would fork AppHash.
func TestReconcileSelfValidator_RefusesMultiNode(t *testing.T) {
	app := setupTestApp(t)
	addValidators(t, app, testArchetypes...)

	changed, err := app.ReconcileSelfValidator("self", testArchetypes, false)
	require.NoError(t, err)
	require.False(t, changed, "reconcile must never fire on a quorum node")
	require.ElementsMatch(t, testArchetypes, app.ValidatorIDs())
}

// Guard (1): the set (minus selfID) must equal EXACTLY the archetype fingerprint —
// a real N-validator genesis quorum, or any set with a non-archetype member, is
// refused.
func TestReconcileSelfValidator_RefusesNonArchetypeSet(t *testing.T) {
	// A genuine 3-validator genesis quorum.
	app := setupTestApp(t)
	realQuorum := []string{"genesis-v1", "genesis-v2", "genesis-v3"}
	addValidators(t, app, realQuorum...)
	changed, err := app.ReconcileSelfValidator("self", testArchetypes, true)
	require.NoError(t, err)
	require.False(t, changed)
	require.ElementsMatch(t, realQuorum, app.ValidatorIDs())

	// Same SIZE as the fingerprint but with one intruder → still refused.
	app2 := setupTestApp(t)
	addValidators(t, app2, "arch-a", "arch-b", "arch-c", "intruder")
	changed2, err := app2.ReconcileSelfValidator("self", testArchetypes, true)
	require.NoError(t, err)
	require.False(t, changed2)
}
