package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/l33tdawg/sage/internal/memory"
)

// Node-local storage for the optional write gate (internal/voter.Gate).
//
// Nothing here is consensus state. Judgements are this node's annotations of
// memories; review decisions are this node's operator's answers for memories
// the gate abstained on; evidence is kept off-chain on purpose, so the material
// a memory was judged against can be removed from the node without touching
// the chain.
const writeGateSchema = `
	CREATE TABLE IF NOT EXISTS memory_judgements (
		memory_id     TEXT NOT NULL,
		check_name    TEXT NOT NULL,
		target_id     TEXT NOT NULL DEFAULT '',
		p_yes         REAL,
		verdict       TEXT NOT NULL,
		reason        TEXT NOT NULL DEFAULT '',
		judge_version TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		PRIMARY KEY (memory_id, check_name, target_id)
	);
	-- Serves the default-recall "is this memory superseded?" lookup.
	CREATE INDEX IF NOT EXISTS idx_memory_judgements_target
		ON memory_judgements(target_id, verdict);
	CREATE INDEX IF NOT EXISTS idx_memory_judgements_final
		ON memory_judgements(check_name, verdict);

	CREATE TABLE IF NOT EXISTS memory_review_decisions (
		memory_id  TEXT PRIMARY KEY,
		decision   TEXT NOT NULL CHECK (decision IN ('accept','reject')),
		decided_by TEXT NOT NULL DEFAULT '',
		note       TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS memory_evidence (
		memory_id  TEXT PRIMARY KEY,
		evidence   TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
`

// MaxEvidenceBytes bounds the evidence stored with one memory.
const MaxEvidenceBytes = 32 << 10

func (s *SQLiteStore) migrateWriteGate(ctx context.Context) error {
	if _, err := s.writeExecContext(ctx, writeGateSchema); err != nil {
		return fmt.Errorf("create write-gate tables: %w", err)
	}
	return nil
}

// GateNeighbours returns up to k committed memories in rec's domain, written
// by rec's own author, nearest to rec by embedding, excluding rec itself.
//
// Same author is a candidate rule, applied in code rather than asked of the
// judge. Measured on a real store: first-person memories from DIFFERENT
// authors ("my on-chain identity is ...", one record per agent) were judged as
// one subject with a changed value, and were 15 of 20 wrong supersede marks;
// restricting candidates to the same author removed all 15 and none of the
// correct marks.
func (s *SQLiteStore) GateNeighbours(ctx context.Context, rec *memory.MemoryRecord, k int) ([]*memory.MemoryRecord, error) {
	if rec == nil || len(rec.Embedding) == 0 || k <= 0 || rec.SubmittingAgent == "" {
		return nil, nil
	}
	got, err := s.QuerySimilar(ctx, rec.Embedding, QueryOptions{
		DomainTag:        rec.DomainTag,
		StatusFilter:     string(memory.StatusCommitted),
		TopK:             k + 1,
		SubmittingAgents: []string{rec.SubmittingAgent},
	})
	if err != nil {
		return nil, fmt.Errorf("gate neighbours: %w", err)
	}
	out := make([]*memory.MemoryRecord, 0, k)
	for _, m := range got {
		if m == nil || m.MemoryID == rec.MemoryID {
			continue
		}
		out = append(out, m)
		if len(out) == k {
			break
		}
	}
	return out, nil
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
				(memory_id, check_name, target_id, p_yes, verdict, reason, judge_version, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			j.MemoryID, j.Check, j.TargetID, p, j.Verdict, j.Reason, j.JudgeVersion,
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
		 WHERE memory_id = ? AND check_name = 'final' AND target_id = ''`, memoryID).Scan(&verdict, &reason)
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
		`SELECT memory_id, check_name, target_id, p_yes, verdict, reason, judge_version, created_at
		 FROM memory_judgements WHERE memory_id = ? ORDER BY check_name, target_id`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("gate judgements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []memory.Judgement
	for rows.Next() {
		var j memory.Judgement
		var p sql.NullFloat64
		var created string
		if err := rows.Scan(&j.MemoryID, &j.Check, &j.TargetID, &p, &j.Verdict, &j.Reason,
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
		 WHERE j.check_name = 'final' AND j.target_id = '' AND j.verdict = 'abstain'
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

// SetMemoryEvidence stores the evidence submitted with a memory, node-local.
func (s *SQLiteStore) SetMemoryEvidence(ctx context.Context, memoryID, evidence string) error {
	if len(evidence) > MaxEvidenceBytes {
		return fmt.Errorf("evidence is %d bytes; the maximum is %d", len(evidence), MaxEvidenceBytes)
	}
	stored, err := s.encryptContent(evidence)
	if err != nil {
		return fmt.Errorf("encrypt evidence: %w", err)
	}
	if _, err := s.writeExecContext(ctx,
		`INSERT OR REPLACE INTO memory_evidence (memory_id, evidence, created_at) VALUES (?, ?, ?)`,
		memoryID, stored, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("set evidence: %w", err)
	}
	return nil
}

// DeleteMemoryEvidence removes a memory's stored evidence from this node.
func (s *SQLiteStore) DeleteMemoryEvidence(ctx context.Context, memoryID string) error {
	if _, err := s.writeExecContext(ctx, `DELETE FROM memory_evidence WHERE memory_id = ?`, memoryID); err != nil {
		return fmt.Errorf("delete evidence: %w", err)
	}
	return nil
}

// MemoryEvidence returns the evidence stored with a memory.
func (s *SQLiteStore) MemoryEvidence(ctx context.Context, memoryID string) (string, bool, error) {
	var stored string
	err := s.conn.QueryRowContext(ctx,
		`SELECT evidence FROM memory_evidence WHERE memory_id = ?`, memoryID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get evidence: %w", err)
	}
	plain, err := s.decryptContent(stored)
	if err != nil {
		return "", false, fmt.Errorf("decrypt evidence: %w", err)
	}
	return plain, true, nil
}

// GateAnnotations is what default recall needs to know about a page of
// memories: which are superseded (and by which surviving memory), and each
// one's judged confidence where evidence was judged.
type GateAnnotations struct {
	SupersededBy     map[string]string
	JudgedConfidence map[string]float64
}

// gateClosureDepth bounds how far restatement and correction chains are
// followed for one recall page.
const gateClosureDepth = 8

// GateAnnotationsFor looks up the gate's annotations for a set of memory IDs.
//
// The judge only ever compares PAIRS; chains are closed here, in code:
//   - restatement: if X and Y were judged duplicates ("same as"), a correction
//     of either hides both — otherwise the restated copy of a corrected fact
//     stays live at full confidence;
//   - survivor: superseded_by points along the chain to the newest memory that
//     is not itself superseded (bounded by gateClosureDepth).
//
// Only edges whose SOURCE memory is committed count: a correction that was
// rejected, abstained on, or never landed must not hide what it claims to
// correct, and neither may a restatement that never landed.
func (s *SQLiteStore) GateAnnotationsFor(ctx context.Context, ids []string) (GateAnnotations, error) {
	out := GateAnnotations{SupersededBy: map[string]string{}, JudgedConfidence: map[string]float64{}}
	if len(ids) == 0 {
		return out, nil
	}

	// 1. Restatement classes: walk committed duplicate edges (either direction)
	// out from the requested IDs and union everything they touch.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == x {
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	union := func(a, b string) { parent[find(a)] = find(b) }
	for _, id := range ids {
		parent[id] = id
	}
	frontier := append([]string(nil), ids...)
	for depth := 0; depth < gateClosureDepth && len(frontier) > 0; depth++ {
		edges, err := s.gateEdges(ctx, "agrees", "duplicate", frontier)
		if err != nil {
			return out, err
		}
		var next []string
		for _, e := range edges {
			for _, n := range e {
				if _, seen := parent[n]; !seen {
					parent[n] = n
					next = append(next, n)
				}
			}
			union(e[0], e[1])
		}
		frontier = next
	}

	// 2. Direct corrections of any class member; the newest committed
	// correction of any member applies to the whole class.
	members := make([]string, 0, len(parent))
	for m := range parent {
		members = append(members, m)
	}
	sup, err := s.gateEdges(ctx, "replaces", "supersedes", members)
	if err != nil {
		return out, err
	}
	classCorrector := map[string]string{} // class root -> corrector
	for _, e := range sup {
		classCorrector[find(e[1])] = e[0] // edges arrive oldest first; the newest wins
	}

	// 3. Follow each correction to its survivor.
	survivor := func(start string) string {
		cur := start
		for i := 0; i < gateClosureDepth; i++ {
			nxt, cerr := s.gateCorrector(ctx, cur)
			if cerr != nil || nxt == "" {
				return cur
			}
			cur = nxt
		}
		return cur
	}
	for _, id := range ids {
		by, ok := classCorrector[find(id)]
		if !ok {
			continue
		}
		sv := survivor(by)
		if _, inClass := parent[sv]; sv != id && (!inClass || find(sv) != find(id)) {
			out.SupersededBy[id] = sv
		}
	}

	ph, args := sqlInList(ids)
	rows, err := s.conn.QueryContext(ctx,
		`SELECT memory_id, p_yes FROM memory_judgements
		 WHERE check_name = 'supported' AND p_yes IS NOT NULL AND memory_id IN (`+ph+`)`, args...)
	if err != nil {
		return out, fmt.Errorf("gate annotations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var p float64
		if err := rows.Scan(&id, &p); err != nil {
			return out, fmt.Errorf("gate annotations: %w", err)
		}
		out.JudgedConfidence[id] = p
	}
	return out, rows.Err()
}

// gateEdges returns (source, target) pairs of one judged relation touching any
// of ids (as target, or as source for the symmetric duplicate relation), whose
// source memory is committed. Oldest first.
func (s *SQLiteStore) gateEdges(ctx context.Context, check, verdict string, ids []string) ([][2]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := sqlInList(ids)
	where := `j.target_id IN (` + ph + `)`
	all := append([]any{check, verdict}, args...)
	if check == "agrees" {
		where = `(j.target_id IN (` + ph + `) OR j.memory_id IN (` + ph + `))`
		all = append(all, args...)
	}
	rows, err := s.conn.QueryContext(ctx,
		`SELECT j.memory_id, j.target_id
		 FROM memory_judgements AS j
		 JOIN memories AS m ON m.memory_id = j.memory_id
		 WHERE j.check_name = ? AND j.verdict = ? AND m.status = 'committed' AND `+where+`
		 ORDER BY j.created_at ASC`, all...)
	if err != nil {
		return nil, fmt.Errorf("gate edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out [][2]string
	for rows.Next() {
		var e [2]string
		if err := rows.Scan(&e[0], &e[1]); err != nil {
			return nil, fmt.Errorf("gate edges: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// gateCorrector returns the newest committed memory judged to supersede id.
func (s *SQLiteStore) gateCorrector(ctx context.Context, id string) (string, error) {
	var by string
	err := s.conn.QueryRowContext(ctx,
		`SELECT j.memory_id FROM memory_judgements AS j
		 JOIN memories AS m ON m.memory_id = j.memory_id
		 WHERE j.check_name = 'replaces' AND j.verdict = 'supersedes'
		   AND m.status = 'committed' AND j.target_id = ?
		 ORDER BY j.created_at DESC LIMIT 1`, id).Scan(&by)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return by, err
}

func sqlInList(ids []string) (string, []any) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return ph, args
}
