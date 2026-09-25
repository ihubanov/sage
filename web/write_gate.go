package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/l33tdawg/sage/internal/memory"
	"github.com/l33tdawg/sage/internal/store"
)

// Operator review queue for the optional memory gate (internal/voter.Gate).
// When the judge is uncertain about a proposed memory the node does not vote
// on it; the memory waits here until the operator accepts or rejects it, and
// the voter then casts that decision on its next tick. Operator-only: the
// queue shows memory content from every domain.

type writeGateReviewStore interface {
	ReviewQueue(ctx context.Context, limit int) ([]store.ReviewItem, error)
	GateJudgements(ctx context.Context, memoryID string) ([]memory.Judgement, error)
	SetReviewDecision(ctx context.Context, memoryID, decision, decidedBy, note string) error
}

func (h *DashboardHandler) writeGateStore(w http.ResponseWriter) (writeGateReviewStore, bool) {
	rs, ok := h.store.(writeGateReviewStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this node's store does not support the write gate")
		return nil, false
	}
	return rs, true
}

// handleReviewQueue: GET /v1/dashboard/memory/review-queue?limit=N
func (h *DashboardHandler) handleReviewQueue(w http.ResponseWriter, r *http.Request) {
	rs, ok := h.writeGateStore(w)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := rs.ReviewQueue(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []store.ReviewItem{}
	}
	writeJSONResp(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// handleMemoryJudgements: GET /v1/dashboard/memory/{id}/judgements
func (h *DashboardHandler) handleMemoryJudgements(w http.ResponseWriter, r *http.Request) {
	rs, ok := h.writeGateStore(w)
	if !ok {
		return
	}
	js, err := rs.GateJudgements(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if js == nil {
		js = []memory.Judgement{}
	}
	writeJSONResp(w, http.StatusOK, map[string]any{"judgements": js})
}

// handleReviewDecision: POST /v1/dashboard/memory/{id}/review
// body {"decision": "accept"|"reject", "note": "..."}
func (h *DashboardHandler) handleReviewDecision(w http.ResponseWriter, r *http.Request) {
	rs, ok := h.writeGateStore(w)
	if !ok {
		return
	}
	var body struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	err := rs.SetReviewDecision(r.Context(), chi.URLParam(r, "id"), body.Decision, "operator", body.Note)
	switch {
	case errors.Is(err, store.ErrNotAwaitingReview):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSONResp(w, http.StatusOK, map[string]any{"memory_id": chi.URLParam(r, "id"),
			"decision": body.Decision, "note": "the node votes this decision on its next voter tick"})
	}
}
