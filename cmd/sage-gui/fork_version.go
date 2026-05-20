package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ConsensusForkVersion is the chain's consensus fork tag. Bumped ONLY when
// a SAGE release introduces a consensus-breaking change that makes existing
// chain state (BadgerDB on-chain registry, CometBFT blocks) invalid under
// the new binary — e.g. tx encoding/decoding shape change, BadgerDB key
// prefix or value encoding change, ABCI semantics change, validator/quorum
// rule change, genesis incompatibility.
//
// INDEPENDENT of release semver. Patch and minor releases that don't break
// consensus do not bump it, so operators keep chain state across upgrades:
// domain registry, access grants, org memberships, validator set, agent
// identities. This is the gate that distinguishes "drag-and-drop chain
// reset is acceptable" (single-user sovereign mode) from "chain state IS
// the deployment substrate" (multi-agent / org-bootstrap / federation).
//
// History:
//
//	1 — Gate introduced in v7.5.5. All prior v7.5.x deployments are treated
//	    as fork=1 on first boot under this gate so the upgrade that adds the
//	    gate itself does not produce a spurious reset.
//
// Declared as a var (not const) so tests can stage fork transitions without
// rebuilding. Mirrors the existing `version` symbol's pattern.
var ConsensusForkVersion = 1

const forkVersionFile = "fork-version.txt"

// readForkVersion returns the consensus fork tag stamped on disk, or 0 when
// the file is absent or unparseable. 0 signals "this install predates the
// gate — adopt the current binary's fork without resetting state".
func readForkVersion(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

// stampForkVersion writes the given fork tag to path. Callers must persist
// this AFTER any reset has completed — a crash mid-migration must leave the
// next boot still seeing the old fork so the reset gets re-attempted.
func stampForkVersion(path string, fork int) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", fork)), 0600)
}

// isLegacyForkOneVersion reports whether lastVersion was produced by a
// pre-gate binary whose chain state is already fork=1 compatible. The gate
// shipped in v7.5.5; v7.5.0..v7.5.4 binaries produced the same encoding,
// so adopting fork=1 without a reset is safe for them. Anything older
// (v6.x, v7.0..v7.4) used a different fork lineage and MUST run the
// destructive reset before adopting fork=1, otherwise the new binary
// reads incompatible Badger/CometBFT state.
//
// Accepts both "v7.5.0" and bare "7.5.0" forms and tolerates post-tag
// suffixes ("v7.5.4-1-gabc"). Empty input returns false (caller handles
// fresh-install separately).
func isLegacyForkOneVersion(lastVersion string) bool {
	v := strings.TrimPrefix(lastVersion, "v")
	return strings.HasPrefix(v, "7.5.")
}
