package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/l33tdawg/sage/internal/store"
)

// runUpgradePublicMemoryAudit implements
// `sage-gui upgrade public-memory-audit`: a stopped-node, read-only census of
// the PUBLIC memories that the app-v28 activation stage must fold into its
// sparse index. It names every record the stage would refuse and every legacy
// record its quarantine would skip, so an operator can tell a small repairable
// set from a corpus-wide class before committing to the fork.
func runUpgradePublicMemoryAudit(args []string) error {
	fs := flag.NewFlagSet("upgrade public-memory-audit", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "SAGE data directory holding badger/ (default: the configured data_dir)")
	limit := fs.Int("limit", 100, "maximum defect records to print (counters always cover the whole corpus)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	resolvedDataDir := *dataDir
	if resolvedDataDir == "" {
		cfg, err := LoadConfig()
		if err != nil {
			return fmt.Errorf("load config (pass --data-dir to audit a specific node): %w", err)
		}
		resolvedDataDir = cfg.DataDir
	}
	badgerPath := filepath.Join(resolvedDataDir, "badger")
	if _, statErr := os.Stat(badgerPath); statErr != nil {
		return fmt.Errorf("no consensus database at %s (wrong --data-dir, or this node was never initialized)", badgerPath)
	}
	bs, err := openConsensusStoreForInspection(badgerPath)
	if err != nil {
		return err
	}
	defer func() { _ = bs.CloseBadger() }()

	fmt.Println("Scanning the PUBLIC memory corpus (read-only; this can take minutes on a large node)…")
	audit, err := inspectPublicMemoryCorpus(bs, *limit)
	if err != nil {
		return err
	}
	fmt.Printf("SAGE app-v28 public-memory envelope audit\n")
	fmt.Printf("  data dir : %s\n\n", resolvedDataDir)
	printPublicMemoryAudit(audit)
	if audit.Blocking > 0 {
		return fmt.Errorf("app-v28 activation would refuse %d public record(s)", audit.Blocking)
	}
	return nil
}

func inspectPublicMemoryCorpus(bs *store.BadgerStore, limit int) (*store.PublicMemoryAudit, error) {
	audit, err := bs.AuditPublicMemoryEnvelopes(limit)
	if err != nil {
		return nil, fmt.Errorf("audit public-memory envelopes: %w", err)
	}
	return audit, nil
}

func printPublicMemoryAudit(audit *store.PublicMemoryAudit) {
	fmt.Printf("Public-memory envelope audit (app-v28 activation prerequisite)\n")
	fmt.Printf("  memories scanned : %d\n", audit.Scanned)
	fmt.Printf("  public records   : %d\n", audit.Public)
	fmt.Printf("  blocking         : %d\n", audit.Blocking)
	fmt.Printf("  legacy quarantine: %d\n", audit.Quarantined)
	if len(audit.Defects) == 0 {
		fmt.Println("  defects          : none")
		fmt.Println()
		return
	}
	fmt.Println()
	for _, defect := range audit.Defects {
		state := "BLOCKING"
		if defect.Quarantined {
			state = "QUARANTINED"
		}
		fmt.Printf("  %s  %s  status=%s  reasons=%v\n",
			state, defect.MemoryID, defect.Status, defect.Reasons)
	}
	if audit.Truncated {
		fmt.Printf("  … defect list truncated; counters still describe the whole corpus\n")
	}
	fmt.Println()
}
