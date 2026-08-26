package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PreCompact capture — the recall-backed-compaction write path.
//
// Claude Code (and Codex) fire a PreCompact hook right before a session is
// summarised by FULL compaction. The shipped hook only NUDGES the model to
// reflect/remember, so per-turn detail is discarded. This subcommand instead
// reads the transcript and writes the not-yet-captured conversation to SAGE as
// a small number of chunked, tagged observations, so the shipped SessionStart
// recall can bring any dropped turn back.
//
// Design notes:
//   - Chunked, not per-turn: one memory per ~chunk of turns bounds a compaction
//     to a handful of signed submits, not thousands. Content already lives in
//     the projection (consensus stores only content_hash+status), so this adds
//     no AppHash bytes and no embedding compute in FinalizeBlock.
//   - Conversation turns are evicted only by full compaction (this hook).
//     Micro-compaction only clears re-derivable tool_result payloads, which the
//     parser skips — so this is lossless for the conversation.
//   - Soft-fail everywhere: a capture failure must never block compaction.

const (
	preCompactMaxContentBytes = 200_000                 // per record, well under the 1 MiB store cap
	preCompactDefaultChunkB   = 6_000                   // ~1.5k tokens/chunk; embedder-friendly
	preCompactMaxChunkB       = 60_000                  // hard ceiling on the env-tunable chunk size
	preCompactDefaultMaxTurns = 200                     // safe per-invocation default, bounded to the hook budget
	preCompactMaxTurnsCeiling = 2_000                   // hard ceiling on the env-tunable turn cap
	preCompactBudget          = 3500 * time.Millisecond // stay well under the 5s installed-hook budget
	preCompactMaxTotalBytes   = 1 << 20                 // total verbatim bytes submitted per invocation
	preCompactDefaultClass    = 2                       // Confidential — own org + explicit cross-org grants (never Public)
)

type preCompactInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Trigger        string `json:"trigger"`
}

type transcriptRow struct {
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type capturedTurn struct {
	Role string
	Text string
}

// runHookPreCompact captures the evicted conversation into SAGE. Always returns
// nil (soft-fail): compaction proceeds regardless of any error here.
func runHookPreCompact() error {
	// Durable verbatim transcript retention requires EXPLICIT consent. Default OFF
	// (opt-in): capture runs only when SAGE_NEVERCOMPACT is explicitly enabled.
	if !neverCompactEnabled() {
		return nil
	}

	var in preCompactInput
	if raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 256<<10)); len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &in)
	}
	if in.TranscriptPath == "" {
		fmt.Fprintln(os.Stderr, "pre-compact: no transcript_path in hook payload; nothing to capture")
		return nil
	}
	sessionID := strings.TrimSpace(in.SessionID)
	if sessionID == "" {
		sessionID = "unknown-session"
	}

	turns, err := loadTranscriptTurns(in.TranscriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pre-compact: read transcript: %v\n", err)
		return nil
	}

	// State is keyed by transcript path, not session_id: the transcript file is
	// stable across --resume/--continue (so no re-capture), while a fork gets a
	// new file (so it captures cleanly). session_id can mint fresh on resume, so
	// keying on it would re-capture the whole conversation.
	hw := readPreCompactHighWater(in.TranscriptPath)
	if hw > len(turns) {
		// Transcript shrank/rotated below the mark. Recapturing from 0 can create
		// duplicate governed records — crash-safe, lifecycle-reconciled idempotency
		// is a required follow-up before this is mergeable (see RFC #275).
		hw = 0
	}
	pending := turns[hw:]
	if len(pending) == 0 {
		return nil
	}
	if len(pending) > preCompactMaxTurns() {
		pending = pending[:preCompactMaxTurns()] // oldest-first; rest wait for next PreCompact
	}

	domain, err := preCompactHomeDomain()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pre-compact: resolve home domain: %v\n", err)
		return nil
	}

	chunks := chunkTurns(pending, preCompactChunkBytes())
	// Bound the whole capture to a wall-clock budget and a total-byte cap so it can
	// never blow the installed hook's 5s window or submit an unbounded number of
	// records. Whatever doesn't fit is deferred to the next PreCompact.
	deadline := time.Now().Add(preCompactBudget)
	turnsWritten, chunksWritten, bytesWritten := 0, 0, 0
	baseSeq := hw
	stopped := ""
	for _, ch := range chunks {
		if time.Now().After(deadline) {
			stopped = "time budget"
			break
		}
		chBytes := chunkByteLen(ch)
		if bytesWritten > 0 && bytesWritten+chBytes > preCompactMaxTotalBytes {
			stopped = "byte budget"
			break
		}
		if !submitCapturedChunk(domain, sessionID, baseSeq+turnsWritten, ch) {
			stopped = "submit error"
			break // high-water advances only over written chunks
		}
		turnsWritten += len(ch)
		bytesWritten += chBytes
		chunksWritten++
	}

	writePreCompactHighWater(in.TranscriptPath, hw+turnsWritten)
	msg := fmt.Sprintf("pre-compact: captured %d turns in %d chunks (of %d pending) → domain %s, thread %s",
		turnsWritten, chunksWritten, len(pending), domain, sessionID)
	if stopped != "" && turnsWritten < len(pending) {
		msg += fmt.Sprintf("; stopped on %s, %d turns deferred to next PreCompact", stopped, len(pending)-turnsWritten)
	}
	fmt.Fprintln(os.Stderr, msg)
	return nil
}

// chunkTurns groups consecutive turns into chunks whose combined text stays
// under maxBytes (a single over-large turn becomes its own chunk). Chunks are
// sub-slices of turns — read-only, no per-chunk allocation.
func chunkTurns(turns []capturedTurn, maxBytes int) [][]capturedTurn {
	chunks := make([][]capturedTurn, 0, len(turns))
	start, curBytes := 0, 0
	for i, t := range turns {
		tb := len(t.Text) + len(t.Role) + 16 // + header overhead
		if i > start && curBytes+tb > maxBytes {
			chunks = append(chunks, turns[start:i])
			start, curBytes = i, 0
		}
		curBytes += tb
	}
	if start < len(turns) {
		chunks = append(chunks, turns[start:])
	}
	return chunks
}

// loadTranscriptTurns parses a Claude-family transcript JSONL into capturable
// turns in order. tool_use / tool_result payloads are skipped: they are
// re-derivable (re-running the tool is cheaper and fresher than storing the
// blob), which is also exactly what micro-compaction clears.
func loadTranscriptTurns(path string) ([]capturedTurn, error) {
	data, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // path from trusted hook payload
	if err != nil {
		return nil, err
	}
	var turns []capturedTurn
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row transcriptRow
		if json.Unmarshal([]byte(line), &row) != nil || row.Message == nil {
			continue
		}
		role, text := extractTurnText(row.Message.Role, row.Message.Content)
		if role == "" {
			continue
		}
		if len(text) > preCompactMaxContentBytes {
			text = text[:preCompactMaxContentBytes] + "\n…[truncated]"
		}
		turns = append(turns, capturedTurn{Role: role, Text: text})
	}
	return turns, nil
}

// extractTurnText returns (role, text) for a capturable user/assistant turn, or
// ("", "") to skip. content is either a JSON string or an array of typed blocks.
func extractTurnText(role string, content json.RawMessage) (string, string) {
	if role != "user" && role != "assistant" {
		return "", ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		if s = strings.TrimSpace(s); s == "" {
			return "", ""
		}
		return role, s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return "", ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
		// tool_use / tool_result: skip (re-derivable)
	}
	if len(parts) == 0 {
		return "", ""
	}
	return role, strings.Join(parts, "\n")
}

// submitCapturedChunk writes one chunk of verbatim turns as an observation,
// tagged for the session thread, into the caller's OWN home domain (a
// per-session domain would be write-accepted but read-denied). Returns true on
// success.
func submitCapturedChunk(domain, sessionID string, startSeq int, chunk []capturedTurn) bool {
	var b strings.Builder
	fmt.Fprintf(&b, "[nevercompact] thread:%s seq:%d..%d\n", sessionID, startSeq, startSeq+len(chunk)-1)
	for i, t := range chunk {
		fmt.Fprintf(&b, "\n--- %s (seq %d) ---\n%s", t.Role, startSeq+i, t.Text)
	}
	content := b.String()
	if len(content) > preCompactMaxContentBytes {
		content = content[:preCompactMaxContentBytes] + "\n…[truncated]"
	}
	body, _ := json.Marshal(map[string]any{
		"content":          content,
		"memory_type":      "observation",
		"domain_tag":       domain,
		"confidence_score": 0.8,
		// Verbatim transcript text can contain credentials, private instructions,
		// and tool-derived secrets, so it is never stored Public (0). Defaults to
		// Confidential; operator-tunable, clamped to [Internal..TopSecret].
		"classification": neverCompactClassification(),
		"tags":           []string{"nevercompact", "thread:" + sessionID},
	})
	if _, err := hookSignedRequest(http.MethodPost, "/v1/memory/submit", body); err != nil {
		fmt.Fprintf(os.Stderr, "pre-compact: submit chunk @seq %d: %v\n", startSeq, err)
		return false
	}
	return true
}

// preCompactHomeDomain resolves the caller's home domain (the domain the shipped
// SessionStart recall reads), so captured chunks are recallable by that hook.
func preCompactHomeDomain() (string, error) {
	var self struct {
		HomeDomain string `json:"home_domain"`
	}
	if err := hookSignedJSON(http.MethodGet, "/v1/agent/me", nil, &self); err != nil {
		return "", err
	}
	domain := strings.TrimSpace(self.HomeDomain)
	if domain == "" {
		return "", fmt.Errorf("caller has no home domain")
	}
	return domain, nil
}

func preCompactChunkBytes() int {
	n := preCompactDefaultChunkB
	if v := strings.TrimSpace(os.Getenv("SAGE_NEVERCOMPACT_CHUNK_BYTES")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			n = p
		}
	}
	if n > preCompactMaxChunkB {
		n = preCompactMaxChunkB
	}
	return n
}

// neverCompactEnabled reports whether the operator has explicitly opted in to
// durable verbatim transcript capture. Default OFF: capturing a verbatim
// conversation to durable governed storage requires explicit consent.
func neverCompactEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SAGE_NEVERCOMPACT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// neverCompactClassification returns the clearance level captured chunks are
// stored at. Verbatim transcript text can contain secrets, so it is never
// Public (0); defaults to Confidential (2), operator-tunable within
// [Internal(1)..TopSecret(4)].
func neverCompactClassification() int {
	c := preCompactDefaultClass
	if v := strings.TrimSpace(os.Getenv("SAGE_NEVERCOMPACT_CLASSIFICATION")); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c = p
		}
	}
	if c < 1 {
		c = 1
	}
	if c > 4 {
		c = 4
	}
	return c
}

// chunkByteLen estimates the submitted size of a chunk, for the total-byte budget.
func chunkByteLen(chunk []capturedTurn) int {
	n := 48 // header overhead
	for _, t := range chunk {
		n += len(t.Text) + len(t.Role) + 24
	}
	return n
}

func preCompactMaxTurns() int {
	n := preCompactDefaultMaxTurns
	if v := strings.TrimSpace(os.Getenv("SAGE_NEVERCOMPACT_MAX_TURNS")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			n = p
		}
	}
	if n > preCompactMaxTurnsCeiling {
		n = preCompactMaxTurnsCeiling
	}
	return n
}

// preCompactHighWaterPath derives a stable state-file path from the transcript
// path (hashed, so any filesystem path maps to one safe filename).
func preCompactHighWaterPath(transcriptPath string) string {
	sum := sha256.Sum256([]byte(transcriptPath))
	return filepath.Join(SageHome(), ".nevercompact", hex.EncodeToString(sum[:])[:32]+".hw")
}

func readPreCompactHighWater(transcriptPath string) int {
	data, err := os.ReadFile(preCompactHighWaterPath(transcriptPath))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func writePreCompactHighWater(transcriptPath string, n int) {
	path := preCompactHighWaterPath(transcriptPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "pre-compact: state dir: %v\n", err)
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "pre-compact: write high-water: %v\n", err)
	}
}
