package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"database/sql"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTokenStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tokens.db")
	s, err := NewSQLiteStore(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mkDigest(token string) string {
	d := sha256.Sum256([]byte(token))
	return hex.EncodeToString(d[:])
}

func TestMCPTokens_InsertAndLookup(t *testing.T) {
	s := newTokenStore(t)
	ctx := context.Background()

	digest := mkDigest("plaintext-token-1")
	require.NoError(t, s.InsertMCPToken(ctx, "tok-1", "chatgpt", "agent-aaa", digest))

	tok, err := s.LookupMCPToken(ctx, digest)
	require.NoError(t, err)
	assert.Equal(t, "tok-1", tok.ID)
	assert.Equal(t, "chatgpt", tok.Name)
	assert.Equal(t, "agent-aaa", tok.AgentID)
	assert.False(t, tok.CreatedAt.IsZero())
}

func TestMCPTokens_Lookup_NoRows(t *testing.T) {
	s := newTokenStore(t)
	_, err := s.LookupMCPToken(context.Background(), mkDigest("missing"))
	assert.True(t, errors.Is(err, sql.ErrNoRows))
}

func TestMCPTokens_Revoke(t *testing.T) {
	s := newTokenStore(t)
	ctx := context.Background()

	digest := mkDigest("plaintext-token-2")
	require.NoError(t, s.InsertMCPToken(ctx, "tok-2", "cursor", "agent-bbb", digest))
	require.NoError(t, s.RevokeMCPToken(ctx, "tok-2"))

	// Lookup should now report ErrTokenRevoked.
	tok, err := s.LookupMCPToken(ctx, digest)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTokenRevoked))
	require.NotNil(t, tok)
	assert.False(t, tok.RevokedAt.IsZero())
}

func TestMCPTokens_Revoke_Idempotent(t *testing.T) {
	s := newTokenStore(t)
	ctx := context.Background()

	digest := mkDigest("plaintext-token-3")
	require.NoError(t, s.InsertMCPToken(ctx, "tok-3", "", "agent-ccc", digest))
	require.NoError(t, s.RevokeMCPToken(ctx, "tok-3"))
	// Second revoke should not error (idempotent — token still exists, just stays revoked).
	require.NoError(t, s.RevokeMCPToken(ctx, "tok-3"))
}

func TestMCPTokens_Revoke_Missing(t *testing.T) {
	s := newTokenStore(t)
	err := s.RevokeMCPToken(context.Background(), "does-not-exist")
	assert.True(t, errors.Is(err, sql.ErrNoRows))
}

func TestMCPTokens_List(t *testing.T) {
	s := newTokenStore(t)
	ctx := context.Background()

	require.NoError(t, s.InsertMCPToken(ctx, "id-a", "a", "agent-1", mkDigest("ta")))
	require.NoError(t, s.InsertMCPToken(ctx, "id-b", "b", "agent-2", mkDigest("tb")))
	require.NoError(t, s.InsertMCPToken(ctx, "id-c", "c", "agent-1", mkDigest("tc")))
	require.NoError(t, s.RevokeMCPToken(ctx, "id-b"))

	rows, err := s.ListMCPTokens(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	revoked := 0
	for _, r := range rows {
		if !r.RevokedAt.IsZero() {
			revoked++
			assert.Equal(t, "id-b", r.ID)
		}
	}
	assert.Equal(t, 1, revoked)
}

func TestMCPTokens_DigestUnique(t *testing.T) {
	s := newTokenStore(t)
	ctx := context.Background()

	digest := mkDigest("dupe-token")
	require.NoError(t, s.InsertMCPToken(ctx, "id-1", "n1", "agent-1", digest))
	err := s.InsertMCPToken(ctx, "id-2", "n2", "agent-2", digest)
	require.Error(t, err) // UNIQUE constraint on token_sha256
}

func TestMCPTokens_LookupBumpsLastUsed(t *testing.T) {
	s := newTokenStore(t)
	ctx := context.Background()

	digest := mkDigest("track-me")
	require.NoError(t, s.InsertMCPToken(ctx, "id-track", "tracked", "agent-x", digest))

	first, err := s.LookupMCPToken(ctx, digest)
	require.NoError(t, err)
	// First lookup may or may not have last_used set yet (write happens after read);
	// list view shows it next time.
	_ = first

	rows, err := s.ListMCPTokens(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].LastUsedAt.IsZero(), "last_used_at should be set after lookup")
}
