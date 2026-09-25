package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/l33tdawg/sage/internal/hunch"
	"github.com/l33tdawg/sage/internal/voter"
)

// writeGateFromEnv builds the optional write gate (internal/voter.Gate) from
// the environment. It is OFF unless SAGE_HUNCH_URL is set.
//
//	SAGE_HUNCH_URL           base URL of a Hunch service (POST /v1/judge)
//	SAGE_HUNCH_API_KEY       bearer key for it, if the service requires one
//	SAGE_HUNCH_MODELS        comma-separated judge models; with two or more the
//	                         gate acts only when every judge agrees (empty = the
//	                         service's default model, one judge)
//	SAGE_HUNCH_NEIGHBOURS    committed neighbours compared per memory (default 5)
//	SAGE_HUNCH_DEDUP_REJECT  "1" to vote REJECT on semantic duplicates instead of
//	                         only marking them (default off)
//	SAGE_HUNCH_TIMEOUT       per-memory judge budget, e.g. "60s"
func writeGateFromEnv(logger zerolog.Logger) *voter.Gate {
	url := strings.TrimSpace(os.Getenv("SAGE_HUNCH_URL"))
	if url == "" {
		return nil
	}
	key := os.Getenv("SAGE_HUNCH_API_KEY")
	timeout := 60 * time.Second
	if raw := os.Getenv("SAGE_HUNCH_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			timeout = d
		} else {
			logger.Warn().Str("SAGE_HUNCH_TIMEOUT", raw).Msg("invalid write-gate timeout — using 60s")
		}
	}
	var models []string
	for _, m := range strings.Split(os.Getenv("SAGE_HUNCH_MODELS"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		models = []string{""}
	}
	g := &voter.Gate{Timeout: timeout, Neighbours: 5, DedupReject: os.Getenv("SAGE_HUNCH_DEDUP_REJECT") == "1"}
	for _, m := range models {
		g.Judges = append(g.Judges, hunch.New(url, key, m, timeout))
	}
	if raw := os.Getenv("SAGE_HUNCH_NEIGHBOURS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			g.Neighbours = n
		}
	}
	g.Version = hunch.ChecksVersion + "|" + strings.Join(models, "+")
	logger.Info().Str("hunch_url", url).Strs("judges", models).Int("neighbours", g.Neighbours).
		Bool("dedup_reject", g.DedupReject).
		Msg("memory write gate ON — proposed memories are judged before this node votes")
	return g
}
