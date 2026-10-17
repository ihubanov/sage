package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

// TestListFederatedGroupGuestsForGroupScopesToTheGroup is the regression guard
// for the group projection: a federated agent must appear in the group it is
// attached to, and must not leak into a group it was never attached to.
func TestListFederatedGroupGuestsForGroupScopesToTheGroup(t *testing.T) {
	ctx := context.Background()
	s, err := NewSQLiteStore(ctx, t.TempDir()+"/guest-group.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, operator, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	remoteAgent := func() string {
		pub, _, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		return strings.ToLower(stringHex(pub))
	}
	put := func(groupID, chainID, agentID, state string, revision int64) {
		t.Helper()
		guest := FederatedGroupGuest{
			GroupID: groupID, RemoteChainID: chainID, RemoteAgentID: agentID,
			AgreementBindingDigest: strings.Repeat("ab", 32),
			MaxClassification:      2, Revision: revision, State: state,
		}
		if err := SignFederatedGroupGuest(&guest, operator); err != nil {
			t.Fatal(err)
		}
		if err := s.PutFederatedGroupGuest(ctx, guest); err != nil {
			t.Fatal(err)
		}
	}

	agentA, agentB, agentC, agentRevoked := remoteAgent(), remoteAgent(), remoteAgent(), remoteAgent()
	put("group-one", "chain-a", agentA, FederatedGuestStateActive, 1)
	put("group-one", "chain-b", agentB, FederatedGuestStateActive, 1)
	put("group-two", "chain-a", agentC, FederatedGuestStateActive, 1)
	// A revoked link stays in the table but must never be projected as a member.
	put("group-one", "chain-a", agentRevoked, FederatedGuestStateRevoked, 1)

	members, err := s.ListFederatedGroupGuestsForGroup(ctx, "group-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("group-one members = %d, want 2 (revoked excluded, other group excluded)", len(members))
	}
	for _, member := range members {
		if member.GroupID != "group-one" {
			t.Fatalf("member from another group leaked: %+v", member)
		}
		if member.State == FederatedGuestStateRevoked {
			t.Fatalf("revoked link projected as a member: %+v", member)
		}
	}

	other, err := s.ListFederatedGroupGuestsForGroup(ctx, "group-two")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].RemoteAgentID != agentC {
		t.Fatalf("group-two members = %+v, want only the link attached to it", other)
	}

	none, err := s.ListFederatedGroupGuestsForGroup(ctx, "group-absent")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("absent group returned %d members", len(none))
	}
}
