package mcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type claimantSessionsRoundTripper func(*http.Request) (*http.Response, error)

func (f claimantSessionsRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func claimantTestDir(t *testing.T) string {
	t.Helper()
	t.Setenv("SAGE_HOME", t.TempDir())
	dir, err := claimantIdentityDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0700))
	return dir
}

func claimantTestIdentity(agentID, sessionID, provider, project, scope string) durableClaimantIdentity {
	created := time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC)
	return durableClaimantIdentity{
		Version:           1,
		AgentID:           agentID,
		Provider:          provider,
		Project:           project,
		TransportScope:    scope,
		ClaimantSessionID: sessionID,
		CreatedAt:         &created,
	}
}

func writeClaimantFixture(t *testing.T, dir, name string, identity durableClaimantIdentity) {
	t.Helper()
	raw, err := json.Marshal(identity)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), raw, 0600))
}

func TestListDurableClaimantIdentitiesFiltersAgentAndLiveness(t *testing.T) {
	dir := claimantTestDir(t)
	liveID := "mcp-" + strings.Repeat("1", 32)
	staleID := "mcp-" + strings.Repeat("2", 32)
	otherID := "mcp-" + strings.Repeat("3", 32)
	writeClaimantFixture(t, dir, "live.json", claimantTestIdentity("agent-a", liveID, "claude-code", "sage", ""))
	writeClaimantFixture(t, dir, "stale.json", claimantTestIdentity("agent-a", staleID, "claude-code", "other", ""))
	writeClaimantFixture(t, dir, "other.json", claimantTestIdentity("agent-b", otherID, "codex", "sage", ""))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "invalid.json"), []byte("not-json"), 0600))

	lease, acquired, err := tryLockClaimantFile(filepath.Join(dir, "live.lock"))
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() { _ = lease.Close() })

	scan, err := listDurableClaimantIdentities("agent-a", "")
	require.NoError(t, err)
	require.Len(t, scan.Sessions, 1)
	require.Equal(t, liveID, scan.Sessions[0].SessionID)
	require.True(t, scan.Sessions[0].Live)
	require.Equal(t, "live", scan.Sessions[0].Liveness)
	require.Equal(t, 1, scan.StaleCount)
	require.Equal(t, 1, scan.InvalidCount)

	require.NoError(t, lease.Close())
	scan, err = listDurableClaimantIdentities("agent-a", "")
	require.NoError(t, err)
	require.Empty(t, scan.Sessions)
	require.Equal(t, 2, scan.StaleCount, "releasing the lock makes the identity stale")
	require.Equal(t, 1, scan.InvalidCount)
}

func TestListDurableClaimantIdentitiesKeepsCurrentSessionLive(t *testing.T) {
	dir := claimantTestDir(t)
	currentID := "mcp-" + strings.Repeat("4", 32)
	writeClaimantFixture(t, dir, "current.json", claimantTestIdentity("agent-a", currentID, "claude-code", "sage", "http-sse:scope"))

	scan, err := listDurableClaimantIdentities("agent-a", currentID)
	require.NoError(t, err)
	require.Len(t, scan.Sessions, 1)
	require.True(t, scan.Sessions[0].Current)
	require.True(t, scan.Sessions[0].Live)
	require.Equal(t, "live", scan.Sessions[0].Liveness)
	require.Zero(t, scan.StaleCount)
}

func TestToolSessionsReturnsOnlyCallerSessionsAndExactClaimCounts(t *testing.T) {
	dir := claimantTestDir(t)
	var requestCount int
	var requestedSession string
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	s := NewServer("http://sage.test", key)
	s.provider = "claude-code"
	s.project = "sage"
	s.httpClient = &http.Client{Transport: claimantSessionsRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/messages/own-claimed-unfinished" {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("not found")),
				Request:    r,
			}, nil
		}
		requestCount++
		requestedSession = r.URL.Query().Get("claimant_session_id")
		if limit := r.URL.Query().Get("limit"); limit != "1" {
			t.Errorf("claim-count probe limit = %q, want 1", limit)
		}
		body, err := json.Marshal(map[string]any{"count": 2, "limit": 1, "truncated": false})
		if err != nil {
			t.Errorf("marshal claim-count response: %v", err)
			return nil, err
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       r,
		}, nil
	})}
	t.Cleanup(s.closeClaimantLease)

	ctx := withClaimantDurableScope(WithConversationID(context.Background(), "sse:sessions-test"), "http-sse:test")
	currentID, err := s.claimantSessionID(ctx)
	require.NoError(t, err)

	staleID := "mcp-" + strings.Repeat("5", 32)
	otherID := "mcp-" + strings.Repeat("6", 32)
	writeClaimantFixture(t, dir, "stale.json", claimantTestIdentity(s.agentID, staleID, "claude-code", "other", ""))
	writeClaimantFixture(t, dir, "other.json", claimantTestIdentity("another-agent", otherID, "codex", "sage", ""))

	raw, err := s.toolSessions(ctx, nil)
	require.NoError(t, err)
	response, ok := raw.(map[string]any)
	require.True(t, ok)
	require.Equal(t, s.agentID, response["agent_id"])
	require.Equal(t, currentID, response["current_session_id"])
	require.Equal(t, 1, response["session_count"])
	require.Equal(t, 1, response["stale_identity_count"])
	require.Equal(t, 0, response["invalid_identity_count"])

	sessions, ok := response["sessions"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, sessions, 1)
	require.Equal(t, currentID, sessions[0]["session_id"])
	require.Equal(t, true, sessions[0]["current"])
	require.Equal(t, "http-sse", sessions[0]["transport"])
	require.Equal(t, 2, sessions[0]["unfinished_claim_count"])
	require.Equal(t, "exact", sessions[0]["unfinished_claim_count_state"])
	require.Equal(t, 1, requestCount)
	require.Equal(t, currentID, requestedSession)
}
