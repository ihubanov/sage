package main

import (
	"errors"

	"gopkg.in/yaml.v3"
)

// privateListenerPolicy reports whether the host's provisioning profile
// requires private listeners. A host with no profile, or a profile that does
// not ask for them, leaves configuration completely untouched.
func privateListenerPolicy() (bool, error) {
	profile, err := activeProvisioningProfile()
	if err != nil {
		return false, err
	}
	return profile != nil && profile.PrivateListeners, nil
}

// validatePrivateListenerYAML refuses a configuration file that does not
// explicitly disable federation and quorum. Explicit is the whole point: an
// omitted key must not be read as the default.
func validatePrivateListenerYAML(data []byte, private bool) error {
	if !private {
		return nil
	}
	var explicit struct {
		Federation *struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"federation"`
		Quorum *struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"quorum"`
	}
	if err := yaml.Unmarshal(data, &explicit); err != nil {
		return errors.New("invalid private-listener configuration")
	}
	if explicit.Federation == nil || explicit.Federation.Enabled == nil || *explicit.Federation.Enabled ||
		explicit.Quorum == nil || explicit.Quorum.Enabled == nil || *explicit.Quorum.Enabled {
		return errors.New("private-listener node requires explicit federation.enabled=false and quorum.enabled=false")
	}
	return nil
}

// enforcePrivateListeners re-checks the merged configuration. The YAML check
// above runs before environment overrides, so a private node needs both: the
// file must say false and the effective config must still be false with fixed
// loopback listeners.
func enforcePrivateListeners(cfg *Config, private bool) error {
	if !private {
		return nil
	}
	if cfg.Federation.Enabled || cfg.Quorum.Enabled {
		return errors.New("private-listener node forbids federation and quorum")
	}
	if cfg.RESTAddr != "127.0.0.1:8080" || cfg.Quorum.TLSAddr != "127.0.0.1:8443" ||
		cmtRPCAddr() != "tcp://127.0.0.1:26657" || cmtP2PAddr("tcp://127.0.0.1:26656") != "tcp://127.0.0.1:26656" {
		return errors.New("private-listener node requires fixed loopback listeners")
	}
	return nil
}
