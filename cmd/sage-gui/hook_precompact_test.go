package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTurnText(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		content  string
		wantRole string
		wantText string
	}{
		{"string content", "user", `"hello world"`, "user", "hello world"},
		{"empty string skipped", "user", `"   "`, "", ""},
		{"non-conversation role skipped", "system", `"x"`, "", ""},
		{"text blocks joined", "assistant",
			`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "assistant", "a\nb"},
		{"tool-only turn skipped", "assistant",
			`[{"type":"tool_use","name":"Bash","input":{}}]`, "", ""},
		{"tool_result-only turn skipped", "user",
			`[{"type":"tool_result","content":"big output"}]`, "", ""},
		{"text + tool_use keeps only text", "assistant",
			`[{"type":"text","text":"doing it"},{"type":"tool_use","name":"Bash"}]`, "assistant", "doing it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, text := extractTurnText(c.role, json.RawMessage(c.content))
			if role != c.wantRole || text != c.wantText {
				t.Fatalf("got (%q,%q), want (%q,%q)", role, text, c.wantRole, c.wantText)
			}
		})
	}
}

func TestChunkTurns(t *testing.T) {
	turns := []capturedTurn{
		{Role: "user", Text: strings.Repeat("a", 40)},
		{Role: "assistant", Text: strings.Repeat("b", 40)},
		{Role: "user", Text: strings.Repeat("c", 40)},
	}
	// maxBytes small enough that each turn (~56 incl. overhead) is its own chunk.
	chunks := chunkTurns(turns, 60)
	if len(chunks) != 3 {
		t.Fatalf("small maxBytes: got %d chunks, want 3", len(chunks))
	}
	// maxBytes large enough to hold all three.
	if got := len(chunkTurns(turns, 10_000)); got != 1 {
		t.Fatalf("large maxBytes: got %d chunks, want 1", got)
	}
	// an over-large single turn still becomes its own (one) chunk.
	big := []capturedTurn{{Role: "user", Text: strings.Repeat("z", 5000)}}
	if got := len(chunkTurns(big, 100)); got != 1 {
		t.Fatalf("oversize turn: got %d chunks, want 1", got)
	}
	// no turns → no chunks.
	if got := len(chunkTurns(nil, 100)); got != 0 {
		t.Fatalf("empty: got %d chunks, want 0", got)
	}
	// total turns are preserved across chunking.
	total := 0
	for _, ch := range chunkTurns(turns, 60) {
		total += len(ch)
	}
	if total != len(turns) {
		t.Fatalf("turns lost in chunking: got %d, want %d", total, len(turns))
	}
}

func TestLoadTranscriptTurns(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"first user turn"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"assistant reply"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"lots of output"}]}}`,
		`{"type":"summary","summary":"a meta row with no message"}`,
		``, // blank line
		`not valid json`,
		`{"type":"user","message":{"role":"user","content":"second user turn"}}`,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	turns, err := loadTranscriptTurns(path)
	if err != nil {
		t.Fatal(err)
	}
	// Expect exactly the 3 real conversation turns; tool-only, meta, blank and
	// invalid rows are all dropped.
	want := []capturedTurn{
		{Role: "user", Text: "first user turn"},
		{Role: "assistant", Text: "assistant reply"},
		{Role: "user", Text: "second user turn"},
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i, w := range want {
		if turns[i] != w {
			t.Fatalf("turn %d: got %+v, want %+v", i, turns[i], w)
		}
	}
}

func TestHighWaterKeyedByTranscriptPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SAGE_HOME", home)

	// distinct transcript paths get distinct state
	writePreCompactHighWater("/a/session-1.jsonl", 5)
	writePreCompactHighWater("/b/session-2.jsonl", 9)
	if got := readPreCompactHighWater("/a/session-1.jsonl"); got != 5 {
		t.Fatalf("path A: got %d, want 5", got)
	}
	if got := readPreCompactHighWater("/b/session-2.jsonl"); got != 9 {
		t.Fatalf("path B: got %d, want 9", got)
	}
	// unknown path starts at zero
	if got := readPreCompactHighWater("/never/seen.jsonl"); got != 0 {
		t.Fatalf("unknown path: got %d, want 0", got)
	}
}

func TestNeverCompactConsentDefaultOff(t *testing.T) {
	// unset → OFF (durable verbatim capture is opt-in)
	t.Setenv("SAGE_NEVERCOMPACT", "")
	if neverCompactEnabled() {
		t.Fatal("unset SAGE_NEVERCOMPACT must be OFF (opt-in consent)")
	}
	for _, off := range []string{"0", "no", "off", "false", "garbage"} {
		t.Setenv("SAGE_NEVERCOMPACT", off)
		if neverCompactEnabled() {
			t.Fatalf("%q must be OFF", off)
		}
	}
	for _, on := range []string{"1", "true", "TRUE", "yes", "on", "On"} {
		t.Setenv("SAGE_NEVERCOMPACT", on)
		if !neverCompactEnabled() {
			t.Fatalf("%q must be ON", on)
		}
	}
}

func TestNeverCompactClassificationNeverPublic(t *testing.T) {
	t.Setenv("SAGE_NEVERCOMPACT_CLASSIFICATION", "")
	if got := neverCompactClassification(); got != 2 {
		t.Fatalf("default classification: got %d, want 2 (Confidential)", got)
	}
	// explicit Public (0) or negative must clamp UP to Internal (1), never Public
	for _, v := range []string{"0", "-1"} {
		t.Setenv("SAGE_NEVERCOMPACT_CLASSIFICATION", v)
		if got := neverCompactClassification(); got != 1 {
			t.Fatalf("%q must clamp to 1 (never Public), got %d", v, got)
		}
	}
	// above TopSecret clamps to 4
	t.Setenv("SAGE_NEVERCOMPACT_CLASSIFICATION", "9")
	if got := neverCompactClassification(); got != 4 {
		t.Fatalf("9 must clamp to 4, got %d", got)
	}
	// a valid mid level passes through
	t.Setenv("SAGE_NEVERCOMPACT_CLASSIFICATION", "3")
	if got := neverCompactClassification(); got != 3 {
		t.Fatalf("3 must pass through, got %d", got)
	}
}

func TestKnobsClampToSafeCeilings(t *testing.T) {
	t.Setenv("SAGE_NEVERCOMPACT_MAX_TURNS", "999999")
	if got := preCompactMaxTurns(); got != preCompactMaxTurnsCeiling {
		t.Fatalf("max-turns override must clamp to %d, got %d", preCompactMaxTurnsCeiling, got)
	}
	t.Setenv("SAGE_NEVERCOMPACT_CHUNK_BYTES", "999999")
	if got := preCompactChunkBytes(); got != preCompactMaxChunkB {
		t.Fatalf("chunk-bytes override must clamp to %d, got %d", preCompactMaxChunkB, got)
	}
	// unset → safe defaults
	t.Setenv("SAGE_NEVERCOMPACT_MAX_TURNS", "")
	if got := preCompactMaxTurns(); got != preCompactDefaultMaxTurns {
		t.Fatalf("default max-turns: got %d, want %d", got, preCompactDefaultMaxTurns)
	}
}

func TestValidateTranscriptPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAGE_NEVERCOMPACT_TRANSCRIPT_ROOT", dir)

	// a regular file under the trusted root is accepted, returned as its abs path
	good := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(good, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := validateTranscriptPath(good); err != nil || got != good {
		t.Fatalf("valid path: got (%q, %v), want (%q, nil)", got, err, good)
	}

	// empty path
	if _, err := validateTranscriptPath(""); err == nil {
		t.Fatal("empty path must be rejected")
	}

	// a path outside the trusted root (a different temp dir)
	outside := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateTranscriptPath(outside); err == nil {
		t.Fatal("path outside the trusted root must be rejected")
	}

	// a directory is not a regular file
	if _, err := validateTranscriptPath(dir); err == nil {
		t.Fatal("a directory must be rejected")
	}

	// a symlink is refused (skip where the platform can't create one)
	link := filepath.Join(dir, "link.jsonl")
	if os.Symlink(good, link) == nil {
		if _, err := validateTranscriptPath(link); err == nil {
			t.Fatal("a symlink transcript must be rejected")
		}
	}
}
