package rest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/auth"
	"github.com/l33tdawg/sage/internal/embedding"
	"github.com/l33tdawg/sage/internal/metrics"
	"github.com/l33tdawg/sage/internal/store"
)

// evidenceMockStore is the mock memory store plus node-local evidence.
type evidenceMockStore struct {
	*mockMemoryStore
	mu       sync.Mutex
	uploaded map[string][2]string // evidence id -> {agent, evidence}
	claimed  map[string]string    // memory id -> evidence
}

func (e *evidenceMockStore) CreateMemoryEvidence(_ context.Context, agentID, evidence string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := "ev-" + strconv.Itoa(len(e.uploaded)+1)
	e.uploaded[id] = [2]string{agentID, evidence}
	return id, nil
}

func (e *evidenceMockStore) ClaimMemoryEvidence(_ context.Context, evidenceID, agentID, memoryID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	up, ok := e.uploaded[evidenceID]
	if !ok || up[0] != agentID {
		return store.ErrEvidenceUnavailable
	}
	delete(e.uploaded, evidenceID)
	e.claimed[memoryID] = up[1]
	return nil
}

func (e *evidenceMockStore) claimedNow() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]string, len(e.claimed))
	for k, v := range e.claimed {
		out[k] = v
	}
	return out
}

func evidenceTestServer(t *testing.T, cometURL string, keepsEvidence bool) (*Server, *evidenceMockStore) {
	t.Helper()
	health := metrics.NewHealthChecker()
	health.SetPostgresHealth(true)
	health.SetCometBFTHealth(true)
	es := &evidenceMockStore{mockMemoryStore: newMockMemoryStore(), uploaded: map[string][2]string{}, claimed: map[string]string{}}
	var ms store.MemoryStore = es.mockMemoryStore
	if keepsEvidence {
		ms = es
	}
	return NewServer(cometURL, ms, newMockScoreStore(), nil, health, zerolog.Nop(), embedding.NewClient("", "")), es
}

// agentCall signs a request with a fixed agent key, so an upload and the
// submission that claims it come from the same agent.
func agentCall(t *testing.T, srv *Server, priv ed25519.PrivateKey, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	ts := time.Now().Unix()
	sig := auth.SignRequest(priv, http.MethodPost, path, []byte(body), ts)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("X-Agent-ID", auth.PublicKeyToAgentID(priv.Public().(ed25519.PublicKey)))
	req.Header.Set("X-Signature", hex.EncodeToString(sig))
	req.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	return rr
}

func uploadEvidence(t *testing.T, srv *Server, priv ed25519.PrivateKey, evidence string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"evidence": evidence})
	rr := agentCall(t, srv, priv, "/v1/memory/evidence", string(body))
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var resp UploadEvidenceResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.EvidenceID)
	return resp.EvidenceID
}

func TestSubmitMemory_EvidenceIsAttachedBeforeBroadcastAndNeverOnChain(t *testing.T) {
	const ev = "Work order 118: north gate alarm disabled today."
	var es *evidenceMockStore
	var mu sync.Mutex
	var atBroadcast map[string]string
	var sawEvidenceInTx atomic.Bool
	cometMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		atBroadcast = es.claimedNow()
		mu.Unlock()
		raw, _ := hex.DecodeString(strings.TrimPrefix(r.URL.Query().Get("tx"), "0x"))
		if bytes.Contains(raw, []byte("Work order 118")) {
			sawEvidenceInTx.Store(true)
		}
		writeCometCommitFixture(t, w, r, 0, "", 0, "memory submitted", 1)
	}))
	defer cometMock.Close()
	var srv *Server
	srv, es = evidenceTestServer(t, cometMock.URL, true)
	_, priv, err := auth.GenerateKeypair()
	require.NoError(t, err)

	id := uploadEvidence(t, srv, priv, ev)
	rr := agentCall(t, srv, priv, "/v1/memory/submit", `{"content":"The north gate alarm is disabled.","memory_type":"fact",
		"domain_tag":"site","confidence_score":0.9,"evidence_id":"`+id+`"}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	require.False(t, sawEvidenceInTx.Load(), "the evidence text never enters the transaction")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, atBroadcast, 1, "evidence is attached before the transaction is broadcast")
	for _, got := range atBroadcast {
		require.Equal(t, ev, got)
	}
}

func TestSubmitMemory_EvidenceIsRefusedNeverDropped(t *testing.T) {
	var broadcasts atomic.Int64
	cometMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broadcasts.Add(1)
		writeCometCommitFixture(t, w, r, 0, "", 0, "memory submitted", 1)
	}))
	defer cometMock.Close()
	_, priv, err := auth.GenerateKeypair()
	require.NoError(t, err)
	_, other, err := auth.GenerateKeypair()
	require.NoError(t, err)

	plain, _ := evidenceTestServer(t, cometMock.URL, false)
	rr := agentCall(t, plain, priv, "/v1/memory/evidence", `{"evidence":"Datasheet: rated power 40 kW."}`)
	require.Equal(t, http.StatusNotImplemented, rr.Code, "a store that cannot keep evidence refuses it")
	rr = agentCall(t, plain, priv, "/v1/memory/submit", `{"content":"The pump is rated 40 kW.","memory_type":"fact",
		"domain_tag":"site","confidence_score":0.9,"evidence_id":"ev-1"}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "Evidence not supported")

	keeps, _ := evidenceTestServer(t, cometMock.URL, true)
	rr = agentCall(t, keeps, priv, "/v1/memory/evidence", `{"evidence":"`+strings.Repeat("e", store.MaxEvidenceBytes+1)+`"}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)

	id := uploadEvidence(t, keeps, priv, "Datasheet: rated power 40 kW.")
	rr = agentCall(t, keeps, other, "/v1/memory/submit", `{"content":"The pump is rated 40 kW.","memory_type":"fact",
		"domain_tag":"site","confidence_score":0.9,"evidence_id":"`+id+`"}`)
	require.Equal(t, http.StatusBadRequest, rr.Code, "another agent cannot claim this agent's evidence")
	require.Contains(t, rr.Body.String(), "Invalid evidence_id")

	rr = agentCall(t, keeps, priv, "/v1/memory/submit", `{"content":"Check the pump","memory_type":"task","domain_tag":"site",
		"confidence_score":0.9,"task_status":"planned","evidence_id":"`+id+`"}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "task")
	require.Zero(t, broadcasts.Load(), "a refused submission is never broadcast")
}
