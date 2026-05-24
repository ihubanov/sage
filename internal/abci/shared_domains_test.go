package abci

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsSharedDomainStatic pins the compile-time shared-domain set and the
// prefix-based carve-out used to keep cross-cutting "meta" domain families
// (e.g. sage-*) from getting captured on first write after a chain reset.
//
// Post-v8.0 the *method* form (SageApp.isSharedDomain) overlays an on-chain
// sentinel on top of this static decision, but the static rules themselves
// must stay byte-identical to v7.1.1 for pre-fork replay parity.
func TestIsSharedDomainStatic(t *testing.T) {
	cases := []struct {
		name   string
		domain string
		shared bool
	}{
		// Exact matches.
		{"general is shared", "general", true},
		{"self is shared", "self", true},
		{"meta is shared", "meta", true},

		// sage-* prefix family. These are SAGE-meta domains that are
		// network-wide-by-convention rather than single-owner.
		{"sage-debugging is shared", "sage-debugging", true},
		{"sage-development is shared", "sage-development", true},
		{"sage-rbac-debug is shared", "sage-rbac-debug", true},
		{"sage-architecture is shared", "sage-architecture", true},

		// Sibling spellings must NOT bleed into the prefix carve-out.
		{"sage (no dash) is owned", "sage", false},
		{"sageops (no dash) is owned", "sageops", false},

		// Per-project domains stay owned. levelup-* in particular has
		// real per-org ownership semantics; we deliberately don't carve
		// it out.
		{"levelup-bugs is owned", "levelup-bugs", false},
		{"levelup-deployment is owned", "levelup-deployment", false},
		{"calibration.sqli is owned", "calibration.sqli", false},
		{"rules.sqli is owned", "rules.sqli", false},
		{"random domain is owned", "some-project-domain", false},
		{"empty string is owned", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSharedDomainStatic(tc.domain); got != tc.shared {
				t.Fatalf("isSharedDomainStatic(%q) = %v, want %v", tc.domain, got, tc.shared)
			}
		})
	}
}

// TestIsSharedDomain_HybridMethod_PreFork — the SageApp.isSharedDomain
// method must collapse to the static rule set on a pre-fork chain. The
// on-chain shared_domain:<name> sentinel is ignored until v8 activates.
func TestIsSharedDomain_HybridMethod_PreFork(t *testing.T) {
	app := setupTestApp(t)
	// Precondition: pre-fork — v8AppliedHeight = 0.
	require.Equal(t, int64(0), app.v8AppliedHeight)

	// Static-shared: covered by the prefix rule.
	require.True(t, app.isSharedDomain("sage-debugging", 100))
	// Static-owned: not in the set or prefix.
	require.False(t, app.isSharedDomain("levelup-bugs", 100))

	// Write an on-chain sentinel for "levelup-bugs". Pre-fork the method
	// MUST ignore it — pre-fork replay byte-identicality with v7.1.1 means
	// the on-chain key has no effect on the decision until the fork lands.
	require.NoError(t, app.badgerStore.SetSharedDomain("levelup-bugs"))
	require.False(t, app.isSharedDomain("levelup-bugs", 100), "pre-fork: on-chain sentinel must be ignored")
}

// TestIsSharedDomain_HybridMethod_PostFork — once v8 has activated and the
// caller's height is strictly past v8AppliedHeight, the on-chain
// shared_domain:<name> sentinel promotes the domain to shared.
func TestIsSharedDomain_HybridMethod_PostFork(t *testing.T) {
	app := setupTestApp(t)
	app.v8AppliedHeight = 50

	// At the activation block: STILL pre-fork (H+1 semantic — fork takes
	// effect on the block AFTER activation).
	require.NoError(t, app.badgerStore.SetSharedDomain("rules.dfir"))
	require.False(t, app.isSharedDomain("rules.dfir", 50), "at activation block: gate is still false")

	// First post-fork block — sentinel takes effect.
	require.True(t, app.isSharedDomain("rules.dfir", 51), "first post-fork block: on-chain sentinel must promote")

	// Static rules still apply post-fork regardless of the sentinel.
	require.True(t, app.isSharedDomain("general", 51))
	require.True(t, app.isSharedDomain("sage-debugging", 51))
	// Owned and no sentinel — stays owned.
	require.False(t, app.isSharedDomain("levelup-deployment", 51))
}
