package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
)

// SentinelDetail is one sentinel finding as the decision dialog and the
// hive's comment name it: the rule that fired, its one-line summary and the
// files it fired on. "Changes 1 sensitive path(s)" alone left the operator
// unable to decide ("knowing what the sensitive path(s) (and files) are in
// this alert dialog would really help me make a decision"), so the rows
// carry the paths themselves, bounded to SentinelDetailMaxPaths per finding
// with MorePaths counting the rest.
type SentinelDetail struct {
	Rule    string   `json:"rule"`
	Summary string   `json:"summary,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	// MorePaths is how many flagged paths were cut after the first
	// SentinelDetailMaxPaths; 0 when every path is listed.
	MorePaths int `json:"more_paths,omitempty"`
}

// SentinelDetailMaxPaths bounds the paths one finding lists in a tracker
// row, the dialog and the hive's comment.
const SentinelDetailMaxPaths = 10

// sentinelDetailOf builds the bounded detail for one finding; paths are
// sorted so the list is stable across snapshots.
func sentinelDetailOf(rule, summary string, paths []string) SentinelDetail {
	d := SentinelDetail{Rule: strings.TrimSpace(rule), Summary: strings.TrimSpace(summary)}
	sorted := make([]string, 0, len(paths))
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			sorted = append(sorted, p)
		}
	}
	sort.Strings(sorted)
	if len(sorted) > SentinelDetailMaxPaths {
		d.MorePaths = len(sorted) - SentinelDetailMaxPaths
		sorted = sorted[:SentinelDetailMaxPaths]
	}
	if len(sorted) > 0 {
		d.Paths = sorted
	}
	return d
}

// sentinelDetailsFromEvidence converts a review evidence bundle's sentinel
// findings for the tracker payload.
func sentinelDetailsFromEvidence(findings []evidence.SentinelFinding) []SentinelDetail {
	if len(findings) == 0 {
		return nil
	}
	out := make([]SentinelDetail, 0, len(findings))
	for _, f := range findings {
		out = append(out, sentinelDetailOf(f.Rule, f.Summary, f.Paths))
	}
	return out
}

// sentinelDetailsFromSweep converts the sweep event's own findings for the
// /api/sentinel ring (field access only: the dashboard does not import the
// sentinel package — see TestDashboardInternalImportCountRatchet).
func sentinelDetailsFromSweep(ev github.SentinelSweepEvent) []SentinelDetail {
	if len(ev.Findings) == 0 {
		return nil
	}
	out := make([]SentinelDetail, 0, len(ev.Findings))
	for _, f := range ev.Findings {
		out = append(out, sentinelDetailOf(f.Rule, f.Summary, f.Paths))
	}
	return out
}

// sentinelDetailsText renders the details on one line for a comment or a
// tooltip: "sensitive_path: .github/workflows/ci.yml, OWNERS (+3 more);
// ci_gate_weakening: .github/workflows/ci.yml". Findings without paths name
// the rule and its summary. Empty when there is nothing to say.
func sentinelDetailsText(details []SentinelDetail) string {
	parts := make([]string, 0, len(details))
	for _, d := range details {
		label := d.Rule
		if label == "" {
			label = d.Summary
		}
		if label == "" {
			continue
		}
		if len(d.Paths) == 0 {
			if d.Rule != "" && d.Summary != "" {
				label += " (" + d.Summary + ")"
			}
			parts = append(parts, label)
			continue
		}
		line := label + ": " + strings.Join(d.Paths, ", ")
		if d.MorePaths > 0 {
			line += fmt.Sprintf(" (+%d more)", d.MorePaths)
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "; ")
}

// sentinelDetailsFor returns the sentinel findings the hive holds for
// repo#number at head without touching GitHub: the /api/sentinel ring the
// sweep's audit hook fills. Nil when the sweep never saw that head.
func (s *Server) sentinelDetailsFor(repo string, number int, head string) []SentinelDetail {
	if s == nil || number <= 0 || strings.TrimSpace(head) == "" {
		return nil
	}
	return s.sentinelFindings.detailsFor(repo, number, head)
}

// sentinelDetailsForHead is sentinelDetailsFor with the on-disk review
// evidence bundle as the second source, for a request path that has not
// loaded the bundle itself (the delegated decision). The ring is in-memory
// and empties on restart; the bundle survives it.
func (s *Server) sentinelDetailsForHead(repo string, number int, head string) []SentinelDetail {
	head = strings.TrimSpace(head)
	if details := s.sentinelDetailsFor(repo, number, head); len(details) > 0 {
		return details
	}
	if s == nil || number <= 0 || head == "" {
		return nil
	}
	verifier, err := s.reviewEvidenceVerifier()
	if err != nil {
		return nil
	}
	b, err := github.LoadVerifiedReviewEvidence(github.ReviewEvidencePath("", repo, number, head), verifier)
	if err != nil || b == nil {
		return nil
	}
	return sentinelDetailsFromEvidence(b.Sentinel)
}
