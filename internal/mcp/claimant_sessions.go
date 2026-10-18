package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxClaimantIdentityFiles = 256

// claimantIdentityView is the payload-free description of one durable MCP
// claimant identity. It deliberately carries no message content or authority.
type claimantIdentityView struct {
	SessionID            string
	Provider             string
	Project              string
	Transport            string
	CreatedAt            time.Time
	Live                 bool
	Liveness             string
	LivenessError        string
	Current              bool
	IdentityMode         string
	UnfinishedClaimCount int
	ClaimCountState      string
	ClaimCountError      string
}

type claimantIdentityScan struct {
	Sessions     []claimantIdentityView
	StaleCount   int
	InvalidCount int
	Truncated    bool
}

// claimantIdentityDir is the single path both identity acquisition and
// read-only enumeration use. Keeping it in one place prevents the two sides
// from silently drifting when SAGE_HOME changes.
func claimantIdentityDir() (string, error) {
	home := strings.TrimSpace(os.Getenv("SAGE_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve SAGE home for claimant identity: %w", err)
		}
		home = filepath.Join(userHome, ".sage")
	}
	return filepath.Join(home, "runtime", "mcp-claimants"), nil
}

// claimantTransportKind reduces a transport scope to a stable, bounded label.
// The full scope can contain a bearer-derived fingerprint, which is not needed
// to choose a session.
func claimantTransportKind(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return "stdio"
	}
	kind := scope
	if i := strings.IndexByte(scope, ':'); i > 0 {
		kind = scope[:i]
	}
	if len(kind) > 32 {
		kind = kind[:32]
	}
	return kind
}

// claimantIdentityIsLive probes the same OS advisory lock that acquisition
// uses. A successful lock acquisition means no runtime currently owns the
// identity, so the probe releases it immediately. The probe is necessarily
// brief; its only purpose is a read-only diagnostic, never ownership.
func claimantIdentityIsLive(lockPath string) (bool, error) {
	info, err := os.Lstat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect claimant lock: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("claimant lock is not a regular file")
	}
	lease, acquired, err := tryLockClaimantFile(lockPath)
	if err != nil {
		return false, fmt.Errorf("probe claimant lock: %w", err)
	}
	if !acquired {
		return true, nil
	}
	_ = lease.Close()
	return false, nil
}

// listDurableClaimantIdentities enumerates the caller's own durable identities
// under this node's SAGE_HOME. The current session is always treated as live
// because this process is using it, including during an inherited runtime
// handoff where the parent may still hold the lock.
func listDurableClaimantIdentities(agentID, currentSessionID string) (claimantIdentityScan, error) {
	dir, err := claimantIdentityDir()
	if err != nil {
		return claimantIdentityScan{}, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return claimantIdentityScan{}, nil
	}
	if err != nil {
		return claimantIdentityScan{}, fmt.Errorf("read claimant identity directory: %w", err)
	}

	scan := claimantIdentityScan{}
	bySession := make(map[string]int)
	processed := 0
	for _, entry := range entries {
		if entry.IsDir() || !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		processed++
		if processed > maxClaimantIdentityFiles {
			scan.Truncated = true
			break
		}
		identityPath := filepath.Join(dir, entry.Name())
		raw, readErr := os.ReadFile(identityPath) //nolint:gosec // path is a directory entry from the private SAGE_HOME runtime directory
		if readErr != nil {
			scan.InvalidCount++
			continue
		}
		var record durableClaimantIdentity
		if json.Unmarshal(raw, &record) != nil || record.Version != 1 ||
			!validMCPClaimantSessionID(record.ClaimantSessionID) {
			scan.InvalidCount++
			continue
		}
		if record.AgentID != agentID {
			continue
		}
		view := claimantIdentityView{
			SessionID:    record.ClaimantSessionID,
			Provider:     record.Provider,
			Project:      record.Project,
			Transport:    claimantTransportKind(record.TransportScope),
			Liveness:     "live",
			Current:      record.ClaimantSessionID == currentSessionID,
			IdentityMode: "durable",
		}
		if record.CreatedAt != nil && !record.CreatedAt.IsZero() {
			view.CreatedAt = record.CreatedAt.UTC()
		} else if info, infoErr := entry.Info(); infoErr == nil {
			// Older identities did not persist a creation timestamp. The file
			// mtime is the best available historical fallback, not a claim of
			// exact process start time.
			view.CreatedAt = info.ModTime().UTC()
		}
		if view.Current {
			view.Live = true
		} else {
			lockPath := filepath.Join(dir, strings.TrimSuffix(entry.Name(), ".json")+".lock")
			live, liveErr := claimantIdentityIsLive(lockPath)
			if liveErr != nil {
				view.Liveness = "unknown"
				view.LivenessError = liveErr.Error()
			} else if !live {
				scan.StaleCount++
				continue
			} else {
				view.Live = true
			}
		}
		if existing, ok := bySession[view.SessionID]; ok {
			if view.Current && !scan.Sessions[existing].Current {
				scan.Sessions[existing] = view
			}
			continue
		}
		bySession[view.SessionID] = len(scan.Sessions)
		scan.Sessions = append(scan.Sessions, view)
	}

	sort.SliceStable(scan.Sessions, func(i, j int) bool {
		left, right := scan.Sessions[i], scan.Sessions[j]
		if left.Current != right.Current {
			return left.Current
		}
		if left.Provider != right.Provider {
			return left.Provider < right.Provider
		}
		if left.Project != right.Project {
			return left.Project < right.Project
		}
		return left.SessionID < right.SessionID
	})
	return scan, nil
}
