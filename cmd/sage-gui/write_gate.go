package main

import (
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/l33tdawg/sage/internal/hunch"
	"github.com/l33tdawg/sage/internal/voter"
)

// writeGateFromEnv builds the optional memory gate (internal/voter.Gate) from
// the environment. It is OFF unless SAGE_HUNCH_URL is set.
//
//	SAGE_HUNCH_URL           base URL of a Hunch service (POST /v1/judge)
//	SAGE_HUNCH_API_KEY       bearer key for it, if the service requires one
//	SAGE_HUNCH_MODELS        comma-separated judge models, the FIRST leading
//	                         (empty = the service's default model, one judge)
//	SAGE_HUNCH_POLICY        "lead" (default: first judge decides, any other can
//	                         veto) or "all" (every judge must agree)
//	SAGE_HUNCH_INCLUDE_DOMAINS comma-separated domain prefixes to judge; when
//	                         set, ONLY these domains' memory content is sent
//	SAGE_HUNCH_EXEMPT_DOMAINS comma-separated domain prefixes never judged (e.g.
//	                         program-written catalogs); their content is not sent
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
	g := &voter.Gate{Timeout: timeout}
	for _, m := range models {
		g.Judges = append(g.Judges, hunch.LastingJudge{Client: hunch.New(url, key, m, timeout)})
	}
	g.Policy = strings.TrimSpace(os.Getenv("SAGE_HUNCH_POLICY"))
	g.ExemptDomainPrefixes = splitList(os.Getenv("SAGE_HUNCH_EXEMPT_DOMAINS"))
	g.IncludeDomainPrefixes = splitList(os.Getenv("SAGE_HUNCH_INCLUDE_DOMAINS"))
	policy := g.Policy
	if policy != voter.PolicyAll {
		policy = voter.PolicyLead
	}
	g.Version = hunch.ChecksVersion + "|" + policy + ":" + strings.Join(models, "+")
	scope := "every domain"
	if len(g.IncludeDomainPrefixes) > 0 {
		scope = "domains " + strings.Join(g.IncludeDomainPrefixes, ", ")
	}
	logger.Info().Str("hunch_url", url).Strs("judges", models).Str("policy", policy).
		Strs("include_domains", g.IncludeDomainPrefixes).Strs("exempt_domains", g.ExemptDomainPrefixes).
		Msg("memory gate ON — the CONTENT of proposed memories in " + scope +
			" (minus exempt domains) is sent to " + url + " to be judged before this node votes; no ids, authors or other metadata")
	return g
}

func splitList(raw string) []string {
	var out []string
	for _, d := range strings.Split(raw, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}
