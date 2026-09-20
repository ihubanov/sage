package tx

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file is the exit for the ONE shape the fence cannot prove, and it exists
// because refusing to have one was itself a failure mode.
//
// The fence's rule is "never lift without proof", and every other way out of a
// fence reads a proof from consensus: the exact transaction in a committed
// block, a committed nonce that has reached or passed the fenced allocation, or
// a re-submission that consensus answers permanently. A fence restored from
// durable intent has no bytes to re-submit, and if its transaction never
// committed — the process was killed after writing the record, or the bytes
// never left the machine — none of those proofs can ever exist. The node then
// refuses to sign that key, refuses every coordinated restart (i.e. every
// update), and has no route back: operators hit exactly this and were told, by
// the veto message itself, to wait for a reconciliation that had nothing to do.
//
// WHAT THIS IS. An explicit, operator-authorized, RECORDED decision that the
// payload is lost. It is not a proof, it is not automatic, and it is not
// available for a fence that still holds its bytes (that fence has a working
// reconciler; abandoning it would throw away a transaction that consensus
// could still be made to commit). The caller must acknowledge that it may
// discard a transaction, and the node must first read everything it can about
// whether that transaction could still come back:
//
//   - the node's LIVE P2P peer count is known, and when peers ARE connected the
//     operator must additionally acknowledge that a connected peer is a route
//     the transaction could still take back into this node's mempool;
//   - the transaction is not in this node's mempool;
//   - the recorded hash is not in a committed block, and the signer's
//     committed nonce has not reached the fenced allocation.
//
// Those are the facts a node CAN read. They do not add up to "the transaction
// is gone" — a peer that holds it from before the shutdown could still
// reconnect and re-deliver it, and a client that kept the signed bytes could
// still re-broadcast them. That residual is why this is an operator decision
// rather than a conclusion the node reaches on its own, and it is why the
// event records the evidence the decision was taken on. What it removes is the
// only alternative that was on offer: a node that could never write and never
// update again.
//
// THE PEER GATE IS AN ACKNOWLEDGEMENT, NOT A VERDICT. An earlier revision
// refused the abandon outright whenever a peer was connected, on the theory
// that a peer could always deliver the transaction back. That refusal was
// unconditional and permanent for any node that keeps a peer — which is every
// federated desktop node and every validator with a persistent peer — so a
// fence that no proof could settle became a node that could never write again,
// with the one documented exit route refusing by construction. The peer count
// is evidence about a ROUTE, not about the transaction: the operator can see
// the topology (whether the peer was running when the submission went out,
// whether it ever admitted the bytes) and the node cannot. So the evidence is
// still read and recorded, the count is still reported, and the decision now
// requires the caller to acknowledge the peer route explicitly instead of
// pretending the node can rule it out.

// FenceAbandonEvidence is what the NODE could read about a restored fence when
// an operator asks to abandon it. Every field is read from the node's own store
// (the committed nonce floor) or its own RPC (peers, mempool, transaction
// index); none of it is asserted by the caller, and the whole struct is
// recorded with the decision.
type FenceAbandonEvidence struct {
	// PeersChecked is false when the peer list could not be read. The abandon is
	// refused in that case: "we could not look" is not "nothing is connected".
	PeersChecked bool `json:"peers_checked"`
	// Peers is the node's live P2P peer count. Anything above zero refuses the
	// AUTOMATIC abandon, because a connected peer can still deliver the
	// transaction; the operator route accepts it against an explicit
	// acknowledgement that this route exists.
	Peers int `json:"peers"`
	// PeerRedeliveryAcknowledged is the operator's explicit acceptance that a
	// connected peer is a route the fenced transaction could still take back
	// into this node's mempool, and that a late arrival would lose the payload
	// instead of the other way round. It is required when Peers > 0 and it is
	// recorded with the decision. It is an ACKNOWLEDGEMENT, not a fact read
	// from the node: a caller that wants the safe answer does not set it.
	PeerRedeliveryAcknowledged bool `json:"peer_redelivery_acknowledged"`
	// MempoolChecked / MempoolCount / MempoolTotal / MempoolHolds describe this
	// node's own mempool. MempoolHolds is true when the fenced transaction
	// itself was found there, which refuses the abandon outright: it is alive,
	// and signing past it would be the nonce inversion the fence exists to
	// prevent.
	MempoolChecked bool `json:"mempool_checked"`
	MempoolCount   int  `json:"mempool_count"`
	MempoolTotal   int  `json:"mempool_total"`
	MempoolHolds   bool `json:"mempool_holds_transaction"`
	// NodeCaughtUp is CometBFT's own `catching_up == false`. A node that is
	// still replaying or state-syncing may not have indexed a transaction that
	// DID commit, so absence of proof is not yet meaningful; the automatic
	// resolution (see AutoResolveUnprovableFence) refuses until this is true.
	NodeCaughtUp bool `json:"node_caught_up"`
	// PeersEverSeen is true when any peer has been observed connected since this
	// PROCESS started. It is the conservative, process-wide latch; see
	// PeersObservedSinceFence for the per-fence answer the automatic route
	// actually uses.
	PeersEverSeen bool `json:"peers_ever_seen"`
	// PeersObservedSinceFence is true when a peer was observed connected at any
	// point AFTER this fence was raised. THIS is the latch that keeps the
	// automatic resolution honest: a transaction is delivered back by a peer,
	// so a fence that has coexisted with a connected peer is not one where
	// "nothing can deliver it back" is true, even if that peer has since
	// disconnected.
	//
	// WHY NOT THE PROCESS-WIDE LATCH. Anchoring on process start meant a
	// single peer sighting at any time — a peer that connected while the node
	// was starting, a peer that has been gone for hours — switched the
	// automatic route off for every fence for the rest of the run, including a
	// fence raised long afterwards. The node then had a stuck key, an automatic
	// route that refused by construction, and an operator route that also
	// refused whenever any peer was connected: the exact "no way back" state
	// the automatic route was added to end.
	PeersObservedSinceFence bool `json:"peers_observed_since_fence"`
	// TxLookup is "committed", "rejected", "not_found", "unavailable" or
	// "unknown_hash". Only the first two refuse the abandon (they ARE proofs,
	// and the lift route is the right one). "unavailable" is the bucket for
	// every answer that is not a verdict — a miss, an index-less node and a
	// broken RPC all land here, deliberately, because telling them apart would
	// mean deciding something from RPC text (see cometIndexedOutcome). It is
	// reported rather than hidden so the operator sees which half of the
	// evidence is actually evidence.
	TxLookup       string `json:"tx_lookup"`
	TxLookupDetail string `json:"tx_lookup_detail,omitempty"`
	// CommittedNonce is the signer's committed nonce floor, when the store
	// could report one. A floor at or above the fenced allocation is a proof
	// and refuses the abandon.
	HasCommittedNonce bool   `json:"has_committed_nonce"`
	CommittedNonce    uint64 `json:"committed_nonce,omitempty"`
}

// FenceAbandonRefusedError is returned when the fence, the evidence or the
// request shape does not permit an abandon. It is a distinct type so the
// handler can answer 409 with the reason instead of a generic error.
type FenceAbandonRefusedError struct {
	Signer string
	Reason string
}

func (e *FenceAbandonRefusedError) Error() string {
	return fmt.Sprintf("fence for signer %s cannot be abandoned: %s", e.Signer, e.Reason)
}

// ReadFenceAbandonEvidence reads every fact the node can contribute to an
// abandon decision. It is deliberately all-or-nothing on the two facts that
// gate the decision (peers and mempool): a caller that gets an error has no
// evidence to decide on, and must not treat the zero value as "nothing found".
func ReadFenceAbandonEvidence(
	ctx context.Context,
	cometRPC string,
	nonceFloor func(ed25519.PublicKey) (uint64, bool),
	fence FencedSigner,
) (FenceAbandonEvidence, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cometRPC), "/")
	if endpoint == "" {
		return FenceAbandonEvidence{}, errors.New(
			"the node's CometBFT RPC endpoint is not known, so its peers and mempool cannot be read")
	}
	ev := FenceAbandonEvidence{TxLookup: "unknown_hash"}

	if raw, err := hex.DecodeString(strings.TrimSpace(fence.SignerPubKeyHex)); err == nil &&
		len(raw) == ed25519.PublicKeySize && nonceFloor != nil {
		if committed, ok := nonceFloor(ed25519.PublicKey(raw)); ok {
			ev.HasCommittedNonce = true
			ev.CommittedNonce = committed
		}
	}

	var netInfo cometNetInfo
	resultOK, err := cometGetJSON(ctx, "comet net_info", endpoint+"/net_info", nil, &netInfo)
	if err != nil {
		return ev, err
	}
	if !resultOK || netInfo.Result == nil {
		return ev, errors.New("comet net_info did not answer with a result")
	}
	ev.PeersChecked = true
	ev.Peers = netInfo.Result.NPeers
	if ev.Peers > 0 {
		peersEverSeen.Store(true)
		observeFencePeer(fence.SignerPubKeyHex, fence.Since)
	}
	ev.PeersEverSeen = peersEverSeen.Load()
	ev.PeersObservedSinceFence = peersObservedSinceFence(fence.SignerPubKeyHex, fence.Since)

	var syncStatus cometSyncStatus
	resultOK, err = cometGetJSON(ctx, "comet status", endpoint+"/status", nil, &syncStatus)
	if err != nil {
		return ev, err
	}
	if !resultOK || syncStatus.Result == nil {
		return ev, errors.New("comet status did not answer with a result")
	}
	ev.NodeCaughtUp = !syncStatus.Result.SyncInfo.CatchingUp

	var mempool cometUnconfirmedTxs
	resultOK, err = cometGetJSON(ctx, "comet unconfirmed_txs",
		fmt.Sprintf("%s/unconfirmed_txs?limit=%d", endpoint, fenceAbandonMempoolLimit), nil, &mempool)
	if err != nil {
		return ev, err
	}
	if !resultOK || mempool.Result == nil {
		return ev, errors.New("comet unconfirmed_txs did not answer with a result")
	}
	ev.MempoolChecked = true
	ev.MempoolCount = mempool.Result.Count
	ev.MempoolTotal = mempool.Result.Total
	wantHash := strings.ToUpper(strings.TrimSpace(fence.TxHash))
	for _, raw := range mempool.Result.Txs {
		decoded, decodeErr := base64.StdEncoding.DecodeString(string(raw))
		if decodeErr != nil {
			continue
		}
		hash := CometTxHash(decoded)
		if strings.ToUpper(hex.EncodeToString(hash[:])) == wantHash {
			ev.MempoolHolds = true
			break
		}
	}

	hashText := strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(fence.TxHash), "0x"))
	if decoded, decodeErr := hex.DecodeString(hashText); decodeErr == nil && len(decoded) == 32 {
		var hash [32]byte
		copy(hash[:], decoded)
		outcome, lookupErr := cometIndexedOutcome(ctx, endpoint, nil, hash)
		switch {
		case lookupErr != nil:
			ev.TxLookup = "unavailable"
			ev.TxLookupDetail = scrubFenceText(lookupErr.Error(), nil)
		case outcome.Verdict == TxVerdictCommitted:
			ev.TxLookup = "committed"
			ev.TxLookupDetail = outcome.Detail
		case outcome.Verdict == TxVerdictRejected:
			ev.TxLookup = "rejected"
			ev.TxLookupDetail = outcome.Detail
		default:
			ev.TxLookup = "not_found"
			ev.TxLookupDetail = outcome.Detail
		}
	}
	return ev, nil
}

// fenceAbandonMempoolLimit bounds the mempool read. A mempool holding more
// transactions than were inspected cannot certify absence, and refusing in that
// case is the safe direction: the operator can retry when the node is quieter.
const fenceAbandonMempoolLimit = 100

// peersEverSeen is the process-lifetime latch described on
// FenceAbandonEvidence.PeersEverSeen. Monotonic on purpose: once a peer has been
// seen, it stays seen for the rest of this run.
var peersEverSeen atomic.Bool

// fencePeerSeenSince records, per signer, whether a connected peer has been
// OBSERVED at any point after that signer's current fence was raised, and the
// times that bound the observation. The automatic resolution reads it as a
// per-fence latch; see FenceAbandonEvidence.PeersObservedSinceFence for why the
// process-wide latch was the wrong anchor.
//
// It is deliberately NOT cleared by a lift: the entry is keyed by fence start
// time, so a fence raised afterwards starts from "no peer seen since" with no
// bookkeeping — a stale entry cannot leak into it, and one that is never
// consulted again is harmless. Only the most recent observation per signer is
// kept, because only the newest fence's window can be current.
type fencePeerObservation struct {
	FenceSince time.Time
	// ObservedAt is when the sighting was recorded. Nothing reads it — the
	// decision turns on whether a sighting belongs to THIS fence, which
	// FenceSince answers by itself — but a diagnostics surface or a future
	// "how long into the hold did the peer appear" question needs the timestamp
	// next to the anchor it was taken against, and reconstructing it later is
	// impossible.
	ObservedAt time.Time
}

var fencePeerObservations struct {
	mu sync.Mutex
	by map[string]fencePeerObservation
}

// observeFencePeer records that a connected peer was seen while the fence
// anchored at fenceSince was held. The comparison is monotonic in the
// observation, not in the wall clock: ObservedAt is taken here, and the entry
// only replaces one anchored at an older (or equal) fence start, so an older
// fence's sighting can never be attributed to a newer fence.
func observeFencePeer(signerHex string, fenceSince time.Time) {
	signerHex = strings.ToLower(strings.TrimSpace(signerHex))
	if signerHex == "" || fenceSince.IsZero() {
		return
	}
	fencePeerObservations.mu.Lock()
	defer fencePeerObservations.mu.Unlock()
	if fencePeerObservations.by == nil {
		fencePeerObservations.by = make(map[string]fencePeerObservation)
	}
	prior, ok := fencePeerObservations.by[signerHex]
	if ok && prior.FenceSince.After(fenceSince) {
		return
	}
	fencePeerObservations.by[signerHex] = fencePeerObservation{
		FenceSince: fenceSince,
		ObservedAt: time.Now(),
	}
}

// peersObservedSinceFence reports whether a connected peer has been observed
// while THIS fence was held. An entry that belongs to an older fence (its
// recorded start is before this one) does not answer for this fence: the
// transaction under this fence did not exist when that peer was seen.
func peersObservedSinceFence(signerHex string, fenceSince time.Time) bool {
	signerHex = strings.ToLower(strings.TrimSpace(signerHex))
	if signerHex == "" || fenceSince.IsZero() {
		return false
	}
	fencePeerObservations.mu.Lock()
	defer fencePeerObservations.mu.Unlock()
	observation, ok := fencePeerObservations.by[signerHex]
	if !ok {
		return false
	}
	return !observation.FenceSince.Before(fenceSince)
}

// AutoResolveUnprovableFence resolves a restored fence that NOTHING can deliver
// back, without waiting for an operator.
//
// WHY THE NODE IS ALLOWED TO DECIDE THIS ONE. Every other lift reads a proof
// from consensus; this one concludes that no proof can ever exist, and the
// desktop product has no operator in the loop to make that call by hand — a
// user whose node came back fenced has a node that cannot write and an updater
// that refuses to restart, and telling them to run a curl is not a recovery.
// The conclusion is drawn from evidence the node reads itself:
//
//   - the fence is restored from durable intent, so its signed bytes are gone
//     and no re-submission is possible;
//   - CometBFT reports catching_up == false, so "no committed fate" means the
//     question was asked of a chain that has caught up;
//   - no peer is connected now, and none has been seen since THIS fence was
//     raised, so nothing can deliver the transaction back into this node's
//     mempool (a node that is currently P2P-connected fails this gate and keeps
//     its fence, which is the correct answer there — and the operator route is
//     the one that can be taken deliberately, against an explicit
//     acknowledgement of the peer route);
//   - the transaction is not in this node's mempool;
//   - the recorded hash is in no committed block, and the signer's committed
//     nonce has not reached the fenced allocation.
//
// The residual is the same one the operator route states: a client that kept
// the signed bytes could still re-broadcast them, and they would commit if they
// landed before the signer's next transaction. The abandoned allocation is
// reserved so the next allocation is above it, and the decision is recorded as
// `fence_abandoned` with mode=automatic and the full evidence — never as a
// proven fate.
func AutoResolveUnprovableFence(
	ctx context.Context,
	cometRPC string,
	nonceFloor func(ed25519.PublicKey) (uint64, bool),
	fence FencedSigner,
) (bool, string, error) {
	ev, err := ReadFenceAbandonEvidence(ctx, cometRPC, nonceFloor, fence)
	if err != nil {
		return false, "", err
	}
	// EACH REFUSAL NAMES THE FACT THAT STOPPED IT. Returning an empty reason
	// here was its own diagnostic hole: the caller records this string with the
	// held fence, and without it the status row could only show the proof-read
	// error — which reads identically for "the chain has no proof yet" and "the
	// node has evidence that the transaction could still come back". A node
	// whose self-heal was refusing on evidence then looked broken.
	if !ev.NodeCaughtUp {
		return false, "the node is still catching up, so a transaction that DID commit may not be indexed " +
			"yet and absence of a committed fate is not yet an answer", nil
	}
	if ev.Peers > 0 {
		return false, fmt.Sprintf("%d peer(s) are connected, so a peer can still deliver this transaction "+
			"back into the mempool; the operator route (POST /v1/dashboard/signer-fence/abandon) is the one "+
			"that can be taken against an explicit acknowledgement of this route", ev.Peers), nil
	}
	if ev.PeersObservedSinceFence {
		return false, "a peer was connected while this fence was held, so these bytes could still be " +
			"delivered back into the mempool; the operator route (POST /v1/dashboard/signer-fence/abandon) " +
			"is the one that can be taken against an explicit acknowledgement of this route", nil
	}
	raw, decodeErr := hex.DecodeString(strings.TrimSpace(fence.SignerPubKeyHex))
	if decodeErr != nil || len(raw) != ed25519.PublicKeySize {
		return false, "", &FenceAbandonRefusedError{
			Signer: fence.SignerPubKeyPrefix,
			Reason: "the fence does not name a valid ed25519 signer",
		}
	}
	key := string(raw)

	fenceMu.Lock()
	live := fences[key]
	var held FencedSigner
	var cause fenceCause
	if live != nil {
		held = live.snapshotLocked(key, time.Now())
		cause = live.cause
	}
	fenceMu.Unlock()
	if live == nil {
		return false, "", nil
	}
	if err := validateAbandonEvidence(held, cause, ev); err != nil {
		return false, "", err
	}

	detail := fmt.Sprintf("resolved at startup without proof: the node is caught up, has never had a peer "+
		"this run, holds no mempool copy, sees no committed fate for the recorded hash and an unspent "+
		"allocation, and is the only process that ever held the signed bytes. Evidence: tx_lookup=%s, "+
		"committed_nonce=%s, peers=%d, mempool=%d/%d. If a copy of this transaction still exists it can "+
		"still commit before the signer's next commit, and is refused as a replay after it.",
		ev.TxLookup, committedNonceText(ev), ev.Peers, ev.MempoolCount, ev.MempoolTotal)
	reserveNonceFloor(key, held.Nonce)
	emitFenceEvent("fence_abandoned",
		fenceKV("signer", held.SignerPubKeyPrefix),
		fenceKV("tx_hash", held.TxHash),
		fenceNonceField(held.Nonce, held.HasNonce),
		fenceAge("held_for", held.HeldFor),
		fenceKV("mode", "automatic_unprovable"),
		fenceKV("tx_lookup", ev.TxLookup),
		fenceNum("peers", uint64(ev.Peers)),                // #nosec G115 -- non-negative count
		fenceNum("mempool_count", uint64(ev.MempoolCount)), // #nosec G115 -- non-negative count
		fenceKV("note", "NODE DECISION, NOT A PROOF: this fence held a key that could not sign, on a node "+
			"that could not prove the transaction's fate and could not be delivered it back; the node "+
			"reserves the abandoned allocation and records the decision instead of refusing every write "+
			"and every update forever"))

	fenceMu.Lock()
	if live.pending > 0 {
		live.pending = 1
	}
	fenceMu.Unlock()
	liftFence(key, live, TxVerdictAbandoned, detail)
	return true, detail, nil
}

// AbandonUnprovableFence resolves a restored, provably-unprovable fence on an
// explicit operator decision, and is the only lift in this package that is not
// a proven fate. Every precondition is read HERE, from the live fence and the
// evidence the caller collected, so a handler cannot widen it by mistake.
//
// The peer count is the one precondition that is partly the caller's: when the
// node reports peers, the operator must have acknowledged that a peer is still
// a route the transaction could take back. The count itself is re-checked
// against the evidence the caller read, so a request cannot claim "no peers"
// and then be handed a decision the node never agreed to.
//
// On success the abandoned allocation is reserved: the next allocation for that
// signer is strictly ABOVE the fenced nonce rather than resuming at the
// committed floor, so the abandoned bytes cannot be duplicated by a same-nonce
// twin. Whether the abandoned transaction ever commits afterwards is then a
// race between it and the next transaction — which is precisely what the
// operator accepted, and what the verdict label "abandoned" records.
func AbandonUnprovableFence(ctx context.Context, signerPubKeyHex, reason string, ev FenceAbandonEvidence) error {
	raw, err := hex.DecodeString(strings.TrimSpace(signerPubKeyHex))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("fence abandon: %q is not an ed25519 signer", signerPubKeyHex)
	}
	key := string(raw)
	reason = strings.TrimSpace(reason)

	fenceMu.Lock()
	fence := fences[key]
	var held FencedSigner
	var cause fenceCause
	if fence != nil {
		held = fence.snapshotLocked(key, time.Now())
		cause = fence.cause
	}
	fenceMu.Unlock()
	if fence == nil {
		return &FenceAbandonRefusedError{
			Signer: signerPrefix(key),
			Reason: "no signer fence is held for that signer",
		}
	}
	if reason == "" {
		return &FenceAbandonRefusedError{
			Signer: held.SignerPubKeyPrefix,
			Reason: "an operator reason is required: this decision accepts that a transaction's payload " +
				"may be lost, and the record must say why it was taken",
		}
	}
	if err := validateAbandonEvidence(held, cause, ev); err != nil {
		return err
	}

	// Reserve the abandoned allocation BEFORE the fence is lifted, so a caller
	// unblocked by the lift cannot allocate at the abandoned nonce.
	reserveNonceFloor(key, held.Nonce)

	detail := fmt.Sprintf("operator abandon without proof: reason %q; evidence: tx_lookup=%s, committed_nonce=%s, "+
		"peers=%d, mempool=%d/%d holding_this_tx=%t. The payload may be lost — if any copy of it still exists "+
		"somewhere, it will be refused as a replay once a higher nonce commits.",
		reason, ev.TxLookup, committedNonceText(ev), ev.Peers, ev.MempoolCount, ev.MempoolTotal, ev.MempoolHolds)
	// The recorded note must not claim a fact the node did not observe. With
	// peers connected the operator accepted the peer route explicitly, so the
	// note says THAT rather than repeating the no-peer wording, which would
	// misdescribe an incident review later.
	peerNote := "the node could read no committed fate for this transaction and held no mempool copy of it, " +
		"so the operator accepted that its payload may be lost"
	if ev.Peers > 0 {
		peerNote = fmt.Sprintf("the node could read no committed fate for this transaction and held no mempool "+
			"copy of it, but %d peer(s) were connected — the operator acknowledged that a peer is still a route "+
			"these bytes could take back into this node, and accepted that a late arrival loses the abandoned "+
			"payload", ev.Peers)
	}
	emitFenceEvent("fence_abandoned",
		fenceKV("signer", held.SignerPubKeyPrefix),
		fenceKV("tx_hash", held.TxHash),
		fenceNonceField(held.Nonce, held.HasNonce),
		fenceAge("held_for", held.HeldFor),
		fenceNum("peers", uint64(ev.Peers)), // #nosec G115 -- peer counts are non-negative
		fenceKV("tx_lookup", ev.TxLookup),
		fenceKV("tx_lookup_detail", ev.TxLookupDetail),
		fenceKV("committed_nonce", committedNonceText(ev)),
		fenceNum("mempool_count", uint64(ev.MempoolCount)), // #nosec G115 -- counts are non-negative
		fenceKV("peer_redelivery_acknowledged", fmt.Sprintf("%t", ev.Peers > 0 && ev.PeerRedeliveryAcknowledged)),
		fenceKV("reason", reason),
		fenceKV("note", "OPERATOR DECISION, NOT A PROOF: "+peerNote+", rather than leaving the key refusing "+
			"to sign and the node refusing to restart forever"))

	// A restored fence has no reconciler to retire its pending count; setting it
	// to 1 (as the operator lift does) makes the decrement inside liftFence land
	// on zero so the key actually opens.
	fenceMu.Lock()
	if fence.pending > 0 {
		fence.pending = 1
	}
	fenceMu.Unlock()
	liftFence(key, fence, TxVerdictAbandoned, detail)
	return nil
}

// validateAbandonEvidence is the ONE gate both exits share: the operator route
// and the automatic startup resolution. Keeping it in one place is what makes
// "the node may resolve this itself, under exactly the conditions an operator
// would have to accept" a checkable claim rather than a coincidence.
func validateAbandonEvidence(held FencedSigner, cause fenceCause, ev FenceAbandonEvidence) error {
	refuse := func(format string, args ...any) error {
		return &FenceAbandonRefusedError{Signer: held.SignerPubKeyPrefix, Reason: fmt.Sprintf(format, args...)}
	}
	// ONLY a fence restored from durable intent. A live fence still has the
	// exact bytes that went out, so reconciliation keeps re-submitting them and
	// can still commit the transaction; abandoning that would discard a
	// transaction consensus has not refused.
	if cause != fenceCauseRestored {
		return refuse("this fence still holds the signed bytes it sent, so reconciliation is re-submitting " +
			"them and consensus can still settle its fate; this route exists only for a fence whose " +
			"bytes did not survive the process that sent them")
	}
	if !held.HasNonce {
		return refuse("the durable record carries no nonce, so there is no allocation to reserve and no way " +
			"to keep a later transaction from colliding with it")
	}
	if !ev.PeersChecked {
		return refuse("the node's live peer count was not read, and a connected peer can still deliver this " +
			"transaction back into the mempool")
	}
	// PEERS ARE AN ACKNOWLEDGEMENT, NOT A VETO. See the file header: refusing
	// outright meant a node that keeps a peer (federated desktop node, validator
	// with a persistent peer) could never take the one route out of an
	// unprovable fence, which is the outage this route exists to end. The peer
	// count is still read, still reported with the decision, and still refuses
	// the AUTOMATIC route outright; the operator route requires an explicit
	// acknowledgement when peers are connected.
	if ev.Peers > 0 && !ev.PeerRedeliveryAcknowledged {
		return refuse("%d peer(s) are connected, so this transaction can still be delivered back to this "+
			"node and commit. Set peer_redelivery_acknowledged to true to abandon it anyway and accept that "+
			"a late arrival loses the abandoned payload; the decision and the peer count are recorded with "+
			"the fence", ev.Peers)
	}
	if !ev.MempoolChecked {
		return refuse("the node's mempool was not read")
	}
	if ev.MempoolHolds {
		return refuse("the transaction is IN this node's mempool right now: it is alive, and signing past " +
			"its nonce is the exact inversion the fence prevents")
	}
	if ev.MempoolTotal > ev.MempoolCount {
		return refuse("the mempool holds %d transactions and only %d were inspected, so absence cannot be "+
			"certified; retry when the node is quieter", ev.MempoolTotal, ev.MempoolCount)
	}
	if ev.HasCommittedNonce && ev.CommittedNonce >= held.Nonce {
		return refuse("the signer's committed nonce has reached the fenced allocation, which is a PROOF " +
			"that the allocation is spent; use the lift route, which records that proof")
	}
	switch ev.TxLookup {
	case "committed", "rejected":
		return refuse("the recorded transaction hash IS in a committed block, which is a proof of its fate; " +
			"use the lift route, which records that proof")
	}
	return nil
}

func committedNonceText(ev FenceAbandonEvidence) string {
	if !ev.HasCommittedNonce {
		return "unknown"
	}
	return fmt.Sprintf("%d", ev.CommittedNonce)
}

// reserveNonceFloor keeps an abandoned allocation out of reach of the next
// allocation for that signer.
//
// The allocator returns max(wall clock, last+1) and seeds from the committed
// floor, which by definition sits BELOW the abandoned nonce. Without this
// reservation a discarded nonce could be handed out again to a different
// payload — two transactions sharing one nonce, one of which the fence just
// declared dead. It also marks the key seeded so a later floor read cannot
// lower what the next allocation must exceed.
func reserveNonceFloor(key string, nonce uint64) {
	if nonce == 0 || key == "" {
		return
	}
	nonceMu.Lock()
	seeded[key] = true
	if nonce > lastNonce[key] {
		lastNonce[key] = nonce
	}
	nonceMu.Unlock()
}

// cometNetInfo and cometUnconfirmedTxs are the two RPC envelopes the evidence
// reader needs. Only the fields the decision uses are decoded, and the response
// body is bound by cometGetJSON's cap and single-document rules.
type cometNetInfo struct {
	Result *struct {
		NPeers int `json:"n_peers"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

type cometUnconfirmedTxs struct {
	Result *struct {
		Count int      `json:"n_txs"`
		Total int      `json:"total"`
		Txs   []string `json:"txs"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

// cometSyncStatus is the slice of CometBFT's /status the automatic resolution
// reads: whether the node has finished catching up. A node still replaying or
// state-syncing has not necessarily indexed a transaction that DID commit, so
// "no committed fate" is not yet a meaningful answer there.
type cometSyncStatus struct {
	Result *struct {
		SyncInfo struct {
			CatchingUp bool `json:"catching_up"`
		} `json:"sync_info"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}
