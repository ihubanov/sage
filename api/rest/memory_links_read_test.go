package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/auth"
	"github.com/l33tdawg/sage/internal/memory"
	"github.com/l33tdawg/sage/internal/store"
)

type linksReadResponse struct {
	Links []struct {
		SourceID string `json:"source_id"`
		TargetID string `json:"target_id"`
		LinkType string `json:"link_type"`
	} `json:"links"`
}

// TestGetLinksAmongGatesOnReadAccess is the load-bearing security test: a link
// must never disclose the existence of a memory the caller cannot read. The
// handler filters the requested IDs down to the caller-readable subset BEFORE
// GetLinksAmong (which requires both endpoints in its input), so a link to an
// unreadable memory can never surface.
func TestGetLinksAmongGatesOnReadAccess(t *testing.T) {
	srv, memStore, badger, agents := newRBACTestServer(t)

	// The store holds an A→B link. It may only surface when BOTH endpoints are
	// requested AND readable. The hook captures the IDs the handler actually
	// passed, so we can assert the unreadable one was filtered before the call.
	seeded := memory.MemoryLink{SourceID: "mem-a", TargetID: "mem-b", LinkType: "supersedes"}
	var passed []string
	memStore.getLinksAmongHook = func(ids []string) ([]memory.MemoryLink, error) {
		passed = append([]string(nil), ids...)
		set := map[string]bool{}
		for _, id := range ids {
			set[id] = true
		}
		if set[seeded.SourceID] && set[seeded.TargetID] {
			return []memory.MemoryLink{seeded}, nil
		}
		return nil, nil
	}

	body := []byte(`{"memory_ids":["mem-a","mem-b"]}`)
	callerPub, callerPriv, err := auth.GenerateKeypair()
	require.NoError(t, err)
	callerID := auth.PublicKeyToAgentID(callerPub)
	req := signedRequestAs(t, callerPriv, callerID, http.MethodPost, "/v1/memory/links", body)
	agents.agents[callerID] = &store.AgentEntry{AgentID: callerID, Name: "reader", Role: "member", Status: "active"}
	require.NoError(t, badger.RegisterAgent(callerID, "reader", "member", "", "test", "", 1))
	require.NoError(t, badger.SetAgentPermission(callerID, 1, "", "*", "", ""))

	const ownerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, badger.RegisterAgent(ownerID, "owner", "member", "", "test", "", 1))
	require.NoError(t, badger.RegisterDomain("source.domain", ownerID, "", 1))
	require.NoError(t, badger.RegisterDomain("secret.domain", ownerID, "", 1))
	require.NoError(t, badger.SetMemoryClassification("mem-a", 1))
	require.NoError(t, badger.SetMemoryClassification("mem-b", 1))
	seedMemory(t, memStore, "mem-a", ownerID, "source.domain", "readable")
	seedMemory(t, memStore, "mem-b", ownerID, "secret.domain", "unreadable")

	// Phase 1: caller can read source.domain but has NO grant on secret.domain.
	require.NoError(t, badger.SetAccessGrant("source.domain", callerID, 1, 0, ownerID))

	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, []string{"mem-a"}, passed,
		"unreadable mem-b must be filtered out BEFORE GetLinksAmong")
	var denied linksReadResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &denied))
	assert.Empty(t, denied.Links, "a link to an unreadable memory must not be disclosed")

	// Phase 2: grant read on secret.domain — now both readable, the link surfaces.
	require.NoError(t, badger.SetAccessGrant("secret.domain", callerID, 1, 0, ownerID))
	passed = nil
	// Reorder the ids so the signed payload differs (avoids replay detection); the
	// handler and GetLinksAmong are order-independent.
	body2 := []byte(`{"memory_ids":["mem-b","mem-a"]}`)
	req2 := signedRequestAs(t, callerPriv, callerID, http.MethodPost, "/v1/memory/links", body2)

	rr2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code, rr2.Body.String())
	require.ElementsMatch(t, []string{"mem-a", "mem-b"}, passed,
		"both readable memories must reach GetLinksAmong")
	var allowed linksReadResponse
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &allowed))
	require.Len(t, allowed.Links, 1)
	assert.Equal(t, "mem-a", allowed.Links[0].SourceID)
	assert.Equal(t, "mem-b", allowed.Links[0].TargetID)
	assert.Equal(t, "supersedes", allowed.Links[0].LinkType)
}

func TestGetLinksAmongRequiresActiveAgent(t *testing.T) {
	srv, _, badger, agents := newRBACTestServer(t)
	body := []byte(`{"memory_ids":["mem-a"]}`)
	req, callerID := signedRequest(t, http.MethodPost, "/v1/memory/links", body)
	agents.agents[callerID] = &store.AgentEntry{AgentID: callerID, Name: "inactive", Role: "member", Status: "inactive"}
	require.NoError(t, badger.RegisterAgent(callerID, "inactive", "member", "", "test", "", 1))

	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "Active agent")
}

func TestGetLinksAmongEmptyInputReturnsEmpty(t *testing.T) {
	srv, memStore, badger, agents := newRBACTestServer(t)
	called := false
	memStore.getLinksAmongHook = func(ids []string) ([]memory.MemoryLink, error) {
		called = true
		return nil, nil
	}
	body := []byte(`{"memory_ids":[]}`)
	req, callerID := signedRequest(t, http.MethodPost, "/v1/memory/links", body)
	agents.agents[callerID] = &store.AgentEntry{AgentID: callerID, Name: "reader", Role: "member", Status: "active"}
	require.NoError(t, badger.RegisterAgent(callerID, "reader", "member", "", "test", "", 1))

	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.False(t, called, "empty input must short-circuit before the store call")
	var resp linksReadResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Empty(t, resp.Links)
}
