package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/l33tdawg/sage/internal/memory"
)

// Node-local storage for the optional memory gate (internal/voter.Gate).
//
// Nothing here is consensus state. Judgements are this node's verdicts on
// memories; review decisions are this node's operator's answers for memories
// the gate abstained on.
const writeGateSchema = `
	CREATE TABLE IF NOT EXISTS memory_judgements (
		memory_id     TEXT NOT NULL,
		check_name    TEXT NOT NULL,
		p_yes         REAL,
		verdict       TEXT NOT NULL,
		reason        TEXT NOT NULL DEFAULT '',
		judge_version TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		PRIMARY KEY (memory_id, check_name)
	);
	CREATE INDEX IF NOT EXISTS idx_memory_judgements_final
		ON memory_judgements(check_name, verdict);

	CREATE TABLE IF NOT EXISTS memory_review_decisions (
		memory_id  TEXT PRIMARY KEY,
		decision   TEXT NOT NULL CHECK (decision IN ('accept','reject')),
		decided_by TEXT NOT NULL DEFAULT '',
		note       TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);

`

func (s *SQLiteStore) migrateWriteGate(ctx context.Context) error {
	if _, err := s.writeExecContext(ctx, writeGateSchema); err != nil {
		return fmt.Errorf("create write-gate tables: %w", err)
	}
	return nil
}

// RecordJudgements stores (or replaces) the gate's verdicts for a memory.
func (s *SQLiteStore) RecordJudgements(ctx context.Context, js []memory.Judgement) error {
	for _, j := range js {
		var p any
		if j.PYes != nil {
			p = *j.PYes
		}
		created := j.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		if _, err := s.writeExecContext(ctx,
			`INSERT OR REPLACE INTO memory_judgements
				(memory_id, check_name, p_yes, verdict, reason, judge_version, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			j.MemoryID, j.Check, p, j.Verdict, j.Reason, j.JudgeVersion,
			created.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record judgement: %w", err)
		}
	}
	return nil
}

// GateFinal returns the gate's recorded overall outcome for a memory.
func (s *SQLiteStore) GateFinal(ctx context.Context, memoryID string) (verdict, reason string, ok bool, err error) {
	err = s.conn.QueryRowContext(ctx,
		`SELECT verdict, reason FROM memory_judgements
		 WHERE memory_id = ? AND check_name = 'final'`, memoryID).Scan(&verdict, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("gate final: %w", err)
	}
	return verdict, reason, true, nil
}

// GateJudgements lists every judgement recorded for a memory, for review.
func (s *SQLiteStore) GateJudgements(ctx context.Context, memoryID string) ([]memory.Judgement, error) {
	rows, err := s.conn.QueryContext(ctx,
		`SELECT memory_id, check_name, p_yes, verdict, reason, judge_version, created_at
		 FROM memory_judgements WHERE memory_id = ? ORDER BY check_name`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("gate judgements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []memory.Judgement
	for rows.Next() {
		var j memory.Judgement
		var p sql.NullFloat64
		var created string
		if err := rows.Scan(&j.MemoryID, &j.Check, &p, &j.Verdict, &j.Reason,
			&j.JudgeVersion, &created); err != nil {
			return nil, fmt.Errorf("gate judgements: %w", err)
		}
		if p.Valid {
			v := p.Float64
			j.PYes = &v
		}
		j.CreatedAt = parseTime(created)
		out = append(out, j)
	}
	return out, rows.Err()
}

// ReviewDecision returns the operator's accept/reject for a memory the gate
// abstained on.
func (s *SQLiteStore) ReviewDecision(ctx context.Context, memoryID string) (string, bool, error) {
	var d string
	err := s.conn.QueryRowContext(ctx,
		`SELECT decision FROM memory_review_decisions WHERE memory_id = ?`, memoryID).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("review decision: %w", err)
	}
	return d, true, nil
}

// ErrNotAwaitingReview means the memory is not in the review queue.
var ErrNotAwaitingReview = errors.New("memory is not awaiting review")

// SetReviewDecision records the operator's decision for an abstained memory.
// Only memories the gate abstained on can be decided, and a decision is final:
// once the voter acts on it the vote is on-chain.
func (s *SQLiteStore) SetReviewDecision(ctx context.Context, memoryID, decision, decidedBy, note string) error {
	if decision != memory.VerdictAccept && decision != memory.VerdictReject {
		return fmt.Errorf("decision must be %q or %q", memory.VerdictAccept, memory.VerdictReject)
	}
	verdict, _, ok, err := s.GateFinal(ctx, memoryID)
	if err != nil {
		return err
	}
	if !ok || verdict != memory.VerdictAbstain {
		return ErrNotAwaitingReview
	}
	res, err := s.writeExecContext(ctx,
		`INSERT OR IGNORE INTO memory_review_decisions (memory_id, decision, decided_by, note, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		memoryID, decision, decidedBy, note, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("set review decision: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotAwaitingReview // already decided
	}
	return nil
}

// ReviewItem is one memory waiting for an operator's decision.
type ReviewItem struct {
	MemoryID  string    `json:"memory_id"`
	Domain    string    `json:"domain_tag"`
	Content   string    `json:"content"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// ReviewQueue lists memories the gate abstained on that are still proposed and
// not yet decided, oldest first.
func (s *SQLiteStore) ReviewQueue(ctx context.Context, limit int) ([]ReviewItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.conn.QueryContext(ctx,
		`SELECT m.memory_id, m.domain_tag, m.content, j.reason, j.created_at
		 FROM memory_judgements AS j
		 JOIN memories AS m ON m.memory_id = j.memory_id
		 WHERE j.check_name = 'final' AND j.verdict = 'abstain'
		   AND m.status = 'proposed'
		   AND NOT EXISTS (SELECT 1 FROM memory_review_decisions AS d WHERE d.memory_id = j.memory_id)
		 ORDER BY j.created_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("review queue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReviewItem
	for rows.Next() {
		var it ReviewItem
		var created string
		if err := rows.Scan(&it.MemoryID, &it.Domain, &it.Content, &it.Reason, &created); err != nil {
			return nil, fmt.Errorf("review queue: %w", err)
		}
		if dec, decErr := s.decryptContent(it.Content); decErr == nil {
			it.Content = dec
		}
		it.CreatedAt = parseTime(created)
		out = append(out, it)
	}
	return out, rows.Err()
}
