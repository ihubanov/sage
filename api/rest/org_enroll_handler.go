package rest

// Org fleet-enrollment endpoint: POST /v1/org/{org_id}/enroll.
//
// A node joins an org fleet by redeeming a single-use bearer enrollment token
// (issued out-of-band by the owner to a named node) and receiving the org's
// first signed policy. This handler is the HTTP consumer of the durable,
// retry-safe enrollment mechanism in internal/orgpolicy (RFC #443 point 2):
//
//   - PoP of the enrollee is free. The request is signed by the enrolling node's
//     own ed25519 key, so Ed25519AuthMiddleware has already proven the caller
//     holds that key; ContextAgentID(r) IS the enrollee pubkey. The node is not
//     yet enrolled and is neither Root nor a promoted Admin, so the outer
//     appV23LocalAdminBoundary passes it through (it clamps only Root/Admin
//     authority to loopback). The bearer token and first-policy verification are
//     what actually gate enrollment.
//
//   - Owner redeem-time confirmation. The token pins, at issue time, the org
//     policy signing key (and optionally the key epoch) the first policy must
//     verify under. The handler resolves the authorized signer from that pinned
//     key (plus any rotation attestations the enrollee presents) and verifies the
//     submitted first policy under it. A policy signed by any other key — e.g. one
//     handed to the node by a rogue peer — is refused.
//
//   - Idempotent, single-write redemption. LoadEnrollment makes redemption
//     replay-safe: the same node re-presenting the same token replays the stored
//     outcome verbatim (a lost HTTP response never strands the node behind a token
//     it cannot reuse); a different node presenting an already-redeemed token is
//     refused. On first redeem, CommitFirstEnrollment persists the consume marker,
//     the bound identity, the first policy and the accepted high-water mark in ONE
//     atomic SetStatesAtomic transaction — there is no two-phase window.
//
// The enrollment-token lookup is wired by the issuance slice; until then the
// endpoint reports 503. The orgpolicy accepted-chain writes go through badgerKV,
// a one-struct adapter from *store.BadgerStore to orgpolicy.KV.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l33tdawg/sage/api/rest/middleware"
	"github.com/l33tdawg/sage/internal/auth"
	"github.com/l33tdawg/sage/internal/orgpolicy"
	"github.com/l33tdawg/sage/internal/store"
)

// orgEnrollTokenStore is the narrow lookup the enrollment handler needs. It is
// an interface so handler tests can stub it without a database, mirroring
// mcpTokenStore. The implementation (SQLite, SHA-256 digest keyed) lands with
// the issuance slice. Lookup is by the hex SHA-256 digest of the presented
// bearer; it returns store.ErrOrgEnrollTokenNotFound for an unknown digest.
type orgEnrollTokenStore interface {
	LookupOrgEnrollmentToken(ctx context.Context, tokenSHA256 string) (*store.OrgEnrollmentToken, error)
}

// badgerKV adapts *store.BadgerStore to orgpolicy.KV. orgpolicy stays pure (no
// badger import) by depending on this minimal surface; the only coupling is the
// field-for-field copy between orgpolicy.KVWrite and store.StateWrite.
type badgerKV struct{ bs *store.BadgerStore }

func (k badgerKV) GetState(key string) ([]byte, error) { return k.bs.GetState(key) }

func (k badgerKV) SetStatesAtomic(writes []orgpolicy.KVWrite) error {
	sw := make([]store.StateWrite, len(writes))
	for i, e := range writes {
		sw[i] = store.StateWrite{Key: e.Key, Value: e.Value}
	}
	return k.bs.SetStatesAtomic(sw)
}

// OrgEnrollRequest is the JSON body for POST /v1/org/{org_id}/enroll.
type OrgEnrollRequest struct {
	Token string `json:"token"` // one-shot bearer enrollment token (plaintext; only its digest is stored)
	// FirstPolicy is the org's signed policy the node will operate under. The
	// server verifies it under the signer pinned by the token before recording it.
	FirstPolicy *orgpolicy.Policy `json:"first_policy"`
	// Attestations is the optional rotation chain, rooted at the token's pinned
	// org signer, proving a successor key currently signs the policy.
	Attestations []orgpolicy.RotationAttestation `json:"attestations,omitempty"`
}

// OrgEnrollResponse is returned on a successful redeem (201) or an idempotent
// replay (200). It carries the verified first policy so the node can start.
type OrgEnrollResponse struct {
	OrgID         string                  `json:"org_id"`
	EnrolleePub   string                  `json:"enrollee_pub"`
	BoundIdentity string                  `json:"bound_identity"`
	FirstPolicy   *orgpolicy.Policy       `json:"first_policy"`
	Accepted      orgpolicy.AcceptedState `json:"accepted"`
	Replayed      bool                    `json:"replayed"` // true on an idempotent replay of a prior redeem
}

// handleOrgEnroll redeems an enrollment token and delivers the org's first
// policy, recording the outcome in a single durable write.
func (s *Server) handleOrgEnroll(w http.ResponseWriter, r *http.Request) {
	if s.orgEnrollTokens == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "org enrollment is not configured on this node")
		return
	}
	if s.badgerStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "consensus state store unavailable")
		return
	}

	orgID := strings.TrimSpace(chi.URLParam(r, "org_id"))
	if orgID == "" {
		writeJSONError(w, http.StatusBadRequest, "org_id is required")
		return
	}

	var req OrgEnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "token is required")
		return
	}
	if req.FirstPolicy == nil {
		writeJSONError(w, http.StatusBadRequest, "first_policy is required")
		return
	}

	// PoP: the request signer is the enrolling node (middleware already verified
	// it holds the private key for this pubkey).
	enrolleePub := middleware.ContextAgentID(r.Context())
	if enrolleePub == "" {
		writeJSONError(w, http.StatusUnauthorized, "request must be signed by the enrolling node")
		return
	}

	// Resolve the bearer to its intended-enrollee binding. Not-found and revoked
	// collapse to one opaque rejection so the endpoint is not an existence oracle.
	digest := sha256.Sum256([]byte(req.Token))
	tok, err := s.orgEnrollTokens.LookupOrgEnrollmentToken(r.Context(), hex.EncodeToString(digest[:]))
	if err != nil {
		if errors.Is(err, store.ErrOrgEnrollTokenNotFound) || errors.Is(err, store.ErrOrgEnrollTokenRevoked) {
			writeJSONError(w, http.StatusForbidden, "invalid or revoked enrollment token")
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "enrollment token lookup unavailable")
		return
	}
	if tok == nil || tok.Revoked() {
		writeJSONError(w, http.StatusForbidden, "invalid or revoked enrollment token")
		return
	}
	if tok.OrgID != orgID {
		writeJSONError(w, http.StatusForbidden, "enrollment token is not valid for this org")
		return
	}

	estore := orgpolicy.NewStore(badgerKV{bs: s.badgerStore})

	// Idempotent redemption: a prior outcome for this token is replayed to the
	// same node and refused to any other.
	if prior, ok, loadErr := estore.LoadEnrollment(tok.ID); loadErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "enrollment lookup failed")
		return
	} else if ok {
		if prior.EnrolleePub != enrolleePub {
			writeJSONError(w, http.StatusConflict, "enrollment token already redeemed by another identity")
			return
		}
		writeJSON(w, http.StatusOK, OrgEnrollResponse{
			OrgID:         prior.OrgID,
			EnrolleePub:   prior.EnrolleePub,
			BoundIdentity: prior.BoundIdentity,
			FirstPolicy:   prior.FirstPolicy,
			Accepted:      prior.Accepted,
			Replayed:      true,
		})
		return
	}

	// First redeem. Resolve the authorized signer from the token's pinned org key
	// (plus any presented rotation attestations), then verify the first policy
	// under it — the owner's redeem-time confirmation that this is the right org
	// under the right signer.
	pinned, err := auth.AgentIDToPublicKey(tok.OrgSignerPub)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "enrollment token has an invalid org signer key")
		return
	}
	authorized, err := orgpolicy.ResolveSigner(pinned, req.Attestations)
	if err != nil {
		writeJSONError(w, http.StatusForbidden, "rotation chain is not rooted at the org key pinned by the token")
		return
	}
	if err := req.FirstPolicy.Verify(authorized); err != nil {
		writeJSONError(w, http.StatusForbidden, "first policy is not signed by the org key pinned by the token")
		return
	}
	if req.FirstPolicy.OrgID != orgID {
		writeJSONError(w, http.StatusForbidden, "first policy org does not match this org")
		return
	}
	if tok.KeyEpoch != "" && req.FirstPolicy.KeyEpoch != tok.KeyEpoch {
		writeJSONError(w, http.StatusForbidden, "first policy key epoch does not match the enrollment token")
		return
	}

	boundIdentity := tok.OwnerID + "/" + tok.NodeLabel + "/" + tok.Role
	outcome := orgpolicy.NewEnrollmentOutcome(tok.ID, enrolleePub, boundIdentity, req.FirstPolicy, time.Now().Unix())
	if err := estore.CommitFirstEnrollment(outcome); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to persist enrollment")
		return
	}

	writeJSON(w, http.StatusCreated, OrgEnrollResponse{
		OrgID:         outcome.OrgID,
		EnrolleePub:   outcome.EnrolleePub,
		BoundIdentity: outcome.BoundIdentity,
		FirstPolicy:   outcome.FirstPolicy,
		Accepted:      outcome.Accepted,
		Replayed:      false,
	})
}
