package validator

import (
	"crypto/ed25519"
	"fmt"
	"sort"
	"sync"
)

// ValidatorInfo holds metadata about a single validator.
type ValidatorInfo struct {
	ID        string            `json:"id"`
	PublicKey ed25519.PublicKey `json:"public_key"`
	Power     int64             `json:"power"`
	PoEWeight float64           `json:"poe_weight"`
}

// ValidatorSet manages the current set of validators.
type ValidatorSet struct {
	mu         sync.RWMutex
	validators map[string]*ValidatorInfo
	totalPower int64
}

// NewValidatorSet creates an empty validator set.
func NewValidatorSet() *ValidatorSet {
	return &ValidatorSet{
		validators: make(map[string]*ValidatorInfo),
	}
}

// AddValidator adds a validator to the set.
func (vs *ValidatorSet) AddValidator(info *ValidatorInfo) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if info.ID == "" {
		return fmt.Errorf("validator ID must not be empty")
	}
	if _, exists := vs.validators[info.ID]; exists {
		return fmt.Errorf("validator %s already exists", info.ID)
	}
	if info.Power < 0 {
		return fmt.Errorf("validator power must be non-negative, got %d", info.Power)
	}

	vs.validators[info.ID] = info
	vs.totalPower += info.Power
	return nil
}

// RemoveValidator removes a validator from the set.
func (vs *ValidatorSet) RemoveValidator(id string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	v, exists := vs.validators[id]
	if !exists {
		return fmt.Errorf("validator %s not found", id)
	}

	vs.totalPower -= v.Power
	delete(vs.validators, id)
	return nil
}

// GetValidator returns a validator by ID.
func (vs *ValidatorSet) GetValidator(id string) (*ValidatorInfo, bool) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	v, ok := vs.validators[id]
	return v, ok
}

// GetAll returns all validators sorted by ID for deterministic iteration.
func (vs *ValidatorSet) GetAll() []*ValidatorInfo {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	ids := make([]string, 0, len(vs.validators))
	for id := range vs.validators {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	result := make([]*ValidatorInfo, 0, len(ids))
	for _, id := range ids {
		result = append(result, vs.validators[id])
	}
	return result
}

// TotalPower returns the sum of all validator powers.
func (vs *ValidatorSet) TotalPower() int64 {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.totalPower
}

// Size returns the number of validators.
func (vs *ValidatorSet) Size() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return len(vs.validators)
}

// UpdatePower updates a validator's voting power, enforcing that the change
// does not exceed 1/3 of the current total power (CometBFT constraint).
func (vs *ValidatorSet) UpdatePower(id string, newPower int64) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	v, exists := vs.validators[id]
	if !exists {
		return fmt.Errorf("validator %s not found", id)
	}
	if newPower < 0 {
		return fmt.Errorf("validator power must be non-negative, got %d", newPower)
	}

	diff := newPower - v.Power
	if diff < 0 {
		diff = -diff
	}

	// CometBFT enforces max 1/3 total power change per block
	maxChange := vs.totalPower / 3
	if maxChange == 0 && vs.totalPower > 0 {
		maxChange = 1
	}

	if diff > maxChange {
		return fmt.Errorf("power change %d exceeds max allowed %d (1/3 of total %d)", diff, maxChange, vs.totalPower)
	}

	vs.totalPower = vs.totalPower - v.Power + newPower
	v.Power = newPower
	return nil
}
