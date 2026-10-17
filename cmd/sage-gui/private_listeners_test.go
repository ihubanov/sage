package main

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// privateListenerProfile writes a minimal profile whose only interesting field
// is private_listeners, and points the package at it.
func privateListenerProfile(t *testing.T, private bool) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	profile := &provisioningProfile{
		Schema:           provisioningProfileSchemaVersion,
		Home:             filepath.Join(base, "sage-personal"),
		IdentityDir:      filepath.Join(base, "identity"),
		Account:          "sage-companion",
		Units:            []string{"unit-a"},
		PrivateListeners: private,
	}
	if err := profile.complete(); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "provisioning-profile.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	priorPath, priorUID := provisioningProfilePath, provisioningProfileOwnerUID
	provisioningProfilePath = path
	provisioningProfileOwnerUID = os.Geteuid()
	t.Cleanup(func() { provisioningProfilePath, provisioningProfileOwnerUID = priorPath, priorUID })
	return path
}

func TestPrivateListenerYAML(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		allowed    bool
	}{
		{"private", "federation: {enabled: false}\nquorum: {enabled: false}\n", true},
		{"federation", "federation: {enabled: true}\n", false},
		{"quorum", "federation: {enabled: false}\nquorum: {enabled: true}\n", false},
		{"omitted_defaults", "{}\n", false},
		{"null_federation", "federation: null\n", false},
		{"duplicate", "federation: {enabled: false}\nfederation: {enabled: true}\n", false},
		{"wildcard_overridden", "federation: {enabled: false}\nrest_addr: 0.0.0.0:8080\nquorum: {enabled: false, tls_addr: '0.0.0.0:8443'}\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			privateListenerProfile(t, true)
			t.Setenv("SAGE_HOME", home)
			t.Setenv("REST_ADDR", "127.0.0.1:8080")
			t.Setenv("SAGE_TLS_ADDR", "127.0.0.1:8443")
			t.Setenv("SAGE_CMT_RPC_ADDR", "tcp://127.0.0.1:26657")
			t.Setenv("SAGE_CMT_P2P_ADDR", "tcp://127.0.0.1:26656")
			if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(test.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig()
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v error=%v", test.allowed, err)
			}
			if err == nil && (cfg.RESTAddr != "127.0.0.1:8080" || cfg.Quorum.TLSAddr != "127.0.0.1:8443") {
				t.Fatal("listener override lost")
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 1 {
				t.Fatal("config check created state", err)
			}
		})
	}
}

func TestPrivateListenerRejectsExternalAddresses(t *testing.T) {
	t.Setenv("SAGE_CMT_RPC_ADDR", "tcp://127.0.0.1:26657")
	t.Setenv("SAGE_CMT_P2P_ADDR", "tcp://127.0.0.1:26656")
	cfg := DefaultConfig(t.TempDir())
	cfg.Federation.Enabled = false
	cfg.Quorum.TLSAddr = "127.0.0.1:8443"
	if err := enforcePrivateListeners(cfg, true); err != nil {
		t.Fatalf("loopback configuration refused: %v", err)
	}
	cfg.Federation.Enabled = true
	if err := enforcePrivateListeners(cfg, true); err == nil {
		t.Fatal("federation admitted under a private profile")
	}
	cfg.Federation.Enabled = false
	for _, name := range []string{"SAGE_CMT_RPC_ADDR", "SAGE_CMT_P2P_ADDR"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "tcp://0.0.0.0:26657")
			if err := enforcePrivateListeners(cfg, true); err == nil {
				t.Fatal("accepted external address")
			}
		})
	}
	// A host with no private-listener profile must be completely untouched.
	t.Setenv("SAGE_CMT_RPC_ADDR", "tcp://0.0.0.0:26657")
	cfg.Federation.Enabled = true
	if err := enforcePrivateListeners(cfg, false); err != nil {
		t.Fatalf("profile-free host was constrained: %v", err)
	}
}

func TestPrivateListenerRechecksConfig(t *testing.T) {
	home := t.TempDir()
	privateListenerProfile(t, true)
	t.Setenv("SAGE_HOME", home)
	t.Setenv("REST_ADDR", "127.0.0.1:8080")
	t.Setenv("SAGE_TLS_ADDR", "127.0.0.1:8443")
	t.Setenv("SAGE_CMT_RPC_ADDR", "tcp://127.0.0.1:26657")
	t.Setenv("SAGE_CMT_P2P_ADDR", "tcp://127.0.0.1:26656")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("missing configuration accepted")
	}
	path := filepath.Join(home, "config.yaml")
	for _, enabled := range []string{"false", "true"} {
		if err := os.WriteFile(path, []byte("quorum: {enabled: false}\nfederation: {enabled: "+enabled+"}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig()
		if (err == nil) != (enabled == "false") {
			t.Fatalf("config was not revalidated: %v", err)
		}
	}
}

// TestPrivateListenerAbsentProfileLeavesNodeAlone is the regression guard for
// the whole point of this refactor: an ordinary personal node, which has no
// provisioning profile, must keep its historical permissive behaviour.
func TestPrivateListenerAbsentProfileLeavesNodeAlone(t *testing.T) {
	home := t.TempDir()
	priorPath, priorUID := provisioningProfilePath, provisioningProfileOwnerUID
	provisioningProfilePath = filepath.Join(t.TempDir(), "absent.yaml")
	provisioningProfileOwnerUID = os.Geteuid()
	t.Cleanup(func() { provisioningProfilePath, provisioningProfileOwnerUID = priorPath, priorUID })
	t.Setenv("SAGE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"),
		[]byte("federation: {enabled: true}\nquorum: {enabled: true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("profile-free node was constrained: %v", err)
	}
}
