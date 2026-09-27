package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/l33tdawg/sage/internal/memory"
	"github.com/l33tdawg/sage/internal/store"
	"github.com/l33tdawg/sage/internal/voter"
)

// Operator review queue for the optional memory gate (internal/voter.Gate).
// When the judges are uncertain about a proposed memory the node does not vote
// on it; the memory waits here until the operator accepts or rejects it, and
// the voter then applies that decision — together with fresh built-in checks —
// on its next tick. Operator-only: the queue shows memory content.
//
// Reads go through the same projection-integrity path as the dashboard's
// other broad memory reads: the broad-read gate on the route, the sealed
// source/retry loop, internal-domain hiding, and the batch classifier that
// omits quarantined rows. Content that cannot be produced in plaintext is
// reported as unavailable and never passed to the classifier.

type memoryGateReviewStore interface {
	ReviewQueue(ctx context.Context, version string, limit int) ([]store.HeldForReview, error)
	ReviewMemory(ctx context.Context, memoryID string) (*memory.MemoryRecord, bool, error)
	GateVerdicts(ctx context.Context, memoryID string) ([]store.GateVerdict, error)
	SetReviewDecision(ctx context.Context, memoryID, version, decision, decidedBy, note string) error
}

// SetMemoryGate tells the dashboard which gate the node runs (nil = off).
func (h *DashboardHandler) SetMemoryGate(g *voter.Gate) { h.memoryGate.Store(g) }

// memoryGateStatus is what the dashboard reports about the gate.
func (h *DashboardHandler) memoryGateStatus() map[string]any {
	g := h.memoryGate.Load()
	if g == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":         true,
		"judge_version":   g.Version,
		"judges":          len(g.Judges),
		"include_domains": nonNilStrings(g.IncludeDomainPrefixes),
		"exempt_domains":  nonNilStrings(g.ExemptDomainPrefixes),
	}
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type reviewQueueItem struct {
	MemoryID           string    `json:"memory_id"`
	DomainTag          string    `json:"domain_tag,omitempty"`
	MemoryType         string    `json:"memory_type,omitempty"`
	Content            string    `json:"content,omitempty"`
	ContentUnavailable bool      `json:"content_unavailable,omitempty"`
	Reason             string    `json:"reason"`
	P                  float64   `json:"p_yes"`
	HeldAt             time.Time `json:"held_at"`
}

// handleReviewQueue: GET /v1/dashboard/memory/review-queue?limit=N
func (h *DashboardHandler) handleReviewQueue(w http.ResponseWriter, r *http.Request) {
	g := h.memoryGate.Load()
	if g == nil {
		writeJSONResp(w, http.StatusOK, map[string]any{
			"gate": h.memoryGateStatus(), "items": []reviewQueueItem{}, "count": 0,
		})
		return
	}
	rs, ok := h.store.(memoryGateReviewStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this node's store does not support the memory gate")
		return
	}
	if !h.appV23IsActive() {
		h.handleReviewQueueUnsealed(w, r, g, rs)
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		source, cacheable, err := h.appV23GraphSource(r.Context())
		if err != nil {
			if writeAppV23DashboardProjectionFailure(w, err) {
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !cacheable {
			writeAppV23DashboardProjectionFailure(w, appV23DashboardProjectionError(
				errors.New("review queue source revisions are unavailable")))
			return
		}
		sealed := newSealedDashboardResponse()
		h.handleReviewQueueUnsealed(sealed, r, g, rs)
		current, sealErr := h.sealAppV23DashboardSource(r.Context(), source)
		if sealErr != nil {
			if writeAppV23DashboardProjectionFailure(w, sealErr) {
				return
			}
			writeError(w, http.StatusInternalServerError, sealErr.Error())
			return
		}
		if !current {
			continue
		}
		writeSealedDashboardResponse(w, sealed)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "review queue changed while it was being prepared; retry")
}

func (h *DashboardHandler) handleReviewQueueUnsealed(w http.ResponseWriter, r *http.Request, g *voter.Gate, rs memoryGateReviewStore) {
	ctx := r.Context()
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	held, err := rs.ReviewQueue(ctx, g.Version, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := make(map[string]store.HeldForReview, len(held))
	readable := make([]*memory.MemoryRecord, 0, len(held))
	items := make([]reviewQueueItem, 0, len(held))
	for _, it := range held {
		rec, available, rerr := rs.ReviewMemory(ctx, it.MemoryID)
		if errors.Is(rerr, store.ErrMemoryNotFound) {
			continue
		}
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, rerr.Error())
			return
		}
		if isCerebrumInternalMemoryDomain(rec.DomainTag) {
			continue
		}
		byID[it.MemoryID] = it
		if !available {
			// Never classified: undecryptable content would be misread as a
			// projection defect. Reported without content; it cannot be
			// decided until the content can be read.
			items = append(items, reviewQueueItem{MemoryID: it.MemoryID, ContentUnavailable: true,
				Reason: it.Reason, P: it.P, HeldAt: it.HeldAt})
			continue
		}
		readable = append(readable, rec)
	}
	kept, err := h.filterAppV23BroadDashboardRecords(readable)
	if err != nil {
		if writeAppV23DashboardProjectionFailure(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, rec := range kept {
		it := byID[rec.MemoryID]
		items = append(items, reviewQueueItem{MemoryID: rec.MemoryID, DomainTag: rec.DomainTag,
			MemoryType: string(rec.MemoryType), Content: rec.Content,
			Reason: it.Reason, P: it.P, HeldAt: it.HeldAt})
	}
	response := map[string]any{"gate": h.memoryGateStatus(), "items": items, "count": len(items)}
	if projection := h.appV23ProjectionResponseForContext(ctx); projection != nil {
		response["projection"] = h.projectionResponseForRequest(r, projection)
	}
	writeJSONResp(w, http.StatusOK, response)
}

// handleMemoryJudgements: GET /v1/dashboard/memory/{id}/judgements — the
// gate's stored verdicts for one memory (no content).
func (h *DashboardHandler) handleMemoryJudgements(w http.ResponseWriter, r *http.Request) {
	rs, ok := h.store.(memoryGateReviewStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this node's store does not support the memory gate")
		return
	}
	vs, err := rs.GateVerdicts(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if vs == nil {
		vs = []store.GateVerdict{}
	}
	writeJSONResp(w, http.StatusOK, map[string]any{"gate": h.memoryGateStatus(), "verdicts": vs})
}

// handleReviewDecision: POST /v1/dashboard/memory/{id}/review
// body {"decision": "accept"|"reject", "note": "..."}
func (h *DashboardHandler) handleReviewDecision(w http.ResponseWriter, r *http.Request) {
	g := h.memoryGate.Load()
	if g == nil {
		writeError(w, http.StatusConflict, "the memory gate is disabled on this node; nothing is held for review")
		return
	}
	rs, ok := h.store.(memoryGateReviewStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this node's store does not support the memory gate")
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
	id := chi.URLParam(r, "id")
	err := rs.SetReviewDecision(r.Context(), id, g.Version, body.Decision, "operator", body.Note)
	switch {
	case errors.Is(err, store.ErrNotAwaitingReview):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSONResp(w, http.StatusOK, map[string]any{"memory_id": id, "decision": body.Decision,
			"note": "the node applies this decision, with fresh built-in checks, on its next voter tick"})
	}
}
