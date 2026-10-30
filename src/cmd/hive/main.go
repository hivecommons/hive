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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/inference"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/crux/trajectory"
	"github.com/hivecommons/hive/crux/watchdog"
	"github.com/hivecommons/hive/crux/watsonx"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/apphealth"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	convergenceaudit "github.com/hivecommons/hive/pkg/convergence/audit"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/discord"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/fleetreport/slogfeed"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/hub"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/jev"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/loginscan"
	"github.com/hivecommons/hive/pkg/mint"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/rebasemerge"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/autoscale"
	"github.com/hivecommons/hive/pkg/rfccoverage"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/sessionprune"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/spokealerts"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/worksource"
	"github.com/hivecommons/hive/pkg/worksource/wavefront" // registers the additive Wavefront source (#8362)
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

// publishFleetReports is the legacy entry point kept for the one call site's
// signature; the policy lives in selfreport_publish.go (#11868).
func publishFleetReports(ctx context.Context, logger *slog.Logger, ghClient *github.Client, dashSrv *dashboard.Server, res *fleetreport.Result, dryRun bool) {
	publishFleetReportsWith(ctx, logger, dashSrv, res, selfReportPublishDeps{
		dryRun:  dryRun,
		gh:      ghClient,
		limits:  fleetreport.DefaultFilingLimits,
		now:     time.Now,
		tracker: fleetreport.DefaultTracker(),
	})
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
	CostDailyRollup       string
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
		CostDailyRollup:       "/data/cost-daily-rollup.json",
		TrendHistory:          "/data/trend-history.json",
		BudgetWindowHistory:   "/data/budget-window-history.json",
		ConvergenceSoak:       "/data/convergence-soak-history.json",
	}
}

// heartbeatProviders is the ADR-0024 per-provider auth/availability roll-up
// sent in the hub heartbeat; nil when the provider-status knob is off.
func heartbeatProviders(cfg *config.Config, agents []spoke.AgentSummary) []crux.ProviderStatus {
	if cfg == nil || !cfg.Providers.StatusEnabled() {
		return nil
	}
	return spoke.ProviderStatusForHeartbeat(agents)
}

func agentActivityFor(mgr crux.Runtime, cfg *config.Config, govState governor.State, currentMode, name string, proc *crux.Process, onDemandFromPack map[string]bool) hub.AgentActivity {
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
	if status := proc.BackendAuth.Status; status != "" && status != crux.BackendAuthOK {
		act.BackendAuthStatus = status
		act.BackendAuthSince = proc.BackendAuth.Since
		act.BackendAuthLastError = proc.BackendAuth.LastError
	}
	if refreshable, known := mgr.AgentCredentialRefreshable(name); known {
		act.BackendAuthRefreshable = &refreshable
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

func heartbeatKickInterval(govState governor.State, name string, proc *crux.Process, onDemandFromPack map[string]bool) time.Duration {
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

// resumeKickHeldAlerts tracks crash-restarted agents the resume-kick gate
// refused, so the eval loop can raise and later clear their dashboard alert.
var resumeKickHeldAlerts = spokealerts.NewResumeKickHeld()

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
		// Hooks (hold labels, #6203 repo pause, #6204 agent scope, and the
		// rest) are installed by bootGitHub through configureGitHubClient, the
		// same function every rebuild path uses (#9614).
		out.Client = github.NewClientFromAppWithBotLogin(out.AppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
		startDocsTokenRefresh(ctx, cfg, appKeyFile, logger)
		return out
	}

	ghToken := cfg.GitHub.Token
	if ghToken == "" {
		ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
	}
	switch {
	case ghToken != "":
		out.Client = github.NewClient(ghToken, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.ResolvedAPIURL()) // hooks: see the App branch above
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
	// proxyInjectConfigExitCode marks an exit caused by the #9586 credential
	// posture guard (config.ValidateProxyInjectGHAuth: injection combined with
	// HIVE_PROXY_ADVISORY_OK=true). Its own code so a refusing pod's
	// termination state says "fix the env", not "bug".
	proxyInjectConfigExitCode = 19
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
	// compares against. The Dockerfile cuts it to 7 chars from the full hash; trim
	// defensively in case an older build used `--short=7` (a minimum) so what we report to the
	// hub is always the same length it stores (no short-vs-full mismatch).
	gitShort = canonicalGitShort(gitShort)
	dashboard.SetGitVersion(gitHash, gitShort)
	dashboard.SetFleetReportBuildInfo(reportedVersion(), gitShort)
	dashboard.SetGitBranch(gitBranch)
	// Resolve channel and tracking together from the cached Deployment image.
	// No authoritative image outside a cluster means tracking stays unknown.
	dashboard.SetDeploymentImageSource(deps.selfImage)

	// slogfeed tees WARN+ records into the self-report tracker so a log line
	// that keeps repeating is noticed even without a hook site (see
	// fleetreport.ClassUnclassifiedRepeated).
	b.logger = slog.New(slogfeed.New(logscrub.NewHandler(slog.NewJSONHandler(deps.stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))))
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

	// #9586: refuse to boot a spoke whose GitHub credential posture is
	// EXPLOITABLE - injection combined with the advisory-mode self-asserted
	// identity, where a spoofed agent name would select whose real token gets
	// injected. Before config load and before any agent or proxy starts, so
	// nothing is ever minted under it. The hub branch above runs no agents and
	// is not gated.
	if err := config.ValidateProxyInjectGHAuth(deps.getenv); err != nil {
		b.logger.Error("refusing to start: contradictory GitHub credential configuration (#9586)", "error", err.Error())
		deps.exit(proxyInjectConfigExitCode)
		return false
	}
	// An unrecognized HIVE_PROXY_INJECT_GH_AUTH value is loud but NOT fatal:
	// spokes auto-deploy within about a minute of a merge, so failing here
	// would crash-loop any live spoke that already carries such a value. It
	// keeps today's meaning (injection off); the dashboard Security tab shows
	// the same diagnosis (securitySectionResponse, credentialWarnings).
	for _, warning := range config.ProxyInjectGHAuthWarnings(deps.getenv) {
		b.logger.Error("GitHub credential configuration warning (#9586): agents hold their real token", "warning", warning)
	}
	// #9586: one line saying which way injection resolved and why. Injection
	// is opt-in only (HIVE_PROXY_INJECT_GH_AUTH=true); unset is off on every
	// hive, and nothing here writes the env.
	injection := config.ResolveProxyInjectGHAuth(deps.getenv)
	b.logger.Info(injection.LogLine(), "enabled", injection.Enabled, "source", string(injection.Source), "env", config.ProxyInjectGHAuthEnv)

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
	for _, warning := range config.WriteSurfaceWarnings(b.cfg) {
		b.logger.Warn("write surface allowlist", "issue", warning)
	}
	for _, warning := range config.ReviewHeadsWarnings(b.cfg) {
		b.logger.Warn("review heads config", "issue", warning)
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

	// Runtime observability (#14681): GOMEMLIMIT from the cgroup limit, the
	// loopback pprof listener and the 60 s `mem:` line. Before tracing so the
	// first mem line lands even when an exporter stalls.
	b.startRuntimeObservability()

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
	// The forgestate cache stretches its reconcile cadence from the same
	// rate-limit headers the GitHub transport observes (hivecommons/hive#13380).
	forgestate.SetDefaultBudgetSource(github.RateLimitBudget)
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
	b.installReviewEvents()
	b.installWebhookWake()
	b.installSentinelEvents()
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
			var laneQueued *int
			if b.sched != nil {
				laneQueued = b.sched.ReviewerLaneQueuedWork(name)
			}
			if includeBlockingChecks {
				full := spoke.AgentActivityFor(b.agentMgr, b.cfg, govState, currentMode, name, proc, b.onDemandFromPack)
				full.LaneQueuedWork = laneQueued
				agents = append(agents, spoke.NewAgentSummary(name, string(proc.State), mode, full))
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
				LaneQueuedWork: laneQueued,
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
			spoke.ApplyBackendAuth(&act, b.agentMgr, name, proc)
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

	var ownerHeartbeatTokenPath string
	var ownerHeartbeatTokenMTime time.Time
	var ownerHeartbeatTokenSize int64
	var ownerHeartbeatLogin string
	var ownerHeartbeatMu sync.Mutex
	b.ownerForHeartbeat = func() string {
		ownerHeartbeatMu.Lock()
		defer ownerHeartbeatMu.Unlock()
		const tokenPath = "/data/gh-user-token"
		st, err := os.Stat(tokenPath)
		if err == nil && tokenPath == ownerHeartbeatTokenPath && st.ModTime().Equal(ownerHeartbeatTokenMTime) && st.Size() == ownerHeartbeatTokenSize {
			return ownerHeartbeatLogin
		}
		ownerHeartbeatTokenPath = tokenPath
		if err == nil {
			ownerHeartbeatTokenMTime = st.ModTime()
			ownerHeartbeatTokenSize = st.Size()
		} else {
			ownerHeartbeatTokenMTime = time.Time{}
			ownerHeartbeatTokenSize = 0
		}
		ownerHeartbeatLogin = ""
		if err == nil {
			if td, err := os.ReadFile(tokenPath); err == nil {
				tok := strings.TrimSpace(string(td))
				if tok != "" {
					// gh-user-token is a github.com OAuth token — validate its identity against github.com,
					// not the (possibly GHE) repo host.
					if u, err := github.ValidateTokenCached(tok, b.cfg.GitHub.OAuthAPIURL()); err == nil {
						ownerHeartbeatLogin = u.Login
					}
				}
			}
		}
		return ownerHeartbeatLogin
	}

	b.dashboardURLForHeartbeat = func() string {
		if b.cfg.Hub.DashboardURL != "" {
			return b.cfg.Hub.DashboardURL
		}
		// Prefer the host our OWN Route/Ingress actually serves. The synthesised
		// "<hiveID>.<hub host>" below is only correct when this spoke is fronted by
		// the hub's wildcard domain; push-reported clusters must report their live host.
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

	b.dashboardDependencies = func() *dashboard.Dependencies {
		var auditLedger *mutation.Ledger
		var auditJournal *mutation.Journal
		if boundary, ok := b.mutationBoundary.(*mutation.Boundary); ok {
			auditLedger = boundary.Executor.Ledger
			auditJournal = boundary.Executor.Journal
		}
		return &dashboard.Dependencies{
			Config:            b.cfg,
			AgentMgr:          b.agentMgr,
			CruxRuntimeHealth: cruxRuntimeHealthFunc(b.cruxSupervision),
			// Provider gateways (#5565 slice 3): concrete openrouter/watsonx/
			// linearagent adapters behind the dashboard's consumer-defined
			// interfaces — this composition root is the only non-test place that
			// names the concrete types.
			Watsonx:                 watsonxGateway{},
			OpenRouter:              openRouterGateway{},
			NewLinearAgent:          newLinearAgentGateway(b.logger),
			LinearStoredViewerID:    linearStoredViewerID,
			DelegateDecider:         newDelegateDeciderRunner(func() *config.Config { return b.cfg }),
			ChatGrounder:            newChatGroundingRunner(func() *config.Config { return b.cfg }),
			Governor:                b.gov,
			GHClient:                b.ghClient,
			StageEngineStatus:       func() stageengine.Status { return stageEngineStatus(b.cfg, b.ghClient, b.logger) },
			ReviewerAutoscaleStatus: func() autoscale.Status { return reviewerAutoscaler.Status() },
			ReviewEvents:            b.reviewEvents,
			WebhookWake:             b.webhookWakeFunc(),
			SweepNudge:              b.sweepNudgeFunc(),
			ApplyWebhookItems:       b.applyWebhookItemsFunc(),
			WebhookDelivery:         b.webhookDeliveryFunc(),
			EvalLoop:                b.evalLoopStatusFunc(),
			SentinelPREvent:         b.sentinelPREventFunc(),
			CIFailureKick:           b.ciFailureKickFunc(),
			ProjectsV2ItemEvent:     b.projectsV2ItemEventFunc(),
			GHAppAuth:               b.appAuth,
			GHTokenScopes:           b.ghAuth.TokenScopes,
			Tokens:                  b.tokenCollector,
			Knowledge:               b.knowledgeAPI,
			Inception:               b.inceptionEngine,
			Nous:                    b.nousState,
			Scheduler:               b.sched,
			KnowledgePrimer:         knowledgePrimerControl{b: b},
			MetricsCollector:        b.metricsCollector,
			RotationMgr:             b.rotationMgr,
			QuestionAutoclose:       questionAutocloseView{b: b},
			HeadroomPublisher:       b.quotaReadingPublisher,
			// #3972: hand the ACMM advisor the SAME cached fleet-stats collector
			// the heartbeat reads, so its merge-success signal reuses the existing
			// 30-minute collect loop instead of issuing a second GitHub fetch.
			FleetStats:            b.fleetStatsCollector,
			Activity:              b.activityCollector,
			RepoCost:              b.repoCostCollector,
			LaunchCost:            b.launchCostCollector,
			BeadSynthesizer:       b.beadSynth,
			KnowledgeConnectors:   b.knowledgeConnectorRuntime(),
			KnowledgePublish:      b.knowledgePublishRuntime(),
			BeadStores:            b.beadStores,
			BeadStoreLoadFailures: b.beadStoreLoadFailures,
			AuditLedger:           auditLedger,
			AuditJournal:          auditJournal,
			AuditPublisherFunc: func() convergenceaudit.FindingPublisher {
				return b.findingPublisher
			},
			AuditOutcomesFunc: func() *outcome.Ledger {
				return b.outcomeLedger
			},
			// RFC #4000 approval desk. Nil unless `tool_approval.enabled`, in which
			// case the Approvals panel renders as "not enabled".
			ApprovalDesk:  b.approvalDesk,
			ApprovalInbox: b.approvalInbox,
			IssueClaims:   b.issueClaims,
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
			// hivecommons/hive#14799: the ledger above is one eval cycle old;
			// ask the forge directly whether an open PR already links the
			// issue before the contribute lane dispatches it again.
			OpenPRLinkingIssue: func(repo string, number int, labels []string) (github.OpenPRLink, bool) {
				client := b.currentGitHubClient()
				if client == nil {
					return github.OpenPRLink{}, false
				}
				ctx, cancel := context.WithTimeout(context.Background(), openPRLinkGuardTimeout)
				defer cancel()
				return client.OpenPRLinkGuard(ctx, repo, number, labels)
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
			OnACMMLevelChanged: b.onACMMLevelChanged,
			RunBurndown: func(ctx context.Context, key string) (*dashboard.RunBurndown, error) {
				w := b.cfg.Governor.WorkSource.Wavefront
				if !w.Enabled {
					return nil, nil
				}
				ref, ok := worksource.ParseKey(key)
				if !ok || ref.Repo != w.Repo {
					return nil, nil
				}
				if _, _, ok := wavefront.SplitExternalID(ref.ExternalID); !ok {
					return nil, nil
				}
				src, err := wavefront.New(wavefront.Options{
					Repo: w.Repo, Path: w.Path, URL: w.URL, ReceiptsDir: w.ReceiptsDir,
				})
				if err != nil {
					return nil, err
				}
				bd, ok, err := src.Burndown(ctx, key)
				if err != nil || !ok {
					return nil, err
				}
				return &dashboard.RunBurndown{
					Source:    "wavefront",
					Satisfied: bd.Satisfied,
					Remaining: bd.Remaining,
					Unknown:   bd.Unknown,
					Scope:     bd.Scope,
				}, nil
			},
			RunFanout: func(ctx context.Context, runKey string, repos []string) ([]string, error) {
				w := b.cfg.Governor.WorkSource.Wavefront
				if !w.Enabled || b.dashSrv == nil {
					return nil, nil
				}
				waveIDs := make([]string, 0, len(repos))
				for i, repo := range repos {
					wave := i + 1
					status := &worksource.SpektacularRunStatus{
						RunKey: runKey,
						Waves: []worksource.RunWaveStatus{{
							Wave: wave,
							Repositories: []worksource.RunRepositoryStatus{{
								Repo: repo,
							}},
						}},
					}
					result, err := (worksource.RunFanoutRunner{
						Status: status,
						Leases: b.dashSrv,
					}).FanOutWave(ctx, runKey, wave)
					if err != nil {
						return nil, err
					}
					if len(result.Created) > 0 {
						waveIDs = append(waveIDs, fmt.Sprintf("wave-%d:%s", wave, repo))
					}
				}
				return waveIDs, nil
			},
			WavefrontComplete: func(ctx context.Context, key, externalID, revision string, startedAt time.Time) error {
				w := b.cfg.Governor.WorkSource.Wavefront
				if !w.Enabled {
					return nil
				}
				ref, ok := worksource.ParseKey(key)
				if !ok || ref.Repo != w.Repo {
					return nil
				}
				src, err := wavefront.New(wavefront.Options{
					Repo: w.Repo, Path: w.Path, URL: w.URL, ReceiptsDir: w.ReceiptsDir,
				})
				if err != nil {
					return err
				}
				_, err = src.Complete(ctx, externalID, revision, nil, startedAt)
				return err
			},
			WavefrontUnknown: func(ctx context.Context, externalID, reason string, startedAt time.Time) error {
				w := b.cfg.Governor.WorkSource.Wavefront
				if !w.Enabled {
					return nil
				}
				src, err := wavefront.New(wavefront.Options{
					Repo: w.Repo, Path: w.Path, URL: w.URL, ReceiptsDir: w.ReceiptsDir,
				})
				if err != nil {
					return err
				}
				_, err = src.MarkUnknown(ctx, externalID, reason, startedAt)
				return err
			},
			// #8361/#6899: external-execution linked-engine status plus
			// lazy dispatch/peer attachment. Adapters register only under
			// their extwork_* build tags.
			ExternalExec: &extworkStatus{registry: extwork.DefaultRegistry, srv: b.dashSrv, cfg: b.cfg, now: time.Now},
			CELTrigger: func(ctx context.Context, ev celtrigger.NormalizedEvent, reason string) {
				celTriggerKickAgents(ctx, celEngineFor(b.cfg, b.logger), ev, b.cfg, b.gov, b.agentMgr.IsPaused, b.agentMgr, reason, b.logger)
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
				// review_bots.min_priority and fix_human_prs are edited
				// through the same PUT; reinstall so the thread reconciler
				// applies them without a restart (#10481).
				installReviewBots(b.ghClient, b.cfg, b.logger)
			},
			EnumerateFunc: func() {
				runEvalCycle(b.ctx, b.cfg, b.ghClient, b.gov, b.sched, b.agentMgr, b.dashSrv, b.notifier, b.beadStores, b.tokenCollector, b.metricsCollector, b.nousState, &b.lastActionable, b.advisoryStore, b.advisoryIssues, nil, b.approvalDesk, evalSourceEnumerate, b.logger)
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
				// #9614: every boot-time hook, not a hand-kept subset.
				newClient := b.newConfiguredGitHubAppClient(newAppAuth)
				// #9621: every consumer follows (sandbox, relays, collectors).
				b.adoptGitHubClient(newClient, newAppAuth)
				// Deliver fresh per-agent scoped tokens to already-running agents
				// immediately — the periodic refresh loop only ticks every 40m,
				// far too long for agents whose caches are empty or stale (#4072).
				go b.agentMgr.RefreshAgentTokens(b.ctx)
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
		homeMaxAge := time.Duration(sessionprune.DefaultHomeRetentionDays) * 24 * time.Hour
		stop := make(chan struct{})

		go func() {
			// Run once at startup rather than waiting a full interval. A spoke that
			// restarts more often than the interval would otherwise never prune.
			runSessionPrune(b.logger, dir, maxAge)
			runAgentSessionTreePrune(b.logger, agentSessionTreesRoot, maxAge)
			runAgentHomePrune(b.logger, agentHomesRoot, homeMaxAge)

			ticker := time.NewTicker(sessionPruneInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					runSessionPrune(b.logger, dir, maxAge)
					runAgentSessionTreePrune(b.logger, agentSessionTreesRoot, maxAge)
					runAgentHomePrune(b.logger, agentHomesRoot, homeMaxAge)
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
	// The same function every rebuild path uses (#9614). Only the tiers whose
	// dependencies exist yet apply here; bootAdvisory, bootAgents and
	// bootDashboard apply the rest as those dependencies come up.
	b.configureGitHubClient(b.ghClient)
	// Long-lived consumers read the client through this provider, never a
	// captured b.ghClient, so every rebuild reaches them (#9621).
	b.publishGitHubClient(b.ghClient)
}

// bootGovernor constructs the governor and scheduler, wires the prompt and
// definition resolvers, restores the persisted history the governor and
// dashboard sparklines seed from, and enables the knowledge primer.
func (b *boot) bootGovernor() {
	// The user write-token client (userGHClient) was removed: every GitHub write
	// — issues, PRs, comments, merges, and the advisory digest — now goes through
	// the hive's App installation token (ghClient / hivecommons-hive[bot]). The
	// user token only ever served as an advisory-digest fallback writer, which is
	// no longer wanted (and forced the excessive "repo" login scope, issue #1927).
	// Dashboard login now requests no scope and no user write-token is persisted.

	b.gov = governor.New(b.cfg.Governor, b.cfg.EnabledAgents(), b.logger)
	// Default mode thresholds scale with how many repos the hive watches, so
	// the mode ladder means the same thing on a 3-repo hive as on a 39-repo
	// one (#3498). Explicit thresholds are unaffected.
	b.gov.SetRepoCount(b.cfg.Project.RepoCount())
	b.sched = scheduler.New(b.cfg, b.logger)
	b.sched.SetSurgeDuration(b.gov.SurgeDuration)
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

	// Restore the per-day cost rollup (the years-long tail behind the 30-day
	// snapshot ring). Missing is ordinary on a hive upgrading into the feature:
	// the live fold starts filling it from the snapshots.
	const costDailyRollupPath = "/data/cost-daily-rollup.json"
	if rollupData, err := os.ReadFile(costDailyRollupPath); err == nil {
		if err := json.Unmarshal(rollupData, &b.pendingCostRollupSeed); err == nil && len(b.pendingCostRollupSeed) > 0 {
			b.logger.Info("cost daily rollup loaded", "days", len(b.pendingCostRollupSeed))
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
		b.sched.SetPrimer(b.newKnowledgePrimer())
	}
}

// newKnowledgePrimer builds a kick primer from the configured wiki layers
// and primer settings, with no file stores registered yet. bootGovernor and
// the live dashboard toggle (knowledgePrimerControl, #9231) share it so both
// prime from the same config.
func (b *boot) newKnowledgePrimer() *knowledge.Primer {
	layers := convertKnowledgeLayers(b.cfg.Knowledge.Layers)
	primerCfg := knowledge.PrimerConfig{
		MaxFacts:      b.cfg.Knowledge.Primer.MaxFacts,
		Priority:      b.cfg.Knowledge.Primer.Priority,
		MergeStrategy: b.cfg.Knowledge.Primer.MergeStrategy,
		MaxBytes:      b.cfg.Knowledge.Primer.MaxBytes,
	}
	p := knowledge.NewPrimer(layers, primerCfg, b.logger)
	b.logger.Info("knowledge primer enabled",
		"layers", len(b.cfg.Knowledge.Layers),
		"max_facts", primerCfg.MaxFacts,
		"max_bytes", p.MaxBytes(),
	)
	return p
}

// bootAdvisory builds the notifier, infers the ACMM level, seeds the
// GitHub App banner state, opens the mutation-convergence ledger and journal,
// finds or creates the pinned advisory issue and seeds the embedded default
// policies to the policy dir. It returns false when main() should return
// without booting further — the v5 mutation ledger/journal failure paths,
// which returned from main() before the split and still do.
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

	// Invocation-attribution trail (pkg/github/attribution.go): the trailer
	// gate was installed by bootGitHub (configureGitHubClient), before the
	// advisory-issue ensure below; the per-agent resolver and the audit sink
	// follow in bootAgents and bootDashboard as their dependencies come up.

	if b.cfg == nil {
		return false
	}
	mode := b.cfg.ConvergenceMode()
	stats := &effects.Recorder{}
	stats.SetMode(mode)
	b.mutationStats = stats
	ledger, err := mutation.OpenLedgerScoped(filepath.Join(mutationStateDir, "claims.json"),
		b.cfg.MutationMaxWritersPerTarget(), mutation.TargetWriterScope)
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
	installMutationSelfReportHooks(boundary)
	b.mutationBoundary = boundary
	b.applyGitHubClientMutationBoundary(b.ghClient)
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

	// Seed every embedded default policy to disk so agents that search the
	// filesystem find them, and keep them in sync with the running image on
	// every later boot without clobbering a genuine dashboard edit. This
	// generalizes what used to be a brainstorm-advisory.md-only force-rewrite
	// (added so an embedded fix, e.g. bug #113's inception reaping guard,
	// always reached the agent) to every template: a stale seeded copy that
	// nobody edited now gets refreshed the same way, instead of silently
	// shadowing embedded policy updates forever (hivecommons/hive#9428).
	if err := policies.ReconcileSeededDefaults(b.policyDirPath, b.logger); err != nil {
		b.logger.Warn("failed to seed default policies", "path", b.policyDirPath, "error", err)
	}

	b.projectCtx = crux.ProjectContext{
		Org: b.cfg.Project.Org,
		// Repos stays the full watched set; the functions below narrow it at
		// read time — RepoPaused to the repos open for work (#6203), AgentRepos
		// and AgentPrimaryRepo to the ones a given agent serves (#6204) — so a
		// pause taken or a scope edited mid-run reaches the next agent launch
		// without rebuilding this context.
		Repos:              b.cfg.Project.Repos,
		RepoPaused:         b.cfg.IsRepoPaused,
		AgentRepos:         b.cfg.ReposForAgent,
		AgentPrimaryRepo:   b.cfg.PrimaryRepoForAgent,
		PrimaryRepoName:    b.cfg.Project.PrimaryRepo,
		ACMMLevel:          b.acmmLevel,
		PRsAllowed:         b.cfg.Project.PRsAllowed(),
		PolicyDir:          b.policyDirPath,
		SharedUIDReadToken: b.cfg.GitHub.SharedUIDReadToken,
		AppAuthoredPRs:     b.cfg.GitHub.AppAuthoredPRsEnabled(),
		AppBotLogin:        b.cfg.GitHub.BotLogin(),
		AppBotUserID:       b.appBotUserIDResolver(),
		BobSessionPrefix:   b.cfg.Governor.Bob.SessionPrefix,
	}
	return true
}

// appBotUserIDResolver returns the lazy lookup of the App bot's GitHub USER id
// that push-capable agents' git noreply email is keyed by. GitHub links
// `<id>+<login>@users.noreply.github.com` by the bot user id, which differs
// from the App id, so this goes through GET /users/<slug>[bot] (cached on the
// client after the first success) rather than config. Nil without a client.
func (b *boot) appBotUserIDResolver() func() int64 {
	if b.ghClient == nil {
		return nil
	}
	return func() int64 {
		ctx := b.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		return b.ghClient.AppBotUserID(ctx)
	}
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
	for _, name := range b.cfg.Governor.ClearInvalidInferenceLaunchCmds(b.cfg.Agents) {
		b.logger.Warn("cleared invalid inference launch_cmd; inference and gateway backends launch through claude", "agent", name)
	}
	rtCfg := cruxconfig.RuntimeConfigFromEnv()
	runtimeOptions := []cruxruntime.Option{
		cruxruntime.WithDefinitions(b.cfg.EnabledAgents()),
		cruxruntime.WithLogger(b.logger),
		cruxruntime.WithProject(b.projectCtx),
	}
	if sharedAgentPoolEnabled() {
		hubURL := strings.TrimSpace(os.Getenv("HIVE_HUB_URL"))
		if hubURL == "" {
			hubURL = b.cfg.Hub.URL
		}
		pool, poolErr := cruxruntime.NewSharedSessionPoolClient(hubURL, b.cfg.HiveID, hub.SpokeHeartbeatKey())
		if poolErr != nil {
			b.logger.Error("shared agent pool is enabled but cannot be configured", "error", poolErr)
			deps.exit(1)
			return
		}
		runtimeOptions = append(runtimeOptions, cruxruntime.WithSharedSessionPool(pool))
	}
	b.agentMgr, err = deps.newRuntime(rtCfg, runtimeOptions...)
	if err != nil {
		b.logger.Error("crux runtime unavailable", "mode", rtCfg.Mode, "error", err)
		deps.exit(1)
		return
	}
	if sh, ok := b.agentMgr.(*sidecarHost); ok {
		sh.setLogger(b.logger)
	}
	// A sidecar runtime reaches hive's capabilities (token minting, PR
	// creation, Linear credentials, scanners, conditions) over the callback
	// endpoint instead of the Set* calls below; serve it before the runtime
	// starts dialing. Failing to bind is as fatal as no runtime: the runtime
	// would otherwise run with every capability reporting unavailable.
	if rtCfg.Mode == cruxconfig.ModeSidecar {
		if err := deps.startHiveCallback(b.ctx, b.hiveCallbackCapabilities(), b.logger); err != nil {
			b.logger.Error("hive callback endpoint unavailable", "error", err)
			deps.exit(1)
			return
		}
		// Stage 4: spawn/supervise `crux serve` (CRUX_SUPERVISE=child) and
		// keep the crux_runtime_unavailable condition in step with the
		// socket. The callback endpoint is up first so a child's first
		// capability call lands.
		if b.cruxSupervision = newCruxRuntimeSupervision(rtCfg, b.agentMgr, b.logger); b.cruxSupervision != nil {
			deps.superviseRuntime(b.ctx, b.cruxSupervision)
		}
	}
	// The runtime's spans go through pkg/tracing so they carry the hive
	// component attribute and count toward reach (#3973); the runtime itself
	// only knows the OpenTelemetry API (#13536).
	b.agentMgr.SetSpanStarter(tracing.StartSpan)
	// The runtime's self-reported error conditions (stalls, restart storms,
	// token-mint failures…) feed the fleet-report tracker through the
	// types.ConditionReporter seam; the runtime does not import pkg/fleetreport.
	b.agentMgr.SetConditionReporter(fleetreport.DefaultReporter{})
	// SIGTERM (pod roll, hive upgrade) destroys every tmux server and with it
	// the in-flight kick's scrollback; archive it to /data first (#4296).
	archiveOnShutdown := func() { b.agentMgr.ArchiveAllKickLogs("shutdown") }
	b.preShutdownHooks.add("archive-kick-logs", archiveOnShutdown)
	b.agentMgr.SetSandboxConfig(b.cfg.AgentSandbox)
	// #7421: the governor records a kick when it is dispatched; the manager
	// tells it afterwards how the turn ENDED, so a kick that produced a
	// clarifying question or a policy stand-down is not counted like one that
	// produced work (and a question earns an early re-kick).
	b.agentMgr.SetKickOutcomeObserver(func(agentName string, outcome crux.KickOutcome) {
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
	b.agentMgr.SetBuildInfo(dashboard.BuildInfo)
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
	b.agentMgr.SetRepoAutoMergeEnabledResolver(func(repo string) bool {
		return b.cfg.RepoAutoMergeEnabled(repo)
	})
	// Kick log source (#11762, #9579): governor.logging.kick_log_source from
	// Settings → Logging, with HIVE_KICK_LOG_SOURCE still winning. A closure
	// for the same reason as explain mode above — the next archive or live
	// full-log read must honour a change made from the dashboard.
	b.agentMgr.SetKickLogSourceResolver(func() string {
		return b.cfg.Governor.Logging.ResolveKickLogSource()
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
		b.agentMgr.SetSandboxPushMinter(appMinter(b.appAuth, ""))
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
	// Issue claims (hivecommons/hive#8380): who is working what, ranked so a
	// human or hub agent can take an issue over from a relay contributor.
	b.issueClaims = buildClaimsLedger(b.cfg, b.logger)
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
		// The agent-manager tier of configureGitHubClient (#9614): the
		// attribution resolver, the self-authorization hold predicate and the
		// merge re-engage hook. The App policy hooks (hive identity, the PR
		// repo policy gate, signed commits) and the review-relay
		// settings were installed by bootGitHub; every rebuild path applies
		// all of them through the same function.
		b.applyGitHubClientAgentHooks(b.ghClient)
	}
	// The relays themselves are built whether or not the App is usable yet
	// and run by a supervisor (#9621): armRequestRelays is the usable-App
	// gate, evaluated now for the boot client and again by adoptGitHubClient
	// for every rebuilt one. A hosted spoke that boots without an App starts
	// its relays when the heartbeat delivers one, and a credential rebuild
	// hands running relays over to the new client instead of leaving them on
	// the old AppAuth for the life of the process.
	//
	// authz enforces the SAME per-agent ACMM write-gate + forge-resistance as
	// the direct `gh pr create` path — the request-file route grants no extra
	// privilege. A denied request is quarantined, never opened.
	// holdLabel (F6): at hold-gated ACMM levels (L3/L4/L5) every agent-opened
	// PR must carry the "hold" label so the merge gate holds it for human
	// approval. Outreach content aimed at a repository the hive does not
	// manage is public speech into someone else's project, so it remains
	// human-reviewed at L6 too; outreach PRs into the agent's own repos are
	// ordinary hive PRs. This is decided server-side from the authenticated
	// agent identity, the PR's target repo and the authoritative hive level
	// (GetACMMLevel), NOT from a client flag — the gh-wrapper.sh tail that used
	// to add the label was dead code after `exec hive-open-pr`. L1/L2 open no
	// agent PRs (manual); non-outreach L6 PRs retain their existing automerge
	// behavior.
	holdLabel := func(agentName, repo string) bool {
		return shouldHoldAgentPR(agentName, repo, b.agentMgr.GetACMMLevel(), b.cfg.AgentOwnsRepo)
	}
	selfAuthorizationHoldEnabled := b.selfAuthorizationHoldEnabled

	// SECURITY (audit F3): re-verify the merger tier inside the sweep. The
	// dashboard's queue endpoint gates on requireMergerOrOwnerRole, but the
	// sweep merges a minute later off the label + App-authored approval body
	// alone, so without this ANY actor who can get the label applied merges
	// anything, and a sockpuppet pair defeats the self-merge ban. Resolved
	// against the SAME allowlist the dashboard uses so there is one notion of
	// trust; read through cfg on every call so a config reload takes effect.
	// ForgeState (hivecommons/hive#13377): with the cache on, the lanes read
	// its open-PR set and run on webhook events instead of listing /pulls.
	forgeOpenPRs := b.forgeStateOpenPRs()
	autoMergeOpts := automerge.Options{
		Logger:                  b.logger,
		MergerAuthorizer:        trustedMergerFunc(b.cfg),
		HumanChangeRequests:     review.DefaultHumanChangeRequests(),
		TrustedAuthorizer:       trustedAuthorFunc(b.cfg),
		TrustedAuthorPolicy:     trustedAuthorPolicyFunc(b.cfg),
		ContributorLanePolicy:   contributorLanePolicyFunc(b.cfg),
		ContributorLaneVerdict:  contributorLaneVerdictFunc(b.cfg, b.logger),
		ContributorRegistryTier: contributorRegistryTierFunc(),
		StageLabelPolicy:        stageLabelPolicyFunc(b.cfg),
		StageEngine:             stageEngineOptions(b.cfg, b.logger),
		SentinelLabel:           func() string { return b.cfg.Sentinel.LabelOrDefault() },
		SweepTrigger:            b.sweepTrigger,
	}
	if forgeOpenPRs != nil {
		autoMergeOpts.OpenPRs = forgeOpenPRs
	}
	// Resolved lazily: bootDashboard has not run yet, so b.dashSrv is nil
	// here (see lateContributorNudgeSink).
	autoMergeOpts.ContributorNudge = lateContributorNudgeSink{hub: func() *dashboard.ContributeWSHub { return b.dashSrv.ContributeHub() }}

	// commitGreen's required-checks gate (self-merge sweep, see
	// automerge_sweep.go): install the operator-declared
	// auto_merge.required_checks list, if any, so naming required checks
	// does not depend on GitHub's required-status-checks branch-protection
	// API. Older Hive App installations often lack administration:read, so
	// that API call fails closed to the coarser isMetaCheck/isIgnorableCICheck
	// allowlist. Unset/empty leaves the API/allowlist fallback chain
	// intact (SetRequiredChecks(nil) is a safe no-op).
	logDeprecatedAllowUnprotectedBase(b.cfg, b.logger)
	// The same set configureGitHubClient installed on the client via
	// syncAutoMergePolicyToGitHubClient.
	if set, ok := b.cfg.AutoMerge.RequiredCheckSet(); ok {
		autoMergeOpts.RequiredChecks = set
	}
	autoMergeOpts.RequiredChecksForRepo = b.cfg.AutoMerge.RequiredCheckSetForRepo

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
	autoMergeOpts.SelfAuthorizationHoldEnabled = selfAuthorizationHoldEnabled
	autoMergeOpts.MinHeadAge = b.cfg.AutoMerge.EffectiveMinHeadAge()
	autoMergeOpts.RepoAutoMergeEnabled = func(repo string) bool { return b.cfg.RepoAutoMergeEnabled(repo) }
	autoMergeOpts.RepoOnGitHub = func(repo string) bool { return b.cfg.RepoOnGitHub(repo) }
	// Read through b.cfg on every sweep tick so a config reload of
	// auto_merge.trusted_bot_authors takes effect without a restart.
	autoMergeOpts.TrustedBotAuthors = func() map[string]bool { return b.cfg.AutoMerge.TrustedBotAuthorSet() }
	autoMergeOpts.BotTakeoverEnabled = func() bool { return b.cfg.AutoMerge.BotTakeover.IsEnabled() }
	// Intent tier gate (#6258): the human lane only queues PRs that
	// survive writeMergeEligible's intent check, but this sweep lists
	// the App's PRs on its own, so it carries the same policy (same
	// config, same bead evidence, same BlocksMerge predicate) and asks
	// intent.EvaluateForAppSelfMerge before every self-merge.
	autoMergeOpts.IntentGate = selfMergeIntentGate(b.cfg, b.beadStores)
	// Review gate (#12592): the same review-verdicts.json the human lane
	// reads, on the re-verified head immediately before every self-merge.
	// governor.review_gate.mode (default shadow), review.require_approval
	// and review.shadow are read through b.cfg on every evaluation. The
	// audit sink resolves b.dashSrv lazily: bootDashboard has not run yet.
	autoMergeOpts.ReviewGate = selfMergeReviewGate(b.cfg, b.logger, func(e automerge.ReviewGateEvaluation) {
		if b.dashSrv != nil {
			b.dashSrv.AuditLogRecord("system", reviewEvaluatedAuditAction, reviewEvaluatedAuditDetail(e), "", e.Repo, e.Number)
		}
	})
	// Human-merge paths (#11038): read through b.cfg on every evaluation so
	// a reload of auto_merge.human_merge_paths applies without a restart.
	autoMergeOpts.HumanMergePaths = func(repo string) []string { return humanMergePathsFor(b.cfg, repo) }
	autoMergeOpts.HumanMergeRule = func() prstage.HumanMergeRule { return humanMergeRuleFor(b.cfg) }
	autoMergeOpts.RequireCurrentBase = func(repo string) bool { return b.cfg != nil && b.cfg.AutoMerge.RequireCurrentBaseFor(repo) }
	autoMergeOpts.RequireCurrentBaseStrict = func(repo string) bool {
		return b.cfg != nil && b.cfg.AutoMerge.RequireCurrentBaseStrictFor(repo)
	}
	autoMergeOpts.MergeForwardTrustedHuman = func(repo string) bool {
		return b.cfg != nil && b.cfg.AutoMerge.MergeForwardTrustedHumanFor(repo)
	}
	// Merge train (#15117): read live so a reload applies. A member the
	// train could not update-branch is handed to the fixer's rebase-resolve
	// episode (#15104): the request admits the PR to the fixer router —
	// which still requires a head the hive can push to — under the shared
	// two-attempt guard, and the lane merges the clean head on its next pass.
	// The adoption lane opens the same episode on the hive's own copy of
	// a contributor PR whose head would not replay onto the base
	// (hivecommons/hive#15307), with RequestedBy naming it.
	autoMergeOpts.RebaseResolver = automerge.RebaseResolverFunc(func(_ context.Context, req automerge.RebaseRequest) error {
		requester := mergeTrainRebaseRequester
		if req.RequestedBy != "" {
			requester = req.RequestedBy
		}
		rebasemerge.SharedAttempts().Request(req.Repo, req.Number, req.HeadSHA, requester, time.Now())
		return nil
	})
	autoMergeOpts.RebaseEpisode = func(repo string, number int) (automerge.RebaseEpisode, bool) {
		a, ok := rebasemerge.SharedAttempts().Get(repo, number)
		if !ok {
			return automerge.RebaseEpisode{}, false
		}
		return automerge.RebaseEpisode{Attempt: a.Count(), MaxAttempts: rebasemerge.MaxRebaseAttempts, Exhausted: a.Exhausted(), RequestedBy: a.RequestedBy}, true
	}
	autoMergeOpts.MergeTrain = func() automerge.MergeTrainPolicy {
		if b.cfg == nil {
			return automerge.DefaultMergeTrainPolicy
		}
		return automerge.MergeTrainPolicy{Enabled: b.cfg.AutoMerge.MergeTrain.IsEnabled(), MaxRebaseAttempts: b.cfg.AutoMerge.MergeTrain.EffectiveMaxRebaseAttempts()}
	}
	// Merge-hotspot advisor intake (hotspot_advisor.go): the train's file
	// lists and sibling conflicts are the advisor's only evidence.
	autoMergeOpts.HotspotRecorder = hotspotRecorder{store: hotspotStore()}
	b.requestRelays = newRequestRelaySupervisor(b.ctx, func(ctx context.Context, client *github.Client) <-chan struct{} {
		laneGate := relayLanePolicyGate(autoMergeOpts)(client)
		setMergeEligibleLaneGate(laneGate)
		relaysDone := deps.startRequestRelays(ctx, client, requestRelays{
			prOpen:    b.agentMgr.AuthorizePROpen,
			holdLabel: holdLabel,
			issueOpen: b.agentMgr.AuthorizeIssueOpen,
			review:    b.agentMgr.AuthorizeReviewRequest,
			merge:     bindMergeAuthz(b.agentMgr.AuthorizeMerge),
			// Lane policy on relayed merges: a contributor PR an agent asks
			// the relay to land is held to the same contributor-lane gates
			// (tier, soak, min_head_age, review verdict) the sweep applies;
			// hive-authored PRs keep the relay's existing behaviour.
			mergePolicy: relayLanePolicyGate(autoMergeOpts),
			// Per-repo forge (#14113): a repo on GitLab/Gitea merges through
			// its forge.Merger after the same merge-eligible binding; GitHub
			// repos keep the path above unchanged.
			forgeMerge: forgeMergeSelector(func() *config.Config { return b.cfg }, b.logger),
			// Push relay (#9771): the same forge-resistance + CanPush ACMM
			// gate a direct `git push` is governed by, so the relay grants
			// no privilege — it only changes which process performs the push.
			pushBranch: b.agentMgr.AuthorizePushBranch,
			logger:     b.logger,
		})
		// The ACMM verdict is re-read on every (re)start; the dashboard's
		// OnACMMLevelChanged hook restarts this generation when a runtime
		// level change crosses the self-merge boundary.
		sweepDone := deps.startSelfAuthoredSweep(ctx, client, b.cfg.AutoMerge.MaxMerges, b.cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(b.cfg.ACMMLevel), b.cfg.ACMMLevel, autoMergeOpts)
		return joinDone(relaysDone, sweepDone)
	}, b.logger)
	b.armRequestRelays(b.ghClient)

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
		if b.saved.BudgetSpend > 0 || b.saved.CoinBudgetSpend > 0 || b.saved.USDBudgetSpend > 0 || !b.saved.BudgetResetAt.IsZero() || len(b.saved.BudgetByAgent) > 0 {
			b.gov.SeedBudget(b.saved.BudgetSpend, b.saved.BudgetByAgent, b.saved.BudgetByModel, b.saved.BudgetResetAt)
			b.gov.SeedBudgetWindowBaseline(b.saved.BudgetWindowBaseline)
			b.gov.SeedCoinBudget(b.saved.CoinBudgetSpend, b.saved.CoinWindowBaseline)
			b.gov.SeedUSDBudget(b.saved.USDBudgetSpend, b.saved.USDWindowBaseline)
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
			b.observeWritebackRepos()
			b.ghClient.SetHoldLabels(b.githubHoldLabels())
			b.ghClient.SetNeverClaimLabels(b.cfg.Governor.Claims.EffectiveNeverClaimLabels())
			b.ghClient.SetSkipIssuesWithOpenPR(b.cfg.Scanner.IsSkipIssuesWithOpenPR())
			b.ghClient.SetSentinelAlertLabel(b.cfg.Sentinel.LabelOrDefault())
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
	if b.cfg.Governor.Budget.USD > 0 {
		limit := b.cfg.Governor.Budget.USD
		if b.saved != nil && b.saved.USDBudgetLimit > 0 {
			limit = b.saved.USDBudgetLimit
		}
		b.gov.SetUSDBudget(limit)
	}
	if coinCfg, ok := b.cfg.Governor.Budget.CoinConfig("bob"); ok {
		limit := coinCfg.Budget
		if b.saved != nil && b.saved.CoinBudgetLimit > 0 {
			limit = b.saved.CoinBudgetLimit
		}
		label := coinCfg.LabelOrDefault()
		if b.saved != nil && b.saved.CoinBudgetLabel != "" {
			label = b.saved.CoinBudgetLabel
		}
		b.gov.SetCoinBudget(limit, label)
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
	worksource.SetRunStageAccessor(b.dashSrv.RunStageAccessor())
	b.dashSrv.SetWritebackGate(b.writebackGate())
	b.dashSrv.SetHotspotSource(hotspotDashSource{store: hotspotStore()})
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
	// Monthly per-repo complexity/static-analysis snapshot (RFC #12062,
	// #12363). Report-only: it writes evidence files and never touches merge
	// eligibility.
	b.startComplexityRunner(newComplexityRunner(b.cfg, b.currentGitHubClient, b.logger))

	// Wire ioscan input enforcement (opt-in via ioscan.enabled) to the dashboard
	// audit log so a blocked/redacted issue title surfaces in the existing
	// audit-trail UI with no new sink. The closure keeps pkg/scheduler decoupled
	// from pkg/dashboard — the scheduler only knows a func(action, detail, agent).
	b.sched.SetAuditFunc(func(action, detail, agent string) {
		b.dashSrv.AuditLog(agent, action, detail, agent)
	})
	// Through the client provider, not a captured b.ghClient (#9621).
	b.sched.SetRunTriageDeps(b.dashSrv, liveTriageCommenter{client: b.currentGitHubClient})
	// Prior-art gate search (hivecommons/hive#13575): knowledge, the
	// Settings index and the forge, through the client provider.
	priorArtSearcher := newPriorArtSearcher(b.cfg, func() *knowledge.KnowledgeAPI { return b.knowledgeAPI }, func() []rfccoverage.Setting { return b.rfcCoverageState().index(b.logger) }, b.currentGitHubClient, b.logger)
	b.priorArtSearcher.Store(priorArtSearcher)
	b.sched.SetPriorArtSearcher(priorArtSearcher)
	b.sched.SetAdvisoryFunc(func(title, detail, agentName string) {
		if store := b.beadStoreFor(agentName); store != nil {
			if b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, agentName, ""); err == nil {
				_ = store.SetMetadata(b.ID, "ioscan_classifier", detail)
			}
		}
	})
	var ioscanAlerts ioscanClassifierAlerter
	if b.dashSrv != nil {
		ioscanAlerts = b.dashSrv
	}
	installIoscanClassifier(b.sched, ioscanAlerts, b.cfg, b.logger, time.Now)
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
	deps.enableSLAHistoryPersistence(b.dashSrv, slaHistoryPath)
	// Writes are throttled to one a minute; flush the last dirty buckets on
	// shutdown so a pod roll never drops the current day's SLA samples.
	b.preShutdownHooks.add("flush-sla-history", func() {
		if err := b.dashSrv.SLATracker().History.Flush(); err != nil {
			b.logger.Warn("sla history flush on shutdown failed", "error", err)
		}
	})

	// Intent evaluation counters (#12362): one audit-log line per evaluation
	// and a durable weekly file beside write-counters.json. Record-only;
	// nothing reads these back into merge eligibility.
	intent.SetEvaluationAuditSink(func(o intent.Observation) {
		b.dashSrv.AuditLogRecord("system", intentEvaluatedAuditAction, intentEvaluatedAuditDetail(o), "", o.Repo, o.Number)
	})
	if err := intent.MigrateCounters(intent.LegacyCountersPath, intent.CountersPath, time.Now()); err != nil {
		b.logger.Warn("intent counters: legacy file not migrated", "error", err)
	}
	b.preShutdownHooks.add("flush-intent-counters", intent.StartEvaluationCounterPersistence(intent.CountersPath, b.logger))

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
	// Installed with the rest of configureGitHubClient's dashboard tier (the
	// merge-failure alert sink, the PR-opened hook and the canary scanner),
	// here, after lifecycle persistence is enabled, so every producer records
	// into a persisted store (#9614).
	b.applyGitHubClientDashboardHooks(b.ghClient)
	// #12007: fold the last week of merged PRs onto their Fixes issues so a
	// freshly upgraded spoke does not report already-merged work as stale
	// in-progress, and start→PR / open→merge have samples on day one.
	if b.ghClient != nil && b.dashSrv != nil {
		go backfillRecentMergedPRs(b.ctx, b.ghClient, b.dashSrv, b.cfg.Project.Org, time.Now(), b.logger)
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
	if len(b.pendingCostRollupSeed) > 0 {
		b.dashSrv.SeedCostDailyRollups(b.pendingCostRollupSeed)
		b.logger.Info("cost daily rollup restored", "days", len(b.pendingCostRollupSeed))
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
		if qPath, qErr, quarantined := store.QuarantinedPath(); quarantined {
			// beads.json was unparseable (e.g. a hand-written file missing a
			// closing brace, kubestellar/hive#9328) but the store itself
			// self-healed and is usable again — the operator still needs to
			// know that this agent's PRIOR beads were lost, so this is ERROR
			// rather than the routine startup Info line below.
			b.logger.Error("beads.json was corrupt and has been quarantined; agent's bead store was reset to empty",
				"agent", name, "quarantined_to", qPath, "parse_error", qErr)
		}
		b.logger.Info("beads store initialized", "agent", name, "count", store.Count())
	}

	// Names attempted by the enabled-agent loop above, success or failure.
	// The orphan scan below must skip these regardless of outcome: a name
	// only in beadStores (the old check) let a load FAILURE fall through to
	// the orphan scan, which re-opens the same directory and re-fails it —
	// counting one broken beads.json as two failures in
	// beadStoreLoadFailures (kubestellar/hive#9328).
	attempted := make(map[string]bool, len(b.cfg.EnabledAgents()))
	for name := range b.cfg.EnabledAgents() {
		attempted[name] = true
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
			if attempted[name] {
				continue // already attempted (successfully or not) from config
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
			if qPath, qErr, quarantined := store.QuarantinedPath(); quarantined {
				b.logger.Error("beads.json was corrupt and has been quarantined; agent's bead store was reset to empty",
					"agent", name, "quarantined_to", qPath, "parse_error", qErr)
			}
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
	// Carry write_surface.enforce into the agent sandboxes (#9587). The proxy
	// refusal (#9772) stays the enforcement; this is the copy the gh wrapper
	// reads, so a lane's direct write is refused even when the request would
	// never have reached the proxy.
	b.publishWriteSurfaceEnforce()

	b.tokenCollector = tokens.NewCollector(b.cfg.Data.MetricsDir, b.logger)
	b.tokenCollector.SetClaudeSessionsDir(b.cfg.Data.ClaudeSessionsDir)
	b.tokenCollector.SetCopilotSessionsDir(b.cfg.Data.CopilotSessionsDir)
	b.tokenCollector.SetBobSessionsDir(b.cfg.Data.BobSessionsDir)
	b.tokenCollector.SetSandboxRunHistoryPath(crux.SandboxRunHistoryPath(crux.WorkDir()))
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
	// nil client + provider: every collect reads the hive's current client,
	// so a rebuilt (or first-delivered) App client is used without a restart
	// (#9621).
	b.metricsCollector = dashboard.NewMetricsCollector(nil, b.cfg.Project.Org, primaryRepo, badgeURL, b.cfg.EffectiveAIAuthor(), b.cfg.Project.Name, b.logger)
	b.metricsCollector.SetProjectScope(b.cfg.Project.Org, primaryRepo, b.cfg.Project.Repos, b.cfg.EffectiveAIAuthor())
	b.metricsCollector.SetGitHubClientProvider(b.currentGitHubClient)
	b.metricsCollector.SetCoverageTargetProvider(func() int { return b.cfg.Governor.EffectiveCoverageTarget() })
	b.metricsCollector.SetReviewEvidenceVerifierProvider(func() (github.ReviewEvidenceVerifier, error) {
		return github.NewReviewEvidenceVerifier(b.cfg.Evidence.SigningKeyPath())
	})
	b.metricsCollector.SetPRIssueWindowStartProvider(b.dashSrv.CostWindowStart)
	// Merged / closed counts move on webhook events and the Search API is
	// asked only hourly; with forgestate.enabled the Overview's open totals
	// come from the cache the governor already reads (#13376).
	wireForgeStateCounts(b.metricsCollector, b.forgeState.Load)
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
	b.fleetStatsCollector = collect.NewFleetStatsCollector(nil, fleetStatsAuthor, b.cfg.Project.Org, b.logger)
	b.fleetStatsCollector.SetGitHubClientProvider(b.currentGitHubClient) // #9621, as above
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

	// Launch attribution (#12128): the kick path records which issue / PR /
	// head SHA each delivered kick was launched for (tokenCollector.RecordLaunch
	// in runEvalCycle); this collector joins those records against the same
	// usage timeline repo-cost uses and caches /api/launch-cost. Read-only —
	// the governor window budget remains the single enforced budget.
	b.tokenCollector.SetLaunchLog(tokens.NewLaunchLog(tokens.DefaultLaunchLogPath, b.logger))
	b.launchCostCollector = collect.NewLaunchCostCollector(b.tokenCollector, b.logger)
	deps.enablePersistence("launch-cost", b.launchCostCollector, launchCostPersistPath)
	deps.startCollector(b.ctx, "launch-cost", b.launchCostCollector)

	// Persistent hourly metrics behind the Operations + Leaderboard sparklines
	// (queue depth, tasks/hour, fleet size, per-contributor completions). The
	// store loads any prior 7-day history from the /data PVC on first use and the
	// rollup goroutine samples + buckets hourly, so a rolling upgrade resumes the
	// trend instead of flattening it. Bound to ctx so it shuts down cleanly with
	// the rest of the background loops (no goroutine leak). See contribute_metrics.go.
	deps.startContributeMetrics(b.ctx, b.dashSrv)
	// Compliance posture checks (#11079): a pass every
	// compliance.posture_checks.interval while a framework is selected, with
	// the history persisted on the /data PVC.
	if deps.startCompliancePosture != nil {
		deps.startCompliancePosture(b.ctx, b.dashSrv)
	}
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
			// A restored snapshot is a previous process's read: GitHub may
			// have moved since. Serve it as last-good with the stale badge
			// so the Overview never shows pre-restart totals as current
			// (hivecommons/hive#13373); the first enumeration replaces it.
			cached.MarkAllStale()
			cached.RestoredAt = time.Now().UTC()
			b.lastActionable.Store(&cached)
			if b.sched != nil {
				b.sched.SetLastActionable(&cached)
			}
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
			Enabled:  b.cfg.Knowledge.Enabled,
			Engine:   b.cfg.Knowledge.Engine,
			Curator:  curatorConfigFromHive(b.cfg.Knowledge.Curator),
			Context7: knowledgeContext7FromHive(b.cfg.Knowledge.Context7),
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
			b.knowledgeAPIFallback = true
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
		}
	}

	// Auto-import configured document sources (PDFs, URLs as knowledge)
	for _, doc := range b.cfg.Knowledge.Documents {
		if b.knowledgeAPI == nil {
			b.knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, b.logger)
			b.knowledgeAPIFallback = true
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
		b.knowledgeAPIFallback = true
		b.logger.Info("auto-enabled file-based knowledge API")
	}
	b.bootKnowledgeConnectors(deps)
	if len(b.beadStores) > 0 {
		synthVaultPath := b.beadSynthVaultPath()
		if err := os.MkdirAll(synthVaultPath, 0o755); err != nil {
			b.logger.Warn("failed to create bead-synth vault dir", "path", synthVaultPath, "error", err)
		}
		if b.knowledgeAPI != nil {
			if connErr := b.knowledgeAPI.ConnectVault(synthVaultPath, "bead-synth-wiki"); connErr != nil {
				b.logger.Warn("failed to auto-connect bead-synth vault", "path", synthVaultPath, "error", connErr)
			} else {
				b.logger.Info("auto-connected bead-synth vault", "path", synthVaultPath)
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

			MaxAPICallsPerCycle: b.cfg.Knowledge.BeadSynthesizer.EffectiveMaxAPICallsPerCycle(),
		}, b.logger, rawGH)

		b.installBeadSynthPublisher()

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

	// Generated repository code maps (#11106); opt-in, connects its vault
	// before stores are registered with the primer below.
	b.startCodeMaps()

	// Register everything connected above with the boot-time primer. With
	// knowledge.enabled false there is none, and the stores are only primed
	// once the dashboard toggle builds one (#9231).
	if p := b.sched.GetPrimer(); p != nil {
		b.registerKnowledgeStores(p)
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
	// After the promotion scheduler so a promotion sweep can trigger a publish.
	b.bootKnowledgePublish(deps, nil)

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
		// Serialized with the live dashboard toggle (knowledgePrimerControl):
		// a primer it builds either reads this graph store from the API in
		// registerKnowledgeStores or is already published and wired here.
		b.knowledgePrimerMu.Lock()
		if b.knowledgeAPI != nil {
			b.knowledgeAPI.SetGraphStore(graphStore)
		}
		if p := b.sched.GetPrimer(); p != nil {
			p.SetGraphStore(graphStore)
			if b.knowledgeAPI != nil {
				b.knowledgeAPI.WireContext7Suggester(p)
			}
		}
		b.knowledgePrimerMu.Unlock()
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
		reviewerAutoscaler.SetHeadroomReporter(b.rotationMgr)
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
	// The fleet view reads the manager's agent table, so it exists only for
	// the in-process runtime; hostedRuntime guarantees that today, and a
	// sidecar replaces it with an API family (#14138 stage 3).
	wdFleet, wdErr := cruxruntime.NewWatchdogFleet(b.agentMgr, cruxruntime.WatchdogHooks{
		// Queue depth for the readiness gate: an agent producing nothing
		// while nothing is queued is correct, not unhealthy. Read live
		// from the governor so it reflects the current sweep.
		Queued: func() (int, bool) {
			st := b.gov.GetState()
			return st.QueueIssues + st.QueuePRs, true
		},
		// Agents the governor deliberately does not kick are idle by
		// design, not "alive but not producing" (#11910).
		KickSuppressed: func(name string) string {
			mode := strings.ToLower(string(b.gov.GetState().Mode))
			ac, ok := b.cfg.Agents[name]
			onDemand := (ok && ac.OnDemand) || b.onDemandFromPack[name]
			if b.cfg.OffByCadence(name, mode, onDemand) {
				return fmt.Sprintf("cadence paused in %s mode", mode)
			}
			return b.sched.ReviewerLaneIdleReason(name)
		},
		// The no-production window is never shorter than the agent's own
		// cadence: a daily lane is on schedule for a day (#13817).
		CadencePeriod: func(name string) (time.Duration, bool) {
			mode := strings.ToLower(string(b.gov.GetState().Mode))
			return b.cfg.CadenceValueForMode(name, mode).Period(time.Now())
		},
	})
	switch {
	case !wdSettings.Enabled():
		b.logger.Info("agent watchdog disabled by config", "mode", string(wdSettings.Mode))
	case wdErr != nil:
		b.logger.Error("agent watchdog disabled: fleet view unavailable", "error", wdErr)
	default:
		b.wd = watchdog.New(wdSettings, wdFleet, b.dashSrv, b.logger,
			watchdog.WithAuthProbes(watchdogAuthProbes(b.cfg)),
			watchdog.WithConditionReporter(fleetreport.DefaultReporter{}))
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
	b.linearCredentialResolver = func() crux.LinearCredential {
		if tok := b.dashSrv.LinearAgentAccessToken(); tok != "" {
			return crux.LinearCredential{AccessToken: tok}
		}
		if b.cfg.Governor.WorkSource.Type == "linear" {
			return crux.LinearCredential{APIKey: strings.TrimSpace(b.cfg.Governor.WorkSource.Linear.APIKey)}
		}
		return crux.LinearCredential{}
	}
	b.agentMgr.SetLinearCredentialResolver(b.linearCredentialResolver)

	// In-flight ledger + session PR link (Linear GitHub-parity follow-ups):
	// the scheduler withholds work a Linear session is already working, and
	// the pr-request watcher narrates opened PRs into the session.
	// #8380: the worker-claim ledger is a second in-flight source — an issue a
	// human, contributor or another agent holds is withheld from kicks too.
	// #10527: a free issue an agent already claimed escalate_after_claims
	// times with nothing moving is withheld and escalated instead of
	// re-claimed, and a likely-done issue an agent already verified after the
	// merge is withheld until the reporter or a maintainer acts.
	b.sched.SetInflightLookup(composeInflight(b.dashSrv.LinearSessionHolder,
		contributorClaimOverrideLookup(contributorClaimGuard.Load, claimsInflightLookup(b.issueClaims, b.cfg.Project.Org)),
		verifyOnceLookup(b.ctx, b.cfg.Project.Org, b.issueClaims, func() *github.Client { return b.ghClient }, b.logger),
		claimEscalationLookup(b.ctx, b.cfg, b.issueClaims, func() *github.Client { return b.ghClient }, b.logger)))
	// #9584: close answered question issues unless the author objects (default off).
	b.questionAutoclose.Store(wireQuestionAutoclose(b.ctx, b.cfg, b.sched, func() *github.Client { return b.ghClient }, b.logger))
	// The pr-request watcher's PR-opened hook (Linear session narration, the
	// lifecycle timeline) and the #9583 PR-opened detail hook (follow-up session
	// pointer and handoff note) are installed
	// by configureGitHubClient's dashboard tier (#9614), so a rebuilt client
	// keeps it.
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
	// The hive's own unblock (blocked sweep, review-backlog block lift)
	// re-reads the issue into the cached snapshot and the Activity Monitor
	// the way an inbound label webhook does (hivecommons/hive#14952).
	github.SetItemRefreshHook(b.dashSrv.RefreshSelfEditedItem)
	wireSpektacularRunner(b.cfg, b.dashSrv, b.logger)
	// #8380: chain GitHub comments/labels behind the relay yank on takeover.
	ghClientFn := func() *github.Client { return b.ghClient }
	b.dashSrv.InstallClaimHooks(gfiClaimHooks(b.ctx, b.cfg, b.gfiState(), ghClientFn, b.dashSrv, b.logger,
		githubClaimHooks(b.ctx, b.cfg, ghClientFn, b.logger)))
	b.dashSrv.SetGoodFirstIssueStats(func() any { return b.gfiState().sweeper.Stats() })
	installClaimAdmissionCheck(b.ctx, b.issueClaims, func() *github.Client { return b.ghClient })
	releaseClaimsOnAgentRelaunch(b.agentMgr, b.dashSrv.IssueClaims, b.logger)
	installContributorClaimGuard(b.cfg, b.dashSrv, b.logger)
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
	b.dashSrv.SetUpgradeDrainSource(selfUpgradeDrain.Report)
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
				// #11791: heal a wrong-account installation BEFORE asking the
				// installation about coverage. After an org transfer the
				// configured org no longer matches the installation's account,
				// and the coverage check below — asked of the OLD installation
				// — answers "repo not covered" and returns, so Re-check could
				// never adopt the new org's installation without a restart.
				// Rediscovery is TTL-cached, so this costs nothing on repeat.
				healGitHubAppInstallation(b.ctx, b.ghClient.AppAuth(), b.cfg, b.logger)
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

	// The authorized issue publisher (#8353) needs the ACMM level applied
	// above, the mutation boundary from bootAdvisory, and the dashboard's
	// audit sink, so it is composed here rather than beside the boundary.
	b.wireFindingPublisher()
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
		b.cfgReloadMu.Lock()
		prevACMMLevel := b.cfg.ACMMLevelOrZero()
		nextACMMLevel := prevACMMLevel
		defer func() {
			b.cfgReloadMu.Unlock()
			if prevACMMLevel != nextACMMLevel {
				b.onACMMLevelChanged(prevACMMLevel, nextACMMLevel)
			}
		}()

		// Preserve runtime-only fields that are not in the YAML
		newCfg.HiveID = b.cfg.HiveID
		// The reviewer autoscaler's current lane size survives a reload so
		// an edit elsewhere in hive.yaml does not snap reviewers back to the
		// declared count (hivecommons/hive#14685).
		newCfg.ReviewerAutoscaleActive = b.cfg.ReviewerAutoscaleActive

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
		nextACMMLevel = b.cfg.ACMMLevelOrZero()

		// Re-sync subsystems that cache config values
		b.ghClient.SetRepos(b.cfg.Project.Repos)
		b.observeWritebackRepos()
		if b.metricsCollector != nil {
			b.metricsCollector.SetProjectScope(b.cfg.Project.Org, metricsPrimaryRepo(b.cfg.Project), b.cfg.Project.Repos, b.cfg.EffectiveAIAuthor())
		}
		syncAutoMergePolicyToGitHubClient(b.cfg, b.ghClient)
		syncStageEngineMode(b.cfg, b.ghClient, b.logger)
		b.gov.UpdateConfig(b.cfg.Governor)
		if coinCfg, ok := b.cfg.Governor.Budget.CoinConfig(tokens.BackendBob); ok {
			b.gov.SetCoinBudget(coinCfg.Budget, coinCfg.LabelOrDefault())
		}
		b.gov.SetUSDBudget(b.cfg.Governor.Budget.USD)
		// governor.claims.*_ttl_s apply to claims made or renewed after the
		// reload; live claims keep their expiry (#10981).
		b.issueClaims.SetPolicy(b.cfg.Governor.Claims.Policy())
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
			Approvals: newHookApprovalAdapter(b.approvalInbox, b.cfg.ACMMLevelOrZero()),
		}, b.logger)

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

		// Republish write_surface.enforce for the sandboxes (#9587). Listing
		// or unlisting a lane in hive.yaml takes effect on the next gh call,
		// with no agent relaunch — the same liveness the proxy predicate has.
		b.publishWriteSurfaceEnforce()

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
					// #9614: every boot-time hook, not a hand-kept subset.
					newClient := b.newConfiguredGitHubAppClient(newAppAuth)
					// #9621: every consumer follows (sandbox, relays, collectors).
					b.adoptGitHubClient(newClient, newAppAuth)
					// Immediate per-agent token delivery — see #4072.
					deps.refreshAgentTokens(b.ctx, b.agentMgr)
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
	config.SetSaveObserver(b.configWatcher.SkipNext)
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
		Approvals: newHookApprovalAdapter(b.approvalInbox, b.cfg.ACMMLevelOrZero()),
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
	dashboard.SetAgentRefreshableProvider(b.agentMgr.AgentCredentialRefreshable)

	// PR follow-up resume counters on /metrics (#9583).
	dashboard.SetPRFollowUpCountersProvider(prFollowUpMetricsCounters)

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

	// The GitHub client's canary scanner is installed by
	// configureGitHubClient's dashboard tier (#9614); the proxy shares its
	// leak handler.
	canaryLeakHandler := b.handleCanaryLeak

	b.githubProxy, err = deps.newGitHubProxy(b.logger, b.cfg.Project.Org, b.cfg.Project.Repos)
	if err != nil {
		b.logger.Error("failed to create github proxy", "error", err)
	} else {
		b.githubProxy.SetCanaryScanner(b.cfg.Ioscan.IsEnabled() && b.cfg.Ioscan.CanariesEnabled(), b.cfg.Ioscan.FailClosedAtLevel(b.cfg.ACMMLevelOrZero()), ioscan.DefaultCanaries, canaryLeakHandler)
		// REST accounting and the shared core-bucket snapshot the agent
		// GitHub budget reads live in pkg/github; the proxy takes them through
		// a seam (#13536).
		b.githubProxy.SetGitHubRESTAccounting(githubRESTAccounting{})
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
		b.githubProxy.SetRepoAutoMergeEnabledFunc(b.cfg.RepoAutoMergeEnabled)
		// Per-lane write_surface.enforce (#9772, default off): a listed lane's
		// direct GitHub writes are refused here and audited as
		// agent_write_refused, so the audited relays are its only write path.
		// Unlisted lanes are unaffected.
		b.githubProxy.SetWriteSurfaceEnforceFunc(b.cfg.WriteSurfaceEnforced)
		b.githubProxy.SetWriteRefusedAuditFunc(func(agentName, kind, method, path, repo string) {
			rec := github.DirectWriteRefusedAuditRecord(agentName, kind, method, path, repo)
			b.dashSrv.AuditLogRecord("system", rec.Action, rec.Detail, rec.Agent, rec.Repo, rec.Target)
		})
		// #1861: the proxy resolves an identified agent to its hub-held scoped
		// token via the package-level registry WriteAgentToken feeds (NOT via
		// the appAuth instance, which is replaced on key rotation — a closure
		// over it would strand the proxy on the stale instance). Wired
		// unconditionally: with HIVE_PROXY_INJECT_GH_AUTH unset (the default)
		// the proxy never consults the source and the registry stays empty.
		b.githubProxy.SetAgentTokenSource(github.AgentProxyToken)
		// Durable attempted-vs-blocked write counters (#12069); observation only.
		if err := proxy.MigrateWriteCounters(proxy.LegacyWriteCountersPath, proxy.WriteCountersPath, time.Now()); err != nil {
			b.logger.Warn("write counters: legacy file not migrated", "error", err)
		}
		stopWriteCounters := b.githubProxy.StartWriteCounterPersistence(proxy.WriteCountersPath)
		b.preShutdownHooks.add("flush-write-counters", stopWriteCounters)
		b.githubProxy.SetAgentGitHubBudgetConfig(b.cfg.AgentGitHubAPIHourlyCap, b.cfg.GitHubAgentReserveFloor)
		if b.agentMgr != nil {
			b.githubProxy.SetAgentGitHubBudgetNudgeFunc(b.agentMgr.SendGitHubAPIBudgetNudge)
		}
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
		// #11761: the full per-agent inference error the proxy returned, so the
		// agent card is not limited to Claude Code's clipped retry line.
		dashboard.SetInferenceLastErrorProvider(b.githubProxy.LastInferenceError)
		// #4294: the provider spending-limit signal, read by the eval cycle to
		// raise an advisory and stop kicking agents at a gateway that is
		// refusing on a money limit.
		dashboard.SetInferenceBudgetProvider(b.githubProxy.InferenceBudgetExceeded)

		// Wire the inference token sink so the translator records per-agent
		// usage (from the gateway's OpenAI usage block) into the same metrics
		// dir the token collector scans. Without this, bare-mode inference
		// agents (litellm/vllm/llm-d) never write a scannable session file and
		// their consumption reads as zero. ONE sink: it owns each agent's
		// usage file and rewrites it from in-memory totals, so a second
		// instance on the same dir would clobber the first's numbers.
		inferenceSink := tokens.NewInferenceSink(b.cfg.Data.MetricsDir, b.logger)
		b.githubProxy.SetTokenSink(inferenceSink)
		b.setInferenceSink(inferenceSink)
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
		llmdEndpoints := parseEndpointList(envOrDefault("HIVE_LLMD_ENDPOINT", defaultLLMDEndpoint))
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
						DefaultModel: gw.DefaultModel,
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
					// The backend's own default (resolved from the same source as
					// the endpoint) rides along so the proxy can fall back to it
					// when the per-agent model is one the gateway's team is not
					// entitled to (hivecommons/hive#9945).
					_, defaultModel, _ := resolveLiteLLMInferenceRoute(b.cfg, backend, "")
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
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						DefaultModel: defaultModel,
						APIKey:       apiKey,
						CABundle:     caBundle,
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
						DefaultModel: gw.DefaultModel,
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
		// Jev decision endpoint (hivecommons/hive#8939). Always bound so a
		// jev_mode toggle in the dashboard takes effect on the agent's next
		// call; every predicate reads the LIVE config, and an agent whose
		// jev_mode is off is refused before any key is resolved or any
		// provider traffic happens. Identity comes from the proxy's UID
		// lookup, budget from the same inference sink the translator feeds,
		// audit from the dashboard's agent audit sink.
		if deps.startJev != nil {
			cfg := b.cfg
			deps.startJev(&jev.Server{
				Identify: b.githubProxy.IdentifyAgentByUID,
				Enabled:  cfg.JevAssistEnabled,
				Key:      cfg.ResolveJevAPIKey,
				Timeout:  cfg.Jev.EffectiveTimeout(),
				Client:   jev.NewClient(func() config.JevConfig { return cfg.Jev }, nil),
				Usage:    inferenceSink,
				Audit:    b.dashSrv.AgentAuditSink(),
				Logger:   b.logger,
			}, b.logger)
		}
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

// bootLaunchWith is bootLaunch with its goroutines, Discord bot, stagger
// wait, and agent starts injected; see bootLaunchDeps.
func (b *boot) bootLaunchWith(deps bootLaunchDeps) {
	deps.spawn("dashboard-serve", func() {
		if err := deps.serve(b.dashSrv); err != nil {
			b.logger.Error("dashboard server failed", "error", err)
		}
	})

	if b.cfg.Notifications.Discord != nil && b.cfg.Notifications.Discord.BotToken != "" && b.cfg.Notifications.Discord.ChannelID != "" {
		var agentNameList []string
		for name := range b.cfg.EnabledAgents() {
			agentNameList = append(agentNameList, name)
		}
		sendDiscord, err := deps.startDiscordBot(b.ctx, discord.Config{
			Token:          b.cfg.Notifications.Discord.BotToken,
			ChannelID:      b.cfg.Notifications.Discord.ChannelID,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", b.cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
			AllowedUsers:   b.cfg.Notifications.Discord.AllowedUsers,
		}, agentNameList, b.logger)
		if err != nil {
			b.logger.Warn("discord bot failed to start", "error", err)
		} else {
			b.dashSrv.SetSwarmAnnouncer(sendDiscord)
			b.logger.Info("discord bot started", "channel", b.cfg.Notifications.Discord.ChannelID)
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
	startupAgents := []string{}
	if sharedAgentPoolEnabled() {
		b.logger.Info("shared agent pool enabled; persistent agents will start on demand")
	} else {
		startupAgents = startupLaunchNames(enabledAgents, b.onDemandFromPack)
	}
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
	// The hub re-pushes its project config every beat, so a refused
	// misconfiguration is logged at ERROR once and at DEBUG while unchanged.
	var refusedProjectConfig atomic.Value // string key of the last refused push
	// Hive-self scheduler (#12397): default off; restores durable reverts
	// before the first heartbeat can apply a self-experiment flag.
	startNousSelfScheduler(b.ctx, b.cfg, b.logger)
	// Start hub heartbeat push if configured (env var or config)
	hubTgt := resolveHubTarget(b.cfg.Hub, os.Getenv("HIVE_HUB_URL"), os.Getenv("HIVE_CLUSTER_ID"))
	b.hubURL = hubTgt.url
	b.cfg.Hub.Enabled, b.cfg.Hub.URL, b.cfg.Hub.ClusterID = hubTgt.enabled, hubTgt.url, hubTgt.clusterID
	spoke.SetHeartbeatOmit(b.cfg.Hub.HeartbeatOmit)
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
			// Every sub-collector below is a named TimedSection so that when the
			// collect exceeds its budget the spoke logs WHICH one ate it (see
			// collectWithTimeout). Keep new collectors wrapped the same way.
			govState := spoke.TimedSection("governor_state", b.gov.GetState)
			currentMode := strings.ToLower(string(govState.Mode))
			agents := spoke.TimedSection("agents", func() []spoke.AgentSummary { return b.heartbeatAgents(govState, currentMode, true) })
			acmmLvl := b.heartbeatACMMLevel()
			prsMerged, prsRejected, cvesClosed, fleetStatsCollectedAt := b.heartbeatFleetStats()
			repoActivity, repoActivityCollectedAt, repoActivityWindowHours, repoActivityCountWindowHours := b.heartbeatRepoActivity()
			// Count agents with a method/model assigned for the hub's
			// user-journey stage detection. Always a non-nil pointer from a
			// spoke new enough to compute it, so the hub can distinguish
			// "genuinely zero agents configured" from "old spoke, unknown".
			agentsWithModel := spoke.TimedSection("agents_with_model", b.agentMgr.CountAgentsWithModel)

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
			awaitingReview := spoke.TimedSection("planning", func() *int {
				if planning := dashboard.BuildPlanning(b.beadStores, false, acmmLvl); planning.Available {
					n := planning.AwaitingReview
					return &n
				}
				return nil
			})

			// Contributor-relay tasks over the trailing 7d, summed from the
			// spoke's own 168 hourly buckets. nil until the store exists; a
			// zero from an existing store is a real "no contributor finished
			// anything" reading.
			tasksCompleted7d := spoke.TimedSection("tasks_completed_7d", func() *int {
				if n, ok := b.dashSrv.TasksCompleted7d(); ok {
					return &n
				}
				return nil
			})

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
			agentErrorStreaks := spoke.TimedSection("agent_error_streaks", b.tokenCollector.AgentErrorStreaks)
			consentWedged := spoke.TimedSection("consent_wedged", b.agentMgr.ConsentWedgedAgents)
			noCadenceAgents := spoke.TimedSection("no_cadence_agents", b.gov.NoCadenceAgents)

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
				GatewayNames: spoke.TimedSection("gateway_names", b.dashSrv.ConfiguredGatewayNames),
				// Hash only, never the raw token: lets the hub verify this
				// spoke's upgrade-proof credential without reading the
				// hive-secrets secret from a cluster it may not reach
				// (push-reported). Empty when no token is configured.
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
				OpenFDs:     spoke.TimedSection("open_fds", spoke.OpenFDCount),
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
				Providers:               spoke.TimedSection("providers", func() []crux.ProviderStatus { return heartbeatProviders(b.cfg, agents) }),
				LastWriteCapableKickAt:  lastWriteKickAt,
				LastKickDisposition:     kickDisposition,
				LastKickSkipReason:      kickSkipReason,
				NotWritableQueued:       notWritableQueued,
				RepoTargetMisconfigured: spoke.TimedSection("repo_target_misconfigured", b.repoTargetMisconfigured),
				RepoTargetIssue:         spoke.TimedSection("repo_target_issue", b.repoTargetIssueMessage),
				Repos:                   b.cfg.Project.Repos,
				PrimaryRepo:             b.cfg.Project.PrimaryRepo,
				ACMMLevel:               acmmLvl,
				Agents:                  agents,
				Governor: func() spoke.GovernorSummary {
					openIssues, openPRs := b.gov.LatestRepoTotals()
					return spoke.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs, OpenIssues: openIssues, OpenPRs: openPRs, WorkSource: func() string {
						if t := b.cfg.Governor.WorkSource.Type; t != "" && t != "github" {
							return t
						}
						return ""
					}()}
				}(),
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
				Leaderboard: spoke.TimedSection("leaderboard", b.leaderboardForHeartbeat),
				// Report who has a live dashboard session so the hub can accumulate
				// per-user "time in hive". Bare usernames only — never session
				// ids/tokens (ActiveSessionUsernames guarantees this).
				ActiveSessionUsers: spoke.TimedSection("active_session_users", b.dashSrv.ActiveSessionUsernames),
				// The honest subset of the above: users whose browser reported
				// focused, recent-input presence (see dashboard/presence.go).
				// An idle open tab appears in ActiveSessionUsers but not here.
				EngagedSessionUsers: spoke.TimedSection("engaged_session_users", b.dashSrv.EngagedSessionUsernames),
				// Per-user last audit-logged real action, so the hub can tell
				// users who DO things from users who merely stay logged in.
				UserLastActions: spoke.TimedSection("user_last_actions", b.dashSrv.UserLastActions),
				Owner:           spoke.TimedSection("owner", b.ownerForHeartbeat),
				// Report the API URL we are actually running against so the hub
				// can see whether a GitHub Enterprise API URL it delivered has
				// landed. Resolved (never empty) so the hub can distinguish
				// "public github.com" from "spoke too old to report this".
				GitHubAPIURL: b.cfg.GitHub.ResolvedAPIURL(),
				Health:       spoke.TimedSection("health", b.dashSrv.HealthSummary),
				DashboardURL: spoke.TimedSection("dashboard_url", b.dashboardURLForHeartbeat),
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
				ImageRef: spoke.TimedSection("image_ref", spoke.SelfDeploymentImage),
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
				UpgradeDrain:             selfUpgradeDrain.Report(selfUpgradeDrainNow()),
				ClusterHealth: spoke.TimedSection("cluster_health", func() *spoke.HeartbeatClusterHealthReport {
					if !shouldCollectHeartbeatClusterHealth(b.cfg.Hub.ClusterID) {
						return nil
					}
					return spoke.CollectClusterHealth(b.logger)
				}),
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
				GitHubAppKeyFingerprint: spoke.TimedSection("app_key_fingerprint", func() string { return appKeys.ReportedFingerprint(b.cfg.GitHub.KeyFile, b.cfg.GitHub.AppID) }),
				GitHubAppKeyPerHive:     spoke.TimedSection("app_key_per_hive", func() bool { return appKeys.HasPerHiveKey(b.cfg.GitHub.KeyFile, b.cfg.GitHub.AppID) }),
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
				GitHubAppKeysHeld: spoke.TimedSection("app_keys_held", appKeys.HeldFingerprints),
				// Component reach counters (#3993, phase 2a of #3973): per
				// (component, running commit) span counts aggregated in-process,
				// exporter or not (D2) — the heartbeat is the only channel that
				// reaches every spoke, push-reported ones included (D1). nil until
				// the first span, which the hub reads as "no data", never as
				// zero reach. Capped at tracing.MaxReachComponents entries.
				ComponentReach: spoke.TimedSection("component_reach", tracing.ReachSnapshot),
				// Compliance profile + latest posture pass/fail (#11083); an
				// empty block when compliance is not configured.
				Compliance: spoke.TimedSection("compliance", b.dashSrv.ComplianceHeartbeat),
				// 90-day evidence-pipeline summary + self-authored auto-merge
				// posture (#12114); nil when bundles are unreadable.
				Evidence: spoke.TimedSection("evidence", b.dashSrv.EvidenceHeartbeat),
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

				if exists, verified := selfUpgradeTargetImageAvailable(targetSHA, b.logger); verified && !exists {
					b.logger.Info("self-upgrade queued: target image is not published yet",
						"target", targetSHA,
						"current", gitShort)
					if err := os.Remove(upgradeMarkerPath); err != nil && !os.IsNotExist(err) {
						b.logger.Warn("failed to clear upgrade marker for unpublished target", "path", upgradeMarkerPath, "error", err)
					}
					return
				}

				// Drain in-flight kicks before exiting (#11876): on a fast release
				// channel the restart cadence is shorter than a long-lane turn, so
				// exiting immediately cut most of those turns. While any kicked
				// turn is still running, defer (the hub re-sends UpgradeTo every
				// heartbeat) and withhold new kicks; proceed once the turns end or
				// hub.upgrade_drain_timeout_s elapses.
				if b.agentMgr != nil {
					statuses := b.agentMgr.AllStatuses()
					inFlight, wedged := agentKicksInFlight(statuses, b.startTime, selfUpgradeDrainNow(), func(name string) time.Duration {
						if ac, ok := b.cfg.Agents[name]; ok {
							return ac.EffectiveContinuousTurnCeiling()
						}
						return 0
					})
					for _, w := range selfUpgradeWedgedKicks.fresh(wedged) {
						b.logger.Warn("self-upgrade drain: ignoring wedged agent kick that never reported a turn end",
							"agent", w.Agent,
							"kick_at", w.KickAt.UTC().Format(time.RFC3339),
							"age", w.Age.Round(time.Second),
							"ceiling", w.Ceiling,
						)
					}
					ignoredWedged := wedgedKickAgents(wedged)
					drainTimeout := b.cfg.Hub.EffectiveUpgradeDrainTimeout()
					if spoke.UpgradeInterruptRequested() && drainTimeout > 0 && len(inFlight) > 0 {
						// The owner chose "Upgrade now (interrupt turns)" on the
						// hub: cut the drain rather than wait for these turns.
						b.logger.Warn("self-upgrade: interrupting in-flight agent turns on the owner's request",
							"target", targetSHA, "in_flight", inFlight)
						drainTimeout = 0
					}
					dec := selfUpgradeDrain.decide(targetSHA, inFlight, drainTimeout, selfUpgradeDrainNow())
					selfUpgradeDrain.setKickTimes(agentKickTimes(statuses, inFlight))
					switch {
					case !dec.Proceed:
						b.logger.Info("self-upgrade deferred: draining in-flight agent kicks",
							"target", targetSHA,
							"current", gitShort,
							"in_flight", dec.InFlight,
							"waited", dec.Waited.Round(time.Second),
							"max_wait_remaining", dec.Remaining.Round(time.Second),
							"drain_timeout", drainTimeout,
							"ignored_wedged", ignoredWedged,
						)
						return
					case dec.TimedOut:
						b.logger.Warn("self-upgrade drain timed out: proceeding and cutting in-flight kicks",
							"target", targetSHA,
							"current", gitShort,
							"cut_kicks", dec.InFlight,
							"waited", dec.Waited.Round(time.Second),
							"drain_timeout", drainTimeout,
							"ignored_wedged", ignoredWedged,
						)
					case dec.Waited > 0 || len(ignoredWedged) > 0:
						b.logger.Info("self-upgrade drain complete: no agent kicks in flight",
							"target", targetSHA,
							"current", gitShort,
							"waited", dec.Waited.Round(time.Second),
							"ignored_wedged", ignoredWedged,
						)
					}
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
						Providers:               heartbeatProviders(b.cfg, agents),
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
					// The patch did not go through. When the Deployment tracks a
					// floating tag that the kubelet re-pulls on container start,
					// a plain process exit still lands the upgrade — so skip the
					// RolloutRestartSelf PATCH (same API, same admission webhook,
					// same failure, another timeout wait) and exit NOW, reporting
					// pending-restart rather than failed. On vllm-d a Kyverno
					// webhook timed out every Deployment patch; the upgrade
					// landed on each restart yet the hub showed "failed 3
					// attempts" with a floating-tag hint (#14385).
					fb := spoke.RestartFallbackForPatchFailure(b.logger, err)
					recordUpgradeError(upgradeMarkerPath, fmt.Errorf("%s: %w", fb.Hint, err), b.logger)
					if fb.RestartLands {
						b.logger.Warn("self-upgrade: Deployment patch failed, exiting so the kubelet re-pulls the floating tag",
							"target", targetSHA,
							"current", gitShort,
							"kind", string(fb.Kind),
							"hint", fb.Hint,
							"error", err,
						)
						spoke.ReportUpgradePendingRestart(b.hubURL, b.cfg.HiveID, targetSHA, gitShort, fb.Hint, b.logger)
						os.Exit(selfUpgradePendingRestartExitCode)
					}
					b.logger.Warn("pinned-image upgrade failed, falling back to rolling restart",
						"target", targetSHA, "kind", string(fb.Kind), "hint", fb.Hint, "error", err)
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
						hint := spoke.PatchFailureHint(spoke.ClassifyPatchFailure(err))
						b.logger.Error("self-upgrade FAILED: could not patch own Deployment, restarting onto the same image",
							"target", targetSHA,
							"current", gitShort,
							"error", err,
							"hint", hint,
						)
						recordUpgradeError(upgradeMarkerPath, err, b.logger)
						spoke.ReportUpgradeFailure(b.hubURL, b.cfg.HiveID, targetSHA, gitShort, hint+": "+err.Error(), b.logger)
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
					changed, err := applyDeliveredPrimaryKey(b.logger, keyPath, ghCfg.PrivateKey)
					if err != nil {
						b.logger.Error("failed to write github app key from heartbeat", "path", keyPath, "error", err)
						return
					}
					keyChanged = changed
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
					// #9614: every boot-time hook, not a hand-kept subset.
					newClient := b.newConfiguredGitHubAppClient(newAppAuth)
					// #9621: every consumer follows (sandbox, relays, collectors).
					// On a spoke that booted without an App this is also where
					// the request relays start for the first time.
					b.adoptGitHubClient(newClient, newAppAuth)
					// Immediate per-agent token delivery: hosted spokes get their
					// App creds via this heartbeat path AFTER agents have already
					// launched (with empty 0-byte caches), so waiting for the next
					// 40-minute tick guarantees a window of gh 401s (#4072).
					go b.agentMgr.RefreshAgentTokens(b.ctx)
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
			// Whole-hive pause/resume requested from the hub's per-hive menu
			// (#15092). Same fleet-breaker calls as the dashboard's
			// /api/breaker/{engage,release}; the run loop's regular persist
			// makes the result durable.
			spoke.BreakerCallback(func(action string) {
				switch action {
				case spoke.BreakerEngage:
					paused := b.agentMgr.EngageBreaker()
					b.dashSrv.AuditLog("hub", "breaker-engage", "count="+strconv.Itoa(len(paused))+", agents="+strings.Join(paused, ","), "")
					b.logger.Info("audit: fleet breaker engaged from hub", "agents", paused)
				case spoke.BreakerRelease:
					resumed := b.agentMgr.ReleaseBreaker(b.ctx)
					b.dashSrv.AuditLog("hub", "breaker-release", "count="+strconv.Itoa(len(resumed))+", agents="+strings.Join(resumed, ","), "")
					b.logger.Info("audit: fleet breaker released from hub", "agents", resumed)
				default:
					b.logger.Warn("unknown breaker instruction from hub; ignoring", "action", action)
				}
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
					key := fmt.Sprintf("%q|%q|%q|%s", pc.Org, pc.Repos, pc.PrimaryRepo, issue.Message)
					logRefusal := b.logger.Error
					if prev, _ := refusedProjectConfig.Swap(key).(string); prev == key {
						logRefusal = b.logger.Debug
					}
					logRefusal("REFUSING hub project config: repo target is misconfigured — project left unchanged",
						"error", issue.Message,
						"pushed_org", pc.Org,
						"pushed_repos", pc.Repos,
						"pushed_primary_repo", pc.PrimaryRepo,
					)
					return
				}
				refusedProjectConfig.Store("")
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
				prevLevel := b.cfg.ACMMLevelOrZero()
				level := pc.ACMMLevel
				b.cfg.ACMMLevel = &level

				// Re-sync the GitHub client that caches the repo list (mirrors the
				// config-watcher reload path). The issue filter is cached the same
				// way, so re-install it too — a hub-delivered filter must take
				// effect on the next enumeration, not the next restart.
				b.ghClient.SetRepos(b.cfg.Project.Repos)
				b.observeWritebackRepos()
				if b.metricsCollector != nil {
					b.metricsCollector.SetProjectScope(b.cfg.Project.Org, metricsPrimaryRepo(b.cfg.Project), b.cfg.Project.Repos, b.cfg.EffectiveAIAuthor())
				}
				b.ghClient.SetHoldLabels(b.githubHoldLabels())
				b.ghClient.SetNeverClaimLabels(b.cfg.Governor.Claims.EffectiveNeverClaimLabels())
				b.ghClient.SetSkipIssuesWithOpenPR(b.cfg.Scanner.IsSkipIssuesWithOpenPR())
				b.ghClient.SetSentinelAlertLabel(b.cfg.Sentinel.LabelOrDefault())
				b.ghClient.SetIssueFilter(b.cfg.Project.IssueFilter)
				syncAutoMergePolicyToGitHubClient(b.cfg, b.ghClient)

				// Persist to the PVC overlay so the claim survives a pod restart
				// (config save writes the overlay hive.yaml, same as level switches).
				if err := b.cfg.Save(); err != nil {
					b.logger.Error("failed to save claimed project config", "error", err)
				}
				if prevLevel != level {
					b.onACMMLevelChanged(prevLevel, level)
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
			spoke.ClusterIDCallback(func(clusterID string) {
				// The hub knows which cluster this hive runs on and sends it while
				// we report none — spokes provisioned before hub.cluster_id was
				// rendered (#11432) on pull_only clusters the hub cannot re-render.
				// Adopting it gates cluster_health on from the next beat; persist
				// so it survives restarts.
				if !adoptHubDeliveredClusterID(&b.cfg.Hub, clusterID) {
					return
				}
				b.logger.Info("adopted cluster_id from hub heartbeat; cluster_health will be reported from the next beat",
					"cluster_id", b.cfg.Hub.ClusterID)
				if err := b.cfg.Save(); err != nil {
					b.logger.Error("failed to save hub-delivered cluster_id", "error", err)
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
				openIssues, openPRs := b.gov.LatestRepoTotals()
				return &spoke.HeartbeatPayload{
					HiveID:                  b.cfg.HiveID,
					Org:                     b.cfg.Project.Org,
					Repos:                   b.cfg.Project.Repos,
					PrimaryRepo:             b.cfg.Project.PrimaryRepo,
					ACMMLevel:               acmmLvl,
					Agents:                  agents,
					Governor:                spoke.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs, OpenIssues: openIssues, OpenPRs: openPRs},
					Health:                  freshHeartbeatHealthSummary(agents),
					ProviderLimitReason:     providerLimitReason,
					ProviderLimitRebuffs:    providerLimitRebuffs,
					ProviderLimitHiveWide:   providerLimitHiveWide,
					ProviderLimitAgents:     providerLimitAgents,
					Providers:               heartbeatProviders(b.cfg, agents),
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

		if b.cfg.Hub.TaskStatusPushEnabled() {
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
		} else {
			b.logger.Info("hub task-status push disabled by operator")
		}
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
			dashboard.SetTrajectoryCountersProvider(trajectoryMetricsCounters(b.trajLane))
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
	for _, rp := range b.cfg.Project.RepoPolicies {
		if rp.ACMMLevel != nil {
			repo := config.QualifyRepo(b.cfg.Project.Org, rp.Repo)
			proxy.SetAutonomyRepoLevel(repo, b.cfg.EffectiveACMMLevelForRepo(repo))
		}
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
		autonomyNotify := func(title, message string, demote bool) {
			if b.notifier == nil {
				return
			}
			priority := notify.PriorityDefault
			if demote {
				priority = notify.PriorityHigh
			}
			b.notifier.Send(title, message, priority)
		}
		b.retroLane.SetAutonomyPolicy(retro.NewAutonomyPolicyEngine(b.cfg, retroStore, dashboard.NewAutonomyDecisionSink(b.dashSrv, autonomyNotify, proxy.SetAutonomyRepoLevel)))
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
	initialEvalInterval, initialMode := evalIntervalAndModeForConfig(b.cfg, b.ghClient, b.logger)
	b.logger.Info("entering governor loop", "interval_seconds", int(initialEvalInterval.Seconds()), "eval_mode", initialMode)
	lastEvalInterval := initialEvalInterval
	ticker := deps.newTicker(initialEvalInterval)
	defer ticker.Stop()
	if b.evalLoop != nil {
		b.evalLoop.setInterval(initialEvalInterval, initialMode)
	}

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
	if b.ghClient != nil {
		if report, err := b.ghClient.MigrateHiveHoldLabel(b.ctx, github.HiveHoldMigrationOptions{
			HiveID: b.cfg.HiveID,
			Logger: b.logger,
		}); err != nil {
			// No fail-closed fallback is possible here: the legacy hold label
			// hive/<id> was the provenance label on every claimed item (now
			// retired, #12539), and github.HasHoldLabelWith never treats
			// provenance labels as holds (#9371 - treating it as one parked
			// every claimed item). Items still carrying only the legacy
			// label are therefore NOT held.
			// Say so loudly, in the log and on the dashboard, instead of
			// pretending to fail closed (#9614).
			reportPath := ""
			if report != nil {
				reportPath = report.ReportPath
			}
			b.logger.Error("hive hold label migration failed; items still carrying only the legacy "+
				github.HiveProvenanceLabel(b.cfg.HiveID)+" label are NOT treated as held (#9371) - re-apply "+
				github.CanonicalHiveHoldLabel(b.cfg.HiveID)+" by hand or restart to retry the migration",
				"error", err, "report", reportPath)
			if b.dashSrv != nil {
				b.dashSrv.AddSystemAlert(holdMigrationFailedAlertID, "warning", holdMigrationFailedAlertMessage(b.cfg.HiveID, reportPath))
			}
		}
		// Provenance-only migration. Retired reviewer labels map to agent/reviewer;
		// redundant lane-origin labels are removed. Nothing reads origin/* as a gate,
		// so a failure here only leaves old names on open items; the marker is not
		// written and the next boot retries.
		if _, err := b.ghClient.MigrateOriginLabels(b.ctx, github.OriginLabelMigrationOptions{Logger: b.logger}); err != nil {
			b.logger.Warn("origin provenance label migration incomplete; open items may still carry retired names such as "+github.LegacyOriginReviewLabel+" or "+github.LegacyFromReviewLabel, "error", err)
		}
		// Issues park at stage/045-needs-decision; stage/085 is PR-only
		// (owner decision). Open issues still wearing 085 are re-staged
		// once; the issue reconciler also does it per cycle for the issues
		// it enumerates, so a failure here only delays the rest.
		if _, err := b.ghClient.MigrateIssueNeedsDecisionStage(b.ctx, github.IssueNeedsDecisionMigrationOptions{Logger: b.logger}); err != nil {
			b.logger.Warn("issue needs-decision stage migration incomplete; open issues may still carry "+github.PRNeedsDecisionStageLabel+" instead of "+github.NeedsDecisionStageLabel, "error", err)
		}
		// The hive/<hive-id> provenance label is retired (#12539): the origin
		// marker in every agent-written body carries the same fact. Runs
		// after the hold migration so that migration still sees the label
		// on an upgrade straight from a pre-#9371 hive. Provenance only, so
		// a failure leaves stale labels and retries next boot.
		if _, err := b.ghClient.RetireHiveIDLabels(b.ctx, github.HiveIDLabelRetireOptions{HiveID: b.cfg.HiveID, Logger: b.logger}); err != nil {
			b.logger.Warn("hive/<hive-id> label retire sweep incomplete; open items may still carry "+github.HiveProvenanceLabel(b.cfg.HiveID), "error", err)
		}
		// The claim "who" modifier for a human principal is the generic
		// hive/claimed-by-human; the per-login spelling is retired (owner
		// decision 2026-10-19). Open items still wearing a login are moved
		// once and the per-login definitions deleted. Display only: every
		// reader matches the prefix, so a failure leaves stale names and
		// retries next boot.
		claimedByOpts := github.ClaimedByHumanMigrationOptions{Logger: b.logger}
		for name := range b.cfg.Agents {
			claimedByOpts.AgentNames = append(claimedByOpts.AgentNames, name)
		}
		if b.issueClaims != nil {
			claimedByOpts.LookupClaim = b.issueClaims.Lookup
		}
		if _, err := b.ghClient.MigrateClaimedByHumanLabels(b.ctx, claimedByOpts); err != nil {
			b.logger.Warn("claimed-by-human label migration incomplete; open issues may still carry a per-login hive/claimed-by-<login> label", "error", err)
		}
	}
	// Startup gate (#13375): with the state cache on, the first decision
	// pass waits for the first successful reconcile of every repo so it
	// never plans against an empty or partial cache.
	if err := waitForgeStateReady(b.ctx, b.forgeState.Load(), b.logger, forgeStateReadyLogInterval); err != nil {
		return
	}
	b.evalCycles++
	deps.runEval(b, nil, evalSourceEnumerate)
	deps.runRotation(b)
	if b.wd != nil {
		b.wd.Tick(b.ctx)
	}
	deps.runSweeps(b)
	deps.persist(b)

	// tick is one governor pass. The cadence ticker runs it with
	// evalSourceEnumerate (the reconcile pass that re-lists every repo). The
	// review webhook wake (hivecommons/hive#11091), the GitHub webhook wake
	// and the continuous re-kick wake run it with evalSourceCached: the same
	// gates, planning from the stored snapshot — already patched by the
	// delivery's targeted re-read — so a wake costs no listing.
	tick := func(source evalCycleSource) {
		b.cfgReloadMu.Lock()
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
		b.evalCycles++
		deps.runEval(b, restarted, source)
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
			refs, coveredRepos := activeIssueRefsFromLastActionable(b.lastActionable.Load())
			b.replanLane.SetActiveIssueRefs(refs, coveredRepos)
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
		effectiveEvalInterval, mode := evalIntervalAndModeForConfig(b.cfg, b.ghClient, b.logger)
		if effectiveEvalInterval != lastEvalInterval {
			b.logger.Info("eval interval changed, resetting ticker",
				"from", int(lastEvalInterval.Seconds()), "to", int(effectiveEvalInterval.Seconds()), "eval_mode", mode)
			ticker.Reset(effectiveEvalInterval)
			lastEvalInterval = effectiveEvalInterval
		}
		if b.evalLoop != nil {
			b.evalLoop.setInterval(effectiveEvalInterval, mode)
		}
		b.cfgReloadMu.Unlock()
	}
	b.startReviewEvents()

	// Webhook health re-check: in webhook mode the base tick (900s) is longer
	// than the health window (2 × eval_interval_s), so without this a lapse
	// would only be noticed at the next stretched tick. The timer fires just
	// after the window would close; if health lapsed, the ticker is reset to
	// the configured interval right away. Nil newHealthTimer disables it.
	var (
		healthTimer loopTicker
		healthCh    <-chan time.Time
	)
	healthNow := deps.now
	if healthNow == nil {
		healthNow = time.Now
	}
	armWebhookHealthRecheck := func() {
		if deps.newHealthTimer == nil || b.cfg == nil {
			return
		}
		base := b.cfg.Governor.EvalIntervalS
		if base <= 0 {
			base = 300
		}
		delay := webhookHealthRecheckDelay(github.WebhookHealthSnapshot(), 2*time.Duration(base)*time.Second, healthNow())
		if delay <= 0 {
			if healthTimer != nil {
				healthTimer.Stop()
			}
			return
		}
		if healthTimer == nil {
			healthTimer = deps.newHealthTimer(delay)
			healthCh = healthTimer.Chan()
		} else {
			healthTimer.Stop()
			healthTimer.Reset(delay)
		}
	}
	armWebhookHealthRecheck()
	defer func() {
		if healthTimer != nil {
			healthTimer.Stop()
		}
	}()

	// Continuous re-kick wake: a continuous agent's NextKick (set when its
	// kick turn ends, + cooldown) was only consumed by the cadence eval tick,
	// so with eval_interval_webhook_s=900 the reviewer sat idle at its prompt
	// for up to 15 minutes after every "kick turn ended". The wake fires an
	// early tick when the earliest NextKick elapses, bounded by
	// continuousWakeMinGap, and otherwise re-checks every continuousWakePoll.
	// When a wake-driven cycle leaves the due re-kick untouched (the agent was
	// gated outside the governor: dashboard pause, first-scan deferral,
	// provider budget, upgrade drain) wakeHold backs the wake off that re-kick
	// so a gated agent does not turn the cadence into one eval per 30s.
	wakeNow := deps.now
	if wakeNow == nil {
		wakeNow = time.Now
	}
	var (
		wakeTimer    loopTicker
		wakeCh       <-chan time.Time
		lastWake     time.Time
		wakeArmedFor time.Time
		wakeHold     continuousWakeHold
	)
	armContinuousWake := func() {
		if deps.newWakeTimer == nil || b.gov == nil {
			return
		}
		next, who := b.gov.NextContinuousKickAt()
		delay, _ := continuousWakeDecision(next, wakeNow(), lastWake, wakeHold.untilFor(next, who))
		if delay <= 0 {
			delay = continuousWakeMinDelay
		}
		if wakeTimer == nil {
			wakeTimer = deps.newWakeTimer(delay)
			wakeCh = wakeTimer.Chan()
		} else {
			wakeTimer.Stop()
			wakeTimer.Reset(delay)
		}
		wakeArmedFor = next
	}
	armContinuousWake()
	defer func() {
		if wakeTimer != nil {
			wakeTimer.Stop()
		}
	}()

	serveContinuousWake := func() {
		if deps.newWakeTimer == nil || b.gov == nil || b.ctx.Err() != nil {
			return
		}
		now := wakeNow()
		next, who := b.gov.NextContinuousKickAt()
		if _, due := continuousWakeDecision(next, now, lastWake, wakeHold.untilFor(next, who)); due {
			idleFor := time.Duration(0)
			if st, ok := b.gov.GetState().Continuous[who]; ok && !st.LastKickEnd.IsZero() {
				idleFor = now.Sub(st.LastKickEnd).Round(time.Second)
			}
			b.logger.Info("continuous re-kick wake: running an early eval cycle",
				"agent", who, "next_kick", next.UTC().Format(time.RFC3339), "idle_for", idleFor.String(),
				"was_armed_for", wakeArmedFor.UTC().Format(time.RFC3339))
			lastWake = now
			// Keep this cycle lean: planning and delivery without sweeps.
			b.cfgReloadMu.Lock()
			b.evalCycles++
			deps.runEval(b, nil, evalSourceCached)
			deps.persist(b)
			b.cfgReloadMu.Unlock()
			after, afterWho := b.gov.NextContinuousKickAt()
			if backoff, stalled := wakeHold.observe(next, after, who, afterWho, wakeNow()); stalled {
				b.logger.Info("continuous re-kick wake: eval cycle left the due re-kick unchanged (agent gated outside the governor); backing off",
					"agent", who, "next_kick", next.UTC().Format(time.RFC3339), "backoff", backoff.String())
			}
		}
	}

	for {
		// Serve due kicks between cycles even when other channels stay ready.
		serveContinuousWake()
		select {
		case <-b.ctx.Done():
			b.logger.Info("shutting down, persisting state")
			deps.persist(b)
			return
		case <-ticker.Chan():
			tick(evalSourceEnumerate)
			armContinuousWake()
			armWebhookHealthRecheck()
		case <-b.reviewWake:
			b.logger.Info("review webhook wake: running an early eval cycle from the cached snapshot")
			tick(evalSourceCached)
			armContinuousWake()
			armWebhookHealthRecheck()
		case batch := <-b.verdictWakeChan():
			b.logger.Info("review verdict wake: running an early eval cycle from the cached snapshot",
				"pr", batch.Reason, "last_pr", batch.LastRe, "verdicts", batch.Count,
				"coalesced", batch.Coalesced.Round(time.Millisecond).String())
			tick(evalSourceCached)
			armContinuousWake()
			armWebhookHealthRecheck()
		case batch := <-b.webhookWakeChan():
			now := healthNow()
			// Apply the delivery's targeted re-reads first so this cycle
			// sees the patched item rather than the row as it was. With the
			// re-read disabled nothing patches the snapshot, so the wake
			// has to enumerate as it did before.
			source := evalSourceCached
			if b.cfg != nil && !b.cfg.StageTrackerWebhookRefresh() {
				source = evalSourceEnumerate
			} else if b.dashSrv != nil {
				b.dashSrv.FlushWebhookTrackerRefresh()
			}
			b.logger.Info("github webhook wake: running an early eval cycle",
				"reason", batch.Reason, "last_reason", batch.LastRe, "deliveries", batch.Count,
				"coalesced", batch.Coalesced.Round(time.Millisecond).String(), "source", source.String())
			if b.evalLoop != nil {
				b.evalLoop.noteWake(batch, now)
			}
			tick(source)
			armContinuousWake()
			armWebhookHealthRecheck()
		case <-healthCh:
			b.cfgReloadMu.Lock()
			effectiveEvalInterval, mode := evalIntervalAndModeForConfig(b.cfg, b.ghClient, b.logger)
			if effectiveEvalInterval != lastEvalInterval {
				b.logger.Info("eval interval changed, resetting ticker",
					"from", int(lastEvalInterval.Seconds()), "to", int(effectiveEvalInterval.Seconds()), "eval_mode", mode)
				ticker.Reset(effectiveEvalInterval)
				lastEvalInterval = effectiveEvalInterval
			}
			if b.evalLoop != nil {
				b.evalLoop.setInterval(effectiveEvalInterval, mode)
			}
			b.cfgReloadMu.Unlock()
			armWebhookHealthRecheck()
		case <-wakeCh:
			// The loop-top due check services this timer.
			armContinuousWake()
		case <-agentTickCh:
			b.cfgReloadMu.Lock()
			govState := b.gov.GetState()
			agentStatuses := b.agentMgr.AllStatuses()
			payload := dashboard.BuildAgentOnlyStatus(govState, agentStatuses, b.cfg)
			b.cfgReloadMu.Unlock()
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

// governorKickFairness remembers, per agent, how many cycles in a row the
// governor kick was deferred behind a delivered review-fix kick
// (hivecommons/hive#15166). Package-level for the same reason as
// providerBudgetProbe: runEvalCycle has no state of its own.
var governorKickFairness = newKickLaneFairness()

var evalAPIBudgetCycle atomic.Uint64

// applyBudgetAlerts is a thin wrapper around spokealerts.ApplyBudget, kept so
// call sites in this file do not need the package-qualified name (same
// pattern as applyModeUnscheduledAlert below).
func applyBudgetAlerts(gov *governor.Governor, trans governor.BudgetTransitions, dashSrv *dashboard.Server, notifier *notify.Notifier) {
	spokealerts.ApplyBudget(gov, trans, dashSrv, notifier)
}

// applyNoCadenceAlert is a thin wrapper around spokealerts.ApplyNoCadence
// (see that package for the banner's design: #5577).
func applyNoCadenceAlert(gov *governor.Governor, dashSrv *dashboard.Server) {
	spokealerts.ApplyNoCadence(gov, dashSrv)
}

func applyModeUnscheduledAlert(gov *governor.Governor, dashSrv *dashboard.Server) {
	spokealerts.ApplyModeUnscheduled(gov, dashSrv)
}

func activeIssueRefsFromLastActionable(actionable *github.ActionableResult) (map[string]struct{}, map[string]struct{}) {
	if actionable == nil {
		return nil, nil
	}
	refs := make(map[string]struct{})
	coveredRepos := make(map[string]struct{})
	for repo := range actionable.TotalByRepo {
		coveredRepos[repo] = struct{}{}
	}
	for _, issue := range actionable.Issues.Items {
		if ref := planning.IssueRef(issue); ref != "" {
			refs[ref] = struct{}{}
		}
	}
	for _, held := range actionable.Hold.Items {
		if held.Type != "issue" || held.Repo == "" || held.Number <= 0 {
			continue
		}
		ref := planning.IssueRef(github.Issue{Repo: held.Repo, Number: held.Number, Title: held.Title})
		if ref != "" {
			refs[ref] = struct{}{}
		}
	}
	return refs, coveredRepos
}

// agentKicker adapts crux.Runtime to planning.Kicker for the Phase 3
// stall-replan lane. Kick delegates to SendKick, which takes the manager lock
// ITSELF and is only ever called here from the governor tick (never from the
// agent-launch path), so it cannot re-enter a held manager lock. This is the
// same out-of-band kick path the eval loop already uses for governor kicks.
type agentKicker struct{ mgr crux.Runtime }

func (k agentKicker) Kick(agent, message string) error {
	if k.mgr == nil {
		return fmt.Errorf("agent manager unavailable: cannot kick %s", agent)
	}
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
	agentMgr crux.Runtime,
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
		// The GitHub created_at is the open→triage / open→merge SLA origin;
		// retained here so the SLA never re-fetches the issue.
		if !issue.CreatedAt.IsZero() {
			event.Attrs = map[string]string{timeline.AttrIssueCreatedAt: issue.CreatedAt.UTC().Format(time.RFC3339)}
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
	return apphealth.ClassifyFailure(ctx, appAuth, expectedOwner, appKeyPaths(appAuthID(appAuth)), logger)
}

func classifyGitHubAppWriteForbidden(ctx context.Context, appAuth *github.AppAuth, expectedOwner, repo string) (string, github.AppAuthState) {
	return apphealth.ClassifyWriteForbidden(ctx, appAuth, expectedOwner, repo, appKeyPaths(appAuthID(appAuth)))
}

func appAuthID(appAuth *github.AppAuth) int64 {
	if appAuth == nil {
		return 0
	}
	return appAuth.AppID()
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

func recordAdvisoryPostForbidden(repo string, now time.Time, backoff time.Duration) {
	advisoryPostGate.RecordWriteForbidden(repo, now, backoff)
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
// On a non-default work source (e.g. Linear), or a default GitHub source with
// additive work enabled (run stages / Wavefront), the GitHub issue call is not
// the only backlog authority. Aborting here meant a Linear-sourced hive whose
// GitHub App could not list issues (403 "Resource not accessible by
// integration", an Issues permission a Linear hive should not need) never
// enumerated its Linear backlog at all and sat at queue 0; the same rule keeps
// local additive sources reachable during a transient GitHub issue outage. Such
// a hive continues with whatever partial result GitHub returned (nil becomes
// an empty result; PRs are kept when obtainable) and lets the overlay populate
// issues.
func actionableAfterGitHubEnumerate(cfg *config.Config, actionable *github.ActionableResult, err error, logger *slog.Logger) (*github.ActionableResult, bool) {
	if err == nil {
		return actionable, true
	}
	if !workSourceOverlayEnabled(cfg.Governor.WorkSource) {
		logger.Error("failed to enumerate actionable items", "error", err)
		return nil, false
	}
	logger.Warn("GitHub enumeration failed; continuing so the configured work source can still populate issues",
		"work_source", cfg.Governor.WorkSource.Type, "run_stages", cfg.Governor.WorkSource.RunStages,
		"wavefront", cfg.Governor.WorkSource.Wavefront.Enabled, "error", err)
	if actionable == nil {
		actionable = &github.ActionableResult{GeneratedAt: time.Now()}
	}
	// The GitHub share of this result is missing, not zero: every configured
	// repo is marked stale so the dashboard shows the gap as stale rather
	// than as an empty hive (a cold cache has no last-good to fall back on).
	markConfiguredReposStale(actionable, cfg.Project.Repos)
	return actionable, true
}

// markConfiguredReposStale flags every configured repo the result does not
// already cover as stale, with a zero count entry so the repo still appears.
func markConfiguredReposStale(actionable *github.ActionableResult, repos []string) {
	if actionable == nil {
		return
	}
	if actionable.TotalByRepo == nil {
		actionable.TotalByRepo = map[string]github.RepoCounts{}
	}
	for _, repo := range repos {
		if _, ok := actionable.TotalByRepo[repo]; !ok {
			actionable.TotalByRepo[repo] = github.RepoCounts{}
		}
	}
	actionable.MarkAllStale()
}

func tokensForBackend(summary *tokens.AggregateSummary, backend string) int64 {
	if summary == nil {
		return 0
	}
	var total int64
	for _, sess := range summary.Sessions {
		if strings.EqualFold(sess.Backend, backend) {
			total += sess.TotalTokens
		}
	}
	return total
}

func runEvalCycle(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	gov *governor.Governor,
	sched *scheduler.Scheduler,
	agentMgr crux.Runtime,
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
	source evalCycleSource,
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

	budgetMode, _ := ghClient.APIBudgetMode()
	budgetDecision := decideEvalBudgetWork(budgetMode, cfg.Governor.OptionalSweepEveryNCycles, evalAPIBudgetCycle.Add(1))

	// A cached cycle (webhook or re-kick wake) plans from the stored
	// snapshot: no listing, and none of the enrichment or unstick sweeps
	// that walk every item — those belong to the reconcile pass. The first
	// cycle has no snapshot yet and enumerates regardless of source.
	cachedSnapshot := cachedSnapshotFor(source, lastActionable)
	if cachedSnapshot != nil {
		budgetDecision.SkipOptional = true
		budgetDecision.SkipUnstick = true
		budgetDecision.SkippedSteps = append(budgetDecision.SkippedSteps, cachedSnapshotSkippedStep)
	} else if source == evalSourceCached {
		logger.Info("eval cycle: no cached snapshot yet; enumerating")
	}
	ghClient.RecordAPIBudgetSkippedSteps(budgetDecision.SkippedSteps)

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
	advisoryEnsureErr := error(nil)
	if !budgetDecision.SkipOptional {
		advisoryEnsureErr = ensurePinnedAdvisoryIssue(
			ctx, advisoryIssues, primaryRepoAtCycleStart, advisoryEnsureDepsForCycle, logger)
	}

	var actionable *github.ActionableResult
	if cachedSnapshot != nil {
		actionable = cachedSnapshot
		logger.Info("eval cycle planning from cached snapshot",
			"snapshot_age", time.Since(actionable.GeneratedAt).Round(time.Second).String(),
			"issues", actionable.Issues.Count, "prs", actionable.PRs.Count, "hold", actionable.Hold.Total,
			"stale", actionable.Stale())
	} else {
		enumCtx, enumSpan := tracing.StartSpan(ctx, "governor.enumerate_actionable")
		fresh, err := ghClient.EnumerateActionable(enumCtx)
		enumSpan.End()
		var ok bool
		actionable, ok = actionableAfterGitHubEnumerate(cfg, fresh, err, logger)
		if !ok {
			if kept := keepLastGoodAfterFailedEnumerate(lastActionable, logger); kept != nil && dashSrv != nil {
				dashSrv.MarkStatusStale(kept.StaleSince)
			}
			return
		}

		// If a non-default or additive work source is configured, overlay its
		// issues onto the actionable result. PRs always come from GitHub. A
		// cached cycle keeps the overlay the reconcile pass already applied.
		if workSourceOverlayEnabled(cfg.Governor.WorkSource) {
			actionable.Issues = workSourceIssuesForConfiguredCycle(ctx, cfg, ghClient, actionable.Issues, logger)
		}
	}
	if swarmRepo := dashSrv.ActiveSwarmRepo(); swarmRepo != "" {
		github.BoostActionableRepoPriority(actionable.Issues.Items, swarmRepo)
		logger.Info("swarm priority boost applied", "repo", swarmRepo)
	}

	ghClient.EnrichCIStatus(ctx, actionable.PRs.Items)
	// Held PRs need CI status too, or a red held PR can never be repaired:
	// the hold label kept it out of PRs.Items, so nothing ever learned it was
	// red and no agent was ever told to fix it (hivecommons/hive#7438). This
	// enriches the held list ONLY for the repair path — held PRs still never
	// reach the merge sweep, escalation or the queue counts.
	if budgetMode != github.APIBudgetCritical {
		ghClient.EnrichCIStatus(ctx, actionable.PRs.Held)
	}
	// Stale drafts sit in the same dashboard PR column; they get the
	// review/link signals only — no mergeability or check-run fetches, a
	// draft is not a merge candidate (hivecommons/hive#8968).
	if !budgetDecision.SkipOptional {
		// Sidelined PRs (drafts, exempt labels) sit in the same column and
		// need their closing references to pair with the issue they fix
		// (hivecommons/hive#13421); one shared collector keeps the budget at
		// one query per repo for both.
		ghClient.EnrichReviewSignalsAndWorkPRs(ctx, actionable.PRs.StaleDrafts, actionable.WorkDetailsByRepo)
		// A closing reference whose issue is already closed (a superseded
		// PR's `Fixes #N`) has no open row to borrow a title from; fill it
		// once per issue, from a long-lived memo, so the activity monitor
		// never shows a bare "#N".
		ghClient.EnrichLinkedIssueTitles(ctx, actionable.WorkDetailsByRepo, actionable.PRs.Items, actionable.PRs.Held, actionable.PRs.StaleDrafts)
	}

	// Publish the human-facing "what should I merge next?" digest. This reads
	// the PR set enumerated and CI-enriched immediately above, so it must stay
	// after those two calls: Mergeable and the failing-check names it sorts on
	// are populated by EnrichCIStatus, not by EnumerateActionable.
	if !budgetDecision.SkipOptional {
		postRecommendationsForCycle(ctx, cfg, ghClient, actionable, logger)
	}

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
	if !budgetDecision.SkipOptional {
		applyDuplicatePRGuard(ctx, cfg, ghClient, actionable, logger)
	}
	// Every cycle, cached ones included: heal issue stage records the
	// claim ledger says an open PR covers but the store still holds below
	// 040 (a claim release written over the cover between reconcile passes).
	reconcileLedgerCoverRecords(ctx, cfg, ghClient, actionable, logger)

	// PR review queue (#9590): stamp each PR's rank, priority and reasons
	// onto the snapshot so last-actionable.json carries them next to
	// review_class. Additive only: Items and Held keep their order, and
	// nothing that gates work reads the stamp.
	reviewQueue := stampReviewQueue(cfg, actionable, logger)

	// Only the reconcile pass publishes the snapshot: a cached cycle works
	// on a copy, and storing it back would overwrite a webhook patch that
	// landed while the cycle ran.
	if cachedSnapshot == nil {
		publishEnumeratedSnapshot(lastActionable, actionable, logger)
		if data, err := json.Marshal(actionable); err == nil {
			atomicWrite(lastActionablePath, data)
		}
	}
	publishStageSharedCIIncidents(actionable)

	// Record enumerated issues into the lifecycle timeline so the dashboard's
	// lifecycle view has real data. Cheap and fully guarded: a nil dashboard or
	// nil store is a no-op (timeline.Store.Record is nil-safe), the loop is
	// bounded by maxTimelineEnumeratePerCycle, and the store dedupes by
	// (ref, kind) so this per-cycle sweep refreshes journeys instead of
	// flooding them (#5656).
	recordEnumeratedIssues(ctx, dashSrv, actionable)

	escalatedPRs := map[string]bool{}
	if !budgetDecision.SkipUnstick {
		escalatedPRs = runEscalationSweep(ctx, cfg, governorForge(cfg, ghClient, logger), actionable, notifier, dashSrv, logger)
	}

	intentVerdicts := writeIntentVerdicts(ctx, cfg, ghClient, actionable, beadStores, logger)
	refreshReviewVerdicts(cfg, logger)
	recordReviewOutcomes(ctx, cfg, ghClient, actionable, logger)
	requiredCheckSet, _ := cfg.AutoMerge.RequiredCheckSetForRepo("")

	// Hold guard (#5589): snapshot hold-gated PR heads, and when a hold lifts
	// on a branch that moved, block the merge lanes and force a fresh review.
	// Runs before writeMergeEligible so drifted PRs are excluded from the very
	// tick their hold lifted — no window for the sweep to race the re-hold.
	selfAuthorizationHoldEnabled := func(repo string) bool {
		level := cfg.ACMMLevelOrZero()
		if agentMgr != nil {
			level = agentMgr.GetACMMLevel()
		}
		return cfg.SelfAuthorizationHoldEnabledForRepoAtLevel(repo, level)
	}
	holdDriftPRs := enforceHoldGuard(ctx, cfg, ghClient, governorForge(cfg, ghClient, logger), actionable, logger, selfAuthorizationHoldEnabled)

	// The per-PR verdicts come back so the dashboard's PR pills can be
	// painted from the sweep's own classification rather than a looser
	// reading of GitHub's mergeable flag (hivecommons/hive#7478).
	mergeVerdicts := writeMergeEligible(actionable, actionable.Hold, cfg.Project.Org, escalatedPRs, cfg.Intent.Enforce, intentVerdicts, cfg.Review.RequireApproval, requiredCheckSet, holdDriftPRs, cfg, logger)

	// Review-bot threads (hivecommons/hive#7360): list every unresolved
	// external-review-bot thread on an open hive-authored PR into
	// review-threads.json, attributed to the agent that opened the PR the
	// same way ci-failing.json is, so the scheduler can route each PR back
	// to its author for a fix + in-thread replies before any new work.
	if !budgetDecision.SkipOptional {
		writeReviewThreads(ctx, ghClient, actionable, cfg.Project.Org, escalatedPRs, logger)
	}

	// PR follow-up session resume (hivecommons/hive#9583, default off): feed
	// CI failures, changes-requested reviews and new review-bot threads on a
	// PR this hive opened back into the CLI session that authored it, while
	// that session is still live. Anything it cannot resume stays on the
	// fix-before-new path above, unchanged.
	routePRFollowUps(ctx, cfg, ghClient, actionable, escalatedPRs, agentMgr, logger)

	// Stuck-PR reaper (backstop): DELIVER a targeted FIX-BEFORE-NEW kick for any
	// hive-authored PR that is red on a required check AND stale (its red head
	// SHA unchanged past RedPRStaleAfter). writeMergeEligible already surfaces
	// every red PR into ci-failing.json (the CI_FAILING kick block), but relying
	// on the owner's next cadence kick to carry it meant a red PR could be
	// escalated as "6 automated fix re-dispatches" having been mentioned to
	// nobody (hivecommons/hive#9472); the reaper now kicks the owner — or the
	// fallback fixer when the owner is paused or unreachable — and charges the
	// re-engagement budget only for kicks that were accepted. Composes with the
	// merge watcher (#2): both go through the same per-red-SHA cap, so the same
	// PR is never double-dispatched within a red-SHA's budget.
	reapStuckRedPRs(cfg, actionable, escalatedPRs, agentKicker{mgr: agentMgr},
		agentAvailability(cfg, agentMgr), logger)

	// Release sentinel (hivecommons/hive#9585), opt-in and default OFF: when
	// the current v<version> tag's CI fails, dispatch a bounded repair round
	// through the same kick path as the reaper above, or escalate to a human
	// when the failure is a setting no commit can fix. Never pushes a branch;
	// moves the tag to a merged fix PR only with retag_enabled (also opt-in).
	runReleaseSentinel(ctx, cfg, ghClient, agentKicker{mgr: agentMgr},
		agentAvailability(cfg, agentMgr), notifier, logger)

	// Upstream watch (hivecommons/hive#9967), opt-in and default OFF: at most
	// once per upstream_watch.interval, file a labelled fork issue for each
	// upstream merged PR or release that still applies to the fork. Opens
	// issues only, never PRs; the watermark and dedupe index live on the PVC.
	runUpstreamWatch(ctx, cfg, ghClient, logger)

	// Merge-hotspot advisor: at most hourly, score the files concurrent
	// PRs keep colliding on (from the merge train's own records) and file
	// one modularize issue per hotspot for the ci-maintainer lane through
	// the writeback gate. Skipped with the other optional sweeps when the
	// GitHub budget is tight.
	if !budgetDecision.SkipOptional {
		runHotspotAdvisor(ctx, cfg, ghClient, actionable, logger)
	}

	if !budgetDecision.SkipUnstick {
		shaResult, shaErr := ghClient.EnforceSHAHold(ctx, github.SHAHoldConfig{
			PrimaryRepo:     cfg.Project.PrimaryRepo,
			AIAuthor:        cfg.Project.AIAuthor,
			InternalAuthors: []string{"hivecommons-hive[bot]", "kubestellar-hive[bot]", "hivecommons-hive-ghe[bot]", "kubestellar-hive-ghe[bot]", "github-actions[bot]", "dependabot[bot]", "copilot-swe-agent[bot]"},
			// Trusted reporters get the soft needs-sha label, not the hard
			// hold (hivecommons/hive#12002).
			TrustedAssociations: cfg.Project.IssueFilter.ReporterTrust.EffectiveTrustedAssociations(),
			TrustedLabel:        cfg.Issues.EffectiveSHAHoldTrustedLabel(),
		})
		if shaErr != nil {
			logger.Warn("SHA hold enforcement failed", "error", shaErr)
		} else {
			logger.Info("SHA hold enforcement complete",
				"held", shaResult.Held,
				"unheld", shaResult.Unheld,
				"softened", shaResult.Softened,
				"skipped", shaResult.Skipped,
			)
		}
		runHardSuppressReconcile(ctx, cfg, ghClient, logger)
	}

	// Refresh budget spend from lifetime token totals before Evaluate so
	// the kick gate sees current-window numbers. The banner itself is
	// level-triggered every cycle (#9612 follow-up): a collector summary may
	// not be available yet (right after a restart, before the first session
	// scan completes), and the one-shot Crossed edge that used to gate the
	// raise is lost on restart too, so gate ONLY the spend refresh on the
	// summary and re-derive the banner from whatever budget state the
	// governor currently holds (persisted spend if the summary is not in
	// yet, refreshed spend once it is) on every cycle.
	trans := gov.BudgetLevel()
	if tokenCollector != nil {
		if summary := tokenCollector.Summary(); summary != nil {
			var bobCoins, bobUSD float64
			if coinCfg, ok := cfg.Governor.Budget.CoinConfig(tokens.BackendBob); ok {
				bobCoins = coinCfg.TokensToCoins(tokensForBackend(summary, tokens.BackendBob))
				bobUSD = coinCfg.CoinsToUSD(bobCoins)
			}
			trans = gov.UpdateBudgetFromTotalsCoinsAndUSD(summary.TotalTokens, summary.ByAgent, summary.ByModel, bobCoins, bobUSD)
		}
	}
	applyBudgetAlerts(gov, trans, dashSrv, notifier)

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
	// Refused restarts are not silent: an interval-throttled agent sits at
	// an empty prompt until its slot, so raise a per-agent alert (with an OOM
	// hint when the cgroup killed the CLI). Every cycle, reconcile clears it
	// once it no longer describes a problem (#9612): kicked, paused/removed,
	// no longer expected to run, working again, or past its max age.
	var resumeHeld []spokealerts.ResumeKickHeldAgent
	agentsDue, resumeHeld = mergeResumeKicks(agentsDue, restartedAgents, gov.AllowResumeKickReason, logger)
	resumeKickHeldAlerts.Apply(dashSrv, resumeHeld, agentMgr.CrashOOMSuspected)
	resumeKickHeldAlerts.Reconcile(dashSrv, resumeKickHeldFacts(agentMgr, gov.ResumeKickVerdict))

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
	agentsDue, agentsSkipped := partitionKickableAgents(agentsDue, cfg.Agents, config.OnDemandAgentsFromPacks(), agentMgr.IsPaused, func(name string) string {
		if st, err := agentMgr.GetStatusFast(name); err == nil && st != nil {
			return st.PausedTrigger
		}
		return ""
	})
	agentsDue, agentsSkipped = reviewerAutoscaler.withholdDrainingReviewers(agentsDue, agentsSkipped)

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
	// The filter compacts agentsDue in place, so snapshot it first to report
	// the targets it withholds alongside the other skips below.
	dueBeforeDeferral := append([]string(nil), agentsDue...)
	agentsDue = sched.FilterFirstScanDeferredAgents(agentsDue)
	agentsSkipped = append(agentsSkipped, firstScanDeferredSkips(dueBeforeDeferral, agentsDue)...)
	reviewPlan := planReviewDispatchWith(ctx, cfg, actionable, agentMgr, beadStores, reviewPlannerGitHubFor(ghClient), logger)
	// Size the reviewer lane to what the planner just left waiting
	// (hivecommons/hive#14685). A replica added here takes kicks from the
	// next cycle on; a retire only lands once the reviewer is idle.
	reviewerAutoscaler.Run(ctx, cfg, reviewPlan, agentMgr, gov, logger, time.Now())
	auditWithheldReviewFixes(reviewPlan, dashSrv, logger)
	if ghClient != nil {
		postReviewAcknowledgements(ctx, ghClient, reviewPlan, logger)
	}
	applyHumanDecisionLabels(ctx, cfg, ghClient, actionable, reviewPlan, logger)
	if ghClient != nil {
		applyReviewPriorityLabels(ctx, cfg, ghClient, reviewQueue, logger)
		applyReviewVerdictLabels(ctx, cfg, ghClient, actionable, reviewPlan, logger)
		sweepClosedPRVerdictLabels(ctx, cfg, ghClient, time.Now(), logger)
	}
	messages, messageSkips := sched.BuildKickMessagesWithSkips(kickActionable, agentsDue)
	agentsSkipped = append(agentsSkipped, messageSkips...)
	// Message construction can withhold a due kick (notably an idle reviewer
	// lane). Report only assembled targets as due. Skipped targets dispatch
	// nothing, so (as before) no LastKick is recorded for them and new lane
	// work stays eligible on the next evaluation.
	assembledTargets := make([]string, 0, len(messages))
	for _, msg := range messages {
		assembledTargets = append(assembledTargets, config.CadenceTargetKey(msg.Agent, msg.Repo))
	}
	logger.Info("governor eval complete",
		"mode", govState.Mode,
		"issues", govState.QueueIssues,
		"prs", govState.QueuePRs,
		"agents_due", assembledTargets,
		"agents_skipped", agentsSkipped,
	)

	reviewKickByMessage := map[string]review.DispatchKick{}
	for _, k := range append(reviewPlan.ReviewKicks, reviewPlan.FixKicks...) {
		if !gov.AgentEligibleForCELKick(k.Agent) || agentMgr.IsPaused(k.Agent) {
			logger.Info("review swarm kick suppressed by governor gate", "agent", k.Agent, "pr", k.PRRef)
			continue
		}
		messages = append(messages, scheduler.KickMessage{Agent: k.Agent, Message: k.Message, IssueRefs: []string{k.PRRef}})
		reviewKickByMessage[k.Agent+"\x00"+k.Message] = k
	}
	// A fix kick (a maintainer's changes_requested / `/hive decision`, or
	// the swarm's) goes ahead of the governor kick for the same agent, or
	// the governor kick owns the agent and the fix kick times out every
	// cycle (hivecommons/hive#13100).
	for _, agent := range pendingKickAgentsFromMessages(messages) {
		if governorKickFairness.GovernorFirst(agent) {
			logger.Warn("governor kick displaced by fix kicks on consecutive cycles; governor kick goes first this cycle",
				"agent", agent, "displaced_cycles", governorKickFairness.Displaced(agent), "limit", governorKickDisplacementLimit)
		}
	}
	messages = orderReviewFixKicksFirst(messages, reviewKickByMessage, governorKickFairness.GovernorFirst)
	// Freshness bound before a kick (#13375): a ref older than
	// github.DecisionFreshnessBound in a cache-served or last-good snapshot
	// is confirmed with one targeted GET; a stale repo's refs are withheld.
	messages = freshenKickMessages(ctx, ghClient.SnapshotFreshnessGuard(actionable, time.Now), actionable, messages, logger)
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
				gov.RecordKickFailure(msg.Agent, fmt.Errorf("provider inference backoff (%s): %s", class, line), time.Now())
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
	// Self-upgrade drain (#11876): a hub-instructed upgrade is waiting for
	// in-flight kicks to end, so starting new turns would only feed the
	// restart. Withheld kicks stay due and go out from the new process.
	if target, draining := selfUpgradeDrain.Active(selfUpgradeDrainNow()); draining && len(messages) > 0 {
		logger.Info("self-upgrade drain: withholding new agent kicks",
			"target", target, "withheld", pendingKickAgentsFromMessages(messages))
		messages = nil
	}
	govState.PendingKicks = pendingKickAgentsFromMessages(messages)
	gov.SetPendingKicks(govState.PendingKicks)
	if notifyProviderBudget {
		notifier.Send("Provider spending limit reached", providerBudgetCause, notify.PriorityHigh)
	}

	// buildStatusSnapshot assembles the full dashboard snapshot from this
	// cycle's evaluation. It is called twice: once BEFORE the kick dispatch
	// below and once at the end of the cycle. Each kick can wait up to the
	// input-prompt timeout for a busy pane; before dispatch ran one agent at
	// a time, a dozen due agents took 10-15 minutes on hosted hives — during
	// which /api/status (and everything derived from it: stage tracker, agent
	// states, governor.nextKickAt) kept showing the previous cycle's data, and
	// a rolling upgrade landing mid-dispatch meant the evaluation never
	// reached the dashboard at all. Dispatch is now concurrent across agents
	// (dispatchAgentKicks), so the wait is one agent's, but the early publish
	// stays: the evaluation should be visible before any kick is typed.
	buildStatusSnapshot := func() *dashboard.StatusPayload {
		payload := dashboard.BuildFrontendStatus(
			govState,
			actionable,
			agentMgr.AllStatuses(),
			cfg,
			tokenCollector,
			gov,
			beadStores,
			ghClient,
			ctx,
			metricsCollector,
		)
		// Green on a PR pill means "the sweep would merge this now" — the
		// verdict writeMergeEligible reached for this same actionable set,
		// not a re-derivation from mergeable_state (#7478).
		dashboard.AttachMergeVerdicts(payload, mergeVerdicts)
		// A PR pill also carries the hive's OWN review on that PR, so the
		// queue view shows where the hive has already spoken. Read from the
		// durable ledger, so this costs no GitHub call per PR; a missing or
		// unreadable ledger simply means no review pills this cycle.
		attachReviewLinksForDashboard(payload, logger)
		return payload
	}
	publishStatusBeforeKicks(dashSrv, len(messages), buildStatusSnapshot, logger)

	// Kick dispatch lives behind a seam (#7232): the skip rules and the
	// single-probe rule are the decisions worth testing, and they were
	// previously unreachable without a tmux session and a live governor.
	var deliveredReviewKicks []review.DispatchKick
	var deliveredKicks []scheduler.KickMessage
	if len(messages) > 0 {
		// governor.kick wraps the whole dispatch pass; each delivered kick
		// still gets its own agent.kick child via startKickSpan below.
		kickCtx, kickPassSpan := tracing.StartSpan(ctx, "governor.kick",
			attribute.Int("hive.kicks.assembled", len(messages)))
		ctx := kickCtx
		dispatchAgentKicks(messages, releaseProviderBudgetProbe, kickDispatchDeps{
			backoffRemaining: agentMgr.ProviderErrorBackoffRemaining,
			sendKick:         agentMgr.SendKick,
			startStoppedAgent: func(agentName string) error {
				if !agentMgr.GovernorMayStart(agentName) {
					return nil
				}
				agentMgr.MarkStartupLaunchQueued([]string{agentName})
				if err := agentMgr.Start(ctx, agentName); err != nil {
					if stoppedAgentStartErrorIsBenign(err) {
						logger.Info("stopped agent start raced with existing launch before governor kick", "agent", agentName, "error", err)
						return nil
					}
					return fmt.Errorf("start stopped agent: %w", err)
				}
				logger.Info("started stopped agent before governor kick", "agent", agentName)
				return nil
			},
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
					persistReviewDispatchStateInProgress(reviewPlan, deliveredReviewKicks, logger)
				}
			},
			onFailed: func(msg scheduler.KickMessage, err error) {
				gov.RecordKickFailure(msg.Agent, err, time.Now())
				if k, ok := reviewKickByMessage[reviewKickKey(msg)]; ok {
					logUndeliveredReviewKick(logger, dashSrv, k, err)
				}
			},
			onDelivered: func(msg scheduler.KickMessage, deliveredAt time.Time) {
				deliveredKicks = append(deliveredKicks, msg)
				gov.RecordKickForRepoAt(msg.Agent, msg.Repo, deliveredAt)
				dashSrv.AuditLog("governor", "kick", "trigger=governor-eval", msg.Agent)

				// Record issue-scoped kicks into the lifecycle timeline. Cheap,
				// guarded, and nil-safe (Record no-ops on a nil dashboard/store).
				recordKick(ctx, dashSrv, msg.Agent, msg.IssueRefs...)
				// A review-swarm kick (review or fix) names a pull request,
				// not an issue: its IssueRefs is the PR ref. Listing it in
				// the issue-claims ledger made the author's next comment on
				// its own PR an issue "start signal", so the PR was claimed
				// as an issue and staged stage/030-pr-in-progress — an
				// issue-only stage — until the PR sweep healed it.
				reviewKick, isReview := reviewKickByMessage[msg.Agent+"\x00"+msg.Message]
				if !isReview {
					// #8380/#10527: list these issues for the kicked agent so
					// contributors and other agents back off while the kick is
					// live; the claim itself is recorded on the agent's first
					// start signal (recordAgentStart), not here.
					recordAgentKickListings(dashSrv, cfg.Project.Org, msg.Agent, msg.IssueRefs, logger)
				}

				// #12128: record which issue/PR/head SHA this kick was launched
				// for, so /api/launch-cost can place the turn's spend. Review
				// kicks carry the PR head; governor kicks carry the issue refs.
				if tokenCollector != nil {
					tokenCollector.RecordLaunch(launchRecordForKick(msg, reviewKick, isReview, time.Now()))
				}

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
		kickPassSpan.SetAttributes(attribute.Int("hive.kicks.review_delivered", len(deliveredReviewKicks)))
		kickPassSpan.End()
	}
	governorKickFairness.ObserveCycle(messages, deliveredKicks, reviewKickByMessage)
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
	statusPayload := buildStatusSnapshot()
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
				MaxFindings:  digestOpts.MaxFindings,
				ShowAll:      digestOpts.ShowAll,
				Org:          org,
				ShowEmpty:    digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0,
				PrimaryRepo:  repoName,
				Advice:       hiveAdvice,
				DashboardURL: advisoryDashboardOrigin(cfg),
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
				onRepoMoved: func(ctx context.Context, moved *github.RepoMovedError) {
					// #11791: the write was refused because the repository
					// now lives under another owner. The installation is
					// healthy, so the generic write-forbidden remedy would
					// misdirect the operator; name the move instead.
					dashSrv.SetGitHubAppRequired(true)
					dashSrv.SetGitHubAppPermIssue(moved.Error())
					dashSrv.SetGitHubAppState(github.AppStateRepoMoved.String())
					logger.Warn("GitHub App write failed — repository transferred to another owner",
						"configured", moved.Configured, "current", moved.Current)
				},
				recordRepoPaused: func(repo string) {
					recordAdvisoryPostSuccess(repo, time.Now())
				},
				recordWriteForbidden: func(repo string) {
					backoff := advCfg.PostRetryBackoff()
					recordAdvisoryPostForbidden(repo, time.Now(), backoff)
					if backoff > 0 {
						logger.Warn("advisory digest write forbidden — pausing post attempts",
							"repo", repo, "retry_after", backoff.String(),
							"knob", "governor.advisory.post_retry_backoff_s")
					}
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

	publishFleetReportsWith(ctx, logger, dashSrv, statusPayload.FleetReport, newSelfReportPublishDeps(cfg, ghClient, dashSrv))

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
		if err := nousState.RecordSnapshot(govState, actionable, agentsDue, agentMgr.AllStatuses(), tokenSummary); err != nil {
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
		out[p.Provider()] = watchdog.RotationAuthProbe{Prober: usageProber{p}}
	}
	return out
}

// usageProber adapts a rotation prober to the watchdog's UsageProber slice
// (provider key + sanitised probe error); the watchdog does not import
// pkg/rotation.
type usageProber struct{ rotation.Prober }

func (u usageProber) ProbeError(ctx context.Context) string {
	return u.Prober.Probe(ctx).ProbeError()
}

// turnLossToSnapshot converts the manager's in-memory turn-loss accumulation
// into its persisted form, or nil when nothing has been recorded.
//
// Nil rather than a zero struct on purpose: `turn_loss` is omitempty, so an
// agent that has never been interrupted adds nothing to /data/hive-state.json.
// The overwhelming majority of agents are in that state, and a measurement that
// bloated every hive's state file with empty records would be its own argument
// for removing it.
func turnLossToSnapshot(loss crux.TurnLoss) *snapshot.AgentTurnLoss {
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

func restartEventsToSnapshot(events []crux.RestartEvent) []snapshot.AgentRestartEvent {
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

func restartEventsFromSnapshot(events []snapshot.AgentRestartEvent) []crux.RestartEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]crux.RestartEvent, 0, len(events))
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, ev := range events {
		if ev.At.IsZero() || ev.At.Before(cutoff) {
			continue
		}
		out = append(out, crux.RestartEvent{At: ev.At, Reason: ev.Reason})
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

func persistState(agentMgr crux.Runtime, gov *governor.Governor, cfg *config.Config, path string, logger *slog.Logger, dashSrv *dashboard.Server, wd *watchdog.Reconciler) {
	persistStateWithPaths(agentMgr, gov, cfg, path, logger, dashSrv, wd, persistPathsForRuntime())
}

func persistStateWithPaths(agentMgr crux.Runtime, gov *governor.Governor, cfg *config.Config, path string, logger *slog.Logger, dashSrv *dashboard.Server, wd *watchdog.Reconciler, paths persistPaths) {
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
		CoinBudgetLimit:      budget.CoinLimit,
		CoinBudgetSpend:      budget.CoinSpend,
		CoinBudgetLabel:      budget.CoinLabel,
		USDBudgetLimit:       budget.USDLimit,
		USDBudgetSpend:       budget.USDSpend,
		BudgetWindowBaseline: budget.WindowBaseline,
		CoinWindowBaseline:   budget.CoinWindowBaseline,
		USDWindowBaseline:    budget.USDWindowBaseline,
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

		costRollup := dashSrv.CostDailyRollups()
		if len(costRollup) > 0 {
			rollupData, err := json.Marshal(costRollup)
			if err == nil {
				atomicWrite(paths.CostDailyRollup, rollupData)
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
// observations (repo fully-qualified, Red == a required check failed). Shared
// by recordRedStaleness and the reaper so both classify PRs identically.
//
// owners maps "org/repo#number" to the agent that opened the PR (see
// auditPRAgents); pass nil when ownership is not needed. Shared/base CI
// breakage is classified across every PR in the pass before human and
// non-Hive observations are removed. A green PR is counter-evidence, and an
// independent human-authored PR must confirm the shared failure.
func hivePRObservations(cfg *config.Config, actionable *github.ActionableResult, owners map[string]string) []escalation.Observation {
	if actionable == nil {
		return nil
	}
	all := allEscalationObservations(cfg, actionable, owners)
	agentObs := all[:0]
	for i, pr := range actionable.PRs.Items {
		if isHiveAgentAuthor(cfg, pr.Author) {
			agentObs = append(agentObs, all[i])
		}
	}
	return agentObs
}

func allEscalationObservations(cfg *config.Config, actionable *github.ActionableResult, owners map[string]string) []escalation.Observation {
	all := make([]escalation.Observation, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	for _, pr := range actionable.PRs.Items {
		all = append(all, escalationObservation(cfg, pr, owners))
	}
	for _, pr := range actionable.PRs.Held {
		all = append(all, escalationObservation(cfg, pr, owners))
	}
	escalation.MarkSharedFailures(all)
	return all
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
func escalationObservation(cfg *config.Config, pr github.PullRequest, owners map[string]string) escalation.Observation {
	repo := pr.Repo
	if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
		repo = cfg.Project.Org + "/" + repo
	}
	red := pr.HasFailingRequiredCheck()
	return escalation.Observation{
		Repo:               repo,
		Number:             pr.Number,
		HeadSHA:            pr.HeadSHA,
		HeadTree:           pr.HeadTree,
		Red:                red,
		Pending:            !red && pr.CIStatus != "success",
		CIRunning:          red && pr.CIChecksRunning,
		Labeled:            escalation.HasNeedsHumanLabel(pr.Labels),
		Excerpt:            pr.CIFailureExcerpt,
		Labels:             pr.Labels,
		FailingChecks:      pr.FailingChecks,
		BaseRedChecks:      pr.BaseRedChecks,
		SharedCIIncident:   pr.SharedCIIncident,
		Agent:              owners[escalation.Key(repo, pr.Number)],
		IndependentControl: isIndependentCIControl(cfg, pr),
	}
}

// redPRFixOwner resolves who should be asked to repair a red PR: the agent the
// audit trail attributes it to, or — when that agent is unattributed — the
// fleet's primary PR creator, exactly the default formatRedPRFixData applies
// to an unattributed ci-failing.json row. Keeping the two in step is what lets
// a hook predicate (`t.agent == "scanner"`) and the FIX-BEFORE-NEW block name
// the same owner (hivecommons/hive#9474).
func redPRFixOwner(agent string) string {
	if agent = strings.TrimSpace(agent); agent != "" {
		return agent
	}
	return review.DefaultFixerAgent
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

// isIndependentCIControl reports whether a PR supplies human-authored evidence
// independent of Hive. Bot-authored and Hive-attributed contributor-relay PRs
// cannot prove that a failure also affects unrelated human work.
func isIndependentCIControl(cfg *config.Config, pr github.PullRequest) bool {
	return !isHiveAgentAuthor(cfg, pr.Author) && !pr.AuthorNonHuman && !pr.HiveAttributed
}

// recordRedStaleness updates the shared staleness clock (first-seen-red per red
// head SHA) for every hive-authored PR in this pass. It must run before the
// claim guard and the reaper so their StaleRed() reads reflect the current tick.
// A disabled escalation subsystem skips it (the whole fix-loop machinery is off).
//
// It also fires the escalation_red hook for each red PR, WITH the owning agent
// on the payload: the catalog and hooks.md have always documented `agent` on
// this transition, but nothing ever set it, so a predicate like
// `t.agent == "scanner"` could never match (hivecommons/hive#9474).
func recordRedStaleness(cfg *config.Config, actionable *github.ActionableResult) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	owners := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	obs := hivePRObservations(cfg, actionable, owners)
	getEscalationStore().ObserveRed(obs)
	for _, ob := range obs {
		if !ob.Red {
			continue
		}
		hookDispatcher().Fire(context.Background(), hooks.Payload{
			Transition: hooks.TransitionEscalationRed,
			Repo:       ob.Repo,
			Agent:      redPRFixOwner(ob.Agent),
			Reason:     "required CI check red",
			Attrs: map[string]string{
				hooks.AttrPR: strconv.Itoa(ob.Number),
				"head_sha":   ob.HeadSHA,
				"excerpt":    ob.Excerpt,
			},
		})
	}
}

// fixKicker is the out-of-band kick path the re-engagement lanes deliver
// through. crux.Runtime satisfies it via agentKicker, which is the same
// adapter the governor tick's replan lane uses.
type fixKicker interface {
	Kick(agent, message string) error
}

// agentAvailability reports, per agent, whether a kick sent right now could
// reach it: the agent must not be paused by the manager, and at least one
// governor mode must schedule it with a non-paused cadence. An agent paused in
// every mode (the v5 L6 pack pauses `strategist` everywhere) can never answer a
// FIX-BEFORE-NEW kick, so charging its red PRs' re-engagement budget only
// escalates them to a human having asked nobody (hivecommons/hive#9472).
func agentAvailability(cfg *config.Config, mgr crux.Runtime) func(string) bool {
	return func(name string) bool {
		if name == "" {
			return false
		}
		if mgr != nil && mgr.IsPaused(name) {
			return false
		}
		return cfg == nil || cadenceReachable(cfg, name)
	}
}

// cadenceReachable reports whether ANY governor mode gives the agent a cadence
// that is not paused/off. An agent with no cadence entry at all is treated as
// reachable: "nothing schedules it" is a fleet-configuration signal of its own
// (NoCadenceAgents), and refusing to route repairs to it would silently widen
// this gate into an agent-disablement policy.
func cadenceReachable(cfg *config.Config, name string) bool {
	configured := false
	for mode := range cfg.Governor.Modes {
		cad := cfg.CadenceValueForMode(name, mode)
		if cad == "" {
			continue
		}
		configured = true
		if !cad.IsPaused() {
			return true
		}
	}
	return !configured
}

// ownerReEngageCooldown is the minimum spacing between two re-engagements of a
// PR owned by this agent: its slowest configured cadence, so a budget of six
// attempts cannot be spent inside a single cadence window where no kick the
// owner could answer would ever fit. The escalation store floors it at
// ReEngageCooldown and caps it at MaxReEngageCooldown.
func ownerReEngageCooldown(cfg *config.Config, name string) time.Duration {
	if cfg == nil || name == "" {
		return 0
	}
	var slowest time.Duration
	for mode := range cfg.Governor.Modes {
		cad := cfg.CadenceValueForMode(name, mode)
		if cad == "" || cad.IsPaused() {
			continue
		}
		d, err := time.ParseDuration(strings.TrimSpace(cad.Interval()))
		if err == nil && d > slowest {
			slowest = d
		}
	}
	return slowest
}

// resolveRedPRFixer picks the agent a stuck red PR's fix kick should go to: its
// owner, else the configured review fixer agent, else the fleet's default
// fixer — the same chain the review-fix kick uses (src/docs/review-swarm.md).
// It returns "" when no candidate is reachable, which tells the caller to leave
// the PR alone rather than spend budget nobody can answer.
func resolveRedPRFixer(cfg *config.Config, owner string, available func(string) bool) string {
	candidates := []string{redPRFixOwner(owner), strings.TrimSpace(cfg.Review.FixerAgent), review.DefaultFixerAgent}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if available == nil || available(c) {
			return c
		}
	}
	return ""
}

// redPRFixKick renders the targeted FIX-BEFORE-NEW kick a re-engagement
// delivers. It names ONE PR — the stuck one — with its evidence, so the kick is
// answerable on its own rather than relying on the owner's next cadence kick
// happening to carry the ci-failing.json block.
func redPRFixKick(o escalation.Observation, rerouted bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 FIX-BEFORE-NEW — %s#%d is red and has not moved\n\n", o.Repo, o.Number)
	if rerouted {
		fmt.Fprintf(&b, "Its owning agent (%s) is not reachable right now, so this repair is routed to you.\n\n", redPRFixOwner(o.Agent))
	}
	b.WriteString("Repair it BEFORE claiming new issues or opening any new PR:\n")
	if slices.Contains(o.Labels, stagelabels.NoDCOLabel) {
		fmt.Fprintf(&b, "  FIRST (required): the PR is %s — it inherited unsigned commits. Re-sign them: gh pr checkout %d --repo %s → git rebase --signoff <base> (original authors preserved) → git push --force-with-lease\n", stagelabels.NoDCOLabel, o.Number, o.Repo)
		b.WriteString("  THEN fix any failing CI the same way.\n")
	}
	fmt.Fprintf(&b, "  gh pr checkout %d --repo %s → fix → git commit -s → git push\n", o.Number, o.Repo)
	b.WriteString("Push to the SAME branch. Do NOT open a replacement PR.\n")
	b.WriteString("After the push, STOP: do not watch, poll, or sleep on CI (no gh run watch/view loops).\n")
	b.WriteString("The automerge sweep merges the PR once it is green; if the failure is infrastructure\n")
	b.WriteString("(runner lost, no space left, checks still queued) leave one comment and end the turn.\n")
	if o.HeadSHA != "" {
		fmt.Fprintf(&b, "\nhead: %s\n", o.HeadSHA)
	}
	if len(o.FailingChecks) > 0 {
		fmt.Fprintf(&b, "failing: %s\n", strings.Join(o.FailingChecks, ", "))
	}
	if excerpt := strings.TrimSpace(o.Excerpt); excerpt != "" {
		if runes := []rune(excerpt); len(runes) > redPRFixKickExcerptRunes {
			excerpt = string(runes[:redPRFixKickExcerptRunes]) + "…"
		}
		fmt.Fprintf(&b, "\nevidence:\n%s\n", excerpt)
	}
	return b.String()
}

// redPRFixKickExcerptRunes bounds the CI evidence carried in a targeted fix
// kick, matching the FIX-BEFORE-NEW block's own budget.
const redPRFixKickExcerptRunes = 400

// mergeReEngageHook builds the Fix #2 re-engagement callback for the merge
// watcher. It normalizes the repo, resolves the PR's owner, DELIVERS a targeted
// fix kick to it (or to the fallback fixer when the owner is unreachable), and
// only then records the attempt under the shared escalation store's per-red-SHA
// cap. Passing an empty head SHA tells the store to reuse the red head SHA it
// last observed for this PR (the eval cycle's ObserveRed keeps it current), so
// the merge watcher does not need to re-fetch the head. Returns false when the
// cap is exhausted or no kick could be delivered, so the watcher can log that
// the escalation path now owns the PR. A disabled escalation subsystem yields a
// nil hook (watcher falls back to quarantine-only).
//
// Delivery is the point: before hivecommons/hive#9472 this only incremented a
// counter, so a PR could be escalated as "6 automated fix re-dispatches" having
// never been mentioned to any agent.
func mergeReEngageHook(cfg *config.Config, kicker fixKicker, available func(string) bool, logger *slog.Logger) github.MergeReEngageFunc {
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
		full := fullRepo(repo)
		owners := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
		o := escalation.Observation{
			Repo:   full,
			Number: number,
			Agent:  owners[escalation.Key(full, number)],
		}
		// Empty head SHA: reuse the store's tracked current red SHA for this PR
		// (do not reset the cap counter). The eval cycle records it via
		// ObserveRed; if the store has never seen this PR red, the store still
		// allows the first MaxReEngagements nudges.
		return deliverReEngagement(cfg, getEscalationStore(), o, "", kicker, available, logger)
	}
}

// deliverReEngagement is the one place a fix re-dispatch is both SENT and
// COUNTED. It resolves a reachable fixer, checks the budget without spending
// it, delivers the kick, and only records the attempt once the kick was
// accepted: an undeliverable kick (agent not running, provider backoff,
// restart) must not consume a PR's budget and strand it in needs-human
// (hivecommons/hive#9472).
func deliverReEngagement(
	cfg *config.Config,
	store *escalation.Store,
	o escalation.Observation,
	headSHA string,
	kicker fixKicker,
	available func(string) bool,
	logger *slog.Logger,
) bool {
	fixer := resolveRedPRFixer(cfg, o.Agent, available)
	if fixer == "" {
		// Nobody can be asked right now. Charging budget here is what escalated
		// red strategist PRs on a pack that pauses strategist in every mode.
		if logger != nil {
			logger.Info("re-engagement skipped: no reachable fixer agent",
				"repo", o.Repo, "pr", o.Number, "owner", redPRFixOwner(o.Agent))
		}
		return false
	}
	if !store.ReEngageEligible(o.Repo, o.Number, headSHA, ownerReEngageCooldown(cfg, fixer)) {
		return false
	}
	if kicker == nil {
		return false
	}
	if err := kicker.Kick(fixer, redPRFixKick(o, fixer != redPRFixOwner(o.Agent))); err != nil {
		if logger != nil {
			level := slog.LevelWarn
			// Pool contention is expected backpressure: retry on a later sweep.
			// Keep it below WARN so slogfeed does not file a runtime defect
			// for a healthy shared pool refusing another session (#15350).
			if isSharedPoolBusyErr(err) {
				level = slog.LevelInfo
			}
			logger.Log(context.Background(), level, "re-engagement kick undeliverable; budget not charged",
				"repo", o.Repo, "pr", o.Number, "agent", fixer, "error", err)
		}
		return false
	}
	store.RecordReEngagement(o.Repo, o.Number)
	return true
}

// reapStuckRedPRs is Fix #4: the governor's backstop sweep. For each
// hive-authored PR that is red on a required check AND stale (StaleRed) AND not
// already escalated to a human, it DELIVERS a targeted FIX-BEFORE-NEW kick to
// the PR's owner (or the fallback fixer when the owner is unreachable) and
// records the re-engagement only if that kick was accepted — deduped and capped
// via the escalation store. Entirely generic: it keys only off check state +
// staleness, never a linter.
func reapStuckRedPRs(
	cfg *config.Config,
	actionable *github.ActionableResult,
	escalatedPRs map[string]bool,
	kicker fixKicker,
	available func(string) bool,
	logger *slog.Logger,
) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	store := getEscalationStore()
	owners := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	for _, o := range hivePRObservations(cfg, actionable, owners) {
		if !o.Red {
			continue
		}
		if o.Shared {
			// Every failing check is red across the fleet: the PR is not
			// broken, a shared incident is. Nudging its owner to "fix" it
			// wastes budget on work that cannot succeed (hivecommons/hive#9473).
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
		if !deliverReEngagement(cfg, store, o, o.HeadSHA, kicker, available, logger) {
			// Cap reached, cooldown not elapsed, or nothing could be delivered.
			// The distinct-SHA escalation path owns it from here.
			continue
		}
		logger.Info("reaper: re-dispatched fix for stuck red PR",
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

	type prMeta struct{ checks []string }
	meta := map[string]prMeta{}
	owners := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	all := allEscalationObservations(cfg, actionable, owners)
	itemCount := len(actionable.PRs.Items)

	obs := all[:0]
	for i, o := range all {
		var pr *github.PullRequest
		if i < itemCount {
			pr = &actionable.PRs.Items[i]
		} else {
			pr = &actionable.PRs.Held[i-itemCount]
		}
		if pr.ReporterTrustReason != "" {
			escalationStore.SetReporterTrustReason(o.Repo, o.Number, pr.ReporterTrustReason)
		}
		if (i >= itemCount || !isHiveAgentAuthor(cfg, pr.Author)) && pr.ReporterTrustReason == "" {
			continue
		}
		obs = append(obs, o)
		meta[escalation.Key(o.Repo, o.Number)] = prMeta{checks: o.FailingChecks}
	}
	results := escalationStore.Sweep(obs, cfg.Escalation.EffectiveThreshold())

	for _, o := range obs {
		key := escalation.Key(o.Repo, o.Number)
		r, ok := results[key]
		if !ok {
			continue
		}
		if r.Unparked {
			// The sweep returned this PR to the automated lane by itself: its
			// head went green, or the shared incident it escalated over
			// cleared. Take the label off (retried on later passes until a
			// pass sees it gone) and say why, once.
			unparkEscalatedPR(ctx, writer, o, r, logger)
			continue
		}
		if r.Escalated {
			escalated[key] = true
		}
		if r.BaseRedHold {
			// Every failing check is red on the base branch too: say so
			// once per head, count nothing, kick no one. The hold clears
			// by itself when the base recovers and the head is re-evaluated.
			body := escalation.BaseRedHoldCommentBody(o.BaseRedChecks, o.SharedCIIncident)
			if err := writer.CreateIssueComment(ctx, o.Repo, o.Number, body); err != nil {
				logger.Warn("base-branch-red hold comment failed; will retry next pass", "repo", o.Repo, "pr", o.Number, "error", err)
			} else {
				escalationStore.MarkBaseRedHeld(o.Repo, o.Number, o.HeadSHA)
				logger.Info("fix loop holding: base branch red", "repo", o.Repo, "pr", o.Number, "head_sha", o.HeadSHA, "checks", strings.Join(o.BaseRedChecks, ","))
			}
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
		ev := escalation.Evidence{
			Attempts:      r.Attempts,
			FailingChecks: meta[key].checks,
			SharedChecks:  r.SharedChecks,
			Excerpt:       excerpt,
			Exhausted:     r.Exhausted,
			ReEngagements: r.ReEngagements,
		}
		body := escalation.CommentBody(ev)
		afterReviewerPass := false
		if sha, at, ok := escalationStore.ReviewerPass(o.Repo, o.Number); ok {
			body = escalation.HandoffCommentBody(ev, escalation.ReviewerHandoff{SHA: sha, At: at})
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

// labelRemover is the optional forge capability the auto-un-park path needs.
// Every real adapter (GitHub, GitLab, Gitea) implements RemoveLabel on
// forge.Forge; the narrow IssueWriter the sweep takes does not declare it, so
// a writer that cannot remove labels simply leaves the PR labeled and logs.
type labelRemover interface {
	RemoveLabel(ctx context.Context, repo string, number int, label string) error
}

// unparkEscalatedPR performs the forge side of an automatic un-park: the
// needs-human label comes off, and — on the pass that decided it — a one-line
// comment says why. Before hivecommons/hive#9473 nothing did this: a PR
// escalated over a base-branch or runner outage stayed parked after the
// incident cleared, and a labeled PR that went green stayed parked forever,
// because the labeled branch returned before the green branch.
func unparkEscalatedPR(ctx context.Context, writer forge.IssueWriter, o escalation.Observation, r escalation.Result, logger *slog.Logger) {
	remover, ok := writer.(labelRemover)
	if !ok {
		logger.Warn("cannot un-park escalated PR: forge writer cannot remove labels",
			"repo", o.Repo, "pr", o.Number)
		return
	}
	if err := remover.RemoveLabel(ctx, o.Repo, o.Number, escalation.NeedsHumanLabel); err != nil {
		// Retried on the next pass: the ledger keeps asking until a pass
		// observes the label gone.
		logger.Warn("un-park label removal failed; will retry next pass",
			"repo", o.Repo, "pr", o.Number, "error", err)
		return
	}
	if r.UnparkReason == "" {
		return // label-removal retry: the comment was posted on the deciding pass
	}
	if err := writer.CreateIssueComment(ctx, o.Repo, o.Number, escalation.UnparkCommentBody(r.UnparkReason)); err != nil {
		logger.Warn("un-park comment failed", "repo", o.Repo, "pr", o.Number, "error", err)
	}
	logger.Info("escalation un-parked", "repo", o.Repo, "pr", o.Number, "reason", r.UnparkReason)
}

// autoMergeSweepInterval is the minimum spacing between label-queued
// auto-merge sweeps. The sweep piggybacks on the governor eval tick, which can
// fire much more often than once a minute; this floor keeps the sweep from
// hammering the GitHub API on short eval intervals.
const autoMergeSweepInterval = time.Minute

// autoMergeSweepTickBudget is how long the queued automerge sweep may wait
// for its first pacing slot before the sweep is shed to the next cycle.
const autoMergeSweepTickBudget = 5 * time.Second

// taskListSweepInterval is the minimum spacing between task-list sweeps. The
// sweep enumerates every open issue in every repo — much heavier than the
// auto-merge sweep's label-scoped query — and "done" moves at PR-merge cadence,
// so a 15-minute floor keeps the API cost modest while still closing completed
// epics on the same day the last box gets ticked.
const taskListSweepInterval = 15 * time.Minute

// supersessionSweepInterval is the minimum spacing between sweeps that retire
// open PRs whose claimed issue was already closed by a different merged PR.
// It walks open PRs and may inspect issue timelines and file lists, so it uses
// the same conservative cadence as the task-list sweep.
const supersessionSweepInterval = 15 * time.Minute

// duplicateFixSweepInterval is the minimum spacing between sweeps that
// resolve two open PRs fixing the same issue (hivecommons/hive#14799). It
// reuses the open-PR listing the pre-dispatch guard caches and reads check
// runs per pair, so it runs on the supersession cadence.
const duplicateFixSweepInterval = 15 * time.Minute

// sharedCIRerunInterval is the minimum spacing between shared-CI rerun
// sweeps. Each pass lists recently closed issues per repo and only walks open
// PRs when a [shared-ci] incident closed inside the lookback window, so it
// shares the supersession sweep's cadence.
const sharedCIRerunInterval = 15 * time.Minute

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
func runRotationCheck(ctx context.Context, cfg *config.Config, rotMgr *rotation.Manager, gov *governor.Governor, agentMgr crux.Runtime, logger *slog.Logger) {
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
		if proc.State == crux.StateRunning {
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
	// Housekeeping against the pacer: while a caution window has the pacing
	// queue deeper than this sweep could wait, skip the sweep (it is due
	// again next cycle) rather than queue behind the enumeration.
	if github.ShedLowPriorityTick("automerge:queued-sweep", autoMergeSweepTickBudget) {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	ctx = github.WithPacingClass(ctx, github.PacingLow)
	// governor.automerge covers the queued, trusted-author and contributor-
	// lane sweeps plus the stage-label pass that follows them. The sweep runs
	// on its own cadence in the run loop, so this is a sibling of
	// governor.eval_cycle rather than a child.
	ctx, sweepSpan := tracing.StartSpan(ctx, "governor.automerge")
	defer sweepSpan.End()
	opts := automerge.Options{
		Logger:                  logger,
		MergerAuthorizer:        trustedMergerFunc(cfg),
		HumanChangeRequests:     review.DefaultHumanChangeRequests(),
		TrustedAuthorizer:       trustedAuthorFunc(cfg),
		TrustedAuthorPolicy:     trustedAuthorPolicyFunc(cfg),
		ContributorLanePolicy:   contributorLanePolicyFunc(cfg),
		ContributorLaneVerdict:  contributorLaneVerdictFunc(cfg, logger),
		ContributorRegistryTier: contributorRegistryTierFunc(),
		StageLabelPolicy:        stageLabelPolicyFunc(cfg),
		StageEngine:             stageEngineOptions(cfg, logger),
	}
	syncStageEngineMode(cfg, ghClient, logger)
	// hivecommons/hive#12436: the contribute hub is the stage-072 nudge sink
	// (relay fix-pr tasks). Guarded so a nil hub never becomes a typed-nil
	// interface.
	if hub := dashSrv.ContributeHub(); hub != nil {
		opts.ContributorNudge = hub
	}
	if cfg != nil {
		opts.SentinelLabel = func() string { return cfg.Sentinel.LabelOrDefault() }
		if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
			opts.RequiredChecks = set
		}
		opts.RequiredChecksForRepo = cfg.AutoMerge.RequiredCheckSetForRepo
		opts.MinHeadAge = cfg.AutoMerge.EffectiveMinHeadAge()
		opts.RepoAutoMergeEnabled = func(repo string) bool { return cfg.RepoAutoMergeEnabled(repo) }
		opts.RepoOnGitHub = func(repo string) bool { return cfg.RepoOnGitHub(repo) }
		opts.HumanMergePaths = func(repo string) []string { return humanMergePathsFor(cfg, repo) }
		opts.HumanMergeRule = func() prstage.HumanMergeRule { return humanMergeRuleFor(cfg) }
		opts.RequireCurrentBase = func(repo string) bool { return cfg.AutoMerge.RequireCurrentBaseFor(repo) }
		opts.RequireCurrentBaseStrict = func(repo string) bool { return cfg.AutoMerge.RequireCurrentBaseStrictFor(repo) }
		opts.MergeForwardTrustedHuman = func(repo string) bool { return cfg.AutoMerge.MergeForwardTrustedHumanFor(repo) }
		opts.MergeTrain = func() automerge.MergeTrainPolicy {
			return automerge.MergeTrainPolicy{Enabled: cfg.AutoMerge.MergeTrain.IsEnabled(), MaxRebaseAttempts: cfg.AutoMerge.MergeTrain.EffectiveMaxRebaseAttempts()}
		}
		opts.TrustedBotAuthors = func() map[string]bool { return cfg.AutoMerge.TrustedBotAuthorSet() }
	}
	audit := func(event automerge.AutoMergeSweepEvent) {
		if dashSrv == nil {
			return
		}
		detail := fmt.Sprintf("repo=%s, pr=%d, author=%s, queued_by=%s, label=%s, tier=%s, head_sha=%s, merge_sha=%s",
			event.Repo, event.Number, event.Author, event.QueuedBy, event.Label, event.Tier, event.HeadSHA, event.MergeSHA)
		dashSrv.AuditLogRecord("system", "automerge-sweep-merged", detail, "", event.Repo, event.Number)
	}
	result, err := automerge.SweepQueuedAutoMerges(ctx, ghClient, opts, automerge.AutoMergeSweepOptions{
		MaxMerges: automerge.DefaultAutoMergeSweepMaxMerges,
		Audit:     audit,
	})
	if err != nil {
		logger.Warn("automerge sweep failed", "error", err)
		return
	}
	if len(result.Merged) > 0 || result.Seen > 0 {
		logger.Info("automerge sweep complete", "seen", result.Seen, "merged", len(result.Merged), "skipped", result.Skipped)
	}
	sweepSpan.SetAttributes(
		attribute.Int("hive.automerge.seen", result.Seen),
		attribute.Int("hive.automerge.merged", len(result.Merged)),
		attribute.Int("hive.automerge.skipped", result.Skipped))
	remaining := automerge.DefaultAutoMergeSweepMaxMerges - len(result.Merged)
	if remaining > 0 && opts.TrustedAuthorPolicy().Enabled {
		trustedResult, trustedErr := automerge.SweepTrustedAuthorAutoMerges(ctx, ghClient, opts, automerge.AutoMergeSweepOptions{
			MaxMerges:              remaining,
			Audit:                  audit,
			SelfAuthoredLaneActive: cfg != nil && cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel),
		})
		if trustedErr != nil {
			logger.Warn("trusted-author automerge sweep failed", "error", trustedErr)
		} else {
			result.Seen += trustedResult.Seen
			result.Skipped += trustedResult.Skipped
			result.Merged = append(result.Merged, trustedResult.Merged...)
			if len(trustedResult.Merged) > 0 || trustedResult.Seen > 0 {
				logger.Info("trusted-author automerge sweep complete", "seen", trustedResult.Seen, "merged", len(trustedResult.Merged), "skipped", trustedResult.Skipped)
			}
		}
	}
	remaining = automerge.DefaultAutoMergeSweepMaxMerges - len(result.Merged)
	if remaining > 0 && opts.ContributorLanePolicy().Enabled {
		laneResult, laneErr := automerge.SweepContributorLaneAutoMerges(ctx, ghClient, opts, automerge.AutoMergeSweepOptions{
			MaxMerges: remaining,
			Audit:     audit,
		})
		if laneErr != nil {
			logger.Warn("contributor-lane automerge sweep failed", "error", laneErr)
		} else {
			result.Seen += laneResult.Seen
			result.Skipped += laneResult.Skipped
			result.Merged = append(result.Merged, laneResult.Merged...)
			if len(laneResult.Merged) > 0 || laneResult.Seen > 0 {
				logger.Info("contributor-lane automerge sweep complete", "seen", laneResult.Seen, "merged", len(laneResult.Merged), "skipped", laneResult.Skipped)
			}
		}
	}
	// Stage labels (#12429): the lanes above each recorded their per-PR
	// reason into the process-shared tracker; one pass writes the labels.
	if stageResult, stageErr := automerge.ApplyStageLabels(ctx, ghClient, opts); stageErr != nil {
		logger.Warn("stage labels reconcile failed", "error", stageErr, "items", stageResult.Items, "writes", stageResult.Writes)
	} else if stageResult.Writes+stageResult.Comments > 0 {
		logger.Info("stage labels reconciled", "items", stageResult.Items, "writes", stageResult.Writes, "comments", stageResult.Comments, "healed", stageResult.Healed)
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

// legacyFleetReportSweepInterval is how often the hive that watches the
// fleet-report repo looks for standalone "Fleet report:" issues to close.
const legacyFleetReportSweepInterval = github.LegacyFleetReportSweepInterval

// hiveWatchesRepo reports whether repo is one of project.repos.
func hiveWatchesRepo(cfg *config.Config, repo string) bool {
	if cfg == nil {
		return false
	}
	for _, r := range cfg.Project.Repos {
		if strings.EqualFold(strings.TrimSpace(r), strings.TrimSpace(repo)) {
			return true
		}
	}
	return false
}

// runLegacyFleetReportSweepIfDue closes standalone "Fleet report:" issues a
// spoke still on a pre-#13470 build filed directly in the report repo
// (hivecommons/hive#13567). Only the hive that watches that repo runs it —
// it is the one with write access there — at most once per
// legacyFleetReportSweepInterval.
func runLegacyFleetReportSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil {
		return
	}
	repo := cfg.Governor.FleetReport.EffectiveRepo()
	if !hiveWatchesRepo(cfg, repo) {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < legacyFleetReportSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepLegacyFleetReportIssues(ctx, github.LegacyFleetReportSweepOptions{
		Repo: repo,
		Audit: func(event github.LegacyFleetReportSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, author=%s, title=%s", event.Repo, event.Number, event.Author, event.Title)
			dashSrv.AuditLogRecord("system", "legacy-fleet-report-closed", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("legacy fleet report sweep failed", "error", err)
		return
	}
	for _, e := range result.Errors {
		logger.Warn("legacy fleet report sweep: issue skipped", "detail", e)
	}
	if len(result.Closed) > 0 {
		logger.Info("legacy fleet report sweep complete", "scanned", result.Scanned, "closed", len(result.Closed), "skipped", result.Skipped)
	}
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
			dashSrv.AuditLogRecord("system", "task-list-sweep-closed", detail, "", event.Repo, event.Number)
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

func runSupersessionSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < supersessionSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	var supersessionCfg config.SupersessionSweepConfig
	if cfg != nil {
		supersessionCfg = cfg.SupersessionSweep
	}
	result, err := ghClient.SweepSupersededOpenPRs(ctx, github.SupersessionSweepOptions{
		MaxActions:          github.DefaultSupersessionSweepMaxActions,
		CloseContributorPRs: supersessionCfg.CloseContributorPRs,
		GracePeriod:         supersessionCfg.GracePeriod,
		AutoCloseSuperseded: func() bool { return cfg == nil || cfg.Governor.AutoCloseSupersededEnabled() },
		ACMMLevelForRepo: func(repo string) int {
			if cfg == nil {
				return 0
			}
			return cfg.EffectiveACMMLevelForRepo(repo)
		},
		Audit: func(event github.SupersessionSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, issue=%s#%d, closer_pr=%d, action=%s, reason=%s",
				event.Repo, event.Number, event.IssueRepo, event.Issue, event.CloserPR, event.Action, event.Reason)
			dashSrv.AuditLogRecord("system", github.SupersessionSweepAuditPrefix+event.Action, detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("supersession sweep failed", "error", err)
		return
	}
	if len(result.Closed) > 0 || len(result.Commented) > 0 || len(result.Resolved) > 0 || result.Seen > 0 {
		logger.Info("supersession sweep complete", "seen", result.Seen, "closed", len(result.Closed), "commented", len(result.Commented), "resolved", len(result.Resolved), "skipped", result.Skipped)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "supersession sweep complete",
		Attrs: map[string]string{
			"seen":      strconv.Itoa(result.Seen),
			"closed":    strconv.Itoa(len(result.Closed)),
			"commented": strconv.Itoa(len(result.Commented)),
			"resolved":  strconv.Itoa(len(result.Resolved)),
			"skipped":   strconv.Itoa(result.Skipped),
		},
	})
}

// runDuplicateFixSweepIfDue resolves same-issue duplicate open PRs
// (hivecommons/hive#14799) at most once per duplicateFixSweepInterval: the
// earlier green-or-pending PR stays, the later hive PR is closed as
// duplicate-of:<keeper>, a person's later PR gets the comment only, and the
// earlier-red-later-green case is parked for the owner. It follows the
// pre-dispatch guard's switch: scanner.skip_issues_with_open_pr off turns
// this off too, since both exist to keep one issue on one PR.
func runDuplicateFixSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	if cfg != nil && !cfg.Scanner.IsSkipIssuesWithOpenPR() {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < duplicateFixSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepDuplicateFixPRs(ctx, github.DuplicateFixSweepOptions{
		MaxActions: github.DefaultDuplicateFixSweepMaxActions,
		Audit: func(event github.DuplicateFixEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, issue=%s#%d, keeper_pr=%d, action=%s, reason=%s",
				event.Repo, event.Number, event.IssueRepo, event.Issue, event.KeeperPR, event.Action, event.Reason)
			dashSrv.AuditLogRecord("system", github.DuplicateFixSweepAuditPrefix+event.Action, detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("duplicate-fix sweep failed", "error", err)
		return
	}
	if result.Pairs > 0 {
		logger.Info("duplicate-fix sweep complete", "pairs", result.Pairs, "closed", len(result.Closed), "commented", len(result.Commented), "escalated", len(result.Escalated), "skipped", result.Skipped)
	}
}

// runSharedCIRerunSweepIfDue re-runs failed jobs on open PRs deferred to a
// [shared-ci] incident that has since closed (hivecommons/hive#12253), at most
// once per sharedCIRerunInterval. Gates and dedupe live in
// SweepSharedCIReruns; this is the scheduler and dashboard audit sink.
func runSharedCIRerunSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	if cfg != nil && !cfg.SharedCIRerun.IsEnabled() {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < sharedCIRerunInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepSharedCIReruns(ctx, github.SharedCIRerunOptions{
		MaxPRs: github.DefaultSharedCIRerunMaxPRs,
		Audit: func(event github.SharedCIRerunEvent) {
			if dashSrv == nil {
				return
			}
			incidents := make([]string, len(event.Incidents))
			for i, n := range event.Incidents {
				incidents[i] = "#" + strconv.Itoa(n)
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, incident=%s, head=%s, runs=%d",
				event.Repo, event.Number, strings.Join(incidents, ","), event.HeadSHA, len(event.RunIDs))
			dashSrv.AuditLogRecord("system", "shared-ci-rerun", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("shared-ci rerun sweep failed", "error", err)
		return
	}
	if len(result.Rerun) > 0 {
		logger.Info("shared-ci rerun sweep complete", "incidents", result.Incidents, "seen", result.Seen, "rerun", len(result.Rerun), "skipped", result.Skipped)
	}
}

// blockedIssuesSweepInterval is how often the periodic sweep reconciles open
// issues carrying the `blocked` label.
const blockedIssuesSweepInterval = 5 * time.Minute

func runBlockedIssuesSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < blockedIssuesSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	var repos []string
	if cfg != nil {
		repos = cfg.Project.Repos
	}
	res, err := ghClient.SweepBlockedIssues(ctx, github.BlockedSweepOptions{Repos: repos})
	if err != nil {
		logger.Warn("blocked issues reconciliation sweep failed", "error", err)
		return
	}
	if res.Unblocked+res.Retargeted+res.Stripped+res.Errors > 0 {
		logger.Info("blocked issues reconciliation sweep complete", "items", res.Items, "unblocked", res.Unblocked, "retargeted", res.Retargeted, "help_wanted_stripped", res.Stripped, "errors", res.Errors)
	}
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
			dashSrv.AuditLogRecord("system", "duplicate-sweep-suggested", detail, "", event.Repo, event.Survivor)
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

// sentinelSweepInterval is the reconcile cadence of the full sentinel pass
// (#13379). `pull_request` opened/synchronize/reopened webhooks evaluate the
// named PR as they arrive (runSentinelPREvent); the full pass enumerates
// open PRs from the forgestate cache, skips every head it already evaluated
// without a GitHub call, and exists to catch a PR whose delivery was lost.
// Fifteen minutes bounds how long such a PR sits unlabelled.
const sentinelSweepInterval = github.SentinelReconcileInterval

// sentinelSweepOptions is the one option set both sentinel entry points use,
// so the webhook path and the reconcile pass cannot disagree on label,
// trust or rules.
func sentinelSweepOptions(cfg *config.Config, ghClient *github.Client, dashSrv *dashboard.Server) github.SentinelSweepOptions {
	sc := cfg.Sentinel
	return github.SentinelSweepOptions{
		Label:               sc.LabelOrDefault(),
		LabelColor:          config.DefaultSentinelLabelColor,
		LabelDescription:    config.DefaultSentinelLabelDescription,
		Evaluator:           sc.EvaluatorConfig(),
		TrustedAuthor:       sentinelTrustedAuthorFunc(cfg, ghClient),
		TrustedAuthorsBlock: sc.TrustedAuthorsBlock,
		RepoAllowed:         sc.RepoAllowed,
		MaxActions:          sc.MaxActionsOrDefault(),
		Audit: func(event github.SentinelSweepEvent) {
			if dashSrv == nil {
				return
			}
			// Findings the write-back gate refused still reach the Security
			// tab and /api/sentinel: the scan happened, only the GitHub
			// write did not (#writeback containment).
			dashSrv.RecordSentinelFinding(event)
			detail := fmt.Sprintf("repo=%s, pr=%d, author=%s, head=%s, rules=%s",
				event.Repo, event.Number, event.Author, event.HeadSHA, strings.Join(event.Rules(), ","))
			action := "sentinel-alert"
			if event.Suppressed {
				action = "sentinel-finding-suppressed"
				detail += ", suppressed=" + event.SuppressedReason
			}
			dashSrv.AuditLogRecord("system", action, detail, "", event.Repo, event.Number)
		},
	}
}

// runSentinelPREvent is the dashboard.Dependencies.SentinelPREvent hook: a
// verified `pull_request` opened/synchronize/reopened delivery evaluates
// that one PR now, reading only it and its files. Default on with the
// sweep; `sentinel.enabled: false` disables both.
func runSentinelPREvent(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, repo string, number int, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.Sentinel.IsEnabled() {
		return
	}
	result, err := ghClient.SweepSentinelPR(ctx, repo, number, sentinelSweepOptions(cfg, ghClient, dashSrv))
	if err != nil {
		logger.Warn("sentinel webhook evaluation failed", "repo", repo, "pr", number, "error", err)
		return
	}
	logSentinelResult(result, logger)
}

// runSentinelSweepIfDue flags open PRs that look like security overrides,
// privilege escalation or codebase damage (pkg/sentinel) at most once per
// sentinelSweepInterval. Default on; `sentinel.enabled: false` disables it.
func runSentinelSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.Sentinel.IsEnabled() {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < sentinelSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepSentinel(ctx, sentinelSweepOptions(cfg, ghClient, dashSrv))
	if err != nil {
		logger.Warn("sentinel sweep failed", "error", err)
		return
	}
	logSentinelResult(result, logger)
	if len(result.Flagged) > 0 || len(result.Notified) > 0 || len(result.Remediated) > 0 || result.Seen > 0 {
		logger.Info("sentinel sweep complete", "seen", result.Seen, "flagged", len(result.Flagged), "notified", len(result.Notified), "suppressed", len(result.Suppressed), "remediated", len(result.Remediated), "skipped", result.Skipped, "errors", len(result.Errors), "cached_repos", result.CachedRepos, "listed_repos", result.ListedRepos)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "sentinel sweep complete",
		Attrs: map[string]string{
			"seen":       strconv.Itoa(result.Seen),
			"flagged":    strconv.Itoa(len(result.Flagged)),
			"notified":   strconv.Itoa(len(result.Notified)),
			"remediated": strconv.Itoa(len(result.Remediated)),
			"skipped":    strconv.Itoa(result.Skipped),
		},
	})
}

func logSentinelResult(result *github.SentinelSweepResult, logger *slog.Logger) {
	for _, e := range result.Errors {
		logger.Warn("sentinel sweep: PR skipped", "detail", e)
	}
	for _, ev := range result.Flagged {
		logger.Warn("sentinel alert", "repo", ev.Repo, "pr", ev.Number, "author", ev.Author, "rules", strings.Join(ev.Rules(), ","))
	}
	for _, ev := range result.Notified {
		logger.Info("sentinel notice", "repo", ev.Repo, "pr", ev.Number, "author", ev.Author, "trusted_reason", ev.TrustedReason, "rules", strings.Join(ev.Rules(), ","))
	}
	for _, ev := range result.Remediated {
		logger.Info("sentinel unflagged trusted author PR", "repo", ev.Repo, "pr", ev.Number, "author", ev.Author, "trusted_reason", ev.TrustedReason, "head", ev.HeadSHA)
	}
}

// issueUnparkSweepInterval is the minimum spacing between un-park sweeps. A
// maintainer who replies "/hive approve" is waiting for something to happen, so
// this runs more often than the task-list sweep; it only reads the comments of
// issues that are actually parked, which keeps the API cost proportional to the
// backlog a person is waiting on.
const issueUnparkSweepInterval = 5 * time.Minute

// issueUnparkSweepWebhookInterval is the minimum spacing when an `issues`
// webhook has marked a label change (hivecommons/hive#11313): a maintainer who
// just added `triage/accepted` should not wait out the polling interval, but a
// burst of label events must not turn into a sweep per event.
const issueUnparkSweepWebhookInterval = 30 * time.Second

// runIssueUnparkSweepIfDue re-queues parked issues a maintainer has answered
// with a `/hive` command, and keeps the "What to reply" block current on every
// parked issue (hivecommons/hive#9879). Every gate — human author, live write
// permission, first-line command, recent unedited comment, per-tick cap — lives
// inside SweepIssueUnparkCommands; this function is only the scheduler and the
// dashboard audit sink, mirroring runTaskListSweepIfDue above. Repos with
// project.repo_policies[].label_driven skip the notice and keep their parking
// labels (hivecommons/hive#10537).
func runIssueUnparkSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() {
		since := now.Sub(*lastRun)
		if since < issueUnparkSweepWebhookInterval {
			return
		}
		if since < issueUnparkSweepInterval && github.TakeDirtyIssueCount() == 0 {
			return
		}
	}
	if lastRun != nil {
		*lastRun = now
	}
	// `/hive approve` on a PR is answered with what the merge lane will do
	// with it (hivecommons/hive#13087); the engine here only reads policy.
	approveOpts := automerge.Options{
		MergerAuthorizer:       trustedMergerFunc(cfg),
		TrustedAuthorizer:      trustedAuthorFunc(cfg),
		TrustedAuthorPolicy:    trustedAuthorPolicyFunc(cfg),
		ContributorLanePolicy:  contributorLanePolicyFunc(cfg),
		ContributorLaneVerdict: contributorLaneVerdictFunc(cfg, logger),
	}
	if cfg != nil {
		approveOpts.RepoAutoMergeEnabled = cfg.RepoAutoMergeEnabled
		approveOpts.HumanMergePaths = func(repo string) []string { return humanMergePathsFor(cfg, repo) }
		approveOpts.HumanMergeRule = func() prstage.HumanMergeRule { return humanMergeRuleFor(cfg) }
		approveOpts.TrustedBotAuthors = func() map[string]bool { return cfg.AutoMerge.TrustedBotAuthorSet() }
	}
	approveEngine := automerge.New(ghClient, approveOpts)
	result, err := ghClient.SweepIssueUnparkCommands(ctx, github.IssueUnparkSweepOptions{
		PRApproveReply: approveEngine.PRApproveReply,
		MaxActions:     github.DefaultIssueUnparkSweepMaxActions,
		// cfg is nil only in tests; nil leaves every repo on the default path.
		LabelDriven:   func(repo string) bool { return cfg.RepoLabelDriven(repo) },
		LabelApproval: labelApprovalPolicyResolver(cfg),
		LabelApprovalAudit: func(event github.IssueLabelApprovalEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, label=%s, actor=%s, trust=%s, removed=%s, released=%t, pending=%t",
				event.Repo, event.Number, event.Label, event.Actor, event.TrustReason, strings.Join(event.Removed, ","), event.Released, event.Pending)
			dashSrv.AuditLogRecord("system", "issue-approved-by-label", detail, "", event.Repo, event.Number)
		},
		Audit: func(event github.IssueUnparkEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, actor=%s, decision=%s",
				event.Repo, event.Number, event.Actor, event.Decision)
			dashSrv.AuditLogRecord("system", "issue-unparked-by-reply", detail, "", event.Repo, event.Number)
		},
		ClosedAudit: func(event github.IssueUnparkEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, actor=%s", event.Repo, event.Number, event.Actor)
			dashSrv.AuditLogRecord("system", "issue-closed-by-fixed", detail, "", event.Repo, event.Number)
		},
		// `/hive decision` on a hive-authored PR at 085 (hivecommons/hive#13041)
		// is routed to the fix lane under the automerge lane's merger trust.
		HumanChangeRequests: review.DefaultHumanChangeRequests(),
		TrustedMerger:       trustedMergerFunc(cfg),
		PRAudit: func(event github.PullRequestDecisionEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, head=%s, actor=%s, decision=%s",
				event.Repo, event.Number, event.HeadSHA, event.Actor, event.Decision)
			dashSrv.AuditLogRecord("system", "pr-decision-routed-to-fix", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("issue un-park sweep failed", "error", err)
		return
	}
	if result == nil {
		return
	}
	if len(result.Unparked) > 0 || len(result.Closed) > 0 || result.Replies > 0 || len(result.LabelApproved) > 0 || len(result.PRDecisions) > 0 {
		logger.Info("issue un-park sweep complete",
			"seen", result.Seen, "unparked", len(result.Unparked), "closed", len(result.Closed), "label_approved", len(result.LabelApproved), "pr_decisions", len(result.PRDecisions), "replies", result.Replies, "skipped", result.Skipped)
	}
}

// closedIssueStageSweepInterval spaces the closed-issue clean-up. A closed
// issue wearing stage/045 is a stale dashboard row, not a person waiting, so
// this runs at the task-list cadence; each pass is one listing per repo.
const closedIssueStageSweepInterval = 10 * time.Minute

// closedIssueStageSweepSlack widens each pass's `since` so an issue that
// closed while the previous pass was running is not missed.
const closedIssueStageSweepSlack = 10 * time.Minute

// runClosedIssueStageSweepIfDue strips hive stage and park labels from issues
// that closed while carrying them (hivecommons/homebrew-hive#127): the
// issue-stage reconciler only visits open issues, so a `Fixes #N` merge or a
// by-hand close left `stage/045-needs-decision` behind and the tracker kept
// reading it as a pending decision. Every rule lives in
// SweepClosedIssueStageLabels; this is the scheduler and audit sink.
func runClosedIssueStageSweepIfDue(ctx context.Context, ghClient *github.Client, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	var since time.Time
	if lastRun != nil && !lastRun.IsZero() {
		if now.Sub(*lastRun) < closedIssueStageSweepInterval {
			return
		}
		since = lastRun.Add(-closedIssueStageSweepSlack)
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepClosedIssueStageLabels(ctx, github.ClosedIssueStageSweepOptions{
		Since:      since,
		MaxActions: github.DefaultClosedIssueStageSweepMaxActions,
		Audit: func(event github.ClosedIssueStageEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, stage=%s, needs_human=%t",
				event.Repo, event.Number, strings.Join(event.Stage, ","), event.NeedsHuman)
			dashSrv.AuditLogRecord("system", "closed-issue-stage-cleared", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("closed-issue stage sweep failed", "error", err)
	} else if result != nil && (len(result.Cleaned) > 0 || result.Skipped > 0) {
		logger.Info("closed-issue stage sweep complete", "seen", result.Seen, "cleaned", len(result.Cleaned), "skipped", result.Skipped)
	}
	runReviewBacklogSplitSweep(ctx, ghClient, dashSrv, since, logger)
}

// runReviewBacklogSplitSweep re-files still-unchecked review findings folded
// into an issue that has since closed (hivecommons/hive#13914). It shares the
// closed-issue sweep's cadence and window; every rule lives in
// SweepReviewBacklogSplits. It then runs the one-off
// BackfillReviewBacklogSplits for hosts closed before that window
// (hivecommons/hive#13947), a no-op once the ledger records it complete.
func runReviewBacklogSplitSweep(ctx context.Context, ghClient *github.Client, dashSrv *dashboard.Server, since time.Time, logger *slog.Logger) {
	audit := func(event github.ReviewBacklogSplitEvent) {
		if dashSrv == nil {
			return
		}
		detail := fmt.Sprintf("repo=%s, host=%d, issue=%d, reused=%t, key=%s",
			event.Repo, event.Host, event.Number, event.Reused, event.Key)
		dashSrv.AuditLogRecord("system", "review-backlog-split", detail, "", event.Repo, event.Host)
	}
	result, err := ghClient.SweepReviewBacklogSplits(ctx, github.ReviewBacklogSplitSweepOptions{
		Since:      since,
		MaxActions: github.DefaultReviewBacklogSplitMaxActions,
		Audit:      audit,
	})
	if err != nil {
		logger.Warn("review backlog split sweep failed", "error", err)
		return
	}
	if result != nil && (len(result.Split) > 0 || result.Skipped > 0 || result.Deferred > 0) {
		logger.Info("review backlog split sweep complete", "hosts", result.Hosts, "split", len(result.Split), "skipped", result.Skipped, "deferred", result.Deferred)
	}
	backfill, err := ghClient.BackfillReviewBacklogSplits(ctx, github.ReviewBacklogSplitSweepOptions{
		MaxActions: github.DefaultReviewBacklogSplitMaxActions,
		Audit:      audit,
	})
	if err != nil {
		logger.Warn("review backlog split backfill failed", "error", err)
		return
	}
	if backfill != nil && (len(backfill.Split) > 0 || backfill.Skipped > 0 || backfill.Deferred > 0) {
		logger.Info("review backlog split backfill pass complete", "hosts", backfill.Hosts, "split", len(backfill.Split), "skipped", backfill.Skipped, "deferred", backfill.Deferred)
	}
}

// labelApprovalPolicyResolver reads label-driven approval settings
// (hivecommons/hive#11313) live from cfg. It answers ok=false for every repo
// without project.repo_policies[].label_driven, which keeps the feature off
// there; a nil cfg (tests) opts nothing in.
func labelApprovalPolicyResolver(cfg *config.Config) func(repo string) (github.LabelApprovalPolicy, bool) {
	return func(repo string) (github.LabelApprovalPolicy, bool) {
		if cfg == nil {
			return github.LabelApprovalPolicy{}, false
		}
		labels := cfg.LabelDrivenApprovalLabels(repo)
		if len(labels) == 0 {
			return github.LabelApprovalPolicy{}, false
		}
		return github.LabelApprovalPolicy{
			Labels:              labels,
			TrustOrgMembers:     cfg.LabelDrivenTrustOrgMembers(),
			TrustedBots:         cfg.LabelDrivenTrustedBots(),
			SupersedeParkActors: cfg.LabelDrivenSupersedeParkActors(repo),
		}, true
	}
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

// issueStageSyncTimeout bounds one eval pass's issue stage-label sync so a
// rate-limit pacing queue cannot stall the run loop (hivecommons/hive#12983).
const issueStageSyncTimeout = 2 * time.Minute

// recheckSoakHubURL is the hub the soak triage probes, "" when this hive has
// no hub (the probe then reads the spoke's own /api/version).
func recheckSoakHubURL(cfg *config.Config) string {
	if cfg == nil || !cfg.Hub.Enabled {
		return ""
	}
	return cfg.Hub.URL
}

// applyDuplicatePRGuard filters issues already claimed by an open hive-authored
// PR out of the actionable set. Failures are logged, never fatal: the guard is
// a safety net, and a broken net must not take the hive down with it.
// reconcileLedgerCoverRecords runs github.BackfillCoveredByLedger with the
// cycle's red+stale valve. It is not an optional step: with webhook-driven
// cached cycles the reconcile pass (and the stage sweep with it) can be
// hours apart, and a record at 010 under an open covering PR re-offers the
// issue to the scanner for all of that time.
func reconcileLedgerCoverRecords(ctx context.Context, cfg *config.Config, ghClient *github.Client, actionable *github.ActionableResult, logger *slog.Logger) {
	ledger := getClaimLedger(logger)
	if ledger == nil || ledger.Len() == 0 || ghClient == nil {
		return
	}
	backfillCtx, cancel := context.WithTimeout(ctx, issueStageSyncTimeout)
	defer cancel()
	res := ghClient.BackfillCoveredByLedger(backfillCtx, ledger, claimingPRRedStale(cfg, actionable))
	if res.Covered+res.Errors > 0 && logger != nil {
		logger.Info("stage backfill: issue records reconciled with the claim ledger",
			"checked", res.Checked, "covered", res.Covered, "skipped_red_stale", res.SkippedRedStale,
			"skipped_not_open", res.SkippedNotOpen, "errors", res.Errors)
	}
}

func applyDuplicatePRGuard(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	logger *slog.Logger,
) {
	// The PR-claim guard runs FIRST: an open PR that fixes the issue is the
	// stronger evidence, and the guards below only walk what is still in
	// the actionable set. With the live-claim filter ahead of it, an issue
	// whose agent claim was still live when its PR opened was withheld as
	// "claimed" before the ledger could bucket it "claimed_by_pr", so every
	// sweep re-derived 020 under the PR-opened 040 and the stage store
	// recorded a spurious regression (hivecommons/hive#13174 et al.). The
	// ledger's open-issue set (OpenIssuesOf) likewise needs the full list.
	ledger := getClaimLedger(logger)
	if ledger != nil {
		github.ApplyDuplicatePRGuard(ctx, ghClient, ledger, hiveIdentity(cfg), actionable, claimingPRRedStale(cfg, actionable), logger)
	}
	// #8380: a LIVE issue claim covers an issue the same way an open PR does,
	// for every kick prompt (the scanner's included). The claim fields are
	// only ever set while governor.claims.enabled is on, so with the feature
	// off this touches nothing.
	// hivecommons/hive#13579: a contributor claim on an issue in flap
	// cool-off, or one that has starved hive agents past the configured
	// window with no PR, is advisory — the issue stays actionable.
	if cfg != nil && cfg.Governor.Claims.Enabled {
		if withheld := contributorClaimGuard.Load().FilterLiveIssueClaims(actionable, time.Now(), logger); withheld > 0 {
			logger.Info("issue-claim guard applied", "withheld", withheld)
		}
	}
	if ledger == nil {
		return
	}
	// The in-progress sweep runs after the PR-claim guard so an issue whose
	// PR just opened sheds the start mark here, and a lapsed mark frees the
	// issue before kicks are built. Independent of governor.claims: the
	// label is read from the issue, the marker from the comment, on every hive.
	if withheld := github.SweepInProgressIssues(ctx, ghClient, actionable, time.Now(), logger); withheld > 0 {
		logger.Info("in-progress guard applied", "withheld", withheld)
	}
	// hivecommons/hive#12436: with every bucket final, mirror the cycle's
	// decision about each open issue onto its stage/NNN label (and strip the
	// retired claimed / hive/in-progress / 🐝 covered by PR labels).
	// hivecommons/hive#12983: the run loop holds cfgReloadMu for the whole
	// pass, so bound the sync; a paced request whose slot would miss the
	// deadline fails fast and the best-effort sync retries next cycle.
	stageCtx, stageCancel := context.WithTimeout(ctx, issueStageSyncTimeout)
	stageCtx, stageSpan := tracing.StartSpan(stageCtx, "governor.stage_labels")
	syncStageEngineMode(cfg, ghClient, logger)
	res := github.SyncIssueStageLabelsWith(stageCtx, ghClient, actionable, nil, time.Now(), logger, stageEngineOptions(cfg, logger))
	stageSpan.SetAttributes(
		attribute.Int("hive.stage_labels.items", res.Items),
		attribute.Int("hive.stage_labels.writes", res.Writes))
	stageSpan.End()
	stageCancel()
	if res.Writes+res.Comments > 0 {
		logger.Info("issue stage labels applied", "items", res.Items, "writes", res.Writes, "comments", res.Comments, "healed", res.Healed)
	}
	// hivecommons/hive#13093: issues parked on an unmerged PR come back once
	// that PR settles, and never advertise `help wanted` while parked.
	// hivecommons/hive#13311: ops-tracking soak checklists past their
	// recheck-after deadline get evidence from the hub (or this spoke).
	github.SetRecheckSoakProbe(github.NewHTTPSoakProbe(recheckSoakHubURL(cfg), fmt.Sprintf("http://localhost:%d", cfg.Dashboard.Port), nil))
	blockedCtx, blockedCancel := context.WithTimeout(ctx, issueStageSyncTimeout)
	// hivecommons/hive#13833: review-backlog items wait on the PR they were
	// filed from in every destination; lift them when it merges, and when it
	// closes unmerged unblock them with a re-triage note (never auto-close).
	// Runs first so the items get the sweep's own note rather than the
	// generic one from the sweep below.
	if rb := github.ReconcileReviewBacklogBlocks(blockedCtx, ghClient, logger); rb.Lifted+rb.Retriaged+rb.Errors > 0 {
		logger.Info("review backlog blocks reconciled", "checked", rb.Checked, "lifted", rb.Lifted, "retriaged", rb.Retriaged, "errors", rb.Errors)
	}
	if br := github.ReconcileBlockedIssues(blockedCtx, ghClient, actionable, logger); br.Unblocked+br.Retargeted+br.Stripped+br.SoakTriaged+br.Errors > 0 {
		logger.Info("blocked issues reconciled", "items", br.Items, "unblocked", br.Unblocked, "retargeted", br.Retargeted, "help_wanted_stripped", br.Stripped, "soak_triaged", br.SoakTriaged, "errors", br.Errors)
	}
	blockedCancel()
	// Closing an issue is irreversible: refuse while the snapshot is stale and
	// re-read an item whose copy is older than DecisionFreshnessBound.
	runCloseOnMergeSweep(github.WithIrreversibleGuard(ctx, ghClient.SnapshotFreshnessGuard(actionable, time.Now)), cfg, ghClient, ledger, logger)
}

// inProgressSetting resolves the pr-in-progress stage label setting from config.
// The label follows the claims label mirror: an operator who set
// governor.claims.label: false asked for no hive-managed claim labels, and
// gets none here either; otherwise the start signal is always labelled. The
// mark lasts the agent claim TTL, so both the ledger and the label agree on
// when an agent's hold lapses.
func inProgressSetting(cfg *config.Config) github.InProgressSetting {
	if cfg == nil {
		return github.InProgressSetting{}
	}
	_, agentTTL, _, _ := cfg.Governor.Claims.EffectiveTTLs()
	return github.InProgressSetting{
		Enabled: !(cfg.Governor.Claims.IsEnabled() && !cfg.Governor.Claims.LabelEnabled()),
		HiveID:  cfg.HiveID,
		TTL:     agentTTL,
	}
}

func logCloseOnMergeResults(logger *slog.Logger, msg string, results []github.CloseOnMergeResult) {
	if logger == nil || len(results) == 0 {
		return
	}
	var closed, awaiting, skipped, noop int
	for _, r := range results {
		switch r.Action {
		case github.CloseOnMergeClosed:
			closed++
		case github.CloseOnMergeAwaiting:
			awaiting++
		case github.CloseOnMergeSkipped:
			skipped++
		default:
			noop++
		}
	}
	if closed > 0 || awaiting > 0 || skipped > 0 {
		logger.Info(msg, "closed", closed, "awaiting_confirmation", awaiting, "skipped", skipped, "noop", noop)
	}
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
		// A person reopening an issue after its closing PR merged voids the
		// merged claim right away (hivecommons/hive#9945 sat likely-done for
		// hours after a /reopen); the enumeration pass is the fallback when
		// webhooks are not delivered.
		github.SetIssueReopenedHook(issueReopenedHookFor(ledger, logger))
	})
	return claimLedger
}

// issueReopenedHookFor builds the webhook hook that voids a merged claim on a
// reopened issue and persists the ledger, logging `reopened issue re-queued`.
func issueReopenedHookFor(ledger *github.ClaimLedger, logger *slog.Logger) github.IssueReopenedHook {
	return func(repo string, number int, at time.Time) {
		if ledger == nil || !ledger.MarkIssueReopened(repo, number, at) {
			return
		}
		claim, _ := ledger.LookupAny(repo, number)
		if logger != nil {
			logger.Info("reopened issue re-queued: merged PR claim voided",
				"repo", repo, "issue", number, "claimed_by_pr", claim.PRNumber, "pr_repo", claim.PRRepo,
				"pr_url", claim.PRURL, "merged_at", claim.MergedAt, "reopened_at", at, "source", "webhook")
		}
		if err := ledger.Save(); err != nil && logger != nil {
			logger.Warn("reopened issue: failed to persist claim ledger", "repo", repo, "issue", number, "error", err)
		}
	}
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

// reviewAgentCapability describes one configured lane the way the review
// dispatcher sees it, so the relay judges reviewer-ness by the same rule.
// An unknown lane has only its name to go on.
func reviewAgentCapability(cfg *config.Config, agent string) review.AgentCapability {
	if cfg != nil {
		if ac, ok := cfg.Agents[agent]; ok {
			return review.CapabilityFromConfig(agent, ac)
		}
	}
	return review.AgentCapability{Name: agent}
}

// reviewAuthorBase resolves an authoring lane to its base agent over the full
// configured agent set, enabled or not. A replica name the current config no
// longer materialises (its replicas: count was reduced) still maps to the
// declared base it was derived from, so the base and its remaining replicas
// stay excluded from reviewing it.
func reviewAuthorBase(cfg *config.Config, agent string) string {
	agent = strings.TrimSpace(agent)
	if cfg == nil || agent == "" {
		return agent
	}
	if _, ok := cfg.Agents[agent]; ok {
		return cfg.BaseAgentName(agent)
	}
	if i := strings.LastIndex(agent, "-"); i > 0 {
		if n, err := strconv.Atoi(agent[i+1:]); err == nil && n >= 2 {
			if ac, ok := cfg.Agents[agent[:i]]; ok && ac.ReplicaOf == "" {
				return agent[:i]
			}
		}
	}
	return agent
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
	client.SetReviewerAgentFunc(func(agent string) bool {
		return review.ReviewCapable(reviewAgentCapability(cfg, agent), review.ReviewerAgentSet(cfg.Review.ReviewerAgents))
	})
	client.SetAgentBaseFunc(func(agent string) string {
		return reviewAuthorBase(cfg, agent)
	})
	client.SetConfidenceScore(func() bool { return cfg.Review.ConfidenceScore })
	client.SetReviewBacklog(func() (bool, int) {
		return !cfg.Review.OutOfScopeBacklogDisabled, cfg.Review.MaxOutOfScopeBacklogIssues
	})
	client.SetReviewBacklogRouting(func() github.ReviewBacklogRouting {
		return reviewBacklogRouting(client, cfg, logger)
	})
	client.SetReviewEvidence(func(repo string) github.ReviewEvidenceSettings {
		return reviewEvidenceSettings(cfg, repo)
	})
	// hivecommons/hive#14581: issue-author credit on every merge path, read
	// live so a reload of auto_merge.credit_issue_authors applies.
	client.SetCreditIssueAuthors(func(string) bool { return cfg.AutoMerge.CreditIssueAuthorsEnabled() })
	// hivecommons/hive#13241: an issue a person reopens after its closing PR
	// merged is a change failure on that PR's evidence record, from both the
	// issues.reopened webhook and the enumeration reopen check.
	github.SetIssueReopenRecorder(client.RecordReviewEvidenceIssueReopen)
	// #8380: issue claims are read at enumeration time only while
	// governor.claims.enabled is on; the setting is read live so the Features
	// toggle applies without a client rebuild.
	client.SetIssueClaims(func() (bool, time.Duration) {
		return cfg.Governor.Claims.Enabled, cfg.Governor.Claims.EffectiveTTL()
	})
	client.SetInProgress(func() github.InProgressSetting { return inProgressSetting(cfg) })
	// hivecommons/hive#12436: the issue stage reconciler follows the same
	// governor.labels.stage policy as the PR one, read live.
	client.SetIssueStageLabels(func() github.IssueStagePolicy {
		p := stageLabelPolicyFunc(cfg)()
		return github.IssueStagePolicy{Enabled: p.Enabled, Dwell: p.Dwell}
	})
	client.SetReviewCadenceLimits(
		func() bool { return cfg.Review.CombinedPerspectives },
		func() int { return cfg.Review.MaxReviewsPerHead },
	)
	// review.require_labels (PR approval allow-list): the relay's half of the
	// gate, read live so a Features-tab edit applies to the next request.
	client.SetReviewRequireLabels(func() []string { return cfg.Review.RequireLabels })
}

// reviewBacklogRouting resolves review.severity / review.backlog and the
// backlog sink for the active work source (hivecommons/hive#11089). A
// destination whose work source cannot be built falls back to GitHub issues;
// the reason is logged so the operator can fix the work-source config.
func reviewBacklogRouting(client *github.Client, cfg *config.Config, logger *slog.Logger) github.ReviewBacklogRouting {
	ghToken := cfg.GitHub.Token
	if ghToken == "" {
		ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
	}
	sink, err := worksource.NewReviewBacklogSink(cfg.Governor.WorkSource, cfg.Review.Backlog, client, ghToken, cfg.Project.Org, logger)
	if err != nil && logger != nil {
		logger.Warn("review backlog: destination unavailable, filing GitHub issues", "error", err)
	}
	return github.ReviewBacklogRouting{Severity: cfg.Review.Severity, Backlog: cfg.Review.Backlog, Sink: sink}
}

// installReviewBots installs classification.review_bots on a (possibly
// rebuilt) GitHub client (hivecommons/hive#7360). hive.yaml's block wins;
// otherwise the same key is read from hive-project.yaml
// (config.DefaultProjectYAMLPath, overridable via HIVE_PROJECT_YAML — the
// path the bash pipeline stages already honour). Nil-safe: a hive without
// GitHub credentials runs with a nil client for the life of the process.
// Also installs review.fix_human_prs (hivecommons/hive#9361), so the
// review-thread reconciler follows PRs the hive did not open whenever the
// fixer is already allowed to push to them.
func installReviewBots(client *github.Client, cfg *config.Config, logger *slog.Logger) {
	if client == nil || cfg == nil {
		return
	}
	rb, err := cfg.EffectiveReviewBots(os.Getenv("HIVE_PROJECT_YAML"))
	if err != nil && logger != nil {
		logger.Warn("classification.review_bots: project file unreadable; review-thread reconciler stays off", "error", err)
	}
	client.SetReviewBots(rb)
	client.SetFixHumanPRs(cfg.Review.FixHumanPRsEnabled())
	if logger != nil && rb.Enabled() {
		logger.Info("review-thread reconciler enabled",
			"review_bots", rb.Logins,
			"max_attempts_per_thread", rb.MaxAttempts(),
			"resolve_after_fix", rb.ResolveAfterFixEnabled(),
			"fix_human_prs", cfg.Review.FixHumanPRsEnabled())
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
	failing := map[string]struct{}{}
	defer func() { humanLabelWarn.retain(failing) }()
	for _, hold := range plan.State.Human {
		if labeled[strings.ToLower(fmt.Sprintf("%s#%d", fullRepoName(hold.Repo, org), hold.Number))] {
			continue
		}
		if err := ghClient.ApplyHumanDecisionLabel(ctx, hold.Repo, hold.Number, label); err != nil {
			// Missing optional labels are configuration notices, not runtime
			// failures: first warnings across different PRs can trigger fleet
			// reports even with per-PR deduplication (#13857).
			key := prWarnKey(hold.Repo, hold.Number)
			missing := errors.Is(err, github.ErrHumanDecisionLabelMissing)
			if missing {
				key += ":missing-label"
			}
			failing[key] = struct{}{}
			level := humanLabelWarn.level(key)
			if missing && level == slog.LevelWarn {
				level = slog.LevelInfo
			}
			logger.Log(ctx, level, "human decision label not applied; review marker still stands",
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
		set = review.PerspectiveSet{}
	}
	// plan_match (#8317) is a separate switch from the selection so turning
	// it on never requires spelling out the whole set. Applied on the
	// fallback path too: a typo elsewhere in the selection must not silently
	// turn plan matching off.
	if cfg.Review.PlanMatch.Enabled {
		set = set.WithPlanMatch()
	}
	return set
}

// planWaveFor renders the approved plan a PR's run trailers name, for the
// plan_match perspective (#8317). The Hive-Plan ref is tried first, then the
// Hive-Run key, since either may carry the epic ID or the source issue ref.
// Empty when plan_match is off, the PR has no trailer, or no store holds the
// plan -- the kick tells the reviewer which of those it is.
func planWaveFor(cfg *config.Config, stores map[string]*beads.Store, planRef, runKey string) string {
	if cfg == nil || !cfg.Review.PlanMatch.Enabled {
		return ""
	}
	for _, ref := range []string{planRef, runKey} {
		if tree, ok := planning.FindPlanTree(stores, ref); ok {
			return tree.WaveText()
		}
	}
	return ""
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

// reviewPlannerGitHub is the slice of the GitHub client the review planner
// reads: a PR's changed paths for the bot-author model route
// (review.bot_author_full_review_paths) and the compare between the head
// the hive last judged and the current one for the delta re-review
// (review.delta_rereview).
type reviewPlannerGitHub interface {
	ReviewPRChangedFiles(ctx context.Context, repo string, number int) ([]string, bool)
	ReviewDeltaDiff(ctx context.Context, repo, previousHead, head string) review.DeltaDiff
}

func planReviewDispatch(cfg *config.Config, actionable *github.ActionableResult, agentMgr crux.Runtime, beadStores map[string]*beads.Store, logger *slog.Logger) review.DispatchPlan {
	return planReviewDispatchWith(context.Background(), cfg, actionable, agentMgr, beadStores, nil, logger)
}

// reviewBotAuthorRoute is review.bot_authors / bot_author_model /
// bot_author_full_review_paths resolved for the planner.
func reviewBotAuthorRoute(cfg *config.Config) review.BotAuthorRoute {
	backend, model := cfg.Review.BotAuthorModelRef()
	return review.BotAuthorRoute{Authors: cfg.Review.EffectiveBotAuthors(), Backend: backend, Model: model, FullReviewPaths: cfg.Review.EffectiveBotAuthorFullReviewPaths()}
}

// reviewBotRouteFilesFor fetches a PR's changed paths when the bot-author
// route may apply to it: route on, author a listed bot, no verdict at this
// head and no review in flight (one listing per head, not per cycle). nil,
// false otherwise, which the planner reads as unknown: no downgrade.
func reviewBotRouteFilesFor(ctx context.Context, cfg *config.Config, lister reviewPlannerGitHub, route review.BotAuthorRoute, artifact review.Artifact, state review.DispatchState, repo string, number int, author, headSHA, bodyDigest string) ([]string, bool) {
	if lister == nil || !route.Enabled() || !review.IsBotAuthor(author, route.Authors, cfg.EffectiveAIAuthor()) {
		return nil, false
	}
	if _, judged := artifact.AggregateForBody(repo, number, headSHA, bodyDigest); judged {
		return nil, false
	}
	if !state.PendingSince(repo, number, headSHA).IsZero() {
		return nil, false
	}
	return lister.ReviewPRChangedFiles(ctx, repo, number)
}

// reviewDeltaDiffFor fetches the delta compare for one PR when it is a
// delta re-review candidate: delta mode on, no verdict yet at this head, no
// dispatch entry for it (a compare per cycle for a review in flight would be
// API spend for nothing), and a prior verdict at another head to diff from.
// nil otherwise, which the planner reads as "not fetched".
func reviewDeltaDiffFor(ctx context.Context, cfg *config.Config, differ reviewPlannerGitHub, artifact review.Artifact, state review.DispatchState, repo string, number int, headSHA, bodyDigest string) *review.DeltaDiff {
	if differ == nil || !cfg.Review.DeltaRereviewEnabled() || strings.TrimSpace(headSHA) == "" {
		return nil
	}
	if _, judged := artifact.AggregateForBody(repo, number, headSHA, bodyDigest); judged {
		return nil
	}
	if !state.PendingSince(repo, number, headSHA).IsZero() {
		return nil
	}
	prior, ok := artifact.PriorAggregateFor(repo, number, headSHA)
	if !ok {
		return nil
	}
	diff := differ.ReviewDeltaDiff(ctx, repo, prior.HeadSHA, headSHA)
	return &diff
}

func planReviewDispatchWith(ctx context.Context, cfg *config.Config, actionable *github.ActionableResult, agentMgr crux.Runtime, beadStores map[string]*beads.Store, ghr reviewPlannerGitHub, logger *slog.Logger) review.DispatchPlan {
	if cfg == nil || actionable == nil || !cfg.Review.RequireApproval || !cfg.Review.FanOut {
		return review.DispatchPlan{}
	}
	botRoute := reviewBotAuthorRoute(cfg)
	state, err := review.LoadDispatchState("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review dispatch state unavailable; starting fresh", "error", err)
	}
	// classification.review_bots, read the same way installReviewBots reads
	// it for the thread reconciler (#7360): hive.yaml wins, hive-project.yaml
	// is the fallback. Feeds the reviewer's read step (#9360); empty/off
	// leaves ReviewBotLogins empty and the prompt section is skipped.
	reviewBots, rbErr := cfg.EffectiveReviewBots(os.Getenv("HIVE_PROJECT_YAML"))
	if rbErr != nil && logger != nil {
		logger.Warn("classification.review_bots: project file unreadable; reviewer will not see bot findings this pass", "error", rbErr)
	}
	artifact, err := review.LoadArtifact("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review verdict artifact unavailable for dispatch planning", "error", err)
	}
	prAgents := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	candidates := make([]github.PullRequest, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	seenPRs := map[string]bool{}
	labelGated := 0
	addCandidates := func(items []github.PullRequest) {
		for _, pr := range items {
			fullRepo := fullRepoName(pr.Repo, cfg.Project.Org)
			key := github.ReviewQueueKey(fullRepo, pr.Number)
			if fullRepo == "" || pr.Number <= 0 || seenPRs[key] {
				continue
			}
			seenPRs[key] = true
			// review.require_labels: an unlabelled PR is never dispatched
			// to the swarm. The relay refuses it independently.
			if !cfg.Review.AdmitsPRLabels(pr.Labels) {
				labelGated++
				continue
			}
			candidates = append(candidates, pr)
		}
	}
	addCandidates(actionable.PRs.Items)
	addCandidates(actionable.PRs.Held)
	if labelGated > 0 && logger != nil {
		logger.Info("review label gate withheld PRs from review dispatch",
			"withheld", labelGated, "candidates", len(candidates),
			"require_labels", strings.Join(cfg.Review.NormalizedRequireLabels(), ","))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, rj := candidates[i].ReviewRank, candidates[j].ReviewRank
		if ri > 0 && rj > 0 && ri != rj {
			return ri < rj
		}
		return ri > 0 && rj <= 0
	})

	scopeReader := reviewScopeLinearReader(cfg, logger)
	prs := make([]review.PullRequest, 0, len(candidates))
	for _, pr := range candidates {
		lane := classify.Classify(github.Issue{Title: pr.Title, Labels: pr.Labels}).Lane
		fullRepo := fullRepoName(pr.Repo, cfg.Project.Org)
		item := review.PullRequest{
			Repo:       pr.Repo,
			Number:     pr.Number,
			Title:      pr.Title,
			Author:     pr.Author,
			Labels:     append([]string(nil), pr.Labels...),
			HeadSHA:    pr.HeadSHA,
			UpdatedAt:  pr.UpdatedAt,
			BodyDigest: pr.BodyDigest,
			URL:        pr.URL,
			Lane:       string(lane),
			// Attribution trailer, carried for consumers and the audit trail.
			// Not a planner input: the fix gate decides on the login author
			// alone (#8421, #13124).
			HiveAttributed: pr.HiveAttributed,
			// The authoring model from the same trailer: what review_models
			// exclusions compare against, and stamped into the verdict as
			// author_model (hivecommons/hive#13243).
			AuthorModel: pr.HiveModel,
			// Grounding anchor for the review prompt. Repo access is the
			// measured active ingredient in review quality (17%→67% hit rate,
			// 61% fewer false positives), so the reviewer is told which commit
			// to read rather than being left to infer from the diff.
			MergeBase:   pr.BaseSHA,
			AuthorAgent: prAgents[fmt.Sprintf("%s#%d", fullRepo, pr.Number)],
			// The author's base over every configured lane, enabled or not,
			// so a disabled or retired authoring replica still excludes its
			// family from reviewing it (hivecommons/hive#13552).
			AuthorBase: reviewAuthorBase(cfg, prAgents[fmt.Sprintf("%s#%d", fullRepo, pr.Number)]),
			// Run trailers (#8310) and the plan they name, for plan_match
			// (#8317). Identifiers and planner output only: the PR body
			// itself still never reaches the prompt.
			RunKey:        pr.HiveRun,
			PlanRef:       pr.HivePlan,
			PlanWave:      planWaveFor(cfg, beadStores, pr.HivePlan, pr.HiveRun),
			ScopeContract: pr.ScopeContract,
			// A maintainer's changes_requested review or `/hive decision`
			// on this head (hivecommons/hive#13041): the planner kicks the
			// fix lane with it instead of reviewing the head again.
			HumanChangeRequest: review.HumanChangeRequestFor(review.DefaultHumanChangeRequests(), fullRepo, pr.Number, pr.HeadSHA),
			// The compare from the last judged head, for the delta
			// re-review and its trivial skip (review.delta_rereview).
			DeltaDiff:     reviewDeltaDiffFor(ctx, cfg, ghr, artifact, state, fullRepo, pr.Number, pr.HeadSHA, pr.BodyDigest),
			BaseDiffEmpty: pr.BaseDiffEmpty,
		}
		// The linked Linear issue's text for the scope contract, read here
		// so reviewers never need a Linear token (hivecommons/hive#14906);
		// only for a PR about to be reviewed at this head.
		if _, judged := artifact.AggregateForBody(fullRepo, pr.Number, pr.HeadSHA, pr.BodyDigest); !judged && state.PendingSince(fullRepo, pr.Number, pr.HeadSHA).IsZero() {
			reviewScopeContractFor(ctx, scopeReader, &item, logger)
		}
		// Changed paths for the bot-author model route's path exceptions
		// (review.bot_author_full_review_paths); fetched only for a listed
		// bot's PR that is about to be reviewed.
		item.ChangedFiles, item.ChangedFilesKnown = reviewBotRouteFilesFor(ctx, cfg, ghr, botRoute, artifact, state, fullRepo, pr.Number, pr.Author, pr.HeadSHA, pr.BodyDigest)
		prs = append(prs, item)
	}
	// PRs a review webhook just fired for go first, so the early eval cycle
	// it triggered spends its review budget on them (hivecommons/hive#11091).
	prs = prioritizeEventPRs(prs, activeReviewEvents.Load())
	agents := make([]review.AgentCapability, 0, len(cfg.Agents))
	for name, ac := range cfg.EnabledAgents() {
		cap := review.CapabilityFromConfig(name, ac)
		cap.Paused = cap.Paused || (agentMgr != nil && agentMgr.IsPaused(name))
		cap.Draining = reviewerAutoscaler.Draining(name)
		agents = append(agents, cap)
	}
	reviewBotLogins := []string{}
	if reviewBots.Enabled() {
		reviewBotLogins = reviewBots.Logins
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
		WritingGuideSection:   cfg.Project.WritingGuideSection(),
		FixHumanPRs:           cfg.Review.FixHumanPRsEnabled(),
		AcknowledgeNoFindings: cfg.Review.AcknowledgeNoFindings,
		ReviseRepos:           cfg.Review.ReviseRepos,
		ReviseVerdictsBefore:  parseReviseCutoff(cfg.Review.ReviseVerdictsBefore, logger),
		PendingTTL:            cfg.Review.PendingReviewTTL(),
		PendingFixTTL:         cfg.Review.PendingReviewFixTTL(),
		MaxRedispatch:         cfg.Review.EffectiveMaxRedispatch(),
		HeadMaxRetries:        cfg.Review.HeadMaxRetries,
		ProjectOrg:            cfg.Project.Org,
		AIAuthor:              cfg.EffectiveAIAuthor(),
		Agents:                agents,
		ReviewBotLogins:       reviewBotLogins,
		ReviewBotMinPriority:  reviewBots.MinPriority,
		PerspectiveModels:     cfg.Review.PerspectiveModels,
		BotAuthorRoute:        botRoute,
		DeltaRereview:         cfg.Review.DeltaRereviewEnabled(),
		DeltaTrivialLines:     cfg.Review.EffectiveDeltaTrivialLines(),
		DeltaMaxAge:           cfg.Review.EffectiveDeltaMaxAge(),
	})
	recordReviewDeltaRestamps(plan, logger)
	for _, k := range plan.ReviewKicks {
		if k.BotAuthorRouted {
			logger.Info("review routed bot-authored PR to bot_author_model", "repo", k.Repo, "pr", k.Number, "head", k.HeadSHA, "agent", k.Agent, "review_model", k.ReviewModel, "review_backend", k.ReviewBackend)
		}
	}
	for _, sk := range plan.SkippedReviewModels {
		logger.Info("review model fallback skipped perspective", "repo", sk.Repo, "pr", sk.Number, "head", sk.HeadSHA, "perspective", sk.Perspective, "agent", sk.Agent, "author_model", sk.AuthorModel)
	}
	expiredNow := map[string]struct{}{}
	for _, e := range plan.Expired {
		// Warn once per PR while it keeps expiring (#13687).
		key := prWarnKey(e.Repo, e.Number)
		expiredNow[key] = struct{}{}
		logger.Log(context.Background(), reviewExpiredWarn.level(key), "review pending expired", "repo", e.Repo, "pr", e.Number, "head", e.HeadSHA, "perspective", e.Perspective, "agent", e.Agent, "age", e.Age.Round(time.Second).String(), "expiries", e.Count)
	}
	reviewExpiredWarn.retain(expiredNow)
	for _, h := range plan.Exhausted {
		logger.Warn("review dispatch exhausted; holding PR for a human", "repo", h.Repo, "pr", h.Number, "head", h.HeadSHA, "reason", h.Reason)
	}
	for _, f := range plan.ForcedFixes {
		logger.Warn("review fix forced: changes_requested head starved by the revisit path", "repo", f.Repo, "pr", f.Number, "head", f.HeadSHA, "reason", f.Reason, "cycles", f.Cycles, "limit", review.RevisitStarvationCycles)
	}
	for _, w := range plan.Promoted {
		logger.Warn("review dispatch promoted: head starved of a reviewer walked ahead of the queue", "repo", w.Repo, "pr", w.Number, "head", w.HeadSHA, "reason", w.Reason, "cycles", w.Cycles, "since", w.FirstAt, "limit", review.ReviewWaitStarvationCycles)
	}
	for _, w := range plan.Waiting {
		logger.Info("review dispatch waiting: perspectives still owed, no reviewer this cycle", "repo", w.Repo, "pr", w.Number, "head", w.HeadSHA, "reason", w.Reason)
	}
	if len(plan.ReviewKicks)+len(plan.FixKicks)+len(plan.FixExclusions) > 0 {
		logger.Info("review swarm dispatch planned", "review_kicks", len(plan.ReviewKicks), "fix_kicks", len(plan.FixKicks), "fix_excluded", len(plan.FixExclusions), "fix_excluded_reasons", fixExclusionReasonSummary(plan.FixExclusions))
	}
	return plan
}

// fixExclusionReasonSummary renders the cycle's fix exclusions as
// "reason=count,..." (sorted) for the dispatch summary line, so fix_kicks: 0
// is never printed without saying why (hivecommons/hive#14665).
func fixExclusionReasonSummary(exclusions []review.FixDispatchExclusion) string {
	if len(exclusions) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, x := range exclusions {
		counts[x.Reason]++
	}
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", r, counts[r]))
	}
	return strings.Join(parts, ",")
}

// recordReviewDeltaRestamps writes the verdicts the planner carried forward
// onto a new head without a session (trivial delta, review.delta_rereview)
// into the verdict artifact, and logs each one. A write failure is logged,
// not fatal: the next cycle plans the same restamp again.
func recordReviewDeltaRestamps(plan review.DispatchPlan, logger *slog.Logger) {
	if len(plan.DeltaRestamps) == 0 {
		return
	}
	for _, r := range plan.DeltaRestamps {
		note := ""
		if r.Aggregate.Delta != nil {
			note = r.Aggregate.Delta.Note
		}
		logger.Info("review delta re-stamped verdict without a session", "repo", r.Repo, "pr", r.Number, "head", r.HeadSHA, "previous_head", r.PreviousHead, "verdict", r.Aggregate.Verdict, "note", note)
	}
	if _, err := review.RecordDeltaRestamps("", plan.DeltaRestamps, time.Now().UTC()); err != nil {
		logger.Warn("failed to record review delta re-stamps", "error", err, "count", len(plan.DeltaRestamps))
	}
}

// reviewPlannerGitHubFor returns the GitHub client as a reviewPlannerGitHub,
// or nil for a nil client: a typed nil in the interface would not read as nil.
func reviewPlannerGitHubFor(c *github.Client) reviewPlannerGitHub {
	if c == nil {
		return nil
	}
	return c
}

func refreshReviewVerdicts(cfg *config.Config, logger *slog.Logger) {
	if cfg == nil || !shouldRefreshReviewVerdicts(cfg) {
		return
	}
	artifact, err := review.CollectAndMerge("", "", review.AggregateOptions{
		// Unanimity is judged against what a PR was eligible to receive. Without
		// this the cap makes approve unreachable and every PR aggregates to
		// requires_human.
		MaxPerspectivesPerPR:       cfg.Review.MaxPerspectivesPerPR,
		Perspectives:               reviewPerspectiveSet(cfg, logger),
		SelfAuthoredDecisionPolicy: cfg.Review.EffectiveSelfAuthoredDecisionPolicy(),
		SensitivePaths:             cfg.Security.SensitivePaths,
		ModelStamps:                reviewModelStamps(logger),
		ExpectedHeads:              cfg.ExpectedReviewHeads(),
		MissingHeads:               reviewMissingHeads(),
	}, time.Now().UTC())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("failed to refresh review verdicts", "error", err)
		}
		return
	}
	logger.Info("review verdict artifact refreshed", "aggregates", len(artifact.Items))
}

// reviewModelStamps reads the dispatch state so a verdict whose report
// omitted author_model / review_model is stamped with what the planner knew
// when it dispatched that review (hivecommons/hive#13243). No state is no
// stamps, never an error: the artifact is still correct, just unstamped.
func reviewModelStamps(logger *slog.Logger) review.ModelStamps {
	state, err := review.LoadDispatchState("")
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("review dispatch state unavailable for model stamping", "error", err)
		}
		return nil
	}
	return review.ModelStampsFromState(state)
}

func shouldRefreshReviewVerdicts(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Review.RequireApproval {
		return true
	}
	entries, err := os.ReadDir(review.ReportDir(""))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, review.ReviewReportFilePrefix) && strings.HasSuffix(name, review.ReviewReportFileSuffix) {
			return true
		}
	}
	return false
}

// reviewOutcomeResolveCap bounds how many vanished PRs one eval cycle asks
// GitHub about. A hive pointed at a busy org can see dozens of PRs merge
// between cycles; the rest are resolved next cycle, and an unresolved PR
// reads as still open, never as merged.
const reviewOutcomeResolveCap = 40

// prStateGetter is the slice of the GitHub client the outcome ledger needs.
type prStateGetter interface {
	GetPRState(ctx context.Context, repo string, number int) (github.PRState, error)
}

// reviewEvidenceOutcomeRecorder is the optional slice of the GitHub client
// that stamps a resolved outcome onto the PR's evidence bundle, so a PR this
// hive reviewed but did not merge stops reading as "outcome unknown".
type reviewEvidenceOutcomeRecorder interface {
	RecordReviewEvidenceOutcome(repo string, number int, st github.PRState, now time.Time)
}

// recordReviewOutcomes folds this cycle's open-PR enumeration into the
// review-outcome ledger (pkg/review/outcomes.go): every open PR is upserted,
// review evidence from the verdict artifact and the posted-review ledger is
// attached, and PRs that left the list are resolved to merged/closed with one
// GET each. It is what lets the dashboard say whether reviewed PRs merge
// faster than unreviewed ones on THIS hive, rather than counting verdicts.
func recordReviewOutcomes(ctx context.Context, cfg *config.Config, gh prStateGetter, actionable *github.ActionableResult, logger *slog.Logger) {
	if cfg == nil || actionable == nil {
		return
	}
	recordReviewOutcomesAt(ctx, cfg, gh, actionable, "", time.Now().UTC(), logger)
}

func recordReviewOutcomesAt(ctx context.Context, cfg *config.Config, gh prStateGetter, actionable *github.ActionableResult, path string, now time.Time, logger *slog.Logger) {
	ledger, err := review.LoadOutcomeLedger(path)
	if err != nil {
		logger.Warn("review outcomes: ledger unreadable, starting fresh", "error", err)
	}
	aiAuthor := cfg.EffectiveAIAuthor()
	open := make([]review.OpenPR, 0, len(actionable.PRs.Items))
	for _, pr := range actionable.PRs.Items {
		full := config.QualifyRepo(cfg.Project.Org, pr.Repo)
		open = append(open, review.OpenPR{
			Repo:          full,
			Number:        pr.Number,
			Author:        pr.Author,
			AgentAuthored: pr.HiveAttributed || pr.AppAuthored || (aiAuthor != "" && strings.EqualFold(pr.Author, aiAuthor)),
			CreatedAt:     pr.CreatedAt,
		})
	}

	// Review evidence: the verdict artifact knows the verdict; the links
	// ledger knows a review was posted even when its verdict could not bind
	// (queue-lane reviews of undispatched PRs). Earliest time wins.
	signals := map[string]review.ReviewSignal{}
	if art, err := review.LoadArtifact(""); err == nil {
		for _, it := range art.Items {
			key := it.Repo + "#" + strconv.Itoa(it.Number)
			prev, ok := signals[key]
			if !ok || (!it.RecordedAt.IsZero() && it.RecordedAt.Before(prev.At)) {
				signals[key] = review.ReviewSignal{At: it.RecordedAt, Verdict: it.Verdict}
			} else if ok && prev.Verdict == "" {
				prev.Verdict = it.Verdict
				signals[key] = prev
			}
		}
	}
	if links, err := github.LoadReviewLinks(""); err == nil {
		for key, link := range links {
			at := link.At
			if at.IsZero() {
				continue
			}
			prev, ok := signals[key]
			if !ok {
				signals[key] = review.ReviewSignal{At: at}
			} else if at.Before(prev.At) {
				prev.At = at
				signals[key] = prev
			}
		}
	}

	missing := ledger.Observe(now, open, signals)
	resolved := 0
	if gh != nil {
		for i, m := range missing {
			if i >= reviewOutcomeResolveCap {
				break
			}
			st, err := gh.GetPRState(ctx, m.Repo, m.Number)
			if err != nil {
				logger.Debug("review outcomes: could not resolve vanished PR", "repo", m.Repo, "number", m.Number, "error", err)
				continue
			}
			ledger.Resolve(m.Repo, m.Number, st.State, st.MergedAt, st.ClosedAt)
			resolved++
			if rec, ok := gh.(reviewEvidenceOutcomeRecorder); ok {
				rec.RecordReviewEvidenceOutcome(m.Repo, m.Number, st, now)
			}
		}
	}
	ledger.Snapshot(now)
	if err := ledger.Save(path, now); err != nil {
		logger.Warn("review outcomes: save failed", "error", err)
		return
	}
	if len(missing) > 0 {
		logger.Info("review outcomes recorded", "open", len(open), "left_queue", len(missing), "resolved", resolved)
	}
}

// reviewFixAuditor is the slice of the dashboard the withheld-fix audit
// needs, so the function is testable without a live server.
type reviewFixAuditor interface {
	AuditLog(user, action, detail, agent string)
}

// auditWithheldReviewFixes puts every fix the planner refused this cycle on
// the audit trail (#8421). A refusal is silent by design — the PR just does
// not get a bot commit — and silence is the one thing an operator cannot
// investigate, so each one names the PR, who opened it, and the
// review.fix_human_prs value at the time (on no longer permits the push:
// hivecommons/hive#12897). The planner records the exclusion and
// the withheld fix only on the cycle that first withholds a head (a persisted
// State.Withheld entry suppresses later cycles), so this does not repeat on
// every eval cycle.
func auditWithheldReviewFixes(plan review.DispatchPlan, auditor reviewFixAuditor, logger *slog.Logger) {
	// One WARN per changes_requested head that ends the cycle without a fix
	// kick: a PR shown as "Fixing" that nobody is fixing must be loud
	// (hivecommons/hive#14665).
	for _, x := range plan.FixExclusions {
		level := slog.LevelWarn
		if x.Reason == review.FixDispatchExcludedNotHiveAuthored || x.Reason == review.FixDispatchExcludedNotHiveAuthoredDCO {
			// Policy skip (the hive never pushes to PRs it did not author), not a fault.
			level = slog.LevelInfo
		}
		logger.Log(context.Background(), level, "review fix dispatch excluded",
			"repo", x.Repo, "pr", x.Number, "head_sha", x.HeadSHA, "author", x.Author, "reason", x.Reason, "attempts", x.Attempts)
	}
	for _, w := range plan.WithheldFixes {
		logger.Info("review fix withheld: PR not hive-authored; review published, nothing pushed",
			"repo", w.Repo, "pr", w.Number, "head_sha", w.HeadSHA, "author", w.Author, "setting", w.Setting, "setting_enabled", w.SettingEnabled)
		if auditor == nil {
			continue
		}
		auditor.AuditLog("governor", "review_fix_withheld",
			fmt.Sprintf("pr=%s#%d head=%s author=%s setting=%s=%t reason=%s", w.Repo, w.Number, w.HeadSHA, w.Author, w.Setting, w.SettingEnabled, w.Reason), "")
	}
}

// reviewMissingHeads reads the review heads that ran out of retries while
// another head answered (hivecommons/hive#13542), so their perspectives count
// as settled for the decision step. No state is none.
func reviewMissingHeads() map[string][]string {
	state, err := review.LoadDispatchState("")
	if err != nil {
		return nil
	}
	return review.MissingHeadsFromState(state)
}

// reviewCommentPoster is the GitHub write postReviewAcknowledgements needs.
type reviewCommentPoster interface {
	CreateIssueCommentBudgeted(ctx context.Context, repo string, number int, body string) error
}

// postReviewAcknowledgements posts the one-line no-findings comment for each
// PR head the review-heads skip rule settled this cycle: every head approved
// every perspective with no findings, so no decision session ran to post it
// (hivecommons/hive#13542). The planner records each head once, so a failed
// post is logged rather than repeated.
func postReviewAcknowledgements(ctx context.Context, poster reviewCommentPoster, plan review.DispatchPlan, logger *slog.Logger) {
	if len(plan.Acknowledgements) == 0 || poster == nil {
		return
	}
	for _, a := range plan.Acknowledgements {
		if err := poster.CreateIssueCommentBudgeted(ctx, a.Repo, a.Number, a.Body); err != nil {
			logger.Warn("review heads: could not post the no-findings acknowledgement", "repo", a.Repo, "pr", a.Number, "head_sha", a.HeadSHA, "error", err)
			continue
		}
		logger.Info("review heads: posted the no-findings acknowledgement", "repo", a.Repo, "pr", a.Number, "head_sha", a.HeadSHA)
	}
}

func persistReviewDispatchState(plan review.DispatchPlan, delivered []review.DispatchKick, logger *slog.Logger) {
	writeReviewDispatchState(plan, func(planned []review.DispatchKick) review.DispatchState {
		return review.ConfirmDelivered(plan.State, planned, delivered)
	}, logger)
}

// persistReviewDispatchStateInProgress is the persist made while the kick pass
// is still delivering: it keeps the pending rows of planned kicks not yet
// confirmed, so a verdict from a kick still in flight binds to its dispatch
// (hivecommons/hive#15559). The end-of-pass persistReviewDispatchState drops
// the kicks that never landed.
func persistReviewDispatchStateInProgress(plan review.DispatchPlan, delivered []review.DispatchKick, logger *slog.Logger) {
	writeReviewDispatchState(plan, func([]review.DispatchKick) review.DispatchState {
		return review.ConfirmDeliveredInProgress(plan.State, delivered)
	}, logger)
}

func writeReviewDispatchState(plan review.DispatchPlan, confirm func(planned []review.DispatchKick) review.DispatchState, logger *slog.Logger) {
	planned := append(append([]review.DispatchKick(nil), plan.ReviewKicks...), plan.FixKicks...)
	if plan.State.GeneratedAt.IsZero() && len(planned) == 0 {
		return
	}
	state := confirm(planned)
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
		Mode:        "observe",
		Scope:       dashboard.NousScopeRepo,
		Phase:       "collecting",
		Status:      make(map[string]interface{}),
		Config:      make(map[string]interface{}),
		GovernorDir: governorDir,
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
		"baseline_pct":    dashboard.NousBaselinePct(snapshotCount),
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
		return slog.New(slogfeed.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(level)})))
	}

	lj := &lumberjack.Logger{
		Filename:   filepath.Join(dir, logFilename),
		MaxSize:    maxSizeMB,
		MaxAge:     maxAgeDays,
		MaxBackups: maxBackups,
		Compress:   compress,
	}

	tee := io.MultiWriter(os.Stdout, lj)
	// Same self-report tee as the boot logger: the file logger replaces it
	// as the default, so the feed must be installed here too.
	return slog.New(slogfeed.New(slog.NewJSONHandler(tee, &slog.HandlerOptions{Level: parseLogLevel(level)})))
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

	for name, ac := range cfg.Agents {
		if len(ac.LaneKeywords) > 0 {
			lanes = append(lanes, classify.LaneConfig{
				Name:     name,
				Keywords: ac.LaneKeywords,
			})
		}
		if len(ac.DetectKeywords) > 0 {
			detectKeywords[name] = ac.DetectKeywords
		}
		if ac.Emoji != "" || ac.Color != "" {
			discordIdentities[name] = discord.AgentIdentity{
				Emoji: ac.Emoji,
				Color: parseColorInt(ac.Color),
			}
		}
		sort.Slice(lanes, func(i, j int) bool { return lanes[i].Name < lanes[j].Name })
		for _, alias := range ac.Aliases {
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
	if len(discordAliases) > 0 {
		discord.SetAgentAliases(discordAliases)
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
	return inference.ResolveLiteLLMRoute(&cfg.Governor.AgentRuntimeGovernorConfig, backend, requestedModel)
}

func resolveWatsonxGateway(cfg *config.Config) *config.GatewayConfig {
	return inference.ResolveWatsonxGateway(&cfg.Governor.AgentRuntimeGovernorConfig)
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

// providerBudgetAlertID is the PROVIDER spend rebuff (#4294) system-alert ID,
// kept distinct from spokealerts.BudgetWarnAlertID/BudgetExhaustedAlertID so
// an operator can tell "we used our token allowance" from "the gateway will
// not spend more money".
const providerBudgetAlertID = "provider-budget-exceeded"

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
	case "agy-turn":
		// Run from inside a headless agy agent's pane, one per kick; see
		// pkg/agent/agy_turn.go.
		return true, cruxruntime.RunAgyTurn(args[1:], stdout, stderr)
	case jev.Subcommand:
		// Run from inside an agent's pane by the jev-decide skill; see
		// pkg/jev/cli.go. Talks only to the hive's loopback decision endpoint.
		return true, jev.Run(args[1:], os.Stdin, stdout, stderr)
	default:
		return false, 0
	}
}

func knowledgeContext7FromHive(c config.KnowledgeContext7) knowledge.Context7Config {
	var out knowledge.Context7Config
	for _, it := range c.AutoImport {
		out.AutoImport = append(out.AutoImport, knowledge.Context7AutoImport{ID: it.ID, Query: it.Query})
	}
	return out
}
