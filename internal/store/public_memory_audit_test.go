package store

import (
	"context"
	"testing"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"
)

// TestPublicMemoryAuditNamesEveryEnvelopeDefect covers the second real-chain
// app-v28 activation blocker: a PUBLIC record that is not merely hashless but
// fails another envelope check (missing principal, truncated hash). The audit
// must name the record, the exact failed check, and whether the activation
// stage would quarantine it or refuse.
func TestPublicMemoryAuditNamesEveryEnvelopeDefect(t *testing.T) {
	base := newTestBadger(t)
	scoped := base.BeginConsensusTransaction(nil)
	publicTestRecord(t, scoped, "clean")
	publicTestRecord(t, scoped, "legacy-missing-principal")
	require.NoError(t, scoped.update(func(txn *badger.Txn) error {
		return scoped.txnDelete(txn, memoryAuthorPrincipalKey("legacy-missing-principal"))
	}))
	publicTestRecord(t, scoped, "modern-missing-principal")
	require.NoError(t, scoped.SetMemorySubmissionHeight("modern-missing-principal", 56126))
	require.NoError(t, scoped.update(func(txn *badger.Txn) error {
		return scoped.txnDelete(txn, memoryAuthorPrincipalKey("modern-missing-principal"))
	}))
	publicTestRecord(t, scoped, "legacy-short-hash")
	require.NoError(t, scoped.SetMemoryHash("legacy-short-hash", make([]byte, 16), "committed"))
	publicTestRecord(t, scoped, "modern-short-hash")
	require.NoError(t, scoped.SetMemorySubmissionHeight("modern-short-hash", 56126))
	require.NoError(t, scoped.SetMemoryHash("modern-short-hash", make([]byte, 16), "committed"))
	publicTestRecord(t, scoped, "private")
	require.NoError(t, scoped.SetMemoryClassification("private", 1))
	require.NoError(t, scoped.CommitConsensusTransaction())

	audit, err := base.AuditPublicMemoryEnvelopes(10)
	require.NoError(t, err)
	require.Equal(t, 6, audit.Scanned)
	require.Equal(t, 5, audit.Public)
	require.Equal(t, 2, audit.Quarantined)
	require.Equal(t, 2, audit.Blocking)
	require.False(t, audit.Truncated)

	byID := map[string]PublicMemoryEnvelopeDefect{}
	for _, defect := range audit.Defects {
		byID[defect.MemoryID] = defect
	}
	require.NotContains(t, byID, "clean", "a valid PUBLIC record must not be reported")
	require.NotContains(t, byID, "private", "non-public records are outside this census")

	legacyPrincipal := byID["legacy-missing-principal"]
	require.True(t, legacyPrincipal.Quarantined)
	require.False(t, legacyPrincipal.BornUnderAppV25)
	require.Equal(t, []string{"author_principal_not_recorded"}, legacyPrincipal.Reasons)

	modernPrincipal := byID["modern-missing-principal"]
	require.False(t, modernPrincipal.Quarantined)
	require.True(t, modernPrincipal.BornUnderAppV25)
	require.Equal(t, []string{"author_principal_not_recorded"}, modernPrincipal.Reasons)

	legacyHash := byID["legacy-short-hash"]
	require.True(t, legacyHash.Quarantined)
	require.Equal(t, []string{"content_hash_length_16"}, legacyHash.Reasons)

	modernHash := byID["modern-short-hash"]
	require.False(t, modernHash.Quarantined)
	require.Equal(t, []string{"content_hash_length_16"}, modernHash.Reasons)

	truncated, err := base.AuditPublicMemoryEnvelopes(1)
	require.NoError(t, err)
	require.True(t, truncated.Truncated)
	require.Len(t, truncated.Defects, 1)
	require.Equal(t, 2, truncated.Quarantined)
	require.Equal(t, 2, truncated.Blocking,
		"the defect list is bounded but the counters still describe the whole corpus")
}

// TestLegacyEnvelopeQuarantineUnblocksActivationButModernStillRefuses asserts
// both halves of the widened quarantine: every envelope defect on a pre-app-v25
// PUBLIC record is folded out of the committed set, while the same defect on a
// record born under app-v25 or later still refuses the build.
func TestLegacyEnvelopeQuarantineUnblocksActivationButModernStillRefuses(t *testing.T) {
	legacy := newTestBadger(t)
	transaction := legacy.BeginConsensusTransaction(nil)
	publicTestRecord(t, transaction, "clean")
	publicTestRecord(t, transaction, "legacy-missing-principal")
	require.NoError(t, transaction.update(func(txn *badger.Txn) error {
		return transaction.txnDelete(txn, memoryAuthorPrincipalKey("legacy-missing-principal"))
	}))
	publicTestRecord(t, transaction, "legacy-short-hash")
	require.NoError(t, transaction.SetMemoryHash("legacy-short-hash", make([]byte, 16), "committed"))
	require.NoError(t, transaction.CommitConsensusTransaction())

	target := newTestBadger(t)
	result, err := legacy.BuildPublicMemoryMigration(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, uint64(3), result.Scanned)
	require.Equal(t, uint64(1), result.Public, "only the complete legacy envelope may enter the public set")
	proof, err := target.PublicMemoryBranch("clean")
	require.NoError(t, err)
	require.NoError(t, VerifyPublicMemoryBranch(*proof, result.Root))
	_, err = target.PublicMemoryBranch("legacy-missing-principal")
	require.Error(t, err)
	_, err = target.PublicMemoryBranch("legacy-short-hash")
	require.Error(t, err)

	modern := newTestBadger(t)
	transaction = modern.BeginConsensusTransaction(nil)
	publicTestRecord(t, transaction, "modern-missing-principal")
	require.NoError(t, transaction.SetMemorySubmissionHeight("modern-missing-principal", 56126))
	require.NoError(t, transaction.update(func(txn *badger.Txn) error {
		return transaction.txnDelete(txn, memoryAuthorPrincipalKey("modern-missing-principal"))
	}))
	require.NoError(t, transaction.CommitConsensusTransaction())
	_, err = modern.BuildPublicMemoryMigration(context.Background(), newTestBadger(t))
	require.ErrorIs(t, err, ErrPublicMemoryIndex)
}

// TestPublicMemoryMigrationSkipsMissingLegacyClassification covers the
// remaining real-chain blocker found by Level Up on 2026-10-17: a record with
// no classification record carries the historical INTERNAL default, and the
// audit correctly excluded it from the public census, but the stage treated the
// same record as a fatal error. Both paths must use the same selector.
func TestPublicMemoryMigrationSkipsMissingLegacyClassification(t *testing.T) {
	base := newTestBadger(t)
	scoped := base.BeginConsensusTransaction(nil)
	publicTestRecord(t, scoped, "no-class")
	require.NoError(t, scoped.update(func(txn *badger.Txn) error {
		if err := scoped.txnDelete(txn, memClassKey("no-class")); err != nil {
			return err
		}
		return scoped.txnDelete(txn, memoryAuthorPrincipalKey("no-class"))
	}))
	publicTestRecord(t, scoped, "clean")
	require.NoError(t, scoped.CommitConsensusTransaction())

	audit, err := base.AuditPublicMemoryEnvelopes(10)
	require.NoError(t, err)
	require.Equal(t, 2, audit.Scanned)
	require.Equal(t, 1, audit.Public, "a missing class record is INTERNAL, not PUBLIC")
	require.Zero(t, audit.Blocking)

	target := newTestBadger(t)
	result, err := base.BuildPublicMemoryMigration(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, uint64(2), result.Scanned)
	require.Equal(t, uint64(1), result.Public,
		"the stage must skip what the audit excludes instead of aborting activation")
	proof, err := target.PublicMemoryBranch("clean")
	require.NoError(t, err)
	require.NoError(t, VerifyPublicMemoryBranch(*proof, result.Root))
}
