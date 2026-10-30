package fleetreport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/config"
)

// TriggerRuntimeError is the fleet-report trigger for the "unusual runtime
// error" allow-list below (hivecommons/hive#11868). Unlike the ACMM-shortfall
// trigger it needs no unmet criterion: the class itself is the attribution.
const TriggerRuntimeError = "runtime-error"

// Runtime error classes. Each is an explicit allow-list entry: an error site
// opts in by calling Observe with one of these, so an arbitrary logged error
// never becomes an upstream issue by accident.
//
// The classes the agent runtime and its watchdog emit are declared in
// the crux root (the runtime reports through crux.ConditionReporter and
// must not import this package); they are aliased here so the allow-list
// below and every host-side caller keep one name.
const (
	ClassRestartStorm             = crux.ClassRestartStorm
	ClassKickFailedRepeatedly     = crux.ClassKickFailedRepeatedly
	ClassPanicRecovered           = "panic_recovered"
	ClassModelDiscoveryDegraded   = "model_discovery_degraded"
	ClassInferenceAuthFailed      = "inference_auth_failed"
	ClassAdvisoryPostFailed       = "advisory_post_failed"
	ClassUpgradeStalled           = "upgrade_stalled"
	ClassDataDiskPressure         = crux.ClassDataDiskPressure
	ClassGitHubAppTokenMintFailed = crux.ClassGitHubAppTokenMintFailed
	ClassGitHubAppNotInstalled    = crux.ClassGitHubAppNotInstalled
	ClassUnexpectedHub5xx         = "unexpected_http_5xx_from_hub"
	ClassUnclassifiedRepeated     = "unclassified_error_repeated"
	// ClassAgentStall, ClassAgentCrashLoop and ClassCIPollNudge replace the
	// retired hive-code-defect trigger (hivecommons/hive#13470): the harness
	// counters that used to be synthesised into a bare "Fleet report: …
	// hive-code defect" issue are now observed at the hook site that owns
	// them, so the report carries kick/pane context like every other class.
	ClassAgentStall     = crux.ClassAgentStall
	ClassAgentCrashLoop = crux.ClassAgentCrashLoop
	ClassCIPollNudge    = crux.ClassCIPollNudge
	// ClassIssueRequestStuck and ClassEffectLedgerOpStuck are queue-health
	// classes: a relay request that keeps failing or a journaled external
	// effect that stays Unknown is work the hive is silently not doing. Both
	// are fed by the owning component once ITS stuck threshold is crossed
	// (pkg/github issue-request watcher, pkg/convergence/mutation boundary),
	// so the class itself fires on the first observation.
	ClassIssueRequestStuck   = "issue_request_stuck"
	ClassEffectLedgerOpStuck = "effect_ledger_op_stuck"
	// ClassCruxRuntimeUnavailable: the sidecar `crux serve` the hive was
	// built against is unreachable (hivecommons/hive#14138 stage 4). Raised
	// by cmd/hive's runtime watcher on the first failed probe and cleared
	// on the first success; while it holds, kicks are refused with
	// crux.ErrRuntimeUnavailable rather than queued.
	ClassCruxRuntimeUnavailable = crux.ClassRuntimeUnavailable
)

// Log-fed classes: faults that have no dedicated hook site and are recognised
// from their WARN+ log line by the slog feed (see LogFedClass).
const (
	ClassTmuxSessionCheckFailed     = "tmux_session_check_failed"
	ClassAgentPaneTimeout           = "agent_pane_timeout"
	ClassProxyTLSHandshakeFailed    = "proxy_tls_handshake_failed"
	ClassAgentDirSetupFailed        = "agent_dir_setup_failed"
	ClassReviewEvidenceWriteFail    = "review_evidence_write_failed"
	ClassFeedbackIssueRefreshFailed = "feedback_issue_refresh_failed"
	ClassBeadFactIngestFailed       = "bead_fact_ingest_failed"
	ClassSelfReportClearedFailed    = "selfreport_cleared_comment_failed"
	ClassGitHubAppRepoNotCovered    = "github_app_repo_not_covered"
	ClassProxyConnectDialFailed     = "proxy_connect_dial_failed"
	ClassProxyPlainForwardRefused   = "proxy_plain_forward_refused"
	ClassInferenceUpstreamError     = "inference_upstream_error"
	ClassInferenceCallerUnknown     = "inference_caller_unidentified"
	ClassSHAPollCommitMsgUnavail    = "sha_poll_commit_message_unavailable"
	ClassKickFreshnessWithheld      = "kick_freshness_withheld"
	ClassAgentLoginRequired         = "agent_login_required"
	ClassPRRequestHeadUnpushed      = "pr_request_head_unpushed"
	ClassIOScanClassifierUnavail    = "ioscan_classifier_unavailable"
	ClassGitHubFetchTransient       = "github_fetch_transient"
	ClassLabelProvisioningFailed    = "label_provisioning_failed"
	ClassAgentCLIRestartLoop        = "agent_cli_restart_loop"
	ClassTranscriptScanSlow         = "transcript_scan_slow"
	ClassSharedCIRerunForbidden     = "shared_ci_rerun_forbidden"
	// A restart or normal stop can invalidate a pending bootstrap without
	// indicating a restart storm; only repeated interruptions are reportable.
	ClassStartupKickInterrupted = "startup_kick_interrupted"
)

const (
	// unclassifiedRepeatCount / unclassifiedRepeatWindow: the same unclassified
	// error string must recur this many times inside the window before it
	// qualifies as unusual.
	unclassifiedRepeatCount  = 5
	unclassifiedRepeatWindow = 10 * time.Minute
	// unclassifiedIdleClear: an unclassified error that has not recurred for
	// this long (2× its window) is cleared, so a "cleared" comment follows
	// once the symptom stops instead of the condition idling until the TTL.
	unclassifiedIdleClear = 2 * unclassifiedRepeatWindow
	// kickFailureStreak is the consecutive-failure count that makes repeated
	// kick failures reportable.
	kickFailureStreak = 3
	// degradedMinDuration is how long model discovery must stay degraded.
	degradedMinDuration = time.Hour
	// hub5xxStreak is how many consecutive 5xx answers from the hub (the
	// condition is cleared on the next accepted request) make it reportable,
	// so a single blip during a hub rollout is not filed upstream.
	hub5xxStreak = 3
	// AgentStallNudgeStreak is how many "continue" nudges an inference agent
	// must swallow without the pane moving before the stall is reportable; a
	// single nudge is the designed self-recovery working. A recovery (the pane
	// moved after a nudge) clears the condition, so the count is consecutive
	// unrecovered nudges — the same bar the retired dashboard synthesis
	// applied as StallNudges − StallRecoveries.
	AgentStallNudgeStreak = 3
	// ciPollNudgeWindow is the window a CI-poll nudge report claims: the
	// harness caps the nudge at one per kick, so recurrence is per day.
	ciPollNudgeWindow = 24 * time.Hour
	// runtimeEventTTL bounds how long a condition that is never cleared keeps
	// producing evidence; anything older is dropped on the next snapshot.
	runtimeEventTTL = 24 * time.Hour
	// maxDiagnosticsBytes caps the scrubbed diagnostics excerpt attached to a
	// report so an issue body stays readable.
	maxDiagnosticsBytes = 8 * 1024
	// maxTrackedConditions bounds the tracker so a misbehaving caller cannot
	// grow memory without limit.
	maxTrackedConditions = 256
)

// ClassSpec describes one runtime error class.
type ClassSpec struct {
	// Component is the hive-owned component the class is attributed to.
	Component string
	Severity  string
	// MinCount is how many observations (within TTL) are needed before the
	// condition is reportable. 1 = immediately.
	MinCount int
	// MinDuration is how long the condition must persist before it is
	// reportable; zero = no persistence requirement.
	MinDuration time.Duration
	// Window, when set, requires MinCount observations inside this window
	// rather than cumulatively.
	Window time.Duration
	// IdleClear, when set, clears a condition that has had no observation
	// for this long on the next snapshot (recorded as cleared when it had
	// qualified), instead of letting it idle until the TTL drops it.
	IdleClear time.Duration
	// Keywords are the human phrasings used to find an existing issue about
	// the same symptom (join-first search). Lower-case.
	Keywords []string
	// LogFed marks a class observed by the slog feed: a log line containing
	// one of its Keywords is counted under the class instead of
	// ClassUnclassifiedRepeated.
	LogFed bool
}

// Classes is the runtime error allow-list.
var Classes = map[string]ClassSpec{
	ClassRestartStorm:               {Component: "agent-runtime", Severity: "high", MinCount: 1, Keywords: []string{"restart storm", "restart-storm"}},
	ClassKickFailedRepeatedly:       {Component: "agent-runtime", Severity: "medium", MinCount: kickFailureStreak, Keywords: []string{"kick failed", "undeliverable kick", "kicks undeliverable", "re-kick backed off"}},
	ClassPanicRecovered:             {Component: "dashboard", Severity: "high", MinCount: 1, Keywords: []string{"panic recovered", "recovered panic"}},
	ClassModelDiscoveryDegraded:     {Component: "inference-gateway", Severity: "medium", MinCount: 1, MinDuration: degradedMinDuration, Keywords: []string{"model discovery degraded", "model discovery failed"}},
	ClassInferenceAuthFailed:        {Component: "backend-auth", Severity: "high", MinCount: 1, Keywords: []string{"litellm 401", "inference 401", "inference 403", "authentication failure"}},
	ClassAdvisoryPostFailed:         {Component: "github-client", Severity: "medium", MinCount: 1, Keywords: []string{"advisory post failed", "advisory digest"}},
	ClassUpgradeStalled:             {Component: "agent-lifecycle", Severity: "high", MinCount: 1, Keywords: []string{"upgrade stalled", "upgrade stall"}},
	ClassDataDiskPressure:           {Component: "agent-runtime", Severity: "high", MinCount: 1, Keywords: []string{"disk full", "disk pressure", "no space left"}},
	ClassGitHubAppTokenMintFailed:   {Component: "github-client", Severity: "high", MinCount: 1, Keywords: []string{"token mint failed", "installation token"}},
	ClassGitHubAppNotInstalled:      {Component: "github-client", Severity: "medium", MinCount: 1, Keywords: []string{"github app installation id is unset", "github app has no installation"}},
	ClassUnexpectedHub5xx:           {Component: "dashboard", Severity: "medium", MinCount: hub5xxStreak, Keywords: []string{"hub 5", "hub returned 5"}},
	ClassUnclassifiedRepeated:       {Component: "agent-runtime", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear},
	ClassAgentStall:                 {Component: "agent-runtime", Severity: "medium", MinCount: AgentStallNudgeStreak, Keywords: []string{"agent stall", "stalled after kick", "continue nudge"}},
	ClassAgentCrashLoop:             {Component: "agent-runtime", Severity: "high", MinCount: 1, Keywords: []string{"crash loop", "crash-loop", "crash looping"}},
	ClassCIPollNudge:                {Component: "agent-runtime", Severity: "low", MinCount: 1, Window: ciPollNudgeWindow, Keywords: []string{"ci-poll nudge", "polling ci", "stop-polling nudge"}},
	ClassIssueRequestStuck:          {Component: "github-relay", Severity: "medium", MinCount: 1, Keywords: []string{"issue-request stuck", "issue request stuck", "issue-request watcher", "relay request stuck"}},
	ClassCruxRuntimeUnavailable:     {Component: "agent-runtime", Severity: "high", MinCount: 1, Keywords: []string{"crux runtime unavailable", "crux server unreachable"}},
	ClassEffectLedgerOpStuck:        {Component: "mutation-ledger", Severity: "medium", MinCount: 1, Keywords: []string{"effect ledger stuck", "ledger op stuck", "needs reconciliation", "mutation journal unknown"}},
	ClassTmuxSessionCheckFailed:     {Component: "agent-runtime", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"tmux has-session failed"}},
	ClassStartupKickInterrupted:     {Component: "agent-runtime", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"startup kick dropped: agent restarted or stopped while waiting"}},
	ClassAgentPaneTimeout:           {Component: "agent-runtime", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"prompt timeout — dumping pane", "pane content at timeout", "startup kick dropped: cli never became ready", "startup kick dropped: cli never reached input prompt"}},
	ClassProxyTLSHandshakeFailed:    {Component: "github-proxy", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"proxy client tls handshake failed", "transparent proxy tls handshake failed", "copilot sniff: client tls handshake failed"}},
	ClassAgentDirSetupFailed:        {Component: "agent-runtime", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"failed to create per-agent xdg dir", "failed to restore per-agent dir mode", "failed to restore interactive home mode", "failed to tighten per-agent dir mode", "failed to tighten interactive home mode", "failed to restore inference home mode", "failed to tighten inference home mode"}},
	ClassReviewEvidenceWriteFail:    {Component: "github-client", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"review evidence: could not write bundle"}},
	ClassFeedbackIssueRefreshFailed: {Component: "dashboard", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"feedback: issue refresh failed"}},
	ClassBeadFactIngestFailed:       {Component: "knowledge", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"failed to ingest bead fact"}},
	ClassSelfReportClearedFailed:    {Component: "github-client", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"fleet report: cleared comment failed"}},
	ClassGitHubAppRepoNotCovered:    {Component: "github-client", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"installation does not cover every configured repo", "state=repo-not-covered"}},
	ClassProxyConnectDialFailed:     {Component: "github-proxy", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"proxy: connect dial failed"}},
	ClassProxyPlainForwardRefused:   {Component: "github-proxy", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"proxy: plain http forward refused"}},
	ClassInferenceCallerUnknown:     {Component: "inference-gateway", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"inference request from unidentified caller refused"}},
	ClassInferenceUpstreamError:     {Component: "inference-gateway", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"inference upstream error", "inference upstream failed"}},
	ClassSHAPollCommitMsgUnavail:    {Component: "github-client", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"sha poll: commit message fetch non-200"}},
	ClassKickFreshnessWithheld:      {Component: "agent-runtime", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"kick withheld: every ref failed the freshness check"}},
	ClassAgentLoginRequired:         {Component: "agent-runtime", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"login required detected"}},
	ClassPRRequestHeadUnpushed:      {Component: "github-relay", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"head branch is not on the remote"}},
	ClassIOScanClassifierUnavail:    {Component: "inference-gateway", Severity: "medium", MinCount: 1, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"ioscan classifier failed closed", "ioscan classifier failed open", "ioscan fail-closed blocked kick", "ioscan fail-closed blocked held-pr coordination kick"}},
	ClassGitHubFetchTransient:       {Component: "github-client", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"channel commit date: fetch failed"}},
	ClassLabelProvisioningFailed:    {Component: "github-client", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"issue stage labels: provisioning failed"}},
	ClassAgentCLIRestartLoop:        {Component: "agent-runtime", Severity: "medium", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"agent cli crashed or stuck on consent screen"}},
	ClassSharedCIRerunForbidden:     {Component: "github-client", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"shared-ci rerun forbidden"}},
	ClassTranscriptScanSlow:         {Component: "agent-runtime", Severity: "low", MinCount: unclassifiedRepeatCount, Window: unclassifiedRepeatWindow, IdleClear: unclassifiedIdleClear, LogFed: true, Keywords: []string{"transcript activity scan outlived its interval"}},
}

// LogFedClass returns the LogFed class whose Keyword msg contains, or "".
func LogFedClass(msg string) string {
	msg = strings.ToLower(msg)
	for class, spec := range Classes {
		if !spec.LogFed {
			continue
		}
		for _, kw := range spec.Keywords {
			if kw != "" && strings.Contains(msg, kw) {
				return class
			}
		}
	}
	return ""
}

// MatchesExplicitClass reports whether msg (lower-cased) contains a Keyword
// of any class other than ClassUnclassifiedRepeated. The generic repeated-
// error feed skips such messages: the owning hook site already reports them
// under their class, and a second low-severity report would only split the
// evidence.
func MatchesExplicitClass(msg string) bool {
	msg = strings.ToLower(msg)
	for class, spec := range Classes {
		if class == ClassUnclassifiedRepeated {
			continue
		}
		for _, kw := range spec.Keywords {
			if kw != "" && strings.Contains(msg, kw) {
				return true
			}
		}
	}
	return false
}

// IsKnownClass reports whether class is on the allow-list.
func IsKnownClass(class string) bool {
	_, ok := Classes[strings.TrimSpace(class)]
	return ok
}

// RuntimeCondition is one tracked (class, key) condition.
type RuntimeCondition struct {
	Class       string
	Key         string
	Message     string
	Diagnostics string
	FirstSeen   time.Time
	LastSeen    time.Time
	Count       int
	// Repo and Ref are the most recent triggering item (see ObserveItem).
	Repo   string
	Ref    string
	events []time.Time
	// qualified records that the condition met its threshold on some
	// snapshot, so an idle clear after the window emptied still posts.
	qualified bool
}

// RuntimeTracker accumulates runtime error observations and turns the ones
// that meet their class thresholds into Evidence. It is safe for concurrent
// use; the clock is injectable so tests never sleep.
type RuntimeTracker struct {
	mu    sync.Mutex
	now   func() time.Time
	conds map[string]*RuntimeCondition
	// cleared records conditions that were active and then cleared, so the
	// publisher can post a single "cleared" comment.
	cleared map[string]RuntimeCondition
}

// NewRuntimeTracker returns a tracker using now (nil = time.Now).
func NewRuntimeTracker(now func() time.Time) *RuntimeTracker {
	if now == nil {
		now = time.Now
	}
	return &RuntimeTracker{now: now, conds: map[string]*RuntimeCondition{}, cleared: map[string]RuntimeCondition{}}
}

var (
	defaultTrackerMu sync.RWMutex
	defaultTracker   = NewRuntimeTracker(nil)
)

// DefaultTracker is the process-wide tracker the hook sites report into.
func DefaultTracker() *RuntimeTracker {
	defaultTrackerMu.RLock()
	defer defaultTrackerMu.RUnlock()
	return defaultTracker
}

// SetDefaultTrackerForTest swaps the process-wide tracker for the lifetime
// of t.
func SetDefaultTrackerForTest(t interface {
	Helper()
	Cleanup(func())
}, tr *RuntimeTracker) {
	t.Helper()
	defaultTrackerMu.Lock()
	old := defaultTracker
	defaultTracker = tr
	defaultTrackerMu.Unlock()
	t.Cleanup(func() {
		defaultTrackerMu.Lock()
		defaultTracker = old
		defaultTrackerMu.Unlock()
	})
}

// Observe records one occurrence of class on the default tracker. key scopes
// the condition (agent name, backend, repo…); message is the error text;
// diagnostics is an optional excerpt (scrubbed and bounded before storage).
func Observe(class, key, message, diagnostics string) {
	DefaultTracker().Observe(class, key, message, diagnostics)
}

// ObserveItem is Observe for an error scoped to one repository item: repo is
// the owner/repo and ref the issue/PR reference ("#123", may be empty). The
// item is carried in the report's evidence table and affected-repos union
// (#12338); it never enters the fingerprint.
func ObserveItem(class, key, message, diagnostics, repo, ref string) {
	DefaultTracker().ObserveItem(class, key, message, diagnostics, repo, ref)
}

// Clear marks a condition as no longer present on the default tracker.
func Clear(class, key string) { DefaultTracker().Clear(class, key) }

// DefaultReporter is the crux.ConditionReporter the host hands the agent
// runtime (Manager.SetConditionReporter) and the watchdog. It forwards to
// the package-level Observe and Clear rather than capturing DefaultTracker
// at wiring time, so a test that swaps the default tracker with
// SetDefaultTrackerForTest sees the runtime's observations too.
type DefaultReporter struct{}

var _ crux.ConditionReporter = DefaultReporter{}

func (DefaultReporter) Observe(class, key, message, diagnostics string) {
	Observe(class, key, message, diagnostics)
}

func (DefaultReporter) Clear(class, key string) { Clear(class, key) }

func condKey(class, key string) string {
	return strings.TrimSpace(class) + "\x00" + strings.TrimSpace(key)
}

// Observe records one occurrence. Unknown classes are ignored: the allow-list
// is the contract.
func (t *RuntimeTracker) Observe(class, key, message, diagnostics string) {
	t.ObserveItem(class, key, message, diagnostics, "", "")
}

// ObserveItem records one occurrence scoped to a repository item; see the
// package-level ObserveItem. Empty repo/ref leave the previous item in place.
func (t *RuntimeTracker) ObserveItem(class, key, message, diagnostics, repo, ref string) {
	if t == nil || !IsKnownClass(class) {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	ck := condKey(class, key)
	c, ok := t.conds[ck]
	if !ok {
		if len(t.conds) >= maxTrackedConditions {
			t.evictOldestLocked()
		}
		c = &RuntimeCondition{Class: strings.TrimSpace(class), Key: safeToken(key), FirstSeen: now}
		t.conds[ck] = c
	}
	delete(t.cleared, ck)
	c.LastSeen = now
	c.Count++
	c.events = append(c.events, now)
	if len(c.events) > unclassifiedRepeatCount*2 {
		c.events = c.events[len(c.events)-unclassifiedRepeatCount*2:]
	}
	if msg := safeToken(logscrub.RedactPaneTokens(message)); msg != "" {
		c.Message = msg
	}
	if d := boundDiagnostics(diagnostics); d != "" {
		c.Diagnostics = d
	}
	if r := NormalizeRepo(repo); r != "" {
		c.Repo = r
		c.Ref = safeToken(ref)
	}
}

// Clear removes a condition; if it was active it is remembered as cleared.
func (t *RuntimeTracker) Clear(class, key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ck := condKey(class, key)
	c, ok := t.conds[ck]
	if !ok {
		return
	}
	t.clearLocked(ck, c, t.now())
}

// clearLocked removes c; if it qualifies now, or ever did, it is remembered
// as cleared. Callers hold t.mu.
func (t *RuntimeTracker) clearLocked(ck string, c *RuntimeCondition, now time.Time) {
	if c.qualified || t.qualifiesLocked(c, now) {
		snap := *c
		snap.events = nil
		snap.LastSeen = now
		t.cleared[ck] = snap
	}
	delete(t.conds, ck)
}

// TakeCleared returns and forgets the conditions that cleared since the last
// call, so a "cleared" comment is posted exactly once.
func (t *RuntimeTracker) TakeCleared() []RuntimeCondition {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RuntimeCondition, 0, len(t.cleared))
	for _, c := range t.cleared {
		out = append(out, c)
	}
	t.cleared = map[string]RuntimeCondition{}
	sort.Slice(out, func(i, j int) bool { return out[i].Class+out[i].Key < out[j].Class+out[j].Key })
	return out
}

func (t *RuntimeTracker) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, c := range t.conds {
		if oldestKey == "" || c.LastSeen.Before(oldest) {
			oldestKey, oldest = k, c.LastSeen
		}
	}
	delete(t.conds, oldestKey)
}

func (t *RuntimeTracker) qualifiesLocked(c *RuntimeCondition, now time.Time) bool {
	spec, ok := Classes[c.Class]
	if !ok {
		return false
	}
	if now.Sub(c.LastSeen) > runtimeEventTTL {
		return false
	}
	if spec.MinDuration > 0 && now.Sub(c.FirstSeen) < spec.MinDuration {
		return false
	}
	min := spec.MinCount
	if min < 1 {
		min = 1
	}
	if spec.Window > 0 {
		inWindow := 0
		for _, at := range c.events {
			if now.Sub(at) <= spec.Window {
				inWindow++
			}
		}
		return inWindow >= min
	}
	return c.Count >= min
}

// Evidence returns one Evidence per condition that currently meets its class
// threshold. Stale conditions (no observation inside the TTL) are dropped.
func (t *RuntimeTracker) Evidence() []Evidence {
	if t == nil {
		return nil
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Evidence
	for ck, c := range t.conds {
		if now.Sub(c.LastSeen) > runtimeEventTTL {
			delete(t.conds, ck)
			continue
		}
		spec := Classes[c.Class]
		if spec.IdleClear > 0 && now.Sub(c.LastSeen) >= spec.IdleClear {
			t.clearLocked(ck, c, now)
			continue
		}
		if !t.qualifiesLocked(c, now) {
			continue
		}
		c.qualified = true
		window := spec.Window
		if window == 0 {
			window = now.Sub(c.FirstSeen).Round(time.Second)
		}
		out = append(out, Evidence{
			Component:    spec.Component,
			Agent:        c.Key,
			ErrorClass:   c.Class,
			Count:        c.Count,
			Window:       window,
			Severity:     spec.Severity,
			Attributable: true,
			Class:        c.Class,
			Message:      c.Message,
			Diagnostics:  c.Diagnostics,
			FirstSeen:    c.FirstSeen,
			Repo:         c.Repo,
			Ref:          c.Ref,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// NormalizeMessage collapses the volatile parts of an error message (numbers,
// hex ids, quoted paths/URLs) so the same defect produces the same fingerprint
// across agents and runs.
func NormalizeMessage(msg string) string {
	msg = strings.ToLower(strings.TrimSpace(logscrub.ScrubString(msg)))
	var b strings.Builder
	runes := []rune(msg)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case r >= '0' && r <= '9' || (r >= 'a' && r <= 'f' && runOfHex(runes, i) >= 8):
			j := i
			for j < len(runes) && ((runes[j] >= '0' && runes[j] <= '9') || (runes[j] >= 'a' && runes[j] <= 'f') || runes[j] == '-' || runes[j] == '.') {
				j++
			}
			b.WriteString("N")
			i = j
		case r == '/' && i+1 < len(runes) && runes[i+1] != ' ':
			// Filesystem paths and URL paths: one placeholder for the whole
			// run so per-agent/per-file directories do not split fingerprints.
			j := i
			for j < len(runes) && runes[j] != ' ' && runes[j] != ',' && runes[j] != ';' && runes[j] != ')' && runes[j] != ']' && runes[j] != ':' && runes[j] != '\n' && runes[j] != '\t' {
				j++
			}
			b.WriteString("P")
			i = j
		case r == '"' || r == '\'' || r == '`':
			j := i + 1
			for j < len(runes) && runes[j] != r {
				j++
			}
			b.WriteString("Q")
			if j < len(runes) {
				j++
			}
			i = j
		case r == '\n' || r == '\t':
			b.WriteByte(' ')
			i++
		default:
			b.WriteRune(r)
			i++
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func runOfHex(runes []rune, i int) int {
	n := 0
	for i+n < len(runes) {
		r := runes[i+n]
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			n++
			continue
		}
		break
	}
	return n
}

// RuntimeFingerprint is class + normalised message. The hive version is
// deliberately NOT part of it: a fingerprint names a defect, not a build.
// When it did include the version (before #14256), every spoke on a
// different build, and the same spoke after every upgrade, got a fresh
// fingerprint for the same message — so a closed issue's cooldown never
// applied, the join-first search never matched, and a hive on a daily
// channel re-filed the same warning as a brand-new issue each day.
func RuntimeFingerprint(class, message string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{TriggerRuntimeError, strings.ToLower(strings.TrimSpace(class)), NormalizeMessage(message)}, "|")))
	return hex.EncodeToString(h[:])[:24]
}

// SimilarityPhrase is the title phrase the join-first search uses when the
// class alone does not identify the defect (the catch-all
// unclassified_error_repeated and any other class without keywords): the
// leading words of the short message, cut at the first clause separator
// and bounded so GitHub's search accepts it as one quoted phrase. Empty
// when the message has no usable words.
func SimilarityPhrase(message string) string {
	msg := ShortMessage(message)
	if msg == "unusual error" {
		return ""
	}
	if i := strings.IndexAny(msg, ";—…\n"); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.NewReplacer("\"", " ", "`", " ", "'", " ", ":", " ", "(", " ", ")", " ", "[", " ", "]", " ").Replace(msg)
	words := strings.Fields(msg)
	const maxWords = 8
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	return strings.Join(words, " ")
}

// ShortMessage trims a message for an issue title.
func ShortMessage(msg string) string {
	msg = strings.Join(strings.Fields(safeToken(msg)), " ")
	const max = 80
	if len(msg) > max {
		return msg[:max-1] + "…"
	}
	if msg == "" {
		return "unusual error"
	}
	return msg
}

// boundDiagnostics scrubs credential shapes (including the sk- API keys
// logscrub.ScrubString leaves alone) and keeps the newest maxDiagnosticsBytes.
func boundDiagnostics(d string) string {
	d = strings.TrimSpace(logscrub.RedactPaneTokens(logscrub.ScrubString(d)))
	if len(d) > maxDiagnosticsBytes {
		d = "…\n" + d[len(d)-maxDiagnosticsBytes:]
	}
	return d
}

// Markers used in issue bodies and comments. The legacy
// hive-fleet-fingerprint marker is kept alongside so older hives still find
// the issue.
const (
	SelfReportMarkerPrefix     = "<!-- hive:selfreport "
	SelfReportSeenMarkerPrefix = "<!-- hive:selfreport-seen "
	AffectedBlockStart         = "<!-- hive:selfreport-affected -->"
	AffectedBlockEnd           = "<!-- /hive:selfreport-affected -->"
	// LabelSelfReport is the self-report joiner's dedupe search key. As a
	// provenance marker it is deprecated in favour of LabelOriginSelfReport
	// (the origin/<source> axis); every report carries both.
	LabelSelfReport       = "hive/selfreport"
	LabelOriginSelfReport = config.OriginLabelPrefix + config.OriginSourceSelfReport
	LabelKindBug          = "kind/bug"
	LabelNeedsTriage      = "needs-triage"
	LabelFleetReport      = "fleet-report"
	LabelObservation      = "hive-observation"
)

// SelfReportMarker is the body marker a runtime-error report carries.
func SelfReportMarker(fp, hive string) string {
	return fmt.Sprintf("%sfp=%s hive=%s -->", SelfReportMarkerPrefix, safeToken(fp), labelValue(hive))
}

// MarkerHive is the normalised hive value the self-report and seen markers
// carry (`hive=<value>`), so a reader can build the needle for a given hive.
func MarkerHive(hive string) string { return labelValue(hive) }

// SeenMarker is the comment marker a joining hive leaves.
func SeenMarker(fp, hive string) string {
	return fmt.Sprintf("%shive=%s fp=%s -->", SelfReportSeenMarkerPrefix, labelValue(hive), safeToken(fp))
}

// SelfReportLabels is the label set every hive-filed report carries: the
// fleet-report / self-report / origin / kind / triage labels that exist in
// the report repo, plus the per-report severity, trigger and (when set)
// class. Sorted so bodies and tests are stable.
func SelfReportLabels(trigger, severity, class string) []string {
	labels := []string{LabelFleetReport, LabelSelfReport, LabelOriginSelfReport, LabelKindBug, LabelNeedsTriage, "severity:" + labelValue(severity), "trigger:" + labelValue(trigger)}
	if strings.TrimSpace(class) != "" {
		labels = append(labels, "class:"+labelValue(class))
	}
	sort.Strings(labels)
	return labels
}

// CoreSelfReportLabels is the subset of SelfReportLabels that is known to
// exist in the report repo; a creator that cannot create labels falls back
// to it rather than filing an unlabelled issue.
func CoreSelfReportLabels() []string {
	return []string{LabelFleetReport, LabelSelfReport, LabelOriginSelfReport, LabelKindBug, LabelNeedsTriage}
}

// BuildRuntimeReport renders a runtime-error report. hive is the display
// identity (hub slug for hosted spokes, anonymous instance otherwise); the
// owner logins come from obs.Owners unless obs.OwnersHidden.
func BuildRuntimeReport(obs Observation, ev Evidence, fp, instance, hive, version, commit string, now time.Time) Report {
	if strings.TrimSpace(hive) == "" {
		hive = instance
	}
	labels := SelfReportLabels(TriggerRuntimeError, ev.Severity, ev.Class)
	title := fmt.Sprintf("[selfreport] %s: %s", ev.Class, ShortMessage(ev.Message))
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- hive-fleet-fingerprint:%s -->\n<!-- hive-fleet-instance:%s -->\n%s\n\n", fp, instance, SelfReportMarker(fp, hive))
	fmt.Fprintf(&b, "A hive hit an unusual runtime error (class `%s`) and is reporting it so the same defect across the fleet lands in one issue.\n\n", ev.Class)
	b.WriteString("## Evidence\n\n| Field | Value |\n|---|---|\n")
	name, spokeURL := writeIdentityRows(&b, obs, hive, instance, version, commit)
	fmt.Fprintf(&b, "| component | %s |\n", ev.Component)
	fmt.Fprintf(&b, "| scope | %s |\n", emptyDash(ev.Agent))
	fmt.Fprintf(&b, "| message | %s |\n", emptyDash(strings.ReplaceAll(ev.Message, "|", "\\|")))
	fmt.Fprintf(&b, "| count + window | %s |\n", countWindow(ev.Count, ev.Window))
	fmt.Fprintf(&b, "| first seen | %s |\n", ev.FirstSeen.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| reported at | %s |\n", now.UTC().Format(time.RFC3339))
	watched := NormalizeRepos(obs.Repos)
	fmt.Fprintf(&b, "| repos | %s |\n", repoListCell(watched))
	fmt.Fprintf(&b, "| owners | %s |\n", repoListCell(OwnersOf(watched)))
	itemRepo := NormalizeRepo(ev.Repo)
	fmt.Fprintf(&b, "| repo | %s |\n", emptyDash(itemRepo))
	fmt.Fprintf(&b, "| ref | %s |\n", emptyDash(itemRef(itemRepo, ev.Ref)))
	b.WriteString("| self-recovered | no |\n\n")
	repos := watched
	if itemRepo != "" {
		repos = NormalizeRepos(append(append([]string(nil), watched...), itemRepo))
	}
	owners := reportOwners(obs)
	b.WriteString(AffectedBlock{Hives: []AffectedHive{{Hive: hive, Name: name, URL: spokeURL, Instance: instance, Owners: owners, OwnersHidden: obs.OwnersHidden, Seen: 1, Last: now}}, Repos: repos, Seen: 1}.Render())
	if ev.Diagnostics != "" {
		fmt.Fprintf(&b, "\n\n<details><summary>Diagnostics (scrubbed)</summary>\n\n```\n%s\n```\n</details>\n", ev.Diagnostics)
	}
	return Report{Fingerprint: fp, InstanceID: instance, Title: safeText(title), Body: safeText(b.String()), Labels: labels, Trigger: TriggerRuntimeError, Class: ev.Class, Hive: hive, HiveName: name, SpokeURL: spokeURL, Evidence: []Evidence{ev}, Repos: repos, Owners: owners, OwnersHidden: obs.OwnersHidden, HiveState: obs.HiveState}
}

// writeIdentityRows writes the WHO rows of an evidence table — hive, hive
// name, spoke link, anonymous instance, owner(s), version, commit, cluster,
// mode — and returns the normalised hive name and spoke URL. Every hive-filed
// report shares them (#13211, #12529, #13470).
func writeIdentityRows(b *strings.Builder, obs Observation, hive, instance, version, commit string) (name, spokeURL string) {
	fmt.Fprintf(b, "| hive | %s |\n", hive)
	name, spokeURL = NormalizeHiveName(obs.HiveName), NormalizeSpokeURL(obs.SpokeURL)
	fmt.Fprintf(b, "| %s | %s |\n", HiveNameField, emptyDash(name))
	fmt.Fprintf(b, "| %s | %s |\n", SpokeField, SpokeLink(name, hive, spokeURL))
	fmt.Fprintf(b, "| anonymous instance | %s |\n", instance)
	fmt.Fprintf(b, "| owner(s) | %s |\n", OwnerMentions(obs.Owners, obs.OwnersHidden))
	fmt.Fprintf(b, "| hive version | %s |\n", version)
	fmt.Fprintf(b, "| hive commit | %s |\n", commit)
	fmt.Fprintf(b, "| cluster | %s |\n", emptyDash(safeToken(obs.Cluster)))
	fmt.Fprintf(b, "| mode | %s |\n", emptyDash(safeToken(obs.Mode)))
	return name, spokeURL
}

// reportOwners is the owner login list a report names: obs.Owners
// normalised, or nil when the operator hid them.
func reportOwners(obs Observation) []string {
	if obs.OwnersHidden {
		return nil
	}
	return NormalizeLogins(obs.Owners)
}

// maxReposInTable bounds the repos / owners cells of the evidence table; the
// affected block below the table always carries the full union.
const maxReposInTable = 20

// repoListCell renders "N: a/b, c/d" for the evidence table, "—" when empty.
func repoListCell(list []string) string {
	if len(list) == 0 {
		return "—"
	}
	shown := list
	more := 0
	if len(shown) > maxReposInTable {
		more = len(shown) - maxReposInTable
		shown = shown[:maxReposInTable]
	}
	cell := fmt.Sprintf("%d: %s", len(list), strings.Join(shown, ", "))
	if more > 0 {
		cell += fmt.Sprintf(", +%d more", more)
	}
	return cell
}

// itemRef renders the triggering item's reference as owner/repo#N when both
// halves are known, the bare ref otherwise.
func itemRef(repo, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if repo != "" && strings.HasPrefix(ref, "#") {
		return repo + ref
	}
	return ref
}

// NormalizeRepo lower-cases and validates an owner/repo name; anything that
// is not exactly two non-empty path segments of GitHub-safe characters
// renders as "" so a stray URL or token can never reach the issue body.
func NormalizeRepo(repo string) string {
	repo = strings.ToLower(strings.TrimSpace(logscrub.ScrubString(repo)))
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimSuffix(repo, ".git")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
		for _, r := range p {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
			if !ok {
				return ""
			}
		}
	}
	return repo
}

// NormalizeRepos normalises, dedupes and sorts a repo list.
func NormalizeRepos(repos []string) []string {
	uniq := map[string]bool{}
	var out []string
	for _, r := range repos {
		r = NormalizeRepo(r)
		if r == "" || uniq[r] {
			continue
		}
		uniq[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// OwnersOf returns the distinct, sorted owners (orgs or users) of repos.
func OwnersOf(repos []string) []string {
	uniq := map[string]bool{}
	var out []string
	for _, r := range repos {
		r = NormalizeRepo(r)
		if r == "" {
			continue
		}
		owner := r[:strings.Index(r, "/")]
		if uniq[owner] {
			continue
		}
		uniq[owner] = true
		out = append(out, owner)
	}
	sort.Strings(out)
	return out
}

// RenderAffectedBlock renders the affected block for hives known by name
// only; see AffectedBlock.Render.
func RenderAffectedBlock(hives []string, seen int) string {
	return RenderAffectedBlockWithRepos(hives, nil, seen)
}

// RenderAffectedBlockWithRepos renders the affected block for hives known by
// name only plus the union of the repos they watch.
func RenderAffectedBlockWithRepos(hives, repos []string, seen int) string {
	return AffectedBlock{Hives: affectedByName(hives), Repos: repos, Seen: seen}.Render()
}

func affectedByName(hives []string) []AffectedHive {
	out := make([]AffectedHive, 0, len(hives))
	for _, h := range hives {
		out = append(out, AffectedHive{Hive: h})
	}
	return out
}

// ParseAffectedBlock extracts the hive names and seen_count from a body; ok
// is false when the body has no block.
func ParseAffectedBlock(body string) (hives []string, seen int, ok bool) {
	hives, _, seen, ok = ParseAffectedBlockWithRepos(body)
	return hives, seen, ok
}

// ParseAffectedBlockWithRepos extracts the hive names, repos and seen_count
// from a body; ok is false when the body has no block. See ParseAffected for
// the per-hive owner / instance / seen details.
func ParseAffectedBlockWithRepos(body string) (hives, repos []string, seen int, ok bool) {
	blk, ok := ParseAffected(body)
	if !ok {
		return nil, nil, 0, false
	}
	for _, h := range blk.Hives {
		hives = append(hives, h.Hive)
	}
	return hives, blk.Repos, blk.Seen, true
}

// affectedLineItems splits the comma list after the ":**" of an affected line.
func affectedLineItems(line string) []string {
	i := strings.Index(line, ":**")
	if i < 0 {
		return nil
	}
	var items []string
	for _, h := range strings.Split(line[i+3:], ",") {
		if h = strings.TrimSpace(h); h != "" {
			items = append(items, h)
		}
	}
	return items
}

// UpdateAffectedBlock returns body with hive added to the affected block (and
// seen_count bumped), appending a block when the body has none. changed is
// false when the body already lists the hive and no bump is requested.
func UpdateAffectedBlock(body, hive string, bump bool) (string, bool) {
	return UpdateAffectedBlockWithRepos(body, hive, nil, bump)
}

// UpdateAffectedBlockWithRepos is UpdateAffectedBlock that also merges repos
// into the block's union. See UpdateAffected for the owner-aware form.
func UpdateAffectedBlockWithRepos(body, hive string, repos []string, bump bool) (string, bool) {
	return UpdateAffected(body, AffectedHive{Hive: hive}, repos, bump)
}

// SeenComment renders the "also seen" comment a joining hive posts.
func SeenComment(fp, hive, version, cluster string, ev Evidence, now time.Time) string {
	var b strings.Builder
	b.WriteString(SeenMarker(fp, hive))
	fmt.Fprintf(&b, "\nAlso seen on **%s** (%s, %s) at %s", labelValue(hive), displayVersion(version), emptyDash(safeToken(cluster)), now.UTC().Format(time.RFC3339))
	if ev.Class != "" {
		fmt.Fprintf(&b, " — class `%s`, %s", ev.Class, countWindow(ev.Count, ev.Window))
	}
	if ev.Message != "" {
		fmt.Fprintf(&b, "\n\n> %s", strings.ReplaceAll(ev.Message, "\n", " "))
	}
	if ev.Diagnostics != "" {
		fmt.Fprintf(&b, "\n\n<details><summary>Diagnostics (scrubbed)</summary>\n\n```\n%s\n```\n</details>", ev.Diagnostics)
	}
	return safeText(b.String())
}

// ClearedComment renders the one-time "condition cleared" comment.
func ClearedComment(fp, hive, version string, now time.Time) string {
	return safeText(fmt.Sprintf("%s\nCleared on **%s** (%s) at %s — the condition is no longer observed there. Leaving the issue open for a human to confirm the fix.", SeenMarker(fp, hive), labelValue(hive), displayVersion(version), now.UTC().Format(time.RFC3339)))
}

// FilingLimits are the rate limits applied to NEW issue creation.
type FilingLimits struct {
	MaxPerDay      int
	MinGap         time.Duration
	ClosedCooldown time.Duration
	SeenGap        time.Duration
}

// DefaultFilingLimits: 5 new issues per hive per day, 1h between filings of
// different classes, a fingerprint whose issue was closed upstream is not
// re-filed until the condition has been absent for 7 days (see
// ClosedSuppressed), and a hive adds itself to an existing issue — or posts
// a "cleared" comment on it — at most once per 24h.
var DefaultFilingLimits = FilingLimits{MaxPerDay: 5, MinGap: time.Hour, ClosedCooldown: 7 * 24 * time.Hour, SeenGap: 24 * time.Hour}

// WithMaxPerDay returns limits with MaxPerDay overridden when n > 0.
func (l FilingLimits) WithMaxPerDay(n int) FilingLimits {
	if n > 0 {
		l.MaxPerDay = n
	}
	return l
}

// AllowNewFiling reports whether a NEW issue may be created now, with the
// reason when it may not. It does not mutate state.
func AllowNewFiling(st State, now time.Time, limits FilingLimits) (bool, string) {
	recent := 0
	var last time.Time
	for _, at := range st.Filings {
		if now.Sub(at) < 24*time.Hour {
			recent++
		}
		if at.After(last) {
			last = at
		}
	}
	if limits.MaxPerDay > 0 && recent >= limits.MaxPerDay {
		return false, fmt.Sprintf("daily cap reached (%d/%d)", recent, limits.MaxPerDay)
	}
	if limits.MinGap > 0 && !last.IsZero() && now.Sub(last) < limits.MinGap {
		return false, fmt.Sprintf("last filing %s ago < min gap %s", now.Sub(last).Round(time.Second), limits.MinGap)
	}
	return true, ""
}

// RecordFiling appends a new-issue creation to state and prunes entries
// older than a day.
func RecordFiling(st *State, now time.Time) {
	var keep []time.Time
	for _, at := range st.Filings {
		if now.Sub(at) < 24*time.Hour {
			keep = append(keep, at)
		}
	}
	st.Filings = append(keep, now.UTC())
}

// AllowSeenComment reports whether this hive may add itself to issue open
// again (once per SeenGap).
func AllowSeenComment(open OpenIssue, now time.Time, limits FilingLimits) bool {
	return open.LastSeenCommentAt.IsZero() || now.Sub(open.LastSeenCommentAt) >= limits.SeenGap
}

// ClosedRecently reports whether a fingerprint's issue closed inside the
// cooldown: treat as known/fixed-pending-rollout and do not re-file.
func ClosedRecently(closedAt time.Time, now time.Time, limits FilingLimits) bool {
	return !closedAt.IsZero() && now.Sub(closedAt) < limits.ClosedCooldown
}

// ClosedSuppressed reports whether a fingerprint whose issue was found
// closed upstream must not be re-filed now. The cooldown runs from the later
// of the close and the last time THIS hive observed the condition while the
// issue was closed (OpenIssue.ClosedObservedAt): a condition that persists on
// this build keeps the suppression alive for as long as it persists, however
// many builds the hive moves through, and only a condition that was absent
// for the whole cooldown and then came back — a regression, not the same
// unfixed instance — is filed again. Whoever closed the issue (maintainer,
// scanner, close-on-merge) therefore silences this reporter for it.
func ClosedSuppressed(open OpenIssue, now time.Time, limits FilingLimits) bool {
	if open.ClosedAt.IsZero() {
		return false
	}
	since := open.ClosedAt
	if open.ClosedObservedAt.After(since) {
		since = open.ClosedObservedAt
	}
	return now.Sub(since) < limits.ClosedCooldown
}

// AllowClearedComment reports whether a runtime "cleared" comment may be
// posted for open now: never on an issue known to be closed, and at most
// once per SeenGap so a condition that flaps between observed and cleared
// does not post "Cleared on …" every cycle (hivecommons/hive#13825 collected
// 79 of them in a day).
func AllowClearedComment(open OpenIssue, now time.Time, limits FilingLimits) bool {
	if !open.ClosedAt.IsZero() {
		return false
	}
	return open.ClearedPostedAt.IsZero() || now.Sub(open.ClearedPostedAt) >= limits.SeenGap
}

// displayVersion renders a version with exactly one leading "v".
func displayVersion(v string) string {
	v = safeToken(v)
	if v == "" {
		return "vunknown"
	}
	return "v" + strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
}
