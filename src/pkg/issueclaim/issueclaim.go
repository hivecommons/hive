// Package issueclaim is the vocabulary of an ISSUE CLAIM (hivecommons/hive#8380):
// a visible, expiring "I am working on this" mark on the issue itself.
//
// # Why the claim lives on GitHub
//
// The hub's PR-claim ledger suppresses an issue once an open PR references it,
// and the relay lease suppresses it while a hub-dispatched task holds it. Both
// protect the END of the pipeline. The window between "I started on this" and
// "I opened a PR" — hours for a large feature — is invisible to every worker
// that does not share the hub's state: a human reading the issue list, a
// second operator session, a spoke on another hive. #8336 grew two identical
// PRs minutes apart for exactly that reason.
//
// So the source of truth is a comment on the issue, carrying a marker any
// session or lane can recognise, or an assignee where the claimant has triage
// rights. The hub READS that marker when it lists work and WRITES it when one
// of its agents takes a lease. It never keeps a claim anywhere the marker is
// not also visible; the only hub-side copy is on the lease record, and that
// copy exists so the runs API can show it, not to outrank the issue.
//
// # Marker shape
//
//	<!-- hive:claim who=<identity> kind=contributor at=<started RFC3339> until=<expires RFC3339> -->
//
// followed by a human line. The marker is an HTML comment so it renders as
// nothing on GitHub; the human line is what a reader sees. The parser also
// accepts the legacy `<!-- hive-claim: <identity> <started> <expires> -->`
// comments already present on issues. Timestamps are RFC3339 in UTC. The
// identity is a GitHub login or a hive agent identity — free text without
// whitespace.
//
// This package is deliberately stdlib-only and a leaf in the import graph so
// pkg/config, pkg/github and pkg/dashboard can all share one parser.
package issueclaim

import (
	"fmt"
	"strings"
	"time"
)

const (
	// MarkerPrefix opens the canonical machine-readable marker. Everything
	// between it and MarkerSuffix is the claim record.
	MarkerPrefix = "<!-- hive:claim"
	// LegacyMarkerPrefix opens the older positional marker. It remains readable
	// so existing issue comments continue to release on expiry instead of being
	// forgotten by new binaries.
	LegacyMarkerPrefix = "<!-- hive-claim:"
	// ReleaseMarkerPrefix opens the marker the hub posts when it releases a
	// claim before its until= (the holding agent was relaunched, a person
	// released it). A release whose who= names the latest claim's identity
	// ends that claim: the 🔒 comment is never edited, so without this the
	// forge keeps reading a hold the ledger already dropped.
	ReleaseMarkerPrefix = "<!-- hive:release"
	// MarkerSuffix closes the marker.
	MarkerSuffix = "-->"
	// DefaultTTL is how long a claim stands when the claimant set no explicit
	// expiry and the operator configured none (governor.claims.ttl_s). Four
	// hours covers a large feature's pre-PR window without letting an
	// abandoned claim block the issue for a working day.
	DefaultTTL = 4 * time.Hour
	// markerFields is the number of whitespace-separated tokens a legacy marker
	// record carries: identity, started, expires.
	markerFields = 3

	// SourceMarker: the claim was read from a marker comment on the issue.
	SourceMarker = "marker"
	// SourceAssignee: the claim was inferred from the issue's assignee (no
	// marker comment). Its expiry is derived from the issue's last activity.
	SourceAssignee = "assignee"
	// SourceLease: the claim was recorded on a hub lease only, because the
	// claimant's level may not write comments on the forge.
	SourceLease = "lease"
)

// Claim is one recognised claim on an issue.
type Claim struct {
	// Identity is who holds the claim: a GitHub login or a hive agent identity.
	Identity string
	// StartedAt is when the claim was asserted.
	StartedAt time.Time
	// ExpiresAt is when the claim lapses on its own. A claim past this instant
	// is not live and must not withhold the issue.
	ExpiresAt time.Time
	// Source says where the claim was read from: SourceMarker, SourceAssignee
	// or SourceLease.
	Source string
	// Kind is the marker's kind= value (human, agent, contributor, external)
	// when the claim came from a key/value marker; empty otherwise.
	Kind string
	// Hive is the marker's hive= value — the hive that recorded the claim —
	// when present; empty otherwise.
	Hive string
	// Tier is the contributor trust tier the hub stamped in tier= when it
	// leased the issue. A kind=contributor marker without tier= parses as
	// ContributorTierDefault so markers written before the field existed
	// fail closed. Empty for other kinds.
	Tier string
	// Verified is the marker's verified= value: true only when the hub had
	// proven the contributor owns the GitHub login (verified=true). Absent or
	// any other value parses as false.
	Verified bool
	// PRStarted is the marker's pr_started= value: the claim carries PR
	// evidence (a pull-request request or a start marker naming a branch /
	// PR), which is what stages the issue stage/030-pr-in-progress. Absent
	// or any other value parses as false (hivecommons/hive#13069).
	PRStarted bool
	// Released is set when a later release marker named this claim's
	// identity: the holder let go before until=, so the claim is not live
	// however far off its expiry is.
	Released bool
}

// ContributorTierDefault is the tier a kind=contributor marker without tier=
// is read as: the lowest, self-registered tier.
const ContributorTierDefault = "newcomer"

// Live reports whether the claim still stands at now: it has an identity and
// an expiry strictly after now. A zero expiry is never live — an unbounded
// claim is exactly the abandoned-claim shape the TTL exists to prevent.
func (c Claim) Live(now time.Time) bool {
	return c.Identity != "" && !c.Released && !c.ExpiresAt.IsZero() && now.Before(c.ExpiresAt)
}

// Release is one parsed release marker: who let go and, when the marker
// carries at=, when.
type Release struct {
	Identity string
	At       time.Time
}

// ParseReleaseMarker reads a `<!-- hive:release who=<identity> ... -->`
// marker. at= is optional (older markers carry none); who= is required.
func ParseReleaseMarker(body string) (Release, bool) {
	start := strings.Index(body, ReleaseMarkerPrefix)
	if start < 0 {
		return Release{}, false
	}
	rest := body[start+len(ReleaseMarkerPrefix):]
	end := strings.Index(rest, MarkerSuffix)
	if end < 0 {
		return Release{}, false
	}
	var rel Release
	for _, field := range strings.Fields(rest[:end]) {
		key, val, ok := strings.Cut(field, "=")
		if !ok || val == "" {
			continue
		}
		switch key {
		case "who":
			rel.Identity = val
		case "at":
			at, err := time.Parse(time.RFC3339, val)
			if err != nil {
				return Release{}, false
			}
			rel.At = at
		}
	}
	if rel.Identity == "" {
		return Release{}, false
	}
	return rel, true
}

// Marker renders the machine-readable comment marker for a claim.
func Marker(identity string, started, expires time.Time) string {
	return fmt.Sprintf("%s who=%s kind=contributor at=%s until=%s %s", MarkerPrefix, identity,
		started.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339), MarkerSuffix)
}

// LegacyMarker renders the older positional marker. It is used only by tests to
// prove old issue comments remain readable; new writers must use Marker.
func LegacyMarker(identity string, started, expires time.Time) string {
	return fmt.Sprintf("%s %s %s %s %s", LegacyMarkerPrefix, identity,
		started.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339), MarkerSuffix)
}

// CommentBody renders the full claim comment: the marker, then the human line
// a reader sees on GitHub. The human line names the expiry so nobody has to
// decode the marker to know when the issue frees up.
func CommentBody(identity string, started, expires time.Time) string {
	return Marker(identity, started, expires) + "\n" +
		fmt.Sprintf("Claimed by %s until %s. The claim lapses on its own after that; a pull request referencing this issue supersedes it.",
			identity, expires.UTC().Format(time.RFC3339))
}

// ParseMarker finds the FIRST claim marker in a comment body and decodes it.
// It accepts the canonical key/value marker and the legacy positional marker.
// It returns false for a body with no marker, a marker with missing required
// fields, or timestamps that do not parse — a malformed marker is not a claim,
// and must never withhold an issue.
func ParseMarker(body string) (Claim, bool) {
	if claim, ok := parseKeyValueMarker(body); ok {
		return claim, true
	}
	return parseLegacyMarker(body)
}

func parseKeyValueMarker(body string) (Claim, bool) {
	start := strings.Index(body, MarkerPrefix)
	if start < 0 {
		return Claim{}, false
	}
	rest := body[start+len(MarkerPrefix):]
	end := strings.Index(rest, MarkerSuffix)
	if end < 0 {
		return Claim{}, false
	}
	fields := strings.Fields(rest[:end])
	vals := make(map[string]string, len(fields))
	for _, field := range fields {
		key, val, ok := strings.Cut(field, "=")
		if !ok || key == "" || val == "" {
			return Claim{}, false
		}
		vals[key] = val
	}
	identity := vals["who"]
	until := vals["until"]
	if identity == "" || until == "" {
		return Claim{}, false
	}
	expires, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return Claim{}, false
	}
	var started time.Time
	if raw := vals["at"]; raw != "" {
		started, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return Claim{}, false
		}
	}
	claim := Claim{Identity: identity, StartedAt: started, ExpiresAt: expires, Source: SourceMarker, Kind: vals["kind"], Hive: vals["hive"], PRStarted: vals["pr_started"] == "true"}
	if strings.EqualFold(claim.Kind, "contributor") {
		claim.Tier = strings.ToLower(vals["tier"])
		if claim.Tier == "" {
			claim.Tier = ContributorTierDefault
		}
		claim.Verified = vals["verified"] == "true"
	}
	return claim, true
}

func parseLegacyMarker(body string) (Claim, bool) {
	start := strings.Index(body, LegacyMarkerPrefix)
	if start < 0 {
		return Claim{}, false
	}
	rest := body[start+len(LegacyMarkerPrefix):]
	end := strings.Index(rest, MarkerSuffix)
	if end < 0 {
		return Claim{}, false
	}
	fields := strings.Fields(rest[:end])
	if len(fields) != markerFields {
		return Claim{}, false
	}
	started, err := time.Parse(time.RFC3339, fields[1])
	if err != nil {
		return Claim{}, false
	}
	expires, err := time.Parse(time.RFC3339, fields[2])
	if err != nil {
		return Claim{}, false
	}
	return Claim{Identity: fields[0], StartedAt: started, ExpiresAt: expires, Source: SourceMarker}, true
}

// ClampToTTL returns claim with its expiry capped at StartedAt + ttl.
func ClampToTTL(claim Claim, ttl time.Duration) Claim {
	if claim.StartedAt.IsZero() || ttl <= 0 {
		return claim
	}
	ttlExpires := claim.StartedAt.Add(ttl)
	if claim.ExpiresAt.IsZero() || claim.ExpiresAt.After(ttlExpires) {
		claim.ExpiresAt = ttlExpires
	}
	return claim
}

// Latest returns the most recently STARTED marker claim among the comment
// bodies, whether or not it is still live. Callers decide what an expired
// claim means for them (usually: nothing) with Claim.Live. The newest claim
// wins so a renewal comment supersedes the one it renews and an expired
// earlier claim cannot shadow a fresh one.
//
// A release marker naming that claim's identity marks it Released: one that
// carries at= must not predate the claim's start; one without at= (older
// hubs) must follow the claim in bodies, which the forge lists oldest first.
// A release of an EARLIER holder never touches a later claim.
func Latest(bodies []string, ttl time.Duration) (Claim, bool) {
	var best Claim
	bestIdx := -1
	type release struct {
		Release
		idx int
	}
	var releases []release
	for i, body := range bodies {
		if rel, ok := ParseReleaseMarker(body); ok {
			releases = append(releases, release{Release: rel, idx: i})
			continue
		}
		claim, ok := ParseMarker(body)
		if !ok {
			continue
		}
		claim = ClampToTTL(claim, ttl)
		if bestIdx < 0 || claimOrderTime(claim).After(claimOrderTime(best)) {
			best = claim
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return Claim{}, false
	}
	for _, rel := range releases {
		if rel.Identity != best.Identity {
			continue
		}
		if !rel.At.IsZero() {
			if !rel.At.Before(claimOrderTime(best)) {
				best.Released = true
			}
			continue
		}
		if rel.idx > bestIdx {
			best.Released = true
		}
	}
	return best, true
}

func claimOrderTime(claim Claim) time.Time {
	if !claim.StartedAt.IsZero() {
		return claim.StartedAt
	}
	return claim.ExpiresAt
}

// FromAssignees infers a claim from an issue's assignees when no marker
// comment exists. The first assignee is the claimant and the claim runs for
// ttl from the issue's last activity (updatedAt), so an assignee who keeps
// the issue moving keeps the claim and one who went quiet loses it. With no
// assignee, or no usable activity timestamp, there is no claim: an expiry the
// hub cannot compute is an expiry it must not assume.
func FromAssignees(assignees []string, updatedAt time.Time, ttl time.Duration) (Claim, bool) {
	if updatedAt.IsZero() || ttl <= 0 {
		return Claim{}, false
	}
	for _, login := range assignees {
		login = strings.TrimSpace(login)
		if login == "" {
			continue
		}
		return Claim{
			Identity:  login,
			StartedAt: updatedAt,
			ExpiresAt: updatedAt.Add(ttl),
			Source:    SourceAssignee,
		}, true
	}
	return Claim{}, false
}

// Resolve is the one decision every listing surface should share: given the
// issue's comment bodies, assignees and last-activity time, which claim (if
// any) is LIVE at now. A marker claim outranks an assignee inference, and an
// expired marker releases the issue rather than falling back to the assignee
// — the marker is the explicit statement, and it explicitly lapsed.
func Resolve(bodies []string, assignees []string, updatedAt time.Time, ttl time.Duration, now time.Time) (Claim, bool) {
	if claim, ok := Latest(bodies, ttl); ok {
		if claim.Live(now) {
			return claim, true
		}
		return Claim{}, false
	}
	if claim, ok := FromAssignees(assignees, updatedAt, ttl); ok && claim.Live(now) {
		return claim, true
	}
	return Claim{}, false
}
