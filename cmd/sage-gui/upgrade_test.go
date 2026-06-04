package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sageabci "github.com/l33tdawg/sage/internal/abci"
	"github.com/l33tdawg/sage/internal/tx"
)

// TestValidateUpgradeTarget exercises the operator-submit guard in isolation —
// the heart of the issue #32 fix: sequential-only targets and canonical names.
func TestValidateUpgradeTarget(t *testing.T) {
	const maxV = 10

	tests := []struct {
		name     string
		current  uint64
		target   uint64
		planName string
		wantName string
		wantErr  string // substring; "" means no error
	}{
		{
			name:     "sequential next, derived name",
			current:  6,
			target:   7,
			wantName: "app-v7",
		},
		{
			name:     "sequential next with matching canonical name",
			current:  6,
			target:   7,
			planName: "app-v7",
			wantName: "app-v7",
		},
		{
			name:     "top supported fork sequential",
			current:  9,
			target:   10,
			wantName: "app-v10",
		},
		{
			name:    "missing target",
			current: 6,
			target:  0,
			wantErr: "--target is required",
		},
		{
			name:    "exceeds max supported",
			current: 9,
			target:  11,
			wantErr: "exceeds the max app version",
		},
		{
			name:    "no-op (target == current)",
			current: 6,
			target:  6,
			wantErr: "would regress or no-op",
		},
		{
			name:    "regression (target < current)",
			current: 7,
			target:  6,
			wantErr: "would regress or no-op",
		},
		{
			name:    "skip-ahead strands single fork",
			current: 8,
			target:  10,
			wantErr: "permanently strand app-v9",
		},
		{
			name:    "skip-ahead strands a range",
			current: 6,
			target:  10,
			wantErr: "permanently strand app-v7…app-v9",
		},
		{
			name:     "non-canonical name rejected",
			current:  6,
			target:   7,
			planName: "v9.2.2",
			wantErr:  "not the canonical activation key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateUpgradeTarget(tc.current, tc.target, maxV, tc.planName)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (name=%q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantName {
				t.Fatalf("canonical name = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// TestValidateUpgradeTarget_SkipAheadMessageActionable verifies the skip-ahead
// error tells the operator the correct next step (current+1), not the rejected
// jump — the whole point of the guard is to steer them onto the safe path.
func TestValidateUpgradeTarget_SkipAheadMessageActionable(t *testing.T) {
	_, err := validateUpgradeTarget(6, 10, 10, "")
	if err == nil {
		t.Fatal("expected skip-ahead error")
	}
	if !strings.Contains(err.Error(), "--target 7 next") {
		t.Fatalf("skip-ahead error should steer to --target 7; got: %v", err)
	}
}

// TestParseProposeSigningKey covers the issue #34 --agent-key parser: it must
// accept the three key forms an operator has on a node host (a raw 32-byte
// agent.key seed, a 64-byte expanded ed25519 key, and a CometBFT
// priv_validator_key.json) and resolve each to the SAME ed25519 identity, while
// rejecting malformed input with a clear error rather than a wrong key.
func TestParseProposeSigningKey(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("rand: %v", err)
	}
	want := ed25519.NewKeyFromSeed(seed)
	wantPub := want.Public().(ed25519.PublicKey)

	// priv_validator_key.json carries the 64-byte ed25519 private key base64'd
	// under priv_key.value — the exact bytes of Go's ed25519.PrivateKey.
	pvJSON, err := json.Marshal(map[string]any{
		"address": "DEADBEEF",
		"pub_key": map[string]any{
			"type":  "tendermint/PubKeyEd25519",
			"value": base64.StdEncoding.EncodeToString(wantPub),
		},
		"priv_key": map[string]any{
			"type":  "tendermint/PrivKeyEd25519",
			"value": base64.StdEncoding.EncodeToString(want),
		},
	})
	if err != nil {
		t.Fatalf("marshal pv json: %v", err)
	}

	good := []struct {
		name string
		data []byte
	}{
		{"raw 32-byte seed", seed},
		{"raw 64-byte expanded key", []byte(want)},
		{"priv_validator_key.json", pvJSON},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProposeSigningKey(tc.data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotPub, ok := got.Public().(ed25519.PublicKey)
			if !ok || !gotPub.Equal(wantPub) {
				t.Fatalf("resolved a different identity than expected")
			}
			// Prove the parsed key is actually usable for signing.
			sig := ed25519.Sign(got, []byte("upgrade"))
			if !ed25519.Verify(wantPub, []byte("upgrade"), sig) {
				t.Fatalf("parsed key did not produce a verifiable signature")
			}
		})
	}

	bad := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"empty", []byte{}, "unrecognized key file"},
		{"wrong-length raw", make([]byte, 48), "unrecognized key file"},
		{"json without priv_key", []byte(`{"pub_key":{"value":"x"}}`), "no priv_key.value"},
		{"priv_key not base64", []byte(`{"priv_key":{"value":"!!!not base64!!!"}}`), "base64"},
		{
			name:    "priv_key wrong byte length",
			data:    []byte(`{"priv_key":{"value":"` + base64.StdEncoding.EncodeToString(make([]byte, 10)) + `"}}`),
			wantErr: "want a 32-byte seed or 64-byte ed25519 key",
		},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProposeSigningKey(tc.data)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestUpgradePropose_AgentKey drives --agent-key end-to-end through
// runUpgradePropose — the half of issue #34 the pure parser test doesn't cover.
// It proves two things the production path must guarantee: (1) the supplied key
// (not the default agent.key) is what actually SIGNS the broadcast tx — the
// identity the post-app-v8 admin gate keys on — and (2) when that key is rejected
// with code 47, the error steers the operator without circular "re-pass
// --agent-key" advice. A stubbed CometBFT (abci_info=app-v8, broadcast_tx_commit
// returning the FinalizeBlock code-47) lets the whole command run hermetically.
func TestUpgradePropose_AgentKey(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("rand: %v", err)
	}
	wantPub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	keyFile := filepath.Join(t.TempDir(), "admin.key")
	if err := os.WriteFile(keyFile, seed, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	// Capture the public key the broadcast tx was actually signed with.
	signedPub := make(chan ed25519.PublicKey, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/abci_info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"response": map[string]any{"app_version": "8"}},
		})
	})
	mux.HandleFunc("/broadcast_tx_commit", func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.URL.Query().Get("tx"), "0x")
		if decoded, decErr := hex.DecodeString(raw); decErr == nil {
			if ptx, txErr := tx.DecodeTx(decoded); txErr == nil {
				signedPub <- ed25519.PublicKey(ptx.PublicKey)
			}
		}
		// CheckTx admits it; the admin-gate rejection is a FinalizeBlock result.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"hash":      "DEAD",
				"height":    "10",
				"check_tx":  map[string]any{"code": 0},
				"tx_result": map[string]any{"code": 47, "log": "upgrade propose: under app-v8 only admin agents may propose upgrades"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := runUpgradePropose([]string{"--target", "9", "--yes", "--rpc", srv.URL, "--agent-key", keyFile})
	if err == nil {
		t.Fatal("expected a code-47 rejection error, got nil")
	}

	// (1) The supplied key signed the tx — not the default agent.key.
	select {
	case got := <-signedPub:
		if !got.Equal(wantPub) {
			t.Fatalf("tx signed with the wrong identity: the --agent-key was not used")
		}
	default:
		t.Fatal("broadcast handler never received a decodable tx")
	}

	// (2) The error is the --agent-key-supplied branch (no circular re-pass advice)
	// and names the real requirement.
	msg := err.Error()
	if !strings.Contains(msg, "The supplied --agent-key") {
		t.Errorf("error should use the --agent-key-supplied branch; got: %v", err)
	}
	if !strings.Contains(msg, "Role==admin") {
		t.Errorf("error should explain the chain-admin requirement; got: %v", err)
	}
}

// TestUpgradeStatus_AdminCaveatPastAppV8 is the issue #34 guard for the
// self-explanatory status output: at app-v8+ the printed next-step must steer the
// operator to --agent-key and explain the chain-admin requirement (otherwise it
// hands them a command that can't run), while below app-v8 it must NOT — there the
// default agent.key works on the legacy self-activating path.
func TestUpgradeStatus_AdminCaveatPastAppV8(t *testing.T) {
	tests := []struct {
		name          string
		appVersion    string
		wantAgentKey  bool
		wantAdminWord bool
	}{
		{"pre app-v8 (v6) — no caveat", "6", false, false},
		{"pre app-v8 (v7) — no caveat", "7", false, false},
		{"at app-v8 — caveat", "8", true, true},
		{"at app-v9 — caveat", "9", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/abci_info", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"result": map[string]any{
						"response": map[string]any{"app_version": tc.appVersion},
					},
				})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			out := captureStdout(t, func() {
				if err := runUpgradeStatus([]string{"--rpc", srv.URL}); err != nil {
					t.Fatalf("runUpgradeStatus: %v", err)
				}
			})

			gotAgentKey := strings.Contains(out, "--agent-key")
			gotAdmin := strings.Contains(out, "chain-admin")
			if gotAgentKey != tc.wantAgentKey {
				t.Errorf("--agent-key in next-step = %v, want %v\noutput:\n%s", gotAgentKey, tc.wantAgentKey, out)
			}
			if gotAdmin != tc.wantAdminWord {
				t.Errorf("chain-admin caveat = %v, want %v\noutput:\n%s", gotAdmin, tc.wantAdminWord, out)
			}
		})
	}
}

// TestBuildUpgradeProposeTx_Parameterized proves the builder now honors an
// arbitrary (validated) target — the capability the operator surface needs.
// Before the fix it was hardwired to upgradeTargetAppVersion (6).
func TestBuildUpgradeProposeTx_Parameterized(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	cfg := upgradeWatchdogConfig{
		BinaryVersion: "v9.2.2-test",
		AgentKey:      priv,
	}

	for _, target := range []uint64{7, 8, 9, 10} {
		ptx, err := buildUpgradeProposeTx(cfg, target)
		if err != nil {
			t.Fatalf("target %d: build: %v", target, err)
		}
		if ptx.Type != tx.TxTypeUpgradePropose {
			t.Fatalf("target %d: tx type = %v, want UpgradePropose", target, ptx.Type)
		}
		if ptx.UpgradePropose == nil {
			t.Fatalf("target %d: nil UpgradePropose payload", target)
		}
		if ptx.UpgradePropose.TargetAppVersion != target {
			t.Errorf("target %d: TargetAppVersion = %d", target, ptx.UpgradePropose.TargetAppVersion)
		}
		want := tx.CanonicalUpgradeName(target)
		if ptx.UpgradePropose.Name != want {
			t.Errorf("target %d: Name = %q, want canonical %q", target, ptx.UpgradePropose.Name, want)
		}
		// The plan name must never be the human binary version — that bug bumps
		// the app version while leaving every fork gate false.
		if ptx.UpgradePropose.Name == cfg.BinaryVersion {
			t.Errorf("target %d: plan named after binary version, not canonical key", target)
		}
	}
}

// TestValidateUpgradeTarget_RespectsBinaryCeiling guards the readiness ceiling
// against the actual exported max, so the test tracks the binary's real support
// window rather than a hardcoded 10.
func TestValidateUpgradeTarget_RespectsBinaryCeiling(t *testing.T) {
	maxV := sageabci.MaxSupportedAppVersion()
	// One past the ceiling, proposed sequentially from the top, must be refused.
	if _, err := validateUpgradeTarget(maxV, maxV+1, maxV, ""); err == nil {
		t.Fatalf("expected refusal for target %d > max %d", maxV+1, maxV)
	}
}

// TestBroadcastTxCommit_SurfacesBlockExecutionResult is the regression guard for
// the false-success bug: /broadcast_tx_commit admits a well-formed tx at CheckTx
// (Code 0) but the real UpgradePropose rejection (e.g. an already-pending plan,
// or a non-admin proposer) is a Code-47 result produced under FinalizeBlock. The
// fire-and-forget /broadcast_tx_sync the watchdog uses would hide that and the
// command would print a false ✓; commit must expose tx_result so it's reported
// as a failure.
func TestBroadcastTxCommit_SurfacesBlockExecutionResult(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/broadcast_tx_commit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"hash":      "ABC123",
				"height":    "4242",
				"check_tx":  map[string]any{"code": 0, "log": ""},
				"tx_result": map[string]any{"code": 47, "log": "upgrade plan already pending"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := broadcastTxCommit(context.Background(), srv.URL, []byte{0x01, 0x02})
	if err != nil {
		t.Fatalf("broadcastTxCommit: %v", err)
	}
	if res.CheckTxCode != 0 {
		t.Errorf("CheckTxCode = %d, want 0 (admitted to mempool)", res.CheckTxCode)
	}
	if res.TxResultCode != 47 {
		t.Errorf("TxResultCode = %d, want 47 (the block-execution rejection sync would hide)", res.TxResultCode)
	}
	if !strings.Contains(res.TxResultLog, "already pending") {
		t.Errorf("TxResultLog = %q, want it to carry the rejection reason", res.TxResultLog)
	}
	if res.Height != 4242 {
		t.Errorf("Height = %d, want 4242", res.Height)
	}
	if res.Hash != "ABC123" {
		t.Errorf("Hash = %q, want ABC123", res.Hash)
	}
}

// TestBroadcastTxCommit_Success confirms the happy path: both CheckTx and the
// block-execution result are Code 0.
func TestBroadcastTxCommit_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/broadcast_tx_commit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"hash":      "FEED",
				"height":    "100",
				"check_tx":  map[string]any{"code": 0},
				"tx_result": map[string]any{"code": 0},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := broadcastTxCommit(context.Background(), srv.URL, []byte{0xaa})
	if err != nil {
		t.Fatalf("broadcastTxCommit: %v", err)
	}
	if res.CheckTxCode != 0 || res.TxResultCode != 0 {
		t.Fatalf("expected success codes, got check=%d tx_result=%d", res.CheckTxCode, res.TxResultCode)
	}
	if res.Height != 100 {
		t.Errorf("Height = %d, want 100", res.Height)
	}
}

// TestBroadcastTxCommit_RPCError surfaces a CometBFT RPC error (e.g. the
// broadcast-commit timeout) as a Go error rather than a nil-result success.
func TestBroadcastTxCommit_RPCError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/broadcast_tx_commit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "timed out waiting for tx to be included in a block",
				"data":    "",
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := broadcastTxCommit(context.Background(), srv.URL, []byte{0x01}); err == nil {
		t.Fatal("expected an error for an RPC-error response, got nil")
	}
}
