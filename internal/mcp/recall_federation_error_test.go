package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/federation"
)

// TestRecallUnavailableDetailDistinguishesLayers pins the fix for the misleading
// error an external agent reported to the operator. An empty federation
// destination set used to be rendered as `%v` of a per-peer error map that the
// topology filter deliberately leaves empty, so it printed `map[]` — which reads
// as an authorization verdict. The three faults below have three different
// fixes, and the message must not let a caller confuse them.
func TestRecallUnavailableDetailDistinguishesLayers(t *testing.T) {
	for _, test := range []struct {
		reason string
		want   string
	}{
		{string(federation.RecallUnavailableNoAgreement), "no peer is implicated"},
		{string(federation.RecallUnavailableDomainNotShared), "access answer"},
		{string(federation.RecallUnavailablePeerUnreachable), "connectivity fault"},
		{string(federation.RecallUnavailableRequestedChains), "currently active agreement"},
		{string(federation.RecallUnavailablePlanner), "not an authorization verdict"},
		{string(federation.RecallUnavailableFederation), "federation subsystem"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			got := recallUnavailableDetail(test.reason, nil)
			assert.Contains(t, got, test.want)
			assert.NotEqual(t, "map[]", got, "an unexplained empty map must never be the whole answer")
		})
	}
}

func TestRecallUnavailableDetailAppendsPeerDetail(t *testing.T) {
	got := recallUnavailableDetail(string(federation.RecallUnavailablePeerUnreachable),
		map[string]string{"chain-a": "dial tcp: i/o timeout"})
	assert.Contains(t, got, "connectivity fault")
	assert.Contains(t, got, "per-peer")
	assert.Contains(t, got, "dial tcp")
}

func TestRecallUnavailableDetailFallsBackForOlderNodes(t *testing.T) {
	// A node that predates the reason code sends neither field. Do not invent a
	// verdict: say the node gave no reason.
	assert.Contains(t, recallUnavailableDetail("", nil), "older node")

	// With per-peer detail available, keep the historical rendering.
	got := recallUnavailableDetail("", map[string]string{"chain-a": "forbidden"})
	require.Contains(t, got, "forbidden")
	assert.NotContains(t, got, "older node")
}
