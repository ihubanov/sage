package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// rawProfileDir makes a symlink-free temp dir and returns a path inside it.
func rawProfileDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// withProfileGlobals points the package at path with the test user's uid.
func withProfileGlobals(t *testing.T, path string) {
	t.Helper()
	priorPath, priorUID := provisioningProfilePath, provisioningProfileOwnerUID
	provisioningProfilePath = path
	provisioningProfileOwnerUID = os.Geteuid()
	t.Cleanup(func() { provisioningProfilePath, provisioningProfileOwnerUID = priorPath, priorUID })
}

func minimalProfileYAML(t *testing.T) []byte {
	t.Helper()
	base := rawProfileDir(t)
	raw, err := yaml.Marshal(&provisioningProfile{
		Schema:           provisioningProfileSchemaVersion,
		Home:             filepath.Join(base, "sage-personal"),
		IdentityDir:      filepath.Join(base, "identity"),
		Account:          "sage-companion",
		Units:            []string{"unit-a"},
		PrivateListeners: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProvisioningProfileRejectsUnsafeFile(t *testing.T) {
	valid := minimalProfileYAML(t)

	t.Run("symlink", func(t *testing.T) {
		dir := rawProfileDir(t)
		target := filepath.Join(dir, "target.yaml")
		if err := os.WriteFile(target, valid, 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "profile.yaml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		withProfileGlobals(t, link)
		if _, err := activeProvisioningProfile(); err == nil {
			t.Fatal("symlinked profile accepted")
		}
	})

	t.Run("group_readable", func(t *testing.T) {
		path := filepath.Join(rawProfileDir(t), "profile.yaml")
		if err := os.WriteFile(path, valid, 0644); err != nil {
			t.Fatal(err)
		}
		withProfileGlobals(t, path)
		if _, err := activeProvisioningProfile(); err == nil {
			t.Fatal("world-readable profile accepted")
		}
	})

	t.Run("group_writable", func(t *testing.T) {
		path := filepath.Join(rawProfileDir(t), "profile.yaml")
		if err := os.WriteFile(path, valid, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0660); err != nil {
			t.Fatal(err)
		}
		withProfileGlobals(t, path)
		if _, err := activeProvisioningProfile(); err == nil {
			t.Fatal("group-writable profile accepted")
		}
	})

	t.Run("wrong_owner", func(t *testing.T) {
		path := filepath.Join(rawProfileDir(t), "profile.yaml")
		if err := os.WriteFile(path, valid, 0600); err != nil {
			t.Fatal(err)
		}
		withProfileGlobals(t, path)
		provisioningProfileOwnerUID = os.Geteuid() + 1
		if _, err := activeProvisioningProfile(); err == nil {
			t.Fatal("profile with the wrong owner accepted")
		}
	})

	t.Run("relative_path", func(t *testing.T) {
		// Existence is decided before shape, so a relative path must still be
		// rejected once it names a real file. Chdir into the directory so the
		// relative path is unambiguous regardless of the test binary's cwd.
		dir := rawProfileDir(t)
		if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), valid, 0600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		withProfileGlobals(t, "profile.yaml")
		if _, err := activeProvisioningProfile(); err == nil {
			t.Fatal("relative profile path accepted")
		}
	})

	t.Run("empty", func(t *testing.T) {
		path := filepath.Join(rawProfileDir(t), "profile.yaml")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		withProfileGlobals(t, path)
		if _, err := activeProvisioningProfile(); err == nil {
			t.Fatal("empty profile accepted")
		}
	})

	t.Run("unknown_field", func(t *testing.T) {
		path := filepath.Join(rawProfileDir(t), "profile.yaml")
		raw := append(valid, []byte("surprise: true\n")...)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		withProfileGlobals(t, path)
		if _, err := activeProvisioningProfile(); err == nil {
			t.Fatal("unknown profile field accepted")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(rawProfileDir(t), "profile.yaml")
		raw := append(valid, []byte(strings.Repeat("#", provisioningProfileMaxBytes)+"\n")...)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		withProfileGlobals(t, path)
		if _, err := activeProvisioningProfile(); err == nil {
			t.Fatal("oversized profile accepted")
		}
	})

	t.Run("absent_is_not_an_error", func(t *testing.T) {
		withProfileGlobals(t, filepath.Join(rawProfileDir(t), "missing.yaml"))
		profile, err := activeProvisioningProfile()
		if err != nil || profile != nil {
			t.Fatalf("absent profile must mean 'no policy': profile=%v err=%v", profile, err)
		}
	})
}

func TestProvisioningProfileRejectsMalformedFields(t *testing.T) {
	base := rawProfileDir(t)
	home := filepath.Join(base, "sage-personal")
	identity := filepath.Join(base, "identity")
	valid := func() *provisioningProfile {
		return &provisioningProfile{
			Schema:           provisioningProfileSchemaVersion,
			Home:             home,
			IdentityDir:      identity,
			Account:          "sage-companion",
			Units:            []string{"unit-a", "unit-b"},
			PrivateListeners: true,
		}
	}
	if err := valid().complete(); err != nil {
		t.Fatalf("baseline profile rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*provisioningProfile)
	}{
		{"schema", func(p *provisioningProfile) { p.Schema = "other.v9" }},
		{"relative_home", func(p *provisioningProfile) { p.Home = "sage-personal" }},
		{"unclean_home", func(p *provisioningProfile) { p.Home = base + "/./sage-personal" }},
		{"root_home", func(p *provisioningProfile) { p.Home = "/" }},
		{"relative_identity", func(p *provisioningProfile) { p.IdentityDir = "identity" }},
		{"identity_is_home", func(p *provisioningProfile) { p.IdentityDir = p.Home }},
		{"identity_under_home", func(p *provisioningProfile) { p.IdentityDir = filepath.Join(p.Home, "identity") }},
		{"account", func(p *provisioningProfile) { p.Account = "Sage Companion" }},
		{"account_path", func(p *provisioningProfile) { p.Account = "../root" }},
		{"no_units", func(p *provisioningProfile) { p.Units = nil }},
		{"unit_traversal", func(p *provisioningProfile) { p.Units = []string{"../unit-a"} }},
		{"unit_upper", func(p *provisioningProfile) { p.Units = []string{"Unit-a"} }},
		{"unit_trailing_space", func(p *provisioningProfile) { p.Units = []string{"unit-a "} }},
		{"unit_duplicate", func(p *provisioningProfile) { p.Units = []string{"unit-a", "unit-a"} }},
		{"artifact_path", func(p *provisioningProfile) { p.PublicKeyFile = "../escape.key" }},
		{"artifact_dotdot", func(p *provisioningProfile) { p.ResultSchema = ".." }},
		{"metadata_half", func(p *provisioningProfile) { p.UnitMetadataFile = "/etc/x/unit.env" }},
		{"metadata_relative", func(p *provisioningProfile) {
			p.UnitMetadataFile, p.UnitMetadataKey = "unit.env", "UNIT_ID="
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := valid()
			test.mutate(profile)
			if err := profile.complete(); err == nil {
				t.Fatal("malformed profile accepted")
			}
		})
	}
}

func TestProvisioningProfileDefaultsAreProductNeutral(t *testing.T) {
	base := rawProfileDir(t)
	profile := &provisioningProfile{
		Schema:           provisioningProfileSchemaVersion,
		Home:             filepath.Join(base, "sage-personal"),
		IdentityDir:      filepath.Join(base, "identity"),
		Account:          "sage-companion",
		Units:            []string{"unit-a"},
		PrivateListeners: true,
	}
	if err := profile.complete(); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"public_key_file":            profile.PublicKeyFile,
		"claim_file":                 profile.ClaimFile,
		"identity_requirements_file": profile.IdentityRequirementsFile,
		"result_schema":              profile.ResultSchema,
	} {
		for _, product := range []string{"lantern", "meshticulum", "commons"} {
			if strings.Contains(strings.ToLower(value), product) {
				t.Fatalf("default %s leaks product name %q: %q", name, product, value)
			}
		}
	}
}

// TestProvisioningProfileKeepsProductArtifactNames proves the reason the
// profile carries artifact names at all: a product that already shipped
// different filenames keeps them, without SAGE core learning the name.
func TestProvisioningProfileKeepsProductArtifactNames(t *testing.T) {
	base := rawProfileDir(t)
	home := filepath.Join(base, "sage-personal")
	profile := &provisioningProfile{
		Schema:                   provisioningProfileSchemaVersion,
		Home:                     home,
		IdentityDir:              filepath.Join(base, "identity"),
		Account:                  "sage-lantern",
		Units:                    []string{"unit-a", "unit-b"},
		PrivateListeners:         true,
		PublicKeyFile:            "lantern-public.key",
		ClaimFile:                "lantern-initialization-started",
		ResultSchema:             "sage.lantern.initialized.v1",
		UnitMetadataFile:         filepath.Join(base, "unit.env"),
		UnitMetadataKey:          "UNIT_ID=",
		IdentityRequirementsFile: "EXTERNAL_IDENTITY_REQUIREMENTS.txt",
	}
	if err := profile.complete(); err != nil {
		t.Fatal(err)
	}
	if profile.PublicKeyFile != "lantern-public.key" || profile.ClaimFile != "lantern-initialization-started" ||
		profile.ResultSchema != "sage.lantern.initialized.v1" {
		t.Fatal("profile artifact names were rewritten")
	}
	if err := os.WriteFile(profile.UnitMetadataFile, []byte("UNIT_ID=unit-b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := profile.verifyUnitBinding("unit-b"); err != nil {
		t.Fatalf("authorized unit refused: %v", err)
	}
	if err := profile.verifyUnitBinding("unit-c"); err == nil {
		t.Fatal("unit outside the profile admitted")
	}
}

func TestVerifyUnitBindingHonoursHostMetadata(t *testing.T) {
	base := rawProfileDir(t)
	metadata := filepath.Join(base, "unit.env")
	profile := &provisioningProfile{
		Schema:           provisioningProfileSchemaVersion,
		Home:             filepath.Join(base, "sage-personal"),
		IdentityDir:      filepath.Join(base, "identity"),
		Account:          "sage-companion",
		Units:            []string{"unit-a", "unit-b"},
		PrivateListeners: true,
		UnitMetadataFile: metadata,
		UnitMetadataKey:  "UNIT_ID=",
	}
	if err := profile.complete(); err != nil {
		t.Fatal(err)
	}

	// Metadata narrows the profile: both units are listed, but this host was
	// prepared for unit-a only.
	if err := os.WriteFile(metadata, []byte("UNIT_ID=unit-a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := profile.verifyUnitBinding("unit-a"); err != nil {
		t.Fatalf("prepared unit refused: %v", err)
	}
	if err := profile.verifyUnitBinding("unit-b"); err == nil {
		t.Fatal("unit not bound by host metadata admitted")
	}

	for name, content := range map[string]string{
		"absent_key":     "SOMETHING=else\n",
		"duplicate_key":  "UNIT_ID=unit-a\nUNIT_ID=unit-a\n",
		"no_newline":     "UNIT_ID=unit-a",
		"empty":          "",
		"prefix_collide": "UNIT_ID=unit-ab\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(metadata, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if err := profile.verifyUnitBinding("unit-a"); err == nil {
				t.Fatalf("malformed host metadata admitted: %q", content)
			}
		})
	}

	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := profile.verifyUnitBinding("unit-a"); err == nil {
		t.Fatal("missing host metadata admitted")
	}
}

// TestLoadProvisioningProfileDecidesExistenceBeforeShape is the regression guard
// for a Windows startup break: the default profile path is POSIX-shaped, and on
// a host with different path semantics filepath.IsAbs returns false for it. If
// path shape were validated first, "this host has no profile" would become a
// configuration error and an ordinary node would refuse to start.
func TestLoadProvisioningProfileDecidesExistenceBeforeShape(t *testing.T) {
	missing := filepath.Join(rawProfileDir(t), "absent.yaml")
	_, err := loadProvisioningProfile(missing)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent absolute profile = %v, want a not-exist error", err)
	}

	// A relative AND absent path must also be not-exist, not a shape rejection:
	// absence is decided first, so the ordering cannot depend on platform path
	// rules.
	if _, err := loadProvisioningProfile("relative/absent.yaml"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent relative profile = %v, want a not-exist error", err)
	}

	// Shape is still enforced once a file actually exists.
	present := filepath.Join(rawProfileDir(t), "profile.yaml")
	if writeErr := os.WriteFile(present, minimalProfileYAML(t), 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if _, err := loadProvisioningProfile("relative/present.yaml"); err == nil {
		t.Fatal("relative path with existing content accepted")
	}
}
