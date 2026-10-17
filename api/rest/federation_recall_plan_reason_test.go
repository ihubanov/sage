package rest

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/l33tdawg/sage/internal/federation"
	"github.com/l33tdawg/sage/internal/store"
)

func TestRecallPlanTimeoutIsBoundedAndTunable(t *testing.T) {
	t.Setenv("SAGE_FED_RECALL_PLAN_TIMEOUT_MS", "")
	assert.Equal(t, 20*time.Second, recallPlanTimeout(), "a plan must never inherit an unbounded caller deadline")

	t.Setenv("SAGE_FED_RECALL_PLAN_TIMEOUT_MS", "2500")
	assert.Equal(t, 2500*time.Millisecond, recallPlanTimeout())

	for _, invalid := range []string{"0", "-5", "not-a-number"} {
		t.Setenv("SAGE_FED_RECALL_PLAN_TIMEOUT_MS", invalid)
		assert.Equal(t, 20*time.Second, recallPlanTimeout(), "invalid override %q must fall back", invalid)
	}
}

// recallTargetsFederation gives each peer its own status or error so a single
// fixture can express "unreachable", "reachable but not sharing", and "shares".
type recallTargetsFederation struct {
	*fakeFederation
	statuses map[string]*federation.StatusResponse
	errs     map[string]error
}

func (f *recallTargetsFederation) PeerStatus(_ context.Context, chainID string) (*federation.StatusResponse, error) {
	if err := f.errs[chainID]; err != nil {
		return nil, err
	}
	return f.statuses[chainID], nil
}

func sharingStatus(domain string) *federation.StatusResponse {
	return &federation.StatusResponse{
		ChainID: "chain-a",
		PeerRBACGrant: &federation.PeerRBACGrant{Domains: []federation.PeerRBACDomainGrant{
			{Domain: domain, Read: true},
		}},
	}
}

func newRecallTargetsServer(t *testing.T, chains ...string) (*Server, *store.BadgerStore) {
	t.Helper()
	badger, err := store.NewBadgerStore(filepath.Join(t.TempDir(), "badger"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = badger.CloseBadger() })
	for _, chain := range chains {
		require.NoError(t, badger.SetCrossFed(chain, "https://redacted.invalid", []byte(chain), 2, 0, []string{"*"}, nil, "active"))
	}
	return &Server{badgerStore: badger}, badger
}

// TestFederationRecallTargetsClassifiesAnEmptySet is the regression guard for
// the defect an external agent reported: an empty destination list used to be
// completely unexplained, so "no peer authorizes this domain" was
// indistinguishable from "every peer probe failed" and from "this node has no
// peers at all". Each case below must answer which layer produced the empty
// set, without naming any peer.
func TestFederationRecallTargetsClassifiesAnEmptySet(t *testing.T) {
	for _, test := range []struct {
		name      string
		chains    []string
		requested []string
		domain    string
		statuses  map[string]*federation.StatusResponse
		errs      map[string]error
		want      federation.UnavailableReason
		wantPeers int
	}{
		{
			name:   "no_active_agreement",
			domain: "shared",
			want:   federation.RecallUnavailableNoAgreement,
		},
		{
			name:      "requested_chains_not_connected",
			chains:    []string{"chain-a"},
			requested: []string{"chain-z"},
			domain:    "shared",
			want:      federation.RecallUnavailableRequestedChains,
		},
		{
			name:   "every_probe_failed_is_not_a_permission_verdict",
			chains: []string{"chain-a"},
			domain: "shared",
			errs:   map[string]error{"chain-a": errors.New("dial tcp: i/o timeout")},
			want:   federation.RecallUnavailablePeerUnreachable,
		},
		{
			name:     "reachable_but_not_sharing",
			chains:   []string{"chain-a"},
			domain:   "shared",
			statuses: map[string]*federation.StatusResponse{"chain-a": sharingStatus("other-domain")},
			want:     federation.RecallUnavailableDomainNotShared,
		},
		{
			name:      "sharing_succeeds",
			chains:    []string{"chain-a"},
			domain:    "shared",
			statuses:  map[string]*federation.StatusResponse{"chain-a": sharingStatus("shared")},
			wantPeers: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, _ := newRecallTargetsServer(t, test.chains...)
			srv.federation = &recallTargetsFederation{
				fakeFederation: &fakeFederation{},
				statuses:       test.statuses,
				errs:           test.errs,
			}
			targets, reason := srv.federationRecallTargetsWithReason(context.Background(), test.requested, test.domain)
			assert.Len(t, targets, test.wantPeers)
			assert.Equal(t, test.want, reason)
		})
	}
}

func TestFederationRecallTargetsUnavailableWhenFederationAbsent(t *testing.T) {
	srv, _ := newRecallTargetsServer(t, "chain-a")
	targets, reason := srv.federationRecallTargetsWithReason(context.Background(), nil, "shared")
	assert.Empty(t, targets)
	assert.Equal(t, federation.RecallUnavailableFederation, reason)
}

func TestRecallPlanUnavailableReasonSerialisesOnlyWhenSet(t *testing.T) {
	empty, err := jsonMarshalReason("")
	require.NoError(t, err)
	assert.NotContains(t, empty, "unavailable_reason", "a successful plan must not carry a reason field")

	set, err := jsonMarshalReason(string(federation.RecallUnavailableDomainNotShared))
	require.NoError(t, err)
	assert.Contains(t, set, `"unavailable_reason":"domain_not_shared"`)
}

func jsonMarshalReason(reason string) (string, error) {
	plan := federation.RecallPlan{
		Destinations:      []string{},
		AgreementBindings: map[string]string{},
	}
	if reason != "" {
		plan.UnavailableReason = federation.UnavailableReason(reason)
	}
	raw, err := json.Marshal(plan)
	return string(raw), err
}
