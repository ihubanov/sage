package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/l33tdawg/sage/internal/memory"
	"github.com/l33tdawg/sage/internal/store"
)

// REST side of the optional write gate (internal/voter.Gate). Stores that do
// not implement these interfaces behave exactly as before: nothing is hidden,
// nothing is annotated, and evidence is refused rather than silently dropped.

type writeGateRecallStore interface {
	GateAnnotationsFor(ctx context.Context, ids []string) (store.GateAnnotations, error)
}

type writeGateEvidenceStore interface {
	SetMemoryEvidence(ctx context.Context, memoryID, evidence string) error
}

// applyWriteGateRecallFilter hides memories the write gate judged to be
// superseded by a later committed memory, unless the caller asked for them.
// It is a server-side default for every recall path (vector, text, hybrid), so
// a client that never heard of the gate cannot recall a corrected fact as if it
// were current. Hidden is not deleted: include_superseded returns them.
func (s *Server) applyWriteGateRecallFilter(ctx context.Context, opts *store.QueryOptions, includeSuperseded bool) {
	if includeSuperseded {
		return
	}
	gs, ok := s.store.(writeGateRecallStore)
	if !ok {
		return
	}
	prev := opts.CandidateBatchFilter
	opts.CandidateBatchFilter = func(recs []*memory.MemoryRecord) ([]*memory.MemoryRecord, error) {
		var err error
		if prev != nil {
			if recs, err = prev(recs); err != nil {
				return nil, err
			}
		}
		if len(recs) == 0 {
			return recs, nil
		}
		ids := make([]string, len(recs))
		for i, r := range recs {
			ids[i] = r.MemoryID
		}
		ann, err := gs.GateAnnotationsFor(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("write gate annotations: %w", err)
		}
		out := recs[:0]
		for _, r := range recs {
			if _, superseded := ann.SupersededBy[r.MemoryID]; !superseded {
				out = append(out, r)
			}
		}
		return out, nil
	}
}

// annotateWriteGateResults adds the gate's judged confidence and, for results
// that are superseded (only returned under include_superseded), the memory
// that replaced them. Best-effort: a lookup failure leaves results unannotated
// rather than failing the recall.
func (s *Server) annotateWriteGateResults(ctx context.Context, results []*MemoryResult) {
	gs, ok := s.store.(writeGateRecallStore)
	if !ok || len(results) == 0 {
		return
	}
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if r != nil {
			ids = append(ids, r.MemoryID)
		}
	}
	ann, err := gs.GateAnnotationsFor(ctx, ids)
	if err != nil {
		s.logger.Warn().Err(err).Msg("write gate annotations unavailable; results returned unannotated")
		return
	}
	for _, r := range results {
		if r == nil {
			continue
		}
		if p, ok := ann.JudgedConfidence[r.MemoryID]; ok {
			pp := p
			r.JudgedConfidence = &pp
		}
		if by, ok := ann.SupersededBy[r.MemoryID]; ok {
			r.SupersededBy = by
		}
	}
}

// storeSubmittedEvidence keeps a submission's optional evidence node-local,
// BEFORE the memory is broadcast, so the voter never judges the memory without
// it. It writes the error response and returns false when the request must stop.
func (s *Server) storeSubmittedEvidence(w http.ResponseWriter, r *http.Request, memoryID, evidence string) bool {
	if evidence == "" {
		return true
	}
	if len(evidence) > store.MaxEvidenceBytes {
		writeProblem(w, http.StatusBadRequest, "Evidence too large",
			fmt.Sprintf("evidence is %d bytes; the maximum is %d", len(evidence), store.MaxEvidenceBytes))
		return false
	}
	es, ok := s.store.(writeGateEvidenceStore)
	if !ok {
		writeProblem(w, http.StatusBadRequest, "Evidence not supported",
			"This node's store does not keep submission evidence.")
		return false
	}
	if err := es.SetMemoryEvidence(r.Context(), memoryID, evidence); err != nil {
		if errors.Is(err, context.Canceled) {
			return false
		}
		writeProblem(w, http.StatusInternalServerError, "Evidence not stored", err.Error())
		return false
	}
	return true
}
