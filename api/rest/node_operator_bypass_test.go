package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// v7.1 hook read-scope fix
//
// The SessionStart direct-write hook signs as the node operator
// (~/.sage/agent.key). Pre-v7.1 the operator key got hit by the same
// agent-isolation RBAC as any other caller, so the hook's prefetch returned
// empty on multi-agent nodes where the LLM identity submits memories under
// a different agent_id. v7.1 adds Server.SetNodeOperatorID; when set, that
// caller short-circuits resolveVisibleAgents to seeAll=true. Domain access
// and classification gates still run.
// ---------------------------------------------------------------------------

// TestNodeOperator_BypassListsAllSubmittersWhenSet seeds a domain with three
// memories from a different submitter than the caller. Without the bypass
// the caller would see one (their own) of three; with the bypass set to the
// caller's id, they see all three and the filter header is absent.
func TestNodeOperator_BypassListsAllSubmittersWhenSet(t *testing.T) {
	srv, memStore, bs, _ := newRBACTestServer(t)

	req, callerID := signedRequest(t, http.MethodGet, "/v1/memory/list?limit=50", nil)

	// Pre-condition: declare the caller as the node operator. From this
	// point on, requests signed with the caller's key bypass agent-isolation
	// regardless of what (if anything) the on-chain agent record says.
	srv.SetNodeOperatorID(callerID)

	// Seed three memories: one submitted by the caller, two by someone else.
	// Under the legacy RBAC path the caller would only see their own; with
	// the operator bypass they see all three.
	otherID := "0000000000000000000000000000000000000000000000000000000000000001"
	require.NoError(t, bs.RegisterAgent(otherID, "other", "member", "", "test", "", 1))

	seedMemory(t, memStore, "m-own", callerID, "general", "operator's own")
	seedMemory(t, memStore, "m-other-1", otherID, "general", "from someone else")
	seedMemory(t, memStore, "m-other-2", otherID, "general", "also from someone else")

	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	// The filter header must NOT appear because the bypass turned the RBAC
	// gate off entirely. Empty header == nothing hidden.
	assert.Empty(t, rr.Header().Get(filterHeader),
		"node operator bypass should produce no rbac filter header")

	var resp struct {
		Memories []any       `json:"memories"`
		Total    int         `json:"total"`
		Filtered *FilterInfo `json:"filtered"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.Total, "operator should see all 3 memories regardless of submitter")
	assert.Nil(t, resp.Filtered, "no filter envelope when bypass is in effect")
}

// TestNodeOperator_NoBypassWhenUnset confirms the bypass is opt-in: with
// SetNodeOperatorID never called, the legacy RBAC behaviour is preserved
// and a non-admin caller still gets the submitting_agents filter applied.
// This is the regression guard against accidentally enabling the bypass
// globally.
func TestNodeOperator_NoBypassWhenUnset(t *testing.T) {
	srv, memStore, bs, _ := newRBACTestServer(t)

	req, callerID := signedRequest(t, http.MethodGet, "/v1/memory/list?limit=50", nil)
	// Intentionally do NOT call SetNodeOperatorID.
	otherID := "0000000000000000000000000000000000000000000000000000000000000001"
	require.NoError(t, bs.RegisterAgent(otherID, "other", "member", "", "test", "", 1))

	seedMemory(t, memStore, "m-own", callerID, "general", "caller-owned")
	seedMemory(t, memStore, "m-hidden", otherID, "general", "not the caller's")

	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	assert.Equal(t, filterBySubmittingAgts, rr.Header().Get(filterHeader),
		"without the bypass the RBAC submitting_agents filter must still fire")

	var resp struct {
		Memories []any       `json:"memories"`
		Total    int         `json:"total"`
		Filtered *FilterInfo `json:"filtered"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total, "RBAC limits caller to own submissions only")
	require.NotNil(t, resp.Filtered)
	assert.Contains(t, resp.Filtered.By, filterBySubmittingAgts)
}

// TestNodeOperator_BypassOnlyMatchesExactID makes sure the bypass keys on
// the full agent_id string and doesn't accidentally match other callers
// who happen to share a prefix or substring. The bypass is a cryptographic
// equality check, not a fuzzy match.
func TestNodeOperator_BypassOnlyMatchesExactID(t *testing.T) {
	srv, memStore, bs, _ := newRBACTestServer(t)

	// Configure the bypass with an ID that does NOT match the caller.
	srv.SetNodeOperatorID("0000000000000000000000000000000000000000000000000000000000000001")

	req, callerID := signedRequest(t, http.MethodGet, "/v1/memory/list?limit=50", nil)
	otherID := "0000000000000000000000000000000000000000000000000000000000000002"
	require.NoError(t, bs.RegisterAgent(otherID, "other", "member", "", "test", "", 1))

	seedMemory(t, memStore, "m-own", callerID, "general", "mine")
	seedMemory(t, memStore, "m-hidden", otherID, "general", "not mine")

	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	// Caller doesn't match the configured operator id, so the filter still
	// fires and the caller sees only their own memory.
	assert.Equal(t, filterBySubmittingAgts, rr.Header().Get(filterHeader))
	var resp struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Total)
}

// TestNodeOperator_GetterReturnsSetValue is a trivial round-trip for the
// SetNodeOperatorID/NodeOperatorID pair so the API stays observable.
func TestNodeOperator_GetterReturnsSetValue(t *testing.T) {
	srv, _, _, _ := newRBACTestServer(t)
	assert.Empty(t, srv.NodeOperatorID(), "default is empty (bypass disabled)")
	srv.SetNodeOperatorID("deadbeef")
	assert.Equal(t, "deadbeef", srv.NodeOperatorID())
	srv.SetNodeOperatorID("")
	assert.Empty(t, srv.NodeOperatorID(), "empty string disables again")
}
