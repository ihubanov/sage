package store

// Org fleet-enrollment bearer tokens.
//
// An org enrollment token is a single-use bearer credential the owner issues to
// a named-but-not-yet-keyed node, which the node redeems at POST
// /v1/org/{org_id}/enroll to join the fleet (see api/rest/org_enroll_handler.go).
// Following the mcp_tokens precedent, only the SHA-256 digest of the token is
// ever persisted; the plaintext is shown once at issuance.
//
// This file defines the data shape and the lookup contract the REST handler
// depends on. The persistence implementation (issue / lookup-by-digest / revoke
// on SQLite) lands with the issuance slice; the handler consumes it through a
// narrow interface so it is testable against a stub today.
//
// Single-use is NOT tracked by a consumed flag here: redemption is made durable
// and idempotent by the orgpolicy enrollment-outcome record (keyed by this
// token's ID), which is both the replayable result and the consume marker. See
// internal/orgpolicy/store.go (CommitFirstEnrollment / LoadEnrollment).

import (
	"errors"
	"time"
)

// Sentinel errors a LookupOrgEnrollmentToken implementation may return. A
// not-found and a revoked token are reported distinctly so the handler can fail
// closed without turning the endpoint into a token-existence oracle (it collapses
// both to one opaque rejection on the wire).
var (
	ErrOrgEnrollTokenNotFound = errors.New("org enrollment token not found")
	ErrOrgEnrollTokenRevoked  = errors.New("org enrollment token revoked")
)

// OrgEnrollmentToken is the issued-once binding an owner commits at token-issue
// time. It names the intended enrollee (owner + node label + org role) and pins
// the org policy signing key the node's first policy MUST verify under — the
// node's redeem-time confirmation that it is joining the right org under the
// right signer, not a policy handed to it by a rogue peer.
//
// It never carries the token plaintext; callers look it up by the hex SHA-256
// digest of the presented bearer.
type OrgEnrollmentToken struct {
	ID           string    // stable token ID; also the orgpolicy enrollment-record key
	OrgID        string    // org this token admits a node into
	OwnerID      string    // owning agent (hex ed25519) that issued the token
	NodeLabel    string    // operator-chosen label for the intended node
	Role         string    // org role the node is admitted as
	OrgSignerPub string    // hex ed25519: pinned org policy signing key (rotation root)
	KeyEpoch     string    // expected key epoch of the first policy ("" = unpinned)
	IssuedAt     time.Time // issuance time
	RevokedAt    time.Time // zero unless revoked
}

// Revoked reports whether the token has been revoked.
func (t *OrgEnrollmentToken) Revoked() bool { return t != nil && !t.RevokedAt.IsZero() }
