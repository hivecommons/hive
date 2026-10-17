package dashboard

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Panel 4 (#12368, RFC #12062): boundary enforcement needs an honest
// denominator. The proxy's write counters count only an agent's DIRECT
// GitHub writes (REST write methods, GraphQL mutations, git push through the
// proxy); the far larger share of agent writes goes through hive-mediated
// request files (hive-open-pr, issue requests, comments, labels, review
// posts) that the control plane validates and performs, and those are
// audited, not proxied. "7 of 16 direct writes blocked" is true and reads as
// 44%; "7 of 6,000 agent writes blocked" is the same seven refusals over
// everything the agents actually wrote. EvidenceWriteActivity carries both,
// with the denominator policy spelled out, so the card can show the rate
// over all writes and still list every refusal. Record-only.

// evidenceMediatedWriteActions are the audited hive-mediated agent writes
// that produced something on GitHub. Refusals are counted separately.
var evidenceMediatedWriteActions = map[string]bool{
	ghpkg.AuditActionAgentPRCreated:       true,
	ghpkg.AuditActionAgentIssueCreated:    true,
	ghpkg.AuditActionAgentCommentCreated:  true,
	ghpkg.AuditActionAgentLabelApplied:    true,
	ghpkg.AuditActionIssueClosed:          true,
	ghpkg.AuditActionAgentBranchPushed:    true,
	ghpkg.AuditActionAgentReviewRequested: true,
	ghpkg.AuditActionPRReviewed:           true,
}

// evidenceMediatedRefusalActions are audited refusals of a hive-mediated
// agent write: the relay or a watcher said no before anything reached GitHub.
var evidenceMediatedRefusalActions = map[string]bool{
	ghpkg.AuditActionAgentWriteRefused: true,
}

// EvidenceWriteActivity is every agent GitHub write the hive observed in the
// window, split by the path it took, with the refusals of each.
type EvidenceWriteActivity struct {
	// TotalWrites is Direct.Attempted + Mediated.N: the denominator policy
	// for the headline rate. Blocked is Direct.Blocked + Mediated.Refused.
	TotalWrites int64                  `json:"total_writes"`
	Blocked     int64                  `json:"blocked"`
	Direct      EvidenceDirectWrites   `json:"direct"`
	Mediated    EvidenceMediatedWrites `json:"mediated"`
}

// EvidenceDirectWrites is the proxy's view of direct agent writes in the
// window, summed from the durable counter file's weekly buckets.
type EvidenceDirectWrites struct {
	// Recorded is false when the counter file is absent; the card then says
	// so instead of showing zero direct writes.
	Recorded bool `json:"recorded"`
	// Since is when the counter file started accumulating. Direct writes
	// before it are not recorded (the file was container-local and reset on
	// every restart before it moved under /data), and PartialCoverage is
	// true when Since falls inside the window.
	Since           string                        `json:"since,omitempty"`
	PartialCoverage bool                          `json:"partial_coverage"`
	Attempted       int64                         `json:"attempted"`
	Blocked         int64                         `json:"blocked"`
	ByTier          map[string]EvidenceWriteCount `json:"by_tier"`
	BlockedByReason map[string]int64              `json:"blocked_by_reason"`
}

// EvidenceMediatedWrites is the audit log's view of hive-mediated agent
// writes in the window: requests the control plane performed (N, by action)
// and requests it refused.
type EvidenceMediatedWrites struct {
	N        int64            `json:"n"`
	Refused  int64            `json:"refused"`
	ByAction map[string]int64 `json:"by_action"`
}

// evidenceWriteActivityActions is the union the audit scan needs.
func evidenceWriteActivityActions() map[string]bool {
	out := map[string]bool{}
	for k := range evidenceMediatedWriteActions {
		out[k] = true
	}
	for k := range evidenceMediatedRefusalActions {
		out[k] = true
	}
	return out
}

// evidenceWriteActivityEntries reads the audit entries the mediated counts
// are built from, from the on-disk log (with rotated backups) when present
// and the in-memory ring otherwise, like /api/pr-throughput.
func evidenceWriteActivityEntries(audit *AuditLog) []AuditEntry {
	if audit == nil {
		audit = &AuditLog{}
	}
	actions := evidenceWriteActivityActions()
	if audit.HasOnDiskLog(prThroughputAuditPath) {
		return audit.OutputActionsSince(time.Time{}, actions, prThroughputAuditPath)
	}
	var out []AuditEntry
	for _, e := range audit.RecentWithPrefixSince(time.Time{}, "") {
		if actions[strings.TrimSpace(e.Action)] {
			out = append(out, e)
		}
	}
	return out
}

// buildEvidenceWriteActivity combines the proxy counter file at path with
// the audited mediated writes for the window and repo. The direct counts
// are not repo-filtered (the counter file has no repo dimension); the card
// says so when a repo filter is on.
func buildEvidenceWriteActivity(path string, entries []AuditEntry, win evidenceWindow, repo string, now time.Time) EvidenceWriteActivity {
	var out EvidenceWriteActivity
	out.Direct = buildEvidenceDirectWrites(path, win, now)
	out.Mediated = buildEvidenceMediatedWrites(entries, win, repo)
	out.TotalWrites = out.Direct.Attempted + out.Mediated.N
	out.Blocked = out.Direct.Blocked + out.Mediated.Refused
	return out
}

func buildEvidenceMediatedWrites(entries []AuditEntry, win evidenceWindow, repo string) EvidenceMediatedWrites {
	m := EvidenceMediatedWrites{ByAction: map[string]int64{}}
	bounded := win.Custom || win.Hours > 0
	for _, e := range entries {
		action := strings.TrimSpace(e.Action)
		write, refusal := evidenceMediatedWriteActions[action], evidenceMediatedRefusalActions[action]
		if !write && !refusal {
			continue
		}
		t, ok := prThroughputTime(e)
		if !ok || !prThroughputEntryMatchesRepo(e, repo) {
			continue
		}
		if bounded && t.Before(win.From) {
			continue
		}
		if !win.To.IsZero() && (t.After(win.To) || (win.Custom && t.Equal(win.To))) {
			continue
		}
		if refusal {
			m.Refused++
			continue
		}
		m.N++
		m.ByAction[action]++
	}
	return m
}

// writeCountersFile mirrors proxy.DurableWriteCounts (write_counters_store.go)
// — the dashboard reads the file rather than importing pkg/proxy, like
// handleWriteCounters. Only the fields this panel needs are decoded.
type writeCountersFile struct {
	RecordedSince time.Time                   `json:"recorded_since"`
	Weeks         map[string]writeCounterWeek `json:"weeks"`
}

type writeCounterWeek struct {
	Attempted       int64                         `json:"attempted"`
	Blocked         int64                         `json:"blocked"`
	Tiers           map[string]EvidenceWriteCount `json:"tiers"`
	BlockedByReason map[string]int64              `json:"blocked_by_reason"`
}

func (f writeCountersFile) earliestWeek() string {
	wk := ""
	for k := range f.Weeks {
		if wk == "" || k < wk {
			wk = k
		}
	}
	return wk
}

// buildEvidenceDirectWrites sums the counter file's ISO-week buckets that
// overlap the window. A week bucket is in when any part of it is in the
// window; weeks are the file's finest grain, so a 24h window reads the
// current week.
func buildEvidenceDirectWrites(path string, win evidenceWindow, now time.Time) EvidenceDirectWrites {
	d := EvidenceDirectWrites{ByTier: map[string]EvidenceWriteCount{}, BlockedByReason: map[string]int64{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return d
	}
	var f writeCountersFile
	if json.Unmarshal(data, &f) != nil {
		return d
	}
	d.Recorded = true
	since := f.RecordedSince
	if wk := f.earliestWeek(); wk != "" {
		if t, perr := time.Parse("2006-01-02", wk); perr == nil && (since.IsZero() || t.Before(since)) {
			since = t
		}
	}
	if !since.IsZero() {
		d.Since = since.UTC().Format(time.RFC3339)
	}
	from, to := win.From, win.To
	if to.IsZero() {
		to = now
	}
	bounded := win.Custom || win.Hours > 0
	if bounded && !since.IsZero() && since.After(from) {
		d.PartialCoverage = true
	}
	if !bounded && since.IsZero() {
		d.PartialCoverage = false
	}
	keys := make([]string, 0, len(f.Weeks))
	for k := range f.Weeks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		start, perr := time.Parse("2006-01-02", k)
		if perr != nil {
			continue
		}
		end := start.AddDate(0, 0, 7)
		if bounded && (end.Before(from) || !start.Before(to)) {
			continue
		}
		w := f.Weeks[k]
		d.Attempted += w.Attempted
		d.Blocked += w.Blocked
		for tier, tc := range w.Tiers {
			cur := d.ByTier[tier]
			cur.Attempted += tc.Attempted
			cur.Blocked += tc.Blocked
			d.ByTier[tier] = cur
		}
		for reason, n := range w.BlockedByReason {
			d.BlockedByReason[reason] += n
		}
	}
	return d
}
