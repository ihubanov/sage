package rest

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/l33tdawg/sage/api/rest/middleware"
	"github.com/l33tdawg/sage/internal/federation"
)

type federationRecallPlanner interface {
	PlanRecall(ctx context.Context, targets []string, agentID, domain string) (*federation.RecallPlan, error)
}

type federationRecallPlanRequest struct {
	DomainTag      string   `json:"domain_tag"`
	FederateChains []string `json:"federate_chains,omitempty"`
}

// recallPlanTimeout bounds one recall-plan request end to end. A plan fans out
// to as many as maxAgentFederationTargets peers and costs two sequential round
// trips each, so it needs a larger budget than a single federated recall
// (fedRecallTimeout, 4s). Without this the handler inherited whatever deadline
// the client chose and the outbound client, which deliberately sets no
// transport timeout, would wait indefinitely.
func recallPlanTimeout() time.Duration {
	if v := os.Getenv("SAGE_FED_RECALL_PLAN_TIMEOUT_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 20 * time.Second
}

// handleFederationRecallPlan is the first half of the v23 two-request flow.
// It is authenticated as the original local agent, filters topology through
// that caller's local Read authority, and returns only exact destination state
// that must be included in the subsequently signed recall body.
func (s *Server) handleFederationRecallPlan(w http.ResponseWriter, r *http.Request) {
	// Bound the whole request. Every sibling handler on this call path already
	// does this; this one did not, so a single unreachable peer could hold the
	// request open for minutes with no way for the caller to tell a stalled
	// plan from a slow success.
	ctx, cancel := context.WithTimeout(r.Context(), recallPlanTimeout())
	defer cancel()

	callerID := middleware.ContextAgentID(r.Context())
	if !s.requireAppV23ActiveOrdinaryAgent(
		w, callerID, "federated recall planning",
	) {
		return
	}
	planner, ok := s.federation.(federationRecallPlanner)
	if !ok {
		writeProblem(w, http.StatusNotImplemented, "Federated recall unavailable",
			"Federation protocol v23 recall planning is not available on this node.")
		return
	}
	var req federationRecallPlanRequest
	if err := decodeJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	allowed, _ := s.federationCallerCanRead(ctx, callerID, req.DomainTag)
	if !allowed {
		writeProblem(w, http.StatusForbidden, "Access denied",
			"Caller is not authorized to read this federated domain.")
		return
	}
	targets := req.FederateChains
	if len(targets) == 0 {
		targets = []string{"*"}
	}
	privilegedTopology := s.nodeOperatorID != "" && callerID == s.nodeOperatorID
	if s.isPostV23ForNextTx() {
		privilegedTopology = s.callerIsOperatorOrAdmin(ctx, callerID)
	}
	if !privilegedTopology {
		var reason federation.UnavailableReason
		targets, reason = s.federationRecallTargetsWithReason(ctx, targets, req.DomainTag)
		if len(targets) == 0 {
			writeJSON(w, http.StatusOK, &federation.RecallPlan{
				ProtocolVersion:           federation.FederationProtocolV23,
				SourceChainID:             s.federation.LocalChainID(),
				Destinations:              []string{},
				AgreementBindings:         map[string]string{},
				QueryChallenges:           map[string]string{},
				AuthorizationModels:       map[string]string{},
				AuthorizationAttestations: map[string]federation.SourceAuthorizationAttestation{},
				ExpiresAt:                 map[string]int64{},
				// Without this the empty plan is indistinguishable from a planner
				// that failed silently, and "no authorized destination" reads as
				// an authorization verdict even when the cause was a dead peer.
				UnavailableReason: reason,
			})
			return
		}
	}
	plan, err := planner.PlanRecall(ctx, targets, callerID, req.DomainTag)
	if err != nil {
		if ctx.Err() != nil {
			// A plan that could not finish is a statement about THIS node's
			// reachability, not about the caller's authority. Say so, so the
			// caller does not narrow its request chasing a permission problem.
			writeJSON(w, http.StatusOK, &federation.RecallPlan{
				ProtocolVersion:           federation.FederationProtocolV23,
				SourceChainID:             s.federation.LocalChainID(),
				Destinations:              []string{},
				AgreementBindings:         map[string]string{},
				QueryChallenges:           map[string]string{},
				AuthorizationModels:       map[string]string{},
				AuthorizationAttestations: map[string]federation.SourceAuthorizationAttestation{},
				ExpiresAt:                 map[string]int64{},
				UnavailableReason:         federation.RecallUnavailablePlanner,
			})
			return
		}
		writeProblem(w, http.StatusBadGateway, "Federated recall plan failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}
