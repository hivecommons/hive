package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
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
	"github.com/hivecommons/hive/crux/trajectory"
	"github.com/hivecommons/hive/crux/watchdog"
	"github.com/hivecommons/hive/crux/watsonx"
	"github.com/hivecommons/hive/pkg/advisory"
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
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/worksource"
	"github.com/hivecommons/hive/pkg/worksource/wavefront" // registers the additive Wavefront source (#8362)
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
		Logger:                   b.logger,
		MergerAuthorizer:         trustedMergerFunc(b.cfg),
		HumanChangeRequests:      review.DefaultHumanChangeRequests(),
		TrustedAuthorizer:        trustedAuthorFunc(b.cfg),
		TrustedAuthorPolicy:      trustedAuthorPolicyFunc(b.cfg),
		ContributorLanePolicy:    contributorLanePolicyFunc(b.cfg),
		ContributorLaneVerdict:   contributorLaneVerdictFunc(b.cfg, b.logger),
		ContributorRegistryTier:  contributorRegistryTierFunc(),
		StageLabelPolicy:         stageLabelPolicyFunc(b.cfg),
		StageEngine:              stageEngineOptions(b.cfg, b.logger),
		SentinelLabel:            func() string { return b.cfg.Sentinel.LabelOrDefault() },
		HiveSupersedingAutoMerge: func() bool { return b.cfg.AutoMerge.HiveSuperseding.AutoMergeEnabled() },
		SweepTrigger:             b.sweepTrigger,
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
	autoMergeOpts.FixDispatchStatus = fixDispatchStatusReader()
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
	// Tell the dashboard the pod is going down BEFORE the stream drops
	// (#15788): an SSE "upgrading" event with phase=restarting and the
	// target the hub armed (when known), so the operator sees the upgrade
	// animation instead of a bare "reconnecting". Registered after the
	// contributor drain with addUrgent so it runs first — the frame is one
	// write on sockets that are already open, and the drain is what takes
	// the time. restartAnnounceSettle gives the SSE handler goroutines a
	// moment to flush before the hooks that follow race the grace period.
	b.preShutdownHooks.addUrgent("announce-restart", func() {
		b.dashSrv.AnnounceRestart(time.Now())
		<-time.After(restartAnnounceSettle)
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
			spoke.UpgradeAnnounceCallback(func(a *spoke.UpgradeAnnounce) {
				// The hub is about to roll (or has rolled) this pod itself
				// (#15788): show the upgrade animation before the kill.
				b.dashSrv.AnnounceUpgrade(*a, time.Now())
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
	b.noteBootPhase(bootPhaseFirstEval)
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
	loopNow := deps.now
	if loopNow == nil {
		loopNow = time.Now
	}
	b.evalCycles++
	deps.runEval(b, nil, evalSourceEnumerate)
	// lastEnumerateAt is when the last reconcile pass started. Wake-driven
	// cycles plan from the cached snapshot, and a busy spoke can be woken
	// every few minutes for hours; the reconcile cadence must not depend on
	// the ticker tick winning the select against those wakes
	// (hivecommons/hive#15779). reconcileOverdue is the one check both the
	// wake paths and the loop top consult.
	lastEnumerateAt := loopNow()
	reconcileOverdue := func(now time.Time) bool {
		return lastEvalInterval > 0 && now.Sub(lastEnumerateAt) >= lastEvalInterval
	}
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
		if now := loopNow(); source == evalSourceCached && reconcileOverdue(now) {
			b.logger.Info("cached snapshot older than eval interval; enumerating",
				"snapshot_age", now.Sub(lastEnumerateAt).Round(time.Second).String(),
				"eval_interval_s", int(lastEvalInterval.Seconds()))
			source = evalSourceEnumerate
		}
		if source == evalSourceEnumerate {
			lastEnumerateAt = loopNow()
		}
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
			if reconcileOverdue(now) {
				// The reconcile pass is owed; run the full tick instead of
				// the lean cached cycle so it is not starved by re-kicks.
				tick(evalSourceCached)
			} else {
				// Keep this cycle lean: planning and delivery without sweeps.
				b.cfgReloadMu.Lock()
				b.evalCycles++
				deps.runEval(b, nil, evalSourceCached)
				deps.persist(b)
				b.cfgReloadMu.Unlock()
			}
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
		// A reconcile pass owed for a full interval runs here regardless of
		// which channel woke the loop, so a ticker tick that was consumed,
		// dropped or outraced by wake channels cannot starve enumeration.
		if now := loopNow(); b.ctx.Err() == nil && reconcileOverdue(now) {
			b.logger.Info("eval interval elapsed without a reconcile pass; enumerating",
				"since_last_enumerate", now.Sub(lastEnumerateAt).Round(time.Second).String(),
				"eval_interval_s", int(lastEvalInterval.Seconds()))
			tick(evalSourceEnumerate)
			armContinuousWake()
			armWebhookHealthRecheck()
			// The cadence tick for this interval, if it is already pending,
			// would only repeat the pass that just ran.
			select {
			case <-ticker.Chan():
			default:
			}
		}
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
