// Package abci — v7.5 snapshot scheduler.
//
// The scheduler decides when Commit should fire internal/snapshot.Take and
// kicks the actual work off on a goroutine so block production isn't
// blocked by serialization to disk. Triggers:
//
//   - height-based: every N committed blocks (default 10_000)
//   - time-based:   when at least M hours have passed since the last
//     successful snapshot (default 6h)
//   - idle-flush:   the time-based check only runs from Commit ticks, so
//     once the chain goes quiet (post-app-v12, issue #40, an idle chain
//     stops minting blocks) the last burst of writes would never be
//     snapshotted. A wall-clock goroutine (started lazily on the first
//     Tick) re-checks the TimeInterval every ~10 minutes and fires when
//     blocks were committed since the last snapshot.
//   - operator-explicit: SnapshotScheduler.Trigger(reason) is exported so
//     the upgrade-watchdog can demand a snapshot immediately before an
//     upgrade activation height.
//
// Concurrency model:
//   - Commit calls Tick(height, appHash) synchronously. Tick takes a
//     single Mutex, decides whether to fire, and returns. If it fires,
//     a goroutine runs snapshot.Take outside the Commit critical path.
//   - inFlight (atomic) guards against concurrent Take invocations —
//     only one snapshot at a time. Subsequent Tick calls see inFlight
//     and skip until the running goroutine finishes.
//   - The idle-flush goroutine reads/writes scheduler state under the
//     same Mutex and routes its firing through Trigger, so it shares the
//     inFlight coalescing with Tick. Close stops it.
//   - The scheduler holds a reference to the live *badger.DB. Take is
//     invoked with Options.LiveBadger set so it doesn't try to reopen
//     the directory.
package abci

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/rs/zerolog"

	"github.com/l33tdawg/sage/internal/snapshot"
)

// defaultSnapshotKeepLast is the retention default applied when
// SnapshotSchedulerConfig.KeepLast is unset (<=0). After each successful
// Take the scheduler keeps the K newest snapshots, plus one anchor per
// distinct binary version (anchors are never pruned, so downgrade stays
// possible regardless of K).
const defaultSnapshotKeepLast = 5

// defaultIdleFlushCheckInterval is how often the wall-clock idle-flush
// goroutine re-evaluates the TimeInterval cadence. Coarse on purpose: the
// loop exists only so a chain that went quiet still gets its final writes
// snapshotted within ~TimeInterval+10m, not to tighten the cadence.
const defaultIdleFlushCheckInterval = 10 * time.Minute

// SnapshotSchedulerConfig is the operator-tunable surface. Zero values
// resolve to sane defaults inside NewSnapshotScheduler. The scheduler is
// disabled if both HeightInterval == 0 and TimeInterval == 0.
type SnapshotSchedulerConfig struct {
	// DataDir is the SAGE chain data directory (where badger/, sage.db,
	// cometbft/ live). Snapshots write to DataDir/snapshots/<height>/.
	DataDir string

	// BinaryVersion is the running binary's version string, recorded
	// in the manifest. The launcher's anchor selection keys off this.
	BinaryVersion string

	// VaultKeyPath, if non-empty, is the vault.key file included in the
	// snapshot's config tarball so a fresh-boot or rollback can decrypt
	// existing memories.
	VaultKeyPath string

	// VaultEncrypted and VaultPassphrase wrap the snapshot's chunks in
	// the v6.8.0 envelope when VaultEncrypted is true.
	VaultEncrypted  bool
	VaultPassphrase string

	// HeightInterval fires a snapshot every N committed blocks. <=0
	// disables height-based snapshots.
	HeightInterval int64

	// TimeInterval fires a snapshot when at least this much wall time
	// has passed since the last successful one. <=0 disables.
	TimeInterval time.Duration

	// KeepLast bounds snapshot retention: after each successful Take the
	// scheduler prunes all but the K newest snapshots, plus one anchor per
	// distinct binary version (anchors are never pruned, so a downgrade stays
	// possible regardless of K). <=0 resolves to defaultSnapshotKeepLast.
	// Off the consensus path — pruning only touches DataDir/snapshots/.
	KeepLast int

	// LiveBadger is the live BadgerDB handle the running node holds.
	// Required when HeightInterval or TimeInterval is non-zero so the
	// scheduler can call (*badger.DB).Backup without lockfile conflict.
	LiveBadger *badger.DB
}

// SnapshotScheduler coordinates Commit-tail snapshot triggers.
type SnapshotScheduler struct {
	cfg    SnapshotSchedulerConfig
	logger zerolog.Logger

	mu         sync.Mutex
	lastHeight int64     // height of the last SUCCESSFUL snapshot
	lastTime   time.Time // wall time of the last successful snapshot
	// lastTickHeight / lastTickAppHash describe the newest committed block
	// Tick has seen — the candidate the idle-flush loop snapshots when the
	// chain goes quiet. lastTickAppHash is a private copy (Tick never stores
	// the caller's buffer).
	lastTickHeight  int64
	lastTickAppHash []byte
	idleLoopStarted bool

	// idleCheckEvery is the idle-flush poll cadence, resolved from
	// defaultIdleFlushCheckInterval in NewSnapshotScheduler. Tests may
	// shorten it before the first Tick (the loop starts lazily there).
	idleCheckEvery time.Duration
	stopIdle       chan struct{}
	stopOnce       sync.Once

	inFlight atomic.Bool
}

// NewSnapshotScheduler builds a scheduler from cfg + logger. Returns nil
// if both HeightInterval and TimeInterval are zero/negative (disabled).
// Returns nil if LiveBadger is nil — the scheduler refuses to run
// against an unmounted handle.
func NewSnapshotScheduler(cfg SnapshotSchedulerConfig, logger zerolog.Logger) *SnapshotScheduler {
	if cfg.HeightInterval <= 0 && cfg.TimeInterval <= 0 {
		return nil
	}
	if cfg.LiveBadger == nil {
		return nil
	}
	if cfg.DataDir == "" || cfg.BinaryVersion == "" {
		return nil
	}
	if cfg.KeepLast <= 0 {
		cfg.KeepLast = defaultSnapshotKeepLast
	}
	s := &SnapshotScheduler{
		cfg:            cfg,
		logger:         logger.With().Str("component", "snapshot-scheduler").Logger(),
		lastTime:       time.Now(),
		idleCheckEvery: defaultIdleFlushCheckInterval,
		stopIdle:       make(chan struct{}),
	}
	s.logger.Info().
		Int64("height_interval", cfg.HeightInterval).
		Dur("time_interval", cfg.TimeInterval).
		Int("keep_last", cfg.KeepLast).
		Str("data_dir", cfg.DataDir).
		Str("binary_version", cfg.BinaryVersion).
		Bool("encrypted", cfg.VaultEncrypted).
		Msg("snapshot scheduler armed")
	return s
}

// Tick is called from app.Commit after SaveState succeeds. It is fast and
// non-blocking: the decision is taken under a short mutex, and any
// firing is dispatched to a goroutine. height and appHash describe the
// block we just committed.
func (s *SnapshotScheduler) Tick(height int64, appHash []byte) {
	if s == nil {
		return
	}

	// Copy appHash so neither the fired goroutine nor the idle-flush loop
	// shares the buffer the caller retains.
	ahCopy := make([]byte, len(appHash))
	copy(ahCopy, appHash)

	s.mu.Lock()
	s.lastTickHeight = height
	s.lastTickAppHash = ahCopy
	// Lazily arm the wall-clock idle-flush fallback: the TimeInterval check
	// below only ever runs from a Commit tick, so without this loop a chain
	// that goes quiet (post-app-v12 an idle chain mints no blocks, issue
	// #40) would leave its final burst of writes un-snapshotted forever.
	startIdleLoop := s.cfg.TimeInterval > 0 && !s.idleLoopStarted
	if startIdleLoop {
		s.idleLoopStarted = true
	}
	fire := s.shouldFireLocked(height)
	s.mu.Unlock()

	if startIdleLoop {
		go s.idleFlushLoop()
	}
	if !fire {
		return
	}
	if !s.inFlight.CompareAndSwap(false, true) {
		// Previous snapshot still running — skip this tick.
		return
	}

	go s.runTake(height, ahCopy, "scheduled")
}

// Trigger forces a snapshot regardless of cadence. Intended for the
// upgrade watchdog's pre-upgrade snapshot. Returns immediately; the
// snapshot runs on a goroutine. If a snapshot is already in flight the
// call is a no-op (the watchdog can poll inFlight or just retry).
func (s *SnapshotScheduler) Trigger(height int64, appHash []byte, reason string) {
	if s == nil {
		return
	}
	if !s.inFlight.CompareAndSwap(false, true) {
		s.logger.Warn().Int64("height", height).Str("reason", reason).Msg("snapshot trigger skipped: another snapshot in flight")
		return
	}
	ahCopy := make([]byte, len(appHash))
	copy(ahCopy, appHash)
	go s.runTake(height, ahCopy, reason)
}

// shouldFireLocked consults the cadence config. Caller holds s.mu.
func (s *SnapshotScheduler) shouldFireLocked(height int64) bool {
	if s.cfg.HeightInterval > 0 && (height-s.lastHeight) >= s.cfg.HeightInterval {
		return true
	}
	if s.cfg.TimeInterval > 0 && time.Since(s.lastTime) >= s.cfg.TimeInterval {
		return true
	}
	return false
}

// idleFlushLoop is the wall-clock fallback for the TimeInterval cadence.
// Tick only runs from Commit, so once the chain stops producing blocks the
// 6h timer can never fire from a tick and the writes committed since the
// last snapshot would stay un-snapshotted indefinitely. The loop wakes
// every idleCheckEvery and triggers a snapshot iff BOTH hold:
//
//   - TimeInterval has elapsed since the last successful snapshot, AND
//   - at least one block was Tick'd after the last snapshotted height —
//     so it never fires when nothing changed since the last snapshot.
//
// Once the idle-flush succeeds, lastHeight catches up to lastTickHeight and
// the loop goes dormant until new blocks arrive: at most one snapshot per
// idle period. Firing routes through Trigger, sharing the inFlight
// coalescing with Tick. Runs until Close.
func (s *SnapshotScheduler) idleFlushLoop() {
	ticker := time.NewTicker(s.idleCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopIdle:
			return
		case <-ticker.C:
		}

		s.mu.Lock()
		due := s.cfg.TimeInterval > 0 && time.Since(s.lastTime) >= s.cfg.TimeInterval
		height := s.lastTickHeight
		hasNewBlocks := height > s.lastHeight
		appHash := s.lastTickAppHash // private copy made by Tick; never mutated
		s.mu.Unlock()

		if !due || !hasNewBlocks {
			continue
		}
		// If a snapshot is in flight Trigger no-ops; the next wake re-checks
		// against the then-updated lastHeight, so a duplicate never fires.
		s.Trigger(height, appHash, "idle-flush")
	}
}

// Close stops the idle-flush goroutine (if it was started). It does not
// wait for an in-flight snapshot.Take to finish — Take is crash-safe by
// design (staging dir + OK sentinel; SweepStaging cleans partials on boot).
// Safe to call multiple times and on a nil scheduler.
func (s *SnapshotScheduler) Close() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopIdle) })
}

// runTake is the goroutine body. It calls snapshot.Take, updates the
// last-success markers on success, and clears inFlight unconditionally.
func (s *SnapshotScheduler) runTake(height int64, appHash []byte, reason string) {
	defer s.inFlight.Store(false)

	start := time.Now()
	s.logger.Info().Int64("height", height).Str("reason", reason).Msg("snapshot.Take starting")

	manifest, err := snapshot.Take(context.Background(), s.cfg.DataDir, height, appHash, reason, snapshot.Options{
		BinaryVersion:   s.cfg.BinaryVersion,
		VaultKeyPath:    s.cfg.VaultKeyPath,
		VaultEncrypted:  s.cfg.VaultEncrypted,
		VaultPassphrase: s.cfg.VaultPassphrase,
		IncludeBinary:   true,
		LiveBadger:      s.cfg.LiveBadger,
	})
	if err != nil {
		s.logger.Error().Err(err).Int64("height", height).Str("reason", reason).
			Dur("elapsed", time.Since(start)).Msg("snapshot.Take failed")
		return
	}

	s.mu.Lock()
	s.lastHeight = height
	s.lastTime = time.Now()
	s.mu.Unlock()

	s.logger.Info().
		Int64("height", manifest.Height).
		Str("reason", reason).
		Int("chunks", len(manifest.Chunks)).
		Str("dir", filepath.Join(s.cfg.DataDir, "snapshots")).
		Dur("elapsed", time.Since(start)).
		Msg("snapshot.Take complete")

	// Enforce retention now that a fresh snapshot is durable. Runs on this
	// same goroutine (still inside the inFlight guard), so it never overlaps
	// another Take.
	s.pruneRetention()
}

// pruneRetention enforces the KeepLast policy: prune all but the K newest
// snapshots, plus one anchor per binary version. snapshot.KeepLast uses
// idempotent RemoveAll and ignores in-progress .staging dirs, so this is
// safe to run concurrently with a boot-time prune. Errors are logged, not
// fatal — retention is best-effort disk housekeeping off the consensus path.
func (s *SnapshotScheduler) pruneRetention() {
	removed, err := snapshot.KeepLast(s.cfg.DataDir, s.cfg.KeepLast)
	if err != nil {
		s.logger.Warn().Err(err).Int("keep_last", s.cfg.KeepLast).
			Msg("snapshot retention (KeepLast) hit an error")
		return
	}
	if removed > 0 {
		s.logger.Info().Int("removed", removed).Int("keep_last", s.cfg.KeepLast).
			Msg("snapshot retention pruned old snapshots")
	}
}

// SetSnapshotScheduler installs s on the app. nil is allowed (disables
// scheduled snapshots without changing app behaviour). Safe to call once
// during boot, before the chain starts producing blocks; not safe to
// call concurrently with Commit.
func (app *SageApp) SetSnapshotScheduler(s *SnapshotScheduler) {
	app.snapshotScheduler = s
}
