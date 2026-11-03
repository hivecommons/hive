package ioscan

import (
	"fmt"
	"sort"
	"strings"
)

// Severity policy matrix (hivecommons/hive#6685, part 3).
//
// Before this, the input block policy was one fixed line: Critical anything,
// or High injection. Every Medium injection rule (role override, pretend
// act-as, reveal prompt, developer mode) and every High dangerous rule
// (disable guardrails, exfiltrate) was recorded and then handed to the agent
// unchanged — 19% of the red-team corpus. The matrix makes the line a
// per-surface setting and moves the default for text by an untrusted author
// down to Medium, so a rule that fires now withholds the text. Text by a
// trusted author (the hive's own identities, repo owners and members) keeps
// the old High line, because that is where the false-positive cost lands on
// people who can be asked.

// Surface names where a piece of untrusted text came from. The kick path
// tags every string it scans with one so the block threshold can differ by
// surface.
type Surface string

const (
	// SurfaceIssueBody covers an issue's title, body and labels.
	SurfaceIssueBody Surface = "issue_body"
	// SurfaceComment covers issue and PR conversation comments.
	SurfaceComment Surface = "comment"
	// SurfacePRBody covers a PR's title, body and author login.
	SurfacePRBody Surface = "pr_body"
	// SurfaceReview covers review bodies and inline review-thread comments.
	SurfaceReview Surface = "review"
)

// Surfaces lists every surface in a stable order.
var Surfaces = []Surface{SurfaceIssueBody, SurfaceComment, SurfacePRBody, SurfaceReview}

const (
	// DefaultUntrustedBlockAt is the default block threshold for injection
	// and dangerous findings in text by an untrusted author.
	DefaultUntrustedBlockAt = SeverityMedium
	// TrustedBlockAt is the block threshold for text by a trusted author:
	// the pre-matrix line, kept so a maintainer discussing "pretend you are
	// the reviewer" in their own issue is not redacted.
	TrustedBlockAt = SeverityHigh
)

// Policy is the per-surface block threshold for text by an untrusted author.
// A surface missing from BlockAt uses DefaultUntrustedBlockAt. The zero
// Policy is valid and equals DefaultPolicy().
type Policy struct {
	BlockAt map[Surface]Severity
}

// DefaultPolicy returns the shipped policy: every surface at
// DefaultUntrustedBlockAt.
func DefaultPolicy() Policy {
	p := Policy{BlockAt: make(map[Surface]Severity, len(Surfaces))}
	for _, s := range Surfaces {
		p.BlockAt[s] = DefaultUntrustedBlockAt
	}
	return p
}

// Threshold returns the severity at which an injection or dangerous finding
// withholds text from the given surface. Critical findings of any kind block
// regardless of the threshold; see blockedInputAt.
func (p Policy) Threshold(surface Surface, trustedAuthor bool) Severity {
	if trustedAuthor {
		return TrustedBlockAt
	}
	if sev, ok := p.BlockAt[surface]; ok {
		return sev
	}
	return DefaultUntrustedBlockAt
}

// String renders the matrix as "surface=severity, ..." for audit detail.
func (p Policy) String() string {
	parts := make([]string, 0, len(Surfaces))
	for _, s := range Surfaces {
		parts = append(parts, string(s)+"="+p.Threshold(s, false).String())
	}
	return strings.Join(parts, ", ")
}

// Source describes where a scanned string came from, so the policy can pick
// its threshold.
type Source struct {
	Surface       Surface
	TrustedAuthor bool
}

// ParseSeverity converts a config string ("low", "medium", "high",
// "critical", case-insensitive) to a Severity.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return SeverityLow, nil
	case "medium":
		return SeverityMedium, nil
	case "high":
		return SeverityHigh, nil
	case "critical":
		return SeverityCritical, nil
	}
	return 0, fmt.Errorf("ioscan: unknown severity %q (want low, medium, high or critical)", s)
}

// ParsePolicy builds a Policy from a surface→severity string map, as read
// from `ioscan.block_at` in the hive config. Unknown surfaces and severities
// are errors so a typo cannot silently leave a surface on the default.
func ParsePolicy(blockAt map[string]string) (Policy, error) {
	p := DefaultPolicy()
	keys := make([]string, 0, len(blockAt))
	for k := range blockAt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		surface := Surface(strings.ToLower(strings.TrimSpace(k)))
		if !knownSurface(surface) {
			return Policy{}, fmt.Errorf("ioscan: unknown block_at surface %q (want one of %s)", k, surfaceList())
		}
		sev, err := ParseSeverity(blockAt[k])
		if err != nil {
			return Policy{}, fmt.Errorf("ioscan: block_at.%s: %w", surface, err)
		}
		p.BlockAt[surface] = sev
	}
	return p, nil
}

func knownSurface(s Surface) bool {
	for _, known := range Surfaces {
		if s == known {
			return true
		}
	}
	return false
}

func surfaceList() string {
	names := make([]string, len(Surfaces))
	for i, s := range Surfaces {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

// blockedInputAt is the input block policy under a threshold: any Critical
// finding, or an injection or dangerous finding at or above threshold.
// Secrets below Critical never block input on their own; a token in an issue
// is a leak to report, not an instruction to the agent.
func blockedInputAt(findings []Finding, threshold Severity) bool {
	for _, f := range findings {
		if f.Severity >= SeverityCritical {
			return true
		}
		if (f.Kind == KindInjection || f.Kind == KindDangerous) && f.Severity >= threshold {
			return true
		}
	}
	return false
}

// ScanInputFrom is ScanInput with the block line taken from the policy for
// the text's source instead of the fixed legacy line.
func (s *Scanner) ScanInputFrom(text string, src Source, p Policy) Verdict {
	findings := s.collect(text)
	return Verdict{Findings: findings, Blocked: blockedInputAt(findings, p.Threshold(src.Surface, src.TrustedAuthor))}
}

// ScanInputFrom is the package-level convenience equivalent to
// NewScanner().ScanInputFrom.
func ScanInputFrom(text string, src Source, p Policy) Verdict {
	return NewScanner().ScanInputFrom(text, src, p)
}

// EnforceInputFrom is EnforceInput under the policy for the text's source.
func EnforceInputFrom(text string, src Source, p Policy) (sanitized string, v Verdict) {
	v = ScanInputFrom(text, src, p)
	if !v.Blocked {
		return text, v
	}
	return annotate(v), v
}
