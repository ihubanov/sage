package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// personalInitFixture builds a prepared personal-SAGE layout and the profile
// that describes it. The names are deliberately NOT any product's: a profile
// is free to choose its own artifact names, and the core must not care.
func personalInitFixture(t *testing.T) (*provisioningProfile, *Config) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "sage-personal")
	identityDir := filepath.Join(base, "identity")
	for _, path := range []string{home, identityDir} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("synthetic test input"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(home)
	cfg.VendoredAgentBootstrap = &VendoredAgentBootstrapConfig{
		AgentKeyFile: filepath.Join(identityDir, "companion-agent.key"),
		HomeDomain:   "private-fixture",
		Clearance:    1,
	}
	profile := &provisioningProfile{
		Schema:           provisioningProfileSchemaVersion,
		Home:             home,
		IdentityDir:      identityDir,
		Account:          "sage-companion",
		Units:            []string{"unit-a", "unit-b"},
		PrivateListeners: true,
		PublicKeyFile:    "companion-public.key",
		ClaimFile:        "companion-initialization-started",
		ResultSchema:     "sage.test.initialized.v1",
	}
	if err := profile.complete(); err != nil {
		t.Fatal(err)
	}
	return profile, cfg
}

// writeProfileFile writes a profile as a mode-0600 file owned by the test user
// and points the package's profile lookup at it.
func writeProfileFile(t *testing.T, profile *provisioningProfile) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provisioning-profile.yaml")
	raw, err := yaml.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	priorPath, priorUID := provisioningProfilePath, provisioningProfileOwnerUID
	provisioningProfilePath = path
	provisioningProfileOwnerUID = os.Geteuid()
	t.Cleanup(func() { provisioningProfilePath, provisioningProfileOwnerUID = priorPath, priorUID })
	return path
}

func TestFreshPersonalInitDelegatesWithoutRealKeys(t *testing.T) {
	profile, cfg := personalInitFixture(t)
	calls := 0
	initialize := func(comet, root string, bootstrap *VendoredAgentBootstrapConfig) error {
		calls++
		if comet != filepath.Join(profile.Home, "data", "cometbft") || root != cfg.AgentKey || bootstrap != cfg.VendoredAgentBootstrap {
			t.Fatal("wrong initializer binding")
		}
		return nil
	}
	public := func(path string) (string, error) {
		calls++
		if path != filepath.Join(profile.IdentityDir, "companion-public.key") {
			t.Fatal("wrong public key destination")
		}
		return "synthetic-public-id", nil
	}
	result, err := initializeFreshPersonalSage(profile, cfg, initialize, public)
	if err != nil || calls != 2 || result["ready"] != false || result["public_enrolled"] != false {
		t.Fatal(result, err, calls)
	}
	if result["schema"] != "sage.test.initialized.v1" {
		t.Fatalf("result schema must come from the profile, got %v", result["schema"])
	}
	if _, err := initializeFreshPersonalSage(profile, cfg, initialize, public); err == nil || calls != 2 {
		t.Fatal("repeated initialization admitted")
	}
	if _, err := os.Stat(cfg.AgentKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("test created a real key")
	}
}

func TestFreshPersonalInitPartialFailureContained(t *testing.T) {
	profile, cfg := personalInitFixture(t)
	_, err := initializeFreshPersonalSage(profile, cfg, func(string, string, *VendoredAgentBootstrapConfig) error { return errors.New("injected failure") },
		func(string) (string, error) { t.Fatal("public generation after failure"); return "", nil })
	if err == nil {
		t.Fatal("failure hidden")
	}
	if err := freshPersonalPaths(profile, cfg); err == nil {
		t.Fatal("partial state reset permitted")
	}
}

func TestFreshPersonalInitConcurrentClaim(t *testing.T) {
	profile, cfg := personalInitFixture(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := initializeFreshPersonalSage(profile, cfg, func(string, string, *VendoredAgentBootstrapConfig) error {
			close(entered)
			<-release
			return nil
		}, func(string) (string, error) { return "synthetic", nil })
		done <- err
	}()
	<-entered
	_, err := initializeFreshPersonalSage(profile, cfg, func(string, string, *VendoredAgentBootstrapConfig) error {
		t.Fatal("second initializer admitted")
		return nil
	}, func(string) (string, error) { return "synthetic", nil })
	close(release)
	if firstErr := <-done; firstErr != nil || err == nil {
		t.Fatal("exclusive claim failed", firstErr, err)
	}
}

func TestFreshPersonalInitPublicFailureContained(t *testing.T) {
	profile, cfg := personalInitFixture(t)
	_, err := initializeFreshPersonalSage(profile, cfg, func(string, string, *VendoredAgentBootstrapConfig) error { return nil },
		func(string) (string, error) { return "", errors.New("injected public key failure") })
	if err == nil || freshPersonalPaths(profile, cfg) == nil {
		t.Fatal("public failure permitted retry")
	}
}

func TestFreshPersonalInitRejectsExistingOrMismatched(t *testing.T) {
	for _, name := range []string{"root", "data", "public", "symlink", "permissions", "chain", "companion"} {
		t.Run(name, func(t *testing.T) {
			profile, cfg := personalInitFixture(t)
			switch name {
			case "root":
				os.WriteFile(cfg.AgentKey, []byte("not a key"), 0600)
			case "data":
				os.Mkdir(cfg.DataDir, 0700)
			case "public":
				os.WriteFile(filepath.Join(profile.IdentityDir, "companion-public.key"), []byte("not a key"), 0600)
			case "symlink":
				os.Remove(filepath.Join(profile.Home, "config.yaml"))
				os.Symlink("/nonexistent", filepath.Join(profile.Home, "config.yaml"))
			case "permissions":
				os.Chmod(profile.Home, 0755)
			case "chain":
				cfg.ChainID = "copied-chain"
			case "companion":
				cfg.VendoredAgentBootstrap = nil
			}
			if err := freshPersonalPaths(profile, cfg); err == nil {
				t.Fatal("unsafe initialization admitted")
			}
		})
	}
}

func TestFreshPersonalInitRejectsIdentityInsideHome(t *testing.T) {
	profile, cfg := personalInitFixture(t)
	profile.IdentityDir = filepath.Join(profile.Home, "identity")
	if err := profile.complete(); err == nil {
		t.Fatal("identity directory inside home admitted")
	}
	_ = cfg
}

func TestParseFreshPersonalInitArgsRequiresExactConsent(t *testing.T) {
	const consent = "--owner-approved-companion-bootstrap"
	profile, _ := personalInitFixture(t)
	good := []string{consent, "--profile", "/etc/sage/provisioning-profile.yaml", "--unit", "unit-a"}
	if p, u, ok := parseFreshPersonalInitArgs(good); !ok || p != "/etc/sage/provisioning-profile.yaml" || u != "unit-a" {
		t.Fatalf("valid invocation refused: %q %q %v", p, u, ok)
	}
	// Order must not matter, but every flag must be present exactly once.
	reordered := []string{"--unit", "unit-a", "--profile", "/etc/sage/provisioning-profile.yaml", consent}
	if _, _, ok := parseFreshPersonalInitArgs(reordered); !ok {
		t.Fatal("reordered flags refused")
	}
	for _, args := range [][]string{
		nil,
		{consent},
		{"--unit", "unit-a", "--profile", "/etc/p"},
		{consent, consent, "--profile", "/etc/p", "--unit", "unit-a"},
		{consent, "--profile", "/etc/p", "--profile", "/etc/p", "--unit"},
		{consent, "--profile", "/etc/p", "--unit", "unit-a", "extra"},
		{consent, "--profile", "/etc/p", "--unit"},
		{consent, "unit-a", "--profile", "/etc/p", "--unit", "unit-b"},
	} {
		if _, _, ok := parseFreshPersonalInitArgs(args); ok {
			t.Fatalf("inexact invocation admitted: %q", args)
		}
	}
	_ = profile
}

func TestFreshPersonalInitRequiresPrivateProfileAndFixedHome(t *testing.T) {
	for _, mode := range []string{"wrong-home", "listeners-disabled"} {
		t.Run(mode, func(t *testing.T) {
			profile, _ := personalInitFixture(t)
			path := writeProfileFile(t, profile)
			t.Setenv("SAGE_HOME", profile.Home)
			if mode == "wrong-home" {
				t.Setenv("SAGE_HOME", t.TempDir())
			} else {
				profile.PrivateListeners = false
				path = writeProfileFile(t, profile)
			}
			err := runFreshPersonalInit([]string{"--owner-approved-companion-bootstrap", "--profile", path, "--unit", "unit-a"})
			if err == nil || err.Error() != "explicit owner Companion bootstrap consent and fixed private home required" {
				t.Fatalf("private environment gate bypassed: %v", err)
			}
		})
	}
}
