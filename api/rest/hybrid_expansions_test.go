package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/l33tdawg/sage/internal/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// v7.1 query expansion: POST /v1/memory/hybrid with optional Expansions[]
//
// When the request includes paraphrase/entity/temporal variants of the
// primary query, the handler runs SearchHybrid once per variant + once for
// the primary, then RRF-merges across variants. With no expansions the
// behaviour is identical to v7.0 (single SearchHybrid call).
// ---------------------------------------------------------------------------

func TestHybridSearchMemory_ExpansionsFanOut(t *testing.T) {
	srv, memStore, _ := newTestServer(t, "")

	// Seed three memories so the response has shape to inspect.
	for i, id := range []string{"a", "b", "c"} {
		memStore.memories[id] = &memory.MemoryRecord{
			MemoryID:        id,
			SubmittingAgent: "agent-x",
			Content:         "memory " + id,
			ContentHash:     []byte{byte(i + 1)},
			MemoryType:      memory.TypeObservation,
			DomainTag:       "general",
			ConfidenceScore: 0.9,
			Status:          memory.StatusCommitted,
			CreatedAt:       time.Now().Add(-time.Hour),
		}
	}

	// Build a request with primary + two expansions.
	body, _ := json.Marshal(HybridSearchMemoryRequest{
		Query:     "primary question",
		Embedding: []float32{0.1, 0.2, 0.3},
		Expansions: []HybridExpansion{
			{Query: "rephrasing one", Embedding: []float32{0.2, 0.2, 0.2}},
			{Query: "rephrasing two", Embedding: []float32{0.3, 0.1, 0.1}},
		},
		TopK: 5,
	})

	req, _ := signedRequest(t, http.MethodPost, "/v1/memory/hybrid", body)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	// We can't directly count SearchHybrid calls through the public surface
	// (the test server uses mockMemoryStore which forwards SearchHybrid to
	// QuerySimilar internally and tracks lastQueryTags). But we CAN verify
	// the response shape is still sane and includes the three seeded memories.
	var resp QueryMemoryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.LessOrEqual(t, resp.TotalCount, 5, "TopK respected")
	assert.GreaterOrEqual(t, resp.TotalCount, 1, "at least one memory should land")
}

func TestHybridSearchMemory_EmptyExpansionsBehavesLikeV70(t *testing.T) {
	srv, memStore, _ := newTestServer(t, "")

	memStore.memories["only"] = &memory.MemoryRecord{
		MemoryID:        "only",
		SubmittingAgent: "agent-x",
		Content:         "single memory",
		ContentHash:     []byte{1},
		MemoryType:      memory.TypeObservation,
		DomainTag:       "general",
		ConfidenceScore: 0.85,
		Status:          memory.StatusCommitted,
		CreatedAt:       time.Now(),
	}

	body, _ := json.Marshal(HybridSearchMemoryRequest{
		Query:     "x",
		Embedding: []float32{0.1, 0.2, 0.3},
		// No expansions intentionally.
		TopK: 3,
	})

	req, _ := signedRequest(t, http.MethodPost, "/v1/memory/hybrid", body)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var resp QueryMemoryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.TotalCount, "single memory returned")
}

func TestHybridSearchMemory_BlankExpansionEntriesSkipped(t *testing.T) {
	srv, memStore, _ := newTestServer(t, "")

	memStore.memories["a"] = &memory.MemoryRecord{
		MemoryID:        "a",
		SubmittingAgent: "agent-x",
		Content:         "a",
		ContentHash:     []byte{1},
		MemoryType:      memory.TypeObservation,
		DomainTag:       "general",
		ConfidenceScore: 0.9,
		Status:          memory.StatusCommitted,
		CreatedAt:       time.Now(),
	}

	// One real expansion, one empty (should be skipped without error).
	body, _ := json.Marshal(HybridSearchMemoryRequest{
		Query:     "q",
		Embedding: []float32{0.1, 0.2, 0.3},
		Expansions: []HybridExpansion{
			{Query: "real variant", Embedding: []float32{0.2, 0.2, 0.2}},
			{Query: "", Embedding: nil}, // skip me
		},
		TopK: 3,
	})

	req, _ := signedRequest(t, http.MethodPost, "/v1/memory/hybrid", body)
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var resp QueryMemoryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.GreaterOrEqual(t, resp.TotalCount, 1)
}
