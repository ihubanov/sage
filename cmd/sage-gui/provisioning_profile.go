package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// A provisioningProfile is the root-owned description of ONE native fresh
// personal-SAGE initialization.
//
// SAGE core intentionally carries no product names. Which product ships a
// profile, what its units are called, and where it keeps identity material are
// all declared here by the administrator who installed the appliance. That
// replaces the earlier arrangement, where one product's home path, Unix
// account and unit list were compiled into SAGE core and gated on a
// product-branded environment variable: the same fixed-path hardening, with no
// product knowledge in the daemon.
type provisioningProfile struct {
	Schema string `yaml:"schema"`

	// Home is the SAGE_HOME of the personal node being initialized.
	Home string `yaml:"home"`
	// IdentityDir holds the companion's private and public identity material.
	// It must be a prepared 0700 directory that is not inside Home, so
	// identity keys never land in the memory node's own data tree.
	IdentityDir string `yaml:"identity_dir"`
	// Account is the unprivileged Unix account initialization must run as.
	Account string `yaml:"account"`
	// Units are the appliance unit ids this profile authorizes. An init call
	// naming a unit outside this list is refused.
	Units []string `yaml:"units"`
	// PrivateListeners requires loopback-only listeners with federation and
	// quorum disabled for every node started under this profile.
	PrivateListeners bool `yaml:"private_listeners"`

	// Optional artifact names. An empty field means the product-neutral
	// default, so a profile only spells these out when its product already
	// shipped different names and must keep them.
	PublicKeyFile            string `yaml:"public_key_file"`
	ClaimFile                string `yaml:"claim_file"`
	IdentityRequirementsFile string `yaml:"identity_requirements_file"`
	ResultSchema             string `yaml:"result_schema"`
	// UnitMetadataFile and UnitMetadataKey optionally bind the requested unit
	// id to prepared root-owned host metadata — a file that must contain
	// exactly one line "<key><unit>". Leave both empty to rely on Units alone.
	UnitMetadataFile string `yaml:"unit_metadata_file"`
	UnitMetadataKey  string `yaml:"unit_metadata_key"`
}

const (
	// provisioningProfileSchemaVersion identifies the profile format itself.
	provisioningProfileSchemaVersion = "sage.provisioning-profile.v1"

	defaultPublicKeyFile            = "public-agent.key"
	defaultClaimFile                = "initialization-started"
	defaultIdentityRequirementsFile = "EXTERNAL_IDENTITY_REQUIREMENTS.txt"
	defaultProvisioningResultSchema = "sage.fresh-personal-init.v1"

	provisioningProfileMaxBytes = 16 << 10
	unitMetadataMaxBytes        = 16 << 10
)

var unitIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var accountNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// provisioningProfilePath is the fixed, root-owned profile that enables
// product-specific native bootstrap policy for this host. It is a variable
// only so tests can point at a fixture; nothing at runtime may set it.
var provisioningProfilePath = "/etc/sage/provisioning-profile.yaml"

// provisioningProfileOwnerUID is the uid a provisioning profile must be owned
// by. Tests lower it so the loader can be exercised without root.
var provisioningProfileOwnerUID = 0

// loadProvisioningProfile reads and authenticates a profile. Every failure is
// fatal: a profile that cannot be proven root-owned and well-formed must never
// be partially applied.
func loadProvisioningProfile(path string) (*provisioningProfile, error) {
	// Existence is checked BEFORE path shape. Hosts whose path semantics differ
	// from the POSIX default (Windows treats "/etc/..." as relative) would
	// otherwise turn "this host has no profile" into a configuration error and
	// stop an ordinary node from starting.
	//
	// The path is caller-supplied by design (the fixed default is only a
	// default); every property that makes it safe is proven below rather than
	// assumed from where it came from: absolute and clean, a regular file, no
	// symlink, root-owned, and unreadable to group/other.
	info, err := os.Lstat(path) //nolint:gosec // G703: taint is the point; validated immediately below
	if err != nil {
		return nil, fmt.Errorf("read provisioning profile: %w", err)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("provisioning profile path must be absolute and clean")
	}
	// Lstat deliberately: a symlinked profile would let whoever controls the
	// link choose the policy.
	if !info.Mode().IsRegular() {
		return nil, errors.New("provisioning profile must be a regular file, not a symlink or device")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("provisioning profile must not be readable or writable by group or other")
	}
	if uid, ok := fileOwnerUID(info); !ok || uid != provisioningProfileOwnerUID {
		return nil, fmt.Errorf("provisioning profile must be owned by uid %d", provisioningProfileOwnerUID)
	}
	if info.Size() > provisioningProfileMaxBytes {
		return nil, errors.New("provisioning profile is too large")
	}
	data, err := os.ReadFile(path) //nolint:gosec // G703: same validated path as the Lstat above
	if err != nil {
		return nil, fmt.Errorf("read provisioning profile: %w", err)
	}
	profile := &provisioningProfile{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(profile); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("provisioning profile is empty")
		}
		return nil, fmt.Errorf("parse provisioning profile: %w", err)
	}
	if err := profile.complete(); err != nil {
		return nil, err
	}
	return profile, nil
}

// activeProvisioningProfile returns the host profile, or nil when this host has
// none. Ordinary personal nodes never install one.
func activeProvisioningProfile() (*provisioningProfile, error) {
	// A platform with no concept of a root-owned file has no profile support at
	// all: fresh native provisioning is a Linux appliance operation. Report "no
	// profile" rather than evaluating a POSIX-shaped path under different rules.
	if !provisioningProfileSupported() {
		return nil, nil
	}
	profile, err := loadProvisioningProfile(provisioningProfilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return profile, nil
}

// complete validates the profile and fills product-neutral defaults.
func (p *provisioningProfile) complete() error {
	if p.Schema != provisioningProfileSchemaVersion {
		return fmt.Errorf("unsupported provisioning profile schema %q", p.Schema)
	}
	if !filepath.IsAbs(p.Home) || filepath.Clean(p.Home) != p.Home || p.Home == string(filepath.Separator) {
		return errors.New("provisioning profile home must be an absolute, clean, non-root path")
	}
	if !filepath.IsAbs(p.IdentityDir) || filepath.Clean(p.IdentityDir) != p.IdentityDir ||
		p.IdentityDir == string(filepath.Separator) {
		return errors.New("provisioning profile identity_dir must be an absolute, clean, non-root path")
	}
	if p.IdentityDir == p.Home || strings.HasPrefix(p.IdentityDir+string(filepath.Separator), p.Home+string(filepath.Separator)) {
		return errors.New("provisioning profile identity_dir must live outside home")
	}
	if !accountNamePattern.MatchString(p.Account) {
		return errors.New("provisioning profile account must be a bare Unix account name")
	}
	if len(p.Units) == 0 {
		return errors.New("provisioning profile must authorize at least one unit")
	}
	seen := make(map[string]bool, len(p.Units))
	for _, unit := range p.Units {
		if !unitIDPattern.MatchString(unit) {
			return fmt.Errorf("provisioning profile unit %q is not a bare unit id", unit)
		}
		if seen[unit] {
			return fmt.Errorf("provisioning profile repeats unit %q", unit)
		}
		seen[unit] = true
	}

	for _, field := range []struct {
		name  string
		value *string
		fall  string
	}{
		{"public_key_file", &p.PublicKeyFile, defaultPublicKeyFile},
		{"claim_file", &p.ClaimFile, defaultClaimFile},
		{"identity_requirements_file", &p.IdentityRequirementsFile, defaultIdentityRequirementsFile},
		{"result_schema", &p.ResultSchema, defaultProvisioningResultSchema},
	} {
		if *field.value == "" {
			*field.value = field.fall
		}
		if *field.value != filepath.Base(*field.value) || *field.value == "." || *field.value == ".." {
			return fmt.Errorf("provisioning profile %s must be a bare file name", field.name)
		}
	}

	if (p.UnitMetadataFile == "") != (p.UnitMetadataKey == "") {
		return errors.New("provisioning profile unit_metadata_file and unit_metadata_key must be set together")
	}
	if p.UnitMetadataFile != "" {
		if !filepath.IsAbs(p.UnitMetadataFile) || filepath.Clean(p.UnitMetadataFile) != p.UnitMetadataFile {
			return errors.New("provisioning profile unit_metadata_file must be an absolute, clean path")
		}
		if p.UnitMetadataKey == "" || strings.ContainsAny(p.UnitMetadataKey, " \t\r\n\x00") ||
			strings.Count(p.UnitMetadataKey, "=") > 1 || (strings.Contains(p.UnitMetadataKey, "=") &&
			!strings.HasSuffix(p.UnitMetadataKey, "=")) {
			return errors.New("provisioning profile unit_metadata_key must be a bare key")
		}
	}
	return nil
}

// verifyUnitBinding enforces that the requested unit is one this host was
// prepared for. The profile's unit list is the primary binding; optional host
// metadata narrows it further so a single unit.env copy cannot authorize a
// different unit on the same machine.
func (p *provisioningProfile) verifyUnitBinding(unit string) error {
	if !unitIDPattern.MatchString(unit) {
		return errors.New("prepared unit binding mismatch")
	}
	allowed := false
	for _, candidate := range p.Units {
		if candidate == unit {
			allowed = true
			break
		}
	}
	if !allowed {
		return errors.New("prepared unit binding mismatch")
	}
	if p.UnitMetadataFile == "" {
		return nil
	}
	info, err := os.Lstat(p.UnitMetadataFile)
	if err != nil || !info.Mode().IsRegular() || info.Size() > unitMetadataMaxBytes {
		return errors.New("prepared unit binding mismatch")
	}
	metadata, err := os.ReadFile(p.UnitMetadataFile)
	if err != nil {
		return errors.New("prepared unit binding mismatch")
	}
	marker := "\n" + p.UnitMetadataKey + unit + "\n"
	if strings.Count(string(metadata), p.UnitMetadataKey) != 1 ||
		!strings.Contains("\n"+string(metadata), marker) {
		return errors.New("prepared unit binding mismatch")
	}
	return nil
}

// fileOwnerUID reports the owning uid of an os.FileInfo on platforms that
// expose it.
func fileOwnerUID(info os.FileInfo) (int, bool) {
	return fileOwnerUIDPlatform(info)
}
