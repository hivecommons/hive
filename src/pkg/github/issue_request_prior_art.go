package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/priorart"
)

// PriorArtFinder is the search half of the prior-art gate
// (priorart.Searcher), as the issue relay sees it.
type PriorArtFinder func(ctx context.Context, issue priorart.Issue) priorart.Report

// PriorArtRefusalRecorder records a knowledge bead when the relay refuses a
// filing because the request already exists. ref is the matching item's URL
// or handle.
type PriorArtRefusalRecorder func(title, detail, agent, ref string)

// SetPriorArtFilingGate enforces priorart.FilingVerdictFor on issue requests
// the relay files for hive agents (hivecommons/hive#13879). gate is read on
// every request so a dashboard edit of runs.triage.prior_art takes effect
// without a client rebuild; a nil gate or finder leaves filing unchanged.
func (c *Client) SetPriorArtFilingGate(gate func() config.PriorArtGateConfig, find PriorArtFinder, record PriorArtRefusalRecorder) {
	if c == nil {
		return
	}
	c.priorArtFilingGate = gate
	c.priorArtFilingFind = find
	c.priorArtFilingRecord = record
}

// priorArtFilingVerdict applies the hive-agent filing rule to an issue
// request: DoNotFile refuses (naming the match) and records a knowledge
// bead; FileWithFindings requires a "Gap vs existing" section in the body
// and pre-attaches the findings note; FileAsIs files body unchanged.
// refusal is non-empty when the request must not be filed.
func (c *Client) priorArtFilingVerdict(ctx context.Context, req IssueRequest, body string) (newBody, refusal string) {
	if c == nil || c.priorArtFilingGate == nil || c.priorArtFilingFind == nil {
		return body, ""
	}
	gate := c.priorArtFilingGate()
	if !gate.IsEnabled() || !priorart.IsFeatureKind(req.Title, req.Labels, gate.EffectiveKinds()) {
		return body, ""
	}
	threshold := gate.EffectiveThreshold()
	report := c.priorArtFilingFind(ctx, priorart.Issue{Repo: req.Repo, Title: req.Title, Body: req.Body, Labels: req.Labels})
	switch priorart.FilingVerdictFor(report, threshold) {
	case priorart.DoNotFile:
		best := report.Findings[0]
		match := priorArtFindingName(best)
		if c.priorArtFilingRecord != nil {
			c.priorArtFilingRecord(
				"prior art: "+strings.TrimSpace(req.Title)+" not filed, already exists",
				fmt.Sprintf("%s asked the relay to file %q in %s; refused at %d%% confidence (threshold %d): already covered by %s %s.", req.Agent, req.Title, req.Repo, report.Confidence, threshold, best.Kind, match),
				req.Agent, firstNonEmptyString(best.URL, best.Ref, best.Title))
		}
		c.logger.Info("issue-request watcher: create refused, prior art matches",
			slog.String("repo", req.Repo), slog.String("agent", req.Agent),
			slog.Int("confidence", report.Confidence), slog.Int("threshold", threshold), slog.String("match", match))
		return body, fmt.Sprintf("not filed: prior art matches at %d%% confidence (threshold %d): %s %s; do not re-file — a knowledge bead recorded the refusal. If the existing item truly lacks something, file that gap with a \"## Gap vs existing\" section naming it", report.Confidence, threshold, best.Kind, match)
	case priorart.FileWithFindings:
		if !hasGapVsExistingSection(req.Body) {
			return body, fmt.Sprintf("not filed: prior art found at %d%% confidence (threshold %d); add a \"## Gap vs existing\" section to the body that states what the existing items do not cover, then re-submit. Template:\n\n%s", report.Confidence, threshold, priorart.RenderGapSection(report))
		}
		return strings.TrimRight(body, "\n") + "\n\n" + priorart.RenderFindingsNote(report), ""
	}
	return body, ""
}

// priorArtFindingName is the finding's number, title and URL for a refusal.
func priorArtFindingName(f priorart.Finding) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{f.Ref, quoteIfSet(f.Title), f.URL} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

func quoteIfSet(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	return fmt.Sprintf("%q", s)
}

func firstNonEmptyString(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// hasGapVsExistingSection reports whether body carries a "Gap vs existing"
// heading (markdown heading or bold line, any case).
func hasGapVsExistingSection(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		l := strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#*_ ")))
		if strings.HasPrefix(l, "gap vs existing") || strings.HasPrefix(l, "gap vs. existing") {
			return true
		}
	}
	return false
}
