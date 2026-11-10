package github

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// zeroFindingDigestMinInterval is the minimum time between two
	// zero-finding advisory digests on the SAME target. A digest reporting no
	// findings says nothing that a reader needs twice a day, and these
	// comments land on THIRD-PARTY repositories where every subscriber is
	// notified. 48h keeps the freshness signal alive (the hub's staleness gate
	// alarms well after this) while collapsing ~12 zero-finding posts a day
	// into one.
	zeroFindingDigestMinInterval = 48 * time.Hour

	// suppressReasonIdentical marks a digest suppressed because its material
	// content is byte-identical to the digest already on the target.
	suppressReasonIdentical = "identical"
	// suppressReasonZeroFindingCap marks a digest suppressed because it is a
	// zero-finding digest posted inside zeroFindingDigestMinInterval of the
	// previous zero-finding digest on the same target.
	suppressReasonZeroFindingCap = "zero_finding_cap"
	// suppressReasonMinInterval marks a digest whose material content DID
	// change but whose target was written less than the configured
	// digest_min_interval ago (#17435). The change is not lost: the next
	// cycle after the window re-renders and posts it.
	suppressReasonMinInterval = "min_interval"
)

// AuditActionAdvisoryDigestSuppressed is the audit action recorded when a
// digest post is suppressed. It is LOCAL-AUDIT-ONLY by design: posting a
// "suppressed" note to the issue would replace one kind of subscriber noise
// with another.
const AuditActionAdvisoryDigestSuppressed = "advisory_digest_suppressed"

// volatileDigestPatterns match the parts of a rendered digest that change on
// every render without the findings changing. They are stripped before
// fingerprinting so that "same findings, one minute later" hashes identically.
//
// Getting this exclusion right is the whole fix: if a single timestamp leaks
// into the fingerprint every digest is unique and the suppression silently
// does nothing — which is precisely how the #4818 guard failed.
var volatileDigestPatterns = []*regexp.Regexp{
	// Header stamp: "## 🐝 Advisory Digest — 2026-08-31 04:00 UTC".
	// Matched by shape, not by prefix, so a renamed heading still scrubs.
	regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}(:\d{2})?( [A-Za-z/_+\-0-9]{1,10})?`),
	// RFC3339 stamps: "evaluated 2026-08-31T04:00:11Z", finding timestamps,
	// and the analyzed-snapshot footer's date.
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+\-]\d{2}:\d{2})`),
	// Relative ages the renderer emits ("3h ago", "2 days ago"): these tick
	// upward every cycle while the underlying finding is unchanged.
	regexp.MustCompile(`\b\d+\s*(s|m|h|d|second|minute|hour|day|week|month)s?\s+ago\b`),
	// Commit-pin footer: the snapshot SHA advances with every upstream push,
	// which is repo churn rather than a change in what the agents found.
	regexp.MustCompile(`\b[0-9a-f]{7,40}\b`),
	// The upsert marker and its "Updated N times" footer (#17435): both are
	// bookkeeping about the comment, not about the findings.
	regexp.MustCompile(`<!-- hive:advisory-digest[^>]*-->`),
	regexp.MustCompile(`(?m)^_?Updated \d+ times?\b[^\n]*$`),
}

// materialDigestFingerprint reduces a rendered digest body to a stable hash of
// its MATERIAL content: the findings, their counts and their prose, with all
// timestamps, relative ages, commit pins and whitespace churn removed. Two
// digests rendered a day apart from an unchanged finding set fingerprint
// identically; a digest whose finding set changed does not.
func materialDigestFingerprint(body string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(materialDigestContent(body))))
}

// materialDigestContent is the normalized text materialDigestFingerprint
// hashes. Split out so tests can assert on the normalization directly rather
// than only on hash equality — an assertion over opaque hashes cannot show
// WHICH part of the exclusion is wrong.
func materialDigestContent(body string) string {
	s := body
	for _, re := range volatileDigestPatterns {
		s = re.ReplaceAllString(s, "")
	}
	// Collapse whitespace last: stripping a stamp mid-line leaves ragged runs
	// of spaces that would otherwise re-introduce spurious differences.
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// zeroFindingLine matches the rendered "**Findings:** 0" summary that
// FormatDigestMarkdown emits for a digest with no open findings. The count is
// captured so a digest with any findings at all is never treated as
// zero-finding.
var zeroFindingLine = regexp.MustCompile(`(?m)^\*\*Findings:\*\*\s*(\d+)`)

// isZeroFindingDigest reports whether a rendered digest body announces zero
// findings. A body with no recognizable findings line returns false — the
// conservative answer, since the zero-finding cap must never suppress a digest
// whose content it could not read.
func isZeroFindingDigest(body string) bool {
	m := zeroFindingLine.FindStringSubmatch(body)
	if m == nil {
		return false
	}
	return m[1] == "0"
}

// ensureStringMap returns m, allocating it first when nil. The advisory maps
// are lazily created across several branches of PostAdvisoryDigest; this keeps
// each of them a one-liner under the same mutex.
func ensureStringMap(m map[string]string) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	return m
}

// digestSuppression is the decision for one post attempt.
type digestSuppression struct {
	// suppress reports whether the forge write must be skipped.
	suppress bool
	// reason is suppressReasonIdentical or suppressReasonZeroFindingCap, set
	// only when suppress is true.
	reason string
}

// evaluateDigestSuppression decides whether posting pending to a target that
// already carries posted (last updated at postedAt) should be suppressed.
//
// hasPosted is false when the target carries no digest comment yet — the first
// digest on a target ALWAYS posts, so a fresh advisory issue is never left
// empty by the guards.
func evaluateDigestSuppression(pending, posted string, hasPosted bool, postedAt, now time.Time, minInterval time.Duration) digestSuppression {
	if !hasPosted {
		return digestSuppression{}
	}
	if materialDigestFingerprint(pending) == materialDigestFingerprint(posted) {
		return digestSuppression{suppress: true, reason: suppressReasonIdentical}
	}
	// Zero-finding cap. Requires BOTH sides to be zero-finding: a transition
	// out of (or into) a non-empty finding set is material news and must post
	// even inside the window.
	if isZeroFindingDigest(pending) && isZeroFindingDigest(posted) &&
		!postedAt.IsZero() && now.Sub(postedAt) < zeroFindingDigestMinInterval {
		return digestSuppression{suppress: true, reason: suppressReasonZeroFindingCap}
	}
	// Changed-content floor (#17435): a digest whose live counts drift every
	// eval cycle must not rewrite the comment every cycle. postedAt is the
	// target's own updated_at, so the floor survives a governor restart.
	if minInterval > 0 && !postedAt.IsZero() && now.Sub(postedAt) < minInterval {
		return digestSuppression{suppress: true, reason: suppressReasonMinInterval}
	}
	return digestSuppression{}
}
