package tx

import (
	"crypto/ed25519"
	"sync"
	"testing"
)

// TestMonotonicNonce_StrictlyIncreasingAndNonZero is the core invariant the
// app-v9 consensus nonce gate relies on: rapid successive calls for one key must
// strictly increase (so two txs from the same producer never collide) and never
// be 0 (which the consensus path rejects as the replay sentinel). 10k rapid calls
// exercise the max(UnixNano, last+1) fallback when the wall clock doesn't advance.
func TestMonotonicNonce_StrictlyIncreasingAndNonZero(t *testing.T) {
	_, sk, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var prev uint64
	for i := 0; i < 10000; i++ {
		n := MonotonicNonce(sk)
		if n == 0 {
			t.Fatalf("MonotonicNonce returned 0 at iter %d (would trip the app-v9 nonce-0 sentinel)", i)
		}
		if n <= prev {
			t.Fatalf("not strictly increasing: got %d <= prev %d at iter %d", n, prev, i)
		}
		prev = n
	}
}

// TestMonotonicNonce_PerKeyIndependent confirms each signing key gets its own
// monotonic sequence (the map is keyed by pubkey), so interleaved producers
// signing with different keys don't perturb each other.
func TestMonotonicNonce_PerKeyIndependent(t *testing.T) {
	_, sk1, _ := ed25519.GenerateKey(nil)
	_, sk2, _ := ed25519.GenerateKey(nil)
	var last1, last2 uint64
	for i := 0; i < 1000; i++ {
		n1 := MonotonicNonce(sk1)
		n2 := MonotonicNonce(sk2)
		if n1 <= last1 {
			t.Fatalf("key1 not strictly increasing: %d <= %d", n1, last1)
		}
		if n2 <= last2 {
			t.Fatalf("key2 not strictly increasing: %d <= %d", n2, last2)
		}
		last1, last2 = n1, n2
	}
}

// TestMonotonicNonce_ConcurrentNoDuplicates is the reason the allocator is
// mutex-guarded: many producers sharing ONE key (e.g. every REST/web handler on
// the shared node priv-validator key) call concurrently and must each get a
// distinct value. Run with -race to verify the lock. Any duplicate would mean two
// txs carry the same nonce and the consensus gate drops the second.
func TestMonotonicNonce_ConcurrentNoDuplicates(t *testing.T) {
	_, sk, _ := ed25519.GenerateKey(nil)
	const goroutines = 16
	const perG = 1000

	var wg sync.WaitGroup
	results := make([][]uint64, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			out := make([]uint64, perG)
			for i := 0; i < perG; i++ {
				out[i] = MonotonicNonce(sk)
			}
			results[g] = out
		}(g)
	}
	wg.Wait()

	seen := make(map[uint64]bool, goroutines*perG)
	for _, out := range results {
		for _, n := range out {
			if n == 0 {
				t.Fatal("MonotonicNonce returned 0 under concurrency")
			}
			if seen[n] {
				t.Fatalf("duplicate nonce %d under concurrency (lock failure)", n)
			}
			seen[n] = true
		}
	}
	if len(seen) != goroutines*perG {
		t.Fatalf("expected %d distinct nonces, got %d", goroutines*perG, len(seen))
	}
}
