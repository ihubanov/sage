package store

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkAuditPublicMemoryEnvelopes measures the stopped-node census that
// `sage-gui upgrade public-memory-audit` and app-v28 preflight run. It exists
// so an operator-facing cost estimate is based on a measurement rather than a
// guess; it is not part of the default test run.
func BenchmarkAuditPublicMemoryEnvelopes(b *testing.B) {
	const records = 2000
	base, err := NewBadgerStore(b.TempDir())
	require.NoError(b, err)
	b.Cleanup(func() { _ = base.CloseBadger() })
	scoped := base.BeginConsensusTransaction(nil)
	for index := 0; index < records; index++ {
		identifier := fmt.Sprintf("audit-bench-%05d", index)
		hash := sha256.Sum256([]byte("bench:" + identifier))
		require.NoError(b, scoped.SetMemoryHash(identifier, hash[:], "committed"))
		require.NoError(b, scoped.SetMemoryAuthor(identifier, strings.Repeat("a", 64)))
		require.NoError(b, scoped.SetMemoryAuthorPrincipal(identifier, "bench-principal"))
		require.NoError(b, scoped.SetMemoryDomain(identifier, "bench-domain"))
		require.NoError(b, scoped.SetMemoryClassification(identifier, 0))
	}
	require.NoError(b, scoped.CommitConsensusTransaction())

	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		audit, auditErr := base.AuditPublicMemoryEnvelopes(10)
		require.NoError(b, auditErr)
		require.Equal(b, records, audit.Public)
		require.Zero(b, audit.Blocking)
	}
}
