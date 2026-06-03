package tx

import (
	"crypto/ed25519"
	"sync"
	"time"
)

// nonceMu guards lastNonce. nonce allocation is process-global because a single
// signing key (notably the node priv-validator key) is shared by every REST and
// web handler in the process; they must draw from one strictly-increasing
// sequence per key.
var (
	nonceMu   sync.Mutex
	lastNonce = make(map[string]uint64)
)

// MonotonicNonce returns a strictly-increasing replay nonce for transactions
// signed by sk.
//
// Why this exists: app-v9 enforces nonce/replay protection in the CONSENSUS path
// (a tx is rejected when its nonce <= the signer's highest committed nonce). So
// every producer signing with a given key MUST emit strictly-increasing nonces,
// or a colliding/out-of-order tx is silently dropped (Code 4). Raw
// time.Now().UnixNano() is NOT safe for this: it can repeat on coarse-resolution
// clocks (notably darwin) and it races across the many concurrent producers that
// share the node signing key, so two txs in one block can carry equal or
// descending nonces and the second is rejected.
//
// This allocator is keyed by the signer's public key and returns
// max(UnixNano, lastForKey+1): it guarantees strict per-key monotonicity within
// the process regardless of clock resolution or producer concurrency, and never
// returns 0 (so it never trips app-v9's nonce-0 rejection).
//
// SCOPE / KNOWN LIMITS (both are liveness-only — the consensus verdict is always
// deterministic, never a fork):
//   - One process per signing key. The map is process-global and NOT seeded from
//     the committed on-chain nonce, so two processes signing with the SAME key
//     against the SAME chain can allocate colliding/descending nonces and app-v9
//     will drop the loser (Code 4). Don't share a node/validator key across live
//     processes — that is already a CometBFT equivocation (double-sign) hazard.
//   - Cross-restart relies on a forward wall clock. On restart the map is empty
//     and the first allocation is raw UnixNano; under NORMAL forward time that
//     exceeds the prior process's last committed nonce. A BACKWARD clock step
//     (NTP correction, manual set, VM snapshot restore) past the committed nonce
//     temporarily stalls that key (Code 4) until the clock catches up. Bounded by
//     the size of the backward step and self-healing. (Seeding lastNonce from the
//     committed on-chain nonce on first use would remove both limits; deferred.)
func MonotonicNonce(sk ed25519.PrivateKey) uint64 {
	pub, ok := sk.Public().(ed25519.PublicKey)
	if !ok {
		// Unreachable for an ed25519 private key; fail safe to a positive value.
		return uint64(time.Now().UnixNano()) // #nosec G115 -- UnixNano is positive
	}
	key := string(pub)

	nonceMu.Lock()
	defer nonceMu.Unlock()

	n := uint64(time.Now().UnixNano()) // #nosec G115 -- UnixNano is positive
	if n <= lastNonce[key] {
		n = lastNonce[key] + 1
	}
	lastNonce[key] = n
	return n
}
