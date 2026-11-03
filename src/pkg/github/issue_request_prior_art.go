package github

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
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
// refusal is non-empty when the request must not be filed; match then
// names the top finding so the caller can audit the refusal.
func (c *Client) priorArtFilingVerdict(ctx context.Context, req IssueRequest, body string) (newBody, refusal string, match priorArtMatch) {
	if c == nil || c.priorArtFilingGate == nil || c.priorArtFilingFind == nil {
		return body, "", priorArtMatch{}
	}
	gate := c.priorArtFilingGate()
	if !gate.IsEnabled() || !priorart.IsFeatureKind(req.Title, req.Labels, gate.EffectiveKinds()) {
		return body, "", priorArtMatch{}
	}
	threshold := gate.EffectiveThreshold()
	report := c.priorArtFilingFind(ctx, priorart.Issue{Repo: req.Repo, Title: req.Title, Body: req.Body, Labels: req.Labels})
	switch priorart.FilingVerdictFor(report, threshold) {
	case priorart.DoNotFile:
		best := report.Findings[0]
		match = newPriorArtMatch(best, report.Confidence, threshold, "do_not_file")
		name := priorArtFindingName(best)
		kind := priorart.KindLabel(best.Kind)
		if c.priorArtFilingRecord != nil {
			c.priorArtFilingRecord(
				"prior art: "+strings.TrimSpace(req.Title)+" not filed, already exists",
				fmt.Sprintf("%s asked the relay to file %q in %s; refused at %d%% confidence (threshold %d): already covered by %s %s.", req.Agent, req.Title, req.Repo, report.Confidence, threshold, kind, name),
				req.Agent, match.Ref)
		}
		c.logger.Info("issue-request watcher: create refused, prior art matches",
			slog.String("repo", req.Repo), slog.String("agent", req.Agent),
			slog.Int("confidence", report.Confidence), slog.Int("threshold", threshold), slog.String("match", name))
		return body, fmt.Sprintf("not filed: prior art matches at %d%% confidence (threshold %d): %s %s; do not re-file — a knowledge bead recorded the refusal, and re-submitting this request (with or without a \"## Gap vs existing\" section) is refused the same way. If the existing item truly lacks something, open a separate, narrower request for that gap alone, titled and described by what is missing rather than by the existing item", report.Confidence, threshold, kind, name), match
	case priorart.FileWithFindings:
		if !hasGapVsExistingSection(req.Body) {
			match = newPriorArtMatch(report.Findings[0], report.Confidence, threshold, "missing_gap_section")
			return body, fmt.Sprintf("not filed: prior art found at %d%% confidence (threshold %d); add a \"## Gap vs existing\" section to the body that states what the existing items do not cover, then re-submit. Template:\n\n%s", report.Confidence, threshold, priorart.RenderGapSection(report)), match
		}
		return strings.TrimRight(body, "\n") + "\n\n" + priorart.RenderFindingsNote(report), "", priorArtMatch{}
	}
	return body, "", priorArtMatch{}
}

// priorArtMatch is the top finding behind a prior-art refusal, in the shape
// the write audit records it.
type priorArtMatch struct {
	// Ref is the match's URL, falling back to its handle or title.
	Ref        string
	URL        string
	Kind       string
	Confidence int
	Threshold  int
	// Verdict is why the request was refused: "do_not_file" (high match)
	// or "missing_gap_section" (medium match without a gap section).
	Verdict string
}

func newPriorArtMatch(f priorart.Finding, confidence, threshold int, verdict string) priorArtMatch {
	return priorArtMatch{
		Ref:        firstNonEmpty(f.URL, f.Ref, f.Title),
		URL:        strings.TrimSpace(f.URL),
		Kind:       string(f.Kind),
		Confidence: confidence,
		Threshold:  threshold,
		Verdict:    verdict,
	}
}

// auditPairs is the match as recordWriteAudit extra pairs.
func (m priorArtMatch) auditPairs() []string {
	return []string{
		"verdict", m.Verdict,
		"match", m.Ref,
		"url", m.URL,
		"kind", m.Kind,
		"confidence", strconv.Itoa(m.Confidence),
		"threshold", strconv.Itoa(m.Threshold),
	}
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
