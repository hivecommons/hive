package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/apphealth"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/dashchat"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/discord"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/hub"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/inference"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/loginscan"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/matrix"
	"github.com/hivecommons/hive/pkg/mention"
	"github.com/hivecommons/hive/pkg/mint"
	"github.com/hivecommons/hive/pkg/msteams"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/proxy"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/sessionprune"
	"github.com/hivecommons/hive/pkg/slack"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/spokealerts"
	"github.com/hivecommons/hive/pkg/taskmcp"
	"github.com/hivecommons/hive/pkg/telegram"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/toolapprove"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/trajectory"
	"github.com/hivecommons/hive/pkg/watchdog"
	"github.com/hivecommons/hive/pkg/watsonx"
	"github.com/hivecommons/hive/pkg/worksource"
	"go.opentelemetry.io/otel/attribute"
	"gopkg.in/natefinch/lumberjack.v2"
)

// version is the single source of truth for the version string Hive reports
// in `--version`, hub heartbeats, and dashboard registration payloads.
//
// It is overridable at build time via `-ldflags -X main.version=...`, exactly
// like gitHash/gitShort/gitBranch below. A plain branch build (no ldflag)
// falls back to "0.0.0-dev" rather than an empty string, so an operator who
// builds locally or from an untagged CI run still sees a sensible, obviously
// non-release value instead of "hive  (commit ...)" or a version that lies by
// claiming a release number it isn't.
//
// src/Dockerfile and src/Dockerfile.hub now stamp this at ordinary
// image-build time: when the VERSION build-arg is not supplied they derive it
// from `git describe --tags --always --dirty` against the /repo/.git they copy
// in, so a release build cut at tag v4.34.0 self-reports "4.34.0" and a branch
// build a few commits ahead self-reports "4.34.0-7-g43a51078" (nearest tag +
// commits-ahead + short commit). .github/workflows/docker.yml checks the build
// out with fetch-depth: 0 so that tag history is present for `git describe`.
// tagged-release.yml still only retags the already-published docker.yml image
// (it never rebuilds — see src/docs/releases.md), which is exactly why the
// version has to be embedded at the ORIGINAL build here rather than at tag
// time. The 0.0.0-dev default below remains the safety net for any build that
// supplies no ldflag at all (a bare local `go build`).
//
// The raw value is reported through reportedVersion(), which strips a leading
// "v" so the string the three consumers see (`--version`, hub heartbeats and
// dashboard registration payloads) is digit-leading semver ("4.34.0…"), not
// the "v"-prefixed git tag form — matching what those surfaces already assume.
var version = "0.0.0-dev"

// normalizeVersion turns a raw build-stamped version (a git tag or
// `git describe` output such as "v4.34.0" or "v4.34.0-7-g43a51078-dirty") into
// the digit-leading semver form Hive reports, by trimming surrounding
// whitespace and stripping a single leading "v"/"V" when it is immediately
// followed by a digit. It is a pure formatter: an empty input stays empty (the
// 0.0.0-dev fallback lives in the `version` default, not here) and a value that
// is not "v<digit>…" (e.g. "0.0.0-dev" itself, or a bare commit sha) passes
// through untouched.
func normalizeVersion(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		v = v[1:]
	}
	return v
}

// reportedVersion is the single version string Hive exposes to operators and
// to the hub, via `--version`, the hub heartbeat, and dashboard registration.
func reportedVersion() string {
	return normalizeVersion(version)
}

func publishFleetReports(ctx context.Context, logger *slog.Logger, ghClient *github.Client, dashSrv *dashboard.Server, res *fleetreport.Result, dryRun bool) {
	if res == nil || dryRun || ghClient == nil || dashSrv == nil {
		return
	}
	for _, report := range res.Reports {
		write, err := ghClient.EnsureFleetReport(ctx, report)
		if err != nil {
			logger.Warn("fleet report: upstream write failed", "fingerprint", report.Fingerprint, "error", err)
			continue
		}
		dashSrv.MarkFleetReportPosted(report.Fingerprint, write.Number, write.URL, write.Created, report.Body)
		logger.Info("fleet report: upstream report recorded", "fingerprint", report.Fingerprint, "issue", write.Number, "created", write.Created, "commented", write.Commented, "reaction", write.ReactionSent)
	}
	for _, report := range res.Recoveries {
		open, ok := dashSrv.FleetReportOpenIssue(report.Fingerprint)
		if !ok {
			continue
		}
		if open.Number <= 0 {
			issue, found, err := ghClient.FleetReportIssue(ctx, report.Fingerprint)
			if err != nil {
				logger.Warn("fleet report: recovery lookup failed", "fingerprint", report.Fingerprint, "error", err)
				continue
			}
			if !found {
				dashSrv.ClearFleetReportOpen(report.Fingerprint)
				continue
			}
			open.Number = issue.Number
			open.URL = issue.URL
		}
		if err := ghClient.PostFleetReportRecovery(ctx, open.Number, report, open.OpenedByHive); err != nil {
			logger.Warn("fleet report: recovery write failed", "fingerprint", report.Fingerprint, "issue", open.Number, "error", err)
			continue
		}
		dashSrv.MarkFleetReportRecovered(report.Fingerprint)
		logger.Info("fleet report: recovery recorded", "fingerprint", report.Fingerprint, "issue", open.Number, "closed", open.OpenedByHive)
	}
}

var (
	gitHash   = "unknown"
	gitShort  = "unknown"
	gitBranch = "unknown"
)

// traceShutdownTimeout bounds how long we wait for the OTel exporter to flush
// pending spans during shutdown, so a slow/unreachable collector can't hang
// process exit.
const traceShutdownTimeout = 5 * time.Second

// reachStatePath is where the component reach counters (#3993) persist on the
// PVC. Deliberately its OWN file, not the main /data/hive-state.json (#3973
// resolved OQ-2): reach state is append-mostly telemetry keyed by the running
// commit, and a parse failure in it must never take agent/governor state down
// with it (or vice versa). Written on the same cadence as the main state file
// (persistState), loaded once at boot.
const reachStatePath = "/data/reach-state.json"

type persistPaths struct {
	ReachState            string
	SparklineHistory      string
	ModeHistory           string
	TokenSparklineHistory string
	FactHistory           string
	CostHistory           string
	TrendHistory          string
	BudgetWindowHistory   string
	ConvergenceSoak       string
}

func defaultPersistPaths() persistPaths {
	return persistPaths{
		ReachState:            reachStatePath,
		SparklineHistory:      "/data/sparkline-history.json",
		ModeHistory:           "/data/mode-history.json",
		TokenSparklineHistory: "/data/token-sparkline-history.json",
		FactHistory:           "/data/fact-history.json",
		CostHistory:           "/data/cost-history.json",
		TrendHistory:          "/data/trend-history.json",
		BudgetWindowHistory:   "/data/budget-window-history.json",
		ConvergenceSoak:       "/data/convergence-soak-history.json",
	}
}

func agentActivityFor(mgr *agent.Manager, cfg *config.Config, govState governor.State, currentMode, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) hub.AgentActivity {
	act := hub.AgentActivity{
		Paused:         proc.Paused,
		PausedTrigger:  proc.PausedTrigger,
		PausedReason:   proc.PausedReason,
		PausedBy:       proc.PausedBy,
		PausedAt:       proc.PausedAt,
		NeedsLogin:     proc.NeedsLogin,
		QuotaExhausted: proc.QuotaExhausted,
		LastActivityAt: proc.LastPaneChange,
		SessionMissing: mgr.SessionMissing(name),
	}
	if status := proc.BackendAuth.Status; status != "" && status != agent.BackendAuthOK {
		act.BackendAuthStatus = status
		act.BackendAuthSince = proc.BackendAuth.Since
		act.BackendAuthLastError = proc.BackendAuth.LastError
	}
	if proc.StartedAt != nil {
		act.StartedAt = *proc.StartedAt
	}
	act.KickInterval = heartbeatKickInterval(govState, name, proc, onDemandFromPack)
	if cfg != nil {
		onDemandAgent := false
		enabled := false
		if ac, ok := cfg.Agents[name]; ok {
			onDemandAgent = ac.OnDemand
			enabled = ac.Enabled
		}
		act.ExpectedActive = cfg.ExpectedActive(name, currentMode, onDemandAgent, onDemandFromPack)
		act.Enabled = enabled
	}
	if canIssue, canPR, canMerge, ok := mgr.AgentCapabilities(name); ok {
		act.CanOpenIssue = canIssue
		act.CanOpenPR = canPR
		act.CanMerge = canMerge
	}
	if backend, ok := mgr.EffectiveBackend(name); ok {
		act.Backend = backend
	}
	if sf, ok := mgr.StartFailureState(name); ok && strings.TrimSpace(sf.Reason) != "" && sf.Count > 0 {
		act.StartFailureReason = sf.Reason
		act.StartFailureCount = sf.Count
		act.StartFailureLastAt = sf.LastAt
		act.StartBlocked = sf.Blocked
		if sf.Blocked {
			act.StartBlockedReason = sf.Reason
		}
		if sf.LastExitCode != nil {
			act.StartFailureExitCode = sf.LastExitCode
		}
		act.StartFailureSignal = sf.LastSignal
	}
	if total, last24h, lastAt, reason, ok := mgr.RestartTelemetry(name); ok {
		act.Restarts.Total = total
		act.Restarts.Last24h = last24h
		act.Restarts.LastReason = reason
		if !lastAt.IsZero() {
			act.Restarts.LastRestartAt = lastAt.UTC().Format(time.RFC3339)
		}
	}
	return act
}

func heartbeatKickInterval(govState governor.State, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) time.Duration {
	if proc == nil || !proc.Config.UsesGovernorKick() || proc.Config.OnDemand || onDemandFromPack[name] {
		return 0
	}
	interval := governorShortestActiveInterval(govState, name)
	return interval
}

func governorCadencesForAgent(govState governor.State, name string) []governor.AgentCadence {
	var cadences []governor.AgentCadence
	for key, cadence := range govState.Cadences {
		agent, _ := config.SplitCadenceTargetKey(key)
		if agent == name || cadence.Agent == name {
			cadences = append(cadences, cadence)
		}
	}
	return cadences
}

func governorLastKickForAgent(govState governor.State, name string) time.Time {
	var newest time.Time
	for key, ts := range govState.LastKick {
		agent, _ := config.SplitCadenceTargetKey(key)
		if agent == name && !ts.IsZero() && ts.After(newest) {
			newest = ts
		}
	}
	return newest
}

func governorShortestActiveInterval(govState governor.State, name string) time.Duration {
	var shortest time.Duration
	for _, cadence := range governorCadencesForAgent(govState, name) {
		if cadence.Paused || cadence.Interval <= 0 {
			continue
		}
		if shortest == 0 || cadence.Interval < shortest {
			shortest = cadence.Interval
		}
	}
	return shortest
}

func outputFreshnessHeartbeatFields(acmmLevel int, govState governor.State, agents []spoke.AgentSummary) (lastWriteKickAt, disposition, reason string, notWritableQueued int) {
	notWritableQueued = govState.QueueHold
	var newest time.Time
	for _, a := range agents {
		if !agentCanProduceJudgedOutput(acmmLevel, a) {
			continue
		}
		if t := governorLastKickForAgent(govState, a.Name); !t.IsZero() && t.After(newest) {
			newest = t
		}
	}
	if !newest.IsZero() {
		lastWriteKickAt = newest.UTC().Format(time.RFC3339)
	}
	switch {
	case acmmLevel > 0 && acmmLevel <= 2:
		disposition = "advisory-only"
		reason = "ACMM advisory band produces advisory output, not writes"
	case govState.BudgetExhausted:
		disposition = "budget-suppressed"
		reason = "governor budget exhausted"
	case govState.QueueIssues+govState.QueuePRs == 0 && govState.QueueHold > 0:
		disposition = "agent-decided-not-writable"
		reason = "queued items are held or otherwise not writable"
	case govState.QueueIssues+govState.QueuePRs == 0:
		disposition = "idle"
		reason = "no actionable work queued"
	case len(govState.Cadences) == 0:
		disposition = "no-due-agents"
		reason = "no agents due in the current governor mode"
	default:
		dueCapable := false
		now := time.Now()
		for _, a := range agents {
			if !agentCanProduceJudgedOutput(acmmLevel, a) {
				continue
			}
			for _, cad := range governorCadencesForAgent(govState, a.Name) {
				if cad.Paused {
					continue
				}
				last := govState.LastKick[config.CadenceTargetKey(cad.Agent, cad.Repo)]
				if cad.Schedule.Mode() != config.CadenceModeInterval {
					if _, ok := cad.Schedule.DueOccurrence(last, now, config.CadenceCatchUpWindow); ok {
						dueCapable = true
						break
					}
					continue
				}
				if cad.Interval <= 0 || last.IsZero() || now.Sub(last) >= cad.Interval {
					dueCapable = true
					break
				}
			}
			if dueCapable {
				break
			}
		}
		if !dueCapable {
			disposition = "no-due-agents"
			reason = "no write-capable agents due in the current governor mode"
		} else {
			disposition = "kick-capable"
			reason = "write-capable agents are eligible to kick"
		}
	}
	return lastWriteKickAt, disposition, reason, notWritableQueued
}

func agentCanProduceJudgedOutput(acmmLevel int, a spoke.AgentSummary) bool {
	switch {
	case acmmLevel >= 6:
		return a.CanMerge
	case acmmLevel >= 3:
		return a.CanOpenIssue || a.CanOpenPR
	default:
		return false
	}
}

// prospectiveGitHubIdentity returns the GitHub identity the spoke WOULD hold
// after adopting ghCfg, or nil when the push speaks to no identity field and
// there is nothing to validate.
//
// It mirrors the adoption rules in the GitHubAppConfigCallback exactly — a
// zero app_id and an empty app_slug both mean "not speaking to this field", and
// the placeholder sentinel is never adopted over a real App. Mirroring rather
// than validating ghCfg alone is what makes the check correct: the damaging
// state is a combination of PUSHED and EXISTING fields (a GHE app_id landing
// beside the spoke's own empty api_url), and validating the push in isolation
// cannot see it.
func prospectiveGitHubIdentity(cur config.GitHubConfig, ghCfg *spoke.HeartbeatGitHubAppConfig) *config.GitHubConfig {
	if ghCfg == nil {
		return nil
	}
	touched := false
	next := cur
	if ghCfg.AppID != 0 && ghCfg.AppID != config.PlaceholderAppID {
		next.AppID = ghCfg.AppID
		touched = true
	}
	if ghCfg.AppSlug != "" && ghCfg.AppSlug != cur.AppSlug {
		next.AppSlug = ghCfg.AppSlug
		touched = true
	}
	// The forge URLs are part of the SAME set as the App above, so they are
	// adopted here and validated with it rather than arriving separately on the
	// project-config channel. An App ID presented to the wrong forge returns
	// "404 Integration not found", so applying one half without the other is the
	// live failure this function exists to prevent.
	//
	// Empty means "unchanged", matching AppSlug. It cannot mean "make me
	// public": empty URLs are also the correct steady state for a public hive
	// (~41 of 50 spokes), so silence here is indistinguishable from "no opinion"
	// and must never blank a working GHE URL. A hive moving TO public gets that
	// from its app_id, which the resolver derives from its forge.
	if ghCfg.APIURL != "" && ghCfg.APIURL != cur.APIURL {
		next.APIURL = ghCfg.APIURL
		touched = true
	}
	if ghCfg.BaseURL != "" && ghCfg.BaseURL != cur.BaseURL {
		next.BaseURL = ghCfg.BaseURL
		touched = true
	}
	if !touched {
		return nil
	}
	return &next
}

// nextInstallationID decides what a hive's installation_id becomes after a
// hub delivery, and reports whether the change is an operator RESET.
//
// Three cases, and the difference between the last two is load-bearing:
//
//	ResetInstallation  -> 0. The operator clicked "Reset App". Clearing makes
//	                     HasUsableApp() false, which raises githubAppRequired:
//	                     the owner is prompted to install the App again and the
//	                     self-heal ticker starts, whose RediscoverAndAdopt
//	                     adopts the correct installation for whatever they
//	                     install.
//	non-zero pushed    -> adopt it.
//	zero pushed        -> KEEP the current value. Zero means "the hub is not
//	                     speaking to this field", not "clear it". The
//	                     cluster-wide key reconcile sends zero on every beat
//	                     because it repairs KEYS on hives whose installation the
//	                     hub does not track; reading that as a clear would blank
//	                     a working installation fleet-wide and turn a key-only
//	                     fault into a total auth outage.
//
// docs_installation_id is DELIBERATELY untouched by all three cases. It is the
// optional docs-org add-on installation, has no rediscovery flow (zero simply
// disables the docs token refresh, permanently), and a stale value is
// non-fatal: the periodic docs mint warns and retries. After an app-changing
// delivery it can therefore briefly equal the PREVIOUS app's installation id —
// which looks like the old installation_id was "parked" there, but is just the
// provisioned docs value (public hives commonly provision both fields with the
// same installation) surviving a reset that correctly cleared only
// installation_id. On a flip-back to the original App it becomes valid again
// on its own.
func nextInstallationID(current int64, ghCfg *spoke.HeartbeatGitHubAppConfig) (next int64, reset bool) {
	if ghCfg == nil {
		return current, false
	}
	if ghCfg.ResetInstallation {
		return 0, current != 0
	}
	if ghCfg.InstallationID != 0 {
		return ghCfg.InstallationID, false
	}
	return current, false
}

var githubAppTokenCachePath = github.TokenCachePath

func githubAppTokenHeartbeatFields(cfg *config.Config, detail string) (status, lastMintAt, lastErr string) {
	if cfg == nil || !cfg.GitHub.HasApp() {
		return "", "", ""
	}
	info, err := os.Stat(githubAppTokenCachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return spoke.GitHubAppTokenStatusMissing, "", detail
		}
		return spoke.GitHubAppTokenStatusError, "", err.Error()
	}
	lastMintAt = info.ModTime().UTC().Format(time.RFC3339)
	if time.Since(info.ModTime()) > spoke.GitHubAppTokenStaleAfter {
		return spoke.GitHubAppTokenStatusStale, lastMintAt, detail
	}
	return spoke.GitHubAppTokenStatusOK, lastMintAt, ""
}

var githubHTTPStatusRe = regexp.MustCompile(`\b([1-5][0-9]{2})\b`)

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func githubAppStructuredFailure(state, detail string) (class string, httpStatus int) {
	switch strings.TrimSpace(state) {
	case github.AppStateNotInstalled.String():
		class = "not-installed"
	case github.AppStateWrongInstallation.String():
		class = "wrong-installation"
	case github.AppStateInsufficientPerms.String():
		class = "insufficient-permissions"
	case github.AppStateKeyMissing.String():
		class = "key-missing"
	case github.AppStateKeyInvalid.String():
		class = "key-invalid"
	case github.AppStateNoAppAssigned.String():
		class = "no-app-assigned"
	case github.AppStateRepoNotCovered.String():
		class = "repo-not-covered"
	case github.AppStateRepoMoved.String():
		class = "repo-moved"
	case github.AppStateWriteForbidden.String():
		class = "write-forbidden"
	}
	if class == "" && strings.TrimSpace(detail) != "" {
		class = "token-error"
	}
	for _, m := range githubHTTPStatusRe.FindAllStringSubmatch(detail, -1) {
		if len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				httpStatus = n
			}
		}
	}
	if httpStatus != 0 {
		if (class == "token-error" || class == "not-installed") && httpStatus == http.StatusNotFound {
			class = "installation-not-found"
		}
	}
	return class, httpStatus
}

// githubAuth is the outcome of resolving this hive's GitHub credentials at
// startup. Every field is optional: a hive with no usable credentials is a
// legitimate, bootable state.
type githubAuth struct {
	// Client is nil when no credentials could be resolved. Callers must treat a
	// nil Client as "GitHub is unavailable", never as a fatal condition.
	Client *github.Client
	// AppAuth is non-nil only when App auth was successfully initialized.
	AppAuth *github.AppAuth
	// Failure, when non-empty, is the operator-facing reason there is no
	// working GitHub client. It is shown in the dashboard's GitHub App banner.
	Failure string
	// State classifies Failure so the banner and the hub's journey nudges can
	// tell an operator-side fault (a key that was never delivered) from a
	// user-actionable one (the App is not installed). Escalating against an
	// owner for a key WE failed to provision is precisely the mistake
	// github.AppAuthState exists to prevent.
	State github.AppAuthState
	// TokenScopes is the boot-time PAT scope probe (see
	// github.CheckTokenScopes). Zero value / ScopeStatusSkipped on the App
	// path. It is advisory: a ScopeStatusMissing result never blocks startup,
	// it only gives github_auth a specific detail string instead of leaving the
	// operator to decode a runtime 403.
	TokenScopes github.ScopeResult
}

// initGitHubAuth resolves this hive's GitHub credentials.
//
// It NEVER exits the process. A hive that cannot authenticate to GitHub must
// still boot and serve its dashboard, because the dashboard is the only place
// its owner can see what is wrong and fix it. Exiting here — which is what this
// code used to do on a key-read failure — happens before the HTTP listener
// binds, so the pod crashloops with the diagnosis visible only in kubectl logs:
// the hub shows the hive offline, the heartbeat never starts, and a rollout
// hangs forever because the new pod never goes Ready.
//
// The placeholder app_id is the reason that path was reachable at all. See
// config.PlaceholderAppID.
func initGitHubAuth(ctx context.Context, cfg *config.Config, logger *slog.Logger) githubAuth {
	var out githubAuth
	appKeyFile := appKeys.Resolve(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID)

	// HasApp() rejects config.PlaceholderAppID. Build AppAuth as soon as a real
	// app_id and key are present, even when installation_id is still empty: the
	// App JWT is exactly what automatic installation-ID discovery needs.
	if cfg.GitHub.HasApp() {
		appAuth, err := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, appKeyFile, logger, cfg.GitHub.ResolvedAPIURL())
		if err != nil {
			// A genuinely-configured App whose key is missing or malformed is a
			// real, actionable fault — but not a reason to refuse to boot.
			out.Failure = describeAppKeyFailure(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), appKeyFile, err)
			// Both states are operator-actionable: the hive's owner cannot
			// deliver a key. Absent vs. unparseable is the distinction the hub
			// needs to tell "never pushed" from "pushed something broken".
			out.State = github.AppStateKeyInvalid
			if errors.Is(err, fs.ErrNotExist) {
				out.State = github.AppStateKeyMissing
			}
			logger.Error("GitHub App auth unavailable — starting in dashboard-only mode",
				"app_id", cfg.GitHub.AppID,
				"installation_id", cfg.GitHub.InstallationID,
				"key_file", appKeyFile,
				"state", out.State.String(),
				"detail", out.Failure,
				"error", err,
			)
		} else {
			out.AppAuth = appAuth
		}
	}

	if out.AppAuth != nil {
		logger.Info("using GitHub App authentication", "app_id", cfg.GitHub.AppID)
		if cfg.GitHub.InstallationID == 0 {
			out.Failure = "The GitHub App is configured but has no installation. Install the app on your org; this hive will discover the installation automatically."
			out.State = github.AppStateNotInstalled
			logger.Warn("GitHub App configured without installation_id — starting in dashboard-only mode while auto-discovery polls")
			return out
		}
		// Correct a stale/wrong installation_id BEFORE building the client, so
		// the very first token this process mints is scoped to the right org
		// rather than 403ing on every write until the self-heal tick runs.
		healGitHubAppInstallation(ctx, out.AppAuth, cfg, logger)
		out.Client = github.NewClientFromAppWithBotLogin(out.AppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
		// Per-repo pause (#6203). A live predicate over the shared config, so a
		// pause taken in the dashboard narrows the very next enumeration and
		// automerge sweep without a restart.
		out.Client.SetRepoPausedFunc(cfg.IsRepoPaused)
		// Per-repo custom agents (#6204). A live predicate over the shared
		// config, so the hive-open-pr / hive-merge / hive-open-issue relays
		// refuse an out-of-scope request without a restart.
		out.Client.SetAgentRepoScopeFunc(cfg.AgentServesRepo)
		startDocsTokenRefresh(ctx, cfg, appKeyFile, logger)
		return out
	}

	ghToken := cfg.GitHub.Token
	if ghToken == "" {
		ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
	}
	switch {
	case ghToken != "":
		out.Client = github.NewClient(ghToken, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.ResolvedAPIURL())
		out.Client.SetRepoPausedFunc(cfg.IsRepoPaused)        // #6203, see the App branch above
		out.Client.SetAgentRepoScopeFunc(cfg.AgentServesRepo) // #6204, see the App branch above
		// PAT path only: introspect the token's granted scopes ONCE, here, so a
		// too-narrow token is named at boot instead of surfacing hours later as
		// a generic 403 inside an agent — or, worse, as an empty backlog that
		// looks like "no work to do". Fail-soft and bounded (see
		// CheckTokenScopes); it never blocks or fails startup. The App branch
		// above returns before this point: Apps have permissions, not scopes.
		// An unset acmm_level is passed through as github.ACMMLevelUnset rather
		// than inferACMMLevel's L1 default: L1 requires no scopes at all, so
		// defaulting to it would silently suppress every warning on exactly the
		// hives whose intent we cannot read. See ACMMLevelUnset.
		scopeLevel := github.ACMMLevelUnset
		if cfg.ACMMLevel != nil {
			scopeLevel = *cfg.ACMMLevel
		}
		out.TokenScopes = out.Client.LogTokenScopeCheck(ctx, logger, scopeLevel)
	case out.Failure != "":
		// Real App, unusable key. Already logged; leave Client nil so nothing
		// tries to act on GitHub with credentials that do not work.
	case cfg.GitHub.IsPlaceholderApp():
		// OPERATOR-actionable, not user-actionable. This hive was provisioned as
		// a placeholder and never assigned a real app_id, so the JWT it signs
		// names no App and fails before any installation is consulted. Nothing
		// the owner enters in the installation-ID box can fix it — reporting this
		// as AppStateNotInstalled sent owners to re-install an App that was
		// already installed and to re-enter an installation_id that was already
		// correct.
		out.Failure = "This hive was never assigned a GitHub App ID: it still carries the placeholder github.app_id, " +
			"which does not name a real GitHub App. Setting an installation ID cannot resolve this — " +
			"the hub operator must assign the real App ID."
		out.State = github.AppStateNoAppAssigned
		logger.Warn("placeholder github.app_id — hive starting in dashboard-only mode",
			"placeholder_app_id", config.PlaceholderAppID,
			"installation_id", cfg.GitHub.InstallationID,
		)
	case cfg.GitHub.AppID != 0:
		out.Failure = "The GitHub App is configured but has no installation. Install the app on your org to enable agents."
		out.State = github.AppStateNotInstalled
		logger.Warn("GitHub App configured without credentials — hive starting in dashboard-only mode. Install the app and provide installation_id + key to enable agents.")
	default:
		// Neither a token nor any app_id at all. config.validate() rejects this
		// at load, so reaching it means the config was mutated afterwards.
		// Still a degraded boot rather than an exit: the dashboard is where an
		// operator fixes it.
		out.Failure = "No GitHub credentials configured. Set github.token, or github.app_id plus an App installation."
		logger.Error("no GitHub token configured (set github.token or github.app_id in config) — starting in dashboard-only mode")
	}
	return out
}

// startDocsTokenRefresh mints and periodically refreshes a token for the
// separate docs-org installation, when one is configured. A failure here is
// always non-fatal: the docs org is an add-on, not this hive's primary auth.
func startDocsTokenRefresh(ctx context.Context, cfg *config.Config, appKeyFile string, logger *slog.Logger) {
	if cfg.GitHub.DocsInstallationID == 0 {
		return
	}
	docsAuth, err := github.NewAppAuthWithCache(
		cfg.GitHub.AppID, cfg.GitHub.DocsInstallationID,
		appKeyFile, github.DocsTokenCachePath, logger, cfg.GitHub.ResolvedAPIURL(),
	)
	if err != nil {
		logger.Warn("failed to init docs org token", "error", err)
		return
	}
	go func() {
		// Mint the initial docs token in the BACKGROUND, not on the startup
		// path. The docs org is an add-on; blocking here on a docs-installation
		// mint that hangs (unreachable/uninstalled GHE) would delay the whole
		// process reaching MarkReady — the #2439 readiness-stall pattern. The
		// mint is bounded (tokenMintTimeout) and non-fatal regardless.
		if _, err := docsAuth.Token(ctx); err != nil {
			logger.Warn("failed to generate initial docs org token", "error", err)
		} else {
			logger.Info("docs org token cached", "installation_id", cfg.GitHub.DocsInstallationID)
		}
		const docsTokenRefreshInterval = 45 * time.Minute
		ticker := time.NewTicker(docsTokenRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := docsAuth.Token(ctx); err != nil {
					logger.Warn("docs token refresh failed", "error", err)
				}
			}
		}
	}()
}

// buildAgentMinter constructs the opt-in per-agent mint credential issuer from
// config. It loads (or creates) the signing key at cfg.Mint.KeyPath, builds a
// Minter with the configured issuer/hive-id/TTL, and wraps it as an AgentMinter.
// Callers gate on cfg.Mint.Enabled before calling. The signing key path comes
// from config (never hardcoded); a missing key file is created with 0600 perms
// by the mint package.
func buildAgentMinter(cfg *config.Config, logger *slog.Logger) (*mint.AgentMinter, error) {
	if cfg.Mint.KeyPath == "" {
		return nil, fmt.Errorf("mint.key_path is required when mint is enabled")
	}
	if cfg.Mint.Issuer == "" {
		return nil, fmt.Errorf("mint.issuer is required when mint is enabled")
	}
	key, err := mint.LoadOrCreateKey(cfg.Mint.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading mint signing key: %w", err)
	}
	maxTTL := time.Duration(cfg.Mint.MaxTTLSeconds) * time.Second
	minter, err := mint.NewMinter(key, cfg.Mint.Issuer,
		mint.WithHiveID(cfg.HiveID),
		mint.WithMaxTTL(maxTTL),
	)
	if err != nil {
		return nil, fmt.Errorf("building minter: %w", err)
	}
	logger.Debug("mint signing key loaded", "key_path", cfg.Mint.KeyPath)
	// ttl<=0 lets the minter fall back to its configured max on each Mint.
	return mint.NewAgentMinter(minter, maxTTL), nil
}

// reporterName identifies this spoke PROCESS to the hub (HeartbeatPayload
// Reporter) as "<hostname>/<pid>". In-cluster the hostname IS the pod name;
// the PID suffix exists because two hive processes were observed beating from
// ONE pod (#2453, #2496) — bare os.Hostname() made them indistinguishable, so
// the hub's duplicate-spoke detector (noteReporter) stayed silent through 11+
// alternating beats while the dashboard flipped state every beat. With the
// PID attached, same-pod duplicates alternate as "pod-x/123 ↔ pod-x/456" and
// the detector names the exact culprits. The hub compares the whole string as
// an opaque identity, so old spokes sending bare hostnames keep working; a
// hostname failure yields empty, which the hub reads as "too old / cannot
// report", never as data.
var reporterName = func() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return ""
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}()

// ── Process singleton (#2453, #2496) ────────────────────────────────────
//
// A spoke pod ran TWO hive processes concurrently: startedAt alternated
// between two values beat to beat, the hub saw 4-5 heartbeat senders behind
// one pod name, and one process's stale App-auth snapshot flipped the
// dashboard every beat. The per-process StartHeartbeat guard (#2462) cannot
// see across processes; only a kernel-level mutual exclusion can. main()
// takes an exclusive flock before doing anything else — the second process
// logs the holder's PID and exits instead of becoming a shadow sender. The
// flock releases automatically on process death, so a crashed holder never
// blocks a legitimate restart.
const (
	// singletonLockEnv overrides the lock file location (tests, unusual
	// container layouts). Empty means the default resolution below.
	singletonLockEnv = "HIVE_SINGLETON_LOCK"
	// singletonLockDisable is the env value that skips the guard entirely —
	// an escape hatch for deliberately running two instances on one host
	// (local development with distinct configs).
	singletonLockDisable = "off"
	// singletonLockDir is the preferred lock directory: container-local tmpfs
	// created by the entrypoint, shared by every process in the container but
	// by NOTHING outside it. Deliberately NOT /data — the PVC is shared
	// across PODS during a rolling update (maxSurge=1), and a pod-spanning
	// lock would deadlock the surge pod against the terminating one.
	singletonLockDir = "/var/run/hive-metrics"
	// singletonLockName is the lock file's basename in whichever directory is
	// chosen.
	singletonLockName = "hive.singleton.lock"
	// duplicateProcessExitCode marks an exit caused by refusing to run beside
	// an already-running hive process. Distinct from 0 (clean) and 17
	// (selfUpgradeFailureExitCode) so the refusal is legible in the
	// container's termination state.
	duplicateProcessExitCode = 18
)

// singletonLockPath resolves where the process singleton lock lives. Every
// process in a container resolves the same path (the filesystem is shared),
// so the choice is deterministic where it matters; the temp-dir fallback
// covers bare-metal/dev runs where the entrypoint never created the
// container dir.
func singletonLockPath() string {
	if p := os.Getenv(singletonLockEnv); p != "" {
		return p
	}
	if st, err := os.Stat(singletonLockDir); err == nil && st.IsDir() {
		return filepath.Join(singletonLockDir, singletonLockName)
	}
	return filepath.Join(os.TempDir(), singletonLockName)
}

// spokeRestartMinUptime is how old this process must be before it acts on a
// hub-delivered restart. The hub delivers the instruction to every beat in a
// multi-minute window so all instances hear it; without this guard the
// restarted process would come back inside the same window and restart again.
const spokeRestartMinUptime = 10 * time.Minute

func main() { runBoot(&boot{}, defaultBootSequence()) }

// bootConfig handles the CLI fast paths, flag parsing, the process
// singleton, config load, logger, tracing and the signal handler. It
// returns false when main() should return without booting (the --version
// fast path and HIVE_MODE=hub); the fatal paths os.Exit as before.
func (b *boot) bootConfig() bool { return b.bootConfigWith(defaultBootConfigDeps()) }

// bootConfigWith is bootConfig with its process-level effects injected; see
// bootConfigDeps for what each seam stands in for.
func (b *boot) bootConfigWith(deps bootConfigDeps) bool {
	args := deps.args
	var err error
	// Startup order is intentionally linear and dependency-ordered:
	//  1. version/config/logging/process singleton
	//  2. config overlays, identity, GitHub auth/client, and dashboard server
	//  3. agent manager, persisted runtime state, governor, scheduler, and queues
	//  4. hub heartbeat/self-upgrade wiring, notification sinks, and background lanes
	//  5. steady-state tick loop: watchdog, eval cycle, rotation, automerge, and persistence
	//
	// Keep new subsystem constructors on the existing *wire.go pattern and insert
	// them at the matching point above; do not move side effects across steps.
	// --version fast path, before any flag parsing or startup work: the CI
	// smoke test (and operators) probe the binary with `hive --version`; the
	// standard flag set would reject it ("flag provided but not defined").
	// dd's full CLI dispatcher handles this via a version subcommand; this is
	// the minimal equivalent for the v4 line.
	if handled, code := dispatchSubcommand(args[1:], deps.stdout, deps.stderr); handled {
		if code == 0 {
			// Success returns rather than os.Exit(0) so main() unwinds the
			// (still empty) cleanup stack and the path stays testable.
			return false
		}
		deps.exit(code)
		return false
	}
	b.startTime = time.Now()
	// The heartbeat identity fields live on boot since the boot split, but
	// nothing assigned them: v5 spokes beat with Reporter "" (the hub reads
	// that as "too old to report") and a zero StartedAt, and the restart
	// min-uptime guard measured uptime from year 1 so it never fired. Set
	// them here, alongside startTime, which is the same instant they mean.
	b.reporterName = reporterName
	b.processStartedAt = b.startTime
	b.configPath = deps.parseFlags(resolveDefaultConfigPath(deps.getenv(hiveConfigEnv)))
	// Canonicalize gitShort to the standard 7-char short SHA the hub stores and
	// compares against. The Dockerfile builds it with `--short=7`, but git can
	// still return more chars when 7 isn't unique; trim so what we report to the
	// hub is always the same length it stores (no short-vs-full mismatch).
	gitShort = canonicalGitShort(gitShort)
	dashboard.SetGitVersion(gitHash, gitShort)
	dashboard.SetFleetReportBuildInfo(reportedVersion(), gitShort)
	dashboard.SetGitBranch(gitBranch)
	// Resolve channel and tracking together from the cached Deployment image.
	// No authoritative image outside a cluster means tracking stays unknown.
	dashboard.SetDeploymentImageSource(deps.selfImage)

	b.logger = slog.New(logscrub.NewHandler(slog.NewJSONHandler(deps.stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.SetDefault(b.logger)

	// Process singleton: refuse to become a second hive process in this
	// container (#2453, #2496). Two concurrent processes beat as the same pod,
	// alternate registry state every beat, and are invisible to both the
	// in-process StartHeartbeat guard and the hub's duplicate-spoke detector.
	// The flock releases on process death, so this never blocks a restart.
	if deps.getenv(singletonLockEnv) != singletonLockDisable {
		lockPath := singletonLockPath()
		releaseLock, lockErr := deps.acquireLock(lockPath)
		if lockErr != nil {
			b.logger.Error("another hive process is already running in this container — refusing to start a duplicate (#2453, #2496)",
				"lock", lockPath,
				"pid", os.Getpid(),
				"error", lockErr.Error(),
			)
			deps.exit(duplicateProcessExitCode)
			return false
		}
		// Held for the process lifetime; the kernel releases it on exit. Kept
		// referenced so the *os.File is never garbage-collected (a collected
		// file closes its descriptor, which would silently drop the flock).
		b.cleanup.push(releaseLock)
	}

	// Auto-update visibility (#7092): before anything reads the upgrade marker,
	// reconcile it against the commit we actually booted on. A marker whose
	// target IS the running commit means the last instructed upgrade LANDED —
	// record that success durably and clear the marker, so the dashboard can
	// show "attempted and succeeded" instead of silently losing the success the
	// moment the new image boots.
	deps.reconcileUpgradeOutcome(gitShort, b.logger)

	// Clear stale upgrade marker if the current SHA differs from the marker's
	// current_sha — this means the upgrade succeeded and the marker is from a
	// previous version.
	if markerData, err := deps.readUpgradeMarker(); err == nil {
		m := parseUpgradeMarker(markerData)
		if judgeUpgradeMarker(m, gitShort) == upgradeLanded {
			// We booted on a different SHA than the one that requested the
			// upgrade: it landed. Drop the marker so the attempt budget resets.
			if err := deps.clearUpgradeMarker(); err != nil && !os.IsNotExist(err) {
				b.logger.Warn("failed to clear stale upgrade marker", "path", upgradeMarkerPath, "error", err)
			}
			b.logger.Info("upgrade landed, cleared marker",
				"current", gitShort, "previous", m.CurrentSHA, "target", m.TargetSHA)
		} else {
			// Same SHA as the attempt that ran before this boot: the image never
			// changed, so that attempt FAILED. Say so at startup — previously
			// this restart looked completely routine in the logs.
			b.logger.Error("previous self-upgrade attempt did not land (still on the same image)",
				"current", gitShort,
				"target", m.TargetSHA,
				"attempts", m.Attempts,
				"last_error", m.LastError,
			)
		}
	}

	if deps.getenv("HIVE_MODE") == "hub" {
		deps.runHub(b.logger, b.configPath)
		return false
	}

	var cancel context.CancelFunc

	b.wireBootClosures()

	// Use LoadWithDashboardOverlay (not plain Load) so the dashboard overlay's
	// removed_agents tombstones are populated into cfg.RemovedAgents at boot —
	// BEFORE the startup ApplyPack below reconciles the ACMM roster. Plain Load
	// never reads the overlay, so on restart the tombstone was invisible and
	// ApplyPack re-added deleted pack agents (brainstorm/guide) every time
	// (#2439). Same return signature as Load; falls back to the seed when no
	// overlay exists or the pod is not in Kubernetes.

	b.cfg, err = deps.loadConfig(b.configPath)
	if err != nil {
		b.logger.Error("failed to load config", "error", err)
		deps.exit(1)
		return false
	}

	// Reconfigure logger with rolling file output
	b.logger = deps.fileLogger(b.cfg)
	slog.SetDefault(b.logger)

	// Load or generate a unique Hive ID for this instance
	b.cfg.HiveID = deps.hiveID(b.logger)
	_ = os.Setenv("HIVE_ID", b.cfg.HiveID) // valid key/value; Setenv cannot fail on Unix

	// Observability (#2439): report the removed-agents tombstone LoadWithDashboardOverlay
	// adopted from the dashboard overlay at boot, BEFORE the startup ApplyPack below. On
	// a non-sticking-removal report this line is the first check — an empty set here on a
	// hive that removed an agent means the tombstone did not persist across the restart.
	b.logger.Info("boot: loaded removed-agents tombstone",
		"hive_id", b.cfg.HiveID,
		"count", len(b.cfg.RemovedAgents),
		"agents", b.cfg.RemovedAgents,
	)

	// Surface config provenance: when the persisted runtime config exists, init
	// containers restore it over the ConfigMap seed on restart, so edits made
	// only to the seed (or only to the live file) silently lose to it.
	//
	// Checks the legacy name too: during the migration a hive may still carry
	// only /data/hive.yaml.bak, and the whole point of this log line is to warn
	// that such a file is shadowing the seed. Note this path was previously
	// built as *configPath + ".bak", which only ever resolved to the real
	// location when HIVE_CONFIG happened to live under /data — a literal grep
	// for "hive.yaml.bak" could not find it either.
	for _, runtimePath := range []string{config.RuntimeConfigFile, config.RuntimeConfigFileLegacy} {
		if _, statErr := deps.stat(runtimePath); statErr == nil {
			b.logger.Info("persisted runtime config present — restored over the seed on pod restart; fixes must land in the live config so the next save refreshes it",
				"path", runtimePath,
				"github_installation_id", b.cfg.GitHub.InstallationID,
			)
			break
		}
	}

	// HIVE_CONFIG names a DIFFERENT file from the one we actually loaded.
	//
	// This is only ever the entrypoint's read-only escape hatch failing to
	// land. When the config path cannot be written, entrypoint.sh exports
	// HIVE_CONFIG=/data/hive.yaml.runtime and logs "config path is read-only —
	// using ... directly"; HIVE_CONFIG is read above only as the DEFAULT of
	// -config, and the image's CMD passes that flag explicitly
	// (Dockerfile: CMD ["--config", "/etc/hive/hive.yaml"]), so the explicit
	// value won and the redirect did nothing.
	//
	// That is #4973, and it is silently destructive rather than merely wrong:
	// the stale file loads, then Config.Save() writes the whole in-memory
	// config back over /data/hive.yaml.runtime, destroying the state the
	// operator had persisted there. An ACMM level set from the dashboard came
	// back at its provisioned value after a restart, twice, with /data intact.
	//
	// entrypoint.sh now appends `--config "$HIVE_CONFIG"` to the argv so the
	// last-occurrence-wins rule in flag.Parse carries the redirect, which means
	// this branch should be unreachable. It is kept — at WARN, naming both
	// paths — because the failure it reports is invisible from every other
	// vantage point: /api/config/provenance reads HIVE_CONFIG directly, so it
	// reports the file the entrypoint chose while the process runs on the one
	// it did not, and the two disagree with no way to tell from the outside.
	if envCfg := deps.getenv(hiveConfigEnv); configPathDisagrees(envCfg, b.configPath) {
		b.logger.Warn("config path disagreement: HIVE_CONFIG names a different file than the one loaded — an explicit -config (the image CMD) outranked the entrypoint's redirect; persisted state in HIVE_CONFIG may be overwritten by the next save",
			"hive_config_env", envCfg,
			"loaded_config", b.configPath,
		)
	}

	b.logger.Info("hive starting",
		"org", b.cfg.Project.Org,
		"repos", b.cfg.Project.Repos,
		"agents", len(b.cfg.Agents),
		"hive_id", b.cfg.HiveID,
	)
	// Per-repo pause (#6203). Say it out loud at boot: a repo that is quiet for
	// an unexplained reason is its own support burden, and the operator reading
	// this log after a restart is exactly the person who needs to know the hive
	// came back up still holding a pause.
	if pausedRepos := b.cfg.PausedRepoNames(); len(pausedRepos) > 0 {
		for _, repo := range pausedRepos {
			rp, _ := b.cfg.RepoPauseFor(repo)
			attrs := []any{"repo", config.QualifyRepo(b.cfg.Project.Org, repo)}
			if rp.By != "" {
				attrs = append(attrs, "paused_by", rp.By)
			}
			if rp.At != nil && !rp.At.IsZero() {
				attrs = append(attrs, "paused_at", rp.At.UTC().Format(time.RFC3339))
			}
			if rp.Reason != "" {
				attrs = append(attrs, "reason", rp.Reason)
			}
			b.logger.Info("repo paused — agents will not write to it or be handed work on it", attrs...)
		}
	}
	for _, warning := range config.PausedRepoWarnings(b.cfg) {
		b.logger.Warn("per-repo pause config", "issue", warning)
	}
	// Per-repo custom agents (#6204). Name the scoped agents at boot: which
	// agents exist is now a per-repo answer, and an operator debugging "why did
	// nothing happen on that repo" needs the roster composition in the same log
	// they already read. A scope that matches nothing is a warning, not fatal —
	// see AgentRepoScopeWarnings.
	for _, name := range b.cfg.RepoScopedAgents() {
		b.logger.Info("agent is repo-scoped — it serves only these repos",
			"agent", name, "declared", b.cfg.AgentRepoScope(name), "watched", b.cfg.ReposForAgent(name))
	}
	for _, warning := range config.AgentRepoScopeWarnings(b.cfg) {
		b.logger.Warn("per-repo agent scope", "issue", warning)
	}
	startupRepoTargetIssue := config.ValidateRepoTargets(b.cfg)
	if startupRepoTargetIssue != nil {
		b.logger.Warn("repo target misconfigured — owner action required",
			"issue", startupRepoTargetIssue.Message,
			"hive_id", b.cfg.HiveID,
			"org", b.cfg.Project.Org,
			"repos", b.cfg.Project.Repos,
			"primary_repo", b.cfg.Project.PrimaryRepo,
		)
	}
	b.repoTargetMisconfigured = func() bool {
		return config.ValidateRepoTargets(b.cfg) != nil
	}
	b.repoTargetIssueMessage = func() string {
		if issue := config.ValidateRepoTargets(b.cfg); issue != nil {
			return issue.Message
		}
		return ""
	}

	b.ctx, cancel = context.WithCancel(context.Background())
	b.cleanup.push(func() { cancel() })

	// Initialize OpenTelemetry tracing. Off by default: with no otel block
	// (or otel.enabled=false) this installs a no-op provider with zero export
	// overhead. Never fatal — a tracing setup error must not stop hive.
	otelCfg := b.cfg.EffectiveOTel()
	traceShutdown, traceErr := deps.initTracing(b.ctx, tracing.Config{
		Enabled:     otelCfg.Enabled,
		Endpoint:    otelCfg.Endpoint,
		Headers:     otelCfg.Headers,
		ServiceName: otelCfg.ServiceNameOrDefault(),
		Insecure:    otelCfg.Insecure,
		SampleRatio: otelCfg.SampleRatio,
		HiveID:      b.cfg.HiveID,
		Branch:      b.cfg.Policies.Branch,
		// Reach anchors (#3973): gitShort is the ldflags-baked commit of THIS
		// binary (already canonicalized to 7 chars above), and the image ref is
		// the Deployment-declared image (cached — warmed by the release-channel
		// read at startup; "" outside a cluster). Spans attribute to the code
		// that actually runs, not to the merge/publish event (#3816).
		Commit: gitShort,
		Image:  deps.selfImage(),
	})
	if traceErr != nil {
		b.logger.Warn("tracing init failed; continuing without tracing", "error", traceErr)
	} else if otelCfg.Enabled {
		b.logger.Info("otel tracing enabled", "endpoint", otelCfg.Endpoint, "service_name", otelCfg.ServiceNameOrDefault())
	}
	// Component reach counters (#3993): resume this commit's counters from the
	// PVC before any span can start. Independent of the otel block above —
	// counters increment with or without an exporter (design D2 of #3973), so
	// this runs unconditionally and a load failure only costs history, never
	// counting. Counters persisted by a DIFFERENT commit are dropped inside
	// LoadReachState: a new binary starts fresh keys naturally.
	if err := deps.loadReachState(gitShort, b.logger); err != nil {
		b.logger.Warn("reach state load failed; starting with fresh counters", "error", err)
	}
	b.cleanup.push(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), traceShutdownTimeout)
		defer shutdownCancel()
		if err := traceShutdown(shutdownCtx); err != nil {
			b.logger.Warn("tracing shutdown error", "error", err)
		}
	})

	sigCh := make(chan os.Signal, 1)
	deps.notifySignals(sigCh)
	// preShutdownHooks run in the signal handler before the context is canceled,
	// in registration order, while every connection and tmux server is still
	// live. Registrations happen later in startup, once the subsystems they
	// touch exist.
	//
	// This was a single atomic.Pointer[func()] until kubestellar/hive#5390. A
	// lone pointer makes registration DESTRUCTIVE: the second Store silently
	// discards the first hook, and the loss is invisible — nothing fails, a
	// shutdown side effect simply stops happening. That is precisely the trap
	// the WebSocket drain walked into, since the slot was already held by
	// #4296's kick-log archive. A slice makes adding a hook additive by
	// construction, so the next one cannot repeat the mistake.
	go func() {
		sig := <-sigCh
		b.logger.Info("received signal, shutting down", "signal", sig)
		b.preShutdownHooks.run()
		cancel()
	}()
	return true
}

// wireBootClosures installs the late-bound accessors the later phases and
// the heartbeat read through *boot (fleet stats, dashboard URLs, the
// dashboard.Dependencies builder, the session-prune janitor). They only
// dereference b at call time, so bootConfig wires them before the
// collaborators exist; tests that drive a single phase call this directly.
func (b *boot) wireBootClosures() {
	b.heartbeatFleetStats = func() (*int, *int, *int, string) {
		var prsMerged, prsRejected, cvesClosed *int
		collectedAt := ""
		if fc, ok := b.fleetStatsCollector.Snapshot(); ok {
			m, rj, cv := fc.PRsMerged, fc.PRsRejected, fc.CVEsClosed
			prsMerged, prsRejected, cvesClosed = &m, &rj, &cv
			if t := b.fleetStatsCollector.CollectedAt(); !t.IsZero() {
				collectedAt = t.UTC().Format(time.RFC3339)
			}
		}
		return prsMerged, prsRejected, cvesClosed, collectedAt
	}

	b.heartbeatRepoActivity = func() ([]spoke.RepoActivityWire, string, int, int) {
		if asnap, ok := b.activityCollector.Snapshot(); ok {
			collectedAt := ""
			if t := b.activityCollector.CollectedAt(); !t.IsZero() {
				collectedAt = t.UTC().Format(time.RFC3339)
			}
			return buildRepoActivityWire(asnap.Repos), collectedAt, asnap.WindowHours, asnap.CountWindowHours
		}
		return nil, "", 0, 0
	}

	b.heartbeatBudgetWindow = func() (*int64, *int64, *bool, string, string) {
		budget := b.gov.GetBudget()
		limit := budget.WeeklyLimit
		ignored := budget.IgnoreAll
		if start, end, ok := b.gov.BudgetWindow(); ok {
			spend := budget.CurrentSpend
			return &spend, &limit, &ignored, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)
		}
		return nil, &limit, &ignored, "", ""
	}

	b.heartbeatACMMLevel = func() int {
		acmmLvl := 0
		if b.cfg.ACMMLevel != nil {
			acmmLvl = *b.cfg.ACMMLevel
		}
		return acmmLvl
	}

	b.heartbeatAgents = func(govState governor.State, currentMode string, includeBlockingChecks bool) []spoke.AgentSummary {
		statuses := b.agentMgr.AllStatuses()
		agents := make([]spoke.AgentSummary, 0, len(statuses))
		for name, proc := range statuses {
			mode := ""
			if ac, ok := b.cfg.Agents[name]; (ok && ac.OnDemand) || b.onDemandFromPack[name] {
				mode = "on_demand"
			}
			if includeBlockingChecks {
				agents = append(agents, spoke.NewAgentSummary(name, string(proc.State), mode,
					spoke.AgentActivityFor(b.agentMgr, b.cfg, govState, currentMode, name, proc, b.onDemandFromPack)))
				continue
			}
			act := spoke.AgentActivity{
				Paused:         proc.Paused,
				PausedTrigger:  proc.PausedTrigger,
				PausedReason:   proc.PausedReason,
				PausedBy:       proc.PausedBy,
				PausedAt:       proc.PausedAt,
				NeedsLogin:     proc.NeedsLogin,
				QuotaExhausted: proc.QuotaExhausted,
				LastActivityAt: proc.LastPaneChange,
				KickInterval:   spoke.HeartbeatKickInterval(govState, name, proc, b.onDemandFromPack),
				Backend:        proc.Config.Backend,
				Enabled:        proc.Config.Enabled,
				CanOpenIssue:   proc.LaunchedMode.CanCreateIssues(),
				CanOpenPR:      proc.LaunchedMode.CanPush(),
				CanMerge:       proc.LaunchedMode.CanMerge(),
				Restarts: spoke.AgentRestartTelemetry{
					Total:      proc.RestartCount,
					LastReason: proc.LastRestartReason,
				},
				StartBlockedReason:   proc.StartFailureReason,
				StartFailureReason:   proc.StartFailureReason,
				StartFailureCount:    proc.StartFailureCount,
				StartFailureLastAt:   proc.StartFailureLastAt,
				StartBlocked:         proc.StartBlocked,
				StartFailureExitCode: proc.StartFailureExitCode,
				StartFailureSignal:   proc.StartFailureSignal,
			}
			if proc.BackendOverride != "" {
				act.Backend = proc.BackendOverride
			}
			if proc.StartedAt != nil {
				act.StartedAt = *proc.StartedAt
			}
			if b.cfg != nil {
				onDemandAgent := false
				if ac, ok := b.cfg.Agents[name]; ok {
					onDemandAgent = ac.OnDemand
					act.Enabled = ac.Enabled
				}
				act.ExpectedActive = b.cfg.ExpectedActive(name, currentMode, onDemandAgent, b.onDemandFromPack)
			}
			agents = append(agents, spoke.NewAgentSummary(name, string(proc.State), mode,
				act))
		}
		return agents
	}

	b.dashboardURLForFreshHeartbeat = func() string {
		if b.cfg.Hub.DashboardURL != "" {
			return b.cfg.Hub.DashboardURL
		}
		if b.cfg.HiveID != "" && b.cfg.Hub.URL != "" {
			if u, err := url.Parse(b.cfg.Hub.URL); err == nil && u.Host != "" {
				return fmt.Sprintf("https://%s.%s", b.cfg.HiveID, u.Host)
			}
		}
		return fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port)
	}

	b.leaderboardForHeartbeat = func() []spoke.LeaderboardEntry {
		lb := b.dashSrv.LeaderboardForHub()
		out := make([]spoke.LeaderboardEntry, len(lb))
		for i, e := range lb {
			out[i] = spoke.LeaderboardEntry{
				GitHubUsername: e.GitHubUsername,
				AvatarURL:      e.AvatarURL,
				TrustTier:      e.TrustTier,
				TasksCompleted: e.TasksCompleted,
				TasksFailed:    e.TasksFailed,
				Active:         e.Active,
				CurrentTask:    e.CurrentTask,
			}
		}
		return out
	}

	b.ownerForHeartbeat = func() string {
		if td, err := os.ReadFile("/data/gh-user-token"); err == nil {
			tok := strings.TrimSpace(string(td))
			if tok != "" {
				// gh-user-token is a github.com OAuth token — validate its identity against github.com,
				// not the (possibly GHE) repo host.
				if u, err := github.ValidateToken(tok, b.cfg.GitHub.OAuthAPIURL()); err == nil {
					return u.Login
				}
			}
		}
		return ""
	}

	b.dashboardURLForHeartbeat = func() string {
		if b.cfg.Hub.DashboardURL != "" {
			return b.cfg.Hub.DashboardURL
		}
		// Prefer the host our OWN Route/Ingress actually serves. The synthesised
		// "<hiveID>.<hub host>" below is only correct when this spoke is fronted by
		// the hub's wildcard domain; pull-only clusters must report their live host.
		if host := spoke.SpokeServedHost(b.ctx); host != "" {
			return "https://" + host
		}
		if b.cfg.HiveID != "" && b.cfg.Hub.URL != "" {
			if u, err := url.Parse(b.cfg.Hub.URL); err == nil && u.Host != "" {
				return fmt.Sprintf("https://%s.%s", b.cfg.HiveID, u.Host)
			}
		}
		return fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port)
	}

	b.installMutationBoundary = func(client interface{ SetMutationBoundary(effects.Boundary) }) {
		if client == nil {
			return
		}
		client.SetMutationBoundary(b.mutationBoundary)
	}

	b.dashboardDependencies = func() *dashboard.Dependencies {
		return &dashboard.Dependencies{
			Config:   b.cfg,
			AgentMgr: b.agentMgr,
			// Provider gateways (#5565 slice 3): concrete openrouter/watsonx/
			// linearagent adapters behind the dashboard's consumer-defined
			// interfaces — this composition root is the only non-test place that
			// names the concrete types.
			Watsonx:              watsonxGateway{},
			OpenRouter:           openRouterGateway{},
			NewLinearAgent:       newLinearAgentGateway(b.logger),
			LinearStoredViewerID: linearStoredViewerID,
			MentionWebhook:       b.mentionWebhook,
			MentionStore:         b.mentionStore,
			DashboardChatSubmit: func(user, text string) (uint64, error) {
				if b.dashChat == nil {
					return 0, fmt.Errorf("dashboard chat is not configured")
				}
				return b.dashChat.Submit(user, text)
			},
			DashboardChatDrain: func(since uint64) []dashboard.ChatOutbound {
				if b.dashChat == nil {
					return nil
				}
				return dashboardChatDrain(b.dashChat, since)
			},
			Governor:         b.gov,
			GHClient:         b.ghClient,
			GHAppAuth:        b.appAuth,
			GHTokenScopes:    b.ghAuth.TokenScopes,
			Tokens:           b.tokenCollector,
			Knowledge:        b.knowledgeAPI,
			Inception:        b.inceptionEngine,
			Nous:             b.nousState,
			Scheduler:        b.sched,
			MetricsCollector: b.metricsCollector,
			RotationMgr:      b.rotationMgr,
			// #3972: hand the ACMM advisor the SAME cached fleet-stats collector
			// the heartbeat reads, so its merge-success signal reuses the existing
			// 30-minute collect loop instead of issuing a second GitHub fetch.
			FleetStats:            b.fleetStatsCollector,
			Activity:              b.activityCollector,
			RepoCost:              b.repoCostCollector,
			BeadSynthesizer:       b.beadSynth,
			BeadStores:            b.beadStores,
			BeadStoreLoadFailures: b.beadStoreLoadFailures,
			// RFC #4000 approval desk. Nil unless `tool_approval.enabled`, in which
			// case the Approvals panel renders as "not enabled".
			ApprovalDesk:  b.approvalDesk,
			ApprovalInbox: b.approvalInbox,
			Logger:        b.logger,
			Ctx:           b.ctx,
			RefreshFunc:   b.refreshDashboard,
			// #3768: give the contribute queue read access to the duplicate-PR
			// claim ledger, so an issue any open PR (hive-authored or a human
			// contributor's) already claims to fix is never offered to another
			// contributor. Lazy: the ledger loads on first use, same as the
			// eval-cycle guard.
			IssueClaimed: func(repo string, number int) (github.IssueClaim, bool) {
				return getClaimLedger(b.logger).Lookup(repo, number)
			},
			// #7871: and write access for the one verified fact a no_work_needed
			// verdict produces — the PR/commit that already settled the issue.
			RecordIssueClaim: func(c github.IssueClaim) error {
				return getClaimLedger(b.logger).Record(c)
			},
			// #7995: and read access to the same ledger's churn history, so an
			// issue that has already absorbed several merged or abandoned PRs
			// without settling is withheld for a maintainer rather than offered
			// for another round of the same.
			IssueChurn: func(repo string, number int) (github.IssueChurn, bool) {
				return getClaimLedger(b.logger).Churn(repo, number)
			},
			HookFire: func(ctx context.Context, p hooks.Payload) {
				hookDispatcher().Fire(ctx, p)
			},
			PersistFunc: func() {
				persistState(b.agentMgr, b.gov, b.cfg, spokeStatePath, b.logger, b.dashSrv, b.wd)
			},
			ReInitFunc: func() {
				initAgentConfigDrivenSystems(b.cfg)
			},
			ReviewConfigApplied: func(rc config.ReviewConfig) {
				if b.ghClient == nil {
					return
				}
				installReviewRelaySettings(b.ghClient, b.cfg, b.logger)
			},
			EnumerateFunc: func() {
				runEvalCycle(b.ctx, b.cfg, b.ghClient, b.gov, b.sched, b.agentMgr, b.dashSrv, b.notifier, b.beadStores, b.tokenCollector, b.metricsCollector, b.nousState, &b.lastActionable, b.advisoryStore, b.advisoryIssues, nil, b.approvalDesk, b.logger)
			},
			// The REPOSITORIES "Rescan" button. Unlike EnumerateFunc above — which
			// runs the WHOLE eval cycle, kicks included — this only refreshes what
			// the operator is looking at. See rescanRepos.
			RescanReposFunc: func(rescanCtx context.Context) (*github.ActionableResult, error) {
				return rescanRepos(rescanCtx, b.cfg, b.ghClient, &b.lastActionable, b.refreshDashboard, b.logger)
			},
			AdvisoryResetFunc: func(newPrimaryRepo string) {
				b.logger.Info("advisory reset: primary repo changed, creating new advisory issue", "repo", newPrimaryRepo)
				if b.ghClient != nil {
					num, err := b.ghClient.EnsureAdvisoryIssue(b.ctx, newPrimaryRepo)
					if err != nil {
						b.logger.Error("failed to create advisory issue on new primary repo", "repo", newPrimaryRepo, "error", err)
						if isGitHubRateLimitText(err) {
							b.logger.Warn("GitHub API rate limit hit during advisory issue creation", "repo", newPrimaryRepo)
						} else {
							// #2224 replaced error-string classification everywhere
							// else but missed this site, which raised the banner on
							// a bare "403"/"401" substring and recorded no state at
							// all — so the UI fell back to "App Not Installed" even
							// for an operator-side key fault. Classify properly.
							raise, diag, state := classifyGitHubAppFailure(b.ctx, b.ghClient.AppAuth(), b.cfg.Project.Org, b.logger)
							if raise {
								b.dashSrv.SetGitHubAppRequired(true)
								b.dashSrv.SetGitHubAppState(state.String())
								if diag != "" {
									b.dashSrv.SetGitHubAppPermIssue(diag)
								}
								b.logger.Warn("GitHub App authentication failed creating advisory issue",
									"repo", newPrimaryRepo, "state", state.String(),
									"operator_actionable", state.OperatorActionable())
							}
						}
					} else {
						b.advisoryIssues[newPrimaryRepo] = num
						_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
						b.dashSrv.SetGitHubAppRequired(false)
						b.dashSrv.ClearPendingGitHubAppInstall()
						b.logger.Info("advisory issue ready on new primary repo", "repo", newPrimaryRepo, "number", num)
					}
				}
			},
			ReinitGitHubFunc: func(newAppID, newInstallationID int64, keyFile string) error {
				newAppAuth, err := github.NewAppAuth(newAppID, newInstallationID, keyFile, b.logger, b.cfg.GitHub.ResolvedAPIURL())
				if err != nil {
					return fmt.Errorf("initializing app auth: %w", err)
				}
				newClient := github.NewClientFromAppWithBotLogin(newAppAuth, b.cfg.Project.Org, b.cfg.Project.Repos, b.logger, b.cfg.GitHub.BotLogin())
				if len(b.cfg.Governor.Labels.Exempt) > 0 {
					newClient.SetExemptLabels(b.cfg.Governor.Labels.Exempt)
					newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(b.cfg.Governor.Labels.AutoMerge))
				}
				newClient.SetIssueFilter(b.cfg.Project.IssueFilter)
				installReviewBots(newClient, b.cfg, b.logger)
				newClient.SetRepoPausedFunc(b.cfg.IsRepoPaused)        // #6203: a client rebuild must not un-pause repos
				newClient.SetAgentRepoScopeFunc(b.cfg.AgentServesRepo) // #6204: a client rebuild must not un-scope agents
				installReviewRelaySettings(newClient, b.cfg, b.logger)
				syncAutoMergePolicyToGitHubClient(b.cfg, newClient)
				b.ghClient = newClient
				b.installMutationBoundary(b.ghClient)
				b.appAuth = newAppAuth
				b.agentMgr.SetAppAuth(newAppAuth)
				// Deliver fresh per-agent scoped tokens to already-running agents
				// immediately — the periodic refresh loop only ticks every 40m,
				// far too long for agents whose caches are empty or stale (#4072).
				go b.agentMgr.RefreshAgentTokens(b.ctx)
				b.dashSrv.UpdateGitHubClient(newClient, newAppAuth)
				b.logger.Info("github client reinitialized via config API", "app_id", newAppID, "installation_id", newInstallationID)

				primaryRepo := b.cfg.Project.PrimaryRepo
				if primaryRepo == "" && len(b.cfg.Project.Repos) > 0 {
					primaryRepo = b.cfg.Project.Repos[0]
				}
				if primaryRepo != "" {
					num, advErr := b.ghClient.EnsureAdvisoryIssue(b.ctx, primaryRepo)
					if advErr != nil {
						b.logger.Warn("advisory issue creation failed after reinit", "repo", primaryRepo, "error", advErr)
					} else {
						b.advisoryIssues[primaryRepo] = num
						_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
						b.logger.Info("advisory issue ready after reinit", "repo", primaryRepo, "number", num)
					}
				}
				return nil
			},
			// Same key resolution as boot (initGitHubAuth) and the heartbeat apply
			// path: without it, the dashboard Set ID handler gated reinit on the
			// raw key_file, which is deliberately empty on hub-delivered per-app-id
			// keys (#2459).
			ResolveAppKeyFileFunc: func(configured string, appID int64) string {
				return appKeys.Resolve(configured, os.Getenv("GH_APP_KEY_FILE"), appID)
			},
		}
	}

	// session start, since the directory is read on startup.
	b.wireSessionPrune = func() {
		retentionDays := sessionprune.DefaultRetentionDays
		if b.cfg.Data.SessionRetentionDays != nil {
			retentionDays = *b.cfg.Data.SessionRetentionDays
		}

		dir := b.cfg.Data.CopilotSessionsDir
		if dir == "" {
			return
		}

		// An explicit 0 (or negative) in config disables the janitor. Say so once at
		// startup: an operator who has turned off the only thing bounding the PVC
		// should be able to see that in the log.
		if retentionDays <= 0 {
			b.logger.Info("session prune disabled by config", "dir", dir)
			return
		}

		maxAge := time.Duration(retentionDays) * 24 * time.Hour
		stop := make(chan struct{})

		go func() {
			// Run once at startup rather than waiting a full interval. A spoke that
			// restarts more often than the interval would otherwise never prune.
			runSessionPrune(b.logger, dir, maxAge)

			ticker := time.NewTicker(sessionPruneInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					runSessionPrune(b.logger, dir, maxAge)
				}
			}
		}()

		defer close(stop)
	}
}

// bootGitHub resolves GitHub credentials and tunes the client. b.ghClient
// and b.appAuth are set here and REASSIGNED later by three closures (the
// dashboard's ReinitGitHubFunc, the config watcher, and the heartbeat's
// app-config callback), which is why every reader goes through b.
func (b *boot) bootGitHub() { b.bootGitHubWith(defaultBootGitHubDeps()) }

// bootGitHubWith is bootGitHub with credential resolution injected; see
// bootGitHubDeps.
func (b *boot) bootGitHubWith(deps bootGitHubDeps) {
	b.ghAuth = deps.initGitHubAuth(b.ctx, b.cfg, b.logger)
	b.ghClient, b.appAuth = b.ghAuth.Client, b.ghAuth.AppAuth
	// appAuthFailure, when non-empty, is the operator-facing reason GitHub auth
	// is unavailable. It is surfaced through the existing
	// GitHubAppRequired/PermIssue banner rather than killing the process.
	b.appAuthFailure = b.ghAuth.Failure
	// appAuthState classifies that failure so the banner and the hub's journey
	// nudges can tell an operator-side fault (no key was ever delivered) from a
	// user-actionable one (the App is not installed).
	b.appAuthState = b.ghAuth.State
	if b.ghClient != nil && len(b.cfg.Governor.Labels.Exempt) > 0 {
		b.ghClient.SetExemptLabels(b.cfg.Governor.Labels.Exempt)
		b.ghClient.SetAutoMergeLabel(normalizedAutoMergeLabel(b.cfg.Governor.Labels.AutoMerge))
	}
	// Unconditional (nil-safe, zero value = no filtering): the issue filter
	// gates which issues become actionable at all, so it must be installed
	// even when no exempt labels are configured.
	b.ghClient.SetIssueFilter(b.cfg.Project.IssueFilter)
	installReviewBots(b.ghClient, b.cfg, b.logger)
}

// bootGovernor constructs the governor and scheduler, wires the prompt and
// definition resolvers, restores the persisted history the governor and
// dashboard sparklines seed from, and enables the knowledge primer.
func (b *boot) bootGovernor() {
	// The user write-token client (userGHClient) was removed: every GitHub write
	// — issues, PRs, comments, merges, and the advisory digest — now goes through
	// the hive's App installation token (ghClient / kubestellar-hive[bot]). The
	// user token only ever served as an advisory-digest fallback writer, which is
	// no longer wanted (and forced the excessive "repo" login scope, issue #1927).
	// Dashboard login now requests no scope and no user write-token is persisted.

	b.gov = governor.New(b.cfg.Governor, b.cfg.EnabledAgents(), b.logger)
	// Default mode thresholds scale with how many repos the hive watches, so
	// the mode ladder means the same thing on a 3-repo hive as on a 39-repo
	// one (#3498). Explicit thresholds are unaffected.
	b.gov.SetRepoCount(b.cfg.Project.RepoCount())
	b.sched = scheduler.New(b.cfg, b.logger)
	b.sched.SetTaskMCPURL(b.taskMCPURLForAgents())
	// A kick_template that resolves nowhere used to fail silently: the kick
	// fell through to the pack/convention template with no log line, and the
	// dashboard prompt editor showed an empty box (hivecommons/hive#7390).
	// Say so once, at boot, per agent.
	b.sched.WarnDanglingKickTemplates()

	// Wire the GitHub prompt-source resolver so agents may source their kick
	// prompt from a repo (agent.prompt_source). Fetching reuses the hive's App
	// token via ghClient and is gated to the seed-only allowlist — the closure
	// captures cfg so a live config reload updates the allowlist on the next kick.
	// A nil ghClient must be passed as a nil Fetcher interface (not a typed-nil
	// *github.Client) so the resolver's nil-fetcher fallback path triggers.
	if b.ghClient != nil {
		b.promptFetcher = b.ghClient
	}
	b.sched.SetGitHubPromptResolver(promptsrc.NewResolver(
		b.promptFetcher,
		func(slug string) bool { return b.cfg.GitHubPromptAllowed(slug) },
		b.logger,
	))

	// Wire the whole-agent definition_source resolver so agents imported with
	// "keep linked" re-fetch their portable AgentDefinition from the repo on
	// reload/kick and re-apply its operator-safe fields (never security/seed-only
	// fields — see pkg/defsrc). Same seed-only allowlist gate and graceful
	// fallback as the prompt resolver.
	if b.ghClient != nil {
		b.defFetcher = b.ghClient
	}
	b.definitionResolver = defsrc.NewResolver(
		b.defFetcher,
		func(slug string) bool { return b.cfg.GitHubDefinitionAllowed(slug) },
		b.logger,
	)
	// Apply live definitions once at startup so a repo edit made while the hive
	// was down is reflected before the first kick.
	defsrc.ApplyToConfig(context.Background(), b.cfg, b.definitionResolver, b.logger)

	// Restore sparkline history from disk so it survives container restarts
	const sparklinePath = "/data/sparkline-history.json"
	if sparkData, err := os.ReadFile(sparklinePath); err == nil {
		var snapshots []governor.EvalSnapshot
		if err := json.Unmarshal(sparkData, &snapshots); err == nil && len(snapshots) > 0 {
			b.gov.SeedEvalHistory(snapshots)
			b.logger.Info("sparkline history restored", "entries", len(snapshots))
		}
	}

	// Restore mode history from disk so the mode timeline survives container restarts
	const modeHistoryPath = "/data/mode-history.json"
	if modeData, err := os.ReadFile(modeHistoryPath); err == nil {
		var changes []governor.ModeChange
		if err := json.Unmarshal(modeData, &changes); err == nil && len(changes) > 0 {
			b.gov.SeedModeHistory(changes)
			b.logger.Info("mode history restored", "entries", len(changes))
		}
	}

	// Restore token sparkline history from disk so token charts survive container restarts
	const tokenSparklinePath = "/data/token-sparkline-history.json"
	if tokenSparkData, err := os.ReadFile(tokenSparklinePath); err == nil {
		if err := json.Unmarshal(tokenSparkData, &b.pendingTokenSeed); err == nil && len(b.pendingTokenSeed) > 0 {
			b.logger.Info("token sparkline history loaded", "entries", len(b.pendingTokenSeed))
		}
	}

	// Restore fact count history from disk so the knowledge sparkline survives restarts
	const factHistoryPath = "/data/fact-history.json"
	if factData, err := os.ReadFile(factHistoryPath); err == nil {
		if err := json.Unmarshal(factData, &b.pendingFactSeed); err == nil && len(b.pendingFactSeed) > 0 {
			b.logger.Info("fact history loaded", "entries", len(b.pendingFactSeed))
		}
	}

	// Restore estimated-cost history from disk so the cost sparkline survives restarts
	const costHistoryPath = "/data/cost-history.json"
	if costData, err := os.ReadFile(costHistoryPath); err == nil {
		if err := json.Unmarshal(costData, &b.pendingCostSeed); err == nil && len(b.pendingCostSeed) > 0 {
			b.logger.Info("cost history loaded", "entries", len(b.pendingCostSeed))
		}
	}

	// #4298: restore per-budget-window history so past resets survive a restart.
	// A missing or unparseable file is ordinary on a hive upgrading into this
	// feature — it simply starts with no history rather than failing to boot.
	const budgetWindowHistoryPath = "/data/budget-window-history.json"
	if budgetData, err := os.ReadFile(budgetWindowHistoryPath); err == nil {
		if err := json.Unmarshal(budgetData, &b.pendingBudgetWindowSeed); err == nil && len(b.pendingBudgetWindowSeed) > 0 {
			b.logger.Info("budget window history loaded", "entries", len(b.pendingBudgetWindowSeed))
		}
	}

	// #4263: restore convergence soak telemetry so a fixed-commit off/shadow/
	// enforce comparison survives restarts. Missing or unparseable is ordinary
	// on a hive that never ran with the toggle on — start empty, never fail.
	const convergenceSoakHistoryPath = "/data/convergence-soak-history.json"
	if soakData, err := os.ReadFile(convergenceSoakHistoryPath); err == nil {
		if err := json.Unmarshal(soakData, &b.pendingConvergenceSoakSeed); err == nil && len(b.pendingConvergenceSoakSeed) > 0 {
			b.logger.Info("convergence soak history loaded", "entries", len(b.pendingConvergenceSoakSeed))
		}
	}

	// Restore governor/repo/beads/system trend history from disk so those
	// sparklines survive restarts and render for any viewer (previously kept
	// only in the browser's localStorage).
	const trendHistoryPath = "/data/trend-history.json"
	if trendData, err := os.ReadFile(trendHistoryPath); err == nil {
		if err := json.Unmarshal(trendData, &b.pendingTrendSeed); err == nil && len(b.pendingTrendSeed) > 0 {
			b.logger.Info("trend history loaded", "entries", len(b.pendingTrendSeed))
		}
	}

	if b.cfg.Knowledge.Enabled {
		layers := convertKnowledgeLayers(b.cfg.Knowledge.Layers)
		primerCfg := knowledge.PrimerConfig{
			MaxFacts:      b.cfg.Knowledge.Primer.MaxFacts,
			Priority:      b.cfg.Knowledge.Primer.Priority,
			MergeStrategy: b.cfg.Knowledge.Primer.MergeStrategy,
		}
		b.primer = knowledge.NewPrimer(layers, primerCfg, b.logger)
		b.sched.SetPrimer(b.primer)
		b.logger.Info("knowledge primer enabled",
			"layers", len(b.cfg.Knowledge.Layers),
			"max_facts", primerCfg.MaxFacts,
		)
	}
}

// bootAdvisory builds the notifier, infers the ACMM level, seeds the
// GitHub App banner state, opens the mutation-convergence ledger and journal,
// finds or creates the pinned advisory issue and writes the embedded
// brainstorm policy to the policy dir. It returns false when main() should
// return without booting further — the v5 mutation ledger/journal failure
// paths, which returned from main() before the split and still do.
func (b *boot) bootAdvisory() bool { return b.bootAdvisoryWith(defaultBootAdvisoryDeps()) }

// bootAdvisoryWith is bootAdvisory with its GitHub calls injected; see
// bootAdvisoryDeps.
func (b *boot) bootAdvisoryWith(deps bootAdvisoryDeps) bool {
	var err error

	b.notifier = notify.New(b.cfg.Notifications, b.logger)
	b.notifier.SetHiveID(b.cfg.HiveID)
	b.acmmLevel = inferACMMLevel(b.cfg)
	// A hive that booted without usable GitHub credentials raises the banner
	// immediately, seeded with the classification made at startup. Otherwise
	// these stay empty and are filled in later by the live probes below.
	b.githubAppRequired = b.appAuthFailure != ""
	// githubAppDiag/githubAppState carry the classified reason App auth failed,
	// so the banner can name the true cause (and the hub can avoid escalating
	// against a hive whose credentials the operator never delivered).
	b.githubAppDiag = b.appAuthFailure
	b.githubAppState = b.appAuthState
	// Config truth outranks live probes: an App with no installation cannot
	// mint, period. A cached token can keep clients green for up to an hour
	// after an installation is cleared, and waiting for the first failed mint
	// left the banner down and the hub green exactly when the operator needed
	// the opposite (the fast-model-actuation incident).
	if b.cfg.GitHub.ConfiguredButUninstalled() {
		b.githubAppRequired = true
		b.githubAppState = github.AppStateNotInstalled
		b.githubAppDiag = "GitHub App " + strconv.FormatInt(b.cfg.GitHub.AppID, 10) +
			" has no installation for this org — install it (the spoke adopts the installation automatically)"
	}

	// Invocation-attribution trail (pkg/github/attribution.go): stamp hive-
	// created PRs/issues with what the hive invoked, and audit every such
	// creation. Wired in stages as dependencies come up: the trailer gate now
	// (cfg exists, and the advisory-issue ensure just below must respect the
	// toggle), the per-agent resolver after the agent manager exists, and the
	// audit sink after the dashboard server exists. cfg is the live pointer
	// (the config watcher swaps contents in place), so the toggle is read
	// fresh per creation — a dashboard flip takes effect immediately.
	if b.ghClient != nil {
		b.ghClient.SetAttributionHooks(github.AttributionHooks{
			TrailerEnabled: func() bool { return b.cfg.Governor.AttributionTrailerEnabled() },
		})
	}

	if b.cfg == nil {
		return false
	}
	mode := b.cfg.ConvergenceMode()
	stats := &effects.Recorder{}
	stats.SetMode(mode)
	b.mutationStats = stats
	ledger, err := mutation.OpenLedger(filepath.Join(mutationStateDir, "claims.json"), mutation.DefaultMaxWritersPerRepo)
	if err != nil {
		b.logger.Error("mutation convergence ledger unavailable; external mutation fencing disabled", "mode", mode, "error", err)
		return false
	}
	journal, err := mutation.OpenJournal(filepath.Join(mutationStateDir, "journal.json"))
	if err != nil {
		b.logger.Error("mutation convergence journal unavailable; external mutation fencing disabled", "mode", mode, "error", err)
		return false
	}
	boundary := &mutation.Boundary{
		Executor: mutation.Executor{Ledger: ledger, Journal: journal, Mode: mode},
		Holder:   "hive:" + b.cfg.HiveID,
		Logger:   b.logger,
		Stats:    stats,
		Mode:     b.cfg.ConvergenceMode,
	}
	b.mutationBoundary = boundary
	if b.ghClient != nil {
		b.ghClient.SetMutationBoundary(boundary)
	}
	b.logger.Info("mutation convergence boundary wired", "mode", mode, "state_dir", mutationStateDir)

	// Find or create the pinned advisory issue. Any level can have advisory
	// agents whose findings should be posted to this issue.
	b.advisoryIssues = map[string]int{}
	if b.acmmLevel > 0 && b.ghClient != nil {
		primaryRepo := b.cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(b.cfg.Project.Repos) > 0 {
			primaryRepo = b.cfg.Project.Repos[0]
		}
		if primaryRepo != "" {
			num, err := deps.ensureAdvisoryIssue(b.ctx, b.ghClient, primaryRepo)
			if err != nil {
				b.logger.Error("failed to ensure advisory issue", "repo", primaryRepo, "error", err)
				// GitHub returns 403 for rate limiting too — a transient
				// condition that must not raise the "App Not Installed"
				// banner (matches the guard on the repo-change path).
				if isGitHubRateLimitText(err) {
					b.logger.Warn("GitHub API rate limit hit during advisory issue ensure", "repo", primaryRepo)
				} else {
					// Do NOT decide from the error string. This call site used
					// to raise the banner on a substring match for "403"/"401"
					// and set githubAppRequired=true BEFORE classifying, then
					// never lower it again when classification came back OK or
					// inconclusive — which is why the banner showed on boot and
					// vanished on the first Re-check with nothing fixed.
					// classifyGitHubAppFailure is the same verdict Re-check
					// uses, and it declines to raise on AppStateUnknown.
					raise, diag, state := deps.classifyAppFailure(b.ctx, b.ghClient.AppAuth(), b.cfg.Project.Org, b.logger)
					if raise {
						b.githubAppRequired = true
						b.githubAppDiag, b.githubAppState = diag, state
						b.logger.Warn("GitHub App authentication failed at startup",
							"state", state.String(),
							"operator_actionable", state.OperatorActionable(),
							"error", err)
					} else {
						b.logger.Warn("advisory issue ensure failed but GitHub App auth verified healthy — not raising the App banner",
							"repo", primaryRepo, "state", state.String(), "error", err)
					}
				}
			} else {
				b.advisoryIssues[primaryRepo] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
				b.logger.Info("advisory issue ready", "repo", primaryRepo, "number", num)
			}
		}
	}

	b.advisoryStore = advisory.NewStore()

	b.policyDirPath = policyDir(b.cfg.Policies)

	// Write brainstorm policy to disk so the agent can find it.
	// The policy is embedded in the binary but the agent searches the filesystem.
	brainstormPolicyDir := b.policyDirPath
	if err := os.MkdirAll(brainstormPolicyDir, 0o755); err != nil {
		b.logger.Warn("failed to create brainstorm policy dir", "path", brainstormPolicyDir, "error", err)
	}
	if policyData, err := policies.DefaultPolicies.ReadFile("defaults/brainstorm-advisory.md"); err == nil {
		policyPath := filepath.Join(brainstormPolicyDir, "brainstorm-advisory.md")
		// Always overwrite — the embedded policy may have been updated
		// (e.g., inception reaping guard added in bug #113 fix).
		if err := os.WriteFile(policyPath, policyData, 0o644); err != nil {
			b.logger.Warn("failed to write brainstorm policy", "path", policyPath, "error", err)
		} else {
			b.logger.Info("wrote brainstorm policy to disk", "path", policyPath)
		}
	}

	b.projectCtx = agent.ProjectContext{
		Org: b.cfg.Project.Org,
		// Repos stays the full watched set; the functions below narrow it at
		// read time — RepoPaused to the repos open for work (#6203), AgentRepos
		// and AgentPrimaryRepo to the ones a given agent serves (#6204) — so a
		// pause taken or a scope edited mid-run reaches the next agent launch
		// without rebuilding this context.
		Repos:            b.cfg.Project.Repos,
		RepoPaused:       b.cfg.IsRepoPaused,
		AgentRepos:       b.cfg.ReposForAgent,
		AgentPrimaryRepo: b.cfg.PrimaryRepoForAgent,
		PrimaryRepoName:  b.cfg.Project.PrimaryRepo,
		ACMMLevel:        b.acmmLevel,
		PRsAllowed:       b.cfg.Project.PRsAllowed(),
		PolicyDir:        b.policyDirPath,
		AppAuthoredPRs:   b.cfg.GitHub.AppAuthoredPRsEnabled(),
		TaskMCPURL:       b.taskMCPURLForAgents(),
	}
	return true
}

func (b *boot) taskMCPURLForAgents() string {
	if b == nil || b.cfg == nil || b.dashboardURLForFreshHeartbeat == nil {
		return ""
	}
	base := strings.TrimRight(b.dashboardURLForFreshHeartbeat(), "/") + taskmcp.EndpointPath
	token := ""
	token = strings.TrimSpace(b.cfg.Dashboard.AuthToken)
	if token == "" {
		return base
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String()
}

// bootAgents constructs the agent manager and everything that hangs off it
// before any agent launches: shutdown archive hook, resolvers, token and
// credential loops, the agent-facing GitHub request relays, and mint.
func (b *boot) bootAgents() { b.bootAgentsWith(defaultBootAgentsDeps()) }

// bootAgentsWith is bootAgents with its long-lived effects injected; see
// bootAgentsDeps. The request relays are armed AFTER every setter on the
// client (identity, hold/signed-commit policy, re-engage hook, merger
// authorizer, required checks, merge policy) so no watcher goroutine can
// observe a half-configured client.
func (b *boot) bootAgentsWith(deps bootAgentsDeps) {
	var err error
	if b.cfg.GitHub.IsGHE() {
		b.projectCtx.GHHost = b.cfg.GitHub.HostLabel()
	}
	b.agentMgr = agent.NewManager(b.cfg.EnabledAgents(), b.logger, b.projectCtx)
	// SIGTERM (pod roll, hive upgrade) destroys every tmux server and with it
	// the in-flight kick's scrollback; archive it to /data first (#4296).
	archiveOnShutdown := func() { b.agentMgr.ArchiveAllKickLogs("shutdown") }
	b.preShutdownHooks.add("archive-kick-logs", archiveOnShutdown)
	b.agentMgr.SetSandboxConfig(b.cfg.AgentSandbox)
	// #7421: the governor records a kick when it is dispatched; the manager
	// tells it afterwards how the turn ENDED, so a kick that produced a
	// clarifying question or a policy stand-down is not counted like one that
	// produced work (and a question earns an early re-kick).
	b.agentMgr.SetKickOutcomeObserver(func(agentName string, outcome agent.KickOutcome) {
		b.gov.RecordKickOutcome(agentName, outcome.Kind, outcome.Reason, outcome.KickAt, outcome.At)
	})

	// Say out loud when the sandbox opt-in is configured but inert. The gate is
	// two-part (global agent_sandbox.enabled AND a per-agent sandbox.enabled),
	// and the dashboard's Security tab writes only the global half — so an
	// owner can turn the sandbox on, be told the setting was updated, and still
	// have every agent running unconfined on the operator's own host.
	//
	// That silence is the part of #4918 that is safe to fix here. The gate
	// itself is load-bearing: a sandboxed agent runs a different execution
	// model and startSandboxKickLocked has no tmux fallback, so collapsing it
	// would convert working agents into permanently failing ones. Telling an
	// operator who believes they are covered that they are not costs nothing.
	logAgentSandboxPosture(b.logger, b.cfg)
	// Treat any configured gateway name as an inference-routable backend so an
	// agent with backend: <gateway> routes through it. Resolution is live
	// (reads cfg on each call) so gateways added from the Model Gateways tab
	// take effect without a restart.
	//
	// Wired HERE — immediately after the manager is constructed — and not in
	// the proxy/dashboard wiring further down, because SetBackendOverride
	// validates backend names against this predicate. The persisted-state
	// replay (restoreAgentRuntimeState, below) re-applies saved backend
	// overrides long before the dashboard wiring runs, and with the predicate
	// still unset every gateway-named override was rejected there — silently,
	// while the model override beside it restored fine. That is the #3961
	// asymmetric revert: an agent switched to a gateway backend came back on
	// its config backend but with the switched model still applied, producing
	// launch-dead hybrids like `pi --model gpt-5.6-luna`.
	b.agentMgr.SetGatewayBackendChecker(func(backend string) bool {
		return b.cfg.Governor.ResolveGateway(backend) != nil &&
			!strings.EqualFold(backend, "") // empty is the default, not a named backend
	})
	// Resolve the bob API key at LAUNCH time, not here: cfg is the live config
	// pointer (the config watcher swaps its contents in place on reload), so a
	// key added via the Secret mount, the PVC file, or a config edit takes
	// effect on the next agent launch with no hive restart. Only the key's
	// LOCATION is ever in cfg; the value is read from file/env on each call and
	// is never logged.
	b.agentMgr.SetBobAPIKeyResolver(func() string {
		return b.cfg.Governor.Bob.ResolveAPIKey()
	})
	// Hive-wide default explain mode, resolved per kick/launch off the live cfg
	// pointer for the same reason as the bob key above: an operator debugging a
	// misbehaving fleet turns explanation on from Settings → Governor and needs
	// it on the NEXT kick, not after a restart. Governor config wins over
	// HIVE_EXPLAIN_MODE; the env var stays as the fallback (#4712).
	b.agentMgr.SetExplainModeDefaultResolver(func() string {
		return b.cfg.Governor.ResolveExplainModeDefault()
	})
	// The launch path also needs to know WHICH FILE the key came from, so it can
	// check that file is readable by the agent UID rather than only by the hive
	// process. Returns a loggable source string, never the key value.
	b.agentMgr.SetBobKeySourceResolver(func() string {
		return b.cfg.Governor.Bob.ResolveAPIKeySource()
	})
	// Log only WHERE the key came from (or that none is set) so a misconfigured
	// hive is diagnosable without the value ever reaching the logs.
	if src := b.cfg.Governor.Bob.ResolveAPIKeySource(); src != "" {
		b.logger.Info("bob api key detected", "source", src)
	} else {
		b.logger.Info("no bob api key configured; agents with backend \"bob\" will not launch",
			"remedy", "set governor.bob.api_key_file or the "+config.DefaultBobAPIKeyEnv+" env var")
	}
	if b.appAuth != nil {
		b.agentMgr.SetAppAuth(b.appAuth)
		b.agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: b.appAuth})
	}
	// Start the per-agent token refresh loop UNCONDITIONALLY. It no-ops until
	// App auth is wired, and on hosted spokes that wiring happens AFTER boot
	// (heartbeat delivery / config API reinit / config reload). Gating this on
	// appAuth != nil at boot meant those hives never refreshed per-agent token
	// caches: agent sessions outlived their scoped token, gh 401'd and printed
	// "gh auth login", and the login-detector auto-paused the agent (#4072).
	deps.startAgentLoops(b.ctx, b.agentMgr)
	// Start the credential watchdog UNCONDITIONALLY. It self-gates per backend
	// on the presence of an agent using that backend each tick, so it is a
	// no-op on gateway/inference-only hives. On Copilot/Claude hives it turns a
	// missing or expired durable credential — the "stuck at login after an
	// upgrade roll" outage — into an immediate Audit Log signal instead of a
	// silent multi-hour stall.
	// Keep the Copilot CLI's config.json copilotTokens populated from the
	// durable user token, so agents never sit stuck at "Please use /login"
	// while a valid token exists (CLI 1.0.78 does not re-populate the emptied
	// store from the injected env token on its own). Self-gates on a copilot
	// backend and only writes when the store is empty; never runs a login.
	if b.ghClient != nil {
		b.agentMgr.SetSandboxPRClient(b.ghClient)
		b.agentMgr.SetSandboxMutationBoundary(b.mutationBoundary)
	}

	// PR-open-as-the-App-bot: agents push their branch (App-token credential
	// helper) then drop a request file; the hive opens the PR here with the App
	// token so it is authored by "<slug>[bot]", not the Copilot login user.
	// Gated on a real client + usable App — with no App there is no bot to author
	// as, and requests simply accumulate rather than opening under a wrong
	// identity. ghClient uses the App installation token (see ghAuth wiring).

	// Approval desk (RFC #4000): the single tool-approval decision point plus
	// its durable operator-lane inbox. Both are nil unless
	// `tool_approval.enabled` is set — the default — so this costs nothing and
	// changes nothing on a hive that has not opted in. Built here, before the
	// auto-merge sweep is started below, because the sweep is the one producer
	// wired in this slice. Also handed to the dashboard for the Approvals panel.
	b.approvalDesk, b.approvalInbox = buildApprovalDesk(b.cfg, b.logger)
	// Create the agent-facing request queues REGARDLESS of App state. The
	// watchers below stay gated (no App, no bot to author as), but the queues
	// must exist either way or the "requests simply accumulate" behavior above
	// is a fiction: hive-open-pr / hive-open-issue run in the AGENT's shell and
	// hard-fail on a missing directory, discarding the finding instead of
	// queueing it. App setup routinely completes after boot (operator saves the
	// installation ID, /gh-setup persists it, auto-discovery finds it later), so
	// this gap silently disarms agent writes on a hive that looks healthy.
	deps.prepareRequestDirs(b.logger)
	// Token-access audit ingest (#6287): the per-UID wrappers record every gh
	// call and credential lookup as an event file, and this loop folds them
	// into the hive-owned 0600 audit log that GET /api/token-access serves.
	// Unconditional, like the request dirs: agents touch tokens whether or
	// not the App is usable, and the trail must never depend on App state.
	deps.startTokenAccessAudit(b.ctx, b.logger)

	// Local alias keeps the gate textually identical to v4, which
	// pkg/github/request_dirs_test.go pins by regexp.
	cfg := b.cfg
	if b.ghClient != nil && cfg.GitHub.HasUsableApp() {
		// Attribution resolver: effective backend/model from the manager
		// (runtime overrides included), falling back to the configured values
		// for an agent the manager does not know; tool version resolved
		// lazily per backend and cached. Only launch descriptors flow here —
		// never tokens, keys, or prompt content.
		b.ghClient.SetAttributionResolver(func(agentName string) github.InvocationMeta {
			backend, model, effort, known := b.agentMgr.InvocationMetadata(agentName)
			if !known {
				if ac, inCfg := b.cfg.Agents[agentName]; inCfg {
					backend, model = ac.Backend, ac.Model
					// Same resolver the Manager uses, not a second copy of the
					// rule: a hardcoded default here would drift silently the
					// moment agy's default effort changed.
					effort = agent.ResolveReasoningEffort(backend, model, ac.ReasoningEffort)
				}
			}
			tool, toolVersion := github.ResolveToolVersion(backend)
			return github.InvocationMeta{
				Agent:   agentName,
				Backend: backend,
				// bob self-selects (no catalog): requested model is honestly
				// "auto" — see github.RequestedModel for the known follow-up
				// on discovering bob's internal routing.
				Model:       github.RequestedModel(backend, model),
				Effort:      effort,
				Tool:        tool,
				ToolVersion: toolVersion,
			}
		})
		// authz enforces the SAME per-agent ACMM write-gate + forge-resistance as
		// the direct `gh pr create` path — the request-file route grants no extra
		// privilege. A denied request is quarantined, never opened.
		// holdLabel (F6): at hold-gated ACMM levels (L3/L4/L5) every agent-opened
		// PR must carry the "hold" label so the merge gate holds it for human
		// approval. Outreach content is public speech on the project's behalf, so
		// it remains human-reviewed at L6 too. This is decided server-side from the
		// authenticated agent identity and authoritative hive level
		// (GetACMMLevel), NOT from a client flag — the gh-wrapper.sh tail that used
		// to add the label was dead code after `exec hive-open-pr`. L1/L2 open no
		// agent PRs (manual); non-outreach L6 PRs retain their existing automerge
		// behavior.
		holdLabel := func(agentName string) bool {
			return shouldHoldAgentPR(agentName, b.agentMgr.GetACMMLevel())
		}
		// #5117: tell the client which accounts are ours, so the
		// self-authorization gate recognises an issue filed under
		// project.ai_author's plain user account as hive-filed rather than
		// mistaking it for a human's. The App bot is recognised without this;
		// hiveIdentity() is the same resolver the duplicate-PR guard uses.
		b.ghClient.SetHiveIdentity(hiveIdentity(b.cfg))
		b.ghClient.SetSelfAuthorizationHoldEnabled(func(repo string) bool { return b.cfg.SelfAuthorizationHoldEnabledForRepo(repo) })
		installReviewRelaySettings(b.ghClient, b.cfg, b.logger)
		// github.app_signed_commits: re-author each agent branch through
		// createCommitOnBranch before the PR opens, so its commit is
		// GitHub-signed and authored by the App bot. Read through a func so a
		// config reload takes effect on the next request.
		b.ghClient.SetSignedCommits(func() bool { return b.cfg.GitHub.AppSignedCommitsEnabled() })
		// Fix #2: on a terminal merge failure caused by a failing REQUIRED check,
		// re-engage the fix loop instead of abandoning the PR. The hook records a
		// re-engagement under the escalation store's per-red-SHA cap (shared with
		// the reaper so a PR is never double-dispatched beyond its budget) and
		// returns whether the cap still allowed a dispatch. The PR is already
		// surfaced into CI_FAILING by writeMergeEligible each eval tick; the hook
		// is the loop-safety authority that decides when to STOP nudging.
		b.ghClient.SetMergeReEngageHook(mergeReEngageHook(b.cfg))

		// SECURITY (audit F3): re-verify the merger tier inside the sweep. The
		// dashboard's queue endpoint gates on requireMergerOrOwnerRole, but the
		// sweep merges a minute later off the label + App-authored approval body
		// alone, so without this ANY actor who can get the label applied merges
		// anything, and a sockpuppet pair defeats the self-merge ban. Resolved
		// against the SAME allowlist the dashboard uses so there is one notion of
		// trust; read through cfg on every call so a config reload takes effect.
		autoMergeOpts := automerge.Options{
			Logger:           b.logger,
			MergerAuthorizer: trustedMergerFunc(b.cfg),
		}

		// commitGreen's required-checks gate (self-merge sweep, see
		// automerge_sweep.go): install the operator-declared
		// auto_merge.required_checks list, if any, so naming required checks
		// does not depend on GitHub's required-status-checks branch-protection
		// API. Older Hive App installations often lack administration:read, so
		// that API call fails closed to the coarser isMetaCheck/isIgnorableCICheck
		// allowlist. Unset/empty leaves the API/allowlist fallback chain
		// intact (SetRequiredChecks(nil) is a safe no-op).
		if set, ok := syncAutoMergePolicyToGitHubClient(b.cfg, b.ghClient); ok {
			autoMergeOpts.RequiredChecks = set
		}

		// Issue relay: agents request issue creation and comments by dropping a
		// file (hive-open-issue via the gh wrapper) instead of calling GitHub
		// from their own shell. The agent-side call used to ride the agent's
		// shell tool — one GHE secondary-rate-limit stall or mangled multiline
		// command and the finding was silently lost (root-caused live
		// 2026-08-21: sec-check's creates timed out and survived only as
		// beads). The watcher executes server-side with the App token, retries
		// with backoff, dedupes by exact open-issue title, and enforces the
		// same forge-resistance + CanCreateIssues mode gate the wrapper does.
		// Review relay: agents request PR reviews by dropping a file (hive-review)
		// instead of running `gh pr review` in their own shell, which the hive
		// never observes. The watcher submits the review with the App token and
		// records it on the audit/activity trail, gated by the same
		// forge-resistance + push-capability (CanPush) check as opening a PR —
		// reviewing is a PR-write, so AuthorizePROpen is the correct gate.
		// Merge relay: agents request merges by dropping a file (hive-merge)
		// instead of calling the GitHub MCP merge_pull_request tool, whose GraphQL
		// mutation GitHub rejects for App tokens ("Resource not accessible by
		// integration"). The hive merges over REST with the App token, gated by
		// the same forge-resistance + a CanMerge ACMM check.
		// bindMergeAuthz layers the F4 target-binding (CWE-863) on top of the
		// manager's agent/UID/CanMerge check: the merge must name a pinned head
		// SHA (no unpinned "merge whatever HEAD is now") AND the (repo, number)
		// must appear in the governor's current merge-eligible list — so an
		// injected agent cannot land an arbitrary reachable PR of its choosing.
		// Fix #2: on a terminal merge failure caused by a failing REQUIRED check,
		// re-engage the fix loop instead of abandoning the PR. The hook records a
		// re-engagement under the escalation store's per-red-SHA cap (shared with
		// the reaper so a PR is never double-dispatched beyond its budget) and
		// returns whether the cap still allowed a dispatch. The PR is already
		// surfaced into CI_FAILING by writeMergeEligible each eval tick; the hook
		// is the loop-safety authority that decides when to STOP nudging.
		// Self-authored auto-merge: the App merges its OWN open, CI-green PRs
		// directly over the REST API, without a human "Approved ... for Hive
		// auto-merge" queue review and without waiting on tide. Prow forbids
		// self-approval (lgtm+approved must come from someone other than the
		// author), and the author here is always the App itself, so the
		// human-queue path (StartMergeRequestWatcher above / the governor
		// sweep) can never clear for the App's own PRs — this is the only
		// route that lands them. See AutoMergeConfig and
		// SweepSelfAuthoredAutoMerges for the full rationale and the safety
		// properties preserved (green required checks, head-SHA re-verified
		// immediately before merge, squash method, all tiers included).
		// Default ON; `auto_merge.self_authored: false` disables it. ALSO
		// gated on ACMM level (config.SelfMergeMinACMMLevel): l4.md/l5.md
		// both forbid the App merging its own PRs, so an L4/L5 hive must
		// never start this loop regardless of the flag above — see
		// AutoMergeConfig.SelfAuthoredAutoMergeAllowed. StartSelfAuthoredAutoMergeSweep
		// itself no-ops (with a one-time INFO log) when acmmAllowed is false.
		// Approval desk (RFC #4000). Installed BEFORE the sweep starts so the
		// first tick already consults it. A nil desk (the default —
		// `tool_approval.enabled` is false) installs no hook, leaving the
		// sweep's behavior byte-identical to the pre-desk build.
		if b.approvalDesk != nil && b.approvalInbox != nil {
			autoMergeOpts.ApprovalDesk = newSelfMergeDeskHook(b.approvalDesk, b.approvalInbox, b.cfg, b.logger)
		}

		autoMergeOpts.MutationBoundary = b.mutationBoundary
		autoMergeOpts.SelfAuthorizationHoldEnabled = func(repo string) bool { return b.cfg.SelfAuthorizationHoldEnabledForRepo(repo) }
		// Intent tier gate (#6258): the human lane only queues PRs that
		// survive writeMergeEligible's intent check, but this sweep lists
		// the App's PRs on its own, so it carries the same policy (same
		// config, same bead evidence, same BlocksMerge predicate) and asks
		// intent.EvaluateForAppSelfMerge before every self-merge.
		autoMergeOpts.IntentGate = selfMergeIntentGate(b.cfg, b.beadStores)
		deps.startRequestRelays(b.ctx, b.ghClient, requestRelays{
			prOpen:    b.agentMgr.AuthorizePROpen,
			holdLabel: holdLabel,
			issueOpen: b.agentMgr.AuthorizeIssueOpen,
			review:    b.agentMgr.AuthorizeReviewRequest,
			merge:     bindMergeAuthz(b.agentMgr.AuthorizeMerge),
			logger:    b.logger,
		})
		deps.startSelfAuthoredSweep(b.ctx, b.ghClient, b.cfg.AutoMerge.MaxMerges, b.cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(b.cfg.ACMMLevel), b.cfg.ACMMLevel, autoMergeOpts)
	}

	// Opt-in mint credential: when mint.enabled, build a Minter from the config
	// (signing key + issuer + TTL) and attach it so each per-agent token refresh
	// ALSO issues a scoped short-lived OIDC token alongside the GitHub App token.
	// Default off — an absent/disabled `mint:` block leaves the credential path
	// byte-identical. Fail-safe: a mint setup error is logged, never fatal.
	if b.cfg.Mint.Enabled {
		if b.agentMinter, err = deps.buildMinter(b.cfg, b.logger); err != nil {
			b.logger.Warn("mint enabled but minter setup failed; agents keep App token only", "error", err)
		} else {
			b.agentMgr.SetAgentMint(b.agentMinter)
			b.logger.Info("mint credential enabled", "issuer", b.cfg.Mint.Issuer, "hive_id", b.cfg.HiveID)
		}
	}

	deps.startPermissionsWatcher(b.logger)
}

// bootState loads the persisted state snapshot and replays it into the
// agent manager, governor and config, migrating legacy config overrides.
func (b *boot) bootState() { b.bootStateWith(defaultBootStateDeps()) }

// bootStateWith is bootState with its disk reads/writes injected; see
// bootStateDeps.
func (b *boot) bootStateWith(deps bootStateDeps) {
	var stateErr error
	b.saved, stateErr = deps.loadState(b.logger)
	if stateErr != nil {
		b.logger.Warn("failed to load persisted state", "error", stateErr)
	} else if b.saved != nil {
		restoreAgentRuntimeState(b.saved, b.cfg, b.agentMgr, b.logger)
		// Re-establish the fleet breaker AFTER per-agent pauses are restored
		// above: the agents it held are already back in the paused state (with
		// PausedTrigger == fleet-breaker from their persisted pause), so this
		// only re-attaches the breaker so a later release resumes exactly them.
		// An engaged breaker must REMAIN engaged across restart — it does not
		// auto-release, and it does not re-pause or resume anything here.
		if b.saved.Breaker != nil && b.saved.Breaker.Engaged {
			b.agentMgr.RestoreBreaker(true, b.saved.Breaker.Paused)
			b.logger.Info("fleet breaker restored from state", "held", len(b.saved.Breaker.Paused))
		}
		if b.saved.BudgetLimit > 0 {
			b.gov.SetBudgetLimit(b.saved.BudgetLimit)
		}
		if b.saved.BudgetIgnoreAll {
			b.gov.SetBudgetIgnoreAll(true)
		}
		if len(b.saved.BudgetIgnored) > 0 {
			b.gov.SetBudgetIgnored(b.saved.BudgetIgnored)
		}
		if len(b.saved.CadenceOverrides) > 0 {
			for modeName, agentCadences := range b.saved.CadenceOverrides {
				mode, ok := b.cfg.Governor.Modes[modeName]
				if !ok {
					continue
				}
				if mode.Cadences == nil {
					mode.Cadences = make(map[string]config.Cadence)
				}
				for agentName, cadence := range agentCadences {
					mode.Cadences[agentName] = cadence
				}
				b.cfg.Governor.Modes[modeName] = mode
			}
			b.logger.Info("cadence overrides restored", "modes", len(b.saved.CadenceOverrides))
		}
		if b.saved.GovernorMode != "" {
			b.gov.SetMode(governor.Mode(b.saved.GovernorMode))
			b.logger.Info("governor mode restored", "mode", b.saved.GovernorMode)
		}
		if len(b.saved.LastKicks) > 0 {
			b.gov.SeedLastKicks(b.saved.LastKicks)
			b.logger.Info("governor last kicks restored", "agents", len(b.saved.LastKicks))
		}
		if b.saved.BudgetSpend > 0 || !b.saved.BudgetResetAt.IsZero() || len(b.saved.BudgetByAgent) > 0 {
			b.gov.SeedBudget(b.saved.BudgetSpend, b.saved.BudgetByAgent, b.saved.BudgetByModel, b.saved.BudgetResetAt)
			b.gov.SeedBudgetWindowBaseline(b.saved.BudgetWindowBaseline)
			b.logger.Info("budget state restored", "spend", b.saved.BudgetSpend, "reset_at", b.saved.BudgetResetAt, "window_baseline", b.saved.BudgetWindowBaseline)
		}
		if len(b.saved.KickHistory) > 0 {
			records := make([]governor.KickRecord, len(b.saved.KickHistory))
			for i, ke := range b.saved.KickHistory {
				records[i] = governor.KickRecord{Timestamp: ke.Timestamp, Agent: ke.Agent, Outcome: ke.Outcome, OutcomeReason: ke.OutcomeReason}
			}
			b.gov.SeedKickHistory(records)
			b.logger.Info("kick history restored", "entries", len(records))
		}
		if !b.saved.LastEval.IsZero() {
			b.gov.SeedLastEval(b.saved.LastEval)
		}
		if b.saved.ACMMLevel != nil && b.cfg.ACMMLevel == nil {
			b.cfg.ACMMLevel = b.saved.ACMMLevel
			b.logger.Info("ACMM level restored", "level", *b.saved.ACMMLevel)
		}
		if b.saved.ConfigOverrides != nil {
			applyConfigOverrides(b.cfg, b.saved.ConfigOverrides)
			b.ghClient.SetRepos(b.cfg.Project.Repos)
			if len(b.cfg.Governor.Labels.Exempt) > 0 {
				b.ghClient.SetExemptLabels(b.cfg.Governor.Labels.Exempt)
				b.ghClient.SetAutoMergeLabel(normalizedAutoMergeLabel(b.cfg.Governor.Labels.AutoMerge))
			}
			b.ghClient.SetIssueFilter(b.cfg.Project.IssueFilter)
			syncAutoMergePolicyToGitHubClient(b.cfg, b.ghClient)
			b.logger.Info("migrated config overrides from state to hive.yaml",
				"repos", b.cfg.Project.Repos)

			// Write merged config to hive.yaml so overrides become the base config
			if err := deps.saveConfig(b.cfg); err != nil {
				b.logger.Error("failed to save migrated config", "error", err)
			}

			// Strip config_overrides from state and re-save
			b.saved.ConfigOverrides = nil
			if err := deps.saveState(b.saved, b.logger); err != nil {
				b.logger.Error("failed to re-save state after migration", "error", err)
			}
		}
	}

	if b.gov.GetBudget().WeeklyLimit == 0 && b.cfg.Governor.Budget.TotalTokens > 0 {
		b.gov.SetBudgetLimit(b.cfg.Governor.Budget.TotalTokens)
	}
}

// bootDashboard constructs the dashboard server, points the scheduler and
// agent manager's audit sinks at it, enables PVC persistence and seeds the
// history bootGovernor loaded. The advisory sinks it installs read
// b.beadStores, which bootStores fills next — before that they see a nil
// map, exactly as the captured local did.
func (b *boot) bootDashboard() { b.bootDashboardWith(defaultBootDashboardDeps()) }

// bootDashboardWith is bootDashboard with the server constructor and PVC
// persistence enables injected; see bootDashboardDeps.
func (b *boot) bootDashboardWith(deps bootDashboardDeps) {
	b.dashSrv = deps.newServer(b.cfg.Dashboard.Port, b.cfg.Dashboard.AuthToken, b.logger)
	b.dashSrv.SetMutationStats(func() interface{} {
		if b.mutationStats == nil {
			return nil
		}
		return b.mutationStats.Snapshot()
	})
	// SIGTERM (pod roll, hive self-upgrade) kills the process and every
	// contributor WebSocket with it, and until #5390 it did so without a word:
	// the peer saw a bare 1006, indistinguishable from a network fault, which is
	// what made #5090 take days to diagnose. Send each contributor a 1012
	// (CloseServiceRestart) first so the relay knows to reconnect immediately —
	// into the replacement pod, which maxSurge=1/maxUnavailable=0 has already
	// brought to readiness before this signal was delivered.
	//
	// Registered as its OWN hook rather than folded into archiveOnShutdown: the
	// two are unrelated, and the drain must not be able to prevent the archive
	// from running. addUrgent, not add, because it is the time-critical half —
	// the sooner the frame is on the wire the sooner the relay reconnects,
	// whereas the kick-log archive does PVC I/O on NFS and nobody is waiting on
	// it. The hub is resolved lazily inside the closure because the contributor
	// hub is not constructed until registerContributeRoutes runs, below.
	b.preShutdownHooks.addUrgent("drain-contributor-websockets", func() {
		b.dashSrv.DrainContributorsForShutdown()
	})
	// Stop the contributor hub's background cleanup loop on the way out
	// (#6272). On v4 this is a `defer dashSrv.CloseContributeHub()` in main();
	// v5 has no such single-function scope, so it rides the same shutdown-hook
	// mechanism as the drain — deliberately NOT urgent, and registered after
	// it, so the 1012 close frames are on the wire before the loop stops.
	b.preShutdownHooks.add("close-contribute-hub", func() {
		b.dashSrv.CloseContributeHub()
	})

	// Wire ioscan input enforcement (opt-in via ioscan.enabled) to the dashboard
	// audit log so a blocked/redacted issue title surfaces in the existing
	// audit-trail UI with no new sink. The closure keeps pkg/scheduler decoupled
	// from pkg/dashboard — the scheduler only knows a func(action, detail, agent).
	b.sched.SetAuditFunc(func(action, detail, agent string) {
		b.dashSrv.AuditLog(agent, action, detail, agent)
	})
	b.sched.SetAdvisoryFunc(func(title, detail, agentName string) {
		store := b.beadStores[agentName]
		if store == nil {
			store = b.beadStores["scanner"]
		}
		if store == nil {
			store = b.beadStores["supervisor"]
		}
		if store == nil {
			for _, candidate := range b.beadStores {
				store = candidate
				break
			}
		}
		if store != nil {
			if b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, agentName, ""); err == nil {
				_ = store.SetMetadata(b.ID, "ioscan_classifier", detail)
			}
		}
	})
	if b.cfg.Ioscan.IsEnabled() && b.cfg.Ioscan.Classifier.Enabled {
		endpoint, apiKey, model := b.cfg.Governor.ResolveReviewer()
		if b.cfg.Ioscan.Classifier.Model != "" {
			model = b.cfg.Ioscan.Classifier.Model
		}
		if model == "" {
			model = ioscan.DefaultClassifierModel
		}
		classifier, cerr := ioscan.NewLLMClassifier(ioscan.LLMClassifierConfig{
			Endpoint: endpoint,
			APIKey:   apiKey,
			Model:    model,
		})
		if cerr != nil {
			b.logger.Warn("ioscan classifier enabled but not running", "reason", cerr.Error())
		} else {
			b.sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), ioscan.Thresholds{
				Warn:  b.cfg.Ioscan.Classifier.WarnThreshold,
				Block: b.cfg.Ioscan.Classifier.BlockThreshold,
			})
			b.logger.Info("ioscan semantic classifier enabled", "model", model)
		}
	}
	b.agentMgr.SetSandboxAuditCallback(func(agentName, action, detail string) {
		b.dashSrv.AuditLog(agentName, action, detail, agentName)
		if action == "sandbox_broker_rejected" {
			if store, ok := b.beadStores[agentName]; ok && store != nil {
				if b, err := store.Create("Sandbox push broker rejected changes", beads.TypeAdvisory, beads.PriorityHigh, agentName, ""); err == nil {
					_ = store.SetMetadata(b.ID, "sandbox_broker_rejection", detail)
				}
			}
		}
	})

	// Persist per-user dashboard sessions on the PVC (/data) so direct-route
	// users aren't logged out by pod restarts. NOTE: use /data explicitly, NOT
	// filepath.Dir(*configPath) — the config lives at /etc/hive/hive.yaml, which
	// is an ephemeral emptyDir (the ConfigMap seed mount), so a sessions file
	// there is wiped on every pod roll. That was the "re-login on every visit"
	// bug on direct-route spokes. /data is the CephFS PVC (same place cost/fact
	// history persist).
	deps.enableSessionPersistence(b.dashSrv, dashboardSessionsPath)

	// Lifecycle timeline journeys persist on the PVC too (#5656): the ring is
	// the panel's only memory of merged/blocked outcomes, so a pod roll must
	// not zero the fleet counters. Enabled before any producer records.
	deps.enableLifecyclePersistence(b.dashSrv, lifecycleTimelinePath)

	// The scheduler's classifier pass records KindClassified journeys the
	// moment lane routing decides an issue's lane — same store, no extra work.
	b.sched.SetLifecycleRecorder(b.dashSrv.LifecycleTimeline())

	// Attribution audit sink: every hive-mediated PR/issue creation lands in
	// the dashboard audit log (audit.jsonl + ring) UNCONDITIONALLY — the
	// trailer toggle never gates this. Creations before this point (the
	// startup advisory-issue ensure) fall back to the hive log inside
	// recordCreationAudit, so no creation goes unrecorded. The same stream
	// feeds the lifecycle timeline: agent_pr_created → pr_opened and
	// pr_merged → merged (both automerge sweep paths, MergePR from the
	// dashboard queue and the merge watcher), see recordLifecycleFromAudit.
	if b.ghClient != nil {
		b.ghClient.SetAttributionAudit(func(action, detail, agent string) {
			b.dashSrv.AuditLog("system", action, detail, agent)
			recordLifecycleFromAudit(b.dashSrv, b.cfg.Project.Org, action, detail, agent)
		})
	}

	var mentionStore *mention.Store
	if b.cfg.GitHub.Mentions.Enabled || b.cfg.GitHub.Actions.OIDC.Enabled {
		store, err := mention.NewStore("/data/github-mention-triggers.json")
		if err != nil {
			b.logger.Warn("mention trigger store unavailable", "error", err)
		} else {
			b.mentionStore = store
			mentionStore = store
		}
	}

	if b.ghClient != nil && b.cfg.GitHub.HasUsableApp() && b.cfg.GitHub.Mentions.Enabled && mentionStore != nil {
		store := mentionStore
		mentionAgents := func() []mention.AgentInfo {
			out := make([]mention.AgentInfo, 0, len(b.cfg.Agents))
			for name, ac := range b.cfg.Agents {
				out = append(out, mention.AgentInfo{
					Name:         name,
					Enabled:      ac.Enabled,
					Converse:     ac.Converse != nil && *ac.Converse,
					Mention:      ac.HasEnabledChannel(config.ChannelTypeMention),
					GovernorKick: ac.UsesGovernorKick(),
				})
			}
			return out
		}
		handler := mention.NewHandler(mention.Options{
			Config:     b.cfg.GitHub.Mentions,
			Actions:    b.cfg.GitHub.Actions,
			ReviewBots: b.cfg.Classification.ReviewBots,
			Roles: func(login string) (string, bool) {
				return b.cfg.Dashboard.AuthorizedRole(login)
			},
			Repos: func() []string {
				if b.ghClient == nil {
					return nil
				}
				return b.ghClient.ActiveRepositories()
			},
			Agents:     mentionAgents,
			GitHubFunc: func() mention.GitHub { return b.ghClient },
			Store:      store,
			Kick: func(agentName, message, source string) error {
				return b.agentMgr.SendKickWithSource(agentName, message, source)
			},
			Audit: func(action, detail, agentName string) {
				b.dashSrv.AuditLog("system", action, detail, agentName)
				recordLifecycleFromAudit(b.dashSrv, b.cfg.Project.Org, action, detail, agentName)
			},
		})
		poller := mention.NewPoller(nil, func() []string {
			if b.ghClient == nil {
				return nil
			}
			return b.ghClient.ActiveRepositories()
		}, store, handler, b.cfg.GitHub.Mentions.PollIntervalEffective(), b.logger)
		poller.SetGitHubGetter(func() mention.GitHub { return b.ghClient })
		if b.cfg.GitHub.Mentions.WebhookEnabled {
			receiver := mention.NewWebhookReceiver(func() string {
				return b.cfg.GitHub.Mentions.WebhookSecretEffective()
			}, poller, b.cfg.GitHub.Mentions.WebhookMinGapEffective(), b.logger)
			receiver.SetReposFunc(func() []string {
				if b.ghClient == nil {
					return nil
				}
				return b.ghClient.ActiveRepositories()
			})
			b.mentionWebhook = receiver
		}
		go poller.Run(b.ctx)
		responder := mention.NewResponder(store, func() mention.GitHub { return b.ghClient }, mentionAgents, b.cfg.Classification.ReviewBots, b.logger)
		b.agentMgr.SetKickObserver(responder.HandleAgentEvent)
		b.logger.Info("GitHub mention trigger poller started", "interval", b.cfg.GitHub.Mentions.PollIntervalEffective())
	}

	// Seed token sparkline history now that the dashboard server exists
	if len(b.pendingTokenSeed) > 0 {
		b.dashSrv.SeedTokenSparklineHistory(b.pendingTokenSeed)
		b.logger.Info("token sparkline history restored", "entries", len(b.pendingTokenSeed))
	}

	if len(b.pendingFactSeed) > 0 {
		b.dashSrv.SeedFactHistory(b.pendingFactSeed)
		b.logger.Info("fact history restored", "entries", len(b.pendingFactSeed))
	}

	if len(b.pendingCostSeed) > 0 {
		b.dashSrv.SeedCostHistory(b.pendingCostSeed)
		b.logger.Info("cost history restored", "entries", len(b.pendingCostSeed))
	}

	if len(b.pendingBudgetWindowSeed) > 0 {
		b.dashSrv.SeedBudgetWindowHistory(b.pendingBudgetWindowSeed)
		b.logger.Info("budget window history restored", "entries", len(b.pendingBudgetWindowSeed))
	}
	if len(b.pendingConvergenceSoakSeed) > 0 {
		b.dashSrv.SeedConvergenceSoak(b.pendingConvergenceSoakSeed)
		b.logger.Info("convergence soak history restored", "entries", len(b.pendingConvergenceSoakSeed))
	}

	if len(b.pendingTrendSeed) > 0 {
		b.dashSrv.SeedTrendHistory(b.pendingTrendSeed)
		b.logger.Info("trend history restored", "entries", len(b.pendingTrendSeed))
	}
}

// bootStores opens every agent's beads store: the enabled agents from
// config, orphan stores left on disk by agents no longer enabled, and the
// retro actor's store when retro is on.
func (b *boot) bootStores() {

	b.beadStores = make(map[string]*beads.Store)

	// Count stores that fail to open. They are dropped from beadStores entirely,
	// which makes an incomplete ledger indistinguishable from a smaller one — and
	// the dependency admission gate must not read a lookup miss in a truncated
	// ledger as "this candidate declared no dependencies".
	b.beadStoreLoadFailures = 0
	for name, agentCfg := range b.cfg.EnabledAgents() {
		store, err := beads.NewStore(agentCfg.BeadsDir)
		if err != nil {
			b.logger.Warn("failed to init beads store", "agent", name, "error", err)
			b.beadStoreLoadFailures++
			continue
		}
		store.SetHiveID(b.cfg.HiveID)
		b.beadStores[name] = store
		b.logger.Info("beads store initialized", "agent", name, "count", store.Count())
	}

	// Scan /data/beads/ for agent directories that have beads.json files on
	// disk but are not covered by the enabled-agent loop above. This handles
	// agents that were disabled between restarts or added by a previous ACMM
	// pack that is no longer active.
	const beadsRootDir = "/data/beads"
	if entries, err := os.ReadDir(beadsRootDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if _, exists := b.beadStores[name]; exists {
				continue // already loaded from config
			}
			agentBeadsDir := filepath.Join(beadsRootDir, name)
			beadsFile := filepath.Join(agentBeadsDir, "beads.json")
			if _, statErr := os.Stat(beadsFile); statErr != nil {
				continue // no beads.json in this directory
			}
			store, err := beads.NewStore(agentBeadsDir)
			if err != nil {
				b.logger.Warn("failed to load orphan beads store", "agent", name, "error", err)
				b.beadStoreLoadFailures++
				continue
			}
			store.SetHiveID(b.cfg.HiveID)
			b.beadStores[name] = store
			b.logger.Info("orphan beads store loaded from disk", "agent", name, "count", store.Count())
		}
	}

	if b.cfg.Retro.Enabled {
		if _, exists := b.beadStores[retro.Actor]; !exists {
			retroStore, err := beads.NewStore(filepath.Join(beadsRootDir, retro.Actor))
			if err != nil {
				b.logger.Warn("failed to init retro beads store", "error", err)
				b.beadStoreLoadFailures++
			} else {
				retroStore.SetHiveID(b.cfg.HiveID)
				b.beadStores[retro.Actor] = retroStore
				b.logger.Info("retro beads store initialized", "count", retroStore.Count())
			}
		}
	}
}

// bootCollectors starts the token, metrics, fleet-stats, activity and
// repo-cost collectors, defines refreshDashboard, and restores the cached
// actionable result into b.lastActionable.
func (b *boot) bootCollectors() { b.bootCollectorsWith(defaultBootCollectorsDeps()) }

// bootCollectorsWith is bootCollectors with its goroutines, PVC persistence,
// and GitHub lookups injected; see bootCollectorsDeps.
func (b *boot) bootCollectorsWith(deps bootCollectorsDeps) {
	initAgentConfigDrivenSystems(b.cfg)

	b.tokenCollector = tokens.NewCollector(b.cfg.Data.MetricsDir, b.logger)
	b.tokenCollector.SetClaudeSessionsDir(b.cfg.Data.ClaudeSessionsDir)
	b.tokenCollector.SetCopilotSessionsDir(b.cfg.Data.CopilotSessionsDir)
	b.tokenCollector.SetBobSessionsDir(b.cfg.Data.BobSessionsDir)
	tokenStop := make(chan struct{})
	deps.startTokenCollector(b.tokenCollector, tokenStop)
	b.cleanup.push(func() { close(tokenStop) })

	// Bound the session-state directories the collectors above read. Nothing
	// else deletes them, so without this the shared PVC grows forever.
	b.wireSessionPrune()

	// The coverage figure on the ci-maintainer card must describe THIS hive's
	// repo. The built-in gist is the flagship project's own badge, so it is
	// the default only for that project — the same gate collectOutreach uses
	// for adopters/ACMM. Every other hive sets HIVE_COVERAGE_BADGE_URL
	// (an http(s) badge, or repo://<ref>/<path> read via the App client) or
	// honestly shows 0; before this gate they all showed hivecommons/hive's
	// number, and when that gist broke they all dropped to 0 at once.
	badgeURL := resolveCoverageBadgeURL(os.Getenv(coverageBadgeURLEnv), b.cfg.Project.Org)
	primaryRepo := metricsPrimaryRepo(b.cfg.Project)
	b.metricsCollector = dashboard.NewMetricsCollector(b.ghClient, b.cfg.Project.Org, primaryRepo, badgeURL, b.cfg.Project.AIAuthor, b.cfg.Project.Name, b.logger)
	deps.startCollector(b.ctx, "metrics", b.metricsCollector)

	// Fleet-stats collector: computes this hive's AI-author contribution counts
	// (merged/rejected PRs, CVE-referencing PRs) across its org on a slow timer
	// and caches them, so each heartbeat can attach a fresh-but-cheap snapshot
	// the hub aggregates into the public landing page's live fleet-stats strip.
	// ai_author is optional config and hosted hives are provisioned without it,
	// so most spokes had an empty author — which silently disabled the collector
	// entirely (Start() returns early) and left the public fleet-stats strip
	// blank. Fall back to the bot token's own GitHub login: that IS the account
	// the agents open PRs as, so it is the correct author to count. Never fall
	// back to an org-wide search with no author filter — that would sweep in
	// human PRs and overstate what the fleet's agents actually did.
	//
	// Use EffectiveAIAuthor(), not the raw Project.AIAuthor field. App-authored
	// hives deliberately leave ai_author EMPTY and derive their identity from
	// the installed App ("<slug>[bot]") — that is what keeps App-bot mode
	// durable across restarts. Reading the raw field saw "" for every one of
	// them and disabled the collector fleet-wide, while the PAT fallback below
	// could not rescue it either: those hives authenticate as a GitHub App and
	// have github.token empty, so there was no token to identify. The result
	// was a fleet where essentially no spoke ever attempted a collect.
	fleetID := resolveFleetStatsIdentity(b.cfg.EffectiveAIAuthor(), b.cfg.GitHub.Token, os.Getenv("HIVE_GITHUB_TOKEN"),
		func(token string) (string, error) { return deps.lookupTokenLogin(token, b.cfg.GitHub.ResolvedAPIURL()) })
	fleetStatsAuthor := fleetID.author
	if fleetID.fromToken {
		b.logger.Info("fleet stats: ai_author unset, using bot token identity",
			"author", fleetStatsAuthor)
	}
	if fleetID.lookupErr != nil {
		b.logger.Warn("fleet stats: ai_author unset and bot identity lookup failed; "+
			"this hive will not contribute to the public fleet-stats total",
			"error", fleetID.lookupErr)
	}
	if !fleetID.enabled(b.cfg.Project.Org) {
		b.logger.Warn("fleet stats collector disabled: author or org is empty; "+
			"set project.ai_author in hive.yaml so this hive contributes to the fleet total",
			"author", fleetStatsAuthor, "org", b.cfg.Project.Org)
	}
	b.fleetStatsCollector = collect.NewFleetStatsCollector(b.ghClient, fleetStatsAuthor, b.cfg.Project.Org, b.logger)
	// Persist the collected counts on the /data PVC (same store as sessions and
	// cost/fact history) so a restart resumes from the last-known counts instead
	// of nil. Without this, a fleet-wide upgrade clears every spoke's in-memory
	// counts and the public landing-page total collapses until all spokes
	// re-collect (#2329, building on the hub-side #2328 defensive aging fix).
	deps.enablePersistence("fleet-stats", b.fleetStatsCollector, fleetStatsPersistPath)
	deps.startCollector(b.ctx, "fleet-stats", b.fleetStatsCollector)

	// Per-repo output-activity collector: reads the local audit log (no GitHub
	// calls) and summarizes issues/PRs/comments/merges/claims/reviews per repo
	// with recency, so the hub can tell — from the heartbeat alone — whether each
	// hive is producing output back to its work source. Persisted to the /data
	// PVC so a restart resumes the last summary; the collector loop reads
	// /data/audit.jsonl every few minutes.
	b.activityCollector = collect.NewActivityCollector(b.dashSrv.GetAudit(), "", b.logger)
	deps.enablePersistence("activity", b.activityCollector, activityPersistPath)
	deps.startCollector(b.ctx, "activity", b.activityCollector)

	// Per-repo cost collector: joins the same audited output events against
	// the token collector's per-message usage timeline, on the same ticker
	// interval as the activity collector above, and caches the result for
	// /api/repo-cost. Before this (#4943), the interval join — including
	// the same expensive audit read the activity collector does — ran on
	// every 60s dashboard poll, per open browser tab, instead of once per
	// collection interval.
	b.repoCostCollector = collect.NewRepoCostCollector(b.dashSrv.GetAudit(), b.tokenCollector, "", b.logger)
	deps.enablePersistence("repo-cost", b.repoCostCollector, repoCostPersistPath)
	deps.startCollector(b.ctx, "repo-cost", b.repoCostCollector)

	// Persistent hourly metrics behind the Operations + Leaderboard sparklines
	// (queue depth, tasks/hour, fleet size, per-contributor completions). The
	// store loads any prior 7-day history from the /data PVC on first use and the
	// rollup goroutine samples + buckets hourly, so a rolling upgrade resumes the
	// trend instead of flattening it. Bound to ctx so it shuts down cleanly with
	// the rest of the background loops (no goroutine leak). See contribute_metrics.go.
	deps.startContributeMetrics(b.ctx, b.dashSrv)
	b.refreshDashboard = func() {
		// Capture the mutation epoch BEFORE reading any state: if a mutation
		// (e.g. a restart-count or budget-window reset) lands while this
		// snapshot is being built, UpdateStatusIfFresh drops it so the stale
		// values never overwrite what the mutation's own refresh will publish
		// (#4348 — the restart-count flicker).
		buildEpoch := b.dashSrv.BeginStatusSnapshot()
		actionable := b.lastActionable.Load()
		govState := b.gov.GetState()
		agentStatuses := b.agentMgr.AllStatuses()
		payload := dashboard.BuildFrontendStatus(
			govState,
			actionable,
			agentStatuses,
			b.cfg,
			b.tokenCollector,
			b.gov,
			b.beadStores,
			b.ghClient,
			b.ctx,
			b.metricsCollector,
		)
		if d := b.dashSrv.GetAdvisoryDigest(); d != nil {
			payload.AdvisoryDigest = d
		}
		attachReviewLinksForDashboard(payload, b.logger)
		b.dashSrv.UpdateStatusIfFresh(payload, buildEpoch)
	}

	if data, err := deps.readLastActionable(); err == nil {
		var cached github.ActionableResult
		if err := json.Unmarshal(data, &cached); err == nil {
			b.lastActionable.Store(&cached)
			b.gov.SeedQueueState(cached.Issues.Count, cached.PRs.Count, cached.Hold.Total, cached.Issues.SLAViolations)
			b.refreshDashboard()
			b.logger.Info("restored cached actionable data", "issues", cached.Issues.Count, "prs", cached.PRs.Count, "age", time.Since(cached.GeneratedAt).Round(time.Second))
		}
	}
}

// bootKnowledge builds the knowledge API, connects vaults, git and document
// sources, starts the bead synthesizer, promotion scheduler and graph store,
// loads nous state, and resumes or parks the brainstorm inception.
func (b *boot) bootKnowledge() { b.bootKnowledgeWith(defaultBootKnowledgeDeps()) }

// bootKnowledgeWith is bootKnowledge with its disk and goroutine effects
// injected; see bootKnowledgeDeps.
func (b *boot) bootKnowledgeWith(deps bootKnowledgeDeps) {
	if b.cfg.Knowledge.Enabled {
		layers := convertKnowledgeLayers(b.cfg.Knowledge.Layers)
		// The curator block was previously dropped here, so NewPromoter always
		// received a zero CuratorConfig and AutoPromoteThreshold never reached
		// the promoter in production. Passing it through is what makes the
		// threshold gate real for the scheduled sweep (#5430).
		b.knowledgeAPI = knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{
			Enabled: b.cfg.Knowledge.Enabled,
			Engine:  b.cfg.Knowledge.Engine,
			Curator: curatorConfigFromHive(b.cfg.Knowledge.Curator),
		}, b.logger)
	}

	// Auto-connect configured vaults and start git-sync for Obsidian Git integration
	b.gitSyncer = knowledge.NewGitSyncer(b.logger)
	for _, vc := range b.cfg.Knowledge.Vaults {
		if err := deps.initVaultRepo(vc.Path, b.logger); err != nil {
			b.logger.Warn("failed to init vault directory", "name", vc.Name, "path", vc.Path, "error", err)
			continue
		}
		if err := deps.seedVaultContent(vc.Path, b.logger); err != nil {
			b.logger.Warn("failed to seed vault content", "name", vc.Name, "error", err)
		}
		if b.knowledgeAPI != nil {
			if err := b.knowledgeAPI.ConnectVault(vc.Path, vc.Name); err != nil {
				b.logger.Warn("failed to connect vault", "name", vc.Name, "path", vc.Path, "error", err)
				continue
			}
			b.logger.Info("vault auto-connected", "name", vc.Name, "path", vc.Path, "auto_index", vc.AutoIndex)
			if b.primer = b.sched.GetPrimer(); b.primer != nil {
				store := b.knowledgeAPI.GetVaultStore(vc.Path)
				if store != nil {
					b.primer.AddFileStore(vc.Name, store, knowledge.LayerPersonal)
					b.logger.Info("vault registered with primer", "name", vc.Name)
				}
			}
		}
		if vc.GitSync {
			// Find the store we just connected so the syncer can trigger reindex
			for _, vi := range b.knowledgeAPI.Vaults() {
				if vi.Name == vc.Name {
					// Re-fetch the FileStore by connecting info — the syncer needs it
					// to call Reindex() after each pull
					store := b.knowledgeAPI.GetVaultStore(vc.Path)
					if store != nil {
						b.gitSyncer.Add(vc.Name, vc.Path, store)
					}
					break
				}
			}
		}
	}

	// Auto-connect configured git sources (remote repos indexed as knowledge)
	for _, gsc := range b.cfg.Knowledge.GitSources {
		if b.knowledgeAPI == nil {
			// Knowledge not enabled but git sources configured — auto-enable
			b.knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, b.logger)
			b.logger.Info("auto-enabled knowledge API for git sources")
		}
		gsConfig := knowledge.GitSourceConfig{
			Name:    gsc.Name,
			URL:     gsc.URL,
			Branch:  gsc.Branch,
			Subpath: gsc.Subpath,
			Layer:   knowledge.LayerType(gsc.Layer),
		}
		if err := b.knowledgeAPI.ConnectGitSource(b.ctx, gsConfig); err != nil {
			b.logger.Warn("failed to connect git source",
				"name", gsc.Name,
				"url", gsc.URL,
				"subpath", gsc.Subpath,
				"error", err,
			)
		} else {
			b.logger.Info("git source connected",
				"name", gsc.Name,
				"url", gsc.URL,
				"subpath", gsc.Subpath,
				"layer", gsc.Layer,
			)
			// Register the FileStore with the scheduler's primer so agents
			// get primed with facts from this git source during kicks.
			if b.primer = b.sched.GetPrimer(); b.primer != nil {
				for _, gs := range b.knowledgeAPI.GitSources() {
					if gs.Name == gsc.Name && gs.Ready {
						store := b.knowledgeAPI.GetGitSourceStore(gsc.Name)
						if store != nil {
							b.primer.AddFileStore(gsc.Name, store, knowledge.LayerType(gsc.Layer))
						}
						break
					}
				}
			}
		}
	}

	// Auto-import configured document sources (PDFs, URLs as knowledge)
	for _, doc := range b.cfg.Knowledge.Documents {
		if b.knowledgeAPI == nil {
			b.knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, b.logger)
			b.logger.Info("auto-enabled knowledge API for document sources")
		}
		docConfig := knowledge.DocSourceConfig{
			Name:     doc.Name,
			URL:      doc.URL,
			FilePath: doc.FilePath,
			Layer:    knowledge.LayerType(doc.Layer),
		}
		meta, err := b.knowledgeAPI.ImportDocument(b.ctx, docConfig)
		if err != nil {
			b.logger.Warn("failed to import document source",
				"name", doc.Name,
				"error", err,
			)
		} else {
			b.logger.Info("document source imported",
				"name", doc.Name,
				"facts", meta.FactCount,
				"content_type", meta.ContentType,
			)
		}
	}

	deps.startGitSyncer(b.ctx, b.gitSyncer)

	// Auto-enable knowledge API when not explicitly configured.
	// Both bead-synth-wiki and inception require it.
	if b.knowledgeAPI == nil {
		b.knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
			Enabled: true,
			Engine:  "file",
		}, b.logger)
		b.logger.Info("auto-enabled file-based knowledge API")
	}
	if len(b.beadStores) > 0 {
		synthVaultPath := b.cfg.Knowledge.BeadSynthesizer.VaultPath
		if synthVaultPath == "" {
			synthVaultPath = beadSynthVaultDefaultPath
		}
		if err := os.MkdirAll(synthVaultPath, 0o755); err != nil {
			b.logger.Warn("failed to create bead-synth vault dir", "path", synthVaultPath, "error", err)
		}
		if b.knowledgeAPI != nil {
			if connErr := b.knowledgeAPI.ConnectVault(synthVaultPath, "bead-synth-wiki"); connErr != nil {
				b.logger.Warn("failed to auto-connect bead-synth vault", "path", synthVaultPath, "error", connErr)
			} else {
				b.logger.Info("auto-connected bead-synth vault", "path", synthVaultPath)
				if b.primer = b.sched.GetPrimer(); b.primer != nil {
					store := b.knowledgeAPI.GetVaultStore(synthVaultPath)
					if store != nil {
						beadLayer := knowledge.LayerType(b.cfg.Knowledge.BeadSynthesizer.TargetLayer)
						if beadLayer == "" {
							beadLayer = knowledge.LayerPersonal
						}
						b.primer.AddFileStore("bead-synth-wiki", store, beadLayer)
						b.logger.Info("bead-synth vault registered with primer", "layer", beadLayer)
					}
				}
			}
		}
		var rawGH *gh.Client
		if b.ghClient != nil {
			rawGH = b.ghClient.GoGitHub()
		}

		var kRetention *knowledge.RetentionPolicy
		if rp := b.cfg.Knowledge.BeadSynthesizer.RetentionPolicy; rp != nil {
			kRetention = &knowledge.RetentionPolicy{
				MaxBeads:               rp.MaxBeads,
				ArchiveAfterSynthDays:  rp.ArchiveAfterSynthDays,
				HighPriorityRetainDays: rp.HighPriorityRetainDays,
				PreserveWithDeps:       rp.PreserveWithDeps,
			}
		} else {
			kRetention = &knowledge.RetentionPolicy{
				PreserveWithDeps: true,
			}
		}

		b.beadSynth = knowledge.NewBeadSynthesizer(b.beadStores, b.knowledgeAPI, knowledge.BeadSynthesizerConfig{
			Schedule:         b.cfg.Knowledge.BeadSynthesizer.Schedule,
			MinConfidence:    b.cfg.Knowledge.BeadSynthesizer.MinConfidence,
			TargetLayer:      b.cfg.Knowledge.BeadSynthesizer.TargetLayer,
			MaxFactsPerCycle: b.cfg.Knowledge.BeadSynthesizer.MaxFactsPerCycle,
			VaultPath:        synthVaultPath,
			Org:              b.cfg.Project.Org,
			Repos:            b.cfg.Project.Repos,
			RetentionPolicy:  kRetention,
		}, b.logger, rawGH)

		if cleaned, err := b.beadSynth.CleanupVault(); err != nil {
			b.logger.Warn("vault cleanup failed", "error", err)
		} else if cleaned > 0 {
			b.logger.Info("cleaned up low-quality bead-synth facts", "removed", cleaned)
		}

		if b.cfg.Knowledge.BeadSynthesizer.IsEnabled() && b.knowledgeAPI != nil {
			deps.startBeadSynth(b.ctx, b.beadSynth)
			b.logger.Info("bead-to-wiki synthesizer started",
				"schedule", b.cfg.Knowledge.BeadSynthesizer.Schedule,
				"target_layer", b.cfg.Knowledge.BeadSynthesizer.TargetLayer,
				"vault_path", synthVaultPath,
				"bead_stores", len(b.beadStores),
			)
		}
	}

	// Scheduled knowledge promotion (#5430). knowledge.curator.schedule used to
	// be parsed, defaulted to "daily", and never read. It now drives a real
	// sweep — but ONLY when knowledge.curator.enabled is explicitly true.
	// StartBackground is a no-op otherwise, and logs a notice if a schedule was
	// configured without the opt-in so the mismatch is visible rather than
	// silent. Do not replace the IsEnabled() guard with a schedule check: that
	// would enable unreviewed promotion on every hive that omits the key.
	if b.knowledgeAPI != nil && b.cfg.Knowledge.Curator.IsEnabled() {
		b.promotionScheduler = knowledge.NewPromotionScheduler(
			b.knowledgeAPI.Promoter(),
			curatorConfigFromHive(b.cfg.Knowledge.Curator),
			b.logger,
		)
		deps.startPromotion(b.ctx, b.promotionScheduler)
	} else if b.cfg.Knowledge.Curator.Schedule != "" {
		b.logger.Info("knowledge.curator.schedule is set but scheduled promotion is disabled",
			"schedule", b.cfg.Knowledge.Curator.Schedule,
			"hint", "set knowledge.curator.enabled: true to opt in",
		)
	}

	// Open the graph store in a background goroutine. NewGraphStore acquires
	// a SQLite file lock that blocks if the old pod still holds it. Deferring
	// this lets the HTTP server start so the readiness probe passes, which
	// tells Kubernetes to terminate the old pod and release the lock.
	deps.openGraphStoreAsync(b.logger, func(graphStore *knowledge.GraphStore, graphErr error) {
		if graphErr != nil {
			b.logger.Warn("failed to open knowledge graph store", "path", knowledgeGraphStorePath, "error", graphErr)
			return
		}
		b.logger.Info("knowledge graph store opened", "path", knowledgeGraphStorePath)
		if b.primer = b.sched.GetPrimer(); b.primer != nil {
			b.primer.SetGraphStore(graphStore)
		}
		if b.knowledgeAPI != nil {
			b.knowledgeAPI.SetGraphStore(graphStore)
			if b.primer = b.sched.GetPrimer(); b.primer != nil {
				b.knowledgeAPI.WireContext7Suggester(b.primer)
			}
		}
		if b.beadSynth != nil {
			b.beadSynth.SetGraphStore(graphStore)
		}
		if b.knowledgeAPI != nil {
			for _, ls := range b.knowledgeAPI.FileStores() {
				if n, err := graphStore.SyncFromFileStore(ls); err != nil {
					b.logger.Warn("graph sync failed", "store", ls.Name(), "error", err)
				} else if n > 0 {
					b.logger.Info("graph synced from vault", "store", ls.Name(), "triples", n)
				}
			}
		}
	})

	deps.startWorkspaceCleanup(b.ctx, b.logger, b.dashSrv.GetAudit())

	deps.ensureNousDirs(b.logger)
	b.nousState = deps.loadNousState(b.logger)
	b.nousState.SnapshotDir = nousSnapshotDir

	b.inceptionEngine = deps.newInceptionEngine(b.knowledgeAPI, b.logger)
	b.sched.SetInception(b.inceptionEngine)

	// Brainstorm is on-demand only. Only restart with bootstrap during
	// capture phase — structure/scaffold phases don't need a fresh kick
	// and restarting would revert the phase back to capture.
	// Skip stale inceptions (> 10 min old) — these are leftovers from
	// previous runs that would interfere with new inceptions.
	const staleInceptionThreshold = 10 * time.Minute
	if state := b.inceptionEngine.GetState(); state != nil &&
		state.Phase != knowledge.PhaseComplete &&
		state.Phase != knowledge.PhaseScaffold {
		if time.Since(state.StartedAt) < staleInceptionThreshold {
			msg := b.sched.BuildAgentMessage("brainstorm", nil, nil)
			if err := deps.restartBrainstorm(b.ctx, b.agentMgr, msg); err != nil {
				b.logger.Warn("failed to resume brainstorm for active inception", "error", err)
			} else {
				b.logger.Info("brainstorm resumed for active inception", "phase", state.Phase)
			}
		} else {
			b.logger.Info("skipping stale inception resume — resetting",
				"phase", state.Phase,
				"age", time.Since(state.StartedAt).Round(time.Second),
			)
			_ = b.inceptionEngine.Reset()
			if err := b.agentMgr.Pause("brainstorm", "startup", "stale inception cleared — on-demand only"); err != nil {
				b.logger.Debug("brainstorm pause on startup", "error", err)
			}
		}
	} else {
		if err := b.agentMgr.Pause("brainstorm", "startup", "on-demand agent — triggered by inception only"); err != nil {
			b.logger.Debug("brainstorm pause on startup", "error", err)
		}
	}

	// Provider rotation (RFC #3958): opt-in automatic failover when a
	// provider's subscription/credit is exhausted. Nil when disabled.
	//
	// The contributor quota reading publisher is deliberately NOT gated on
	// rotation being enabled (kubestellar/hive#6987, condition (a) in
	// src/docs/contributor-relay.md): the pool directory is derived per-install
	// when HIVE_CONTRIBUTOR_QUOTA_POOL_DIR is not set, so a default install
	// gets a real reading with no hand-configured env var (#6967 criterion 1)
	// and no rotation opt-in. An explicit HIVE_CONTRIBUTOR_QUOTA_POOL_DIR
	// still overrides the location in both modes.
	b.quotaAccount = strings.TrimSpace(os.Getenv("HIVE_CONTRIBUTOR_QUOTA_POOL_ACCOUNT"))
	b.quotaPoolDir = strings.TrimSpace(os.Getenv("HIVE_CONTRIBUTOR_QUOTA_POOL_DIR"))
	b.explicitQuotaPoolDir = b.quotaPoolDir != ""
	if b.quotaPoolDir == "" {
		b.quotaPoolDir = rotation.DefaultContributorPoolDir()
	}
}

// bootSupervision starts provider rotation and the agent watchdog, and
// wires the Linear credential resolver, in-flight lookup and PR-opened hook.
func (b *boot) bootSupervision() {
	if b.cfg.Governor.Rotation.Enabled {
		b.rotationMgr = rotation.NewManager(b.cfg.Governor.Rotation)
		// Publish normalized readings to where the contributor quota guard reads
		// them (kubestellar/hive#6967), off the rotation manager's own probe
		// loop and the operator's configured provider→backends map.
		if b.quotaPoolDir != "" {
			b.rotationMgr.EnableContributorReadingPublish(b.quotaPoolDir, b.quotaAccount)
			b.logger.Info("contributor quota reading publisher enabled", "pool_dir", b.quotaPoolDir, "explicit_pool_dir", b.explicitQuotaPoolDir)
		}
		b.rotationMgr.Start(b.ctx)
		b.logger.Info("provider rotation enabled",
			"threshold_pct", b.cfg.Governor.Rotation.EffectiveThreshold(),
			"providers", len(b.cfg.Governor.Rotation.Providers))
	} else if b.quotaPoolDir != "" {
		// Rotation disabled (the default): run a PUBLISH-ONLY manager so a
		// supported backend still gets a reading (kubestellar/hive#6987). It is
		// kept off rotationMgr on purpose — rotation decision paths
		// (runRotationCheck, dashboard RotationMgr) stay nil/disabled, so this
		// publishes data and rotates nothing. A probe that fails because the
		// CLI is not installed publishes nothing rather than an `unknown` the
		// relay would hold on (see rotation.NewContributorReadingPublisher).
		b.quotaReadingPublisher = rotation.NewContributorReadingPublisher(b.quotaPoolDir, b.quotaAccount)
		b.quotaReadingPublisher.Start(b.ctx)
		b.logger.Info("contributor quota reading publisher enabled (publish-only; provider rotation disabled)",
			"pool_dir", b.quotaPoolDir, "explicit_pool_dir", b.explicitQuotaPoolDir)
	}

	// Agent self-healing watchdog (RFC #4665): liveness/readiness
	// reconciliation on the governor tick. Config problems fall back to the
	// RFC defaults loudly — a typo must not disable self-healing silently.
	wdSettings, wdCfgErrs := watchdog.SettingsFrom(b.cfg.Governor.Watchdog)
	for _, e := range wdCfgErrs {
		b.logger.Warn("watchdog config problem", "error", e)
	}
	if wdSettings.Enabled() {
		wdFleet := agent.WatchdogFleet{
			M: b.agentMgr,
			// Queue depth for the readiness gate: an agent producing nothing
			// while nothing is queued is correct, not unhealthy. Read live
			// from the governor so it reflects the current sweep.
			Queued: func() (int, bool) {
				st := b.gov.GetState()
				return st.QueueIssues + st.QueuePRs, true
			},
		}
		b.wd = watchdog.New(wdSettings, wdFleet, b.dashSrv, b.logger,
			watchdog.WithAuthProbes(watchdogAuthProbes(b.cfg)))
		if b.saved != nil && len(b.saved.Watchdog) > 0 {
			b.wd.Restore(b.saved.Watchdog)
		}
		// Dead-session recovery moves under the watchdog's bounded ladder ONLY
		// when the watchdog may actually act. In observe mode the manager's
		// crash loop keeps its existing job, so there is never a window in
		// which neither component restarts a dead agent.
		b.agentMgr.SetDeadSessionRecoveryOwner(wdSettings.MayAct())
		b.logger.Info("agent watchdog enabled (RFC #4665)",
			"mode", string(wdSettings.Mode),
			"probe_interval", wdSettings.ProbeInterval,
			"crash_loop_after", wdSettings.CrashLoopAfter,
			"auth_probe", wdSettings.AuthProbe,
			"dead_session_recovery", map[bool]string{true: "watchdog", false: "crash-loop"}[wdSettings.MayAct()])
		if wdSettings.Mode == watchdog.ModeObserve {
			b.logger.Info("agent watchdog is in OBSERVE mode: it will classify agents, publish conditions and record what it WOULD have done, but will not restart or pause anything. Set governor.watchdog.mode: heal to enable healing.")
		}
	} else {
		b.logger.Info("agent watchdog disabled by config", "mode", string(wdSettings.Mode))
	}

	// Linear write credential for ISSUES_ONLY+ agents (GitHub-issue parity):
	// prefer the connected Linear agent app's OAuth token, so agent writes are
	// authored by the same "Hive" app identity that acknowledges sessions —
	// the analogue of App-bot authorship on GitHub — and fall back to the
	// work-source API key from hive.yaml. Resolved live off the dashboard's
	// install store and the cfg pointer so a workspace connected after boot
	// reaches agents on their next launch / hourly token refresh. Values are
	// never logged. Wired before RegisterAPI so the resolver is in place
	// before any agent launches.
	//
	// The same resolver is handed to the egress proxy
	// (githubProxy.SetLinearCredentialResolver in wireSpokeProxyReadyAndLaunch),
	// which attaches the CURRENT credential to every ISSUES_ONLY+ agent request
	// to api.linear.app — the rotated OAuth token never reaches a running CLI
	// through the session environment (a process keeps the environment it was
	// forked with), so the proxy, not the environment, is what keeps agent
	// Linear writes authenticated across rotations.
	b.linearCredentialResolver = func() agent.LinearCredential {
		if tok := b.dashSrv.LinearAgentAccessToken(); tok != "" {
			return agent.LinearCredential{AccessToken: tok}
		}
		if b.cfg.Governor.WorkSource.Type == "linear" {
			return agent.LinearCredential{APIKey: strings.TrimSpace(b.cfg.Governor.WorkSource.Linear.APIKey)}
		}
		return agent.LinearCredential{}
	}
	b.agentMgr.SetLinearCredentialResolver(b.linearCredentialResolver)

	// In-flight ledger + session PR link (Linear GitHub-parity follow-ups):
	// the scheduler withholds work a Linear session is already working, and
	// the pr-request watcher narrates opened PRs into the session.
	b.sched.SetInflightLookup(b.dashSrv.LinearSessionHolder)
	if b.ghClient != nil {
		b.ghClient.SetPROpenedHook(func(agentName, repo string, number int, url string) {
			b.dashSrv.LinearAgentPROpened(agentName, repo, number, url)
			// Same typed hook feeds the lifecycle timeline: the watcher fires
			// it on the exact path that opened the PR, with the agent name the
			// audit stream attributes to the governor flow (#5656). The store
			// dedupes with the audit-sink bridge by (ref, kind).
			recordPROpened(b.dashSrv, b.cfg.Project.Org, agentName, repo, number, url)
		})
	}
}

func attachReviewLinksForDashboard(payload *dashboard.StatusPayload, logger *slog.Logger) {
	if payload == nil {
		return
	}
	reviewLinks, err := github.LoadReviewLinks("")
	if err != nil {
		if logger != nil {
			logger.Warn("could not load review links for the status snapshot", "error", err)
		}
		return
	}
	dashboard.AttachReviewLinks(payload, reviewLinks)
}

// bootDashboardAPI registers the dashboard API with its dependencies and
// callbacks, publishes the GitHub App banner state, installs the Re-check
// callback, and starts the installation-discovery, banner self-heal and
// inception watcher loops.
func (b *boot) bootDashboardAPI() { b.bootDashboardAPIWith(defaultBootDashboardAPIDeps()) }

// bootDashboardAPIWith is bootDashboardAPI with its long-lived effects
// injected; see bootDashboardAPIDeps.
func (b *boot) bootDashboardAPIWith(deps bootDashboardAPIDeps) {
	deps.registerAPI(b.dashSrv, b.dashboardDependencies())
	// Forge App tab inventory: the resolved active key path and the per-app-id
	// PVC keys live here in cmd/hive, so they are injected as a provider (the
	// SetGitHubAppRecheckFn pattern). Fingerprints and paths only — the
	// provider never touches key material.
	deps.setForgeAppInventory(b.dashSrv, func() dashboard.ForgeAppInventory {
		held := appKeys.HeldFingerprints()
		keys := make([]dashboard.ForgeAppKey, 0, len(held))
		for idStr, fp := range held {
			keys = append(keys, dashboard.ForgeAppKey{
				AppID:       idStr,
				Path:        appKeys.PerAppIDKeyPathFor(idStr),
				Fingerprint: fp,
			})
		}
		return dashboard.ForgeAppInventory{
			ActiveKeyFile: appKeys.Resolve(b.cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), b.cfg.GitHub.AppID),
			HeldKeys:      keys,
		}
	})

	b.dashSrv.SetGitHubAppRequired(b.githubAppRequired)
	// Order matters: SetGitHubAppRequired(false) clears both fields, so the
	// classified state is applied only after it, and only when a failure was
	// actually detected.
	if b.githubAppRequired {
		b.dashSrv.SetGitHubAppState(b.githubAppState.String())
		if b.githubAppDiag != "" {
			b.dashSrv.SetGitHubAppPermIssue(b.githubAppDiag)
		}
	}

	// Wire up the manual re-check callback for the dashboard button.
	{
		recheckRepo := b.cfg.Project.PrimaryRepo
		if recheckRepo == "" && len(b.cfg.Project.Repos) > 0 {
			recheckRepo = b.cfg.Project.Repos[0]
		}
		if recheckRepo != "" {
			b.dashSrv.SetGitHubAppRecheckFn(func() bool {
				// The Re-check button is the first thing an owner clicks on a
				// degraded hive. Report the real cause instead of the generic
				// "not accessible" — there is no client to check WITH, so the
				// credentials themselves are what must be fixed.
				if b.ghClient == nil {
					b.logger.Warn("github app recheck: hive is running without GitHub credentials", "detail", b.appAuthFailure)
					b.dashSrv.AuditLog("system", "github_app_check", "result=no GitHub client: "+b.appAuthFailure, "")
					return false
				}
				// #4360: ask about repo COVERAGE before attempting a read.
				// A repo the installation does not cover answers 404, which is
				// indistinguishable from "no such repo" and used to be reported
				// as "app not installed / no read" — sending the operator after
				// credentials that were never broken. Checking first means the
				// specific, correct message wins over the generic one.
				if raise, diag, state := classifyGitHubAppRepoCoverage(b.ctx, b.ghClient.AppAuth(), b.cfg.Project.Org, b.cfg.Project.Repos, b.logger); raise {
					b.dashSrv.SetGitHubAppPermIssue(diag)
					b.dashSrv.SetGitHubAppState(state.String())
					b.logger.Warn("github app recheck: installation does not cover every configured repo",
						"org", b.cfg.Project.Org, "state", state.String(), "detail", diag)
					b.dashSrv.AuditLog("system", "github_app_check", "result=repos not in installation: "+diag, "")
					return false
				}
				num, err := b.ghClient.EnsureAdvisoryIssue(b.ctx, recheckRepo)
				if err != nil {
					b.logger.Debug("github app recheck: not accessible", "repo", recheckRepo, "error", err)
					b.dashSrv.AuditLog("system", "github_app_check", "result=not accessible (app not installed / no read)", "")
					return false
				}
				b.advisoryIssues[recheckRepo] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
				// Finding the advisory issue only proves the app is installed
				// (reads succeed on public repos even with a token from the
				// wrong installation). Verify write capability before letting
				// the handler clear the banner, so Re-check can't produce a
				// clears-then-returns flip-flop.
				// Before reporting a wrong-account installation, try to fix it:
				// this is the exact case rediscovery exists for. Cached by TTL,
				// so a repeated re-check does not re-hit the API.
				healGitHubAppInstallation(b.ctx, b.ghClient.AppAuth(), b.cfg, b.logger)
				// Shared verdict with the boot and advisory-digest paths. Re-check
				// previously branched on `diag != ""` while boot branched on the
				// error string, which is how the two came to disagree about the
				// same hive; routing both through classifyGitHubAppFailure means
				// they cannot drift again.
				if raise, diag, state := classifyGitHubAppFailure(b.ctx, b.ghClient.AppAuth(), b.cfg.Project.Org, b.logger); raise {
					b.dashSrv.SetGitHubAppPermIssue(diag)
					b.dashSrv.SetGitHubAppState(state.String())
					b.logger.Warn("github app recheck: app detected but write not verified",
						"repo", recheckRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable(), "detail", diag)
					b.dashSrv.AuditLog("system", "github_app_check", "result=installed but write NOT verified: "+diag, "")
					return false
				}
				// #2353: the classifier above only proves the installation
				// authenticates and grants issues:write — NOT that this repo can
				// actually be written. Finding the advisory issue is a READ, which
				// succeeds even when the repo is not in the App installation's
				// selected repos. Perform a REAL write probe before clearing the
				// banner, so re-check cannot falsely "verify write" for a repo the
				// App can only read (the recheck false-positive).
				if werr := b.ghClient.ProbeIssueWrite(b.ctx, recheckRepo, num); werr != nil {
					if strings.Contains(werr.Error(), "403") && strings.Contains(werr.Error(), "Resource not accessible by integration") {
						msg, state := classifyGitHubAppWriteForbidden(b.ctx, b.ghClient.AppAuth(), b.cfg.Project.Org, recheckRepo)
						b.dashSrv.SetGitHubAppPermIssue(msg)
						b.dashSrv.SetGitHubAppState(state.String())
						b.logger.Warn("github app recheck: write probe returned 403 — not clearing the banner",
							"repo", recheckRepo, "state", state.String(), "detail", msg)
						b.dashSrv.AuditLog("system", "github_app_check", "result=write probe FORBIDDEN: "+msg, "")
						return false
					}
					// A non-403 probe failure is inconclusive (rate limit,
					// transient network). Do NOT clear the banner on a write we
					// could not confirm, but also do NOT accuse anyone.
					b.logger.Warn("github app recheck: write probe inconclusive — leaving banner as-is",
						"repo", recheckRepo, "error", werr)
					b.dashSrv.AuditLog("system", "github_app_check", "result=write probe inconclusive", "")
					return false
				}
				b.logger.Info("github app recheck: app detected, write verified", "repo", recheckRepo, "number", num)
				b.dashSrv.AuditLog("system", "github_app_check", "result=OK (installed, write verified)", "")
				return true
			})
		}
	}

	// If the App credentials are present but github.installation_id is still
	// empty, discover it automatically. This covers the delayed approval path:
	// a non-admin requests installation, an org admin approves later, and the
	// spoke adopts the installation ID without requiring anyone to paste it.
	deps.startInstallDiscovery(b.ctx, func() {
		if b.cfg.GitHub.InstallationID != 0 {
			return
		}
		_, _ = b.dashSrv.AutoDiscoverGitHubInstallationID(b.ctx, false)
	})

	// Self-heal the "GitHub App not installed" banner. This handles:
	//  1. GitHub App credentials arrived after startup (via heartbeat/webhook)
	//  2. ReinitGitHubFunc succeeded but cleared githubAppRequired before
	//     EnsureAdvisoryIssue could run against the new client
	//  3. A TRANSIENT startup/runtime 4xx (rate-limit blip, brief token-refresh
	//     window, momentary permission propagation delay) latched the banner even
	//     though the app is really installed and can write. Previously the retry
	//     loop exited permanently after the first advisory-issue READ succeeded,
	//     so a later transient write failure that re-set the flag was never
	//     re-evaluated — the banner stuck until the pod was restarted.
	//
	// The loop therefore runs for the lifetime of the process (it does NOT
	// return after the first success) and, whenever the banner is currently
	// showing, re-runs the SAME read+write verification as the manual "Re-check"
	// button (githubAppRecheckFn, which calls diagnoseGitHubApp) and clears
	// the flag on success. When the banner is not showing there is nothing to do,
	// so the tick is a cheap no-op that makes no GitHub API calls.
	{
		primaryRepo := b.cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(b.cfg.Project.Repos) > 0 {
			primaryRepo = b.cfg.Project.Repos[0]
		}
		if primaryRepo != "" {
			deps.startSelfHeal(b.ctx, func() {
				// Nothing to heal unless the banner is showing.
				if !b.dashSrv.IsGitHubAppRequired() {
					return
				}
				_, _ = b.dashSrv.AutoDiscoverGitHubInstallationID(b.ctx, false)
				// Re-run the same read+write verification the manual
				// Re-check button uses. It clears the flag on success
				// (installed AND write-verified) and leaves it set on a
				// genuine failure (not installed / insufficient perms).
				if b.dashSrv.RecheckGitHubApp() {
					if num, exists := b.advisoryIssues[primaryRepo]; exists {
						_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
					}
					b.logger.Info("github app self-heal: banner cleared, app installed and write verified", "repo", primaryRepo)
				} else {
					b.logger.Debug("github app self-heal: still not verified, banner remains", "repo", primaryRepo)
				}
			})
		}
	}

	if brainstormBeads, ok := b.beadStores["brainstorm"]; ok {
		deps.startInceptionWatcher(b.ctx, dashboard.NewInceptionWatcher(brainstormBeads, b.inceptionEngine, b.sched, b.agentMgr, b.gov, b.logger))
	}
}

// bootPolicies applies the ACMM pack planACMMBoot chose and starts the
// policies repo watcher.
func (b *boot) bootPolicies() { b.bootPoliciesWith(defaultBootPoliciesDeps()) }

// bootPoliciesWith is bootPolicies with the pack apply and policy watcher
// injected; see bootPoliciesDeps.
func (b *boot) bootPoliciesWith(deps bootPoliciesDeps) {
	// The ACMM pack decision (config vs persisted vs HIVE_LEVEL, and whether
	// this is a merge or a re-apply) lives in planACMMBoot (#7232); only the
	// ApplyPack effect and its audit lines stay here.
	var savedACMMLevel *int
	if b.saved != nil {
		savedACMMLevel = b.saved.ACMMLevel
	}
	acmmPlan := planACMMBoot(b.saved == nil, b.cfg.ACMMLevel, savedACMMLevel, os.Getenv(hiveLevelEnv))
	if acmmPlan.invalidEnv != "" {
		b.logger.Warn("invalid HIVE_LEVEL, skipping auto-apply", "value", acmmPlan.invalidEnv)
	}
	if acmmPlan.level > 0 {
		if b.saved == nil {
			b.logger.Info("first start detected, auto-applying ACMM pack", "level", acmmPlan.level)
		} else {
			b.logger.Info("audit: "+acmmPlan.action, "level", acmmPlan.level, "saved_level", b.saved.ACMMLevel, "trigger", "startup")
		}
		result, err := deps.applyPack(b.dashSrv, acmmPlan.level)
		switch {
		case err != nil && b.saved == nil:
			b.logger.Error("failed to auto-apply ACMM pack", "level", acmmPlan.level, "error", err)
		case err != nil:
			b.logger.Error("failed to apply ACMM pack", "level", acmmPlan.level, "error", err)
		case b.saved == nil:
			b.logger.Info("ACMM pack auto-applied",
				"level", acmmPlan.level,
				"name", result.Name,
				"created", result.Created,
				"skipped", result.Skipped,
				"paused", result.Paused,
				"resumed", result.Resumed,
			)
		default:
			b.logger.Info("ACMM pack applied on startup",
				"level", acmmPlan.level,
				"name", result.Name,
				"created", result.Created,
				"updated", result.Updated,
				"skipped", result.Skipped,
				"paused", result.Paused,
				"resumed", result.Resumed,
			)
		}
	}

	if b.cfg.Policies.Repo != "" {
		localDir := policiesLocalDir(b.cfg.Policies)
		if err := deps.startPolicyWatcher(b.ctx, b.cfg.Policies.Repo, b.cfg.Policies.Branch, b.cfg.Policies.Path, localDir, b.cfg.Policies.PollInterval, b.logger); err != nil {
			b.logger.Warn("policy watcher failed to start", "error", err)
		}
	}
}

// bootWatchers starts the hive.yaml watcher with its reload handler, wires
// pause persistence, the prompt and audit sinks, compiles the operator's
// hooks, installs the hook emitters, registers GHE hosts with the proxy
// allowlist and sets the dashboard's auth providers.
func (b *boot) bootWatchers() { b.bootWatchersWith(defaultBootWatchersDeps()) }

// bootWatchersWith is bootWatchers with its long-lived effects injected; see
// bootWatchersDeps.
func (b *boot) bootWatchersWith(deps bootWatchersDeps) {
	// Watch hive.yaml for external changes and reload config when modified
	b.configWatcher = deps.newConfigWatcher(b.configPath, func(newCfg *config.Config) {
		// Preserve runtime-only fields that are not in the YAML
		newCfg.HiveID = b.cfg.HiveID

		// Preserve ACMM level from the agent manager — it is the
		// authoritative source. The file may have a stale value if
		// a watcher reload races with a level-switch saveConfig().
		if b.cfg.ACMMLevel != nil {
			newCfg.ACMMLevel = b.cfg.ACMMLevel
		}

		// Preserve removed-agent tombstones across the swap as a union of the
		// live cfg and the incoming reload. LoadWithDashboardOverlay now carries
		// the overlay's tombstones into newCfg, but a removal that landed in the
		// live cfg after this reload's snapshot (or an overlay too short/stale to
		// echo it back yet) must not be lost — otherwise the next persistState
		// saver rewrites every layer tombstone-free and the deleted agents
		// reappear (#2439). Union keeps any tombstone present in either side.
		for _, name := range b.cfg.RemovedAgents {
			newCfg.MarkAgentRemoved(name)
		}
		newCfg.PruneRemovedAgents()

		// Observability (#2439): this is the ~2-min interval reload path, so keep it
		// at DEBUG to avoid spamming a healthy hive. When a removal is not sticking,
		// enabling DEBUG shows the tombstone surviving each swap — an empty count here
		// while the agent keeps reappearing localizes the leak to this union-preserve.
		b.logger.Debug("reload: preserved removed-agents",
			"hive_id", b.cfg.HiveID,
			"count", len(newCfg.RemovedAgents),
			"agents", newCfg.RemovedAgents,
		)

		// Capture the outgoing GitHub App identity before the swap so we can
		// tell whether the reload changed it.
		prevGitHub := b.cfg.GitHub

		// Swap the in-memory config pointer contents
		*b.cfg = *newCfg

		// Re-sync subsystems that cache config values
		b.ghClient.SetRepos(b.cfg.Project.Repos)
		syncAutoMergePolicyToGitHubClient(b.cfg, b.ghClient)
		b.gov.UpdateConfig(b.cfg.Governor)
		// A reload can add or archive repos, which moves every scaled default
		// threshold — re-sync it alongside the repo list above.
		b.gov.SetRepoCount(b.cfg.Project.RepoCount())
		b.agentMgr.SetSandboxConfig(b.cfg.AgentSandbox)
		// Re-run the posture check on reload, not only at boot: flipping the
		// Security tab's sandbox toggle writes the config and lands here, which
		// is the exact moment an operator forms the belief that they are now
		// sandboxed. See logAgentSandboxPosture.
		logAgentSandboxPosture(b.logger, b.cfg)

		// Hot-reload the state-triggered hooks (RFC #4001). Recompiles only
		// when the `hooks:` list actually changed, and swaps the registry in
		// place so per-hook rate-limit windows SURVIVE the reload — otherwise
		// a reload loop would be a way to clear the anti-storm ceiling.
		buildHookDispatcher(b.cfg, hookSinks{
			Notifier: b.notifier,
			AgentMgr: b.agentMgr,
			Timeline: b.dashSrv.LifecycleTimeline(),
			Audit:    b.dashSrv.AgentAuditSink(),
		}, b.logger)

		// Hot-reload the state-triggered hooks (RFC #4001). Recompiles only
		// when the `hooks:` list actually changed, and swaps the registry in
		// place so per-hook rate-limit windows SURVIVE the reload — otherwise
		// a reload loop would be a way to clear the anti-storm ceiling.
		buildHookDispatcher(b.cfg, hookSinks{
			Notifier: b.notifier,
			AgentMgr: b.agentMgr,
			Timeline: b.dashSrv.LifecycleTimeline(),
			Audit:    b.dashSrv.AgentAuditSink(),
			// #4000 ↔ #4001 seam: an `enqueue-approval` hook lands in the same
			// durable operator inbox the desk uses. nil when the desk is off,
			// which keeps the dispatcher's "no approval queue wired" error
			// honest rather than failing on every firing.
			Approvals: newHookApprovalAdapter(b.approvalInbox, toolapprove.ACMMLevelOf(b.cfg)),
		}, b.logger)
		configureEscalationDispatcher(b.ctx, b.cfg, b.notifier, b.dashSrv.AgentAuditSink(), b.logger)

		// Re-apply live agent definitions (definition_source) on reload so an
		// operator's edit to a linked repo propagates. Merges only operator-safe
		// fields; a fetch failure keeps each agent's baked definition. Runs before
		// initAgentConfigDrivenSystems so downstream systems see the merged config.
		defsrc.ApplyToConfig(context.Background(), b.cfg, b.definitionResolver, b.logger)
		if err := b.cfg.ExpandAgentReplicas(); err != nil {
			b.logger.Warn("failed to expand agent replicas after config reload", "error", err)
		}
		addedAgents := b.agentMgr.ReconcileAgents(b.cfg.EnabledAgents())
		for _, added := range addedAgents {
			if ac, ok := b.cfg.Agents[added]; ok && !ac.OnDemand {
				deps.startAgent(b.ctx, b.agentMgr, added, b.logger)
			}
		}
		b.gov.UpdateAgents(b.cfg.EnabledAgents())

		initAgentConfigDrivenSystems(b.cfg)

		// Rebuild GitHub App auth when its identity changed. AppAuth captures
		// app_id/installation_id at construction, so without this a corrected
		// installation_id in hive.yaml keeps minting tokens for the OLD
		// installation until the pod restarts.
		//
		// RESOLVE the key file rather than reading cfg.GitHub.KeyFile raw. An
		// unset key_file is the CORRECT steady state on a hosted spoke — the
		// heartbeat apply path deliberately does not persist one, because the
		// path is derivable from app_id and a stored value outlives the App it
		// was derived for. Gating on the raw field therefore skipped the rebuild
		// entirely on exactly the hives that need it: a corrected
		// installation_id saved to hive.yaml kept minting tokens for the old
		// installation until the pod restarted. Startup (resolveAppKeyFile
		// above), the heartbeat rebuild, and the dashboard's Set ID handler
		// (#2459) all already resolve here; this was the last raw reader.
		//
		// Comparing RESOLVED paths also catches a change the raw comparison
		// cannot see: a per-app-id key arriving on the PVC changes which key
		// this process should sign with while cfg.GitHub.KeyFile stays "".
		prevKeyFile := appKeys.Resolve(prevGitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), prevGitHub.AppID)
		nextKeyFile := appKeys.Resolve(b.cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), b.cfg.GitHub.AppID)
		if prevGitHub.AppID != b.cfg.GitHub.AppID ||
			prevGitHub.InstallationID != b.cfg.GitHub.InstallationID ||
			prevKeyFile != nextKeyFile ||
			prevGitHub.APIURL != b.cfg.GitHub.APIURL {
			if b.cfg.GitHub.HasUsableApp() && nextKeyFile != "" {
				newAppAuth, appErr := github.NewAppAuth(b.cfg.GitHub.AppID, b.cfg.GitHub.InstallationID, nextKeyFile, b.logger, b.cfg.GitHub.ResolvedAPIURL())
				if appErr != nil {
					b.logger.Error("github app auth rebuild after config reload failed", "error", appErr)
				} else {
					newClient := github.NewClientFromAppWithBotLogin(newAppAuth, b.cfg.Project.Org, b.cfg.Project.Repos, b.logger, b.cfg.GitHub.BotLogin())
					if len(b.cfg.Governor.Labels.Exempt) > 0 {
						newClient.SetExemptLabels(b.cfg.Governor.Labels.Exempt)
						newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(b.cfg.Governor.Labels.AutoMerge))
					}
					newClient.SetIssueFilter(b.cfg.Project.IssueFilter)
					installReviewBots(newClient, b.cfg, b.logger)
					newClient.SetRepoPausedFunc(b.cfg.IsRepoPaused)        // #6203: a client rebuild must not un-pause repos
					newClient.SetAgentRepoScopeFunc(b.cfg.AgentServesRepo) // #6204: a client rebuild must not un-scope agents
					installReviewRelaySettings(newClient, b.cfg, b.logger)
					syncAutoMergePolicyToGitHubClient(b.cfg, newClient)
					b.ghClient = newClient
					b.installMutationBoundary(b.ghClient)
					b.appAuth = newAppAuth
					b.agentMgr.SetAppAuth(newAppAuth)
					// Immediate per-agent token delivery — see #4072.
					deps.refreshAgentTokens(b.ctx, b.agentMgr)
					b.agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: newAppAuth})
					b.agentMgr.SetSandboxPRClient(newClient)
					b.dashSrv.UpdateGitHubClient(newClient, newAppAuth)
					b.logger.Info("github app auth rebuilt after config reload",
						"app_id", b.cfg.GitHub.AppID,
						"installation_id", b.cfg.GitHub.InstallationID,
						"key_file", nextKeyFile,
					)
				}
			}
		}

		b.refreshDashboard()
	}, b.logger)
	b.dashSrv.SetSkipReloadFunc(b.configWatcher.SkipNext)
	deps.startConfigWatcher(b.ctx, b.configWatcher)

	// Persist operator pause/resume into the on-disk config so it survives
	// restarts and upgrades. Without this, a pod restart rebuilt every agent
	// un-paused, silently undoing an operator's pause on the next upgrade.
	// Concurrent pauses (e.g. an ACMM pack pausing several agents in a loop,
	// or login-detector firing while the operator pauses) each did an
	// unsynchronized cfg.Agents map write + cfg.Save(). The saves clobbered
	// each other (last writer wins), so only some pauses reached the PVC and
	// the rest were silently lost on the next restart. Serialize the
	// read-modify-save under a dedicated mutex so every pause transition is
	// durably persisted.
	b.agentMgr.SetPersistPauseCallback(func(name string, paused bool) {
		b.configWatcher.SkipNext() // don't let our own write trigger a reload
		changed, err := b.cfg.SetAgentPausedAndSave(name, paused)
		if err != nil {
			b.logger.Warn("failed to persist agent pause state", "agent", name, "paused", paused, "error", err)
		}
		_ = changed
	})

	// Persist the fully-expanded prompt text of every kick so owners can review
	// what their agents were actually told, over a day/week window, in the
	// per-agent "Prompts" tab. Redaction and truncation happen inside the
	// store, before anything is written to the PVC.
	b.agentMgr.SetRecordPromptCallback(b.dashSrv.RecordPrompt)

	// Feed agent lifecycle events (start, stop, launch FAILURE, backend/model
	// change) into the durable audit store behind the dashboard's Audit Log.
	// Injected as an interface because pkg/dashboard already imports pkg/agent,
	// so the manager cannot reach the audit store directly without an import
	// cycle. Motivating case: an agent configured with a backend its hive image
	// did not support failed at every launch for a day, visible only as a WARN
	// line inside the pod.
	b.agentMgr.SetAuditSink(b.dashSrv.AgentAuditSink())
	configureEscalationDispatcher(b.ctx, b.cfg, b.notifier, b.dashSrv.AgentAuditSink(), b.logger)

	// Compile the operator's state-triggered hooks (RFC #4001). Every sink the
	// vetted actions act through exists by this point: the notifier, the agent
	// manager's AUDITED pause, the lifecycle timeline, and the same audit store
	// the dashboard writes. The approvals sink stays nil until #4000's queue
	// lands — an enqueue-approval hook then reports a wiring failure per firing
	// rather than silently dropping the request.
	//
	// Fail-closed: an invalid hooks list logs and leaves the previous set
	// armed; it never crashes the process or silently disarms working hooks.
	buildHookDispatcher(b.cfg, hookSinks{
		Notifier: b.notifier,
		AgentMgr: b.agentMgr,
		Timeline: b.dashSrv.LifecycleTimeline(),
		Audit:    b.dashSrv.AgentAuditSink(),
		// #4000 ↔ #4001 seam: see the reload site above.
		Approvals: newHookApprovalAdapter(b.approvalInbox, toolapprove.ACMMLevelOf(b.cfg)),
	}, b.logger)

	// Emit the governor_mode_change transition post-commit. Installed once:
	// the observer reads the dispatcher through hookDispatcher() on each
	// firing, so a later config reload that arms or disarms hooks is picked up
	// without re-registering.
	installGovernorModeChangeEmitter(b.gov)
	installAgentPauseEmitter(b.agentMgr)

	// Register custom GHE hostnames with the proxy allowlist so mode
	// enforcement applies to GitHub Enterprise API and web requests.
	for _, rawURL := range []string{b.cfg.GitHub.ResolvedAPIURL(), b.cfg.GitHub.ResolvedBaseURL()} {
		if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
			deps.registerGitHubHost(parsed.Host)
		}
	}

	dashboard.SetBackendAuthProvider(b.agentMgr.BackendAuthAvailable)

	// Per-agent probe supersedes the backend-level one: under the per-agent-UID
	// layout each agent has its own HOME, so the shared credential path is empty
	// even for authenticated agents (see pkg/agent/authprobe.go).
	dashboard.SetAgentAuthProvider(b.agentMgr.AgentAuthAvailable)

	// Release-line drift surface (#6960): report how far the hosted edge line
	// (v5) has fallen behind the stable default branch (v4). Reuses the hub's
	// commit-behind compare/cache path; renders "unknown" (never a healthy
	// zero) until the SHA poller resolves both tips.
	dashboard.SetReleaseLineLagProvider(func() *dashboard.FrontendReleaseLineLag {
		lag := hub.ReleaseLineLagStatus(b.logger)
		return &dashboard.FrontendReleaseLineLag{
			EdgeBranch:   lag.EdgeBranch,
			StableBranch: lag.StableBranch,
			EdgeSHA:      lag.EdgeSHA,
			StableSHA:    lag.StableSHA,
			BehindBy:     lag.BehindBy,
			Known:        lag.Known,
			Threshold:    lag.Threshold,
			Exceeded:     lag.Exceeded,
		}
	})
}

// bootProxy installs the canary scanner, then builds and starts the egress
// proxy and inference translator with their per-agent route callbacks.
func (b *boot) bootProxy() { b.bootProxyWith(defaultBootProxyDeps()) }

// bootProxyWith is bootProxy with its long-lived effects injected; see
// bootProxyDeps.
func (b *boot) bootProxyWith(deps bootProxyDeps) {
	var err error

	canaryLeakHandler := func(leak ioscan.CanaryLeak) {
		detail := fmt.Sprintf("rule=%s, agent=%s, source=%s", ioscan.CanaryLeakRule, leak.Agent, leak.Source)
		b.dashSrv.AuditLog(leak.Agent, "ioscan_canary_leak", detail, leak.Agent)
		if store, ok := b.beadStores[leak.Agent]; ok && store != nil {
			if b, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
				_ = store.SetMetadata(b.ID, "rule", ioscan.CanaryLeakRule)
				_ = store.SetMetadata(b.ID, "source", leak.Source)
			}
		}
	}
	if b.ghClient != nil {
		b.ghClient.SetCanaryScanner(b.cfg.Ioscan.IsEnabled() && b.cfg.Ioscan.CanariesEnabled(), b.cfg.Ioscan.FailClosedAtLevel(b.cfg.ACMMLevelOrZero()), ioscan.DefaultCanaries, canaryLeakHandler)
	}

	b.githubProxy, err = deps.newGitHubProxy(b.logger, b.cfg.Project.Org, b.cfg.Project.Repos)
	if err != nil {
		b.logger.Error("failed to create github proxy", "error", err)
	} else {
		b.githubProxy.SetCanaryScanner(b.cfg.Ioscan.IsEnabled() && b.cfg.Ioscan.CanariesEnabled(), b.cfg.Ioscan.FailClosedAtLevel(b.cfg.ACMMLevelOrZero()), ioscan.DefaultCanaries, canaryLeakHandler)
		// Per-repo pause (#6203). This is the deterministic refusal the feature
		// rests on: whatever an agent believes, a write to a paused repo is
		// answered with a 403 here. The predicate reads live config, so pausing
		// a repo in the dashboard takes effect on the next request.
		b.githubProxy.SetRepoPausedFunc(b.cfg.IsRepoPaused)
		// Per-repo custom agents (#6204). This is the deterministic refusal the
		// feature rests on: whatever an agent believes, a write to a repo it is
		// not scoped to is answered with a 403 here. The predicate reads live
		// config, so re-scoping an agent in the dashboard takes effect on the
		// next request.
		b.githubProxy.SetAgentRepoScopeFunc(b.cfg.AgentServesRepo)
		// #1861: the proxy resolves an identified agent to its hub-held scoped
		// token via the package-level registry WriteAgentToken feeds (NOT via
		// the appAuth instance, which is replaced on key rotation — a closure
		// over it would strand the proxy on the stale instance). Wired
		// unconditionally: with HIVE_PROXY_INJECT_GH_AUTH unset (the default)
		// the proxy never consults the source and the registry stays empty.
		b.githubProxy.SetAgentTokenSource(github.AgentProxyToken)
		dashboard.SetProxyViolationsProvider(b.githubProxy.Violations)
		// Lets the dashboard narrow the LiteLLM model dropdown to the set the
		// configured key is entitled to, learned by the proxy from a key-info
		// probe or a "team not allowed" 403.
		dashboard.SetEntitledModelsProvider(b.githubProxy.EntitledModels)
		// Surface a stale/invalid inference gateway key (repeated 401s on every
		// inference call) as a hive health signal: the proxy latches the failure
		// after several consecutive rejections and clears it on the next success,
		// and the heartbeat builder reports it to the hub (both as an immediate
		// advisory-staleness cause and as a dedicated inference-auth alert).
		dashboard.SetInferenceAuthProvider(b.githubProxy.InferenceAuthError)
		// #4294: the provider spending-limit signal, read by the eval cycle to
		// raise an advisory and stop kicking agents at a gateway that is
		// refusing on a money limit.
		dashboard.SetInferenceBudgetProvider(b.githubProxy.InferenceBudgetExceeded)

		// Wire the inference token sink so the translator records per-agent
		// usage (from the gateway's OpenAI usage block) into the same metrics
		// dir the token collector scans. Without this, bare-mode inference
		// agents (litellm/vllm/llm-d) never write a scannable session file and
		// their consumption reads as zero.
		b.githubProxy.SetTokenSink(tokens.NewInferenceSink(b.cfg.Data.MetricsDir, b.logger))
		// Live Linear credential for agent requests — see
		// spokeWire.linearCredentialResolver and proxy.injectLinearCredential.
		b.githubProxy.SetLinearCredentialResolver(b.linearCredentialResolver)

		// With the sink active, the proxy also MITMs the Copilot completion host
		// (api.githubcopilot.com) to record Copilot token usage live per
		// response — so Copilot cost shows up while an agent runs instead of only
		// tallying at session shutdown. Tell the collector to defer Copilot token
		// accrual to the sink ONLY for sessions active from NOW on (the moment
		// live capture starts). Sessions that ended earlier were never sniffed by
		// the proxy, so the scanner keeps counting their shutdown tokens —
		// otherwise all pre-existing Copilot spend would vanish.
		b.tokenCollector.SetCopilotLiveCapture(time.Now().UnixMilli())

		vllmEndpoints := parseEndpointList(os.Getenv("HIVE_VLLM_ENDPOINT"))
		llmdEndpoints := parseEndpointList(envOrDefault("HIVE_LLMD_ENDPOINT", "http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000"))
		inferenceEndpoints := map[string][]string{
			"vllm":  vllmEndpoints,
			"llm-d": llmdEndpoints,
		}
		// litellm has no in-cluster default: register it only when an
		// endpoint is configured (yaml or HIVE_LITELLM_ENDPOINT), so an
		// unconfigured backend doesn't show up in model discovery. A URL
		// saved later from the governor LiteLLM tab is registered at
		// runtime via dashSrv.UpdateInferenceEndpoint.
		if b.cfg.Governor.LiteLLM.LocalProxy {
			inferenceEndpoints["litellm"] = []string{litellmLocalProxyURL()}
		} else if litellmEndpoint := b.cfg.Governor.LiteLLM.ResolveEndpoint(); litellmEndpoint != "" {
			inferenceEndpoints["litellm"] = parseEndpointList(litellmEndpoint)
		}
		// Register every explicitly-configured named gateway's endpoint by
		// gateway NAME so the Model Gateways tab's per-gateway model discovery
		// and per-gateway routing resolve on boot (the legacy "litellm" block
		// is already registered above; ResolvedGateways only synthesizes it
		// when no explicit gateways are set, so this loop never double-adds it).
		for _, gw := range b.cfg.Governor.Gateways {
			if ep := strings.TrimSpace(gw.Endpoint); ep != "" {
				inferenceEndpoints[gw.Name] = parseEndpointList(ep)
			}
		}
		b.dashSrv.SetInferenceEndpoints(inferenceEndpoints)
		// The gateway-name predicate (SetGatewayBackendChecker) is wired right
		// after the manager is constructed — see the comment there (#3961): it
		// must be live before the persisted-state replay re-applies saved
		// backend overrides, which happens well before this point.
		deps.installInferenceCallbacks(b.agentMgr,
			func(agentName, backend, model string) {
				// Named model gateway (OpenRouter, a second LiteLLM, etc.): resolve
				// endpoint/key/model from the gateway and route through it. Built-in
				// backend names (litellm/vllm/llm-d) are handled below; a gateway
				// literally named "litellm" resolves here to the same legacy block
				// via ResolvedGateways, so behavior is identical.
				if gw := b.cfg.Governor.ResolveGateway(backend); gw != nil && !config.IsInferenceBackend(backend) {
					endpoint := gw.Endpoint
					if endpoint == "" {
						b.logger.Warn("gateway backend selected but no endpoint configured",
							"agent", agentName, "gateway", backend, "model", model)
						return
					}
					if model == "" {
						model = gw.DefaultModel
					}
					// watsonx authenticates the OpenAI-compatible model gateway
					// with a short-lived IAM bearer minted from the IBM Cloud API
					// key (NOT the raw key), and scopes billing/limits by a
					// project id sent as X-IBM-Project-ID. Mint (cached) and set
					// both here; every other kind sends the resolved key verbatim.
					apiKey, extraHeaders := resolveGatewayAuth(gw, agentName, backend, b.logger)
					deps.setInferenceRoute(b.githubProxy, agentName, &proxy.InferenceRoute{
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						APIKey:       apiKey,
						CABundle:     gw.CABundle,
						ExtraHeaders: extraHeaders,
					})
					return
				}
				if backend == "litellm" {
					// Resolve endpoint/key at call time so a URL saved from
					// the governor LiteLLM tab (or a rotated key) takes
					// effect without a hive restart. cfg is the live config
					// pointer — the config watcher swaps its contents in
					// place on reload.
					lc := b.cfg.Governor.LiteLLM
					// Endpoint/model resolution lives in a pure function so the
					// decision tree (local proxy / legacy block / explicit-gateway
					// fallback / no route at all) is unit-testable — it is not
					// reachable from a test while inline in main(). See #5460.
					endpoint, resolvedModel, ok := resolveLiteLLMInferenceRoute(b.cfg, backend, model)
					if !ok {
						b.logger.Warn("litellm backend selected but no endpoint configured",
							"agent", agentName, "model", model)
						return
					}
					model = resolvedModel
					// Key source must MATCH the entitlement/probe path (gateways.go,
					// cost.go, openrouter.go), which resolve the key from the gateway
					// via ResolveGateway(backend).ResolveAPIKey(). When an EXPLICIT
					// `gateways:` block names this backend, that gateway carries its
					// own api_key_file (e.g. the key saved from the Model Gateways
					// tab). Reading the legacy Governor.LiteLLM key file here instead
					// would send a DIFFERENT (often stale) key than entitlement
					// validated, causing inference 401s after a key rotation done via
					// the Gateways tab. Resolve from the same gateway so inference and
					// entitlement always agree on one key source.
					//
					// Only explicit gateways override: ResolvedGateways synthesizes an
					// implicit "litellm" gateway from the legacy block when no
					// `gateways:` are set, but that synthetic gateway lacks the
					// multi-location file fallback of LiteLLMConfig.ResolveAPIKey
					// (k8s Secret mount + PVC copy). For no-gateway hives we therefore
					// keep the legacy resolver to preserve today's behavior.
					apiKey := b.cfg.Governor.ResolveLiteLLMInferenceKey(backend)
					caBundle := lc.CABundle
					if len(b.cfg.Governor.Gateways) > 0 {
						if gw := b.cfg.Governor.ResolveGateway(backend); gw != nil {
							caBundle = gw.CABundle
						}
					}
					deps.setInferenceRoute(b.githubProxy, agentName, &proxy.InferenceRoute{
						Backend:  backend,
						Endpoint: endpoint,
						Model:    model,
						APIKey:   apiKey,
						CABundle: caBundle,
					})
					return
				}
				if backend == config.GatewayKindWatsonx {
					// Built-in "watsonx" backend: the operator set
					// `backend: watsonx` without a gateway literally NAMED
					// watsonx (a named one is handled by the gateway branch
					// above). Resolve the watsonx gateway by KIND so the
					// endpoint, IBM Cloud key, project id and region all come
					// from the existing `gateways:` plumbing rather than being
					// re-derived here.
					gw := resolveWatsonxGateway(b.cfg)
					if gw == nil {
						b.logger.Warn("watsonx backend selected but no watsonx gateway is configured; add one under the Model Gateways tab",
							"agent", agentName, "model", model)
						return
					}
					// Region-only gateways are legal (the guided form can save a
					// region without an endpoint), so fall back to the shared
					// region template — the same helper the dashboard preset uses.
					endpoint := strings.TrimSpace(gw.Endpoint)
					if endpoint == "" {
						endpoint = watsonx.EndpointForRegion(gw.Region)
					}
					if model == "" {
						model = gw.DefaultModel
					}
					apiKey, extraHeaders := resolveGatewayAuth(gw, agentName, backend, b.logger)
					deps.setInferenceRoute(b.githubProxy, agentName, &proxy.InferenceRoute{
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						APIKey:       apiKey,
						CABundle:     gw.CABundle,
						ExtraHeaders: extraHeaders,
					})
					return
				}
				endpoints := vllmEndpoints
				if backend == "llm-d" {
					endpoints = llmdEndpoints
				}
				// vllm/llm-d endpoints are unauthenticated with a public
				// or in-cluster CA — no bearer key or custom CA bundle.
				if len(endpoints) == 0 {
					b.logger.Warn("inference backend selected but no endpoint configured",
						"agent", agentName, "model", model, "backend", backend)
					deps.clearInferenceRoute(b.githubProxy, agentName)
					return
				}
				endpoint := proxy.FindEndpointForModel(endpoints, model, "", "")
				if endpoint == "" {
					b.logger.Warn("no endpoint serves model, using first endpoint",
						"agent", agentName, "model", model, "backend", backend)
					endpoint = endpoints[0]
				}
				deps.setInferenceRoute(b.githubProxy, agentName, &proxy.InferenceRoute{
					Backend:  backend,
					Endpoint: endpoint,
					Model:    model,
				})
			},
			func(agentName string) {
				deps.clearInferenceRoute(b.githubProxy, agentName)
			},
		)

		deps.startProxy(b.githubProxy, b.logger)
		if b.cfg.Governor.LiteLLM.LocalProxy {
			deps.startLocalLiteLLM(b.ctx, b.logger)
		}
		b.logger.Info("github proxy started", "addr", b.githubProxy.ListenAddr())
	}
}

// bootLaunch starts the dashboard listener and Discord bot, writes the
// hive_restart audit marker, marks the pod Ready, and launches the
// persistent agents in the background.
func (b *boot) bootLaunch() { b.bootLaunchWith(defaultBootLaunchDeps()) }

func dashboardChatAllowedUsers(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	allowed := make([]string, 0, len(cfg.Dashboard.AuthorizedUsers))
	for _, entry := range cfg.Dashboard.AuthorizedUsers {
		user := strings.TrimSpace(entry)
		if user == "" {
			continue
		}
		if head, _, ok := strings.Cut(user, ":"); ok {
			user = strings.TrimSpace(head)
		}
		if user != "" {
			allowed = append(allowed, user)
		}
	}
	return allowed
}

func dashboardChatDrain(bot *dashchat.Bot, since uint64) []dashboard.ChatOutbound {
	if bot == nil {
		return nil
	}
	msgs := bot.Drain(since)
	out := make([]dashboard.ChatOutbound, 0, len(msgs))
	for _, msg := range msgs {
		out = append(out, dashboard.ChatOutbound{
			Seq:      msg.Seq,
			Text:     msg.Text,
			Role:     msg.Role,
			AuthorID: msg.AuthorID,
		})
	}
	return out
}

// bootLaunchWith is bootLaunch with its goroutines, Discord bot, stagger
// wait, and agent starts injected; see bootLaunchDeps.
func (b *boot) bootLaunchWith(deps bootLaunchDeps) {
	var agentNameList []string
	for name := range b.cfg.EnabledAgents() {
		agentNameList = append(agentNameList, name)
	}
	if deps.startDashChat != nil {
		bot, err := deps.startDashChat(b.ctx, dashchat.Config{
			DashboardURL:   fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
			AllowedUsers:   dashboardChatAllowedUsers(b.cfg),
		}, agentNameList, b.logger)
		if err != nil {
			b.logger.Warn("dashboard chat failed to start", "error", err)
		} else {
			b.dashChat = bot
			b.logger.Info("dashboard chat started")
		}
	}

	deps.spawn("dashboard-serve", func() {
		if err := deps.serve(b.dashSrv); err != nil {
			b.logger.Error("dashboard server failed", "error", err)
		}
	})

	if b.cfg.Notifications.Discord != nil && b.cfg.Notifications.Discord.BotToken != "" && b.cfg.Notifications.Discord.ChannelID != "" {
		err := deps.startDiscordBot(b.ctx, discord.Config{
			Token:          b.cfg.Notifications.Discord.BotToken,
			ChannelID:      b.cfg.Notifications.Discord.ChannelID,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
			AllowedUsers:   b.cfg.Notifications.Discord.AllowedUsers,
		}, agentNameList, b.logger)
		if err != nil {
			b.logger.Warn("discord bot failed to start", "error", err)
		} else {
			b.logger.Info("discord bot started", "channel", b.cfg.Notifications.Discord.ChannelID)
		}
	}

	if b.cfg.Notifications.Slack != nil && b.cfg.Notifications.Slack.Enabled {
		slackBot := slack.NewBot(slack.Config{
			AppToken:       b.cfg.Notifications.Slack.AppToken,
			BotToken:       b.cfg.Notifications.Slack.BotToken,
			ChannelID:      b.cfg.Notifications.Slack.ChannelID,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
			AllowedUsers:   b.cfg.Notifications.Slack.AllowedUsers,
		}, b.logger)
		slackBot.SetAgentNames(agentNameList)
		if err := slackBot.Start(b.ctx); err != nil {
			b.logger.Warn("slack bot failed to start", "error", err)
		} else {
			b.logger.Info("slack bot started", "channel", b.cfg.Notifications.Slack.ChannelID)
		}
	}

	if b.cfg.Notifications.Telegram != nil && b.cfg.Notifications.Telegram.Enabled {
		telegramBot := telegram.NewBot(telegram.Config{
			BotToken:       b.cfg.Notifications.Telegram.BotToken,
			ChatID:         b.cfg.Notifications.Telegram.ChatID,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
			AllowedUsers:   b.cfg.Notifications.Telegram.AllowedUsers,
		}, b.logger)
		telegramBot.SetAgentNames(agentNameList)
		if err := telegramBot.Start(b.ctx); err != nil {
			b.logger.Warn("telegram bot failed to start", "error", err)
		} else {
			b.logger.Info("telegram bot started", "chat", b.cfg.Notifications.Telegram.ChatID)
		}
	}

	if b.cfg.Notifications.Matrix != nil && b.cfg.Notifications.Matrix.Enabled {
		matrixBot := matrix.NewBot(matrix.Config{
			HomeserverURL:  b.cfg.Notifications.Matrix.HomeserverURL,
			AccessToken:    b.cfg.Notifications.Matrix.AccessToken,
			RoomID:         b.cfg.Notifications.Matrix.RoomID,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
			AllowedUsers:   b.cfg.Notifications.Matrix.AllowedUsers,
		}, b.logger)
		matrixBot.SetAgentNames(agentNameList)
		if err := matrixBot.Start(b.ctx); err != nil {
			b.logger.Warn("matrix bot failed to start", "error", err)
		} else {
			b.logger.Info("matrix bot started", "room", b.cfg.Notifications.Matrix.RoomID)
		}
	}

	if b.cfg.Notifications.MSTeams != nil && b.cfg.Notifications.MSTeams.Enabled {
		teamsBot := msteams.NewBot(msteams.Config{
			TenantID:       b.cfg.Notifications.MSTeams.TenantID,
			ClientID:       b.cfg.Notifications.MSTeams.ClientID,
			ClientSecret:   b.cfg.Notifications.MSTeams.ClientSecret,
			TeamID:         b.cfg.Notifications.MSTeams.TeamID,
			ChannelID:      b.cfg.Notifications.MSTeams.ChannelID,
			WebhookURL:     b.cfg.Notifications.MSTeams.WebhookURL,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
			AllowedUsers:   b.cfg.Notifications.MSTeams.AllowedUsers,
		}, b.logger)
		teamsBot.SetAgentNames(agentNameList)
		if err := teamsBot.Start(b.ctx); err != nil {
			b.logger.Warn("msteams bot failed to start", "error", err)
		} else {
			b.logger.Info("msteams bot started", "team", b.cfg.Notifications.MSTeams.TeamID, "channel", b.cfg.Notifications.MSTeams.ChannelID)
		}
	}

	b.onDemandFromPack = deps.onDemandFromPack()
	if len(b.onDemandFromPack) > 0 {
		b.logger.Info("on-demand agents from pack definitions", "agents", b.onDemandFromPack)
	}
	// One visible "hive restarted" marker per boot, so the audit log shows a
	// restart happened (and at what build) instead of only a burst of
	// per-agent agent_start rows. Include the persisted pauses being restored
	// so the operator can confirm pause state survived the restart — broken
	// down by trigger, and EXCLUDING agents that are startup-paused by design
	// (on-demand agents like brainstorm), whose inclusion turned "restoring 9
	// paused agent(s)" into a false systemic-incident signal on every upgrade
	// restart of a deliberately owner-quiesced fleet (#4041).
	b.dashSrv.AuditLog("system", "hive_restart",
		fmt.Sprintf("build=%s version=%s; %s", gitShort, reportedVersion(),
			pausedRestoreDetail(b.cfg.EnabledAgents(), b.onDemandFromPack, b.agentMgr.AllStatuses())), "")

	// Mark the dashboard READY as soon as the HTTP server can serve requests —
	// which is NOW: config is loaded, GitHub client/App auth are wired, the
	// dashboard deps are set, and the listener (dashboard-serve above) is up.
	// None of /api/*, /sso, /open, /api/livez or /api/health depend on the agent
	// fleet being up; the frontend already handles agents appearing over time.
	//
	// This MUST precede the staggered agent-launch loop below. That loop sleeps
	// ~15s per agent (× the whole fleet = several minutes) and previously ran
	// BEFORE MarkReady, so /api/livez returned 503 "starting" for the entire
	// launch window. The liveness probe (period 30s × failureThreshold 3 ≈ 90s)
	// then SIGKILLed the container (exit 137) before readiness was ever reached
	// on cold start, and rolling upgrades left the Service with no Ready endpoint
	// for minutes → 503s on /open and /sso. Flipping ready here makes the pod
	// Ready in seconds and moves the fleet spin-up entirely off the critical path.
	b.dashSrv.MarkReady()

	// Launch the persistent (non-on-demand) agents in the BACKGROUND so the
	// staggered start no longer gates pod readiness. The loop honors ctx: on
	// shutdown the ctx-aware stagger returns immediately instead of leaking a
	// goroutine parked in a bare time.Sleep.
	enabledAgents := b.cfg.EnabledAgents()
	startupAgents := startupLaunchNames(enabledAgents, b.onDemandFromPack)
	b.agentMgr.MarkStartupLaunchQueued(startupAgents)
	deps.spawn("agent-launch", func() {
		agentIndex := 0
		for _, name := range startupAgents {
			ac := enabledAgents[name]
			if agentIndex > 0 {
				b.logger.Info("staggering agent launch", "name", name, "delay_sec", int(agentLaunchStagger/time.Second))
				if !deps.waitStagger(b.ctx) {
					b.logger.Info("aborting staggered agent launch: shutting down")
					return
				}
			}
			// Bail before starting another agent if we are already shutting down,
			// so a SIGTERM during the launch window doesn't spawn fresh processes.
			if b.ctx.Err() != nil {
				b.logger.Info("aborting staggered agent launch: shutting down")
				return
			}
			b.logger.Info("audit: starting agent", "name", name, "trigger", "startup")
			if err := deps.startAgent(b.ctx, b.agentMgr, name); err != nil {
				b.logger.Warn("failed to start agent", "name", name, "error", err)
			} else {
				// Surface whether a persisted operator pause was honored on this
				// restart, so the audit log shows pause state survived (or didn't).
				detail := "trigger=startup"
				if ac.Paused {
					detail = "trigger=startup; restored paused (persisted)"
				}
				b.dashSrv.AuditLog("system", "agent_start", detail, name)
			}
			agentIndex++
		}
	})
}

// bootHeartbeat resolves the hub target and, when this spoke reports to a
// hub, starts the heartbeat and task-status pushes with every hub-delivered
// callback (restart, upgrade, App config, banner, project claim, ...).
func (b *boot) bootHeartbeat() { b.bootHeartbeatWith(defaultBootHeartbeatDeps()) }

// bootHeartbeatWith is bootHeartbeat with its long-lived effects injected;
// see bootHeartbeatDeps.
func (b *boot) bootHeartbeatWith(deps bootHeartbeatDeps) {
	// Start hub heartbeat push if configured (env var or config)
	hubTgt := resolveHubTarget(b.cfg.Hub, os.Getenv("HIVE_HUB_URL"), os.Getenv("HIVE_CLUSTER_ID"))
	b.hubURL = hubTgt.url
	b.cfg.Hub.Enabled, b.cfg.Hub.URL, b.cfg.Hub.ClusterID = hubTgt.enabled, hubTgt.url, hubTgt.clusterID
	if hubTgt.heartbeatsToHub() {
		// Publish the collect-independent identity BEFORE the loop starts, so
		// this spoke can report liveness even if its very first collects time
		// out. collect() below reaches api.github.com (owner-token validation,
		// and it shares the pass that enumerates issues/PRs for MTTR), which on
		// a hive with real repos routinely exceeds the collect budget right
		// after a restart. Without this, such a spoke sent NOTHING and read
		// OFFLINE on the hub while being perfectly healthy.
		deps.publishIdentity(
			b.cfg.HiveID,
			b.cfg.Project.Org,
			b.cfg.Project.PrimaryRepo,
			b.cfg.Project.Repos,
			b.reporterName,
			b.processStartedAt.UTC().Format(time.RFC3339),
			gitShort,
		)
		deps.startHeartbeat(b.ctx, b.hubURL, func() *spoke.HeartbeatPayload {
			if !b.cfg.Hub.Enabled {
				return nil
			}
			govState := b.gov.GetState()
			currentMode := strings.ToLower(string(govState.Mode))
			agents := b.heartbeatAgents(govState, currentMode, true)
			acmmLvl := b.heartbeatACMMLevel()
			prsMerged, prsRejected, cvesClosed, fleetStatsCollectedAt := b.heartbeatFleetStats()
			repoActivity, repoActivityCollectedAt, repoActivityWindowHours, repoActivityCountWindowHours := b.heartbeatRepoActivity()
			// Count agents with a method/model assigned for the hub's
			// user-journey stage detection. Always a non-nil pointer from a
			// spoke new enough to compute it, so the hub can distinguish
			// "genuinely zero agents configured" from "old spoke, unknown".
			agentsWithModel := b.agentMgr.CountAgentsWithModel()

			// --- Quadrant signals ------------------------------------------
			// All read from state this spoke already maintains on an existing
			// timer: ZERO new GitHub API calls, which matters because the whole
			// fleet shares one search quota. Every one stays nil unless its
			// source has actually produced a measurement — the hub's scorer
			// reads nil as absent evidence and a zero as a genuine low score,
			// so emitting a zero for missing data would silently misinform
			// operators rather than merely lose precision.

			budgetSpend, budgetLimit, budgetIgnored, budgetWindowStartsAt, budgetWindowEndsAt := b.heartbeatBudgetWindow()
			budgetExhausted, slaViolations := heartbeatBudgetState(govState)

			// Hold comes from the cached actionable result rather than
			// govState.QueueHold: both carry the same number, but the cache is
			// a nilable pointer, so a spoke that has not yet completed (or
			// restored) a scan reports nil instead of an int zero that is
			// indistinguishable from "nothing is on hold".
			var holdTotal *int
			if act := b.lastActionable.Load(); act != nil {
				total := act.Hold.Total
				holdTotal = &total
			}

			// Planning is unavailable below ACMM L5, where AwaitingReview is
			// structurally zero rather than measured — report nil so the hub
			// does not read "no plans are blocked on a human" into a hive that
			// has no planning subsystem at all.
			//
			// architectPaused is passed false rather than resolved from agent
			// statuses: it feeds only FrontendPlanning.ArchitectPaused, which
			// this heartbeat does not send, and the resolver is unexported to
			// pkg/dashboard. Passing false cannot perturb AwaitingReview.
			var awaitingReview *int
			if planning := dashboard.BuildPlanning(b.beadStores, false, acmmLvl); planning.Available {
				n := planning.AwaitingReview
				awaitingReview = &n
			}

			// Contributor-relay tasks over the trailing 7d, summed from the
			// spoke's own 168 hourly buckets. nil until the store exists; a
			// zero from an existing store is a real "no contributor finished
			// anything" reading.
			var tasksCompleted7d *int
			if n, ok := b.dashSrv.TasksCompleted7d(); ok {
				tasksCompleted7d = &n
			}

			providerLimitReason, providerLimitRebuffs, providerLimitHiveWide, providerLimitAgents := spoke.ProviderLimitHeartbeatFields(agents, dashboard.InferenceBudgetExceeded)
			lastWriteKickAt, kickDisposition, kickSkipReason, notWritableQueued :=
				outputFreshnessHeartbeatFields(acmmLvl, govState, agents)
			ghAppTokenStatus, ghAppTokenLastMintAt, ghAppTokenError :=
				githubAppTokenHeartbeatFields(b.cfg, b.dashSrv.GetGitHubAppPermIssue())
			ghAppErrorClass, ghAppHTTPStatus := githubAppStructuredFailure(
				b.dashSrv.GetGitHubAppState(),
				firstNonEmpty(b.dashSrv.GetGitHubAppPermIssue(), ghAppTokenError),
			)

			// Remediation-hint detectors (#5577). All three read state the
			// spoke already maintains — no new GitHub calls, no new file
			// scans on the beat path. AgentErrorStreaks is nil until the
			// token collector's first bob-recording scan completes ("not
			// measured", hub carries forward); the other two are always live
			// measurements and send [] to clear a stale carry-forward.
			agentErrorStreaks := b.tokenCollector.AgentErrorStreaks()
			consentWedged := b.agentMgr.ConsentWedgedAgents()
			noCadenceAgents := b.gov.NoCadenceAgents()

			return &spoke.HeartbeatPayload{
				AgentsWithModel:      &agentsWithModel,
				BudgetCurrentSpend:   budgetSpend,
				BudgetLimit:          budgetLimit,
				BudgetWindowStartsAt: budgetWindowStartsAt,
				BudgetWindowEndsAt:   budgetWindowEndsAt,
				BudgetExhausted:      budgetExhausted,
				BudgetIgnored:        budgetIgnored,
				HoldTotal:            holdTotal,
				AwaitingReview:       awaitingReview,
				SLAViolations:        slaViolations,
				TasksCompleted7d:     tasksCompleted7d,
				AgentErrorStreaks:    agentErrorStreaks,
				ConsentWedged:        consentWedged,
				NoCadenceAgents:      noCadenceAgents,
				// Read-back for hub-funded gateways: the hub clears its pending
				// record only when it sees the gateway named here, so a lost
				// delivery is re-offered rather than dropped. Names only — the
				// key never leaves the spoke.
				GatewayNames: b.dashSrv.ConfiguredGatewayNames(),
				// Hash only, never the raw token: lets the hub verify this
				// spoke's upgrade-proof credential without reading the
				// hive-secrets secret from a cluster it may not reach
				// (pull-only). Empty when no token is configured.
				DashboardTokenHash: func() string {
					if b.cfg.Dashboard.AuthToken == "" {
						return ""
					}
					return spoke.HashDashboardToken(b.cfg.Dashboard.AuthToken)
				}(),
				HiveID:            b.cfg.HiveID,
				Org:               b.cfg.Project.Org,
				AIAuthor:          b.cfg.Project.AIAuthor,
				AIAuthorEffective: b.cfg.EffectiveAIAuthor(),
				StartedAt:         b.processStartedAt.UTC().Format(time.RFC3339),
				// FD gauge (#3875): a socket leak reached 92,962 FDs and
				// self-DoSed spokes with nothing surfacing it. Report the count
				// and its rlimit every beat so the next leak is a climbing
				// number on the hub, not a manual /proc excavation.
				OpenFDs:     spoke.OpenFDCount(),
				FDSoftLimit: spoke.FDSoftLimit(),
				// Reporter names THIS process (the pod) so the hub can tell two
				// instances reporting as one hive apart — the pod name is the
				// hostname inside the container.
				Reporter: b.reporterName,
				// Advisory-staleness signal (mirrors StartedAt/uptime). Report the
				// last successful digest-post time only if the spoke has actually
				// posted one — a zero time is left as an empty string so the hub
				// reads it as UNKNOWN (not-advisory-mode / old spoke), never a
				// false stale alarm. The last post error rides alongside so a
				// working-App-but-failing-post hive can be flagged with its cause.
				AdvisoryLastPostedAt: func() string {
					postedAt, _, _ := b.dashSrv.AdvisoryState()
					if postedAt.IsZero() {
						return ""
					}
					return postedAt.UTC().Format(time.RFC3339)
				}(),
				AdvisoryError: func() string {
					_, _, errMsg := b.dashSrv.AdvisoryState()
					return errMsg
				}(),
				// Digest SHAPE: how many findings went out, and how many the
				// top-N cap withheld. The hub renders the pair so a capped
				// digest never reads as the complete picture.
				AdvisoryFindingCount: func() int {
					findings, _ := b.dashSrv.AdvisoryCounts()
					return findings
				}(),
				AdvisoryOverflowCount: func() int {
					_, overflow := b.dashSrv.AdvisoryCounts()
					return overflow
				}(),
				// Inference-backend auth-failure signal (repeated 401s from a
				// stale gateway key). Reported as its own field so the hub can
				// raise a dedicated inference-auth alert whose ROOT cause an
				// operator sees directly — distinct from the advisory-staleness
				// pill AdvisoryError also trips. Empty when inference auth is
				// healthy or the hive routes to no inference backend; self-clears
				// on the next successful inference call.
				InferenceAuthError: func() string {
					errMsg, _ := b.dashSrv.InferenceAuthState()
					return errMsg
				}(),
				ProviderLimitReason:     providerLimitReason,
				ProviderLimitRebuffs:    providerLimitRebuffs,
				ProviderLimitHiveWide:   providerLimitHiveWide,
				ProviderLimitAgents:     providerLimitAgents,
				LastWriteCapableKickAt:  lastWriteKickAt,
				LastKickDisposition:     kickDisposition,
				LastKickSkipReason:      kickSkipReason,
				NotWritableQueued:       notWritableQueued,
				RepoTargetMisconfigured: b.repoTargetMisconfigured(),
				RepoTargetIssue:         b.repoTargetIssueMessage(),
				Repos:                   b.cfg.Project.Repos,
				PrimaryRepo:             b.cfg.Project.PrimaryRepo,
				ACMMLevel:               acmmLvl,
				Agents:                  agents,
				Governor: spoke.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs, WorkSource: func() string {
					if t := b.cfg.Governor.WorkSource.Type; t != "" && t != "github" {
						return t
					}
					return ""
				}()},
				// Tokens carries the spoke's authoritative cumulative token
				// total (same store the dashboard token panel and governor
				// budget read). It flows to the hub's My Hives token column so
				// heartbeat-only hives (reached via heartbeat, not hub-kubectl)
				// display real consumption. Refreshed each heartbeat, so the
				// column is as fresh as the last heartbeat. Despite the
				// "24h"-suffixed field name this is a lifetime/window total,
				// consistent with what the spoke dashboard already shows.
				Tokens24h: func() int64 {
					if b.tokenCollector == nil {
						return 0
					}
					if summary := b.tokenCollector.Summary(); summary != nil {
						return summary.TotalTokens
					}
					return 0
				}(),
				Contributors: func() spoke.ContributorSummary {
					reg, active := b.dashSrv.ContributorSummary()
					return spoke.ContributorSummary{Registered: reg, Active: active}
				}(),
				Leaderboard: b.leaderboardForHeartbeat(),
				// Report who has a live dashboard session so the hub can accumulate
				// per-user "time in hive". Bare usernames only — never session
				// ids/tokens (ActiveSessionUsernames guarantees this).
				ActiveSessionUsers: b.dashSrv.ActiveSessionUsernames(),
				// The honest subset of the above: users whose browser reported
				// focused, recent-input presence (see dashboard/presence.go).
				// An idle open tab appears in ActiveSessionUsers but not here.
				EngagedSessionUsers: b.dashSrv.EngagedSessionUsernames(),
				// Per-user last audit-logged real action, so the hub can tell
				// users who DO things from users who merely stay logged in.
				UserLastActions: b.dashSrv.UserLastActions(),
				Owner:           b.ownerForHeartbeat(),
				// Report the API URL we are actually running against so the hub
				// can see whether a GitHub Enterprise API URL it delivered has
				// landed. Resolved (never empty) so the hub can distinguish
				// "public github.com" from "spoke too old to report this".
				GitHubAPIURL: b.cfg.GitHub.ResolvedAPIURL(),
				Health:       b.dashSrv.HealthSummary(),
				DashboardURL: b.dashboardURLForHeartbeat(),
				SnapshotURL:  b.cfg.Hub.SnapshotURL,
				HiveType:     b.cfg.Hub.HiveType,
				ClusterID:    b.cfg.Hub.ClusterID,
				IsPublic:     b.cfg.Hub.IsPublic,
				Version:      reportedVersion(),
				GitHash:      gitShort,
				GitBranch:    gitBranch,
				// The image ref the Deployment tracks, read in-cluster and
				// cached. The hub cannot see it for firewalled spokes, and it
				// is the only way to distinguish a hive pinned to an immutable
				// SHA tag (which can never receive a rolling upgrade) from one
				// riding <branch>-latest. Empty off-cluster — never guessed.
				ImageRef: spoke.SelfDeploymentImage(),
				// The GitHub instance this spoke actually runs against. Only
				// the spoke knows this for certain: a hive's GitHub can differ
				// from its cluster's default, so the hub cannot infer it.
				// Reported as a bare hostname via HostLabel(), which reads BOTH
				// base_url and api_url — a GHE placeholder with base_url:"" but
				// api_url: github.ibm.com must report github.ibm.com, not be
				// silently rendered as github.com in the spokes table.
				GitHubHost:               b.cfg.GitHub.HostLabel(),
				GitHubAppRequired:        b.dashSrv.IsGitHubAppRequired(),
				GitHubAppPermIssue:       b.dashSrv.GetGitHubAppPermIssue(),
				GitHubAppState:           b.dashSrv.GetGitHubAppState(),
				GitHubAppTokenStatus:     ghAppTokenStatus,
				GitHubAppTokenLastMintAt: ghAppTokenLastMintAt,
				GitHubAppTokenError:      ghAppTokenError,
				GitHubAppErrorClass:      ghAppErrorClass,
				GitHubAppHTTPStatus:      ghAppHTTPStatus,
				PendingGitHubAppInstall:  b.dashSrv.IsPendingGitHubAppInstall(),
				AutoUpgrade:              b.cfg.Hub.AutoUpgrade,
				ClusterHealth: func() *spoke.HeartbeatClusterHealthReport {
					if os.Getenv("HIVE_CLUSTER_ID") == "" {
						return nil
					}
					return spoke.CollectClusterHealth(b.logger)
				}(),
				PRsMerged90d:                 prsMerged,
				PRsRejected90d:               prsRejected,
				CVEsClosed:                   cvesClosed,
				FleetStatsCollectedAt:        fleetStatsCollectedAt,
				RepoActivity:                 repoActivity,
				RepoActivityCollectedAt:      repoActivityCollectedAt,
				RepoActivityWindowHours:      repoActivityWindowHours,
				RepoActivityCountWindowHours: repoActivityCountWindowHours,
				// Report WHICH App key we hold, never the key. The hub compares
				// this against its per-cluster key and pushes a correction only
				// on a mismatch, so a spoke already holding the right key costs
				// nothing and a spoke holding the wrong one self-heals.
				GitHubAppKeyFingerprint: appKeys.ReportedFingerprint(b.cfg.GitHub.KeyFile, b.cfg.GitHub.AppID),
				GitHubAppKeyPerHive:     appKeys.HasPerHiveKey(b.cfg.GitHub.KeyFile, b.cfg.GitHub.AppID),
				// Report the App this hive believes it authenticates as. The hub
				// pairs it with the fingerprint above to tell a per-hive key that
				// is WRONG for this App from one that is deliberately for another.
				GitHubAppID: b.cfg.GitHub.AppID,
				// Report the REST of the identity set too. app_id alone cannot
				// distinguish a correctly-delivered identity from a
				// half-applied one: a GHE app_id with an empty api_url looks
				// identical to the hub, and 404s on every token request. All
				// four together let the hub see the whole set.
				GitHubAppSlug:        b.cfg.GitHub.AppSlug,
				GitHubInstallationID: b.cfg.GitHub.InstallationID,
				GitHubBaseURL:        b.cfg.GitHub.BaseURL,
				// Report the fingerprint of every ADDITIONAL per-app-id key already
				// on the PVC, so the hub delivers the fleet's other App keys once
				// and then stops re-sending them.
				GitHubAppKeysHeld: appKeys.HeldFingerprints(),
				// Component reach counters (#3993, phase 2a of #3973): per
				// (component, running commit) span counts aggregated in-process,
				// exporter or not (D2) — the heartbeat is the only channel that
				// reaches every spoke, pull-only ones included (D1). nil until
				// the first span, which the hub reads as "no data", never as
				// zero reach. Capped at tracing.MaxReachComponents entries.
				ComponentReach: tracing.ReachSnapshot(),
			}

		}, heartbeatSendInterval, b.logger,
			spoke.RestartSpokeCallback(func() {
				if up := time.Since(b.processStartedAt); up < spokeRestartMinUptime {
					b.logger.Info("hub requested a spoke restart; ignoring — this process just started",
						"uptime", up.Round(time.Second))
					return
				}
				b.logger.Warn("hub requested a spoke restart — rolling this deployment",
					"reporter", b.reporterName)
				if err := spoke.RolloutRestartSelf(b.logger); err != nil {
					// Do NOT exit here: without deployment-patch RBAC an exit would
					// restart onto the same state every delivery and look like a
					// crash-loop. The error names the missing Role instead.
					b.logger.Error("spoke restart failed: could not patch own Deployment",
						"error", err,
						"hint", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)")
				}

			}),
			spoke.UpgradeCallback(func(targetSHA string) {
				// attemptCount carries the number of PREVIOUS failed attempts for this
				// (current_sha → target_sha) pair, read from the marker below.
				attemptCount := 0

				// Never self-upgrade to the commit we are already running. The hub
				// may instruct an upgrade to a short SHA that is a prefix of our
				// full gitShort (or vice-versa); treating that as "behind" caused a
				// crash-loop (patch → 403 → os.Exit → repeat) on hives sitting
				// exactly at HEAD. Prefix-compare so same-commit is a no-op.
				if sha1, sha2 := targetSHA, gitShort; sha1 != "" && sha2 != "" {
					n := len(sha1)
					if len(sha2) < n {
						n = len(sha2)
					}
					if strings.EqualFold(sha1[:n], sha2[:n]) {
						b.logger.Info("self-upgrade skipped: target is the running commit",
							"target", targetSHA, "current", gitShort)
						return
					}
				}

				// A previous process attempted an upgrade and we booted with the same
				// git hash, so the image did not actually change: the attempt FAILED.
				// Back off rather than retrying instantly (that was a crash-loop), but
				// do NOT latch forever — "image unchanged" is the signature of a failed
				// upgrade, not a reason to stop trying. The latch is keyed on
				// (current_sha → target_sha) and bounded to selfUpgradeMaxAttempts, so a
				// NEW target always gets a fresh budget and a transient failure (an RBAC
				// Role that showed up late, a registry blip) still converges.
				if markerData, err := os.ReadFile(upgradeMarkerPath); err == nil {
					m := parseUpgradeMarker(markerData)
					if m.CurrentSHA == gitShort && sameUpgradeTarget(m.TargetSHA, targetSHA) {
						if m.Attempts >= selfUpgradeMaxAttempts {
							// Terminal: report it LOUDLY and tell the hub, so the UI stops
							// claiming "Upgrading" forever and a human sees the real cause.
							b.logger.Error("self-upgrade FAILED: giving up after repeated attempts (image never changed)",
								"target", targetSHA,
								"current", gitShort,
								"attempts", m.Attempts,
								"max_attempts", selfUpgradeMaxAttempts,
								"last_error", m.LastError,
								"hint", "the spoke must be able to get/patch its own Deployment; check the hive-self-upgrade Role/RoleBinding in this namespace",
							)
							spoke.ReportUpgradeFailure(b.hubURL, b.cfg.HiveID, targetSHA, gitShort,
								upgradeFailureSummary(m.Attempts, m.LastError), b.logger)
							return
						}
						// Exponential backoff between attempts so a hard failure does not
						// spin every heartbeat while a recoverable one still retries.
						backoff := selfUpgradeBaseBackoff << (m.Attempts - 1)
						if backoff > selfUpgradeMaxBackoff {
							backoff = selfUpgradeMaxBackoff
						}
						if since := time.Since(m.RequestedAt); since < backoff {
							b.logger.Warn("self-upgrade retry deferred: backing off after a failed attempt",
								"target", targetSHA,
								"current", gitShort,
								"attempts", m.Attempts,
								"retry_in", (backoff - since).Round(time.Second),
								"last_error", m.LastError,
							)
							return
						}
						b.logger.Warn("self-upgrade retrying after a failed attempt (image unchanged)",
							"target", targetSHA,
							"current", gitShort,
							"attempt", m.Attempts+1,
							"max_attempts", selfUpgradeMaxAttempts,
							"last_error", m.LastError,
						)
						attemptCount = m.Attempts
					} else {
						// Different SHA or a different target — the old marker is stale.
						if err := os.Remove(upgradeMarkerPath); err != nil && !os.IsNotExist(err) {
							b.logger.Warn("failed to clear stale upgrade marker", "path", upgradeMarkerPath, "error", err)
						}
					}
				}

				// Minimum uptime before allowing self-upgrade to avoid restart loops.
				const minUptimeBeforeUpgrade = 5 * time.Minute
				uptime := time.Since(b.startTime)
				if uptime < minUptimeBeforeUpgrade {
					b.logger.Warn("self-upgrade deferred: minimum uptime not reached",
						"target", targetSHA,
						"current", gitShort,
						"uptime", uptime.Round(time.Second),
						"min_uptime", minUptimeBeforeUpgrade,
					)
					return
				}

				// Record the attempt BEFORE acting: if the process dies mid-upgrade the
				// next boot must still see an incremented count, otherwise a crash loop
				// would retry without ever exhausting the budget.
				writeUpgradeMarker(upgradeMarkerPath, upgradeMarker{
					TargetSHA:   targetSHA,
					CurrentSHA:  gitShort,
					RequestedAt: time.Now().UTC(),
					Attempts:    attemptCount + 1,
				}, b.logger)

				b.logger.Info("self-upgrade triggered: sending upgrading heartbeat then exiting",
					"current", gitShort,
					"latest", targetSHA,
					"uptime", uptime.Round(time.Second),
				)

				spoke.SendUpgradingHeartbeat(b.hubURL, func() *spoke.HeartbeatPayload {
					if !b.cfg.Hub.Enabled {
						return nil
					}
					statuses := b.agentMgr.AllStatuses()
					govState := b.gov.GetState()
					currentMode := strings.ToLower(string(govState.Mode))
					agents := make([]spoke.AgentSummary, 0, len(statuses))
					for name, proc := range statuses {
						mode := ""
						if ac, ok := b.cfg.Agents[name]; (ok && ac.OnDemand) || b.onDemandFromPack[name] {
							mode = "on_demand"
						}
						agents = append(agents, spoke.NewAgentSummary(name, string(proc.State), mode,
							spoke.AgentActivityFor(b.agentMgr, b.cfg, govState, currentMode, name, proc, b.onDemandFromPack)))
					}
					acmmLvl := 0
					if b.cfg.ACMMLevel != nil {
						acmmLvl = *b.cfg.ACMMLevel
					}
					providerLimitReason, providerLimitRebuffs, providerLimitHiveWide, providerLimitAgents := spoke.ProviderLimitHeartbeatFields(agents, dashboard.InferenceBudgetExceeded)
					lastWriteKickAt, kickDisposition, kickSkipReason, notWritableQueued :=
						outputFreshnessHeartbeatFields(acmmLvl, govState, agents)
					return &spoke.HeartbeatPayload{
						HiveID: b.cfg.HiveID,
						Org:    b.cfg.Project.Org,
						// Project identity rides even this minimal beat. The hub
						// rebuilds the registry entry from each payload VERBATIM
						// (no carry-forward for these fields), and this beat is
						// the LAST one the hub holds for the whole restart window
						// that follows — omitting repos/primary_repo here blanked
						// the entry (org set, primaryRepo "", repos []) until the
						// new process's first successful collect, breaking the
						// public-directory row (no repo link) and rendering the
						// hive name as "org/". Both values are plain config reads,
						// exactly as cheap as Org above.
						Repos:                   b.cfg.Project.Repos,
						PrimaryRepo:             b.cfg.Project.PrimaryRepo,
						ACMMLevel:               acmmLvl,
						Agents:                  agents,
						GitHash:                 gitShort,
						ClusterID:               b.cfg.Hub.ClusterID,
						HiveType:                b.cfg.Hub.HiveType,
						IsPublic:                b.cfg.Hub.IsPublic,
						Version:                 reportedVersion(),
						RepoTargetMisconfigured: b.repoTargetMisconfigured(),
						RepoTargetIssue:         b.repoTargetIssueMessage(),
						ProviderLimitReason:     providerLimitReason,
						ProviderLimitRebuffs:    providerLimitRebuffs,
						ProviderLimitHiveWide:   providerLimitHiveWide,
						ProviderLimitAgents:     providerLimitAgents,
						LastWriteCapableKickAt:  lastWriteKickAt,
						LastKickDisposition:     kickDisposition,
						LastKickSkipReason:      kickSkipReason,
						NotWritableQueued:       notWritableQueued,
						// Remediation-hint detectors (#5577): all three are
						// cheap in-memory reads, so even this minimal upgrading
						// beat carries them — the pod is about to restart, and
						// carrying the last real measurement across the roll keeps
						// a live wedge visible instead of blanking it.
						AgentErrorStreaks: b.tokenCollector.AgentErrorStreaks(),
						ConsentWedged:     b.agentMgr.ConsentWedgedAgents(),
						NoCadenceAgents:   b.gov.NoCadenceAgents(),
					}

				}, targetSHA, b.logger)

				// A plain rollout restart only advances a deployment tracking a
				// MUTABLE tag. On a SHA-pinned deployment it relaunches the very
				// same image, so the hive reports the old hash and the hub re-sends
				// this upgrade every heartbeat — a restart loop that never lands.
				// UpgradeSelfToSHA patches the image instead when we are pinned.
				needsRestart, err := spoke.UpgradeSelfToSHA(b.logger, targetSHA)
				if err != nil {
					b.logger.Warn("pinned-image upgrade failed, falling back to rolling restart",
						"target", targetSHA, "error", err)
					recordUpgradeError(upgradeMarkerPath, err, b.logger)
					needsRestart = true
				}
				if needsRestart {
					if err := spoke.RolloutRestartSelf(b.logger); err != nil {
						// This is the wedge. os.Exit here restarts the pod onto the
						// SAME image, so the upgrade silently never lands. It is an
						// ERROR, not a Warn, and the cause (typically a 403 because
						// the spoke lacks patch on its own Deployment) must be both
						// persisted for the next attempt and reported to the hub so
						// the UI stops showing a permanent "Upgrading".
						b.logger.Error("self-upgrade FAILED: could not patch own Deployment, restarting onto the same image",
							"target", targetSHA,
							"current", gitShort,
							"error", err,
							"hint", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)",
						)
						recordUpgradeError(upgradeMarkerPath, err, b.logger)
						spoke.ReportUpgradeFailure(b.hubURL, b.cfg.HiveID, targetSHA, gitShort, err.Error(), b.logger)
						// Exit NON-ZERO. Exiting 0 on a failed upgrade told Kubernetes
						// the process had completed successfully, so the restart looked
						// routine and nothing — not the pod's exit code, not an event,
						// not a probe — recorded that an upgrade had just failed. A
						// non-zero code makes the failure visible in the pod's
						// lastState.terminated and in `kubectl describe`.
						os.Exit(selfUpgradeFailureExitCode)
					}
				}
				// Rolling restart initiated — K8s will start a new pod and
				// send SIGTERM to this one once the replacement is Ready.
				// Block here so the process stays alive until terminated.
				b.logger.Info("waiting for SIGTERM after rolling restart")
				<-b.ctx.Done()

			}),
			spoke.GitHubAppConfigCallback(func(ghCfg *spoke.HeartbeatGitHubAppConfig) {
				b.logger.Info("received github app config via heartbeat",
					"app_id", ghCfg.AppID,
					"installation_id", ghCfg.InstallationID,
					"has_key", ghCfg.PrivateKey != "",
				)

				// WRITE-PATH GUARD. Compute the identity this push WOULD produce and
				// refuse the whole delivery if it is internally inconsistent.
				//
				// This is the guard the 2026-07-31 incident needed. That push carried
				// the GHE app_id and slug with no api_url; the adoption below applies
				// each field independently under an "empty means unchanged" contract,
				// so seven public-GitHub hives took the GHE App ID, kept api_url: "",
				// and every token request 404'd. Refusing the whole delivery leaves
				// those hives on their previous, working identity instead of a half of
				// two identities.
				//
				// Rejection is loud and repeats on every beat: the hub keeps pushing
				// until the spoke reports back, so a silent skip would be an invisible
				// permanent stall. There is no auto-repair here — the fix is on the
				// hub, in clusters.json.
				if prospective := prospectiveGitHubIdentity(b.cfg.GitHub, ghCfg); prospective != nil {
					if err := config.RejectIdentitySet(*prospective); err != nil {
						b.logger.Error("REFUSING hub github app config: the pushed identity set is inconsistent and would half-apply — nothing was changed",
							"error", err,
							"pushed_app_id", ghCfg.AppID,
							"pushed_app_slug", ghCfg.AppSlug,
							"current_app_id", b.cfg.GitHub.AppID,
							"current_api_url", b.cfg.GitHub.APIURL,
							"current_base_url", b.cfg.GitHub.BaseURL,
							"remedy", "correct github_app_id/github_app_slug/github_api_url/github_base_url for this cluster on the hub",
						)
						return
					}
				}

				// Write the delivered key to the path that NAMES the App it belongs
				// to, not to a generic filename.
				//
				// /data/gh-app-key.pem carries no evidence of which App signed it, so
				// a key delivered for one App silently becomes "the key" for whatever
				// app_id the config later claims. On 2026-07-31 that is exactly what
				// happened: all 33 heartbeat-only-cluster spokes had key_file pinned to the generic
				// path holding the GHE key, so correcting app_id to the public App
				// still produced 404 Integration not found — the right key was
				// already on disk at gh-app-key-3568013.pem and unreachable, because
				// an explicit key_file short-circuits resolveAppKeyFile before the
				// per-app-id lookup runs.
				//
				// Deriving the filename from the app_id makes that mismatch
				// unrepresentable: a key can only be found under the App it was
				// delivered for. Falls back to the generic path when the delivery
				// names no App, so a key is never dropped on the floor.
				keyPath := deliveredKeyPath(ghCfg.AppID)
				// keyChanged gates dropping the cached installation token below: a
				// token minted under the previous key is invalid the moment the key
				// is replaced, but a redelivery of the SAME key must not throw away a
				// perfectly good token every heartbeat.
				keyChanged := false
				if ghCfg.PrivateKey != "" {
					// Fingerprint before and after so the key rotation is auditable
					// from the spoke's own logs. Fingerprints only — the key itself
					// is never logged.
					beforeFP, _ := config.AppKeyFingerprintFromFile(keyPath)
					if err := os.WriteFile(keyPath, []byte(ghCfg.PrivateKey), appKeys.FileMode); err != nil {
						b.logger.Error("failed to write github app key from heartbeat", "error", err)
						return
					}
					// os.WriteFile does NOT re-apply the mode to a file that already
					// exists, so a key written by an older build (or restored from a
					// looser-moded source) would keep its old permissions forever.
					// Chmod unconditionally so every path converges on 0600.
					if err := os.Chmod(keyPath, appKeys.FileMode); err != nil {
						b.logger.Warn("could not tighten github app key permissions", "path", keyPath, "error", err)
					}
					afterFP, _ := config.AppKeyFingerprintFromFile(keyPath)
					keyChanged = afterFP != "" && afterFP != beforeFP
					b.logger.Info("github app private key written via heartbeat",
						"path", keyPath,
						"from_fingerprint", beforeFP,
						"to_fingerprint", afterFP,
						"key_changed", keyChanged,
					)
					if keyChanged {
						// Invalidate before the new client is built, so nothing can
						// read the dead token out of the shared on-disk cache in
						// between. Agents read that file directly.
						b.appAuth.DropCachedToken()
					}
				}

				// Persist the fleet's ADDITIONAL App keys — every OTHER App's key,
				// keyed by app_id — so this spoke can sign for the App it is actually
				// configured as even when that is NOT its cluster's default. This is
				// the both-keys fix: a github.com hive on a GHE cluster now receives
				// and stores the github.com key here, and resolveAppKeyFile selects it
				// by matching cfg.GitHub.AppID.
				//
				// Written to distinct /data/gh-app-key-<appid>.pem files, so they never
				// collide with the primary /data/gh-app-key.pem above. Writing one that
				// matches our OWN app_id must take effect immediately: flip keyChanged
				// so the client is rebuilt below, exactly as a primary-key change does.
				// applyDeliveredPerAppKey writes ONE (app_id, key) pair to its
				// per-app-id file and reports whether that changed the key we
				// ourselves sign with. Shared by the (now-inert) AdditionalKeys loop
				// and the targeted SecondaryKey delivery below so both write through
				// identical code — the alternative is two copies of an atomic 0600
				// write, one of which eventually loses a guard.
				applyDeliveredPerAppKey := func(kind string, appID int64, privateKey string) {
					if privateKey == "" || appID <= 0 {
						return
					}
					perAppPath := appKeys.PerAppIDKeyPath(appID)
					beforeFP, _ := config.AppKeyFingerprintFromFile(perAppPath)
					fp, err := appKeys.WritePerAppIDKey(appID, privateKey)
					if err != nil {
						b.logger.Error("failed to write "+kind+" github app key from heartbeat",
							"app_id", appID, "error", err)
						return
					}
					changed := fp != "" && fp != beforeFP
					b.logger.Info(kind+" github app private key written via heartbeat",
						"app_id", appID,
						"path", perAppPath,
						"from_fingerprint", beforeFP,
						"to_fingerprint", fp,
						"key_changed", changed,
					)
					// If this key is for the App we ourselves authenticate as, it is
					// now the key resolveAppKeyFile will pick — treat it like a
					// primary-key rotation so the client rebuild below uses it.
					if changed && appID == b.cfg.GitHub.AppID {
						keyChanged = true
						b.appAuth.DropCachedToken()
					}
				}
				for _, ak := range ghCfg.AdditionalKeys {
					applyDeliveredPerAppKey("additional", ak.AppID, ak.PrivateKey)
				}

				// The OPTIONAL SECOND App key (#4815), delivered targeted at this
				// hive alone rather than broadcast. It lands in the same
				// /data/gh-app-key-<appid>.pem namespace the spoke has always used,
				// so heldPerAppIDKeyFingerprints reports it back on the next beat
				// (which is what stops the hub re-pushing it) and the Forge App tab
				// renders it, both with no further change. nil for every hive with no
				// second App.
				if ghCfg.SecondaryKey != nil {
					applyDeliveredPerAppKey("secondary", ghCfg.SecondaryKey.AppID, ghCfg.SecondaryKey.PrivateKey)
				}

				// Adopt a hub-delivered app_id only when it names a REAL App. Zero
				// means "not speaking to this field"; the placeholder sentinel is
				// what a pre-provisioned hive already carries, so re-adopting it
				// would overwrite a good app_id with a non-App on any heartbeat
				// that echoed the original seed back.
				if ghCfg.AppID != 0 && ghCfg.AppID != config.PlaceholderAppID {
					b.cfg.GitHub.AppID = ghCfg.AppID
				}
				// A zero installation_id means "the hub is not speaking to this
				// field", not "clear it". The cluster-wide key reconcile repairs the
				// KEY on hives whose installation_id is already correct (and which
				// the hub does not track); assigning zero here would blank a working
				// value and turn a key-only fault into a total auth outage.
				if next, cleared := nextInstallationID(b.cfg.GitHub.InstallationID, ghCfg); cleared {
					// The banner and the hub must flip to not-installed NOW, not
					// when the cached token dies an hour from now. Same config-truth
					// rule as startup.
					b.dashSrv.SetGitHubAppRequired(true)
					b.dashSrv.SetGitHubAppState(github.AppStateNotInstalled.String())
					b.logger.Info("clearing github app installation_id on operator request",
						"was", b.cfg.GitHub.InstallationID)
					b.cfg.GitHub.InstallationID = next
				} else {
					b.cfg.GitHub.InstallationID = next
				}
				// Deliberately NOT `cfg.GitHub.KeyFile = keyPath`.
				//
				// key_file is DERIVABLE from app_id (resolveAppKeyFile prefers
				// /data/gh-app-key-<app_id>.pem, the only key correct by
				// construction). Persisting the path turns a derived value into a
				// stored one that outlives the App it was derived for: once written,
				// it short-circuits resolveAppKeyFile on every later boot, so a
				// corrected app_id keeps signing with the previous App's key. That is
				// what left all 33 heartbeat-only-cluster spokes pinned to the GHE key.
				//
				// Leaving it empty lets derivation run every time, so the key always
				// tracks the App actually in effect. An operator-set key_file still
				// wins — that override is intentional, for a hive whose App this
				// build does not know (e.g. a hive on a third App ID with a key at a
				// bespoke path).
				// Same "empty means unchanged" contract as installation_id: adopting
				// an empty slug would blank a working install link.
				if ghCfg.AppSlug != "" && b.cfg.GitHub.AppSlug != ghCfg.AppSlug {
					b.logger.Info("adopting github app slug from hub",
						"was", b.cfg.GitHub.AppSlug, "now", ghCfg.AppSlug)
					b.cfg.GitHub.AppSlug = ghCfg.AppSlug
				}
				// Adopt the forge URLs from the SAME delivery as the App above.
				// prospectiveGitHubIdentity already validated all four fields
				// together, so reaching here means the complete set is coherent —
				// applying the App without its URLs would undo that check by leaving
				// the spoke pointed at the previous forge.
				//
				// Same "empty means unchanged" contract: empty is the correct steady
				// state for a public hive, so it can never be read as "blank this".
				if ghCfg.APIURL != "" && b.cfg.GitHub.APIURL != ghCfg.APIURL {
					b.logger.Info("adopting github api url from hub",
						"was", b.cfg.GitHub.APIURL, "now", ghCfg.APIURL)
					b.cfg.GitHub.APIURL = ghCfg.APIURL
				}
				if ghCfg.BaseURL != "" && b.cfg.GitHub.BaseURL != ghCfg.BaseURL {
					b.logger.Info("adopting github base url from hub",
						"was", b.cfg.GitHub.BaseURL, "now", ghCfg.BaseURL)
					b.cfg.GitHub.BaseURL = ghCfg.BaseURL
				}

				// Persist the adopted App IDENTITY to the PVC overlay, exactly as the
				// claimed-project-config callback persists what it adopts.
				//
				// Without this the adoption lived only in memory. The key files are
				// written to /data (durable), but app_id/app_slug/installation_id were
				// not, so on every pod restart the entrypoint re-merged the ConfigMap
				// seed and the spoke reverted to whatever App it was provisioned with —
				// silently undoing a completed repair and making the hub's push look
				// like it had never happened. That is how a GHE hive kept re-appearing
				// with the github.com app_id and an empty slug across restarts.
				//
				// Saved even when the App is not yet usable (installation_id still 0):
				// the corrected app_id and slug are precisely what the owner needs on
				// disk so the dashboard renders a working install link BEFORE they have
				// installed anything.
				if err := b.cfg.Save(); err != nil {
					b.logger.Error("failed to persist github app config from heartbeat", "error", err)
				}

				// Resolve the key file the same way startup does, so a hive whose
				// only correct key arrived as an ADDITIONAL per-app-id key (no
				// primary key_file configured — the exact heartbeat-only-cluster state) still finds
				// it: resolveAppKeyFile prefers /data/gh-app-key-<appid>.pem for the
				// app_id we now claim. An explicit key_file still wins outright.
				rebuildKeyFile := appKeys.Resolve(b.cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), b.cfg.GitHub.AppID)
				if b.cfg.GitHub.HasUsableApp() && rebuildKeyFile != "" {
					newAppAuth, err := github.NewAppAuth(b.cfg.GitHub.AppID, b.cfg.GitHub.InstallationID, rebuildKeyFile, b.logger, b.cfg.GitHub.ResolvedAPIURL())
					if err != nil {
						b.logger.Error("github app auth init via heartbeat failed", "error", err)
						return
					}
					// Deliberately NOT persisted. rebuildKeyFile is the RESOLVED
					// path, and writing a resolved value back into config is what
					// converts a derivation into a pin: the next boot reads it as an
					// explicit key_file, short-circuits resolveAppKeyFile, and keeps
					// using this App's key even after app_id changes. Re-resolving on
					// every use costs a stat and cannot go stale.
					// Hub-delivered creds can carry a wrong installation_id just as
					// easily as a hand-edited config; correct (and persist) it
					// before building a client that would 403 on every write.
					healGitHubAppInstallation(b.ctx, newAppAuth, b.cfg, b.logger)
					newClient := github.NewClientFromAppWithBotLogin(newAppAuth, b.cfg.Project.Org, b.cfg.Project.Repos, b.logger, b.cfg.GitHub.BotLogin())
					if len(b.cfg.Governor.Labels.Exempt) > 0 {
						newClient.SetExemptLabels(b.cfg.Governor.Labels.Exempt)
						newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(b.cfg.Governor.Labels.AutoMerge))
					}
					newClient.SetIssueFilter(b.cfg.Project.IssueFilter)
					installReviewBots(newClient, b.cfg, b.logger)
					newClient.SetRepoPausedFunc(b.cfg.IsRepoPaused)        // #6203: a client rebuild must not un-pause repos
					newClient.SetAgentRepoScopeFunc(b.cfg.AgentServesRepo) // #6204: a client rebuild must not un-scope agents
					syncAutoMergePolicyToGitHubClient(b.cfg, newClient)

					b.ghClient = newClient
					b.installMutationBoundary(b.ghClient)
					b.appAuth = newAppAuth
					b.agentMgr.SetAppAuth(newAppAuth)
					// Immediate per-agent token delivery: hosted spokes get their
					// App creds via this heartbeat path AFTER agents have already
					// launched (with empty 0-byte caches), so waiting for the next
					// 40-minute tick guarantees a window of gh 401s (#4072).
					go b.agentMgr.RefreshAgentTokens(b.ctx)
					b.dashSrv.UpdateGitHubClient(newClient, newAppAuth)
					b.dashSrv.SetGitHubAppRequired(false)
					b.dashSrv.ClearPendingGitHubAppInstall()
					b.logger.Info("github app configured via heartbeat delivery",
						"app_id", b.cfg.GitHub.AppID,
						"installation_id", b.cfg.GitHub.InstallationID,
					)
				}

			}),
			spoke.HubBannerCallback(func(banner *spoke.HubBanner) {
				if banner == nil {
					b.dashSrv.ClearHubBanner()
					return
				}
				b.dashSrv.SetHubBanner(banner.ID, banner.Message, banner.Color)

			}),
			spoke.UpgradePolicyCallback(func(p *spoke.HeartbeatUpgradePolicy) {
				// Descriptive only (#7262): the hub's upgrade posture for this
				// spoke, so the dashboard measures "behind" against the commit
				// the hub will actually roll us to and renders the hub's schedule.
				b.dashSrv.SetHubUpgradePolicy(p)
			}),
			spoke.VisibilityCallback(func(isPublic bool) {
				if b.cfg.Hub.IsPublic != isPublic {
					b.logger.Info("hub overrode visibility via heartbeat",
						"was", b.cfg.Hub.IsPublic, "now", isPublic)
					b.cfg.Hub.IsPublic = isPublic
				}

			}),
			spoke.SwitchBranchCallback(func(tag string) {
				// Branch switch delivered via heartbeat (the hub couldn't reach
				// this cluster over kubectl). Patch our OWN deployment image via
				// the in-cluster K8s API — the pod has no kubectl binary, but its
				// SA holds the hive-self-upgrade role (patch on deployment/hive).
				// K8s then rolls the pod onto the new tag.
				image := "ghcr.io/hivecommons/hive:" + tag
				if err := spoke.SwitchImageSelf(b.logger, image); err != nil {
					b.logger.Warn("branch switch via heartbeat failed", "tag", tag, "image", image, "error", err)
					return
				}
			}),
			spoke.AgentRestartResetCallback(func(name string) {
				if err := b.agentMgr.ResetRestartCount(name); err != nil {
					b.logger.Warn("agent restart reset from hub failed", "agent", name, "error", err)
					return
				}
				b.logger.Info("audit: agent restart counter reset from hub", "agent", name)
			}),
			spoke.AuthorizedUsersCallback(func(users []string, names map[string]string) {
				// The hub delivered its authoritative access list. Reconcile our
				// login allowlist so Manage Access grants take effect on this
				// heartbeat-only spoke without any kubectl push. The dashboard reads
				// cfg.Dashboard.AuthorizedUsers live on each login, so updating it in
				// place is enough. Only log when it actually changes to avoid noise.
				if !sameStringSlice(b.cfg.Dashboard.AuthorizedUsers, users) {
					b.logger.Info("authorized users updated from hub heartbeat",
						"was", len(b.cfg.Dashboard.AuthorizedUsers), "now", len(users))
					b.cfg.Dashboard.AuthorizedUsers = users
				}
				// AuthorizedUserNames is purely cosmetic (see its doc) — it never
				// gates sign-in, so it's fine to just take whatever the hub sent
				// (including nil, which means "no names known") without the
				// same-value guard above.
				b.cfg.Dashboard.AuthorizedUserNames = names

			}),
			spoke.ProjectConfigCallback(func(pc *spoke.HeartbeatProjectConfig) {
				// The hub assigned this (previously placeholder) hive a real project.
				// Reconcile our running project config so agents work the claimed
				// org/repos at the claimed maturity level. This is the ONLY delivery
				// channel on heartbeat-only clusters (the heartbeat-only cluster) — no kubectl push is
				// possible. The hub keeps sending this every beat until we report the
				// matching project back, so an idempotent no-op when already matched
				// is expected and cheap.
				if pc == nil {
					return
				}
				// A URL-only push (org empty, dashboard_url set) delivers the vanity
				// dashboard URL to an already-claimed hive whose meta project is stale/
				// empty on the hub — we must still adopt+report it, or the hub keeps
				// showing the raw placeholder host forever. Handle it BEFORE the
				// org-empty bail below, which exists so an empty project never blanks a
				// working config: with no org there is nothing to reconcile except the
				// URL, so adopt it, persist, and return without touching the project.
				if pc.Org == "" {
					// Whether or not it differs, a pushed URL is the hub saying it
					// owns this value; the dashboard renders the field read-only
					// from here on (#7451).
					b.dashSrv.SetHubPushedDashboardURL(pc.DashboardURL)
					if pc.DashboardURL != "" && b.cfg.Hub.DashboardURL != pc.DashboardURL {
						b.logger.Info("adopting vanity dashboard URL from hub heartbeat (url-only push)",
							"was", b.cfg.Hub.DashboardURL, "now", pc.DashboardURL)
						b.cfg.Hub.DashboardURL = pc.DashboardURL
						if err := b.cfg.Save(); err != nil {
							b.logger.Error("failed to save adopted vanity dashboard URL", "error", err)
						}
					}
					return
				}
				if issue := config.ValidateProjectRepoTargets(pc.Org, pc.Repos, pc.PrimaryRepo, b.cfg.GitHub.HostLabel()); issue != nil {
					b.logger.Error("REFUSING hub project config: repo target is misconfigured — project left unchanged",
						"error", issue.Message,
						"pushed_org", pc.Org,
						"pushed_repos", pc.Repos,
						"pushed_primary_repo", pc.PrimaryRepo,
					)
					return
				}
				curACMM := 0
				if b.cfg.ACMMLevel != nil {
					curACMM = *b.cfg.ACMMLevel
				}
				// Adopt the vanity dashboard URL delivered on claim, if any. We
				// report cfg.Hub.DashboardURL in our heartbeats, so once set the hub
				// registry's dashboardUrl becomes the vanity URL (not the placeholder
				// host). Track it in the already-reconciled check so a URL-only change
				// still gets applied and persisted.
				vanityMatched := pc.DashboardURL == "" || b.cfg.Hub.DashboardURL == pc.DashboardURL
				b.dashSrv.SetHubPushedDashboardURL(pc.DashboardURL) // #7451: hub-owned from here on
				authorMatched := pc.AIAuthor == "" || b.cfg.Project.AIAuthor == pc.AIAuthor
				apiURLMatched := pc.GitHubAPIURL == "" || b.cfg.GitHub.APIURL == pc.GitHubAPIURL
				// Issue filter: nil means "the hub is not speaking to this field"
				// (mirrors AIAuthor's empty-means-keep), so the hub's every-beat
				// echo can never blank a locally configured filter.
				issueFilterMatched := pc.IssueFilter == nil || b.cfg.Project.IssueFilter.Equal(*pc.IssueFilter)
				if b.cfg.Project.Org == pc.Org &&
					sameStringSlice(b.cfg.Project.Repos, pc.Repos) &&
					b.cfg.Project.PrimaryRepo == pc.PrimaryRepo &&
					curACMM == pc.ACMMLevel &&
					authorMatched &&
					apiURLMatched &&
					issueFilterMatched &&
					vanityMatched {
					return // already reconciled
				}
				if pc.DashboardURL != "" && b.cfg.Hub.DashboardURL != pc.DashboardURL {
					b.logger.Info("adopting vanity dashboard URL from hub heartbeat",
						"was", b.cfg.Hub.DashboardURL, "now", pc.DashboardURL)
					b.cfg.Hub.DashboardURL = pc.DashboardURL
				}
				b.logger.Info("project config updated from hub heartbeat (placeholder claimed)",
					"was_org", b.cfg.Project.Org, "now_org", pc.Org,
					"repos", pc.Repos, "primary_repo", pc.PrimaryRepo,
					"acmm_level", pc.ACMMLevel)
				b.cfg.Project.Org = pc.Org
				b.cfg.Project.Repos = pc.Repos
				b.cfg.Project.PrimaryRepo = pc.PrimaryRepo
				// Only adopt a non-empty author. The hub echoes this struct back on
				// every beat, so assigning unconditionally would reset a locally
				// configured ai_author to "" each time — which is precisely what
				// kept the fleet-stats collector disabled on every hive.
				if pc.AIAuthor != "" {
					b.cfg.Project.AIAuthor = pc.AIAuthor
				}
				// Adopt a hub-delivered issue filter only when the hub actually
				// sent one (non-nil). A push without the field leaves the spoke's
				// locally configured filter untouched — the org/repos assignments
				// above never wipe it either, so a local filter SURVIVES claim
				// delivery. A non-nil but EMPTY filter is an explicit clear.
				if pc.IssueFilter != nil && !b.cfg.Project.IssueFilter.Equal(*pc.IssueFilter) {
					b.logger.Info("adopting issue filter from hub heartbeat",
						"require_labels", pc.IssueFilter.RequireLabels)
					b.cfg.Project.IssueFilter = *pc.IssueFilter
				}
				// Adopt a GitHub Enterprise API URL when the hub sends one. Empty
				// means "leave mine alone" — the spoke's own default is already
				// api.github.com, so this never clobbers a working config.
				if pc.GitHubAPIURL != "" && b.cfg.GitHub.APIURL != pc.GitHubAPIURL {
					// WRITE-PATH GUARD, mirroring the App-config callback: an api_url
					// that names a different forge than our app_id is the same
					// half-applied identity arriving from the other direction. Skip
					// only this field — the org/repos/ACMM adoption around it is
					// unrelated and must still land.
					prospective := b.cfg.GitHub
					prospective.APIURL = pc.GitHubAPIURL
					if err := config.RejectIdentitySet(prospective); err != nil {
						b.logger.Error("REFUSING hub GitHub API URL: it does not match this hive's app_id and would half-apply an identity — api_url left unchanged",
							"error", err,
							"pushed_api_url", pc.GitHubAPIURL,
							"current_api_url", b.cfg.GitHub.APIURL,
							"current_app_id", b.cfg.GitHub.AppID,
							"remedy", "correct github_api_url/github_app_id for this cluster on the hub",
						)
					} else {
						b.logger.Info("adopting GitHub API URL from hub heartbeat",
							"was", b.cfg.GitHub.APIURL, "now", pc.GitHubAPIURL)
						b.cfg.GitHub.APIURL = pc.GitHubAPIURL
					}
				}
				level := pc.ACMMLevel
				b.cfg.ACMMLevel = &level

				// Re-sync the GitHub client that caches the repo list (mirrors the
				// config-watcher reload path). The issue filter is cached the same
				// way, so re-install it too — a hub-delivered filter must take
				// effect on the next enumeration, not the next restart.
				b.ghClient.SetRepos(b.cfg.Project.Repos)
				b.ghClient.SetIssueFilter(b.cfg.Project.IssueFilter)
				syncAutoMergePolicyToGitHubClient(b.cfg, b.ghClient)

				// Persist to the PVC overlay so the claim survives a pod restart
				// (config save writes the overlay hive.yaml, same as level switches).
				if err := b.cfg.Save(); err != nil {
					b.logger.Error("failed to save claimed project config", "error", err)
				}

			}),
			spoke.GatewayConfigCallback(func(gw *spoke.HeartbeatGatewayConfig) {
				// The hub funded an OpenRouter gateway on this hive's behalf (scan-to-
				// fund from My Hives) and delivered it over the heartbeat channel — the
				// only path that reaches a firewalled/heartbeat-only spoke (the heartbeat-only cluster). We
				// store the key in our OWN per-gateway secret-file store and create the
				// "openrouter" gateway. The hub drains the delivery after sending, so
				// this fires once per fund; the key value is never logged.
				if gw == nil || gw.Key == "" {
					return
				}
				if err := b.dashSrv.ApplyDeliveredGateway(gw.Name, gw.Kind, gw.Endpoint, gw.DefaultModel, gw.Key); err != nil {
					b.logger.Error("failed to apply hub-delivered gateway", "gateway", gw.Name, "error", err)
				}

			}),
			spoke.FreshStatusCollector(func() *spoke.HeartbeatPayload {
				if !b.cfg.Hub.Enabled {
					return nil
				}
				govState := b.gov.GetState()
				currentMode := strings.ToLower(string(govState.Mode))
				agents := b.heartbeatAgents(govState, currentMode, false)
				acmmLvl := b.heartbeatACMMLevel()
				providerLimitReason, providerLimitRebuffs, providerLimitHiveWide, providerLimitAgents := spoke.ProviderLimitHeartbeatFields(agents, dashboard.InferenceBudgetExceeded)
				lastWriteKickAt, kickDisposition, kickSkipReason, notWritableQueued :=
					outputFreshnessHeartbeatFields(acmmLvl, govState, agents)
				return &spoke.HeartbeatPayload{
					HiveID:                  b.cfg.HiveID,
					Org:                     b.cfg.Project.Org,
					Repos:                   b.cfg.Project.Repos,
					PrimaryRepo:             b.cfg.Project.PrimaryRepo,
					ACMMLevel:               acmmLvl,
					Agents:                  agents,
					Governor:                spoke.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs},
					Health:                  freshHeartbeatHealthSummary(agents),
					ProviderLimitReason:     providerLimitReason,
					ProviderLimitRebuffs:    providerLimitRebuffs,
					ProviderLimitHiveWide:   providerLimitHiveWide,
					ProviderLimitAgents:     providerLimitAgents,
					LastWriteCapableKickAt:  lastWriteKickAt,
					LastKickDisposition:     kickDisposition,
					LastKickSkipReason:      kickSkipReason,
					NotWritableQueued:       notWritableQueued,
					AgentErrorStreaks:       b.tokenCollector.AgentErrorStreaks(),
					ConsentWedged:           b.agentMgr.ConsentWedgedAgents(),
					NoCadenceAgents:         b.gov.NoCadenceAgents(),
					Reporter:                b.reporterName,
					StartedAt:               b.processStartedAt.UTC().Format(time.RFC3339),
					DashboardURL:            b.dashboardURLForFreshHeartbeat(),
					GitHash:                 gitShort,
					GitBranch:               gitBranch,
					Version:                 reportedVersion(),
					HiveType:                b.cfg.Hub.HiveType,
					ClusterID:               b.cfg.Hub.ClusterID,
					IsPublic:                b.cfg.Hub.IsPublic,
					RepoTargetMisconfigured: b.repoTargetMisconfigured(),
					RepoTargetIssue:         b.repoTargetIssueMessage(),
				}
			}))

		deps.startTaskStatusPush(b.ctx, b.hubURL, func() *spoke.TaskStatusPayload {
			reg, active := b.dashSrv.ContributorSummary()
			lb := b.dashSrv.LeaderboardForHub()
			out := make([]spoke.LeaderboardEntry, len(lb))
			for i, e := range lb {
				out[i] = spoke.LeaderboardEntry{
					GitHubUsername: e.GitHubUsername,
					AvatarURL:      e.AvatarURL,
					TrustTier:      e.TrustTier,
					TasksCompleted: e.TasksCompleted,
					TasksFailed:    e.TasksFailed,
					Active:         e.Active,
					CurrentTask:    e.CurrentTask,
				}
			}
			return &spoke.TaskStatusPayload{
				HiveID:       b.cfg.HiveID,
				Leaderboard:  out,
				Contributors: spoke.ContributorSummary{Registered: reg, Active: active},
			}
		}, b.logger)
	}
}

// bootLanes builds the opt-in trajectory-review, stall-replan and retro
// lanes that runLoop drives off the governor tick.
func (b *boot) bootLanes() {

	// Trajectory-review lane (opt-in): a second-model check that reads each
	// running agent's recent transcript and pauses on goal drift. Built once;
	// runs off the governor tick, gated by its own cadence. If the reviewer
	// cannot be constructed (no LiteLLM endpoint/model), the lane is disabled
	// with a single warning rather than erroring every tick.
	if b.cfg.Governor.Trajectory.IsEnabled() {
		reviewEndpoint, reviewKey, reviewModel := b.cfg.Governor.ResolveReviewer()
		reviewer, terr := trajectory.NewReviewer(trajectory.Config{
			Endpoint:        reviewEndpoint,
			APIKey:          reviewKey,
			Model:           reviewModel,
			TranscriptLines: b.cfg.Governor.Trajectory.TranscriptLines,
		})
		if terr != nil {
			// Enabled but not runnable — surface it as a dashboard alert, not
			// just a log line, so a safety control is never silently inert.
			// (Reconciled below so it also clears when the lane is disabled.)
			b.logger.Warn("trajectory-review lane enabled but not running", "reason", terr.Error())
		} else {
			b.trajLane = trajectory.NewLane(reviewer, b.agentMgr,
				dashboard.NewTrajectorySink(b.dashSrv, b.notifier),
				trajectory.LaneConfig{
					IntervalS:    b.cfg.Governor.Trajectory.IntervalS,
					OnDivergence: b.cfg.Governor.Trajectory.OnDivergence,
					ExemptAgents: b.cfg.Governor.Trajectory.ExemptAgents,
				}, b.logger)
			b.logger.Info("trajectory-review lane enabled",
				"model", reviewModel,
				"interval_s", b.cfg.Governor.Trajectory.IntervalS,
				"on_divergence", b.cfg.Governor.Trajectory.OnDivergence)
		}
	}
	// Clear any legacy "not configured" banner alert persisted by an older
	// build. The half-configured state is shown inline in Settings →
	// General, not in the top banner.
	b.dashSrv.ReconcileTrajectoryAlert(&b.cfg.Governor)

	// Stall-replan lane (Phase 3 planning intelligence): periodically detects
	// approved plans whose sub-tasks have stopped progressing and re-kicks the
	// architect to revise them, bounded by a per-plan replan cap. It runs off the
	// governor tick, gated by its own Due() cadence (no goroutine of its own), and
	// drives the architect only through SendKick (agentKicker) from this tick —
	// never from the agent-launch path — so it cannot touch the manager lock
	// unsafely. On by default; a no-op when there are no approved plans.
	if b.cfg.Governor.Replan.IsEnabled() {
		rc := b.cfg.Governor.Replan
		b.replanLane = planning.NewReplanLane(
			b.beadStores,
			agentKicker{mgr: b.agentMgr},
			b.gov,
			dashboard.NewReplanSink(b.dashSrv, b.notifier),
			planning.ReplanLaneConfig{
				IntervalS: rc.IntervalS,
				Stall: planning.StallConfig{
					StallThreshold: time.Duration(rc.StallThresholdS) * time.Second,
					MaxReplans:     rc.MaxReplans,
				},
			}, b.logger)
		b.logger.Info("stall-replan lane enabled",
			"interval_s", rc.IntervalS,
			"stall_threshold_s", rc.StallThresholdS,
			"max_replans", rc.MaxReplans)
	}
	if b.cfg.Retro.Enabled {
		retroStore := b.beadStores[retro.Actor]
		escalationStoreOnce.Do(func() {
			escalationStore = escalation.Load(escalationLedgerPath)
		})
		b.retroLane = retro.NewLane(b.beadStores, retroStore, b.dashSrv.LifecycleTimeline(), escalationStore, retro.Config{
			Enabled:             b.cfg.Retro.Enabled,
			ScanIntervalS:       b.cfg.Retro.ScanIntervalS,
			MaxFixAttempts:      b.cfg.Retro.MaxFixAttempts,
			MaxKicks:            b.cfg.Retro.MaxKicks,
			LongStallDays:       b.cfg.Retro.LongStallDays,
			RecentClosedWindowS: b.cfg.Retro.RecentClosedWindowS,
			AnalysisModel:       b.cfg.Retro.AnalysisModel,
			AnalysisEndpoint:    b.cfg.Governor.LiteLLM.ResolveEndpoint(),
			AnalysisAPIKey:      b.cfg.Governor.LiteLLM.ResolveAPIKey(),
		}, b.logger)
		if b.knowledgeAPI != nil {
			b.retroLane.SetKnowledgeSink(b.knowledgeAPI)
		}
		b.logger.Info("retro lane enabled",
			"scan_interval_s", b.cfg.Retro.ScanIntervalS,
			"max_fix_attempts", b.cfg.Retro.MaxFixAttempts,
			"max_kicks", b.cfg.Retro.MaxKicks,
			"long_stall_days", b.cfg.Retro.LongStallDays,
			"analysis_enabled", b.cfg.Retro.AnalysisModel != "")
	}
}

// runLoop waits for the CLIs to come up, runs the first eval cycle, then
// blocks on the governor ticker until the context is canceled. Its ticker
// defers are real defers: it is the last call in main(), so they fire at
// the same moment they always did.
func (b *boot) runLoop() { b.runLoopWith(defaultRunLoopDeps()) }

// runLoopWith is runLoop with its timers and per-tick IO injected; see
// runLoopDeps.
func (b *boot) runLoopWith(deps runLoopDeps) {
	b.logger.Info("entering governor loop", "interval_seconds", b.cfg.Governor.EvalIntervalS)
	lastEvalInterval := b.cfg.Governor.EvalIntervalS
	ticker := deps.newTicker(time.Duration(b.cfg.Governor.EvalIntervalS) * time.Second)
	defer ticker.Stop()

	var agentTickCh <-chan time.Time
	if b.cfg.Dashboard.AgentPollIntervalS > 0 {
		agentTicker := deps.newTicker(time.Duration(b.cfg.Dashboard.AgentPollIntervalS) * time.Second)
		defer agentTicker.Stop()
		agentTickCh = agentTicker.Chan()
		b.logger.Info("fast agent status enabled", "interval_seconds", b.cfg.Dashboard.AgentPollIntervalS)
	}

	// NOTE: dashSrv.MarkReady() was previously HERE, after the staggered agent
	// launch and the heartbeat/trajectory/ticker setup. It has been moved to
	// immediately after the HTTP listener starts (before the agent-launch loop),
	// so the pod becomes Ready in seconds instead of minutes. See the MarkReady
	// call and comment above the agent-launch goroutine.

	b.logger.Info("waiting for CLI startup before first eval", "delay", cliStartupDelay)
	if !deps.waitCLIStartup(b.ctx) {
		return
	}

	// #2573: startup must NOT clear persisted last-kick timestamps. It used to
	// (gov.ClearLastKicks) so that every eligible agent was kicked on the first
	// eval — "one kick per agent per pod boot, by design". But on hosted hives
	// the hub rolls the Deployment for every auto-upgrade, and each roll is a
	// brand-new pod with container restart count 0, so agents on 4h/6h cadences
	// were re-kicked at roll frequency — burning backend tokens ("Bob coins")
	// far beyond any configured cadence, while "next run" (recomputed from the
	// wiped timestamps) showed sooner than the cadence implied. LastKick state
	// is persisted to /data (PVC) after every eval and restored above via
	// SeedLastKicks, so this first eval kicks exactly the agents whose cadence
	// has actually elapsed — including everything after downtime longer than an
	// interval. A hive with no persisted state (fresh install) has no LastKick
	// entries, and every cadenced agent is still kicked here, unchanged.
	b.logger.Info("startup honors persisted cadence state — first eval kicks only agents whose cadence has elapsed")
	deps.runEval(b, nil)
	deps.runRotation(b)
	if b.wd != nil {
		b.wd.Tick(b.ctx)
	}
	deps.runSweeps(b)
	deps.persist(b)

	for {
		select {
		case <-b.ctx.Done():
			b.logger.Info("shutting down, persisting state")
			deps.persist(b)
			return
		case <-ticker.Chan():
			restarted := deps.restartCrashed(b.ctx, b.agentMgr)
			for _, name := range restarted {
				b.dashSrv.AuditLog("system", "restart", "trigger=crash-recovery", name)
			}
			// If brainstorm crashed during inception, re-kick via SendKick.
			// SendKick waits for the CLI to be ready and sends the message
			// If brainstorm crashed during inception, re-kick with bootstrap.
			// The table parser in the watcher will catch questions from the
			// agent's output even if bd create doesn't execute.
			for _, name := range restarted {
				if name == "brainstorm" && b.inceptionEngine != nil {
					if state := b.inceptionEngine.GetState(); state != nil && state.Phase == knowledge.PhaseCapture {
						msg := b.sched.BuildAgentMessage("brainstorm", nil, b.sched.GetLastActionable())
						if err := b.agentMgr.RestartWithBootstrap(b.ctx, "brainstorm", msg); err != nil {
							b.logger.Warn("inception re-kick after crash failed", "error", err)
						} else {
							b.logger.Info("brainstorm re-kicked after crash", "phase", state.Phase)
							b.dashSrv.AuditLog("system", "kick", "trigger=inception-crash-recovery", "brainstorm")
						}
						b.gov.RecordKick("brainstorm")
					}
				}
			}
			// Watchdog sweep (RFC #4665): synchronous but bounded — every
			// probe carries a deadline and restarts run detached under a hard
			// timeout, so a wedged agent can never stall this tick. Tick
			// self-gates to watchdog.probe_interval_s.
			//
			// It runs BEFORE runEvalCycle so agents it revived join this
			// cycle's resume-kick list rather than waiting a full eval
			// interval. Restarts are detached, so a given sweep's completions
			// are usually collected on the next pass — TakeRestarted drains
			// whatever has finished, and the governor gate gets the final say
			// either way.
			if b.wd != nil {
				// Re-resolve the mode each sweep so a change saved from the
				// dashboard (or the fleet-wide kill switch being engaged)
				// takes effect without a restart — and so dead-session
				// ownership moves with it. Without this, leaving heal via the
				// settings page would stop the watchdog restarting while the
				// manager's crash loop was still standing down: a window in
				// which NEITHER recovers a dead agent.
				if s, errs := watchdog.SettingsFrom(b.cfg.Governor.Watchdog); s.Mode != b.wd.Mode() {
					for _, e := range errs {
						b.logger.Warn("watchdog config problem", "error", e)
					}
					b.logger.Info("watchdog mode changed", "from", string(b.wd.Mode()), "to", string(s.Mode))
					b.dashSrv.AuditLog("system", "watchdog-mode", "from="+string(b.wd.Mode())+", to="+string(s.Mode), "")
					b.wd.SetSettings(s)
					b.agentMgr.SetDeadSessionRecoveryOwner(s.MayAct())
				}
				b.wd.Tick(b.ctx)
				for _, name := range b.wd.TakeRestarted() {
					b.dashSrv.AuditLog("system", "restart", "trigger=watchdog", name)
					restarted = append(restarted, name)
				}
			}
			deps.runEval(b, restarted)
			deps.runRotation(b)
			deps.runSweeps(b)
			// Trajectory review runs after the eval cycle (so kicks/intents are
			// current) on its own cadence, gated by Due().
			if b.trajLane != nil && b.trajLane.Due(time.Now()) {
				b.trajLane.Run(b.ctx)
			}
			// Stall-replan runs on the same tick, gated by its own Due() cadence.
			// It is synchronous and adds no goroutine; kicks go through the same
			// out-of-band SendKick path as the eval cycle above.
			if b.replanLane != nil && b.replanLane.Due(time.Now()) {
				if n := b.replanLane.Run(b.ctx); n > 0 {
					b.logger.Info("stall-replan lane re-kicked stalled plans", "replans", n)
				}
			}
			if b.retroLane != nil && b.retroLane.Due(time.Now()) {
				if n := b.retroLane.Run(b.ctx); n > 0 {
					b.logger.Info("retro lane filed advisory beads", "findings", n)
				}
			}
			deps.persist(b)
			if b.cfg.Governor.EvalIntervalS != lastEvalInterval && b.cfg.Governor.EvalIntervalS > 0 {
				b.logger.Info("eval interval changed, resetting ticker",
					"from", lastEvalInterval, "to", b.cfg.Governor.EvalIntervalS)
				ticker.Reset(time.Duration(b.cfg.Governor.EvalIntervalS) * time.Second)
				lastEvalInterval = b.cfg.Governor.EvalIntervalS
			}
		case <-agentTickCh:
			govState := b.gov.GetState()
			agentStatuses := b.agentMgr.AllStatuses()
			payload := dashboard.BuildAgentOnlyStatus(govState, agentStatuses, b.cfg)
			b.dashSrv.BroadcastAgentStatus(payload)
		}
	}

}

// buildRepoActivityWire maps the dashboard activity collector's per-repo
// snapshot into the plain hub wire structs the heartbeat carries. Kept here (in
// the one package that imports both hub and dashboard) so pkg/hub never has to
// import pkg/dashboard back — that would be an import cycle, since dashboard
// already imports hub. A field-by-field copy, mirroring how the fleet-stat
// scalars are lifted out of their snapshot at the beat's build site.
func buildRepoActivityWire(repos []collect.RepoActivity) []spoke.RepoActivityWire {
	if len(repos) == 0 {
		return nil
	}
	stat := func(s collect.ActivityActionStat) spoke.ActivityStatWire {
		return spoke.ActivityStatWire{Count: s.Count, NewestAt: s.NewestAt}
	}
	out := make([]spoke.RepoActivityWire, 0, len(repos))
	for _, r := range repos {
		agents := make([]spoke.AgentRepoActivityWire, 0, len(r.Agents))
		for _, a := range r.Agents {
			agents = append(agents, spoke.AgentRepoActivityWire{
				Agent:      a.Agent,
				Issues:     stat(a.Issues),
				PRs:        stat(a.PRs),
				Comments:   stat(a.Comments),
				Merges:     stat(a.Merges),
				Claims:     stat(a.Claims),
				Reviews:    stat(a.Reviews),
				Advisory:   stat(a.Advisory),
				Reconciled: stat(a.Reconciled),
			})
		}
		out = append(out, spoke.RepoActivityWire{
			Repo:       r.Repo,
			Issues:     stat(r.Issues),
			PRs:        stat(r.PRs),
			Comments:   stat(r.Comments),
			Merges:     stat(r.Merges),
			Claims:     stat(r.Claims),
			Reviews:    stat(r.Reviews),
			Advisory:   stat(r.Advisory),
			Reconciled: stat(r.Reconciled),
			Agents:     agents,
		})
	}
	return out
}

// providerBudgetNotify is the one-shot guard for the provider spend-rebuff
// notification (#4294). Package-level because runEvalCycle is a function called
// once per tick with no state of its own; mutex-guarded inside pkg/governor.
var providerBudgetNotify governor.ProviderBudgetNotifyState

// providerBudgetProbe remembers when the last probe kick was released while a
// provider spend rebuff is latched. Package-level for the same reason as
// providerBudgetNotify: runEvalCycle has no state of its own.
var providerBudgetProbe governor.ProviderBudgetProbeState

// applyBudgetAlerts turns budget threshold crossings into dashboard system
// alerts and notifications. Crossings fire once per window (governor tracks
// the one-shot flags); alerts are cleared when the threshold no longer
// applies (window rolled, limit raised, or budgeting disabled).
func applyBudgetAlerts(gov *governor.Governor, trans governor.BudgetTransitions, dashSrv *dashboard.Server, notifier *notify.Notifier) {
	if !trans.WarnActive {
		dashSrv.ClearSystemAlert(budgetWarnAlertID)
	}
	if !trans.ExhaustedActive {
		dashSrv.ClearSystemAlert(budgetExhaustedAlertID)
	}

	budget := gov.GetBudget()
	if trans.WarnCrossed {
		msg := fmt.Sprintf("token budget at %d%%+ of weekly limit: %d of %d tokens used",
			governor.BudgetWarnPct, budget.CurrentSpend, budget.WeeklyLimit)
		dashSrv.AddSystemAlert(budgetWarnAlertID, "warning", msg)
		notifier.Send("Budget warning", msg, notify.PriorityDefault)
	}
	if trans.ExhaustedCrossed {
		windowEnd := budget.ResetAt.Add(governor.BudgetWindowDuration)
		msg := fmt.Sprintf("token budget exhausted: %d of %d tokens used — agent kicks suspended until %s (exempt agents keep running)",
			budget.CurrentSpend, budget.WeeklyLimit, windowEnd.Format(time.RFC1123))
		dashSrv.AddSystemAlert(budgetExhaustedAlertID, "error", msg)
		notifier.Send("Budget exhausted", msg, notify.PriorityHigh)
		emitBudgetExhaustedEscalation(msg)
	}
}

// applyNoCadenceAlert keeps the never-kicked cause+fix banner (#5577) in sync
// with the governor's view: raised (warning, not error — the hive is not
// broken, it is unconfigured) while any enabled, governor-kickable agent has
// no cadence in any mode and has never been kicked; cleared the moment the
// operator sets a cadence or any kick path reaches the agent. This is the
// spoke-side parity for the hub verdict's no-cadence amber: the same
// governor-derived signal, rendered where the operator can act on it, with no
// hub round-trip.
func applyNoCadenceAlert(gov *governor.Governor, dashSrv *dashboard.Server) {
	agents := gov.NoCadenceAgents()
	if len(agents) == 0 {
		dashSrv.ClearSystemAlert(noCadenceAlertID)
		return
	}
	dashSrv.AddSystemAlert(noCadenceAlertID, "warning", noCadenceAlertMessage(agents))
}

func applyModeUnscheduledAlert(gov *governor.Governor, dashSrv *dashboard.Server) {
	spokealerts.ApplyModeUnscheduled(gov, dashSrv)
}

// agentKicker adapts *agent.Manager to planning.Kicker for the Phase 3
// stall-replan lane. Kick delegates to SendKick, which takes the manager lock
// ITSELF and is only ever called here from the governor tick (never from the
// agent-launch path), so it cannot re-enter a held manager lock. This is the
// same out-of-band kick path the eval loop already uses for governor kicks.
type agentKicker struct{ mgr *agent.Manager }

func (k agentKicker) Kick(agent, message string) error {
	return k.mgr.SendKick(agent, message)
}

// planFromLabeledIssues is Phase 4 Part B: for each actionable issue carrying a
// `plan`/`epic` label, mint an epic (idempotent) and hand it to the architect,
// RESPECTING the architect's pause. It is a plain synchronous call on the eval
// tick — no goroutine — and only ever touches the manager via SendKick/IsPaused,
// exactly like every governor kick, so it never re-enters the launch-path mutex.
// Epics are minted into the architect store (falling back to any store) so the
// dashboard plan-review flow and replan lane find them the same way.
//
// A minted epic is decompose_pending (plan_status=draft). While the architect is
// paused, the request stays queued and visible (the PLANNING tile shows it as
// pending) — we never force-unpause. Once the architect is available, we kick it
// each cycle until it decomposes and clears the pending marker.
func planFromLabeledIssues(
	actionable *github.ActionableResult,
	beadStores map[string]*beads.Store,
	agentMgr *agent.Manager,
	gov *governor.Governor,
	dashSrv *dashboard.Server,
	logger *slog.Logger,
	cfg *config.Config,
	acmmLevel int,
) {
	if actionable == nil || len(beadStores) == 0 {
		return
	}
	store, ok := beadStores[planning.ArchitectAgentName]
	if !ok {
		for name := range beadStores {
			store = beadStores[name]
			break
		}
	}
	if store == nil {
		return
	}

	designCfg := planning.DesignConfig{
		PlanLabels:    cfg.Planning.PlanLabelsOrDefault(),
		DesignLabels:  cfg.Planning.DesignLabelsOrDefault(),
		ApprovedLabel: cfg.Planning.DesignApprovedLabelOrDefault(),
		MaxRevisions:  cfg.Planning.MaxDesignRevisionsOrDefault(),
		MaxConcurrent: cfg.Planning.MaxConcurrentDesignsOrDefault(),
	}
	issues := actionable.Issues.Items
	if cfg == nil || !cfg.Planning.PlanFromLabelEnabled(acmmLevel) {
		issues = nil
		for _, issue := range actionable.Issues.Items {
			if store.FindByExternalRef(planning.IssueRef(issue)) != nil {
				issues = append(issues, issue)
			}
		}
	}
	sink := labelPlanSink{gov: gov, dashSrv: dashSrv, logger: logger}
	planning.PlanIssuesFromLabelsWithConfig(store, agentMgr, issues, designCfg, sink,
		func(ref string, err error) {
			logger.Warn("plan-from-label: minting epic failed", "issue", ref, "error", err)
		}, acmmLevel)
	if config.PlanAutoApproveForLevel(acmmLevel) {
		for _, id := range planning.AutoApproveDrafts(store) {
			if dashSrv != nil {
				dashSrv.AuditLog("planning", "plan_auto_approved", "epic="+id, planning.ArchitectAgentName)
			}
			logger.Info("audit: plan auto-approved by ACMM pack", "epic", id, "acmm_level", acmmLevel)
		}
	}
}

// labelPlanSink adapts the governor/dashboard/logger to planning.LabelPlanSink so
// the label-trigger core lives (and is tested) in pkg/planning.
type labelPlanSink struct {
	gov     *governor.Governor
	dashSrv *dashboard.Server
	logger  *slog.Logger
}

func (s labelPlanSink) KickedPlan(epic *beads.Bead) {
	s.gov.RecordKick(planning.ArchitectAgentName)
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "plan_from_label", "epic="+epic.ID+" ref="+epic.ExternalRef, planning.ArchitectAgentName)
	}
	s.logger.Info("audit: plan requested from labeled issue", "epic", epic.ID, "ref", epic.ExternalRef)
}

func (s labelPlanSink) FailedPlan(epic *beads.Bead) {
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "plan_decompose_failed", "epic="+epic.ID+" ref="+epic.ExternalRef+" attempts="+strconv.Itoa(planning.DecomposeMaxAttempts), planning.ArchitectAgentName)
	}
	s.logger.Warn("plan-from-label: architect produced no plan after max attempts — epic marked stuck; re-request it from the dashboard",
		"epic", epic.ID, "ref", epic.ExternalRef, "attempts", planning.DecomposeMaxAttempts)
}

func (s labelPlanSink) QueuedPlan(epic *beads.Bead, paused bool) {
	if paused {
		// Architect deliberately paused — queue, do not unpause, log once.
		s.logger.Info("plan-from-label: architect paused, plan queued", "epic", epic.ID, "ref", epic.ExternalRef)
		return
	}
	s.logger.Warn("plan-from-label: architect unavailable, plan queued", "epic", epic.ID, "ref", epic.ExternalRef)
}

func (s labelPlanSink) KickedDesign(epic *beads.Bead, revision int) {
	s.gov.RecordKick(planning.ArchitectAgentName)
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "design_kicked", "epic="+epic.ID+" ref="+epic.ExternalRef+" revision="+strconv.Itoa(revision), planning.ArchitectAgentName)
	}
	s.logger.Info("audit: design requested from labeled issue", "epic", epic.ID, "ref", epic.ExternalRef, "revision", revision)
}

func (s labelPlanSink) ApprovedDesign(epic *beads.Bead) {
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "design_approved", "epic="+epic.ID+" ref="+epic.ExternalRef, planning.ArchitectAgentName)
	}
	s.logger.Info("audit: design approved from labeled issue", "epic", epic.ID, "ref", epic.ExternalRef)
}

func (s labelPlanSink) DesignNeedsHuman(epic *beads.Bead, revisions int) {
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "design_needs_human", "epic="+epic.ID+" ref="+epic.ExternalRef+" revisions="+strconv.Itoa(revisions), planning.ArchitectAgentName)
	}
	s.logger.Warn("plan-from-label: design revision cap reached — epic needs a human", "epic", epic.ID, "ref", epic.ExternalRef, "revisions", revisions)
}

// healGitHubAppInstallation self-heals a hive whose github.installation_id
// points at the WRONG account — the failure mode diagnoseGitHubApp
// already detects and reports ("installation N belongs to 'X', not 'Y'"). It
// asks pkg/github to rediscover the installation covering cfg.Project.Org via
// the App JWT and, only on an unambiguous match, adopts it in place and
// persists it so the fix survives a pod restart.
//
// Every failure path is soft and silent-ish: a hive with no App key is not
// App-authenticated (skip), an API error or an ambiguous/absent discovery
// result leaves installation_id exactly as configured so the existing
// "check github.installation_id" banner still stands. It never returns an
// error to the caller and never blocks startup or a heartbeat.
//
// Rediscovery is rate-limited by pkg/github's discovery cache
// (github.InstallationDiscoveryTTL), so calling this from the self-heal tick
// is cheap even when the App is genuinely not installed on the org.
func healGitHubAppInstallation(ctx context.Context, appAuth *github.AppAuth, cfg *config.Config, logger *slog.Logger) {
	if appAuth == nil || !appAuth.HasKey() || cfg == nil {
		return
	}
	org := cfg.Project.Org
	if org == "" {
		return
	}
	newID, err := appAuth.RediscoverAndAdopt(ctx, org, logger)
	if err != nil {
		logger.Debug("github app installation rediscovery did not adopt a new id",
			"org", org, "error", err)
		return
	}
	if newID == 0 {
		return // already correct, or nothing safe to adopt
	}
	cfg.GitHub.InstallationID = newID
	if err := cfg.Save(); err != nil {
		logger.Error("adopted rediscovered installation_id but failed to persist it — "+
			"it will revert on the next pod restart",
			"installation_id", newID, "error", err)
		return
	}
	logger.Info("persisted rediscovered github app installation_id",
		"installation_id", newID, "org", org)
}

// maxTimelineEnumeratePerCycle bounds how many enumerated-issue events a single
// eval cycle records into the lifecycle timeline, keeping the recording loop
// O(1)-bounded so it never slows the eval cycle. Since #5656 the store dedupes
// by (ref, kind) — re-enumeration refreshes the journey instead of appending —
// so the cap no longer protects the ring from eviction floods; it matches the
// endpoint's default journey limit so every renderable journey gets refreshed.
const maxTimelineEnumeratePerCycle = 200

// lifecycleRecorder narrows *dashboard.Server to just the timeline accessor the
// recording helpers need, so they stay trivially testable with a fake and never
// depend on the rest of the Server surface.
type lifecycleRecorder interface {
	LifecycleTimeline() *timeline.Store
}

// recordEnumeratedIssues records a KindEnumerated event for each enumerated
// actionable issue, bounded by maxTimelineEnumeratePerCycle. The store dedupes
// by (ref, kind), so each cycle refreshes the journeys' enumerated stage
// rather than appending a flood (#5656). It is fully guarded: a nil recorder,
// nil store, or nil actionable set is a no-op, and Record itself is nil-safe.
// This must never slow or break the eval loop, so it does no blocking I/O
// (journey persistence is throttled and atomic inside the store).
//
// PR-open/merge lifecycle spans are emitted by the same tracing mapper when
// callers record those timeline events; this helper only has enumerated issues.
func recordEnumeratedIssues(ctx context.Context, rec lifecycleRecorder, actionable *github.ActionableResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rec == nil || actionable == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	limit := maxTimelineEnumeratePerCycle
	if len(actionable.Issues.Items) < limit {
		limit = len(actionable.Issues.Items)
	}
	for i := 0; i < limit; i++ {
		issue := actionable.Issues.Items[i]
		event := timeline.Event{
			IssueRef: issueRef(issue.Repo, issue.Number),
			Kind:     timeline.KindEnumerated,
		}
		_, span := tracing.StartTimelineSpan(ctx, event)
		store.Record(event)
		span.End()
	}
}

// recordKick records KindKicked events for the given agent. When issue refs are
// supplied it records one issue-scoped event per ref so post-completion lanes
// can reconstruct per-bead kick counts. With no refs, it records one
// agent-scoped event. Guarded and nil-safe; no I/O.
func recordKick(ctx context.Context, rec lifecycleRecorder, agent string, issueRefs ...string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rec == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	if len(issueRefs) > 0 {
		for _, ref := range issueRefs {
			if ref == "" {
				continue
			}
			event := timeline.Event{
				IssueRef: ref,
				Kind:     timeline.KindKicked,
				Agent:    agent,
			}
			_, span := tracing.StartTimelineSpan(ctx, event)
			store.Record(event)
			span.End()
		}
		return
	}
	event := timeline.Event{
		Kind:  timeline.KindKicked,
		Agent: agent,
	}
	_, span := tracing.StartTimelineSpan(ctx, event)
	store.Record(event)
	span.End()
}

// issueRef renders the canonical "repo#number" reference the timeline uses to
// group events by issue. An empty repo yields an empty ref (the store tolerates
// it).
func issueRef(repo string, number int) string {
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("%s#%d", repo, number)
}

func actionableIssueRef(issue github.Issue) string {
	ref := worksource.Ref{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		URL:        issue.URL,
	}
	if key := ref.Key(); key != "" {
		return key
	}
	if issue.Repo != "" {
		return issue.Repo
	}
	return issue.ExternalID
}

// githubRateLimitErrText is the substring GitHub's client surfaces on a rate or
// abuse limit. Matching text is acceptable ONLY here: a rate limit is a reason
// to skip classification entirely, never a reason to accuse anyone of anything,
// so a false negative costs one extra (correct) classification round-trip.
const githubRateLimitErrText = "rate limit"

// isGitHubRateLimitText reports whether an error looks like a GitHub rate limit.
func isGitHubRateLimitText(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), githubRateLimitErrText)
}

// The GitHub App credential classifiers moved to pkg/apphealth (#7238). These
// wrappers exist so call sites keep their current shape and so the App key
// paths -- vars that tests repoint at a temp dir -- are read HERE, at call
// time, rather than captured once at init.
func classifyGitHubAppFailure(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, logger *slog.Logger) (bool, string, github.AppAuthState) {
	return apphealth.ClassifyFailure(ctx, appAuth, expectedOwner, appKeyPaths(), logger)
}

func classifyGitHubAppWriteForbidden(ctx context.Context, appAuth *github.AppAuth, expectedOwner, repo string) (string, github.AppAuthState) {
	return apphealth.ClassifyWriteForbidden(ctx, appAuth, expectedOwner, repo, appKeyPaths())
}

func classifyGitHubAppRepoCoverage(ctx context.Context, appAuth *github.AppAuth, org string, repos []string, logger *slog.Logger) (bool, string, github.AppAuthState) {
	return apphealth.ClassifyRepoCoverage(ctx, appAuth, org, repos, logger)
}

// The advisory digest posting policy moved to pkg/advisory (#7238 stage 2).
// These wrappers keep the existing call sites unchanged; advisoryPostGate is
// now an owned instance rather than a package-level struct tests reset.
var advisoryPostGate = advisory.NewPostGate()

func primaryAdvisoryRepo(cfg *config.Config) string {
	return advisory.PrimaryRepo(cfg)
}

func advisoryIssueUnresolved(advisoryIssues map[string]int, repo string) bool {
	return advisory.IssueUnresolved(advisoryIssues, repo)
}

func advisoryIssueNumber(advisoryIssues map[string]int, repo string) (int, bool) {
	return advisory.IssueNumber(advisoryIssues, repo)
}

func shouldBuildAdvisoryDigest(beadStores map[string]*beads.Store, ghClient *github.Client, hasExistingPinnedIssue bool) bool {
	return advisory.ShouldBuildDigest(beadStores, ghClient != nil, hasExistingPinnedIssue)
}

func shouldPostAdvisoryDigest(digest *advisory.Digest, ghClient *github.Client, hasPinnedIssue bool) bool {
	return advisory.ShouldPostDigest(digest, ghClient != nil, hasPinnedIssue)
}

func advisoryPostDue(advCfg config.AdvisoryConfig, repo string, now time.Time, logger *slog.Logger) bool {
	return advisoryPostGate.Due(advCfg, repo, now, logger)
}

func recordAdvisoryPostSuccess(repo string, now time.Time) {
	advisoryPostGate.RecordSuccess(repo, now)
}

// advisoryIssueMissingError is the error recorded (and reported to the hub) when
// a hive has findings to publish but no advisory issue to publish them to. It is
// deliberately an ERROR rather than a silent skip: the hub's staleness gate
// treats a hive reporting neither a post time nor an error as "not an advisory
// participant" and never alarms, which is how a wedged digest went unnoticed for
// six days in #4167.
func advisoryIssueMissingError(repo string, cause error) string {
	base := fmt.Sprintf("no advisory issue resolved for %s — digest not posted", repo)
	// Issues-disabled is the one ensure failure with a remedy the OPERATOR of
	// the target repo owns (#4329): flipping a repo setting, not fixing App
	// auth. Fold its actionable message into the alert text so the fleet
	// stale-advisory pill says so instead of reading like an auth failure.
	var disabled *github.IssuesDisabledError
	if errors.As(cause, &disabled) {
		return base + ": " + disabled.Error()
	}
	return base
}

// actionableAfterGitHubEnumerate decides whether an eval cycle survives a
// failed GitHub enumeration. On the default (GitHub) work source the answer
// is no: an all-repos failure usually means a rate limit or outage, and a
// zero-count result would idle the agents, so the cycle keeps prior state.
//
// On a non-default work source (e.g. Linear) the GitHub call is only there
// for PR maintenance; the backlog comes from the work-source overlay that
// runs next. Aborting here meant a Linear-sourced hive whose GitHub App could
// not list issues (403 "Resource not accessible by integration", an Issues
// permission a Linear hive should not need) never enumerated its Linear
// backlog at all and sat at queue 0. Such a hive continues with whatever
// partial result GitHub returned (nil becomes an empty result; PRs are kept
// when obtainable) and lets the overlay populate issues.
func actionableAfterGitHubEnumerate(cfg *config.Config, actionable *github.ActionableResult, err error, logger *slog.Logger) (*github.ActionableResult, bool) {
	if err == nil {
		return actionable, true
	}
	wsType := cfg.Governor.WorkSource.Type
	if wsType == "" || wsType == "github" {
		logger.Error("failed to enumerate actionable items", "error", err)
		return nil, false
	}
	logger.Warn("GitHub enumeration failed; continuing so the configured work source can still populate issues",
		"work_source", wsType, "error", err)
	if actionable == nil {
		actionable = &github.ActionableResult{GeneratedAt: time.Now()}
	}
	return actionable, true
}

func runEvalCycle(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	gov *governor.Governor,
	sched *scheduler.Scheduler,
	agentMgr *agent.Manager,
	dashSrv *dashboard.Server,
	notifier *notify.Notifier,
	beadStores map[string]*beads.Store,
	tokenCollector *tokens.Collector,
	metricsCollector *dashboard.MetricsCollector,
	nousState *dashboard.NousState,
	lastActionable *atomic.Pointer[github.ActionableResult],
	advisoryStore *advisory.Store,
	advisoryIssues map[string]int,
	restartedAgents []string,
	approvalDesk *toolapprove.Desk,
	logger *slog.Logger,
) {
	// Governor eval-cycle span. When tracing is disabled (the default) this is
	// a no-op span with no allocation of note and no export — see pkg/tracing.
	ctx, span := tracing.StartSpan(ctx, "governor.eval_cycle",
		attribute.String("hive.id", cfg.HiveID),
		attribute.Int(tracing.AttrHiveACMMLevel, inferACMMLevel(cfg)))
	defer span.End()

	// A hive running without GitHub credentials (placeholder app_id, or a real
	// App whose key could not be read) has nothing to enumerate. Return before
	// the first API call rather than logging a misleading enumeration failure
	// once per eval interval — the dashboard banner already states the cause.
	if ghClient == nil {
		logger.Debug("skipping eval cycle: hive is running without GitHub credentials")
		return
	}

	// Re-ensure the pinned advisory issue whenever it is still unresolved, not
	// only while the App banner is up (#4167). The startup ensure can fail for
	// reasons that deliberately do NOT raise that banner — a rate limit, a 5xx,
	// a search-API blip — and the old gate meant such a hive kept an empty
	// advisoryIssues map for the rest of the process lifetime: every later
	// digest silently found no issue to post to, so the pinned comment froze at
	// whatever it last said. Retrying here is cheap (one search per eval cycle
	// only while unresolved, nothing once resolved) and is the difference
	// between a transient boot error and a permanently wedged digest.
	//
	// advisoryEnsureErr keeps this cycle's ensure failure so the post-path
	// error recorded below can name the CAUSE (e.g. Issues disabled on a fork,
	// #4329) instead of only the symptom.
	primaryRepoAtCycleStart := primaryAdvisoryRepo(cfg)
	_, hadPinnedAdvisoryIssueAtCycleStart := advisoryIssueNumber(advisoryIssues, primaryRepoAtCycleStart)
	advisoryEnsureDepsForCycle := advisoryEnsureDeps{setenv: os.Setenv}
	if ghClient != nil {
		advisoryEnsureDepsForCycle.ensure = ghClient.EnsureAdvisoryIssue
	}
	advisoryEnsureErr := ensurePinnedAdvisoryIssue(
		ctx, advisoryIssues, primaryRepoAtCycleStart, advisoryEnsureDepsForCycle, logger)

	enumCtx, enumSpan := tracing.StartSpan(ctx, "governor.enumerate_actionable")
	actionable, err := ghClient.EnumerateActionable(enumCtx)
	enumSpan.End()
	actionable, ok := actionableAfterGitHubEnumerate(cfg, actionable, err, logger)
	if !ok {
		return
	}

	// If a non-default work source is configured, overlay its issues onto
	// the actionable result. PRs always come from GitHub.
	if wsType := cfg.Governor.WorkSource.Type; wsType != "" && wsType != "github" {
		ghToken := cfg.GitHub.Token
		if ghToken == "" {
			ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
		}
		ws, wsErr := worksource.FromConfig(cfg.Governor.WorkSource, ghClient, ghToken, cfg.Project.Org, logger)
		actionable.Issues = workSourceIssuesForCycle(ctx, ws, wsErr, cfg.Governor.Labels.Exempt, cfg.Project.IssueFilter, logger)
	}

	ghClient.EnrichCIStatus(ctx, actionable.PRs.Items)
	// Held PRs need CI status too, or a red held PR can never be repaired:
	// the hold label kept it out of PRs.Items, so nothing ever learned it was
	// red and no agent was ever told to fix it (hivecommons/hive#7438). This
	// enriches the held list ONLY for the repair path — held PRs still never
	// reach the merge sweep, escalation or the queue counts.
	ghClient.EnrichCIStatus(ctx, actionable.PRs.Held)

	// Publish the human-facing "what should I merge next?" digest. This reads
	// the PR set enumerated and CI-enriched immediately above, so it must stay
	// after those two calls: Mergeable and the failing-check names it sorts on
	// are populated by EnrichCIStatus, not by EnumerateActionable.
	postRecommendationsForCycle(ctx, cfg, ghClient, actionable, logger)

	// Fold this pass's CI state into the fix-loop staleness clock BEFORE any
	// consumer reads it, so the claim-suppression guard (#3), the merge watcher
	// (#2), and the stuck-PR reaper (#4) all key off a consistent, current
	// signal within the same tick. This only records first-seen-red times; the
	// distinct-SHA attempt counting still happens in runEscalationSweep below.
	recordRedStaleness(cfg, actionable)

	// Duplicate-PR guard: drop issues an open hive-authored PR already claims,
	// before the governor counts the queue or the scheduler builds kicks. A
	// restart storm otherwise re-offers the same issue on every fresh agent
	// start, and the agent — having no memory of the PR it just filed — files
	// another. Backed by a PVC ledger so it survives those restarts, and fails
	// closed (keeps the last known claims) when the GitHub API is unavailable.
	applyDuplicatePRGuard(ctx, cfg, ghClient, actionable, logger)

	lastActionable.Store(actionable)
	if data, err := json.Marshal(actionable); err == nil {
		atomicWrite(lastActionablePath, data)
	}

	// Record enumerated issues into the lifecycle timeline so the dashboard's
	// lifecycle view has real data. Cheap and fully guarded: a nil dashboard or
	// nil store is a no-op (timeline.Store.Record is nil-safe), the loop is
	// bounded by maxTimelineEnumeratePerCycle, and the store dedupes by
	// (ref, kind) so this per-cycle sweep refreshes journeys instead of
	// flooding them (#5656).
	recordEnumeratedIssues(ctx, dashSrv, actionable)

	escalatedPRs := runEscalationSweep(ctx, cfg, governorForge(cfg, ghClient, logger), actionable, notifier, dashSrv, logger)

	intentVerdicts := writeIntentVerdicts(ctx, cfg, ghClient, actionable, beadStores, logger)
	refreshReviewVerdicts(cfg, logger)
	requiredCheckSet, _ := cfg.AutoMerge.RequiredCheckSet()

	// Hold guard (#5589): snapshot hold-gated PR heads, and when a hold lifts
	// on a branch that moved, block the merge lanes and force a fresh review.
	// Runs before writeMergeEligible so drifted PRs are excluded from the very
	// tick their hold lifted — no window for the sweep to race the re-hold.
	holdDriftPRs := enforceHoldGuard(ctx, cfg, ghClient, governorForge(cfg, ghClient, logger), actionable, logger)

	// The per-PR verdicts come back so the dashboard's PR pills can be
	// painted from the sweep's own classification rather than a looser
	// reading of GitHub's mergeable flag (hivecommons/hive#7478).
	mergeVerdicts := writeMergeEligible(actionable, actionable.Hold, cfg.Project.Org, escalatedPRs, cfg.Intent.Enforce, intentVerdicts, cfg.Review.RequireApproval, requiredCheckSet, holdDriftPRs, logger)

	// Review-bot threads (hivecommons/hive#7360): list every unresolved
	// external-review-bot thread on an open hive-authored PR into
	// review-threads.json, attributed to the agent that opened the PR the
	// same way ci-failing.json is, so the scheduler can route each PR back
	// to its author for a fix + in-thread replies before any new work.
	writeReviewThreads(ctx, ghClient, actionable, cfg.Project.Org, escalatedPRs, logger)

	// Stuck-PR reaper (backstop): re-dispatch a fix for any hive-authored PR
	// that is red on a required check AND stale (its red head SHA unchanged past
	// RedPRStaleAfter). writeMergeEligible already surfaces every red PR into
	// ci-failing.json (the CI_FAILING kick block), so the PR is already in the
	// work list; the reaper's job is to guarantee a STALE one is not silently
	// abandoned, to dedup the dispatch via the escalation store's re-engagement
	// cap (so a permanently-red PR is not re-nudged every tick forever), and to
	// stand down for PRs already escalated to a human. Composes with the merge
	// watcher (#2): both go through the same TryReEngage cap, so the same PR is
	// never double-dispatched within a red-SHA's budget.
	reapStuckRedPRs(cfg, actionable, escalatedPRs, logger)

	shaResult, shaErr := ghClient.EnforceSHAHold(ctx, github.SHAHoldConfig{
		PrimaryRepo:     cfg.Project.PrimaryRepo,
		AIAuthor:        cfg.Project.AIAuthor,
		InternalAuthors: []string{"kubestellar-hive[bot]", "github-actions[bot]", "dependabot[bot]", "copilot-swe-agent[bot]"},
	})
	if shaErr != nil {
		logger.Warn("SHA hold enforcement failed", "error", shaErr)
	} else {
		logger.Info("SHA hold enforcement complete",
			"held", shaResult.Held,
			"unheld", shaResult.Unheld,
			"skipped", shaResult.Skipped,
		)
	}

	// Refresh budget spend from lifetime token totals before Evaluate so
	// the kick gate sees current-window numbers.
	if tokenCollector != nil {
		if summary := tokenCollector.Summary(); summary != nil {
			trans := gov.UpdateBudgetFromTotals(summary.TotalTokens, summary.ByAgent, summary.ByModel)
			applyBudgetAlerts(gov, trans, dashSrv, notifier)
		}
	}

	// Cause+fix banner for the never-kicked class (#5577): the dashboard's
	// not-producing warnings name the SYMPTOM (agent idle, zero tokens); this
	// names the cause — enabled agent, no cadence in any mode, never kicked —
	// and the fix. Computed from the spoke's own governor config, no hub
	// round-trip; self-clears the moment a cadence is set or any kick lands.
	applyNoCadenceAlert(gov, dashSrv)

	agentsDue := gov.EvaluateWithRepoDepths(
		actionable.Issues.Count,
		actionable.PRs.Count,
		actionable.Hold.Total,
		actionable.Issues.SLAViolations,
		governor.RepoDepthsFromActionable(actionable),
	)

	// The weaker sibling of the banner above (#7474): an agent SOME mode
	// schedules but the mode the fleet is now in does not — a reviewer with a
	// cadence only in surge goes silent the moment its own work drives the
	// backlog below the surge threshold, and every other signal calls it
	// healthy. Applied after Evaluate so it reads the mode this tick settled
	// on; self-clears when the mode changes back or the operator fills the
	// gap.
	applyModeUnscheduledAlert(gov, dashSrv)

	// Crash-restarted agents may get a "resume" kick ahead of their cadence
	// slot so work interrupted mid-task resumes promptly — but ONLY through
	// the governor's gate (#2573). Unconditionally kicking every restarted
	// agent meant a crash-looping CLI was kicked on every eval cycle,
	// burning backend tokens far faster than any configured cadence and
	// bypassing the budget gate; AllowResumeKick bounds resume kicks to one
	// per cadence interval and respects mode pauses and the budget.
	agentsDue = mergeResumeKicks(agentsDue, restartedAgents, gov.AllowResumeKick, logger)

	govState := gov.GetState()
	span.SetAttributes(
		attribute.String(tracing.AttrHiveGovernorMode, string(govState.Mode)),
		attribute.Int("hive.queue.issues", govState.QueueIssues),
		attribute.Int("hive.queue.prs", govState.QueuePRs),
		attribute.Int("hive.queue.hold", govState.QueueHold),
	)
	// cadence.Paused (cadence: "pause" in config) means "don't kick this agent
	// in this mode" — it does NOT force-pause the agent. Manual pause/resume
	// via the dashboard is always respected; the governor only controls kicks.

	// Filter out on-demand agents — they are only triggered explicitly
	// Operator-paused agents must consume NOTHING (#2573); see
	// filterKickableAgents for the full gate. This runs BEFORE the eval-cycle
	// log so "agents_due" names the agents that are actually about to be
	// kicked; anything gated out is reported in "agents_skipped" with its
	// reason rather than vanishing silently.
	agentsDue, agentsSkipped := partitionKickableAgents(agentsDue, cfg.Agents, config.OnDemandAgentsFromPacks(), agentMgr.IsPaused)

	logger.Info("governor eval complete",
		"mode", govState.Mode,
		"issues", govState.QueueIssues,
		"prs", govState.QueuePRs,
		"agents_due", agentsDue,
		"agents_skipped", agentsSkipped,
	)

	// PROVIDER SPEND REBUFF (#4294). When the inference gateway is refusing on a
	// money limit, every kick launched this cycle is a run that cannot buy a
	// single token — precisely the failure this addresses: a hive that kept
	// firing its cadence into a gateway rejecting 100% of requests all day,
	// silently, until the provider's spend window happened to roll over.
	//
	// The alert is raised HERE, before kick assembly, so an operator is told
	// even on a cycle where nothing happened to be due. The actual suppression
	// happens after every kick source has contributed — see below.
	providerBudgetCause, providerBudgetSince, providerBudgetLastRebuff, providerBudgetRebuffs := dashboard.InferenceBudgetExceeded()
	// Suppress only while the latch is FRESH. Withholding kicks also withholds
	// the inference calls that clear the latch, so a hive whose only kick source
	// is the governor cadence would never learn the provider's window reset and
	// would stay muted forever — the exact topology in the field report. Once
	// the last rebuff is older than the probe interval, this cycle's kicks go
	// through as a probe: still clipped re-freshens the stamp and suppression
	// resumes, served clears the latch outright.
	providerBudgetProbeInterval := cfg.Governor.ProviderBudget.EffectiveProbeInterval()
	providerBudgetLatched := providerBudgetCause != ""
	suppressKicks := governor.ProviderBudgetSuppresses(providerBudgetLatched,
		providerBudgetProbe.Freshest(providerBudgetLastRebuff), time.Now(), providerBudgetProbeInterval)
	budgetAlert := decideProviderBudgetAlert(providerBudgetLatched, suppressKicks,
		providerBudgetCause, providerBudgetSince, providerBudgetRebuffs,
		func() string {
			return hub.QuotaExhaustedAgentReason(hub.QuotaExhaustedProcessCount(agentMgr.AllStatuses()))
		})
	if budgetAlert.Message != "" {
		dashSrv.AddSystemAlert(providerBudgetAlertID, "error", budgetAlert.Message)
	}
	if budgetAlert.Clear {
		dashSrv.ClearSystemAlert(providerBudgetAlertID)
	}
	if budgetAlert.Cause != "" {
		providerBudgetCause = budgetAlert.Cause
	}
	// Notify ONCE per latch, not once per cycle. The deduped banner above
	// already carries the ongoing state; a high-priority notification repeated
	// every eval cycle for as long as the provider stays clipped is a day of
	// pages saying the same thing. Keyed on the latch time, which recordRebuff
	// deliberately does not move forward, so a genuinely new clip after a
	// recovery notifies again. Matches applyBudgetAlerts, which notifies on the
	// crossing rather than on the condition.
	notifyProviderBudget := providerBudgetLatched && providerBudgetNotify.ShouldSend(providerBudgetSince)
	if !providerBudgetLatched {
		providerBudgetProbe.Reset()
		// The RECOVERY crossing: the latch a notification went out for has
		// cleared (a probe's inference call succeeded), so tell the operator
		// once that the hive resumed — the counterpart of the entering page,
		// without which the only signal of recovery is a banner quietly
		// vanishing. Every later healthy cycle is silent.
		if providerBudgetNotify.Reset() {
			logger.Info("provider spending limit lifted: agent kicks resumed")
			notifier.Send("Provider spending limit lifted",
				"the inference provider is serving again — agent kicks have resumed",
				notify.PriorityDefault)
		}
	}

	// ADDITIVE CEL routing: evaluate operator-defined CEL trigger rules
	// (cfg.Triggers, pkg/celtrigger) against the items enumerated this cycle and
	// UNION any matched, gated agents into agentsDue. This runs alongside — never
	// instead of — the label/governor selection above: unionAgents only ever adds
	// names, and celMatchedAgents enforces the same pause/budget/on-demand gates,
	// so a CEL match can neither remove a governor-selected agent nor bypass a
	// paused agent or exhausted budget. Fully guarded/cheap: with no `triggers:`
	// configured, celEngineFor returns nil and celMatchedAgents does zero work.
	if celEngine := celEngineFor(cfg, logger); celEngine != nil {
		celAgents := celMatchedAgents(celEngine, actionable, cfg, gov, agentMgr.IsPaused, logger)
		if len(celAgents) > 0 {
			before := len(agentsDue)
			agentsDue = unionAgents(agentsDue, celAgents)
			if len(agentsDue) > before {
				logger.Info("celtrigger: unioned CEL-matched agents into due set",
					"added", agentsDue[before:], "cel_matched", celAgents)
			}
		}
	}

	// #4247/#4263 (parent #3845): apply the shared convergence admission at the
	// internal-kick dispatch boundary — AFTER governor policy evaluated the raw
	// queue, BEFORE the scheduler caches and renders issues. Gated by the
	// runtime convergence rollout mode, captured once per pass: with mode "off"
	// (the DEFAULT) this is entirely inert and kickActionable IS actionable;
	// "shadow" logs and records what would be withheld but still dispatches the
	// raw population; only "enforce" gates the scheduled/cached issue payloads
	// below. The raw actionable population stays authoritative for governor
	// policy, dashboard status, PR/review dispatch, escalation, and every path
	// not explicitly enrolled.
	kickActionable := applyConvergenceKickAdmission(cfg, dashSrv, actionable, notifier, logger)

	sched.SetLastActionable(kickActionable)
	reviewPlan := planReviewDispatch(cfg, actionable, agentMgr, logger)
	emitReviewHumanEscalations(reviewPlan)
	applyHumanDecisionLabels(ctx, cfg, ghClient, actionable, reviewPlan, logger)
	messages := sched.BuildKickMessages(kickActionable, agentsDue)
	reviewKickByMessage := map[string]review.DispatchKick{}
	for _, k := range append(reviewPlan.ReviewKicks, reviewPlan.FixKicks...) {
		if !gov.AgentEligibleForCELKick(k.Agent) || agentMgr.IsPaused(k.Agent) {
			logger.Info("review swarm kick suppressed by governor gate", "agent", k.Agent, "pr", k.PRRef)
			continue
		}
		messages = append(messages, scheduler.KickMessage{Agent: k.Agent, Message: k.Message, IssueRefs: []string{k.PRRef}})
		reviewKickByMessage[k.Agent+"\x00"+k.Message] = k
	}
	// #4294: drop EVERY assembled kick while the provider is refusing on a
	// spending limit. Placed after all three sources have contributed —
	// governor-due agents, the CEL union, and the review swarm — because gating
	// only `agentsDue` earlier would still let a CEL match or a review kick fire
	// into the same clipped key.
	//
	if len(messages) > 0 {
		filtered := messages[:0]
		for _, msg := range messages {
			if remaining, class, line, ok := agentMgr.ProviderErrorBackoffRemaining(msg.Agent); ok {
				logger.Warn("provider inference error: withholding agent kick during backoff",
					"agent", msg.Agent,
					"class", class,
					"retry_in", remaining.Round(time.Second),
					"error", line)
				continue
			}
			filtered = append(filtered, msg)
		}
		messages = filtered
	}

	// Suppression is total rather than per-agent because the limit is on the
	// KEY: no agent can succeed while it is clipped. It self-heals — the first
	// inference call that succeeds after the provider's window resets clears the
	// signal — so there is no timer to tune and no operator action required.
	//
	// Deliberately does NOT force-pause agents. Operator pause state is a human
	// decision (#2573) and must not be forged by an automatic signal that will
	// clear itself; withholding kicks achieves the saving without leaving paused
	// agents behind for a human to un-pause by hand.
	//
	// Probe cycle: when the last rebuff has gone stale, ONE kick is
	// deliberately allowed through to find out whether the provider is
	// serving again. Its inference calls are what clear the latch (on a
	// 2xx) or re-freshen it (on another rebuff) — nothing else can. Only
	// one: the question is "is the window still clipped", and every kick
	// beyond the first spends a run to learn the same answer. Releasing it
	// re-arms suppression immediately, so the cycles while the probe's run
	// is still in flight withhold again rather than leaking more kicks.
	kickGate := gateKickMessagesForProviderBudget(messages, suppressKicks, providerBudgetLatched)
	releaseProviderBudgetProbe := kickGate.ReleaseProbe
	if suppressKicks && len(messages) > 0 {
		logger.Warn("provider spending limit: withholding agent kicks",
			"withheld", kickGate.Withheld, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince,
			"next_probe_in", (providerBudgetProbeInterval - time.Since(providerBudgetProbe.Freshest(providerBudgetLastRebuff))).Truncate(time.Second))
	} else if releaseProviderBudgetProbe {
		if len(kickGate.Withheld) > 0 {
			logger.Warn("provider spending limit: withholding all but the probe kick",
				"withheld", kickGate.Withheld, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince)
		}
		logger.Info("provider spending limit: releasing a single probe kick",
			"probe_agent", kickGate.Kept[0].Agent, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince,
			"last_rebuff", providerBudgetLastRebuff, "probe_interval", providerBudgetProbeInterval)
	}
	messages = kickGate.Kept
	if notifyProviderBudget {
		notifier.Send("Provider spending limit reached", providerBudgetCause, notify.PriorityHigh)
	}

	// Kick dispatch lives behind a seam (#7232): the skip rules and the
	// single-probe rule are the decisions worth testing, and they were
	// previously unreachable without a tmux session and a live governor.
	var deliveredReviewKicks []review.DispatchKick
	if len(messages) > 0 {
		dispatchAgentKicks(messages, releaseProviderBudgetProbe, kickDispatchDeps{
			backoffRemaining: agentMgr.ProviderErrorBackoffRemaining,
			sendKick:         agentMgr.SendKick,
			startKickSpan: func(agentName string) func(error) {
				agentCfg := cfg.Agents[agentName]
				_, kickSpan := tracing.StartSpan(ctx, "agent.kick", tracing.AgentKickAttributes(
					agentName,
					agentCfg.Backend,
					agentCfg.Model,
					agentCfg.Role,
					string(govState.Mode),
					inferACMMLevel(cfg),
				)...)
				return func(err error) {
					if err != nil {
						kickSpan.RecordError(err)
					}
					kickSpan.End()
				}
			},
			onReviewDelivered: func(msg scheduler.KickMessage) {
				if k, ok := reviewKickByMessage[msg.Agent+"\x00"+msg.Message]; ok {
					deliveredReviewKicks = append(deliveredReviewKicks, k)
					persistReviewDispatchState(reviewPlan, deliveredReviewKicks, logger)
				}
			},
			onDelivered: func(msg scheduler.KickMessage) {
				gov.RecordKickForRepo(msg.Agent, msg.Repo)
				dashSrv.AuditLog("governor", "kick", "trigger=governor-eval", msg.Agent)

				// Record issue-scoped kicks into the lifecycle timeline. Cheap,
				// guarded, and nil-safe (Record no-ops on a nil dashboard/store).
				recordKick(ctx, dashSrv, msg.Agent, msg.IssueRefs...)

				// Log token state at time of kick for cost attribution
				if tokenCollector != nil {
					if summary := tokenCollector.Summary(); summary != nil {
						agentTokens := summary.ByAgent[msg.Agent]
						logger.Info("kick token snapshot",
							"agent", msg.Agent,
							"agent_tokens", agentTokens,
							"total_tokens", summary.TotalTokens,
							"total_sessions", summary.SessionCount,
						)
					}
				}
			},
			markProbeReleased: providerBudgetProbe.MarkReleased,
			now:               time.Now,
		}, logger)
	}
	persistReviewDispatchState(reviewPlan, deliveredReviewKicks, logger)

	if actionable.Issues.SLAViolations > 0 {
		toNotify, capped := selectSLABreachNotifications(actionable.Issues.Items)
		for _, issue := range toNotify {
			notifier.Send(
				"SLA 2x breach",
				fmt.Sprintf("%s age %dm: %s\n%s", actionableIssueRef(issue), issue.AgeMinutes, issue.Title, issue.URL),
				notify.PriorityHigh,
			)
		}
		if capped {
			logger.Info("SLA notification cap reached, skipping remaining", "remaining", actionable.Issues.SLAViolations-len(toNotify))
		}
	}

	// Scan agent panes for login-required patterns and pause + notify if detected
	loginscan.Scan(ctx, cfg, agentMgr, notifier, dashSrv, logger, loginSightings)

	// Epoch captured before reading agent/governor state so a mutation that
	// lands mid-build (restart-count/budget reset) drops this snapshot instead
	// of letting it revert the mutation on the dashboard (#4348).
	buildEpoch := dashSrv.BeginStatusSnapshot()
	agentStatuses := agentMgr.AllStatuses()

	statusPayload := dashboard.BuildFrontendStatus(
		govState,
		actionable,
		agentStatuses,
		cfg,
		tokenCollector,
		gov,
		beadStores,
		ghClient,
		ctx,
		metricsCollector,
	)
	// Green on a PR pill means "the sweep would merge this now" — the
	// verdict writeMergeEligible reached for this same actionable set a
	// few lines up, not a re-derivation from mergeable_state (#7478).
	dashboard.AttachMergeVerdicts(statusPayload, mergeVerdicts)
	// A PR pill also carries the hive's OWN review on that PR, so the queue
	// view shows where the hive has already spoken. Read from the durable
	// ledger, so this costs no GitHub call per PR; a missing or unreadable
	// ledger simply means no review pills this cycle.
	attachReviewLinksForDashboard(statusPayload, logger)
	statusPublished := false
	// Ingest any JSONL findings agents wrote and persist them as beads.
	if advisoryStore != nil {
		findings, err := advisoryStore.ReadNewFindings()
		if err != nil {
			logger.Warn("failed to read advisory findings", "error", err)
		} else if len(findings) > 0 {
			// The canary gate's decisions live behind a seam (#7232); only the
			// effects — audit entry, critical bead — are supplied here.
			var scanCanary func(agent, reportText, source string) (ioscan.CanaryLeak, bool)
			if cfg.Ioscan.IsEnabled() && cfg.Ioscan.CanariesEnabled() {
				scanCanary = ioscan.DefaultCanaries.Scan
			}
			safeFindings := gateAdvisoryFindings(findings, advisoryIngestDeps{
				scanCanary: scanCanary,
				failClosed: cfg.Ioscan.FailClosed(),
				auditLog:   dashSrv.AuditLog,
				recordLeakBead: func(leak ioscan.CanaryLeak) {
					if store, ok := beadStores[leak.Agent]; ok && store != nil {
						if b, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
							_ = store.SetMetadata(b.ID, "rule", ioscan.CanaryLeakRule)
							_ = store.SetMetadata(b.ID, "source", leak.Source)
						}
					}
				},
			}, logger)
			if persisted := advisory.PersistAsBeads(safeFindings, beadStores); persisted > 0 {
				logger.Info("advisory findings persisted as beads", "count", persisted)
			}
		}
	}

	// Reload bead stores from disk before building the digest. Agents write
	// beads via the bd CLI which persists directly to disk, so the in-memory
	// stores can become stale between eval cycles. Reload failures are deduped
	// (WARN once per distinct error, then DEBUG) — see beads_reload.go (#5505).
	reloadBeadStores(beadStores, logger)

	// Phase 4 Part B: `plan`/`epic` label trigger. An actionable issue carrying a
	// plan label auto-mints an epic and requests decomposition — the same flow as
	// the dashboard "Plan this issue" click, but triggered by the label. It runs
	// AFTER the store reload so FindByExternalRef sees current state (idempotent:
	// no duplicate epic if one already exists). Cheap, synchronous, adds NO
	// goroutine, and drives the architect only via SendKick (never the launch
	// path). Gated by config/ACMM so low-maturity hives stay advisory-only.
	if acmmLvl := inferACMMLevel(cfg); planning.PlanningAllowedAtLevel(acmmLvl) &&
		approvalDeskAllowsLegacyOperation(ctx, approvalDesk, cfg, toolapprove.KindPlanFromLabel, "plan-from-label", planning.ArchitectAgentName, logger) {
		planFromLabeledIssues(actionable, beadStores, agentMgr, gov, dashSrv, logger, cfg, acmmLvl)
	}

	// Advisory digest: build from beads (the source of truth) before status broadcast.
	primaryRepo := primaryAdvisoryRepo(cfg)
	issueNum, hasPinnedAdvisoryIssue := advisoryIssueNumber(advisoryIssues, primaryRepo)
	hasExistingPinnedIssueForEmptyDigest := hasPinnedAdvisoryIssue &&
		primaryRepo == primaryRepoAtCycleStart &&
		hadPinnedAdvisoryIssueAtCycleStart
	if shouldBuildAdvisoryDigest(beadStores, ghClient, hasExistingPinnedIssueForEmptyDigest) {
		// Mark findings no agent has re-reported inside the staleness window
		// BEFORE the digest is built, so stale evidence is captioned. Silence is
		// not proof a finding healed: an agent run can be partial, truncated, or
		// non-deterministic, so absence cannot move an item to Recently Resolved.
		advCfg := cfg.Governor.Advisory
		if advCfg.StalenessDays > 0 {
			if marked := advisory.MarkStaleAdvisoryBeads(beadStores, time.Duration(advCfg.StalenessDays)*24*time.Hour); len(marked) > 0 {
				logger.Info("marked advisory findings not re-reported within the staleness window",
					"count", len(marked), "staleness_days", advCfg.StalenessDays, "titles", strings.Join(marked, "; "))
			}
		}
		// Repo entries may be org-qualified ("org/repo"); the digest linkifier
		// needs the bare repo name alongside the org.
		org, repoName := cfg.Project.Org, primaryRepo
		if parts := strings.SplitN(primaryRepo, "/", 2); len(parts) == 2 {
			org, repoName = parts[0], parts[1]
		}

		// #3704: pin the digest to ONE repo commit. Resolve the target repo's
		// latest commit ONCE here (invariants 1 & 3), cite it in the rendered
		// comment (invariant 2, via the footer FormatDigestMarkdown emits when
		// AnalyzedSnapshot is set), and verify each finding's file path against
		// that exact commit so a since-removed path (e.g. "docs/install.md") is
		// flagged as outdated rather than cited as live. Best-effort: if the SHA
		// cannot be resolved, fall back to the previous unpinned behavior rather
		// than skip the digest.
		//
		// This is resolved BEFORE the digest is built because the top-N cap
		// consumes it: ranking cannot prefer a live finding over a since-removed
		// one unless it knows which is which at ranking time (#2364).
		digestOpts := advisory.DigestOptions{
			MaxFindings: advCfg.MaxFindings,
			ShowAll:     advCfg.ShowAll,
		}
		if ghClient != nil {
			pinDigestSnapshot(ctx, &digestOpts, org, repoName, primaryRepo, cfg.Policies.Branch, digestSnapshotDeps{
				defaultBranch: func(ctx context.Context, owner, repo string) (string, error) {
					r, _, err := ghClient.GetRepo(ctx, owner, repo)
					if err != nil {
						return "", err
					}
					return r.GetDefaultBranch(), nil
				},
				latestCommit:  ghClient.LatestCommitHash,
				pathExists:    ghClient.PathExistsAtRef,
				issueClosedAt: ghClient.IssueClosedAt,
			}, logger)
		}
		digest := advisory.BuildDigestFromBeads(beadStores, string(govState.Mode), digestOpts)
		enrichAdvisoryLinkedWork(ctx, ghClient, digest, org, repoName, logger)
		if advisoryStore != nil {
			advisoryStore.SetLatestDigest(digest)
		}
		dashSrv.SetAdvisoryDigest(digest)
		statusPayload.AdvisoryDigest = digest
		statusPublished = dashSrv.UpdateStatusIfFresh(statusPayload, buildEpoch)
		if !statusPublished {
			return
		}
		hiveAdvice := statusPayload.HiveAdvice

		// Post whenever there is something CURRENT to say: open findings,
		// recently resolved ones, or an empty evaluation for a hive that already
		// has a pinned advisory issue. The resolved and empty cases matter for
		// freshness: otherwise the pinned comment and AdvisoryLastPostedAt
		// freeze after the last finding disappears, and the hub reports a stale
		// advisory loop even though the agents are running cleanly.
		//
		// advisoryPostDue additionally paces the GitHub write to the
		// operator's governor.advisory.update_interval_s (#4820); 0/unset
		// keeps this exact per-cycle cadence. cfg is read live each cycle —
		// the same pattern as the staleness/max-findings knobs above — so a
		// dashboard edit applies from the next cycle without a restart. The
		// digest itself and the dashboard state above still refresh every
		// cycle; only the comment write is throttled. Note the #4821
		// write-through counts consecutive unchanged post ATTEMPTS, so its
		// forced full rewrite stretches with this interval (60 attempts ×
		// interval) — acceptable, since it only heals out-of-band comment
		// edits, and documented in the settings tooltip.
		// governor.advisory.target routes the comment write: GitHub (default,
		// the unchanged path below) or a designated Linear issue. For the
		// Linear route the configured issue plays the pinned issue's role in
		// the empty-digest freshness rule, so a clean Linear-sourced hive
		// keeps refreshing its comment exactly as a GitHub one does.
		advisoryTarget, advisoryLinearIssue, advisoryRouteErr := resolveAdvisoryDigestRoute(cfg)
		hasDigestHome := hasExistingPinnedIssueForEmptyDigest ||
			(advisoryTarget == config.AdvisoryTargetLinear && advisoryRouteErr == nil)
		if shouldPostAdvisoryDigest(digest, ghClient, hasDigestHome) &&
			advisoryPostDue(advCfg, primaryRepo, time.Now(), logger) {
			bySeverity, agentNames := summarizeDigestForLog(digest)
			logger.Info("advisory digest built",
				"total_findings", digest.TotalCount,
				"critical", bySeverity["critical"],
				"high", bySeverity["high"],
				"medium", bySeverity["medium"],
				"low", bySeverity["low"],
				"agents", agentNames,
				"resolved_count", len(digest.RecentlyResolved),
			)
			if digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0 {
				logger.Info("advisory digest empty — posting freshness marker",
					"repo", primaryRepo, "issue", issueNum)
			}

			md := advisory.FormatDigestMarkdown(digest, advisory.DigestOptions{
				MaxFindings: digestOpts.MaxFindings,
				ShowAll:     digestOpts.ShowAll,
				Org:         org,
				ShowEmpty:   digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0,
				PrimaryRepo: repoName,
				Advice:      hiveAdvice,
			})
			// The routing/classification decisions live in
			// publishAdvisoryDigest (#7232); only the effects are wired here.
			// The App is the sole advisory-digest writer (#1927): a user-token
			// failure must never drive the App banner, so every banner hook
			// below is fed exclusively by the App's own error.
			publishAdvisoryDigest(ctx, md, digest, advisoryPublishRoute{
				target:         advisoryTarget,
				linearIssue:    advisoryLinearIssue,
				routeErr:       advisoryRouteErr,
				primaryRepo:    primaryRepo,
				issueNum:       issueNum,
				hasPinnedIssue: hasPinnedAdvisoryIssue,
				ensureErr:      advisoryEnsureErr,
			}, advisoryPublishDeps{
				postGitHub: ghClient.PostAdvisoryDigest,
				postLinear: func(ctx context.Context, linearIssue, md string) error {
					return postAdvisoryDigestToLinear(ctx, cfg, linearIssue, md)
				},
				recordError: dashSrv.RecordAdvisoryError,
				recordPosted: func(findings, overflow int) {
					dashSrv.RecordAdvisoryPost(findings)
					recordAdvisoryPostSuccess(primaryRepo, time.Now())
					dashSrv.RecordAdvisoryOverflow(overflow)
				},
				onWriteForbidden: func(ctx context.Context) {
					// App is installed (we found the issue) but a real WRITE
					// was forbidden. #2353: attribute this honestly — surface
					// a DISTINCT write-forbidden state naming the likeliest
					// cause instead of faking a permission gap the diagnosis
					// just disproved.
					msg, state := classifyGitHubAppWriteForbidden(ctx, ghClient.AppAuth(), cfg.Project.Org, primaryRepo)
					dashSrv.SetGitHubAppRequired(true)
					dashSrv.SetGitHubAppPermIssue(msg)
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("GitHub App write failed — cannot write issue comments",
						"repo", primaryRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable(), "detail", msg)
				},
				onAuthFailure: func(ctx context.Context) {
					// Same verdict function as boot and Re-check, so a healthy
					// or unclassifiable probe cannot raise the banner here.
					raise, msg, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger)
					if !raise {
						return
					}
					dashSrv.SetGitHubAppRequired(true)
					if msg != "" {
						dashSrv.SetGitHubAppPermIssue(msg)
					}
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("GitHub App authentication failed posting advisory digest",
						"repo", primaryRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable())
				},
				onWriteProven: func(ctx context.Context) {
					// A successful write proves the app is installed AND has
					// write access — clear BOTH the perm issue and the
					// app-required flag, or the "Not Installed" banner sticks
					// forever despite tokens working.
					dashSrv.SetGitHubAppPermIssue("")
					dashSrv.SetGitHubAppRequired(false)
					dashSrv.ClearPendingGitHubAppInstall()
					// The same proof retires stale ACCESS findings (#2575).
					if healed := advisory.CloseHealedAppAuthFindings(beadStores); len(healed) > 0 {
						logger.Info("closed healed GitHub App access findings after successful App digest post",
							"count", len(healed), "titles", strings.Join(healed, "; "))
					}
					// Repo-ACCESS findings need their own proof: a digest post
					// only proves issues:WRITE, so verify with a real
					// advisor-scoped Contents read, memoized per repo.
					readVerified := map[string]bool{}
					canRead := func(ownerRepo string) bool {
						target := ownerRepo
						if target == "" {
							target = primaryRepo
						}
						owner, name := cfg.Project.Org, target
						if i := strings.LastIndex(target, "/"); i > 0 {
							owner, name = target[:i], target[i+1:]
						}
						if owner == "" || name == "" {
							return false
						}
						key := owner + "/" + name
						if v, ok := readVerified[key]; ok {
							return v
						}
						appAuth := ghClient.AppAuth()
						if appAuth == nil {
							// Static-token client: no advisor-tier token can be
							// minted, so leave the finding open.
							return false
						}
						err := appAuth.VerifyRepoRead(ctx, owner, name)
						if err != nil {
							logger.Info("repo-access finding left open: advisor read probe failed",
								"repo", key, "error", err)
						}
						readVerified[key] = err == nil
						return readVerified[key]
					}
					if healed := advisory.CloseHealedRepoAccessFindings(beadStores, canRead); len(healed) > 0 {
						logger.Info("closed healed repo-access findings after verified advisory read path",
							"count", len(healed), "titles", strings.Join(healed, "; "))
					}
				},
			}, logger)
		}
	} else if d := dashSrv.GetAdvisoryDigest(); d != nil {
		statusPayload.AdvisoryDigest = d
	}

	if !statusPublished {
		dashSrv.UpdateStatusIfFresh(statusPayload, buildEpoch)
	}

	publishFleetReports(ctx, logger, ghClient, dashSrv, statusPayload.FleetReport, cfg.Governor.FleetReport.DryRun())

	if agentStats := dashboard.CollectAgentStats(statusPayload); len(agentStats) > 0 {
		gov.AttachAgentStats(agentStats)
	}

	if repoSnaps := dashboard.CollectRepoSnapshots(statusPayload); len(repoSnaps) > 0 {
		gov.AttachRepoSnapshots(repoSnaps)
	}

	if nousState != nil {
		var tokenSummary *tokens.AggregateSummary
		if tokenCollector != nil {
			tokenSummary = tokenCollector.Summary()
		}
		if err := nousState.RecordSnapshot(govState, actionable, agentsDue, agentStatuses, tokenSummary); err != nil {
			logger.Warn("failed to record nous snapshot", "error", err)
		}
	}
}

func convertKnowledgeLayers(cfgLayers []config.KnowledgeLayer) []knowledge.LayerConfig {
	layers := make([]knowledge.LayerConfig, len(cfgLayers))
	for i, l := range cfgLayers {
		layers[i] = knowledge.LayerConfig{
			Type:   knowledge.LayerType(l.Type),
			Path:   l.Path,
			URL:    l.URL,
			Shared: l.Shared,
		}
	}
	return layers
}

// curatorConfigFromHive maps the hive.yaml curator block onto the knowledge
// package's own config. Enabled is carried across as a pointer so "absent"
// stays distinguishable from "explicitly false" — the scheduled promotion loop
// treats absent as OFF, and flattening it to a bool here would quietly turn
// unreviewed promotion on fleet-wide (#5430).
func curatorConfigFromHive(c config.KnowledgeCurator) knowledge.CuratorConfig {
	return knowledge.CuratorConfig{
		Enabled:              c.Enabled,
		Schedule:             c.Schedule,
		ExtractFrom:          c.ExtractFrom,
		AutoPromoteThreshold: c.AutoPromoteThreshold,
		PromoteFrom:          c.PromoteFrom,
		PromoteTo:            c.PromoteTo,
	}
}

// hiveIDFilePath is the persistent file where the Hive ID is stored across restarts.
var hiveIDFilePath = "/data/hive-id"

func loadOrGenerateHiveID(logger *slog.Logger) string {
	if envID := os.Getenv("HIVE_ID"); envID != "" {
		if err := os.WriteFile(hiveIDFilePath, []byte(envID+"\n"), 0o644); err == nil {
			logger.Info("hive ID set from HIVE_ID env var", "id", envID)
		}
		return envID
	}

	if data, err := os.ReadFile(hiveIDFilePath); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			logger.Info("hive ID loaded from disk", "id", id)
			return id
		}
	}

	id := "hive-" + randomName()

	if err := os.WriteFile(hiveIDFilePath, []byte(id+"\n"), 0o644); err != nil {
		logger.Warn("failed to persist hive ID", "error", err)
	} else {
		logger.Info("generated new hive ID", "id", id)
	}

	return id
}

// randomName generates a Docker-style adjective-noun name.
func randomName() string {
	adjectives := []string{
		"bold", "calm", "cool", "dark", "deep", "fair", "fast", "keen",
		"kind", "loud", "mild", "neat", "pale", "pure", "rare", "rich",
		"safe", "slim", "soft", "tall", "thin", "true", "vast", "warm",
		"wise", "able", "busy", "easy", "epic", "free", "glad", "good",
		"idle", "just", "lazy", "lean", "live", "long", "lost", "main",
		"next", "open", "real", "sure", "wild", "worn", "zero", "blue",
	}
	nouns := []string{
		"ant", "ape", "bat", "bee", "cow", "doe", "eel", "elk",
		"fox", "gnu", "hen", "jay", "kit", "lark", "moth", "newt",
		"owl", "pug", "ram", "ray", "seal", "swan", "toad", "wren",
		"bear", "colt", "crow", "deer", "dove", "duck", "fawn", "frog",
		"goat", "gull", "hare", "hawk", "ibis", "lynx", "mink", "mole",
		"orca", "pike", "puma", "slug", "stag", "wolf", "yak", "wasp",
	}

	buf := make([]byte, 2)
	if _, err := rand.Read(buf); err != nil {
		return "bold-ant"
	}
	adj := adjectives[int(buf[0])%len(adjectives)]
	noun := nouns[int(buf[1])%len(nouns)]
	return adj + "-" + noun
}

// watchdogAuthProbes builds the per-provider credential probes for the
// watchdog by adapting the rotation package's provider probers (#4608) —
// the same machinery the #4645 probe rewrite targets, so that rewrite reaches
// the watchdog automatically.
func watchdogAuthProbes(cfg *config.Config) map[string]watchdog.AuthProbe {
	threshold := cfg.Governor.Rotation.EffectiveThreshold()
	probers := []rotation.Prober{
		rotation.ClaudeProber{ThresholdPct: threshold},
		rotation.CodexProber{ThresholdPct: threshold},
		rotation.AgyProber{ThresholdPct: threshold},
		rotation.DeepSeekProber{},
	}
	out := make(map[string]watchdog.AuthProbe, len(probers))
	for _, p := range probers {
		out[p.Provider()] = watchdog.RotationAuthProbe{Prober: p}
	}
	return out
}

// turnLossToSnapshot converts the manager's in-memory turn-loss accumulation
// into its persisted form, or nil when nothing has been recorded.
//
// Nil rather than a zero struct on purpose: `turn_loss` is omitempty, so an
// agent that has never been interrupted adds nothing to /data/hive-state.json.
// The overwhelming majority of agents are in that state, and a measurement that
// bloated every hive's state file with empty records would be its own argument
// for removing it.
func turnLossToSnapshot(loss agent.TurnLoss) *snapshot.AgentTurnLoss {
	if loss.Interruptions == 0 && len(loss.Recent) == 0 {
		return nil
	}
	out := &snapshot.AgentTurnLoss{
		Interruptions: loss.Interruptions,
		Producing:     loss.Producing,
		UpperBoundS:   loss.UpperBound.Seconds(),
		Bytes:         loss.Bytes,
	}
	for _, r := range loss.Recent {
		rec := snapshot.AgentTurnInterruption{
			At:         r.At,
			Reason:     r.Reason,
			SinceKickS: r.SinceKick.Seconds(),
			Producing:  r.Producing,
			Bytes:      r.Bytes,
		}
		if r.SinceOutput != nil {
			s := r.SinceOutput.Seconds()
			rec.SinceOutputS = &s
		}
		out.Recent = append(out.Recent, rec)
	}
	return out
}

func restartEventsToSnapshot(events []agent.RestartEvent) []snapshot.AgentRestartEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]snapshot.AgentRestartEvent, 0, len(events))
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, ev := range events {
		if ev.At.IsZero() || ev.At.Before(cutoff) {
			continue
		}
		out = append(out, snapshot.AgentRestartEvent{At: ev.At, Reason: ev.Reason})
	}
	return out
}

func restartEventsFromSnapshot(events []snapshot.AgentRestartEvent) []agent.RestartEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]agent.RestartEvent, 0, len(events))
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, ev := range events {
		if ev.At.IsZero() || ev.At.Before(cutoff) {
			continue
		}
		out = append(out, agent.RestartEvent{At: ev.At, Reason: ev.Reason})
	}
	return out
}

// persistPathsForRuntime resolves the file set persistState writes. It is a
// package var (the same seam pattern as githubAppTokenCachePath) rather than a
// direct defaultPersistPaths() call so the wrapper's delegation can be pinned
// by a test without writing the live /data files the default set hardwires
// (#6846). persistState is synchronous, so a test that swaps and restores
// this var cannot race a goroutine still reading it.
var persistPathsForRuntime = defaultPersistPaths

func persistState(agentMgr *agent.Manager, gov *governor.Governor, cfg *config.Config, path string, logger *slog.Logger, dashSrv *dashboard.Server, wd *watchdog.Reconciler) {
	persistStateWithPaths(agentMgr, gov, cfg, path, logger, dashSrv, wd, persistPathsForRuntime())
}

func persistStateWithPaths(agentMgr *agent.Manager, gov *governor.Governor, cfg *config.Config, path string, logger *slog.Logger, dashSrv *dashboard.Server, wd *watchdog.Reconciler, paths persistPaths) {
	statuses := agentMgr.AllStatuses()
	agents := make(map[string]snapshot.AgentState, len(statuses))
	for name, proc := range statuses {
		as := snapshot.AgentState{
			Paused:            proc.Paused,
			PinnedCLI:         proc.PinnedCLI,
			PinnedModel:       proc.PinnedModel,
			ModelOverride:     proc.ModelOverride,
			BackendOverride:   proc.BackendOverride,
			RestartCount:      proc.RestartCount,
			RestartEvents:     restartEventsToSnapshot(proc.RestartEvents),
			LastRestartReason: proc.LastRestartReason,
			LastKick:          proc.LastKick,
			PausedReason:      proc.PausedReason,
			PausedTrigger:     proc.PausedTrigger,
			PausedBy:          proc.PausedBy,
			TurnLoss:          turnLossToSnapshot(proc.TurnLoss),
		}
		if !proc.PausedAt.IsZero() {
			t := proc.PausedAt
			as.PausedAt = &t
		}
		if len(proc.KickHistory) > 0 {
			as.KickHistory = make([]snapshot.AgentKickEntry, len(proc.KickHistory))
			for i, kr := range proc.KickHistory {
				as.KickHistory[i] = snapshot.AgentKickEntry{
					Timestamp: kr.Timestamp,
					Agent:     kr.Agent,
					Snippet:   kr.Snippet,
				}
			}
		}
		if agentCfg, ok := cfg.Agents[name]; ok {
			as.DisplayName = agentCfg.DisplayName
			as.Description = agentCfg.Description
			enabled := agentCfg.Enabled
			as.Enabled = &enabled
			clearOnKick := agentCfg.ClearOnKick
			as.ClearOnKick = &clearOnKick
			staleTimeout := agentCfg.StaleTimeout
			as.StaleTimeout = &staleTimeout
			as.RestartStrategy = agentCfg.RestartStrategy
			as.LaunchCmd = agentCfg.LaunchCmd
		}
		agents[name] = as
	}

	cadenceOverrides := make(map[string]map[string]config.Cadence)
	for modeName, mode := range cfg.Governor.Modes {
		if len(mode.Cadences) > 0 {
			cadenceOverrides[modeName] = make(map[string]config.Cadence, len(mode.Cadences))
			for agentName, cadence := range mode.Cadences {
				cadenceOverrides[modeName][agentName] = cadence
			}
		}
	}

	budget := gov.GetBudget()
	govState := gov.GetState()

	govKickHistory := gov.KickHistory()
	kickEntries := make([]snapshot.GovKickEntry, len(govKickHistory))
	for i, kr := range govKickHistory {
		kickEntries[i] = snapshot.GovKickEntry{Timestamp: kr.Timestamp, Agent: kr.Agent, Outcome: kr.Outcome, OutcomeReason: kr.OutcomeReason}
	}

	state := &snapshot.PersistedState{
		Agents:               agents,
		GovernorMode:         string(govState.Mode),
		BudgetLimit:          budget.WeeklyLimit,
		BudgetIgnored:        budget.IgnoredAgents,
		BudgetIgnoreAll:      budget.IgnoreAll,
		CadenceOverrides:     cadenceOverrides,
		LastKicks:            govState.LastKick,
		BudgetSpend:          budget.CurrentSpend,
		BudgetResetAt:        budget.ResetAt,
		BudgetByAgent:        budget.ByAgent,
		BudgetByModel:        budget.ByModel,
		BudgetWindowBaseline: budget.WindowBaseline,
		KickHistory:          kickEntries,
		LastEval:             govState.LastEval,
		ACMMLevel:            cfg.ACMMLevel,
	}

	// Persist the fleet breaker so an engaged kill-switch survives a restart.
	// Only written when engaged — a never-thrown breaker adds nothing.
	if engaged, breakerPaused := agentMgr.BreakerState(); engaged {
		state.Breaker = &snapshot.BreakerState{Engaged: true, Paused: breakerPaused}
	}

	// Persist the watchdog's backoff/crash-loop/condition state (RFC #4665
	// open question 2: it rides the existing state file).
	if wd != nil {
		if wdState := wd.Snapshot(); len(wdState) > 0 {
			state.Watchdog = wdState
		}
	}

	if err := snapshot.SaveState(path, state, logger); err != nil {
		logger.Error("failed to persist state", "error", err)
	}

	// Component reach counters (#3993) ride the SAME save cadence as the main
	// state file but live in their own file (reachStatePath — resolved OQ-2 of
	// #3973), so a reach write failure never corrupts agent/governor state.
	if err := tracing.SaveReachState(paths.ReachState); err != nil {
		logger.Error("failed to persist reach state", "error", err)
	}

	// Reconcile the persisted pause field from the authoritative live manager
	// state and save, atomically under the config save mutex. persistState runs
	// async (go PersistFunc()) on every pause/resume; doing the c.Agents update
	// and Save under saveMu (via ReconcilePausedAndSave) means it can neither
	// race the pause callback's map write nor clobber its file write with a
	// stale paused=false. livePaused is built from AllStatuses(), read above.
	livePaused := make(map[string]bool, len(agents))
	for name, as := range agents {
		livePaused[name] = as.Paused
	}
	if err := cfg.ReconcilePausedAndSave(livePaused); err != nil {
		logger.Error("failed to persist config to yaml", "error", err)
		if dashSrv != nil {
			dashSrv.AddSystemAlert("config-save-failed", "error",
				"Config save failed — runtime state (ACMM level, agent config) will be lost on restart: "+err.Error())
		}
	} else if dashSrv != nil {
		dashSrv.ClearSystemAlert("config-save-failed")
	}

	history := gov.EvalHistory()
	if len(history) > 0 {
		historyData, err := json.Marshal(history)
		if err == nil {
			atomicWrite(paths.SparklineHistory, historyData)
		}
	}

	modeHistory := gov.ModeHistory()
	if len(modeHistory) > 0 {
		modeData, err := json.Marshal(modeHistory)
		if err == nil {
			atomicWrite(paths.ModeHistory, modeData)
		}
	}

	if dashSrv != nil {
		tokenHistory := dashSrv.TokenSparklineHistory()
		if len(tokenHistory) > 0 {
			tokenData, err := json.Marshal(tokenHistory)
			if err == nil {
				atomicWrite(paths.TokenSparklineHistory, tokenData)
			}
		}

		factHist := dashSrv.FactHistory()
		if len(factHist) > 0 {
			factData, err := json.Marshal(factHist)
			if err == nil {
				atomicWrite(paths.FactHistory, factData)
			}
		}

		costHist := dashSrv.CostHistory()
		if len(costHist) > 0 {
			costData, err := json.Marshal(costHist)
			if err == nil {
				atomicWrite(paths.CostHistory, costData)
			}
		}

		trendHist := dashSrv.TrendHistory()
		if len(trendHist) > 0 {
			trendData, err := json.Marshal(trendHist)
			if err == nil {
				atomicWrite(paths.TrendHistory, trendData)
			}
		}

		// #4298: per-budget-window history. Written on the same cadence as the
		// other series so a pod roll cannot lose more of one than the others.
		budgetHist := dashSrv.BudgetWindowHistory()
		if len(budgetHist) > 0 {
			budgetData, err := json.Marshal(budgetHist)
			if err == nil {
				atomicWrite(paths.BudgetWindowHistory, budgetData)
			}
		}

		// #4263: convergence soak telemetry, written atomically on the same
		// cadence as the other series so a pod roll cannot lose more of one
		// than the others.
		soakHist := dashSrv.ConvergenceSoakHistory()
		if len(soakHist) > 0 {
			soakData, err := json.Marshal(soakHist)
			if err == nil {
				atomicWrite(paths.ConvergenceSoak, soakData)
			}
		}
	}
}

var (
	escalationStoreOnce sync.Once
	escalationStore     *escalation.Store
)

const escalationLedgerPath = "/data/metrics/fix-streaks.json"

// getEscalationStore lazily loads the shared fix-loop ledger (staleness clock +
// distinct-SHA attempt counts + re-engagement caps). It is the SINGLE
// loop-safety/dedup authority shared by the re-engagement paths (#2 merge
// watcher, #3 claim release, #4 reaper) and the human-escalation sweep, so they
// all agree on which red PRs are stale, how many times each has been
// re-nudged, and which have crossed the human-escalation threshold.
func getEscalationStore() *escalation.Store {
	escalationStoreOnce.Do(func() {
		escalationStore = escalation.Load(escalationLedgerPath)
	})
	return escalationStore
}

// hivePRObservations projects the enumerated hive-authored PRs into escalation
// observations (repo fully-qualified, Red == a required check failed). Shared by
// recordRedStaleness and the reaper so both classify PRs identically. A PR is
// "red" here strictly per HasFailingRequiredCheck — GENERIC check state, never a
// specific linter or language.
func hivePRObservations(cfg *config.Config, actionable *github.ActionableResult) []escalation.Observation {
	if actionable == nil {
		return nil
	}
	var obs []escalation.Observation
	for _, pr := range actionable.PRs.Items {
		if !isHiveAgentAuthor(cfg, pr.Author) {
			continue
		}
		obs = append(obs, escalationObservation(cfg, pr))
	}
	return obs
}

// escalationObservation projects one enumerated PR into the fix-loop ledger's
// view of it. Red means a required check concluded failure. Pending means this
// pass could not conclude CI at all — checks still running, no check runs, or
// (per EnrichCIStatus) the check-run fetch errored — which the ledger must
// treat as "no information", never as "went green". Labeled mirrors the forge's
// needs-human label so the ledger and the label can never disagree about
// whether a PR has already been handed to a human. Labels carry the PR's
// current forge labels so Sweep can reconcile reviewer-lane verdicts
// (label-only edits: needs-human removed, reviewer-passed added) back into
// the ledger (#5511, gap G1).
func escalationObservation(cfg *config.Config, pr github.PullRequest) escalation.Observation {
	repo := pr.Repo
	if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
		repo = cfg.Project.Org + "/" + repo
	}
	red := pr.HasFailingRequiredCheck()
	return escalation.Observation{
		Repo:    repo,
		Number:  pr.Number,
		HeadSHA: pr.HeadSHA,
		Red:     red,
		Pending: !red && pr.CIStatus != "success",
		Labeled: escalation.HasNeedsHumanLabel(pr.Labels),
		Excerpt: pr.CIFailureExcerpt,
		Labels:  pr.Labels,
	}
}

// dependencyBots are forge bots whose PRs are dependency bumps, not hive fix
// attempts. They carry the "[bot]" suffix that otherwise marks a PR as
// agent-authored, but nothing in the hive opened them and no hive agent is
// iterating on them, so a red one is not a fix loop to break: escalating it
// only pages a human with "1 distinct fix attempts" about a crate bump that
// renovate will rebase on its own. Mirrors the hub's default contribute
// deny-authors list (config: hub.contribute_deny_authors).
var dependencyBots = map[string]bool{
	"renovate[bot]":    true,
	"dependabot[bot]":  true,
	"mergeraptor[bot]": true,
}

// isHiveAgentAuthor reports whether a PR author is one of OUR agents — the
// configured ai_author, the App bot identity agents author as, or another
// bot account — excluding the dependency bots above. Shared by every fix-loop
// path (staleness clock, reaper, escalation sweep) so they classify PRs
// identically.
func isHiveAgentAuthor(cfg *config.Config, author string) bool {
	if author == "" {
		return false
	}
	if author == cfg.Project.AIAuthor {
		return true
	}
	if eff := cfg.EffectiveAIAuthor(); eff != "" && author == eff {
		return true
	}
	return strings.HasSuffix(author, "[bot]") && !dependencyBots[author]
}

// recordRedStaleness updates the shared staleness clock (first-seen-red per red
// head SHA) for every hive-authored PR in this pass. It must run before the
// claim guard and the reaper so their StaleRed() reads reflect the current tick.
// A disabled escalation subsystem skips it (the whole fix-loop machinery is off).
func recordRedStaleness(cfg *config.Config, actionable *github.ActionableResult) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	obs := hivePRObservations(cfg, actionable)
	getEscalationStore().ObserveRed(obs)
	for _, ob := range obs {
		if !ob.Red {
			continue
		}
		hookDispatcher().Fire(context.Background(), hooks.Payload{
			Transition: hooks.TransitionEscalationRed,
			Repo:       ob.Repo,
			Reason:     "required CI check red",
			Attrs: map[string]string{
				hooks.AttrPR: strconv.Itoa(ob.Number),
				"head_sha":   ob.HeadSHA,
				"excerpt":    ob.Excerpt,
			},
		})
	}
}

// mergeReEngageHook builds the Fix #2 re-engagement callback for the merge
// watcher. It normalizes the repo, then records a re-engagement under the shared
// escalation store's per-red-SHA cap. Passing an empty head SHA tells the store
// to reuse the red head SHA it last observed for this PR (the eval cycle's
// ObserveRed keeps it current), so the merge watcher does not need to re-fetch
// the head. Returns false when the cap is exhausted, so the watcher can log that
// the escalation path now owns the PR. A disabled escalation subsystem yields a
// nil hook (watcher falls back to quarantine-only).
func mergeReEngageHook(cfg *config.Config) github.MergeReEngageFunc {
	if cfg.Escalation.Disabled {
		return nil
	}
	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	return func(repo string, number int) bool {
		// Empty head SHA: reuse the store's tracked current red SHA for this PR
		// (do not reset the cap counter). The eval cycle records it via
		// ObserveRed; if the store has never seen this PR red, TryReEngage still
		// allows the first MaxReEngagements nudges.
		return getEscalationStore().TryReEngage(fullRepo(repo), number, "")
	}
}

// reapStuckRedPRs is Fix #4: the governor's backstop sweep. For each
// hive-authored PR that is red on a required check AND stale (StaleRed) AND not
// already escalated to a human, it records a re-engagement (deduped + capped via
// the escalation store) and logs the fix dispatch. The PR is already present in
// ci-failing.json via writeMergeEligible, so recording the re-engagement is what
// guarantees a stale PR is treated as actionable rather than abandoned, while
// the cap prevents re-firing every tick on a permanently-red, never-moving head.
// Entirely generic: it keys only off check state + staleness, never a linter.
func reapStuckRedPRs(cfg *config.Config, actionable *github.ActionableResult, escalatedPRs map[string]bool, logger *slog.Logger) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	store := getEscalationStore()
	for _, o := range hivePRObservations(cfg, actionable) {
		if !o.Red {
			continue
		}
		key := escalation.Key(o.Repo, o.Number)
		if escalatedPRs[key] {
			// Already handed to a human (needs-human label); kick builders skip
			// it and we must not re-dispatch automated fixes.
			continue
		}
		if !store.StaleRed(o.Repo, o.Number, o.HeadSHA) {
			continue // still churning (fresh red SHA) — leave it to the fix agent
		}
		if !store.TryReEngage(o.Repo, o.Number, o.HeadSHA) {
			// Re-engagement cap reached for this red SHA: stop nudging. The
			// distinct-SHA escalation path owns it from here.
			continue
		}
		logger.Info("reaper: re-dispatching fix for stuck red PR",
			"repo", o.Repo, "pr", o.Number, "head_sha", o.HeadSHA,
			"re_engagements", store.ReEngagements(o.Repo, o.Number))
	}
}

// runEscalationSweep folds this enumeration pass into the fix-loop breaker
// ledger and fires the one-time escalation actions (evidence comment +
// needs-human label + ntfy) for any agent-authored PR that just crossed the
// threshold of distinct failed fix attempts. Returns the full set of
// escalated PR keys so the work-list writers can flag them. Deterministic by
// design: no agent judgment is involved in counting, evidence, or the
// stop-order. Human-authored PRs are never escalated.
//
// The two forge writes go through forge.IssueWriter rather than *github.Client
// so the evidence lands on whichever forge the hive is actually configured for
// (see governorForge in forgewire.go). On a GitHub hive the writer IS the
// *github.Client this used to take, so nothing about that path changed.
//
// rec receives a KindBlocked lifecycle event for each newly-escalated PR
// (#5656); a nil rec is a no-op, matching the other timeline producers.
func runEscalationSweep(
	ctx context.Context,
	cfg *config.Config,
	writer forge.IssueWriter,
	actionable *github.ActionableResult,
	notifier *notify.Notifier,
	rec lifecycleRecorder,
	logger *slog.Logger,
) map[string]bool {
	escalated := map[string]bool{}
	if cfg.Escalation.Disabled || writer == nil || actionable == nil {
		return escalated
	}
	getEscalationStore()

	var obs []escalation.Observation
	type prMeta struct{ checks []string }
	meta := map[string]prMeta{}
	for _, pr := range actionable.PRs.Items {
		if !isHiveAgentAuthor(cfg, pr.Author) {
			continue
		}
		o := escalationObservation(cfg, pr)
		obs = append(obs, o)
		meta[escalation.Key(o.Repo, o.Number)] = prMeta{checks: pr.FailingChecks}
	}
	results := escalationStore.Sweep(obs, cfg.Escalation.EffectiveThreshold())

	for _, o := range obs {
		key := escalation.Key(o.Repo, o.Number)
		r, ok := results[key]
		if !ok {
			continue
		}
		if r.Escalated {
			escalated[key] = true
		}
		if r.NeedsLabel && !o.Labeled {
			// Escalated on an earlier pass but the label never landed (the
			// AddLabels call failed). Retry the LABEL ONLY — the evidence
			// comment already reached the human and must not be repeated.
			if err := writer.AddLabels(ctx, o.Repo, o.Number, []string{escalation.NeedsHumanLabel}); err != nil {
				logger.Warn("escalation label retry failed", "repo", o.Repo, "pr", o.Number, "error", err)
			} else {
				escalationStore.MarkLabelApplied(o.Repo, o.Number)
			}
		}
		if !r.NewlyEscala {
			continue
		}
		escalated[key] = true
		excerpt := o.Excerpt
		if excerpt == "" {
			excerpt = escalationStore.Excerpt(o.Repo, o.Number)
		}
		// A PR that re-escalates AFTER a reviewer-lane pass reaches its human
		// with a structured hand-off note (#5617 item 3): what the reviewer
		// left on the branch, when, and that no second automated pass is
		// coming. Before this, the only thing distinguishing that hand-off
		// from a first escalation was the label set.
		body := escalation.CommentBody(r.Attempts, meta[key].checks, excerpt, r.Exhausted)
		afterReviewerPass := false
		if sha, at, ok := escalationStore.ReviewerPass(o.Repo, o.Number); ok {
			body = escalation.HandoffCommentBody(r.Attempts, meta[key].checks, excerpt, r.Exhausted,
				escalation.ReviewerHandoff{SHA: sha, At: at})
			afterReviewerPass = true
		}
		if err := writer.CreateIssueComment(ctx, o.Repo, o.Number, body); err != nil {
			// Retry next pass rather than marking escalated with no comment:
			// the whole point is that the evidence reaches a human.
			logger.Warn("escalation comment failed; will retry next pass",
				"repo", o.Repo, "pr", o.Number, "error", err)
			continue
		}
		// Mark escalated BEFORE the label call: once the comment is on the
		// PR, nothing may post it again, whatever happens to the label.
		escalationStore.MarkEscalated(o.Repo, o.Number)
		if err := writer.AddLabels(ctx, o.Repo, o.Number, []string{escalation.NeedsHumanLabel}); err != nil {
			logger.Warn("escalation label failed; will retry next pass", "repo", o.Repo, "pr", o.Number, "error", err)
		} else {
			escalationStore.MarkLabelApplied(o.Repo, o.Number)
		}
		// The escalation IS the real "blocked" lifecycle signal (#5656): a PR
		// out of automated fix attempts, handed to a human. Record it on the
		// item's journey so the panel's Blocked counter reflects reality, not
		// just hook annotations.
		recordBlocked(ctx, rec, cfg.Project.Org, o.Repo, o.Number, r.Attempts, meta[key].checks)
		logger.Info("fix loop escalated to human",
			"repo", o.Repo, "pr", o.Number, "attempts", r.Attempts,
			"failing_checks", strings.Join(meta[key].checks, ","))
		if notifier != nil {
			title := "Fix loop escalated"
			detail := fmt.Sprintf("%s#%d red on %d fix attempts — needs a human (see PR comment for the raw error)", o.Repo, o.Number, r.Attempts)
			if afterReviewerPass {
				// Materially more urgent than a first escalation: a reviewer
				// has already had its one pass, so nothing automated remains
				// behind this page.
				title = "Fix loop escalated after reviewer pass"
				detail = fmt.Sprintf("%s#%d red again on %d fix attempts since the reviewer's pass — no further automated pass will run (see PR comment)", o.Repo, o.Number, r.Attempts)
			}
			notifier.Send(title, detail, notify.PriorityHigh)
		}
	}
	return escalated
}

// autoMergeSweepInterval is the minimum spacing between label-queued
// auto-merge sweeps. The sweep piggybacks on the governor eval tick, which can
// fire much more often than once a minute; this floor keeps the sweep from
// hammering the GitHub API on short eval intervals.
const autoMergeSweepInterval = time.Minute

// taskListSweepInterval is the minimum spacing between task-list sweeps. The
// sweep enumerates every open issue in every repo — much heavier than the
// auto-merge sweep's label-scoped query — and "done" moves at PR-merge cadence,
// so a 15-minute floor keeps the API cost modest while still closing completed
// epics on the same day the last box gets ticked.
const taskListSweepInterval = 15 * time.Minute

// duplicateSweepInterval is the minimum spacing between duplicate sweeps. It
// is an hour rather than the task-list sweep's fifteen minutes for two
// reasons. Cost: the sweep fingerprints the changed-file set of every open PR,
// which on a large queue is the heaviest read the hive performs (the head-SHA
// cache makes steady-state passes nearly free, but a cold pass is not).
// Signal: duplicates accumulate at human-PR-opening cadence, so nothing is
// lost by noticing one an hour later, while a tighter loop only multiplies the
// chance of editing a suggestion under a reader's cursor.
const duplicateSweepInterval = time.Hour

// runAutoMergeSweepIfDue drains the label-queued auto-merge queue (the human
// "Approved ... for Hive auto-merge" path) at most once per
// autoMergeSweepInterval. All merge-eligibility decisions — queue-approval
// trust, the trusted-merger tier gate (SetMergerAuthorizer), check
// verification — live inside SweepQueuedAutoMerges; this function is only the
// scheduler and the dashboard audit sink.
// rotationTrigger is the PausedTrigger stamped on strand-pauses so rotation's
// auto-resume never resumes a pause it did not create.
const rotationTrigger = "provider-rotation"

// runRotationCheck applies RFC #3958 provider rotation after an eval cycle:
// for each agent not mid-task whose provider was positively measured as
// exhausted, move it to a backend with headroom at the same tier; when
// nothing has headroom, pause it loudly (strand). Stranded agents are
// auto-resumed when their provider recovers headroom. Never runs mid-task:
// only idle agents are candidates.
func runRotationCheck(ctx context.Context, cfg *config.Config, rotMgr *rotation.Manager, gov *governor.Governor, agentMgr *agent.Manager, logger *slog.Logger) {
	if rotMgr == nil || !cfg.Governor.Rotation.Enabled {
		return
	}
	govState := gov.GetState()
	for name, proc := range agentMgr.AllStatuses() {
		backend := proc.Config.Backend
		if proc.BackendOverride != "" {
			backend = proc.BackendOverride
		}

		// Auto-resume: a stranded agent whose provider recovered.
		if proc.Paused && proc.PausedTrigger == rotationTrigger {
			if rotMgr.StrandRecovered(backend) {
				if err := agentMgr.Resume(ctx, name, rotationTrigger, "provider headroom recovered"); err != nil {
					logger.Warn("rotation: auto-resume failed", "agent", name, "error", err)
				} else {
					logger.Info("rotation: auto-resumed stranded agent", "agent", name, "backend", backend)
				}
			}
			continue
		}
		if proc.Paused {
			continue // never touch an operator pause
		}
		// Never rotate mid-task: only idle agents are candidates.
		if proc.State == agent.StateRunning {
			continue
		}

		cadenceS := 0
		if interval := governorShortestActiveInterval(govState, name); interval > 0 {
			cadenceS = int(interval / time.Second)
		}

		if !rotMgr.Exhausted(backend) {
			continue
		}
		next := rotMgr.NextBackendForCadence(name, backend, cadenceS)
		if next == "" {
			// Strand loudly: pause so the agent burns nothing until a
			// provider recovers; the loop above auto-resumes it.
			if err := agentMgr.Pause(name, rotationTrigger, "no provider has headroom (RFC #3958)"); err != nil {
				logger.Warn("rotation: strand-pause failed", "agent", name, "error", err)
			} else {
				logger.Info("rotation: stranding agent, no headroom anywhere", "agent", name, "backend", backend)
			}
			continue
		}
		if err := agentMgr.SetBackendOverride(name, next); err != nil {
			logger.Warn("rotation: backend override failed", "agent", name, "to", next, "error", err)
			continue
		}
		logger.Info("rotation: moved agent to new backend", "agent", name, "from", backend, "to", next)
	}
}

func runAutoMergeSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < autoMergeSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	opts := automerge.Options{Logger: logger, MergerAuthorizer: trustedMergerFunc(cfg)}
	if cfg != nil {
		if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
			opts.RequiredChecks = set
		}
	}
	result, err := automerge.SweepQueuedAutoMerges(ctx, ghClient, opts, automerge.AutoMergeSweepOptions{
		MaxMerges: automerge.DefaultAutoMergeSweepMaxMerges,
		Audit: func(event automerge.AutoMergeSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, author=%s, queued_by=%s, label=%s, head_sha=%s, merge_sha=%s",
				event.Repo, event.Number, event.Author, event.QueuedBy, event.Label, event.HeadSHA, event.MergeSHA)
			dashSrv.AuditLog("system", "automerge-sweep-merged", detail, "")
		},
	})
	if err != nil {
		logger.Warn("automerge sweep failed", "error", err)
		return
	}
	if len(result.Merged) > 0 || result.Seen > 0 {
		logger.Info("automerge sweep complete", "seen", result.Seen, "merged", len(result.Merged), "skipped", result.Skipped)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "queued automerge sweep complete",
		Attrs: map[string]string{
			"seen":    strconv.Itoa(result.Seen),
			"merged":  strconv.Itoa(len(result.Merged)),
			"skipped": strconv.Itoa(result.Skipped),
		},
	})
}

// runTaskListSweepIfDue closes hive-filed issues whose task-list bodies are
// fully ticked, at most once per taskListSweepInterval. The finding-granularity
// policy (guidance now in every issue-filing template) tells agents to encode
// multi-part deliverables as `- [ ]` boxes; this is the sink that turns those
// boxes into closures. All safety gates — hive-filed only, at-least-one-box,
// all-boxes-ticked, hold-label respected, per-tick cap — live inside
// SweepCompletedTaskListIssues; this function is only the scheduler and the
// dashboard audit sink, mirroring runAutoMergeSweepIfDue above.
func runTaskListSweepIfDue(ctx context.Context, ghClient *github.Client, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < taskListSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepCompletedTaskListIssues(ctx, github.TaskListSweepOptions{
		MaxCloses: github.DefaultTaskListSweepMaxCloses,
		Audit: func(event github.TaskListSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, author=%s, boxes=%d",
				event.Repo, event.Number, event.Author, event.TotalBoxes)
			dashSrv.AuditLog("system", "task-list-sweep-closed", detail, "")
		},
	})
	if err != nil {
		logger.Warn("task-list sweep failed", "error", err)
		return
	}
	if len(result.Closed) > 0 || result.Seen > 0 {
		logger.Info("task-list sweep complete", "seen", result.Seen, "closed", len(result.Closed), "skipped", result.Skipped)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "task-list sweep complete",
		Attrs: map[string]string{
			"seen":    strconv.Itoa(result.Seen),
			"closed":  strconv.Itoa(len(result.Closed)),
			"skipped": strconv.Itoa(result.Skipped),
		},
	})
}

// runDuplicateSweepIfDue clusters open PRs by changed-file set and suggests
// which one to keep, at most once per duplicateSweepInterval
// (hivecommons/hive#7469 capability B).
//
// Fail-closed and opt-in at both levels. `duplicate_sweep.enabled` is off by
// default, so a hive that has not asked for this performs no extra API calls
// at all; `duplicate_sweep.post_comments` is a SECOND, separately-off grant
// for the write, so the natural first configuration is a report-only pass an
// operator can read in the log before the hive says anything on a
// contributor's PR.
//
// The sweep only ever comments. It does not close, label, approve or merge,
// and the reviewer gains no permission from its existence: the write is the
// hive's own App token going through the same canary-gated, scrubbed comment
// path every other hive-authored comment uses.
func runDuplicateSweepIfDue(ctx context.Context, cfg *config.Config, ghClient *github.Client, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.DuplicateSweep.Enabled {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < duplicateSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepDuplicatePRs(ctx, github.DuplicateSweepOptions{
		PostComments:  cfg.DuplicateSweep.PostComments,
		MaxComments:   cfg.DuplicateSweep.MaxComments,
		MaxPRsPerRepo: cfg.DuplicateSweep.MaxPRsPerRepo,
		BotAuthors:    cfg.DuplicateSweep.BotAuthors,
		Audit: func(event github.DuplicateSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, survivor=%d, superseded=%d, confidence=%s, commented=%d",
				event.Repo, event.Survivor, len(event.Superseded), event.Confidence, len(event.Commented))
			dashSrv.AuditLog("system", "duplicate-sweep-suggested", detail, "")
		},
	})
	if err != nil {
		logger.Warn("duplicate sweep reported a problem", "error", err)
	}
	if result == nil {
		return
	}
	if len(result.Clusters) > 0 || result.Scanned > 0 {
		logger.Info("duplicate sweep complete",
			"scanned", result.Scanned,
			"skipped", result.Skipped,
			"clusters", len(result.Clusters),
			"commented", result.Commented,
			"post_comments", cfg.DuplicateSweep.PostComments)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "duplicate sweep complete",
		Attrs: map[string]string{
			"scanned":   strconv.Itoa(result.Scanned),
			"clusters":  strconv.Itoa(len(result.Clusters)),
			"commented": strconv.Itoa(result.Commented),
		},
	})
}

var (
	ciFailingPath      = "/var/run/hive-metrics/ci-failing.json"
	intentVerdictsPath = "/var/run/hive-metrics/intent-verdicts.json"
)

// claimLedger holds the duplicate-PR guard's persisted issue→PR claim mapping
// across eval cycles. It is loaded lazily on first use (and retried on a load
// failure) rather than at startup, so a missing or corrupt /data ledger can
// never block the hive from booting.
var (
	claimLedgerOnce   sync.Once
	claimLedger       *github.ClaimLedger
	claimLedgerPath   = github.ClaimLedgerPath
	claimLedgerLoader = github.LoadClaimLedger
)

// hiveIdentity determines which PR authors count as "this hive", so only our
// own PRs suppress work. Two accounts can open PRs on our behalf:
//   - project.ai_author — the account agents push and open PRs as
//   - the GitHub App bot login ("<app-slug>[bot]") when the hive authenticates
//     as an installation, which is what actually authors PRs in that mode
func hiveIdentity(cfg *config.Config) github.HiveIdentity {
	id := github.HiveIdentity{AIAuthor: cfg.Project.AIAuthor}
	if slug := cfg.GitHub.ResolvedAppSlug(); slug != "" {
		id.AppLogin = slug + "[bot]"
	}
	return id
}

// applyDuplicatePRGuard filters issues already claimed by an open hive-authored
// PR out of the actionable set. Failures are logged, never fatal: the guard is
// a safety net, and a broken net must not take the hive down with it.
func applyDuplicatePRGuard(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	logger *slog.Logger,
) {
	ledger := getClaimLedger(logger)
	if ledger == nil {
		return
	}
	github.ApplyDuplicatePRGuard(ctx, ghClient, ledger, hiveIdentity(cfg), actionable, claimingPRRedStale(cfg, actionable), logger)
}

// getClaimLedger lazily loads the persisted claim ledger on first use (and
// keeps a usable empty ledger on a load failure), so a missing or corrupt
// /data ledger can never block the hive from booting. It is shared by the
// eval-cycle guard above and by the dashboard's IssueClaimed hook (#3768); the
// sync.Once publication makes the pointer safe to read from either goroutine,
// and the ledger itself is internally locked.
func getClaimLedger(logger *slog.Logger) *github.ClaimLedger {
	claimLedgerOnce.Do(func() {
		ledger, err := claimLedgerLoader(claimLedgerPath, logger)
		if err != nil {
			// LoadClaimLedger always returns a usable (possibly empty) ledger
			// alongside the error, so we keep it and just report the problem.
			logger.Warn("duplicate-PR guard: could not load persisted claim ledger, starting empty",
				"path", claimLedgerPath, "error", err)
		}
		claimLedger = ledger
	})
	return claimLedger
}

// claimingPRRedStale builds the Fix #3 release predicate: given a claiming PR
// (prRepo, prNumber), report whether it is red on a required check AND stale.
// It looks up the PR's live CI state + head SHA from this pass's enumeration and
// consults the shared staleness clock. A PR not found in the enumeration, or one
// that is green/pending, or one whose red head only just appeared, returns false
// — so a HEALTHY claiming PR still suppresses its issue. Returns a nil func when
// escalation is disabled, preserving the original unconditional-suppress
// behavior. GENERIC: keys only off check state + staleness.
func claimingPRRedStale(cfg *config.Config, actionable *github.ActionableResult) github.RedStaleFunc {
	if cfg.Escalation.Disabled || actionable == nil {
		return nil
	}
	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	// Index this pass's PRs by bare-repo#number so a claim's PRRepo (which may
	// be bare or "owner/repo") resolves regardless of prefix.
	type prState struct {
		red     bool
		headSHA string
		repo    string
	}
	index := map[string]prState{}
	for _, pr := range actionable.PRs.Items {
		index[fmt.Sprintf("%s#%d", bareRepoName(pr.Repo), pr.Number)] = prState{
			red:     pr.HasFailingRequiredCheck(),
			headSHA: pr.HeadSHA,
			repo:    fullRepo(pr.Repo),
		}
	}
	store := getEscalationStore()
	return func(prRepo string, prNumber int) bool {
		st, ok := index[fmt.Sprintf("%s#%d", bareRepoName(prRepo), prNumber)]
		if !ok || !st.red {
			return false // not enumerated, or healthy → keep suppressing
		}
		return store.StaleRed(st.repo, prNumber, st.headSHA)
	}
}

func fullRepoName(repo, org string) string {
	if strings.Contains(repo, "/") || org == "" {
		return repo
	}
	return org + "/" + repo
}

// installReviewRelaySettings gives a (possibly rebuilt) GitHub client the
// review-relay knobs that live in cfg.Review: which repos may be revised in
// place, which perspectives a verdict may name, and whether comments carry a
// confidence score (hivecommons/hive#8182). One place, so an app-auth rebuild
// cannot silently drop a setting the first client had — the perspective set
// was previously installed only on the boot path.
func installReviewRelaySettings(client *github.Client, cfg *config.Config, logger *slog.Logger) {
	if client == nil || cfg == nil {
		return
	}
	client.SetReviseRepos(cfg.Review.ReviseRepos)
	client.SetPerspectives(reviewPerspectiveSet(cfg, logger))
	client.SetConfidenceScore(func() bool { return cfg.Review.ConfidenceScore })
	client.SetReviewCadenceLimits(
		func() bool { return cfg.Review.CombinedPerspectives },
		func() int { return cfg.Review.MaxReviewsPerHead },
	)
}

// installReviewBots installs classification.review_bots on a (possibly
// rebuilt) GitHub client (hivecommons/hive#7360). hive.yaml's block wins;
// otherwise the same key is read from hive-project.yaml
// (config.DefaultProjectYAMLPath, overridable via HIVE_PROJECT_YAML — the
// path the bash pipeline stages already honour). Nil-safe: a hive without
// GitHub credentials runs with a nil client for the life of the process.
func installReviewBots(client *github.Client, cfg *config.Config, logger *slog.Logger) {
	if client == nil || cfg == nil {
		return
	}
	rb, err := cfg.EffectiveReviewBots(os.Getenv("HIVE_PROJECT_YAML"))
	if err != nil && logger != nil {
		logger.Warn("classification.review_bots: project file unreadable; review-thread reconciler stays off", "error", err)
	}
	client.SetReviewBots(rb)
	if logger != nil && rb.Enabled() {
		logger.Info("review-thread reconciler enabled",
			"review_bots", rb.Logins,
			"max_attempts_per_thread", rb.MaxAttempts(),
			"resolve_after_fix", rb.ResolveAfterFixEnabled())
	}
}

// reviewThreadsRefreshInterval throttles the review-thread monitor: the eval
// tick runs about once a minute, but every hive-authored open PR costs one
// GraphQL query per pass, and bot threads move on the scale of minutes, not
// seconds. A var so tests can drive it.
var reviewThreadsRefreshInterval = 5 * time.Minute

// reviewThreadsLastRefresh is the wall-clock of the last CollectReviewThreads
// pass (zero = never). Package-level because the eval tick is a free
// function; only the eval goroutine touches it.
var reviewThreadsLastRefresh time.Time

// writeReviewThreads is the eval-tick half of the #7360 reconciler. It runs
// CollectReviewThreads over the governor's actionable PRs at most once per
// reviewThreadsRefreshInterval, stamps each PR with the agent whose relay
// request opened it (auditPRAgents — the same attribution ci-failing.json
// carries) and with the escalation sweep's needs-human verdict, and writes
// review-threads.json. The feature-off case still writes the (empty) file
// each refresh so a reader can tell "off" from "never ran".
func writeReviewThreads(ctx context.Context, client *github.Client, actionable *github.ActionableResult, org string, escalatedPRs map[string]bool, logger *slog.Logger) {
	if client == nil || actionable == nil {
		return
	}
	now := time.Now()
	if !reviewThreadsLastRefresh.IsZero() && now.Sub(reviewThreadsLastRefresh) < reviewThreadsRefreshInterval {
		return
	}
	reviewThreadsLastRefresh = now

	report := client.CollectReviewThreads(ctx, actionable.PRs.Items, now)
	if len(report.PRs) > 0 {
		prAgents := auditPRAgents(org, now.Add(-auditPRAttributionWindow), "")
		for i := range report.PRs {
			pr := &report.PRs[i]
			pr.Agent = prAgents[fmt.Sprintf("%s#%d", pr.Repo, pr.Number)]
			pr.Escalated = escalatedPRs[escalation.Key(pr.Repo, pr.Number)]
		}
	}
	if err := github.WriteReviewThreadsReport("", report); err != nil {
		logger.Warn("failed to write review-threads.json", "error", err)
		return
	}
	if report.Enabled {
		logger.Info("review-threads.json refreshed", "prs", len(report.PRs), "threads", report.TotalThreads)
	}
}

// auditPRAttributionWindow bounds how far back the audit trail is scanned to
// map open PRs to the agent that opened them. Red PRs older than this fall
// back to scanner ownership in the kick builders — acceptable: 14d exceeds any
// PR the fleet should still be iterating on.
const auditPRAttributionWindow = 14 * 24 * time.Hour

// auditPRAgents maps "org/repo#number" → agent name from the audit trail's
// agent_pr_created entries (attribution.go records one per relay-opened PR,
// reuses included). Reading the on-disk log per eval tick keeps this
// stateless; OutputActionsSince touches no receiver state, so a zero-value
// AuditLog is safe here.
func auditPRAgents(org string, since time.Time, auditPath string) map[string]string {
	entries := (&dashboard.AuditLog{}).OutputActionsSince(since,
		map[string]bool{github.AuditActionAgentPRCreated: true}, auditPath)
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Agent == "" {
			continue
		}
		var repo, number string
		for _, part := range strings.Split(e.Detail, ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
				switch k {
				case "repo":
					repo = v
				case "number":
					number = v
				}
			}
		}
		if repo == "" || number == "" {
			continue
		}
		if !strings.Contains(repo, "/") && org != "" {
			repo = org + "/" + repo
		}
		out[repo+"#"+number] = e.Agent
	}
	return out
}

// applyHumanDecisionLabels mirrors the review swarm's "a human must decide"
// holds onto the operator's existing triage label, so they can be filtered
// from the PR list instead of being found by opening threads.
//
// This is strictly additive to the marker the reviewer writes into the review
// body. Every failure path here — no label configured, label absent from the
// repo, API refusal — is logged and skipped, because a mislabelled hold is a
// smaller problem than a hold that never got reported.
func applyHumanDecisionLabels(ctx context.Context, cfg *config.Config, ghClient *github.Client, actionable *github.ActionableResult, plan review.DispatchPlan, logger *slog.Logger) {
	if cfg == nil || ghClient == nil {
		return
	}
	label := strings.TrimSpace(cfg.Review.HumanDecisionLabel)
	if label == "" || len(plan.State.Human) == 0 {
		return
	}
	// Holds persist across cycles, so re-deriving them every pass would re-ask
	// GitHub to apply a label the PR already carries. The enumeration already
	// fetched each PR's labels, so skipping the settled ones costs nothing.
	//
	// Holds carry the owner/repo form (they descend from review reports, which
	// record GitHub's full name) while PRs.Items carry whatever governor.repos
	// says — usually the bare repo under project.org. Key both through
	// fullRepoName or the skip never matches and every cycle re-labels every
	// hold (observed on a hive whose repos were configured bare).
	org := cfg.Project.Org
	labeled := map[string]bool{}
	if actionable != nil {
		for _, pr := range actionable.PRs.Items {
			for _, have := range pr.Labels {
				if strings.EqualFold(strings.TrimSpace(have), label) {
					labeled[strings.ToLower(fmt.Sprintf("%s#%d", fullRepoName(pr.Repo, org), pr.Number))] = true
				}
			}
		}
	}
	for _, hold := range plan.State.Human {
		if labeled[strings.ToLower(fmt.Sprintf("%s#%d", fullRepoName(hold.Repo, org), hold.Number))] {
			continue
		}
		if err := ghClient.ApplyHumanDecisionLabel(ctx, hold.Repo, hold.Number, label); err != nil {
			logger.Warn("human decision label not applied; review marker still stands",
				"repo", hold.Repo, "pr", hold.Number, "label", label, "error", err)
			continue
		}
		logger.Info("human decision label applied", "repo", hold.Repo, "pr", hold.Number, "label", label, "reason", hold.Reason)
	}
}

// parseReviseCutoff reads Review.ReviseVerdictsBefore. A malformed value is
// logged and ignored rather than defaulting to "now", because a cutoff that
// silently becomes the current time would re-open every verdict in the
// artifact at once — the opposite of the narrow, deliberate correction this
// setting exists for.
// reviewPerspectiveSet resolves the hive's configured review perspectives.
//
// A bad configuration falls back to the built-in set and says so, rather than
// disabling review. Silently reviewing nothing would look identical to a healthy
// hive with an empty queue; reviewing with the defaults while logging the error
// keeps coverage up and makes the mistake findable.
func reviewPerspectiveSet(cfg *config.Config, logger *slog.Logger) review.PerspectiveSet {
	if cfg == nil {
		return review.PerspectiveSet{}
	}
	set, err := review.NewPerspectiveSet(cfg.Review.Perspectives, cfg.Review.PerspectivePrompts)
	if err != nil {
		if logger != nil {
			logger.Warn("review.perspectives is invalid; reviewing with the built-in set until it is fixed",
				"error", err)
		}
		return review.PerspectiveSet{}
	}
	return set
}

func parseReviseCutoff(raw string, logger *slog.Logger) time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}
	}
	cutoff, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		if logger != nil {
			logger.Warn("review revise_verdicts_before is not RFC3339; ignoring",
				"value", trimmed, "error", err)
		}
		return time.Time{}
	}
	return cutoff
}

func planReviewDispatch(cfg *config.Config, actionable *github.ActionableResult, agentMgr *agent.Manager, logger *slog.Logger) review.DispatchPlan {
	if cfg == nil || actionable == nil || !cfg.Review.RequireApproval || !cfg.Review.FanOut {
		return review.DispatchPlan{}
	}
	state, err := review.LoadDispatchState("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review dispatch state unavailable; starting fresh", "error", err)
	}
	artifact, err := review.LoadArtifact("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review verdict artifact unavailable for dispatch planning", "error", err)
	}
	prAgents := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	prs := make([]review.PullRequest, 0, len(actionable.PRs.Items))
	for _, pr := range actionable.PRs.Items {
		lane := classify.Classify(github.Issue{Title: pr.Title, Labels: pr.Labels}).Lane
		fullRepo := fullRepoName(pr.Repo, cfg.Project.Org)
		prs = append(prs, review.PullRequest{
			Repo:    pr.Repo,
			Number:  pr.Number,
			Title:   pr.Title,
			Author:  pr.Author,
			HeadSHA: pr.HeadSHA,
			URL:     pr.URL,
			Lane:    string(lane),
			// Grounding anchor for the review prompt. Repo access is the
			// measured active ingredient in review quality (17%→67% hit rate,
			// 61% fewer false positives), so the reviewer is told which commit
			// to read rather than being left to infer from the diff.
			MergeBase:   pr.BaseSHA,
			AuthorAgent: prAgents[fmt.Sprintf("%s#%d", fullRepo, pr.Number)],
		})
	}
	agents := make([]review.AgentCapability, 0, len(cfg.Agents))
	for name, ac := range cfg.EnabledAgents() {
		agents = append(agents, review.AgentCapability{
			Name:           name,
			Enabled:        true,
			Paused:         ac.Paused || (agentMgr != nil && agentMgr.IsPaused(name)),
			OnDemand:       ac.OnDemand,
			UsesKick:       ac.UsesGovernorKick(),
			Role:           ac.Role,
			LaneKeywords:   ac.LaneKeywords,
			DetectKeywords: ac.DetectKeywords,
			Aliases:        ac.Aliases,
		})
	}
	plan := review.PlanDispatch(prs, artifact, state, review.DispatchOptions{
		RequireApproval:       cfg.Review.RequireApproval,
		FanOut:                cfg.Review.FanOut,
		MaxParallelReviews:    cfg.Review.EffectiveMaxParallelReviews(),
		MaxPerspectivesPerPR:  cfg.Review.MaxPerspectivesPerPR,
		Perspectives:          reviewPerspectiveSet(cfg, logger),
		CombinedPerspectives:  cfg.Review.CombinedPerspectives,
		ReviewerAgents:        cfg.Review.ReviewerAgents,
		FixerAgent:            cfg.Review.FixerAgent,
		PostComments:          cfg.Review.PostComments,
		AllAuthors:            cfg.Review.AllAuthors,
		AcknowledgeNoFindings: cfg.Review.AcknowledgeNoFindings,
		ReviseRepos:           cfg.Review.ReviseRepos,
		ReviseVerdictsBefore:  parseReviseCutoff(cfg.Review.ReviseVerdictsBefore, logger),
		ProjectOrg:            cfg.Project.Org,
		AIAuthor:              cfg.EffectiveAIAuthor(),
		Agents:                agents,
	})
	if len(plan.ReviewKicks)+len(plan.FixKicks) > 0 {
		logger.Info("review swarm dispatch planned", "review_kicks", len(plan.ReviewKicks), "fix_kicks", len(plan.FixKicks))
	}
	return plan
}

func refreshReviewVerdicts(cfg *config.Config, logger *slog.Logger) {
	if cfg == nil || !cfg.Review.RequireApproval {
		return
	}
	artifact, err := review.CollectAndMerge("", "", review.AggregateOptions{
		// Unanimity is judged against what a PR was eligible to receive. Without
		// this the cap makes approve unreachable and every PR aggregates to
		// requires_human.
		MaxPerspectivesPerPR: cfg.Review.MaxPerspectivesPerPR,
		Perspectives:         reviewPerspectiveSet(cfg, logger),
	}, time.Now().UTC())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("failed to refresh review verdicts", "error", err)
		}
		return
	}
	logger.Info("review verdict artifact refreshed", "aggregates", len(artifact.Items))
}

func persistReviewDispatchState(plan review.DispatchPlan, delivered []review.DispatchKick, logger *slog.Logger) {
	planned := append(append([]review.DispatchKick(nil), plan.ReviewKicks...), plan.FixKicks...)
	if plan.State.GeneratedAt.IsZero() && len(planned) == 0 {
		return
	}
	state := review.ConfirmDelivered(plan.State, planned, delivered)
	if err := review.WriteDispatchState("", state); err != nil {
		logger.Warn("failed to persist review dispatch state", "error", err)
	}
}

func atomicWrite(path string, data []byte) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

func applyConfigOverrides(cfg *config.Config, o *snapshot.ConfigOverrides) {
	if len(o.ProjectRepos) > 0 {
		cfg.Project.Repos = o.ProjectRepos
	}
	if o.EvalIntervalS != nil {
		cfg.Governor.EvalIntervalS = *o.EvalIntervalS
	}
	if len(o.Thresholds) > 0 {
		for name, threshold := range o.Thresholds {
			if mode, ok := cfg.Governor.Modes[name]; ok {
				mode.Threshold = threshold
				cfg.Governor.Modes[name] = mode
			}
		}
	}
	if len(o.SensingGHRate) > 0 {
		cfg.Governor.Sensing.GHRatePatterns = o.SensingGHRate
	}
	if len(o.SensingCLIExclude) > 0 {
		cfg.Governor.Sensing.CLIExcludePatterns = o.SensingCLIExclude
	}
	// #4041: a persisted sensing_login that is byte-identical to the
	// pre-#3959 default set carries no operator intent — it is the old
	// defaults materialized by an earlier save. Replaying it here would
	// re-pin the false-positive-prone generic patterns over the corrected
	// code defaults the config layer just applied. Skip it; a genuinely
	// customized list still replays verbatim.
	if len(o.SensingLogin) > 0 && !config.IsLegacyDefaultLoginPatterns(o.SensingLogin) {
		cfg.Governor.Sensing.LoginPatterns = o.SensingLogin
	}
	if o.SensingTTL != nil {
		cfg.Governor.Sensing.TTLSeconds = *o.SensingTTL
	}
	if o.SensingPullback != nil {
		cfg.Governor.Sensing.PullbackSeconds = *o.SensingPullback
	}
	if len(o.ExemptLabels) > 0 {
		cfg.Governor.Labels.Exempt = o.ExemptLabels
	}
	if o.NtfyServer != "" || o.NtfyTopic != "" {
		if cfg.Notifications.Ntfy == nil {
			cfg.Notifications.Ntfy = &config.NtfyConfig{}
		}
		if o.NtfyServer != "" {
			cfg.Notifications.Ntfy.Server = o.NtfyServer
		}
		if o.NtfyTopic != "" {
			cfg.Notifications.Ntfy.Topic = o.NtfyTopic
		}
	}
	if o.DiscordWebhook != "" {
		if cfg.Notifications.Discord == nil {
			cfg.Notifications.Discord = &config.DiscordConfig{}
		}
		cfg.Notifications.Discord.Webhook = o.DiscordWebhook
	}
	if o.ModelLock != nil {
		cfg.Governor.Health.ModelLock = *o.ModelLock
	}
	if o.LogMaxSizeMB != nil {
		cfg.Governor.Logging.MaxSizeMB = *o.LogMaxSizeMB
	}
	if o.LogMaxAgeDays != nil {
		cfg.Governor.Logging.MaxAgeDays = *o.LogMaxAgeDays
	}
	if o.LogMaxBackups != nil {
		cfg.Governor.Logging.MaxBackups = *o.LogMaxBackups
	}
	if o.LogCompress != nil {
		cfg.Governor.Logging.Compress = *o.LogCompress
	}
	if o.LogLevel != "" {
		cfg.Governor.Logging.Level = o.LogLevel
	}
}

const (
	nousGovernorDir = "/var/run/nous/governor"
	nousSnapshotDir = "/data/nous/snapshots"
)

func loadNousState(logger *slog.Logger) *dashboard.NousState {
	return loadNousStateFromPaths(logger, nousGovernorDir, nousSnapshotDir)
}

func loadNousStateFromPaths(logger *slog.Logger, governorDir, snapshotDir string) *dashboard.NousState {
	state := &dashboard.NousState{
		Mode:   "observe",
		Scope:  "governor",
		Phase:  "collecting",
		Status: make(map[string]interface{}),
		Config: make(map[string]interface{}),
	}

	if ledgerData, err := os.ReadFile(filepath.Join(governorDir, "ledger.json")); err == nil {
		var ledger struct {
			Iterations []map[string]interface{} `json:"iterations"`
		}
		if err := json.Unmarshal(ledgerData, &ledger); err == nil {
			state.Ledger = ledger.Iterations
			logger.Info("nous ledger loaded", "iterations", len(state.Ledger))
		}
	}

	if principlesData, err := os.ReadFile(filepath.Join(governorDir, "principles.json")); err == nil {
		var pFile struct {
			Principles []json.RawMessage `json:"principles"`
		}
		if err := json.Unmarshal(principlesData, &pFile); err == nil {
			for _, raw := range pFile.Principles {
				var p map[string]interface{}
				if json.Unmarshal(raw, &p) == nil {
					state.Principles = append(state.Principles, dashboard.NousPrinciple{
						ID:         stringFromMap(p, "id"),
						Text:       stringFromMap(p, "statement"),
						Confidence: confidenceToFloat(stringFromMap(p, "confidence")),
						Source:     stringFromMap(p, "category"),
					})
				}
			}
			logger.Info("nous principles loaded", "count", len(state.Principles))
		}
	}

	snapshotCount := 0
	if entries, err := os.ReadDir(snapshotDir); err == nil {
		snapshotCount = len(entries)
	}

	iterationCount := len(state.Ledger)
	if iterationCount > 0 {
		state.Phase = "observing"
	}

	state.Status = map[string]interface{}{
		"status":          "active",
		"mode":            state.Mode,
		"scope":           state.Scope,
		"phase":           state.Phase,
		"snapshots":       snapshotCount,
		"snapshotCount":   snapshotCount,
		"iterations":      iterationCount,
		"principles":      len(state.Principles),
		"principleCount":  len(state.Principles),
		"baseline_target": dashboard.NousBaselineTarget,
		"snapshotTarget":  dashboard.NousBaselineTarget,
		"baseline_pct":    float64(snapshotCount) * 100 / dashboard.NousBaselineTarget,
	}

	return state
}

func stringFromMap(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func confidenceToFloat(s string) float64 {
	switch s {
	case "high":
		return 0.9
	case "medium":
		return 0.7
	case "low":
		return 0.4
	default:
		return 0.5
	}
}

const logFilename = "hive.log"

func setupLogger(dir string, maxSizeMB, maxAgeDays, maxBackups int, compress bool, level string) *slog.Logger {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("failed to create log directory, falling back to stdout only", "dir", dir, "error", err)
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(level)}))
	}

	lj := &lumberjack.Logger{
		Filename:   filepath.Join(dir, logFilename),
		MaxSize:    maxSizeMB,
		MaxAge:     maxAgeDays,
		MaxBackups: maxBackups,
		Compress:   compress,
	}

	tee := io.MultiWriter(os.Stdout, lj)
	return slog.New(slog.NewJSONHandler(tee, &slog.HandlerOptions{Level: parseLogLevel(level)}))
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// initAgentConfigDrivenSystems wires up config-driven agent metadata to subsystems
// that previously relied on hardcoded agent name maps (classifier, discord, token detector).
func initAgentConfigDrivenSystems(cfg *config.Config) {
	var lanes []classify.LaneConfig
	detectKeywords := make(map[string][]string)
	discordIdentities := make(map[string]discord.AgentIdentity)
	discordAliases := make(map[string]string)

	for name, agent := range cfg.Agents {
		if len(agent.LaneKeywords) > 0 {
			lanes = append(lanes, classify.LaneConfig{
				Name:     name,
				Keywords: agent.LaneKeywords,
			})
		}
		if len(agent.DetectKeywords) > 0 {
			detectKeywords[name] = agent.DetectKeywords
		}
		if agent.Emoji != "" || agent.Color != "" {
			discordIdentities[name] = discord.AgentIdentity{
				Emoji: agent.Emoji,
				Color: parseColorInt(agent.Color),
			}
		}
		sort.Slice(lanes, func(i, j int) bool { return lanes[i].Name < lanes[j].Name })
		for _, alias := range agent.Aliases {
			discordAliases[alias] = name
		}
	}

	if len(lanes) > 0 {
		classify.SetLanes(lanes)
	}
	// Tier-classification keywords (config-driven, mirroring SetLanes). Empty
	// lists leave the built-in defaults in force, so an absent classifier block
	// keeps behavior unchanged. Always call so a reload that CLEARS the block
	// restores defaults.
	classify.SetTierKeywords(cfg.Classifier.SimpleKeywords, cfg.Classifier.ComplexSignals)
	if len(detectKeywords) > 0 {
		tokens.SetDetectKeywords(detectKeywords)
	}
	discord.SetAgentIdentities(discordIdentities)
	slack.SetAgentIdentities(discordIdentities)
	matrix.SetAgentIdentities(discordIdentities)
	telegram.SetAgentIdentities(discordIdentities)
	msteams.SetAgentIdentities(discordIdentities)
	if len(discordAliases) > 0 {
		discord.SetAgentAliases(discordAliases)
		slack.SetAgentAliases(discordAliases)
		matrix.SetAgentAliases(discordAliases)
		telegram.SetAgentAliases(discordAliases)
		msteams.SetAgentAliases(discordAliases)
	}
}

// inferACMMLevel returns the configured ACMM level, defaulting to L1 (advisory-only).
// sameStringSlice reports whether two string slices have identical contents in
// the same order. Used to skip no-op authorized-users updates from heartbeats.
func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func inferACMMLevel(cfg *config.Config) int {
	if cfg.ACMMLevel != nil {
		return *cfg.ACMMLevel
	}
	return 1
}

// parseColorInt converts a hex color string like "#3498db" to an int.
func parseColorInt(color string) int {
	color = strings.TrimPrefix(color, "#")
	if color == "" {
		return 0x95a5a6
	}
	var result int
	if _, err := fmt.Sscanf(color, "%x", &result); err != nil {
		return 0x95a5a6 // malformed hex: fall back to the same default as an empty string
	}
	return result
}

// logAgentSandboxPosture emits the sandbox gate diagnostics from
// config.AgentSandboxGateWarnings at WARN.
//
// Split out so boot and the config-watcher reload report identically — an
// operator who flips the Security tab's sandbox toggle never restarts, so a
// boot-only check would never reach the person who most needs it.
func logAgentSandboxPosture(logger *slog.Logger, cfg *config.Config) {
	for _, warning := range config.AgentSandboxGateWarnings(cfg) {
		logger.Warn("agent sandbox posture", "warning", warning)
	}
}

func runHub(logger *slog.Logger, configPath string) {
	runHubWithDeps(context.Background(), logger, configPath, defaultHubDeps())
}

func resolveLiteLLMInferenceRoute(cfg *config.Config, backend, requestedModel string) (endpoint, model string, ok bool) {
	return inference.ResolveLiteLLMRoute(cfg, backend, requestedModel)
}

func resolveWatsonxGateway(cfg *config.Config) *config.GatewayConfig {
	return inference.ResolveWatsonxGateway(cfg)
}

func resolveGatewayAuth(gw *config.GatewayConfig, agentName, backend string, logger *slog.Logger) (string, map[string]string) {
	return inference.ResolveGatewayAuth(gw, agentName, backend, logger)
}

func superviseLocalLiteLLM(ctx context.Context, logger *slog.Logger) {
	inference.SuperviseLocalLiteLLM(ctx, logger)
}

func litellmLocalProxyURL() string {
	return inference.LocalLiteLLMProxyURL()
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseEndpointList splits a comma-separated list of URLs into a slice.
// A single URL is returned as a one-element slice.
func parseEndpointList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Dashboard system-alert IDs for the budget thresholds.
const (
	budgetWarnAlertID      = "budget-warn"
	budgetExhaustedAlertID = "budget-exhausted"
	// noCadenceAlertID is the never-kicked cause+fix banner (#5577): enabled
	// agents with no cadence in any mode and no kick ever.
	noCadenceAlertID = "agent-no-cadence"
	// providerBudgetAlertID is the PROVIDER spend rebuff (#4294), kept distinct
	// from the two token-budget alerts above so an operator can tell "we used
	// our token allowance" from "the gateway will not spend more money".
	providerBudgetAlertID = "provider-budget-exceeded"
)

func dispatchSubcommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "--version", "version":
		fmt.Fprintf(stdout, "hive %s (commit %s, branch %s)\n", reportedVersion(), gitShort, gitBranch)
		return true, 0
	case "validate", "--config-check":
		return true, runConfigCheck(args[1:], stdout, stderr)
	default:
		return false, 0
	}
}
