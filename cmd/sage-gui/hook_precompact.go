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
	preCompactMaxContentBytes = 200_000 // per record, well under the 1 MiB store cap
	preCompactDefaultChunkB   = 6_000   // ~1.5k tokens/chunk; embedder-friendly
	preCompactDefaultMaxTurns = 2_000   // per-invocation ceiling; rest wait for next PreCompact
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
	if strings.TrimSpace(os.Getenv("SAGE_NEVERCOMPACT")) == "0" {
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
		hw = 0 // transcript truncated/rotated — recapture (server dedups identical content_hash)
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
	turnsWritten, chunksWritten := 0, 0
	baseSeq := hw
	for _, ch := range chunks {
		if !submitCapturedChunk(domain, sessionID, baseSeq+turnsWritten, ch) {
			break // stop at first failure; high-water advances only over written chunks
		}
		turnsWritten += len(ch)
		chunksWritten++
	}

	writePreCompactHighWater(in.TranscriptPath, hw+turnsWritten)
	fmt.Fprintf(os.Stderr, "pre-compact: captured %d turns in %d chunks (of %d pending) → domain %s, thread %s\n",
		turnsWritten, chunksWritten, len(pending), domain, sessionID)
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
		"tags":             []string{"nevercompact", "thread:" + sessionID},
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
	if v := strings.TrimSpace(os.Getenv("SAGE_NEVERCOMPACT_CHUNK_BYTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return preCompactDefaultChunkB
}

func preCompactMaxTurns() int {
	if v := strings.TrimSpace(os.Getenv("SAGE_NEVERCOMPACT_MAX_TURNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return preCompactDefaultMaxTurns
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
