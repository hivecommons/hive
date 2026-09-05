package hub

import (
	"context"
	cryptoRand "crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/auth"
	"github.com/hivecommons/hive/pkg/delegation"
	"github.com/hivecommons/hive/pkg/inferencehealth"
	"github.com/hivecommons/hive/pkg/openrouter"
	"github.com/hivecommons/hive/pkg/reach"
	"github.com/hivecommons/hive/pkg/tracing"
)

//go:embed static/*
var staticFS embed.FS

// registryPath is the DEFAULT on-disk hub registry file. A var (not a const)
// so tests can redirect it at a temp file before constructing a server;
// production keeps the default. NewHubServer copies it into the per-instance
// HubServer.registryPath at construction, and all production reads and writes
// go through that field — never this global — so a saveLoop goroutine leaked
// by one test's server can never race a later test reassigning this var.
var registryPath = "/data/hub-registry.json"

// hubBannersPath is the on-disk file where admin-sent banners are persisted so
// they survive hub restarts and upgrades (an in-place upgrade rolls the pod,
// which would otherwise wipe the in-memory map and silently drop every active
// banner). A var (not a const) so tests can redirect it at a temp file.
var hubBannersPath = "/data/saas/hub-banners.json"

// hubSecretPath is the on-disk file holding the hub's MASTER secret when
// HIVE_HUB_SECRET is unset: NewHubServer reads it, and generates + writes it
// when absent; provisionMasterSecret (hub_keys.go) reads it at provision time.
// A var (not a const) for the same reason as registryPath: NewHubServer
// touches this path UNCONDITIONALLY on construction, so every test that builds
// a real server would otherwise read — or worse, create — /data/saas on the
// host running the suite. newHubServerForTest points it into a per-test
// t.TempDir. Production never reassigns it.
var hubSecretPath = "/data/saas/hub-secret.key"

// hubBannerMaxAge bounds how long a persisted banner is honored after it was
// sent. On load, entries older than this are dropped so a long-forgotten banner
// does not resurrect across an upgrade months later. Admins still clear banners
// explicitly; this is only a backstop for stale on-disk state.
const hubBannerMaxAge = 30 * 24 * time.Hour

const (
	maxHeartbeatAge     = 5 * time.Minute
	staleRemoveAge      = 24 * time.Hour
	registrySaveDelay   = 5 * time.Second
	maxPayloadBytes     = 1 << 20 // 1MB
	staleUpgradeTimeout = 10 * time.Minute
)

type Registry struct {
	Hives     []RegistryEntry `json:"hives"`
	UpdatedAt string          `json:"updatedAt"`
	// FleetStatsLKG is the last fleet-stats aggregate that met the coverage
	// bar, persisted with the registry so it survives a hub restart. It is what
	// the landing page falls back to when coverage dips: a figure that is
	// honestly labelled stale beats a blank strip, because "no number" reads to
	// a visitor as "this fleet did nothing" rather than "we are recollecting".
	// Nil until the fleet has ever reported well enough to produce one — that
	// genuine first-boot state is the only case where the page shows no total.
	FleetStatsLKG *FleetStatsSnapshot `json:"fleetStatsLastKnownGood,omitempty"`
}

// FleetStatsSnapshot is a point-in-time fleet aggregate retained as the
// last-known-good figure. It stores the counts together with the coverage they
// were computed from, so the UI can say not just "as of 14:20" but "based on
// 12 of 18 hives" — staleness that describes itself rather than a bare number
// the viewer has to trust blindly.
type FleetStatsSnapshot struct {
	ReposManaged int `json:"repos_managed"`
	PRsMerged    int `json:"prs_merged"`
	PRsRejected  int `json:"prs_rejected"`
	CVEsClosed   int `json:"cves_closed"`
	Hives        int `json:"hives"`
	Reporting    int `json:"reporting"`
	Eligible     int `json:"eligible"`
	// ContributorsTotal carries the registered-contributor total (see
	// FleetStats.ContributorsTotal) into the last-known-good fallback so a
	// recollecting fleet still serves a stable figure instead of dropping to
	// 0 while coverage is below the publish threshold.
	ContributorsTotal int `json:"contributors_total"`
	// CollectedAt is when this aggregate was computed, i.e. what "as of" means
	// on the public page.
	CollectedAt time.Time `json:"collected_at"`
}

type RegistryEntry struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Org   string   `json:"org"`
	Repos []string `json:"repos"`
	// ProjectName is the hive's operator-editable display name, overlaid onto the
	// registry entry from the authoritative SaaSHive record (sh.ProjectName) on
	// the heartbeat path — NOT reported by the spoke. It is what the My Hives row
	// renders as the editable top line; when empty the client falls back to the
	// org/repo-derived label (see hiveLabel in the dashboard JS), exactly as
	// before this field existed. handleRenameHive is the only writer.
	ProjectName string `json:"projectName,omitempty"`
	// AIAuthor is the GitHub account this hive's agents open PRs as, as
	// reported by the spoke. The hub needs it to echo project config back
	// without blanking it, and it is the author the fleet-stats counts are
	// scoped to.
	AIAuthor string `json:"aiAuthor,omitempty"`
	// AIAuthorEffective is who the spoke's agents actually author PRs/commits
	// as — ai_author when set, else the GitHub App bot login ("<slug>[bot]").
	// Display-only in the spokes table; never echoed back into project config.
	AIAuthorEffective string `json:"aiAuthorEffective,omitempty"`
	// StartedAt is the spoke process start time, reported over the heartbeat.
	// Rendered as an uptime pill in My Hives so a crash-looping hive is visible.
	StartedAt string `json:"startedAt,omitempty"`
	// AdvisoryLastPostedAt is when the spoke last SUCCESSFULLY posted its
	// advisory digest (RFC3339), reported over the heartbeat. Empty means the
	// hive has never posted one — not-advisory-mode or an old spoke — which the
	// staleness gate reads as UNKNOWN, never as an alarm. My Hives renders a
	// "stale advisory" pill (advisoryStaleSummary) when this ages past
	// advisoryStaleThreshold on a hive whose App can write.
	AdvisoryLastPostedAt string `json:"advisoryLastPostedAt,omitempty"`
	// AdvisoryError is the log-safe error from the spoke's most recent failed
	// advisory-post attempt ("" on success). When set on an app-can-write hive
	// it trips the stale pill directly, carrying the specific cause.
	AdvisoryError string `json:"advisoryError,omitempty"`
	// AdvisoryFindingCount is the finding count in the spoke's last posted
	// digest; AdvisoryOverflowCount is how many further findings the top-N cap
	// withheld from it (0 = nothing capped, or an old spoke that does not report
	// it). My Hives renders the pair so a capped digest never reads as complete.
	AdvisoryFindingCount  int `json:"advisoryFindingCount,omitempty"`
	AdvisoryOverflowCount int `json:"advisoryOverflowCount,omitempty"`
	// InferenceAuthError is the log-safe cause set when the spoke's inference
	// backend has rejected several consecutive calls with 401 (a stale gateway
	// key). Empty = inference auth healthy / no inference backend / old spoke,
	// which the hub reads as no-signal. Non-empty raises the inference-auth
	// alert (AlertTypeInferenceAuthFailed) and, on an advisory-participating
	// hive, also trips advisory staleness immediately with this cause. Self-
	// clears when inference recovers. Never carries key material.
	InferenceAuthError string `json:"inferenceAuthError,omitempty"`
	// GatewayHealth is the spoke-reported set of currently failing inference gateways.
	GatewayHealth []inferencehealth.GatewayStatus `json:"gatewayHealth,omitempty"`
	// ProviderLimitReason is the spoke-reported provider spending/quota refusal
	// banner. It is separate from BudgetExhausted, which is hive-local governor
	// budget; this means the upstream provider is refusing token purchases.
	ProviderLimitReason  string `json:"providerLimitReason,omitempty"`
	ProviderLimitRebuffs int    `json:"providerLimitRebuffs,omitempty"`
	PrimaryRepo          string `json:"primaryRepo"`
	DashboardURL         string `json:"dashboardUrl"`
	// PublicURLSelfCheck is the spoke's own reachability verdict for
	// DashboardURL. It is intentionally separate from the hub-side probe:
	// private-network hives may be unreachable from the public hub while alive
	// from their own cluster. Nil means an old spoke did not report it.
	PublicURLSelfCheck *PublicURLSelfCheck `json:"publicUrlSelfCheck,omitempty"`
	// RouteExists is the spoke's in-cluster confirmation that an Ingress or
	// OpenShift Route exists for DashboardURL's host. Nil means an old spoke
	// did not report it; unknown means it could not verify (for example RBAC).
	RouteExists      *RouteExistenceCheck `json:"routeExists,omitempty"`
	SnapshotURL      string               `json:"snapshotUrl,omitempty"`
	ACMMLevel        int                  `json:"acmmLevel"`
	AgentCount       int                  `json:"agentCount"`
	GovernorMode     string               `json:"governorMode"`
	TotalTokens24h   int64                `json:"totalTokens24h"`
	ActionableIssues int                  `json:"actionableIssues"`
	ActionablePRs    int                  `json:"actionablePRs"`
	// WorkSource is the spoke's configured non-default work source type
	// ("github_projects", "linear", "jira"). Empty for GitHub Issues (the
	// default) — the dashboard only shows a badge when non-empty.
	WorkSource         string                `json:"workSource,omitempty"`
	ContributorCount   int                   `json:"contributorCount"`
	ActiveContributors int                   `json:"activeContributors"`
	Owner              string                `json:"owner,omitempty"`
	ClusterID          string                `json:"clusterId,omitempty"`
	ClusterName        string                `json:"clusterName,omitempty"`
	HiveType           string                `json:"hiveType,omitempty"`
	Lite               bool                  `json:"lite,omitempty"`
	LiteConfig         *LiteEnrollmentConfig `json:"liteConfig,omitempty"`
	// Namespace is the Kubernetes namespace this hive's spoke runs in. For a
	// hub-provisioned (hosted) hive it is hostedNamespaceForHive(sh) —
	// "hive-hosted-<id>" — overlaid from the authoritative SaaSHive record on
	// the heartbeat path (NOT reported by the spoke), so operators can
	// `kubectl -n <ns> exec …` straight from My Hives without re-grepping
	// `kubectl get ns` on a live cluster. It is derived, not renamed: a claimed
	// placeholder keeps its "hive-hosted-<placeholder-id>" namespace forever
	// (the ID never changes on claim — see hosted_namespace_identity.go), so
	// this stays correct across reassignment. Empty for a self-hosted/BYO hive
	// that does not run in a hub-provisioned namespace, or for a hive with no
	// SaaSHive record (old spoke) — never guessed for those.
	Namespace string `json:"namespace,omitempty"`
	// ProvStatus is the authoritative provisioning status copied from the hive's
	// SaaSHive record (sh.Status) on the heartbeat path — statusAvailable
	// ("available") for a genuinely-unclaimed pool placeholder, else a claimed
	// status ("claimed", "assigned", …) or "" for a hive with no SaaSHive record.
	//
	// It exists because a CLAIMED placeholder KEEPS its "hosted-available-" ID and
	// leftover pool org after assignment (the ID is minted at pool creation and
	// never changes on claim), so the ID/org prefix alone OVER-counts available
	// hives. computeFleetStats and isPlaceholderEntry both treat this field as
	// authoritative, with the prefix used only as a fallback when it is empty, so
	// the landing-page count and the dashboard's Assigned/Unassigned sections
	// agree on what "available" means. A scalar status — no PII.
	ProvStatus    string         `json:"provStatus,omitempty"`
	IsPublic      bool           `json:"isPublic"`
	RegisteredAt  string         `json:"registeredAt"`
	LastHeartbeat string         `json:"lastHeartbeat"`
	Health        map[string]any `json:"health"`
	Version       string         `json:"version"`
	GitHash       string         `json:"gitHash,omitempty"`
	GitBranch     string         `json:"gitBranch,omitempty"`
	// ImageRef is the container image this spoke's own Deployment runs, as
	// read in-cluster by the spoke and reported over the heartbeat. The hub
	// cannot see it any other way for firewalled/heartbeat-only spokes, and
	// without it there is no way to tell a hive pinned to an immutable
	// ghcr.io/hivecommons/hive:<sha> tag — which can never receive a rolling
	// upgrade — from one riding <branch>-latest. Empty on spokes that are not
	// in-cluster or predate this field; drift detection skips the signal
	// rather than guessing.
	ImageRef string `json:"imageRef,omitempty"`
	// GitHubHost is the bare hostname of the GitHub instance this hive targets
	// ("github.com", "github.ibm.com", …), rendered as a pill in the My Hives
	// Location column. Resolution order, most to least authoritative:
	//   1. the spoke's own reported runtime github.base_url (heartbeat), which
	//      is the only source that is right when a hive's GitHub differs from
	//      its cluster's default;
	//   2. the hive's recorded SaaSHive.GitHubHost / its cluster default, for
	//      spokes too old to report it;
	//   3. "github.com" at render time, so a chip is never blank.
	// Empty here means "not reported" — resolved on read, never guessed.
	GitHubHost       string             `json:"githubHost,omitempty"`
	Agents           []AgentSummary     `json:"agents,omitempty"`
	Leaderboard      []LeaderboardEntry `json:"leaderboard,omitempty"`
	Online           bool               `json:"online"`
	Upgrading        bool               `json:"upgrading,omitempty"`
	UpgradeTarget    string             `json:"upgradeTarget,omitempty"`
	UpgradeStartedAt time.Time          `json:"upgradeStartedAt,omitempty"`
	// UpgradeFailed records that the spoke reported an upgrade it could not
	// complete, with the cause. Distinct from Upgrading: "failed" is a terminal
	// state a human must see, not an in-flight one.
	UpgradeFailed   bool      `json:"upgradeFailed,omitempty"`
	UpgradeError    string    `json:"upgradeError,omitempty"`
	UpgradeFailedAt time.Time `json:"upgradeFailedAt,omitempty"`
	// OrphanedUpgradeSweeps counts how many times the orphan sweep has cleared
	// and re-armed an upgrade for this hive without it ever landing. A spoke
	// that cannot advance (it restarts, comes back on a SHA that is neither the
	// old one nor the target, and goes quiet again) would otherwise be swept and
	// re-armed forever, which looks like progress but never is. Past
	// maxOrphanedUpgradeSweeps the hub stops retrying and records a failure a
	// human can see. Reset whenever the hive reaches a target.
	OrphanedUpgradeSweeps    int          `json:"orphanedUpgradeSweeps,omitempty"`
	IssueHistory             []SparkPoint `json:"issueHistory,omitempty"`
	PRHistory                []SparkPoint `json:"prHistory,omitempty"`
	GitHubAppRequired        bool         `json:"githubAppRequired,omitempty"`
	GitHubAppPermIssue       string       `json:"githubAppPermIssue,omitempty"`
	GitHubAppState           string       `json:"githubAppState,omitempty"`
	GitHubAppTokenStatus     string       `json:"githubAppTokenStatus,omitempty"`
	GitHubAppTokenLastMintAt string       `json:"githubAppTokenLastMintAt,omitempty"`
	GitHubAppTokenError      string       `json:"githubAppTokenError,omitempty"`
	RepoTargetMisconfigured  bool         `json:"repoTargetMisconfigured,omitempty"`
	RepoTargetIssue          string       `json:"repoTargetIssue,omitempty"`
	// ConflictingReporters names two spoke instances that are BOTH reporting
	// as this hive (e.g. "hive-abc… ↔ hive-def…"), set when their beats
	// alternate. Non-empty is a critical drift signal: every field in this
	// entry is only as trustworthy as whichever instance reported last.
	ConflictingReporters string `json:"conflictingReporters,omitempty"`
	// StatusFlipping is true while this hive's reported App-auth state keeps
	// alternating between two values (status_flip.go) — the row cannot be
	// trusted because each beat tells a different story. Works for spokes too
	// old to report a Reporter; when both fire, duplicate-spoke names the
	// culprit pods.
	StatusFlipping bool `json:"statusFlipping,omitempty"`
	// GitHubAppID is the App ID the spoke reports it is authenticating AS.
	//
	// Carried into the registry so the hub can SEE a spoke running the
	// placeholder sentinel (config.PlaceholderAppID) without waiting for that
	// spoke to classify and report the fault itself. The spoke's own
	// classification is the better signal when it arrives, but it depends on the
	// spoke being new enough to produce it — and the sentinel's whole failure
	// mode is that nothing said anything for weeks. A raw, hub-observed number
	// cannot be silenced by a stale spoke.
	//
	// Zero means "not reported", never "no App".
	GitHubAppID int64 `json:"githubAppId,omitempty"`
	// GitHubAppSlug, GitHubInstallationID, GitHubAPIURL and GitHubBaseURL
	// complete the hub's picture of the identity the spoke is RUNNING.
	//
	// They are carried here for the same reason GitHubAppID is: a hub-observed
	// fact cannot be silenced by a stale or too-old spoke. Together they make a
	// half-applied identity detectable — a GHE app_id with an empty api_url
	// authenticates as nothing and 404s on every token request, and with only
	// app_id reported it looked exactly like a healthy delivery.
	//
	// Zero/empty means "not reported", never "not configured".
	GitHubAppSlug             string    `json:"githubAppSlug,omitempty"`
	GitHubInstallationID      int64     `json:"githubInstallationId,omitempty"`
	GitHubAPIURL              string    `json:"githubApiUrl,omitempty"`
	GitHubBaseURL             string    `json:"githubBaseUrl,omitempty"`
	PendingGitHubAppInstall   bool      `json:"pendingGitHubAppInstall,omitempty"`
	PendingGitHubAppInstallAt time.Time `json:"pendingGitHubAppInstallAt,omitempty"`
	// Fleet contribution counts reported by the spoke (nil = not reported /
	// not yet computed). Aggregated across public, non-stale hives into the
	// /api/fleet-stats total. Pointers preserve the nil-vs-zero distinction so
	// a hive that never reported is never counted as a zero.
	PRsMerged90d   *int `json:"prsMerged90d,omitempty"`
	PRsRejected90d *int `json:"prsRejected90d,omitempty"`
	CVEsClosed     *int `json:"cvesClosed,omitempty"`
	// FleetStatsCollectedAt is when the spoke last SUCCESSFULLY computed the
	// counts above — not when it last sent a heartbeat. The counts are carried
	// forward across restarts, so without this the hub cannot tell a count
	// gathered minutes ago from one frozen since a collector started failing.
	// The aggregator refuses to count contributions older than
	// fleetStatsMaxAge. Zero means the spoke is too old to report it, which is
	// treated as "not stale" so an upgrade does not blank the strip.
	FleetStatsCollectedAt time.Time `json:"fleetStatsCollectedAt,omitempty"`

	// RepoActivity is the spoke's audit-derived per-repo output summary
	// (hive-health), always the sanitized product of sanitizeRepoActivity,
	// never the raw payload. nil = the spoke is too old to report it ("no
	// data", never "zero output"); carried forward across beats that omit it,
	// like ComponentReach, so a restart or minimal upgrade-beat does not blank
	// the last real summary. RepoActivityCollectedAt / RepoActivityWindowHours
	// travel with it so the hub can age the summary and know the intended
	// freshness window (see health_verdict.go).
	RepoActivity                 []RepoActivityWire `json:"repoActivity,omitempty"`
	RepoActivityCollectedAt      time.Time          `json:"repoActivityCollectedAt,omitempty"`
	RepoActivityWindowHours      int                `json:"repoActivityWindowHours,omitempty"`
	RepoActivityCountWindowHours int                `json:"repoActivityCountWindowHours,omitempty"`

	// Quadrant signals reported by the spoke (nil = not reported). These back
	// the per-hive quadrant score; see quadrant.go for how each is used and
	// why absent evidence must never collapse to zero.
	//
	// BudgetCurrentSpend is tokens used in the CURRENT budget window, which is
	// only meaningful alongside the window bounds — a bare zero cannot be told
	// apart from a window that just rolled. The window length is the spoke's
	// governor.budget.period_days (default 7d), so it must be normalised before
	// being compared with the 90-day PR counters above.
	BudgetCurrentSpend   *int64    `json:"budgetCurrentSpend,omitempty"`
	BudgetLimit          *int64    `json:"budgetLimit,omitempty"`
	BudgetWindowStartsAt time.Time `json:"budgetWindowStartsAt,omitempty"`
	BudgetWindowEndsAt   time.Time `json:"budgetWindowEndsAt,omitempty"`
	BudgetExhausted      *bool     `json:"budgetExhausted,omitempty"`
	BudgetIgnored        *bool     `json:"budgetIgnored,omitempty"`
	// HoldTotal / AwaitingReview / SLAViolations measure work stalled on a
	// human or aging past its threshold; TasksCompleted7d is relay throughput
	// summed on the spoke from its hourly buckets.
	HoldTotal        *int `json:"holdTotal,omitempty"`
	AwaitingReview   *int `json:"awaitingReview,omitempty"`
	SLAViolations    *int `json:"slaViolations,omitempty"`
	TasksCompleted7d *int `json:"tasksCompleted7d,omitempty"`

	// Remediation-hint detectors (#5577), always the sanitized product of
	// sanitizeAgentErrorStreaks / sanitizeAgentNameList, never the raw
	// payload. nil = not measured (old spoke / collector not warm), carried
	// forward across beats that omit them like the quadrant signals above; a
	// MEASURED all-clear arrives as an empty (non-nil) value and overwrites,
	// so a recovered hive clears its own signal. See the matching
	// HeartbeatPayload fields for the semantics of each.
	AgentErrorStreaks map[string]int `json:"agentErrorStreaks,omitempty"`
	ConsentWedged     []string       `json:"consentWedged,omitempty"`
	NoCadenceAgents   []string       `json:"noCadenceAgents,omitempty"`

	// ComponentReach is the LATEST component-reach report from this hive's
	// spoke (#3993, phase 2a of #3973): per (component, running commit) span
	// counters aggregated in-process on the spoke and carried on the
	// heartbeat. Persisted with the registry so fleet reach survives hub
	// restarts. STORAGE ONLY at this phase — no endpoint, no PR mapping, no
	// ancestry join, no UI reads it until #3994; the JSON entry keys
	// (component / commit / spans_total / spans_error / first_seen /
	// last_seen) are 2b's fixed read interface. Always the sanitized/clipped
	// product of sanitizeComponentReach, never the raw payload. nil means the
	// spoke has never reported reach (old spoke, or no spans yet) — "no
	// data", never "zero reach". Carried forward across beats that omit it so
	// a spoke restart does not blank the last real report.
	ComponentReach *tracing.ReachReport `json:"component_reach,omitempty"`
	// AgentsWithModel is the spoke-reported count of agents that have a method
	// (backend) or model assigned. nil = the spoke is too old to report it, so
	// journey stage 2 is treated as unknown rather than unsatisfied.
	AgentsWithModel *int `json:"agentsWithModel,omitempty"`
	// Journey is the computed user-journey status (which adoption stage this
	// hive is stalled on, how hard it is being nudged, and whether stage 2 is
	// satisfied via ClankeR, the contributor relay). Computed on read from the journey
	// state store — never persisted on the registry entry itself.
	Journey *JourneyStatus `json:"journey,omitempty"`
}

type SparkPoint struct {
	T int64 `json:"t"`
	V int   `json:"v"`
}

// sparkWirePoints is how many history points /api/saas/my-hives ships per
// series. The registry keeps the full 7-day, 15-minute series (sparkMaxPoints
// = 672), but the dashboard renders it into a 50x14 px SVG polyline — at 672
// points that is ~13 points per horizontal pixel, so all but a handful are
// invisible. 48 points keeps the shape of the trend at roughly one point per
// pixel while cutting the two history arrays from ~755 KB to ~54 KB across a
// 42-hive fleet (they were 92% of an 818 KB payload).
const sparkWirePoints = 48

// downsampleSpark reduces a stored series to at most sparkWirePoints for the
// wire. It keeps the FIRST and LAST samples exactly (the sparkline's endpoints
// are what the eye anchors on, and the last point is the current value shown
// next to it) and picks evenly spaced samples in between. Series already at or
// under the cap are returned unchanged, so short histories keep full detail.
//
// Returning a fresh slice matters: the input aliases the registry's stored
// history, and callers marshal the result while other goroutines may append.
func downsampleSpark(points []SparkPoint, max int) []SparkPoint {
	if max <= 0 {
		return nil
	}
	if len(points) <= max {
		return points
	}
	if max == 1 {
		// A single-point cap has no "in between" to spread across, and the step
		// formula below divides by (max-1)==0 → +Inf, so int(Round(0*Inf)) is
		// math.MinInt64 and indexes out of range. Keep the first sample. (Ported
		// from v2 alongside its TestDownsampleSpark_MaxOne regression test.)
		return []SparkPoint{points[0]}
	}
	out := make([]SparkPoint, 0, max)
	// Spread max samples across the series, inclusive of both endpoints.
	step := float64(len(points)-1) / float64(max-1)
	for i := 0; i < max; i++ {
		idx := int(math.Round(float64(i) * step))
		if idx >= len(points) {
			idx = len(points) - 1
		}
		out = append(out, points[idx])
	}
	return out
}

var safeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func sanitizeField(s string) string {
	s = html.EscapeString(strings.TrimSpace(s))
	const maxSanitizedRunes = 200
	if runes := []rune(s); len(runes) > maxSanitizedRunes {
		s = string(runes[:maxSanitizedRunes])
	}
	return s
}

func isValidName(s string) bool {
	return safeNamePattern.MatchString(s) && len(s) <= 100
}

// publicGitHubHost is the bare hostname of public GitHub. A hive record stores
// "" for public GitHub, but a REQUEST must name its forge explicitly, and this
// is the label a requester types (or the picker submits) to mean "public".
const publicGitHubHost = "github.com"

// minForgeHostLabels is the fewest dot-separated labels a forge hostname can
// have. A GitHub forge is always at least "host.tld" (github.com,
// github.ibm.com); a single bare label like "github" is a typo, not a forge,
// and accepting it would provision a hive against a host that resolves nowhere.
const minForgeHostLabels = 2

// normalizeForgeHost turns whatever a user typed into a "GitHub forge" field
// into a bare hostname, and reports whether the result is a usable forge.
//
// It accepts every form the request modal advertises — with or without a
// scheme, with or without a trailing slash — because those are what people
// actually paste out of a browser address bar:
//
//	github.com            -> ("github.com",     true)
//	https://github.com    -> ("github.com",     true)
//	github.ibm.com        -> ("github.ibm.com", true)
//	https://github.ibm.com/ -> ("github.ibm.com", true)
//	""                    -> ("",               false)
//	"not a host"          -> ("not a host",     false)
//
// Normalization runs BEFORE validation deliberately. isValidName rejects ":"
// and "/", so validating the raw input would reject "https://github.ibm.com" —
// a form the request form explicitly tells users to use. Callers must
// normalize first and validate the result.
//
// githubHostLabel does the scheme/slash stripping and is reused here rather
// than duplicated, so the hostname a request records and the hostname the
// dashboard renders can never drift into two spellings.
func normalizeForgeHost(s string) (string, bool) {
	host := strings.ToLower(githubHostLabel(strings.TrimSpace(s)))
	// githubHostLabel maps empty -> "github.com". For a REQUEST that default is
	// wrong: an omitted forge must fail, not silently become public GitHub.
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	// A pasted org URL ("github.ibm.com/my-org") carries a path; keep only the
	// host so the forge field tolerates the same paste the org field does.
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	if !isValidName(host) {
		return host, false
	}
	// Every label must be non-empty and contain something other than dots, so
	// "..", "../..", and ".github.com" are rejected. isValidName permits dots
	// (legitimately — hostnames need them), which on its own would let a
	// path-traversal prefix like "../../etc/passwd" survive truncation at the
	// first slash and be stored as the forge "..".
	labels := strings.Split(host, ".")
	if len(labels) < minForgeHostLabels {
		return host, false
	}
	for _, label := range labels {
		if label == "" {
			return host, false
		}
	}
	return host, true
}

// normalizeOrgRef accepts what a user actually pastes into an "org" field and
// splits it into a GitHub host and a bare org name.
//
// The field is labelled "GitHub Organization", but pasting the org's URL is the
// obvious reading, and safeNamePattern rejects ":" and "/" — so a paste like
// "https://github.ibm.com/z-aiops-unite" failed with a bare "invalid org name"
// that named neither the problem nor the fix.
//
// Any GitHub Enterprise host is accepted (github.ibm.com, github.cisco.com, …),
// not a fixed allow-list. Returns (host, org); host is "" when the input was a
// bare org name, which callers should read as public github.com.
//
//	https://github.ibm.com/z-aiops-unite  -> ("github.ibm.com", "z-aiops-unite")
//	github.ibm.com/z-aiops-unite          -> ("github.ibm.com", "z-aiops-unite")
//	z-aiops-unite                         -> ("",               "z-aiops-unite")
//	https://github.ibm.com                -> ("github.ibm.com", "")
//	github.ibm.com                        -> ("github.ibm.com", "")
//
// The last two cases are the claim-path footgun this guards: a user who reads
// "GitHub Organization" and pastes ONLY the forge host (no org segment) must
// NOT have that hostname captured as the org. Before this, "github.ibm.com"
// with no path fell through to the bare-name return and became org
// "github.ibm.com" — the hive was then claimed against host-as-org, its vanity
// URL minted from <host>-<repo>, and agents targeted github.ibm.com/<repo>.
// Returning ("host", "") instead leaves the org empty, so the caller's
// isValidName("") check rejects the paste with the "use the org name or its
// URL" message rather than silently storing a broken project.
func normalizeOrgRef(s string) (host, org string) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	if s == "" {
		return "", ""
	}
	parts := strings.Split(s, "/")
	// A leading segment containing a dot is a hostname (github.com,
	// github.ibm.com). A bare org can also contain dots, so only treat the
	// first segment as a host when something follows it.
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		return parts[0], parts[1]
	}
	// A lone segment with no path that is itself a GitHub forge host is a host,
	// not an org — the user pasted the forge URL into the org field and left the
	// real org out. Recognise "github.<something>" (github.com, github.ibm.com,
	// github.cisco.com, …) so the hostname is never mistaken for the org. A
	// legitimate org that merely contains a dot ("my.org") does not start with
	// "github." and is untouched.
	if len(parts) == 1 && looksLikeGitHubForgeHost(parts[0]) {
		return parts[0], ""
	}
	return "", parts[0]
}

// normalizeProjectRef splits a GitHub project reference into forge host, org,
// and repo path segments. Public GitHub's ordinary "org/repo" shape is left as
// (host="", org="org", repos=["repo"]); a leading GitHub forge host is stripped,
// so "github.ibm.com/org/repo" becomes (host="github.ibm.com", org="org",
// repos=["repo"]). Dotted orgs such as "my.org/repo" remain public org/repo
// refs, matching normalizeOrgRef's host-vs-org rule.
func normalizeProjectRef(s string) (host, org string, repos []string) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return "", "", nil
	}
	rawParts := strings.Split(s, "/")
	parts := make([]string, 0, len(rawParts))
	for _, p := range rawParts {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "", "", nil
	}
	if looksLikeGitHubForgeHost(parts[0]) {
		host = parts[0]
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return host, "", nil
	}
	org = parts[0]
	if len(parts) > 1 {
		repos = append(repos, parts[1:]...)
	}
	return host, org, repos
}

func firstCSV(s string) string {
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			return p
		}
	}
	return ""
}

func replaceFirstCSV(s, first string) string {
	parts := strings.Split(s, ",")
	for i := range parts {
		if strings.TrimSpace(parts[i]) != "" {
			if strings.TrimSpace(first) == "" {
				parts = append(parts[:i], parts[i+1:]...)
			} else {
				parts[i] = first
			}
			return strings.Join(parts, ",")
		}
	}
	return strings.TrimSpace(first)
}

// looksLikeGitHubForgeHost reports whether a bare label is a GitHub forge
// hostname (github.com or a GitHub Enterprise host like github.ibm.com) rather
// than an org name. GitHub Enterprise hosts are conventionally "github.<org
// domain>", and the public host is "github.com"; both start with "github." and
// carry at least two dot-separated labels. This is deliberately narrow: it must
// never reclassify a real org that happens to contain a dot, so it keys on the
// "github." prefix that only a forge host carries. Case-insensitive.
func looksLikeGitHubForgeHost(label string) bool {
	l := strings.ToLower(strings.TrimSpace(label))
	if !strings.HasPrefix(l, "github.") {
		return false
	}
	// "github." alone (a trailing-dot typo) is not a host; require a non-empty
	// domain after the prefix, i.e. at least two labels total.
	return len(strings.Split(l, ".")) >= minForgeHostLabels && !strings.HasSuffix(l, ".")
}

// normalizeRepoRef strips a GitHub URL or "org/repo" prefix down to the bare
// repo name, so users can paste a repo URL into the repos field. Returns the
// repo name; the org is already carried separately.
//
//	https://github.ibm.com/z-aiops-unite/ui -> "ui"
//	z-aiops-unite/ui                        -> "ui"
//	ui                                      -> "ui"
func normalizeRepoRef(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	return parts[len(parts)-1]
}

// repoRefHost extracts the GitHub host a repo string was pasted with, or "" when
// none was given (a bare name or "owner/repo" with no host = "belongs to the
// spoke's host"). It parallels normalizeOrgRef's host detection: a leading
// dotted segment is the hostname.
//
//	https://github.ibm.com/z-aiops-unite/ui -> "github.ibm.com"
//	github.ibm.com/z-aiops-unite/ui         -> "github.ibm.com"
//	z-aiops-unite/ui                        -> ""   (no explicit host)
//	ui                                      -> ""
//
// This is what makes the single-host-per-spoke invariant enforceable: the
// caller compares every repo's host (and the primary's) against the spoke host
// and rejects any that differ, so a spoke can never mix github.com and a GHE
// instance. Hosts are compared case-insensitively; "github.com" and "" both
// mean public GitHub (see sameGitHubHost).
func repoRefHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		return parts[0]
	}
	return ""
}

// repoDisplayLine derives the human-readable "owner/repo" project line shown in
// My Hives (the name-cell second line) and in audit/timeline strings, from a
// hive's org + primary repo. It is the doubling-safe replacement for the naive
// org + "/" + primaryRepo join.
//
// Why a join is not enough: primaryRepo is stored VERBATIM and, for some hives,
// already carries a full "owner/repo" path rather than a bare repo name. Live
// fleet evidence: a GitHub Pages / GHE hive was recorded with
//
//	org         = "castrojo.github.io"   (a HOST wrongly parsed into the org field)
//	primaryRepo = "castrojo/endusers"    (already owner/repo)
//
// so org + "/" + primaryRepo rendered "castrojo.github.io/castrojo/endusers" —
// the host masquerading as an org, with the owner doubled. When primaryRepo
// already contains a slash it is a complete path and is returned as-is; only a
// bare repo name is qualified with the org. The result never contains a
// dangling slash: with one half known it returns that half alone, with neither
// it returns "".
//
//	repoDisplayLine("myorg", "repo")               -> "myorg/repo"
//	repoDisplayLine("castrojo.github.io", "castrojo/endusers") -> "castrojo/endusers"
//	repoDisplayLine("myorg", "")                   -> "myorg"
//	repoDisplayLine("", "owner/repo")              -> "owner/repo"
//	repoDisplayLine("", "")                        -> ""
//
// The dashboard JS hiveLabel() mirrors this logic; keep the two in sync.
func repoDisplayLine(org, primaryRepo string) string {
	org = strings.TrimSpace(org)
	primaryRepo = strings.TrimSpace(primaryRepo)
	if primaryRepo == "" {
		return org
	}
	// A primaryRepo that already carries an "owner/repo" path is complete;
	// prefixing the org would double the owner (the github.io/GHE defect).
	if strings.Contains(primaryRepo, "/") {
		return primaryRepo
	}
	if org == "" {
		return primaryRepo
	}
	return org + "/" + primaryRepo
}

// sameGitHubHost reports whether two host labels refer to the same GitHub. Both
// "" and "github.com" mean public GitHub, so they are equal; a GHE host
// ("github.ibm.com") equals only itself. Case-insensitive.
func sameGitHubHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || h == "github.com" {
			return "github.com"
		}
		return h
	}
	return norm(a) == norm(b)
}

// validateSingleRepoHost enforces the single-host-per-spoke invariant: every
// repo (and the primary repo) must live on the SAME GitHub host as the spoke.
// spokeHost is the host derived from the org/primary (normalizeOrgRef); "" means
// public github.com. repos is the raw, still-host-qualified list as the user
// pasted it (parse the host BEFORE normalizeRepoRef strips it). Returns a
// non-nil error naming the first offending repo, so the request/assign handler
// can 400 with a clear, onboarding-friendly message.
func validateSingleRepoHost(spokeHost, primaryRepo string, repos []string) error {
	label := func(h string) string {
		if h == "" || strings.EqualFold(h, "github.com") {
			return "github.com"
		}
		return h
	}
	for _, ref := range append(append([]string{}, repos...), primaryRepo) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		rh := repoRefHost(ref)
		if rh == "" {
			continue // no explicit host — belongs to the spoke host by definition
		}
		if !sameGitHubHost(rh, spokeHost) {
			return fmt.Errorf("repo %q is on %s but this hive is on %s — a hive's repos must all be on one GitHub host (github.com or a single GitHub Enterprise instance). Fix the mismatched repo or request a separate hive for it",
				ref, label(rh), label(spokeHost))
		}
	}
	return nil
}

// isValidRepoRef validates a repo entry, which may be a bare name ("repo")
// or a cross-org "owner/repo" reference. Both segments must be safe names
// (no path traversal); at most one slash is allowed. Cross-org repos are a
// supported hive config (e.g. a primary repo in one org plus a secondary in
// another), so the heartbeat must not 400-reject them — doing so silently
// took a whole hive offline (no heartbeats → no upgrades, shows offline).
func isValidRepoRef(s string) bool {
	if len(s) > 201 {
		return false
	}
	parts := strings.Split(s, "/")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !isValidName(p) {
			return false
		}
	}
	return true
}

func secureCompareHub(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// AUDIT F24 TOMBSTONE — heartbeatBearerOK WAS HERE AND IS DELETED.
// Do not reintroduce it, and do not "restore" it from history.
//
// It was a complete F2/N1 lane: it accepted the RAW fleet master (s.hubSecret)
// as a bearer, and the fleet-wide heartbeatKey() = HMAC(master,
// "hive-heartbeat-v1"). Neither comparand binds a hive identity, so any spoke
// presenting either token could claim ANY hive_id — the original N1 IDOR. It was
// the only place in the hub tree where the raw master was an authentication
// comparand.
//
// It had zero non-test callers when deleted, so removal is behaviour-preserving.
// It is removed rather than left dead because its tests ASSERTED THE VULNERABLE
// BEHAVIOUR ("heartbeatBearerOK must accept raw master secret"), which would
// have made a future refactor that re-wired it look verified-correct against a
// green suite. Those tests are INVERTED, not deleted, in
// server_handlers_coverage_test.go — they now assert the fleet-wide and
// raw-master bearers are REJECTED, so this cannot come back silently.
//
// The live, correct path is verifyHeartbeatBearer (hub_keys.go), which is
// per-hive-only: derivePerHiveKey(master_g, infoHeartbeatKey, hiveID) for every
// live generation, with no deriveDomainKey fleet-wide branch anywhere (F2).

// heartbeatHealthStaleness is the maximum age of heartbeat-reported health
// data before it is considered stale and displayed with a warning.
const heartbeatHealthStaleness = 5 * time.Minute

// HeartbeatHealthEntry stores heartbeat-reported cluster health along with
// the timestamp it was received, so the hub can detect staleness.
type HeartbeatHealthEntry struct {
	Report     *HeartbeatClusterHealthReport
	ReceivedAt time.Time
}

type HubServer struct {
	mux      *http.ServeMux
	registry Registry
	mu       sync.RWMutex
	logger   *slog.Logger
	saveCh   chan struct{}
	// saveLoopStop / saveLoopDone make the debounced saveLoop goroutine
	// joinable (#4774). A hub built by NewHubServer used to leak its saveLoop
	// forever: in tests, the loop could wake up to registrySaveDelay after the
	// test returned and recreate registry files inside a t.TempDir the test
	// framework was already removing — "TempDir RemoveAll cleanup: directory
	// not empty" flakes. StopSaveLoop closes saveLoopStop and blocks on
	// saveLoopDone, so a test that stops the server KNOWS no further registry
	// write can happen. Both are nil on bare &HubServer{} test literals, which
	// never start the loop; StopSaveLoop is a no-op for them.
	saveLoopStop     chan struct{}
	saveLoopDone     chan struct{}
	saveLoopStopOnce sync.Once

	// authProviders is the enabled human-login provider set (GitHub OAuth plus any
	// configured OIDC providers — Google/IBMid/Red Hat/Microsoft/custom). Built
	// once in registerOAuth from the environment; nil until then. The login picker
	// and the OIDC callback read it. It governs ONLY human dashboard login/access —
	// never the GitHub App token or agent logins.
	authProviders *auth.Registry
	// registryPath is this server's on-disk registry file, captured from the
	// package-level default at construction and immutable afterwards. It is a
	// per-instance field (not a use-time read of the global) so the saveLoop
	// goroutine — which outlives test servers, since nothing closes saveCh —
	// only ever sees its own path and cannot race a test redirecting the
	// global for the next server (the TestLoadRegistry -race failure).
	registryPath string
	hubGitHash   string
	hubGitBranch string
	hubSecret    string
	// keyGenerations is the ordered set of master generations this hub accepts
	// (hub_generations.go). Until a rotation happens it holds exactly ONE
	// generation whose secret IS hubSecret, so every derived key is
	// byte-identical to the single-master behavior and dual acceptance
	// degenerates to single acceptance.
	//
	// GUARDED BY keyGenerationsMu. The admin rotate endpoint replaces this
	// pointer while heartbeat and cookie verifiers are concurrently reading it,
	// so every read goes through currentGenerations() rather than touching the
	// field, and every write holds keyGenerationsMu for the duration of its
	// evaluate-persist-install sequence. The field itself is only ever REPLACED,
	// never mutated in place — generationSet.rotate is pure and returns a new
	// set — so a reader that grabbed the old pointer keeps a consistent,
	// immutable snapshot and simply verifies against the pre-rotation set for
	// the remainder of its request. That is correct, not a race: the outgoing
	// generation is still acceptable.
	keyGenerationsMu sync.RWMutex
	keyGenerations   *generationSet
	// lastKeyRotation is when the current generation was minted, used ONLY by
	// the double-rotation cooldown (evaluateRotation). It is never consulted to
	// decide whether a key is acceptable — VerifyUntil alone does that.
	lastKeyRotation time.Time
	// lastHubUpgradeTrigger debounces the hub self-upgrade rollout restart so the
	// every-cycle behind-latest check doesn't re-restart while a rollout is still
	// in flight. See the auto-upgrade block in the SHA-poll loop. It also marks
	// the START of an in-flight roll: for hubUpgradeDebounce after a trigger the
	// hub reports upgrade state "upgrading", so the dashboard shows "Upgrading"
	// (not a misleading "queued") while the new pod rolls — for BOTH the auto and
	// the admin-triggered path. hubUpgradeTarget is the SHA being rolled to.
	lastHubUpgradeTrigger time.Time
	hubUpgradeTarget      string
	// hubUpgradeFault holds a human-readable reason the last self-upgrade did
	// not land: a refused (malformed) image tag, or a rollout that never became
	// Ready. Empty means healthy. It exists so a stuck upgrade is visible in the
	// dashboard instead of only in `kubectl describe` — the `target1` incident
	// left the hub serving stale code for ~20 minutes with nothing surfaced.
	hubUpgradeFault string
	hubUpgradeMu    sync.Mutex // guards lastHubUpgradeTrigger + hubUpgradeTarget + hubUpgradeFault
	// Admin upgrade kill switch (upgrade_pause.go). Lazily loaded from its
	// durable JSON file on first use so the state survives the hub's own
	// frequent auto-rolls; guarded by its own mutex because it is consulted on
	// every heartbeat and every upgrade-poll cycle, never under s.mu.
	upgradePause         UpgradePauseState
	upgradePauseLoaded   bool
	upgradePauseMu       sync.Mutex // guards upgradePause + upgradePauseLoaded
	upgradePauseObserver func(UpgradePauseEvent)
	httpServer           *http.Server
	httpMu               sync.Mutex // guards httpServer (Start runs in a goroutine; Shutdown races it)
	clusters             map[string]ClusterConfig

	// vanityHostServable overrides how the retroactive vanity-URL repair makes a
	// vanity host servable (see makeVanityHostServable). nil in production, where
	// the real addVanityHostToIngress runs; set by tests, which have no cluster to
	// kubectl to.
	vanityHostServable func(hiveID, vanityHost string, cluster *ClusterConfig) error
	// vanityRepairInFlight tracks hive IDs whose retroactive vanity-URL repair
	// is currently running in the background (kickVanityURLRepairAsync), so the
	// heartbeat path starts at most one per hive. On an unreachable cluster a
	// single repair attempt spends minutes in kubectl dial timeouts while beats
	// keep arriving every ~2 min; without this guard the attempts pile up
	// goroutines all hanging against the same dead cluster.
	vanityRepairInFlight sync.Map // hive ID → struct{}
	// vanityMintTimes are the timestamps of vanity-host MINTS performed by the
	// retroactive repair path within the current vanityMintWindow — the
	// fleet-wide budget guarding the registered domain's shared ACME quota
	// (#5923: an unbounded re-mint storm exhausted Let's Encrypt's
	// 50-certs/168h cap in about an hour). Persisted to the SaaS data directory
	// so a hub restart cannot forget the registered domain's still-live 168h
	// ACME debt and reopen the storm. Guarded by vanityMintMu, never s.mu.
	vanityMintTimes        []time.Time
	vanityMintLedgerLoaded bool
	vanityMintMu           sync.Mutex
	// claimWorkInFlight tracks hive IDs whose claim-time cluster work
	// (namespace identity stamp + vanity mint) is currently running in the
	// background (kickClaimClusterWorkAsync), so assign/approve start at most
	// one work-set per hive. Same rationale as vanityRepairInFlight above: a
	// single attempt against an unreachable cluster spends minutes in kubectl
	// dial timeouts, and stacking attempts piles up goroutines for no benefit.
	claimWorkInFlight sync.Map                         // hive ID → struct{}
	heartbeatHealth   map[string]*HeartbeatHealthEntry // cluster ID → latest health from spoke heartbeat
	heartbeatHealthMu sync.RWMutex

	// liveHiveUsers maps a GitHub username → the last time any hive reported it as
	// having a live dashboard session. It powers the "logged into their hive right
	// now" avatar treatment (a green dashed border). In-memory only and rebuilt
	// from heartbeats — a hub restart simply re-learns it within a beat or two.
	// Freshness-gated on read (liveHiveUsernames), so a user whose hive stopped
	// beating drops out on its own rather than staying "live" forever.
	liveHiveUsers   map[string]time.Time
	liveHiveUsersMu sync.RWMutex
	// engagedHiveUsers is the same shape as liveHiveUsers but for ENGAGED
	// presence: username → last time any hive reported the user's browser as
	// focused with recent input (heartbeat EngagedSessionUsers). The gap
	// between the two sets is exactly the idle-open-tab crowd. Guarded by
	// liveHiveUsersMu (always touched on the same paths), freshness-gated on
	// read like liveHiveUsers. Old spokes never report engaged users, so this
	// map simply stays empty for them — optional data, never an error.
	engagedHiveUsers map[string]time.Time

	// usageHistory is the sampled fleet-total token trend, appended on
	// heartbeat at usageSnapshotInterval and bounded to
	// usageSnapshotMaxPoints (see usage.go). Guarded by usageMu rather than
	// s.mu: the heartbeat path already holds s.mu when it samples, and
	// re-locking a held non-reentrant mutex would deadlock the hub.
	usageHistory []UsageSnapshot
	usageMu      sync.RWMutex

	// heartbeatUpgrade tracks hives that should be upgraded via heartbeat
	// UpgradeTo because kubectl rollout restart failed (cluster unreachable).
	// Key: hive ID, value: target SHA.  Cleared when the spoke reports the
	// target SHA, proving the upgrade completed.
	heartbeatUpgrade map[string]string

	// uncollectibleUpgradeNoted de-duplicates the timeline entry written when
	// the hub declines to arm an upgrade a hive cannot collect. Key: hive ID,
	// value: the target the refusal was last reported for, so a genuinely new
	// upgrade opportunity is reported again while the same refusal is not
	// repeated every 2-minute poll. See pullonly_upgrade.go.
	//
	// PER-SERVER, not a package global: two hubs in one process would otherwise
	// share and clobber this, letting one server's arming suppress a refusal the
	// other should have reported. Guarded by its own mutex rather than s.mu
	// because noteUncollectibleUpgrade is called from paths that do not hold
	// s.mu and goes on to call recordTimeline.
	uncollectibleUpgradeMu    sync.Mutex
	uncollectibleUpgradeNoted map[string]string

	// clusterUnreachableUntil suppresses kubectl against clusters the hub has
	// just proven it cannot route to (firewalled GPU clusters like the heartbeat-only cluster).
	// Without this, triggerAutoUpgrades() paid a full dial timeout PER HIVE PER
	// CYCLE — 15 heartbeat-only-cluster hives serialized into ~30 minutes of blocking, which
	// starved the hub's own auto-upgrade check at the end of the same loop and
	// left the hub pinned to an old SHA indefinitely. Key: cluster ID, value:
	// the time after which kubectl may be retried.
	clusterUnreachableUntil map[string]time.Time
	clusterUnreachableMu    sync.Mutex
	// lastNetAdminReconcile throttles the NET_ADMIN securityContext-drift
	// reconcile (netadmin_reconcile.go). The SHA poller ticks every 2 min, but
	// the drift is static remediation, not a hot path, so we run the sweep at
	// most once per netAdminReconcileInterval. Guarded by clusterUnreachableMu
	// (both are poller-loop-only state; no need for a separate mutex).
	lastNetAdminReconcile time.Time
	// lastPerHiveEnvReconcile throttles the per-hive security env reconcile
	// (perhive_env_reconcile.go), which ensures HIVE_HEARTBEAT_KEY /
	// HIVE_TERMINAL_KEY / HIVE_SESSION_KEY / HIVE_SSO_PUBLIC_KEY /
	// HIVE_SESSION_PUBLIC_KEY are present and correct on every hosted spoke.
	// Same rationale and same guarding mutex as lastNetAdminReconcile above:
	// both are poller-loop-only state.
	lastPerHiveEnvReconcile time.Time
	// lastOrphanedPodReap throttles the orphaned Terminating-pod reaper
	// (orphaned_pod_reaper.go), which force-deletes hive-namespace pods left
	// behind when a node disappears without draining (#5328). Same rationale
	// and same guarding mutex as the two above: poller-loop-only state.
	lastOrphanedPodReap time.Time
	// lastGenerationRetire throttles the expired-master-generation retirement
	// sweep (hub_generations_retire.go). Same rationale and same guarding mutex
	// as the two above: poller-loop-only state.
	//
	// NOTE this throttles CLEANUP AND ALERTING ONLY. A generation stops being
	// ACCEPTED at its VerifyUntil via acceptableGenerations, on the wall clock,
	// whether or not this sweep ever runs — so a missed tick delays rewriting
	// hub-generations.json, never extends the acceptance window.
	lastGenerationRetire time.Time
	// lastAccessExpirySweep throttles the expired-access persistence sweep
	// (access_expiry.go). Same rationale and same guarding mutex as the sweeps
	// above: poller-loop-only state.
	//
	// NOTE this throttles PERSISTENCE AND THE TIMELINE EVENT ONLY. An expired
	// grant stops being honored at read time via loadSaaSUser's prune, on the
	// wall clock, whether or not this sweep ever runs.
	lastAccessExpirySweep time.Time
	// lastOrphanedUpgradeSweep throttles the orphaned-upgrade latch sweep
	// (stale_upgrade.go). Same rationale and same guarding mutex as the sweeps
	// above: poller-loop-only state.
	lastOrphanedUpgradeSweep time.Time
	// lastStuckAssignmentSweep throttles the stuck-assignment reset sweep
	// (saas_reset_assignment.go). Same rationale and same guarding mutex as the
	// sweeps above: poller-loop-only state.
	lastStuckAssignmentSweep time.Time
	// lastPoolReplenish throttles the watermark pool replenisher
	// (pool_replenisher.go); poolReplenishHold suspends a cluster's
	// replenishing until the recorded time after a failed seed. Same
	// poller-loop-only guarding mutex as the sweeps above.
	lastPoolReplenish time.Time
	poolReplenishHold map[string]time.Time
	// perHiveEnvSeen is the Deployment-SOURCED convergence view backing
	// PerHiveEnvSnapshot: hive ID → what the hub last actually read off that
	// hive's Deployment. Deliberately NOT derived from heartbeat recency (see
	// the long note on PerHiveEnvSnapshot) so a paused spoke cannot drop out of
	// the denominator and make the fleet read converged while it is not.
	perHiveEnvSeen map[string]perHiveEnvObservation
	// perHiveEnvConsidered / perHiveEnvSkippedByStatus record the LAST sweep's
	// hive-selection split: how many hives the status filter admitted versus
	// rejected. Published on the readiness surface so "the sweep is selecting
	// nobody" is visible rather than silent — the failure this lane shipped
	// with, where every downstream counter sat at zero and read exactly like a
	// converged fleet. Guarded by perHiveEnvMu with perHiveEnvSeen.
	perHiveEnvConsidered      int
	perHiveEnvSkippedByStatus int
	// perHiveEnvUnreachable / perHiveEnvUnreachableClusters record the LAST
	// sweep's hives that were admitted by status but could not be READ at all —
	// pull-only cluster, no registry entry, or inside the unreachable-cluster
	// breaker window. Without these the sweep's skip paths left no trace, so an
	// unreachable spoke was indistinguishable from one that does not exist and
	// the surface reported a partially-observed fleet as converged. Guarded by
	// perHiveEnvMu with perHiveEnvSeen.
	perHiveEnvUnreachable         int
	perHiveEnvUnreachableClusters []string
	perHiveEnvMu                  sync.RWMutex
	// reporterSeen tracks which spoke instance (payload.Reporter, the pod
	// name) last reported as each hive, to catch two instances alternating
	// under one hive_id. Guarded by reporterMu, not s.mu — it is touched on
	// every beat and must never contend with the registry lock.
	reporterSeen map[string]*reporterFlipState
	reporterMu   sync.Mutex
	// statusFlipSeen tracks each hive's reported App-auth state to catch
	// oscillation (status_flip.go). Same shape as reporterSeen, separate
	// mutex for the same reason: touched on every beat.
	statusFlipSeen map[string]*reporterFlipState
	statusFlipMu   sync.Mutex
	// appKeyDelivery tracks, per hive, consecutive GitHub App key deliveries
	// that the spoke never reflected, so a broken reporter cannot pull key
	// material onto the wire every beat forever (app_key_backoff.go, #2496).
	// Own mutex for the reporterSeen reason: touched on every beat.
	appKeyDelivery   map[string]*appKeyDeliveryState
	appKeyDeliveryMu sync.Mutex
	// heartbeatSwitchTag tracks hives that should switch their deployment
	// image to a specific tag (branch switch) via heartbeat, for clusters
	// the hub can't reach over kubectl. Cleared once the spoke reports it.
	heartbeatSwitchTag map[string]string

	// pendingWebhooks stores GitHub App installation webhooks that arrived
	// before the matching spoke heartbeat.  Key: lowercase org name.
	pendingWebhooks   map[string]*pendingWebhookEntry
	pendingWebhooksMu sync.Mutex

	// pendingGitHubAppConfigs stores GitHub App config to deliver to spokes
	// via heartbeat response (bypasses OAuth proxies). Key: hive ID.
	pendingGitHubAppConfigs   map[string]*HeartbeatGitHubAppConfig
	pendingGitHubAppConfigsMu sync.Mutex

	// hubBanners stores admin-sent banners to deliver to spokes via
	// heartbeat response. Key: hive ID → banner entry.
	hubBanners   map[string]*HubBannerEntry
	hubBannersMu sync.RWMutex

	// spokeProxyAuthCache memoizes each hive's dashboard token (the shared
	// secret the spoke holds as DASHBOARD_AUTH_TOKEN) so the auth-check
	// subrequest — which fires on EVERY hub-proxied request — does not pay a
	// kubectl exec per call. Key: hive ID → cached token + expiry. Guarded by
	// spokeProxyAuthMu. See handleSaaSAuthCheck / spokeProxyAuthToken.
	spokeProxyAuthCache map[string]spokeProxyAuthEntry
	spokeProxyAuthMu    sync.Mutex

	// authRolloutSeen records, per hive, which credential FORMAT that spoke last
	// authenticated with — the readiness signal for #3234.
	//
	// N1 and N2 shipped verify-both/mint-new compatibility lanes so the fleet
	// could roll without a flag day. Those lanes ARE the vulnerabilities they
	// replaced (a fleet-wide bearer does not bind identity; a symmetric session
	// key that verifies can also mint), so they must be deleted — but deleting
	// them before every spoke has re-provisioned 401s the whole fleet.
	//
	// Without this, "has the fleet rolled?" is unanswerable except by inspecting
	// every Deployment by hand. Key: hive ID → last-seen auth path. Guarded by
	// authRolloutMu.
	authRolloutSeen map[string]authRolloutEntry
	authRolloutMu   sync.RWMutex

	// pendingGateways stores OpenRouter gateways funded via the hub's
	// scan-to-fund flow, to deliver to firewalled/heartbeat-only spokes via the
	// heartbeat response. Key: hive ID → gateway (carries the secret key VALUE,
	// so it is drained on delivery, never re-sent). In-memory only — an
	// undelivered fund is retried by the sponsor, not persisted across restarts.
	pendingGateways   map[string]*HeartbeatGatewayConfig
	pendingGatewaysMu sync.Mutex

	// openRouterState holds in-progress hub-side OpenRouter PKCE flows
	// (single-use state → verifier/hive/model). Lazily initialized.
	openRouterStateOnce  sync.Once
	openRouterStateStore *openrouter.StateStore

	// timeline is the append-only per-hive activity event store. It records
	// state TRANSITIONS observed on the heartbeat (version, health, upgrade,
	// restart, …) plus human-initiated actions, so "why is hive X stuck?" is
	// answerable from the hub instead of `kubectl logs`. It carries its own
	// leaf mutex and is never guarded by s.mu. See timeline.go.
	timeline *timelineStore

	// journey holds per-hive user-journey nudge state: which banner each owner
	// was last sent and when, plus admin snoozes. Persisted to the PVC.
	journey *journeyStore

	// alerts holds the fleet-alert observation memory (restart history,
	// per-condition first-seen timestamps, admin acknowledgements). It carries
	// its own leaf mutex and is never touched while s.mu is held — see
	// alerts.go.
	alerts *alertState

	// revokedSessions holds session IDs invalidated before their signed expiry
	// (audit F10 — logout used to only delete the browser's copy). Persisted to
	// the PVC because the hub restarts often and an in-memory-only set would
	// un-revoke everything on the next roll. Carries its own leaf mutex and is
	// never touched while s.mu is held — see hub_session_revocation.go.
	revokedSessions *revokedSessions

	// revokedSave* coalesce the revocation set's disk writes (audit F15) without
	// giving up durability: concurrent revocations collapse into one full-file
	// rewrite instead of one each, but every revocation is still on disk before
	// its call returns — a deferred-only flush would un-revoke sessions across a
	// hub roll, which is the F10 bug. Leaf mutex like the store's own, never
	// touched while s.mu is held — see flushRevokedSessions.
	revokedSaveMu    sync.Mutex
	revokedSaveDirty bool

	// urlHealth remembers how many consecutive times each hive's PUBLIC
	// dashboard URL failed to serve, so a transient blip can be told apart from
	// a link that has been dead for hours. Populated by the auth-audit loop,
	// which already probes those URLs. Carries its own leaf mutex and is never
	// touched while s.mu is held — see url_reachability.go.
	urlHealth *urlHealthState

	// githubInstallationAccount resolves a GitHub App installation ID to its
	// owning account login. Nil means use the fleet App key. Tests replace it
	// with a fake API.
	githubInstallationAccount func(context.Context, int64) (string, error)

	// reachReporter / reachPRSource / reachAncestry are the three injected
	// dependencies of the read-only /api/reach endpoint (#3994, phase 2b of
	// #3973). The reporter defaults to reach.StubReachReporter until phase
	// 2a's heartbeat-fed store lands (its wiring is a one-line
	// SetReachReporter call); the PR source is wired from runHub when GitHub
	// credentials exist and the handler answers 503 while it is nil; the
	// ancestry resolver defaults to the compare-API adapter over the same
	// cache commit_order.go uses. Set before Start, read-only afterwards —
	// see reach_api.go.
	reachReporter reach.ReachReporter
	reachPRSource reach.PRSource
	reachAncestry reach.Ancestry

	// reachHistory is the fleet-level error-rate retention (#3995, phase 2c):
	// hourly per-component activity buckets folded in on heartbeat receive,
	// persisted to reachHistoryPath, read by /api/reach for before/after
	// deltas. nil on bare test servers; every touch point is nil-safe.
	reachHistory *reachHistoryStore

	// dibsPublic caches "is this repo public on github.com?" verdicts for the
	// dibs repo feed (#4233, dibs_public_check.go). Created lazily by
	// dibsChecker on the first /api/saas/dibs/repos request; tests pre-set the
	// field to point at a fake GitHub API before that first call.
	dibsPublicOnce sync.Once
	dibsPublic     *dibsPublicChecker
}

// HubBannerEntry stores an admin banner targeted at a specific hive.
type HubBannerEntry struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Color   string `json:"color"`
	SentAt  string `json:"sent_at"`
}

// saveHubBanners persists the in-memory banner map to the PVC so banners survive
// hub restarts and upgrades. Caller must NOT hold hubBannersMu — this takes an
// RLock itself. Writes atomically (tmp + rename) so a crash mid-write never
// leaves a half-written file. A persistence failure is logged, not fatal: a lost
// banner is a soft failure, and the admin can resend.
func (s *HubServer) saveHubBanners() {
	s.hubBannersMu.RLock()
	snapshot := make(map[string]*HubBannerEntry, len(s.hubBanners))
	for id, entry := range s.hubBanners {
		snapshot[id] = entry
	}
	s.hubBannersMu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		s.logger.Error("failed to marshal hub banners for persistence", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(hubBannersPath), 0o755); err != nil {
		s.logger.Error("failed to create hub banners dir", "error", err)
		return
	}
	tmpPath := hubBannersPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		s.logger.Error("failed to write hub banners tmp file", "error", err)
		return
	}
	if err := os.Rename(tmpPath, hubBannersPath); err != nil {
		s.logger.Error("failed to rename hub banners file", "error", err)
	}
}

// loadHubBanners reads persisted banners from the PVC into the in-memory map on
// startup. Entries older than hubBannerMaxAge (by SentAt) are dropped so stale
// banners do not resurrect long after they were relevant; if any are dropped the
// pruned set is written back. A missing file is normal (no banners yet).
func (s *HubServer) loadHubBanners() {
	data, err := os.ReadFile(hubBannersPath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Error("failed to read persisted hub banners", "error", err)
		}
		return
	}
	var stored map[string]*HubBannerEntry
	if err := json.Unmarshal(data, &stored); err != nil {
		s.logger.Error("failed to parse persisted hub banners", "error", err)
		return
	}

	pruned := false
	s.hubBannersMu.Lock()
	for id, entry := range stored {
		if entry == nil {
			pruned = true
			continue
		}
		if sent, perr := time.Parse(time.RFC3339, entry.SentAt); perr == nil {
			if time.Since(sent) > hubBannerMaxAge {
				pruned = true
				continue // drop stale banner
			}
		}
		s.hubBanners[id] = entry
	}
	loaded := len(s.hubBanners)
	s.hubBannersMu.Unlock()

	if loaded > 0 {
		s.logger.Info("loaded persisted hub banners", "count", loaded)
	}
	if pruned {
		// Rewrite the file without the dropped/stale entries.
		s.saveHubBanners()
	}
}

func NewHubServer(port int, logger *slog.Logger, gitHash, gitBranch string) *HubServer {
	if gitBranch == "" || gitBranch == "unknown" {
		gitBranch = "v2"
	}
	secret := os.Getenv("HIVE_HUB_SECRET")
	if secret == "" {
		if data, err := os.ReadFile(hubSecretPath); err == nil {
			secret = strings.TrimSpace(string(data))
		}
	}
	if secret == "" {
		const secretLen = 32
		b := make([]byte, secretLen)
		cryptoRand.Read(b)
		secret = fmt.Sprintf("%x", b)
		// Best-effort: a failed mkdir surfaces via the WriteFile error below.
		_ = os.MkdirAll(filepath.Dir(hubSecretPath), 0o755)
		if err := os.WriteFile(hubSecretPath, []byte(secret), 0o600); err != nil {
			logger.Error("failed to write hub secret", "error", err)
		}
		logger.Info("generated hub secret", "path", hubSecretPath)
	}
	s := &HubServer{
		mux:          http.NewServeMux(),
		logger:       logger,
		saveCh:       make(chan struct{}, 1),
		saveLoopStop: make(chan struct{}),
		saveLoopDone: make(chan struct{}),
		registryPath: registryPath,
		hubGitHash:   gitHash,
		hubGitBranch: gitBranch,
		hubSecret:    secret,
		// Provisional single generation holding the existing master. Replaced
		// immediately below by loadGenerations, which reads any persisted
		// rotation off the PVC. It is set here too so the field is never nil
		// between struct construction and that load.
		keyGenerations:          legacyGenerationSet(secret),
		clusters:                loadClusters(logger),
		heartbeatHealth:         make(map[string]*HeartbeatHealthEntry),
		poolReplenishHold:       make(map[string]time.Time),
		heartbeatUpgrade:        make(map[string]string),
		heartbeatSwitchTag:      make(map[string]string),
		clusterUnreachableUntil: make(map[string]time.Time),
		pendingWebhooks:         make(map[string]*pendingWebhookEntry),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		pendingGateways:         make(map[string]*HeartbeatGatewayConfig),
		hubBanners:              make(map[string]*HubBannerEntry),
		spokeProxyAuthCache:     make(map[string]spokeProxyAuthEntry),
		authRolloutSeen:         make(map[string]authRolloutEntry),
		timeline:                newTimelineStore(),
		journey:                 newJourneyStore(),
		alerts:                  newAlertState(),
		revokedSessions:         newRevokedSessions(),
		urlHealth:               newURLHealthState(),
		// /api/reach dependencies (#3994): the stub reporter answers an
		// empty fleet until phase 2a's store is wired via SetReachReporter;
		// the ancestry backend defaults to the compare-API adapter over
		// commit_order.go's cache (no repo clone ships in the hub image).
		reachReporter: &reach.StubReachReporter{},
		reachAncestry: newReachAncestry(logger),
		// Fleet error-rate history (#3995, phase 2c): loads its persisted
		// ring from disk here, before the first heartbeat can append.
		reachHistory: newReachHistoryStore(reachHistoryPath, logger),
	}

	// Restore any persisted rotation BEFORE anything mints or verifies. A hub
	// that came back on generation 1 after a rotation would reject every
	// artifact minted since it and quietly re-mint on the old key — strictly
	// worse than never having rotated.
	//
	// AUDIT 8 / F20. Three outcomes, and the third is the fix. An ABSENT file
	// means no rotation has ever been persisted, so the provisional legacy set
	// assigned above is already correct and generation 1 really is current. An
	// UNTRUSTED file — unreadable after retries, unparseable, or carrying no
	// minting generation — means a rotation MAY have been recorded and the hub
	// cannot tell which generation is current. In that case it must not keep
	// the provisional legacy set either: that set is the SUPERSEDED master, and
	// installing it is the silent un-rotation this finding is about.
	// AUDIT 8 / F19. Each arm below ALSO installs the set into the package-level
	// pointer the spoke-bound derivation path reads (setLiveGenerations). Before
	// this, provisionGenerationSet() ignored everything decided here and
	// unconditionally rebuilt generation 1 from the raw hub-secret.key, so the
	// hub minted on one set and provisioned the fleet from another. The two must
	// come from the same load or a rotation never reaches a spoke.
	switch gs, rotatedAt, outcome := loadGenerations(secret, logger); outcome {
	case generationsLoaded:
		s.keyGenerations = gs
		// Restore the rotation timestamp too, or the cooldown would reset on
		// every hub roll and stop guarding anything.
		s.lastKeyRotation = rotatedAt
		setLiveGenerations(gs, false)
	case generationsNeverRotated:
		// Legitimate fallback: keep the provisional legacy set built from
		// hub-secret.key. This is the state of every hub in the fleet today.
		if gs != nil {
			s.keyGenerations = gs
		}
		// Install it on the derivation side too. On a never-rotated hub this is
		// byte-identical to the pre-F19 legacyGenerationSet(provisionMasterSecret())
		// whenever HIVE_HUB_SECRET and hub-secret.key agree, which is the whole
		// fleet today — so this arm is a no-op in effect, by construction.
		setLiveGenerations(gs, false)
	case generationsUntrusted:
		// FAIL CLOSED. Drop the provisional legacy set rather than serve on it.
		//
		// WHY THIS SHAPE OF FAIL-CLOSED, and not the two alternatives:
		//
		//   - Not "refuse to start". The hub rolls several times a day and a
		//     crash-loop on a transient PVC fault would take SSO and the
		//     dashboard down fleet-wide — an availability failure strictly
		//     worse than the confidentiality one being fixed, and the hub
		//     cannot re-read the file if it is not running.
		//
		//   - Not "keep the last known in-memory set". At NewHubServer there is
		//     no last known set; the only thing in the field is the provisional
		//     legacy one, which IS the superseded material.
		//
		// So: nil the generation set. Every consumer of it is already
		// fail-closed on nil by contract — currentGeneration returns
		// (zero,false), currentSecret returns "", deriveDomainKey returns "" for
		// an empty master, and acceptableGenerations returns nil — so nothing
		// mints on a key the hub cannot vouch for. Heartbeat verification and
		// session verification both fall through to their documented
		// single-master paths (hub_keys.go:287, hub_session_revocation.go:434),
		// which key off s.hubSecret and are untouched here, so EXISTING SPOKES
		// KEEP AUTHENTICATING while the rotation-dependent lanes are held. That
		// is the deliberate trade: hold what could downgrade, keep what keeps
		// the fleet up.
		//
		// lastKeyRotation is left at its zero value only because nothing has
		// been loaded to set it from — and with keyGenerations nil, rotation is
		// refused outright by rotateMasterSecret, so the cleared cooldown can
		// no longer be used to strand anyone. See generationsUntrusted.
		s.keyGenerations = nil
		// AUDIT 8 / F19 + F20. Latch the untrusted state on the derivation side
		// too, and note this is NOT the same as installing nil there: nil means
		// "no hub in this process" and falls back to the legacy set, which is
		// exactly the re-derivation from superseded material that must not
		// happen here. The explicit flag makes provisionGenerationSet() return
		// nil, so desiredPerHiveEnv's empty-master guard skips every hive and the
		// sweep patches nothing rather than pushing the fleet backwards onto
		// generation 1.
		setLiveGenerations(nil, true)
		logger.Error("hub master generation state is UNTRUSTED — minting and rotation are DISABLED until an operator resolves the generations file; "+
			"existing spokes continue to authenticate on the single-master path",
			"path", hubGenerationsPath)
	}

	s.loadRegistry()
	// Rebuild the in-memory upgrade-delivery map from the durable registry
	// BEFORE the server takes its first heartbeat, so a hub restart can never
	// orphan an armed upgrade (#2476).
	s.recoverArmedUpgrades()
	s.loadHubBanners()
	s.timeline.load()
	if err := s.journey.load(time.Now()); err != nil {
		// Losing journey state is a soft failure: the worst case is that a few
		// owners get one duplicate nudge. Never block hub startup on it.
		logger.Error("failed to load journey nudge state", "error", err)
	}
	s.loadAlertAcks()
	// F10: restore the session revocation set BEFORE the mux starts serving, so
	// a hub that has just rolled never honours a session revoked before it.
	s.loadRevokedSessions()

	s.mux.HandleFunc("POST /api/heartbeat", s.handleHeartbeat)
	s.mux.HandleFunc("POST /api/task-status", s.handleTaskStatus)
	s.mux.HandleFunc("GET /api/registry", s.handleRegistry)
	s.mux.HandleFunc("GET /api/hub/leaderboard", s.handleLeaderboard)
	s.mux.HandleFunc("GET /api/hub/stats", s.handleStats)
	s.mux.HandleFunc("GET /api/fleet-stats", s.handleFleetStats)
	s.mux.HandleFunc("GET /api/hub/version", s.handleHubVersion)
	// Delegation public-key material (JWKS-equivalent). Registered WITHOUT
	// requireAuth because the response contains only Ed25519 public keys and
	// generation numbers, both non-secret in this codebase.
	s.mux.HandleFunc("GET "+delegation.KeysPath, s.handleDelegationKeys)
	s.mux.HandleFunc("DELETE /api/hub/registry/{id}", s.handleRegistryDelete)
	s.mux.HandleFunc("POST /api/contribute/register", s.handleContributeProxy)
	s.mux.HandleFunc("GET /api/contribute/status", s.handleContributeStatus)
	s.mux.HandleFunc("GET /api/contribute/ws", s.handleContributeWSProxy)
	s.mux.HandleFunc("POST /api/github/webhook", s.handleGitHubWebhook)
	s.mux.HandleFunc("GET /gh-setup", s.handleGitHubAppSetupRouter)
	s.mux.HandleFunc("GET /learn", s.serveStatic("static/learn.html"))
	s.mux.HandleFunc("GET /get-started", s.serveStatic("static/get-started.html"))
	s.mux.HandleFunc("GET /api/docs", s.serveStatic("static/api-docs.html"))
	s.mux.HandleFunc("GET /api/reading-list", s.handleReadingList)
	s.mux.HandleFunc("GET /reading", s.serveStatic("static/reading.html"))
	// Unlinked page (not in nav, noindex) — direct-URL only. The CNCF End User
	// reference-architecture draft, shareable without artifact permissions.
	s.mux.HandleFunc("GET /cncf-reference-architecture", s.serveStatic("static/cncf-reference-architecture.html"))
	// Fleet-divergence view. The HTML shell is inert (no data) and served
	// unauthenticated like every other static page; all sensitive data comes
	// from GET /api/saas/my-hives, which is requireAuth-gated. On a 401 the page
	// redirects to the OAuth login (see static/my-hives.html).
	// The page lives at /fleet — matching the "Fleet" nav button that opens it.
	// /my-hives (the original path) permanently redirects so old bookmarks,
	// OAuth redirect params, and shared links keep working; the API endpoint
	// /api/saas/my-hives is unchanged.
	s.mux.HandleFunc("GET /fleet", s.serveStatic("static/my-hives.html"))
	s.mux.HandleFunc("GET /my-hives", func(w http.ResponseWriter, r *http.Request) {
		target := "/fleet"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /{$}", s.serveStatic("static/index.html"))
	// Open Graph preview image for shared links. Registered here rather than in
	// registerOAuth because that function returns early when OAuth is
	// unconfigured, which would leave the image 404ing and every unfurled Hive
	// link showing a blank card.
	s.mux.HandleFunc("GET /og-card.png", s.handleOGCard)
	s.mux.Handle("GET /", http.FileServerFS(staticFS))

	s.registerOAuth()
	s.registerSaaSRoutes()
	s.registerOpenRouterRoutes()
	go s.saveLoop()

	return s
}

const (
	hubReadTimeout  = 30 * time.Second
	hubWriteTimeout = 60 * time.Second
	hubIdleTimeout  = 120 * time.Second
)

func (s *HubServer) Start(port int) error {
	addr := fmt.Sprintf(":%d", port)
	s.logger.Info("hub server starting", "addr", addr)
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.mux,
		ReadTimeout:  hubReadTimeout,
		WriteTimeout: hubWriteTimeout,
		IdleTimeout:  hubIdleTimeout,
	}
	// Start typically runs in its own goroutine while Shutdown is called
	// from another; guard the handoff of the server handle.
	s.httpMu.Lock()
	s.httpServer = srv
	s.httpMu.Unlock()
	return srv.ListenAndServe()
}

func (s *HubServer) Shutdown(timeout time.Duration) error {
	// Join the registry save loop first, flushing any pending debounced write
	// (#4774) — after this, no background registry I/O remains.
	s.StopSaveLoop()
	// Flush the reach history's final partial save interval (#3995) — the
	// same durability courtesy the registry gets from its synchronous saves.
	// Before the listener check: a server shut down before Start still owes
	// its history a flush, and SaveNow is nil-safe and dirty-gated.
	s.reachHistory.SaveNow()
	s.httpMu.Lock()
	srv := s.httpServer
	s.httpMu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return srv.Shutdown(ctx)
}

func (s *HubServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	// Heartbeats authenticate with the derived HEARTBEAT sub-key, not the master
	// secret — the master never leaves the hub (C2 domain separation). Spokes are
	// injected only this sub-key, so a spoke operator's bearer can prove
	// "I am a provisioned spoke" but cannot sign a session/SSO/impersonation value.
	// The bearer is captured here but VERIFIED below, after the body is parsed:
	// the per-hive bearer (N1) can only be checked once the claimed hive_id is
	// known. It is now the ONLY accepted credential — the fleet-wide lane was
	// deleted (F2). See verifyHeartbeatBearer.
	presentedBearer := ""
	if s.hubSecret != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		presentedBearer = strings.TrimPrefix(auth, "Bearer ")
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var payload HeartbeatPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	payload.HiveID = sanitizeHeartbeatField(payload.HiveID)
	if payload.HiveID == "" {
		http.Error(w, "hive_id required", http.StatusBadRequest)
		return
	}
	if !isValidName(payload.HiveID) {
		http.Error(w, "invalid hive_id", http.StatusBadRequest)
		return
	}

	// SECURITY (audit N1, CWE-200/639/862): bind the bearer to the CLAIMED
	// identity.
	//
	// The bearer used to be checked against s.heartbeatKey() alone — one
	// fleet-wide value derived from a fixed label, so every spoke held the same
	// bytes. Possession proved "some provisioned spoke" and nothing more, and
	// payload.HiveID was then trusted verbatim. Three key-delivery lanes read
	// off that claimed ID (pending OpenRouter key, legacy pending App config,
	// standing cluster App key), so any spoke could beat as any victim and be
	// handed the victim's key material.
	if s.hubSecret != "" {
		if !s.verifyHeartbeatBearer(presentedBearer, payload.HiveID) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// #3234: record WHICH format authenticated. The fleet-wide compat lane is
		// now DELETED (F2), so this can only ever record the per-hive format; it is
		// retained to back GET /api/saas/admin/auth-rollout, where an operator can
		// confirm no hive regressed after the cutover.
		s.noteHeartbeatAuthPath(payload.HiveID, s.heartbeatBearerIsPerHive(presentedBearer, payload.HiveID))
	}
	// Adopt the spoke's reported dashboard-token hash into the hub's stored
	// record — the reference spoke-relayed upgrade requests verify their proof
	// against. Done only after the per-hive bearer authenticated the beat (a
	// hive can only set its OWN record), only for hosted hives that have a
	// record, and only when the value is a well-formed digest that changed.
	s.adoptSpokeDashboardTokenHash(&payload)
	// Normalize the reported SHA to the canonical short length up front so every
	// downstream comparison is same-length against the hub's 7-char stored SHAs.
	// (Spokes now build gitShort with `--short=7`; this covers any that predate
	// that or report a longer value.)
	payload.GitHash = shortSHA(payload.GitHash)
	// (hive_id is validated above, before the bearer check — the per-hive bearer
	// is derived from it, so it must be well-formed before it can authenticate.)
	if payload.Org != "" && !isValidName(payload.Org) {
		http.Error(w, "invalid org name", http.StatusBadRequest)
		return
	}
	if payload.PrimaryRepo != "" && !isValidRepoRef(payload.PrimaryRepo) {
		http.Error(w, "invalid repo name", http.StatusBadRequest)
		return
	}
	for _, repo := range payload.Repos {
		if !isValidRepoRef(repo) {
			http.Error(w, "invalid repo name in list", http.StatusBadRequest)
			return
		}
	}
	if payload.DashboardURL != "" && !strings.HasPrefix(payload.DashboardURL, "http://") && !strings.HasPrefix(payload.DashboardURL, "https://") {
		http.Error(w, "dashboard_url must start with http:// or https://", http.StatusBadRequest)
		return
	}
	if payload.SnapshotURL != "" && !strings.HasPrefix(payload.SnapshotURL, "http://") && !strings.HasPrefix(payload.SnapshotURL, "https://") {
		http.Error(w, "snapshot_url must start with http:// or https://", http.StatusBadRequest)
		return
	}

	payload.Org = sanitizeField(payload.Org)
	payload.PrimaryRepo = sanitizeField(payload.PrimaryRepo)
	payload.Owner = sanitizeField(payload.Owner)
	for i, a := range payload.Agents {
		if !isValidName(a.Name) {
			http.Error(w, "invalid agent name", http.StatusBadRequest)
			return
		}
		payload.Agents[i].Name = sanitizeField(a.Name)
	}
	for i, lb := range payload.Leaderboard {
		if !isValidName(lb.GitHubUsername) {
			http.Error(w, "invalid leaderboard username", http.StatusBadRequest)
			return
		}
		payload.Leaderboard[i].GitHubUsername = sanitizeField(lb.GitHubUsername)
		payload.Leaderboard[i].HiveName = sanitizeField(lb.HiveName)
	}
	safeOrg := payload.Org
	safePrimary := payload.PrimaryRepo

	entry := RegistryEntry{
		ID:   payload.HiveID,
		Name: safeOrg + "/" + safePrimary,
		Org:  safeOrg,
		Repos: func() []string {
			safe := make([]string, len(payload.Repos))
			for i, r := range payload.Repos {
				safe[i] = sanitizeHeartbeatField(r)
			}
			return safe
		}(),
		PrimaryRepo:       safePrimary,
		AIAuthor:          sanitizeField(payload.AIAuthor),
		AIAuthorEffective: sanitizeField(payload.AIAuthorEffective),
		StartedAt:         sanitizeField(payload.StartedAt),
		// Duplicate-instance detection. noteReporter returns "" until two
		// distinct reporters have ALTERNATED (A→B→A), so a normal rollout
		// (A→B, B stays) never trips it.
		ConflictingReporters: s.noteReporter(payload.HiveID, sanitizeField(payload.Reporter)),
		// Advisory-staleness signal. The timestamp is an identifier-shaped
		// string; the error is prose (a sentence shown to a human), so it takes
		// the prose sanitizer — html.EscapeString here plus the dashboard's own
		// esc() double-escaped it into "&amp;amp;" artifacts. An empty
		// AdvisoryLastPostedAt is preserved as empty so the render/gate reads
		// it as UNKNOWN rather than stale.
		AdvisoryLastPostedAt:  sanitizeField(payload.AdvisoryLastPostedAt),
		AdvisoryFindingCount:  payload.AdvisoryFindingCount,
		AdvisoryOverflowCount: payload.AdvisoryOverflowCount,
		AdvisoryError:         sanitizeProseField(payload.AdvisoryError),
		// Inference-backend auth-failure signal. Sanitized like every other
		// spoke-reported string; empty is preserved as empty (no signal).
		InferenceAuthError:   sanitizeField(payload.InferenceAuthError),
		GatewayHealth:        sanitizeGatewayHealth(payload.GatewayHealth),
		ProviderLimitReason:  sanitizeProseField(payload.ProviderLimitReason),
		ProviderLimitRebuffs: clampInt(payload.ProviderLimitRebuffs, 0, 1_000_000),
		DashboardURL:         payload.DashboardURL,
		PublicURLSelfCheck:   sanitizePublicURLSelfCheck(payload.PublicURLSelfCheck),
		RouteExists:          sanitizeRouteExistenceCheck(payload.RouteExists),
		SnapshotURL:          payload.SnapshotURL,
		ACMMLevel:            clampInt(payload.ACMMLevel, 0, 6),
		AgentCount: func() int {
			count := 0
			for _, a := range payload.Agents {
				if a.State == "running" {
					count++
				}
			}
			return count
		}(),
		GovernorMode:       sanitizeHeartbeatField(payload.Governor.Mode),
		TotalTokens24h:     clampInt64(payload.Tokens24h, 0, 100_000_000_000),
		ActionableIssues:   clampInt(payload.Governor.Issues, 0, 10_000),
		ActionablePRs:      clampInt(payload.Governor.PRs, 0, 10_000),
		WorkSource:         sanitizeHeartbeatField(payload.Governor.WorkSource),
		ContributorCount:   clampInt(payload.Contributors.Registered, 0, 10_000),
		ActiveContributors: clampInt(payload.Contributors.Active, 0, 10_000),
		Owner:              sanitizeHeartbeatField(payload.Owner),
		HiveType: func() string {
			if payload.HiveType != "" {
				return sanitizeHeartbeatField(payload.HiveType)
			}
			if strings.HasPrefix(payload.HiveID, "hosted-") || strings.HasPrefix(payload.HiveID, "saas-") {
				return "hosted"
			}
			return "local"
		}(),
		IsPublic:      payload.IsPublic,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
		Health:        payload.Health,
		Version:       sanitizeHeartbeatField(payload.Version),
		GitHash:       shortSHA(sanitizeHeartbeatField(payload.GitHash)),
		GitBranch:     sanitizeHeartbeatField(payload.GitBranch),
		ImageRef:      sanitizeImageRef(payload.ImageRef),
		// Normalize through githubHostLabel so "https://github.ibm.com/" and
		// "github.ibm.com" persist identically, then sanitize as a hostname.
		// A spoke that does not report the field leaves this empty rather
		// than recording a fabricated "github.com" — empty is what lets the
		// read path fall back to the hive's recorded/cluster host instead.
		GitHubHost: func() string {
			if strings.TrimSpace(payload.GitHubHost) == "" {
				return ""
			}
			return sanitizeHeartbeatField(githubHostLabel(payload.GitHubHost))
		}(),
		Agents: func() []AgentSummary {
			for i := range payload.Agents {
				payload.Agents[i].Name = sanitizeHeartbeatField(payload.Agents[i].Name)
				payload.Agents[i].State = sanitizeHeartbeatField(payload.Agents[i].State)
				payload.Agents[i].Mode = sanitizeHeartbeatField(payload.Agents[i].Mode)
				// Timestamps are spoke-reported strings like every other field
				// here. An unparseable value is later read as "unknown" by the
				// inactive-agent rule, never as evidence of idleness.
				payload.Agents[i].StartedAt = sanitizeHeartbeatField(payload.Agents[i].StartedAt)
				payload.Agents[i].LastActivityAt = sanitizeHeartbeatField(payload.Agents[i].LastActivityAt)
				// Pause provenance (#4041). Trigger and actor are identifiers
				// ("dashboard-api", a GitHub login); the reason is prose the
				// hover renders to a human; the timestamp keeps its colons via
				// the HTML-escaping sanitizer, same as the entry's StartedAt.
				payload.Agents[i].PausedTrigger = sanitizeHeartbeatField(payload.Agents[i].PausedTrigger)
				payload.Agents[i].PausedBy = sanitizeHeartbeatField(payload.Agents[i].PausedBy)
				payload.Agents[i].PausedReason = sanitizeProseField(payload.Agents[i].PausedReason)
				payload.Agents[i].PausedAt = sanitizeField(payload.Agents[i].PausedAt)
				// Fleet-divergence signals (#hub-fleet-view). Backend is a spoke-
				// reported identifier (claude/copilot/gemini/…) and is sanitized
				// like State/Mode. The remaining new signals — ExpectedActive,
				// CanOpenIssue, CanOpenPR, CanMerge, Enabled — are bools with no
				// injectable surface, so they pass through unchanged; they are
				// acknowledged here so a future reader sees every new field was
				// considered by the sanitize pass.
				payload.Agents[i].Backend = sanitizeHeartbeatField(payload.Agents[i].Backend)
			}
			const maxAgents = 50
			if len(payload.Agents) > maxAgents {
				payload.Agents = payload.Agents[:maxAgents]
			}
			return payload.Agents
		}(),
		Leaderboard: func() []LeaderboardEntry {
			hiveName := safeOrg + "/" + safePrimary
			for i := range payload.Leaderboard {
				payload.Leaderboard[i].HiveName = hiveName
				payload.Leaderboard[i].GitHubUsername = sanitizeHeartbeatField(payload.Leaderboard[i].GitHubUsername)
				// A task title is prose ("Fix the drift hover"), not an
				// identifier — the strict sanitizer would strip its spaces.
				payload.Leaderboard[i].CurrentTask = sanitizeProseField(payload.Leaderboard[i].CurrentTask)
			}
			return payload.Leaderboard
		}(),
		Online:            true,
		GitHubAppRequired: payload.GitHubAppRequired,
		// GitHubAppPermIssue is a full sentence the drift hover renders
		// verbatim ("The GitHub App is configured but has no installation.
		// Install the app on your org to enable agents.") — the strict
		// identifier sanitizer stripped every space out of it.
		GitHubAppPermIssue:       sanitizeProseField(payload.GitHubAppPermIssue),
		GitHubAppState:           sanitizeHeartbeatField(payload.GitHubAppState),
		GitHubAppTokenStatus:     sanitizeHeartbeatField(payload.GitHubAppTokenStatus),
		GitHubAppTokenLastMintAt: sanitizeField(payload.GitHubAppTokenLastMintAt),
		GitHubAppTokenError:      sanitizeProseField(payload.GitHubAppTokenError),
		RepoTargetMisconfigured:  payload.RepoTargetMisconfigured,
		RepoTargetIssue:          sanitizeProseField(payload.RepoTargetIssue),
		StatusFlipping:           s.noteStatusFlip(payload.HiveID, sanitizeHeartbeatField(payload.GitHubAppState)),
		GitHubAppID:              payload.GitHubAppID,
		GitHubAppSlug:            payload.GitHubAppSlug,
		GitHubInstallationID:     payload.GitHubInstallationID,
		GitHubAPIURL:             payload.GitHubAPIURL,
		GitHubBaseURL:            payload.GitHubBaseURL,
		PendingGitHubAppInstall:  payload.PendingGitHubAppInstall,
		PendingGitHubAppInstallAt: func() time.Time {
			if payload.PendingGitHubAppInstall {
				return time.Now()
			}
			return time.Time{}
		}(),
		PRsMerged90d:   clampFleetCount(payload.PRsMerged90d),
		PRsRejected90d: clampFleetCount(payload.PRsRejected90d),
		CVEsClosed:     clampFleetCount(payload.CVEsClosed),
		// Quadrant signals. Reuse clampFleetCount's discipline: a negative is
		// nonsense from a spoke and becomes nil ("not reported") rather than
		// being clamped to zero, which would be indistinguishable from a real
		// measurement and would quietly drag the hive's score.
		BudgetCurrentSpend:   clampQuadrantSpend(payload.BudgetCurrentSpend),
		BudgetLimit:          clampQuadrantSpend(payload.BudgetLimit),
		BudgetWindowStartsAt: parseHeartbeatTime(payload.BudgetWindowStartsAt),
		BudgetWindowEndsAt:   parseHeartbeatTime(payload.BudgetWindowEndsAt),
		BudgetExhausted:      payload.BudgetExhausted,
		BudgetIgnored:        payload.BudgetIgnored,
		HoldTotal:            clampFleetCount(payload.HoldTotal),
		AwaitingReview:       clampFleetCount(payload.AwaitingReview),
		SLAViolations:        clampFleetCount(payload.SLAViolations),
		TasksCompleted7d:     clampFleetCount(payload.TasksCompleted7d),
		// Remediation-hint detectors (#5577): sanitized + clamped, never the
		// raw payload. nil stays nil ("not measured") and is carried forward
		// below; a measured empty value stays empty and overwrites.
		AgentErrorStreaks: sanitizeAgentErrorStreaks(payload.AgentErrorStreaks),
		ConsentWedged:     sanitizeAgentNameList(payload.ConsentWedged),
		NoCadenceAgents:   sanitizeAgentNameList(payload.NoCadenceAgents),
		FleetStatsCollectedAt: func() time.Time {
			if payload.FleetStatsCollectedAt == "" {
				return time.Time{}
			}
			t, err := time.Parse(time.RFC3339, payload.FleetStatsCollectedAt)
			if err != nil {
				return time.Time{}
			}
			return t
		}(),
		// Preserve nil (old spoke, unknown) vs a real count — journey stage
		// detection depends on telling those two apart.
		AgentsWithModel: clampFleetCount(payload.AgentsWithModel),
		// Component reach (#3993): sanitized + clipped, never the raw spoke
		// report — see sanitizeComponentReach for the bounds. Storage only.
		ComponentReach: sanitizeComponentReach(payload.ComponentReach),
		// Per-repo output activity (hive-health): sanitized/clamped, never the
		// raw payload. The collected-at + window travel with it so the verdict
		// can age the summary.
		RepoActivity:                 sanitizeRepoActivity(payload.RepoActivity),
		RepoActivityCollectedAt:      parseHeartbeatTime(payload.RepoActivityCollectedAt),
		RepoActivityWindowHours:      clampInt(payload.RepoActivityWindowHours, 0, repoActivityMaxWindowHours),
		RepoActivityCountWindowHours: clampInt(payload.RepoActivityCountWindowHours, 0, repoActivityMaxWindowHours),
	}

	// Fleet error-rate history (#3995, phase 2c): fold this beat's rolling
	// window_1h buckets into the hub's per-component retention ring. Runs on
	// the SANITIZED report only, and only when the beat actually carried one
	// — entry.ComponentReach is still the fresh value here; the carry-forward
	// of a previous report (below) never reaches this call, so a restarting
	// spoke's stale report is not re-appended (the dedupe cursor would refuse
	// it anyway, but not appending is cheaper and clearer).
	if entry.ComponentReach != nil {
		s.reachHistory.Append(entry.ID, entry.ComponentReach, time.Now())
	}

	// Populate ClusterID and the authoritative ProvStatus from the SaaS hive
	// record (a single load reused for both). ProvStatus is what lets
	// computeFleetStats tell a genuinely-unclaimed placeholder (statusAvailable)
	// from a CLAIMED one that still carries the "hosted-available-" ID — the ID
	// prefix alone over-counts available hives because it never changes on claim.
	clusterID := ""
	// Whether this hive has an authoritative SaaS record. Its Owner (the
	// authorization principal) comes from there when present, and the
	// existing-entry preservation below uses this to decide whether to carry the
	// prior Owner forward for non-SaaS (local) hives.
	hasSaaSRecord := false
	// saasVisibility carries the SaaS store's authoritative is_public for this
	// hive when a SaaSHive record exists. handleToggleVisibility persists the
	// operator's choice there (meta.json) as the source of truth, so it — not the
	// spoke's self-report — is what entry.IsPublic must reflect. nil means "no
	// SaaS record" (a bare/BYO spoke), in which case the spoke's own report stands.
	var saasVisibility *bool
	if sh := loadSaaSHive(payload.HiveID); sh != nil {
		hasSaaSRecord = true
		clusterID = sh.ClusterID
		entry.ProvStatus = sh.Status
		isPub := sh.IsPublic
		saasVisibility = &isPub
		// The SaaS store is authoritative for the operator-editable display name
		// (handleRenameHive persists it here), so overlay it onto the registry
		// entry the dashboard renders. The spoke never reports this, so a blank
		// ProjectName simply leaves the client on its org/repo fallback label.
		entry.ProjectName = sh.ProjectName
		// SECURITY (C1/N3, CWE-639): Owner is an authorization principal — every
		// saas.go handler gates writes on `h.Owner == username`. The SaaS store is
		// its authoritative source (set at provisioning and by ownership transfer),
		// so overlay it here and NEVER let the heartbeat body assert it. A spoke
		// beating with someone else's hive_id could otherwise rewrite that hive's
		// Owner in the registry the dashboard reads.
		entry.Owner = sh.Owner
		// Persist the hosted namespace ("hive-hosted-<id>") the same way — the
		// hub already derives it deterministically from the (stable) hive ID, so
		// storing it here makes it authoritative, self-healing across a claim/
		// reassign, and available to My Hives + kubectl-exec without a live
		// `kubectl get ns` hunt. Only set when a SaaSHive record exists, so a
		// self-hosted/BYO hive keeps Namespace empty rather than being wrongly
		// labelled with a namespace it does not run in.
		entry.Namespace = hostedNamespaceForHive(sh)
	}
	// The SaaS store's is_public is authoritative over the spoke's self-report.
	// Apply it here so BOTH the update path (existing registry entry) and the
	// first-heartbeat path (new entry, no previous registry row to preserve
	// from) reflect the operator's choice rather than the spoke's provisioning
	// default. Without this, an available placeholder — which keeps reporting
	// is_public: true every heartbeat — reverted the operator's PRIVATE setting
	// back to public on the next beat, and the authoritative-visibility push to
	// the spoke (gated on entry.IsPublic != payload.IsPublic below) never fired
	// because the two had been made equal.
	if saasVisibility != nil {
		entry.IsPublic = *saasVisibility
	}
	if clusterID == "" && payload.ClusterID != "" {
		clusterID = sanitizeHeartbeatField(payload.ClusterID)
	}
	if clusterID == "" {
		clusterID = defaultClusterID
	}
	entry.ClusterID = clusterID
	if c, ok := s.clusters[clusterID]; ok {
		entry.ClusterName = c.Name
	}

	if payload.Upgrading {
		entry.Upgrading = true
	}

	// A spoke that explicitly reports a FAILED upgrade must not be left showing
	// the "Upgrading" spinner, and must not be re-instructed on a loop. Record
	// the cause so operators can see WHY, and drop the pending instruction: the
	// spoke has already exhausted its own retry budget, so re-sending the same
	// target every heartbeat only reproduces the failure.
	if payload.UpgradeFailed {
		entry.Upgrading = false
		entry.UpgradeFailed = true
		// An upgrade failure cause is a sentence, not an identifier.
		entry.UpgradeError = sanitizeProseField(payload.UpgradeError)
		entry.UpgradeFailedAt = time.Now()
		s.mu.Lock()
		delete(s.heartbeatUpgrade, payload.HiveID)
		s.mu.Unlock()
		s.logger.Error("heartbeat: spoke reported a FAILED upgrade",
			"hive_id", payload.HiveID,
			"from", payload.GitHash,
			"to", payload.UpgradeTargetSHA,
			"error", payload.UpgradeError,
		)
	}

	// Timeline: capture the pre-heartbeat entry inside the lock (it is the only
	// place old and new coexist), but diff and persist AFTER unlocking —
	// s.mu is a write lock held across the whole registry scan, so a file
	// write under it would serialize every heartbeat in the fleet.
	var prevEntry RegistryEntry
	hadPrev := false

	s.mu.Lock()
	found := false
	for i, h := range s.registry.Hives {
		if h.ID == payload.HiveID {
			// `h` is a value copy from the range, so it already snapshots the
			// scalar fields the timeline compares. Health is a map that the
			// new entry never mutates in place, so aliasing it is safe here.
			prevEntry, hadPrev = h, true
			entry.RegisteredAt = h.RegisteredAt
			if entry.SnapshotURL == "" {
				entry.SnapshotURL = h.SnapshotURL
			}
			// Project-identity carry-forward for DEGRADED beats. Three beat
			// classes legitimately omit project fields: the pre-restart
			// "upgrading" beat (a minimal payload, and the LAST beat the hub
			// holds for the whole restart window that follows), the identity-
			// only minimal liveness beat (fresh restart, collect timed out
			// with no cache — StatsStale), and the upgrade-failure report
			// (hive_id + SHAs only). The entry is otherwise rebuilt from the
			// payload VERBATIM, so those beats blanked org/primaryRepo/repos —
			// the public-directory row lost its repo link (contributors had
			// nothing to contribute to) and the hive rendered as "org/" until
			// the next full collect landed (observed live on the kubestellar/
			// hive spoke, which auto-upgrades several times a day).
			//
			// Gated on the degraded classes so a full, healthy collect always
			// wins — including a genuine clear. Repo fields carry only under
			// the SAME org: a reassignment/reset changes the org (a reset slot
			// reports its synthetic "available-<id>" org), and the old
			// tenant's repos must never survive it.
			if payload.Upgrading || payload.StatsStale || payload.UpgradeFailed {
				if entry.Org == "" && h.Org != "" {
					entry.Org = h.Org
				}
				if strings.EqualFold(entry.Org, h.Org) {
					if entry.PrimaryRepo == "" && h.PrimaryRepo != "" {
						entry.PrimaryRepo = h.PrimaryRepo
					}
					if len(entry.Repos) == 0 && len(h.Repos) > 0 {
						entry.Repos = h.Repos
					}
				}
				// Name was computed from the payload's (possibly empty)
				// fields at build time — recompute it from the carried ones.
				entry.Name = entry.Org + "/" + entry.PrimaryRepo
			}
			// SECURITY (C1/N3, CWE-639): for an EXISTING entry, Owner is never taken
			// from the heartbeat body. When this hive has an authoritative SaaS
			// record its Owner was already overlaid above (sh.Owner). Otherwise
			// carry the previously-recorded Owner forward so a caller cannot rewrite
			// another hive's ownership principal simply by beating with its hive_id.
			// The heartbeat body's Owner is honoured only when first registering a
			// brand-new entry (the !found branch below), where there is no prior
			// value and no SaaS record to contradict it.
			if !hasSaaSRecord {
				entry.Owner = h.Owner
			}
			// Preserve hub-side visibility. When the hive has a SaaS record its
			// is_public was already applied above (authoritative in BOTH
			// directions). This fallback covers a bare/BYO spoke with no SaaS
			// record: keep a hub-set PUBLIC from being reverted by a spoke that
			// reports private.
			if saasVisibility == nil && h.IsPublic && !payload.IsPublic {
				entry.IsPublic = h.IsPublic
			}
			// Carry the last known fleet-stat counts forward when this
			// heartbeat does not carry them. The counts live only in the
			// spoke's memory, so every restart reports nil until the periodic
			// collector next succeeds. Overwriting the stored value with nil
			// dropped the hive out of the public total for that whole gap —
			// with a fleet-wide restart, that silently collapsed the landing
			// page's numbers. Preserving the previous value (with its
			// collection timestamp, which the aggregator ages out) keeps the
			// total honest instead of quietly shrinking it.
			if entry.PRsMerged90d == nil && h.PRsMerged90d != nil {
				entry.PRsMerged90d = h.PRsMerged90d
			}
			if entry.PRsRejected90d == nil && h.PRsRejected90d != nil {
				entry.PRsRejected90d = h.PRsRejected90d
			}
			if entry.CVEsClosed == nil && h.CVEsClosed != nil {
				entry.CVEsClosed = h.CVEsClosed
			}
			if entry.FleetStatsCollectedAt.IsZero() {
				entry.FleetStatsCollectedAt = h.FleetStatsCollectedAt
			}
			// Carry the quadrant signals forward for the same reason as the
			// counts above: a spoke that just restarted reports nil until its
			// governor and collectors warm back up, and overwriting with nil
			// would collapse that hive's kite to "not measured" for the gap —
			// which reads as a data problem rather than the transient it is.
			//
			// The budget window is carried WITH its spend, never apart from
			// it. The spend number means nothing without the bounds that scope
			// it, so preserving one while dropping the other would produce a
			// figure that looks current but describes a window that has since
			// rolled.
			if entry.BudgetCurrentSpend == nil && h.BudgetCurrentSpend != nil {
				entry.BudgetCurrentSpend = h.BudgetCurrentSpend
				entry.BudgetWindowStartsAt = h.BudgetWindowStartsAt
				entry.BudgetWindowEndsAt = h.BudgetWindowEndsAt
			}
			if entry.BudgetLimit == nil && h.BudgetLimit != nil {
				entry.BudgetLimit = h.BudgetLimit
			}
			if entry.BudgetExhausted == nil && h.BudgetExhausted != nil {
				entry.BudgetExhausted = h.BudgetExhausted
			}
			if entry.BudgetIgnored == nil && h.BudgetIgnored != nil {
				entry.BudgetIgnored = h.BudgetIgnored
			}
			if entry.HoldTotal == nil && h.HoldTotal != nil {
				entry.HoldTotal = h.HoldTotal
			}
			if entry.AwaitingReview == nil && h.AwaitingReview != nil {
				entry.AwaitingReview = h.AwaitingReview
			}
			if entry.SLAViolations == nil && h.SLAViolations != nil {
				entry.SLAViolations = h.SLAViolations
			}
			if entry.TasksCompleted7d == nil && h.TasksCompleted7d != nil {
				entry.TasksCompleted7d = h.TasksCompleted7d
			}
			// Carry the remediation-hint detectors (#5577) forward the same
			// way: a restarting spoke reports nil ("not measured") until its
			// collectors warm up, and blanking the last real measurement
			// would hide a live wedge for exactly the window an operator is
			// most likely to be looking. A MEASURED all-clear arrives as an
			// empty non-nil value, is not nil here, and correctly overwrites.
			if entry.AgentErrorStreaks == nil && h.AgentErrorStreaks != nil {
				entry.AgentErrorStreaks = h.AgentErrorStreaks
			}
			if entry.ConsentWedged == nil && h.ConsentWedged != nil {
				entry.ConsentWedged = h.ConsentWedged
			}
			if entry.NoCadenceAgents == nil && h.NoCadenceAgents != nil {
				entry.NoCadenceAgents = h.NoCadenceAgents
			}
			// Carry the last real component-reach report forward when this
			// beat omits one (#3993) — a restarting spoke reports nil until
			// its counters warm back up from /data/reach-state.json, and
			// overwriting with nil would blank the hive's reach history for
			// that gap. Same pattern as the fleet-stat counts above.
			if entry.ComponentReach == nil && h.ComponentReach != nil {
				entry.ComponentReach = h.ComponentReach
			}
			// Carry the last real repo-activity summary forward when this beat
			// omits it (minimal upgrade-beat, or a spoke whose collector hasn't
			// re-warmed from /data/activity.json yet). The collected-at and
			// window travel WITH the summary — same reasoning as the budget
			// window: a count aged by a stale timestamp is honest, a count
			// carried without its timestamp looks current when it isn't.
			if entry.RepoActivity == nil && h.RepoActivity != nil {
				entry.RepoActivity = h.RepoActivity
				entry.RepoActivityCollectedAt = h.RepoActivityCollectedAt
				entry.RepoActivityWindowHours = h.RepoActivityWindowHours
				entry.RepoActivityCountWindowHours = h.RepoActivityCountWindowHours
			}
			// Advisory post time survives a spoke restart — see
			// carryAdvisoryPostTime for why that is the difference between a
			// wedged digest being flagged and being invisible forever.
			carryAdvisoryPostTime(&entry, h)
			branchForLatest := payload.GitBranch
			if branchForLatest == "" {
				branchForLatest = "v2"
			}
			registryLatestSHA := getLatestSHAForBranch(branchForLatest)
			// The orphan-sweep retry budget is HUB-side state and must survive
			// ordinary heartbeats. The rebuilt entry starts from the payload
			// (which never carries it), so without this carry every ~2-min
			// beat silently reset OrphanedUpgradeSweeps to 0 — the terminal
			// "give up after maxOrphanedUpgradeSweeps and report UpgradeFailed"
			// state was unreachable for any hive that heartbeats at all, and a
			// structurally-unable-to-upgrade hive looped clear→re-arm→re-latch
			// forever, pinned in "Upgrading" with no human-visible fault. The
			// completion branches below still reset it to 0 explicitly, which
			// is the only legitimate reset: the upgrade actually landed.
			entry.OrphanedUpgradeSweeps = h.OrphanedUpgradeSweeps
			// A hive that demonstrably MOVED builds has ended whatever wedge
			// streak the budget was counting, even when no latch is set (e.g.
			// the sweep cleared it and the upgrade then landed) — carrying the
			// spent budget into the NEXT upgrade would let a single future
			// sweep tip a healthy hive straight into terminal UpgradeFailed.
			if entry.GitHash != "" && h.GitHash != "" && !sameCommit(entry.GitHash, h.GitHash) {
				entry.OrphanedUpgradeSweeps = 0
			}
			// Carry a previously-reported upgrade failure forward across ordinary
			// heartbeats (which do not repeat it), but clear it the moment the
			// spoke actually reports a NEW git hash — that means it finally moved.
			if h.UpgradeFailed && !payload.UpgradeFailed {
				if entry.GitHash != "" && h.GitHash != "" && entry.GitHash != h.GitHash {
					entry.UpgradeFailed = false
					entry.UpgradeError = ""
					entry.UpgradeFailedAt = time.Time{}
					s.logger.Info("heartbeat: previously failed upgrade has landed",
						"hive_id", payload.HiveID, "git_hash", entry.GitHash)
				} else {
					entry.UpgradeFailed = true
					entry.UpgradeError = h.UpgradeError
					entry.UpgradeFailedAt = h.UpgradeFailedAt
				}
			}
			// A floating-tag deployment (…-latest) pulls the tag on every
			// rollout and therefore CANNOT land on a specific historical
			// commit — it lands on whatever CI last published. So its reported
			// GitHash chases an ever-advancing branch HEAD and may equal
			// neither the armed target NOR the poller's registryLatestSHA at the
			// instant this beat arrives (the two caches can be a commit apart
			// while CI is active). Comparing SHAs at all is the wrong test for
			// these hives: a non-upgrading heartbeat is itself proof the rollout
			// finished onto the latest tag. Treat that as complete, or the hive
			// stays latched "Upgrading" and the stale-upgrade sweep re-arms and
			// re-rolls its pod every staleUpgradeTimeout forever. Commit-pinned
			// hives keep the exact-SHA test below — their tag resolves to one
			// build, so "did it reach the target" is a meaningful question.
			// A CHANNEL-switch latch ("stable") is only satisfied by the
			// reported image actually running that tag. Without this guard, a
			// spoke still on v4-latest (mutable) sending one quiet beat
			// cleared the latch — the switch evaporated from the dashboard
			// while the hive never moved (observed live 2026-08-13 when the
			// hub auto-rolled mid-switch).
			channelTarget := isReleaseChannel(h.UpgradeTarget)
			channelSatisfied := imageTagOf(sanitizeImageRef(payload.ImageRef)) == h.UpgradeTarget
			floatingAtLatest := h.Upgrading && !payload.Upgrading && imageTagIsMutable(payload.ImageRef) &&
				(!channelTarget || channelSatisfied)
			if floatingAtLatest {
				entry.Upgrading = false
				entry.UpgradeTarget = ""
				// The upgrade is done — drop the start clock so the next one
				// times from zero, not from this completed rollout.
				entry.UpgradeStartedAt = time.Time{}
				entry.OrphanedUpgradeSweeps = 0
			} else if h.Upgrading && !payload.Upgrading && (sameCommit(payload.GitHash, h.UpgradeTarget) || (registryLatestSHA != "" && sameCommit(payload.GitHash, registryLatestSHA)) || commitAtOrAheadOfTarget(payload.GitHash, h.UpgradeTarget, s.logger)) {
				// Non-upgrading heartbeat at the target SHA, at latest, or at
				// a commit AHEAD of the target — upgrade completed (image may
				// have advanced past the original target before the spoke
				// pulled it). sameCommit tolerates short/full SHA length
				// mismatch — a raw == left the Upgrading badge spinning
				// forever when the spoke reported a full SHA and the
				// target/latest was the 7-char short form. The at-or-ahead
				// check closes the remaining gap (the vllmd-13 wedge): a
				// floating-tag spoke that pulled a build NEWER than the armed
				// target while the branch advanced yet again reports a hash
				// that equals neither the target nor the current latest, and
				// only commit ANCESTRY can prove it surpassed the target.
				// Cache-only under s.mu — see commit_order.go.
				entry.Upgrading = false
				entry.UpgradeTarget = ""
				// Upgrade landed — clear the start clock so the next upgrade
				// times from zero.
				entry.UpgradeStartedAt = time.Time{}
				// The upgrade landed, so any orphan-sweep retry budget spent
				// getting here is no longer relevant. Reset it, or a hive that
				// needed one sweep on each of three separate upgrades would be
				// declared permanently faulty on the third.
				entry.OrphanedUpgradeSweeps = 0
			} else if h.Upgrading && !payload.Upgrading && h.UpgradeTarget == "" {
				// Upgrading with no target (stale flag from config-only restart).
				entry.Upgrading = false
				entry.UpgradeTarget = ""
				entry.UpgradeStartedAt = time.Time{}
			} else if payload.UpgradeFailed {
				// The spoke just told us the upgrade FAILED. That is direct
				// evidence and must beat the inference below, which would
				// otherwise re-assert the stale Upgrading flag (same GitHash as
				// before is exactly what a failed upgrade looks like) and put the
				// permanent spinner straight back.
				entry.Upgrading = false
				entry.UpgradeTarget = ""
				entry.UpgradeStartedAt = time.Time{}
			} else if h.Upgrading && h.UpgradeTarget != "" {
				if entry.GitHash != h.GitHash && entry.GitHash != "" {
					entry.Upgrading = false
					entry.UpgradeTarget = ""
					entry.UpgradeStartedAt = time.Time{}
				} else {
					entry.Upgrading = true
					entry.UpgradeTarget = h.UpgradeTarget
					// Preserve the trigger timestamp — losing it here made
					// the stale-upgrade recovery in triggerAutoUpgrades()
					// unreachable (it saw a zero UpgradeStartedAt forever).
					entry.UpgradeStartedAt = h.UpgradeStartedAt
				}
			}
			// A true Upgrading flag must ALWAYS carry a start time. The chain
			// above only carries the clock forward for HUB-armed upgrades
			// (h.UpgradeTarget != ""); a spoke-REPORTED upgrade left the
			// rebuilt entry with a zero clock every beat, so the "Upgrading"
			// badge rendered without its elapsed counter and the stuck-upgrade
			// alert (which gates on a non-zero clock) was blind. Carry the
			// previous beat's clock forward when there is one — re-stamping a
			// live clock would repeat the #2725 reset-every-retry bug — and
			// stamp now only when no clock has ever existed.
			stampObservedUpgrade(&entry, h.UpgradeStartedAt)
			// Sparkline history: sample every 15 min, keep 7 days (672 points)
			const sparkSampleInterval = 15 * 60 // seconds
			const sparkMaxPoints = 672
			now := time.Now().Unix()
			entry.IssueHistory = h.IssueHistory
			entry.PRHistory = h.PRHistory
			shouldSample := len(h.IssueHistory) == 0 || (now-h.IssueHistory[len(h.IssueHistory)-1].T) >= sparkSampleInterval
			if shouldSample {
				entry.IssueHistory = append(entry.IssueHistory, SparkPoint{T: now, V: entry.ActionableIssues})
				entry.PRHistory = append(entry.PRHistory, SparkPoint{T: now, V: entry.ActionablePRs})
				if len(entry.IssueHistory) > sparkMaxPoints {
					entry.IssueHistory = entry.IssueHistory[len(entry.IssueHistory)-sparkMaxPoints:]
				}
				if len(entry.PRHistory) > sparkMaxPoints {
					entry.PRHistory = entry.PRHistory[len(entry.PRHistory)-sparkMaxPoints:]
				}
			}
			s.registry.Hives[i] = entry
			found = true
			// Clear stale "error" status on SaaS hives that are actively heartbeating.
			if strings.HasPrefix(payload.HiveID, "hosted-") {
				if sh := loadSaaSHive(payload.HiveID); sh != nil && sh.Status == "error" {
					sh.Status = "running"
					sh.Error = ""
					_ = saveSaaSHive(sh)
				}
			}
			break
		}
	}
	const maxRegistryEntries = 200
	if !found {
		if strings.HasPrefix(payload.HiveID, "hosted-") || strings.HasPrefix(payload.HiveID, "saas-") {
			if loadSaaSHive(payload.HiveID) == nil {
				s.mu.Unlock()
				http.Error(w, "hosted/saas hive IDs can only be created via provisioning", http.StatusForbidden)
				return
			}
		}
		if len(s.registry.Hives) >= maxRegistryEntries {
			s.mu.Unlock()
			http.Error(w, "registry full", http.StatusServiceUnavailable)
			return
		}
		entry.RegisteredAt = time.Now().UTC().Format(time.RFC3339)
		now := time.Now().Unix()
		entry.IssueHistory = []SparkPoint{{T: now, V: entry.ActionableIssues}}
		entry.PRHistory = []SparkPoint{{T: now, V: entry.ActionablePRs}}
		// A brand-new hive whose FIRST heartbeat already reports Upgrading has
		// no previous beat to carry a clock from — stamp it now so the badge
		// never renders without its elapsed counter.
		stampObservedUpgrade(&entry, time.Time{})
		s.registry.Hives = append(s.registry.Hives, entry)
	}
	s.registry.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()

	// Timeline: record whatever changed on this beat. Nothing is written when
	// nothing changed, which is the common case — at ~2 min per beat and 42
	// hives, an event per beat would be pure noise.
	if hadPrev {
		s.recordHeartbeatTransitions(&prevEntry, &entry)
	} else {
		s.recordTimeline(entry.ID, TimelineCameOnline, "hive registered with the hub for the first time", "")
	}

	// Sample the fleet-total token trend. Deliberately AFTER s.mu.Unlock():
	// sampleUsageHistory takes s.mu.RLock itself, and s.mu is a
	// non-reentrant sync.RWMutex, so calling it while the write lock is held
	// would deadlock every heartbeat.
	s.sampleUsageHistory(time.Now())

	s.requestSave()

	// Credit per-user "time in hive" from the spoke's active-session report.
	// Deliberately AFTER s.mu.Unlock() (like the timeline/token-trend work above):
	// it does per-user load-modify-write file I/O and must not serialize behind
	// the registry write lock. hadPrev==false (a brand-new hive) credits nothing —
	// there is no prior sample point to measure an interval against.
	s.creditActiveSessionTime(&prevEntry, hadPrev, payload.ActiveSessionUsers, payload.EngagedSessionUsers)

	// Fold the spoke's per-user last-real-action timestamps into the user
	// records. Unlike time crediting this needs no prior beat — a timestamp is
	// absolute — so it runs even on a hive's first heartbeat. Also after
	// Unlock(): per-user file I/O.
	s.applyUserLastActions(payload.UserLastActions)

	// Store heartbeat-reported cluster health so the hub can use it as a
	// fallback when it cannot reach the cluster directly via kubectl.
	if payload.ClusterHealth != nil && entry.ClusterID != "" {
		s.heartbeatHealthMu.Lock()
		s.heartbeatHealth[entry.ClusterID] = &HeartbeatHealthEntry{
			Report:     payload.ClusterHealth,
			ReceivedAt: time.Now(),
		}
		s.heartbeatHealthMu.Unlock()
		s.logger.Debug("stored heartbeat cluster health",
			"hive_id", payload.HiveID,
			"cluster_id", entry.ClusterID,
			"nodes", len(payload.ClusterHealth.Nodes),
		)
	}

	// Check if a pending webhook matches this heartbeat's pending install flag.
	if payload.PendingGitHubAppInstall {
		s.checkPendingWebhookMatch(payload.Org, payload.HiveID)
	}

	s.logger.Info("audit: hub heartbeat received",
		"hive_id", payload.HiveID,
		"org", payload.Org,
		"acmm_level", payload.ACMMLevel,
		"agents", len(payload.Agents),
	)

	// Build the heartbeat response with version info for all spokes.
	branch := payload.GitBranch
	if branch == "" {
		branch = "v2"
	}
	latestSHA := getLatestSHAForBranch(branch)
	resp := HeartbeatResponse{
		OK:         true,
		HubGitHash: s.hubGitHash,
		LatestSHA:  latestSHA,
	}

	if entry.IsPublic != payload.IsPublic {
		resp.IsPublic = &entry.IsPublic
	}

	// Deliver the hub's authoritative access list so heartbeat-only spokes
	// (which authorize their own logins) stay in sync with Manage Access grants.
	// Only for hives the hub actually has a SaaS record for; a bare non-SaaS
	// spoke gets nothing and keeps its own configured allowlist.
	if authUsers, authNames := authorizedUsersAndNamesForHiveID(payload.HiveID); authUsers != nil {
		resp.AuthorizedUsers = authUsers
		resp.AuthorizedUserNames = authNames
	}

	// Deliver the claimed project's real org/repos/ACMM so a spoke that was
	// assigned a pre-provisioned placeholder reconciles its running project
	// config. Mirrors AuthorizedUsers: the hub keeps sending it every beat
	// until the spoke reports the matching project. This is the ONLY channel
	// that reaches heartbeat-only clusters (the heartbeat-only cluster) — the hub cannot push
	// config there over kubectl. A nil return means "spoke already matches, or
	// no SaaS record / still a placeholder" — send nothing.
	// Org/repos/primary_repo/ACMM are operator-controlled on the spoke dashboard
	// once a hive is claimed, so the spoke's reported values are authoritative:
	// adopt them into meta every beat. This runs BEFORE the reconcile below,
	// which now only delivers the vanity URL. Without it, an operator's dashboard
	// edit (repos or level) is reverted every heartbeat — the spyre / Joe Runde
	// bug. (No-op for unclaimed placeholders and for empty/zero reported values.)
	s.adoptSpokeProjectConfig(payload.HiveID, payload.Org, payload.Repos, payload.PrimaryRepo, clampInt(payload.ACMMLevel, 0, 6))

	// FORGE IS PER-HIVE: a hive's forge comes from its own recorded github_host
	// (empty = public github.com), never from its cluster's default — a cluster
	// hosts hives of both forges. The per-beat blank-fill-from-cluster and the
	// cluster-keyed "GHE guard" that used to run here are gone: on 2026-08-05
	// the guard rewrote github_host on every heartbeat-only-cluster hive whose spoke reported
	// the public App — github.com projects included — and 9 spokes were flipped
	// onto an App that 404'd every token mint.
	//
	// Repair a persisted host that is set but WRONG from the forge the spoke
	// actually reports it runs against (public→GHE only, never demoting a legit
	// public pin, and never trusting a spoke whose App auth is failing). Closes
	// the vllmd-06 class: meta said github.com while the spoke ran api_url
	// github.ibm.com.
	s.reconcileGitHubHostFromSpoke(&payload)
	// Restore a github_host the cluster-keyed guard STOMPED: a claimed hive
	// whose provision request explicitly records public github.com, whose meta
	// now says GHE, and whose spoke is failing App auth on that GHE forge, gets
	// its recorded host set back to github.com — after which the per-hive
	// identity reconcile below re-delivers its public App on this same beat.
	// No-op (and silent) for every hive not in that exact state.
	s.repairMisflippedForgeFromRequest(&payload)

	// Close the FORGE handshake before building the project config, so the beat
	// on which the spoke first reports the requested host is also the beat the
	// push stops. Ordering matters for the same reason it does above: doing this
	// after projectConfigForHiveID would re-send an api_url that has already
	// landed. No-op (and silent) for every hive with no outstanding switch,
	// which is all of them until an operator runs one.
	s.adoptSpokeForge(payload.HiveID, payload.GitHubHost)

	// Retroactively mint the vanity host for a hive CLAIMED BEFORE the vanity
	// feature existed. VanityURL is otherwise written only at assign time, so
	// those hives would display and open their raw placeholder host forever.
	// No-op for unclaimed placeholders and hives that already have a vanity URL.
	//
	// ASYNC, deliberately. The repair shells out to kubectl against the hive's
	// cluster (existingVanityHost, then addVanityHostToIngress), and on a
	// cluster the hub cannot reach (the heartbeat-only cluster is heartbeat-only/firewalled) each
	// call blocks until its TCP dial times out — ~90s per beat observed live.
	// The spoke's heartbeat client gives up after heartbeatTimeout (10s), so
	// running the repair synchronously here meant EVERY response built below —
	// including the claimed-project config that is the ONLY channel by which an
	// assignment can reach a heartbeat-only spoke — was written to a connection
	// the spoke had already abandoned. The claim never landed, the spoke kept
	// reporting its placeholder org, and the assignStuckResetTimeout sweep
	// auto-reset the assignment at 15m, every time (the vllmd-14 wedge; EPM and
	// vllmd-13 before it). Backgrounding the repair trades the same-beat URL
	// push (the URL now lands on a later beat, once minted) for a response the
	// spoke actually receives — and on unreachable clusters the repair never
	// succeeded anyway.
	s.kickVanityURLRepairAsync(payload.HiveID)

	if projCfg := projectConfigForHiveID(payload.HiveID, payload.Org, payload.Repos, payload.PrimaryRepo, payload.ACMMLevel, payload.DashboardURL, payload.GitHubAPIURL); projCfg != nil {
		resp.ProjectConfig = projCfg
		s.logger.Info("heartbeat: delivering claimed project config to spoke",
			"hive_id", payload.HiveID,
			"org", projCfg.Org,
			"primary_repo", projCfg.PrimaryRepo,
			"acmm_level", projCfg.ACMMLevel,
		)
	}

	// Deliver a funded OpenRouter gateway to a firewalled/heartbeat-only spoke.
	// Re-offered until the spoke REPORTS the gateway back, then cleared. The key
	// value is never logged. Draining on send instead made this at-most-once for
	// something already paid for — see pendingGatewayFor.
	if gw := s.pendingGatewayFor(payload.HiveID, payload.GatewayNames); gw != nil {
		resp.PendingGateway = gw
		s.logger.Info("heartbeat: delivering funded gateway to spoke",
			"hive_id", payload.HiveID, "gateway", gw.Name, "default_model", gw.DefaultModel)
	}

	// Check if the hive should self-upgrade via heartbeat response.
	//
	// Three upgrade paths, all controlled by a single toggle:
	//   1. Spoke-managed: ConfigMap auto_upgrade=true, hub has no SaaS record.
	//      Hub sends UpgradeTo in every heartbeat when SHA differs.
	//   2. Hub-managed (kubectl reachable): SaaS AutoUpgrade=true.
	//      triggerAutoUpgrades() does "kubectl rollout restart". No UpgradeTo
	//      sent — avoids pod dying before replacement is Ready (503 window).
	//   3. Hub-managed (kubectl unreachable): SaaS AutoUpgrade=true but
	//      kubectl failed. triggerAutoUpgrades() adds hive to
	//      s.heartbeatUpgrade; the next heartbeat sends UpgradeTo as fallback.
	spokeManaged := payload.AutoUpgrade
	saasHive := loadSaaSHive(payload.HiveID)
	if saasHive != nil && saasHive.AutoUpgrade {
		spokeManaged = false
	}

	// Admin kill switch (upgrade_pause.go): while spoke upgrades are paused,
	// this handler must not DELIVER any image change — no UpgradeTo, no
	// SwitchToTag, no tracked-channel re-arm. Completion CLEARING below still
	// runs (a spoke that already landed somewhere must not stay latched), and
	// armed targets are left in place so resuming delivers them again.
	spokePauseSw, spokeUpgradesPausedNow := s.spokeUpgradesPaused()

	// Pending branch switch (image-tag change) delivered via heartbeat for
	// clusters the hub can't kubectl-reach. Takes precedence over a plain
	// SHA upgrade. Cleared when the spoke reports running that tag's build.
	s.mu.RLock()
	switchTag := s.heartbeatSwitchTag[payload.HiveID]
	hbTarget := s.heartbeatUpgrade[payload.HiveID]
	s.mu.RUnlock()
	// Durable channel tracking: heartbeatSwitchTag is in-memory, so a hub
	// restart mid-switch silently dropped the delivery and stranded the hive
	// on its old tag while the pill kept promising the channel (the persisted
	// tracked_channel survives; the directive did not). Re-arm from the
	// hive record whenever the spoke's REPORTED image tag disagrees with its
	// tracked channel. Guards: a reported ImageRef (unknown means we cannot
	// tell drift from a stale cache — do not roll pods on a guess) and a
	// non-upgrading spoke (mid-roll, the old pod still reports the old tag;
	// re-arming would stamp a fresh restart-at annotation and re-roll the
	// pod every beat). Also self-heals the delivered-upgrade-overwrote-the-
	// channel-tag drift, not just hub restarts.
	// The pause guard leads: a re-arm IS a delivery decision (the very next
	// branch would put the tag on the wire), and the aggressive self-healing
	// here is exactly what an admin reaching for the kill switch needs stopped.
	if !spokeUpgradesPausedNow && switchTag == "" && saasHive != nil && isReleaseChannel(saasHive.TrackedChannel) &&
		!payload.Upgrading && payload.ImageRef != "" &&
		imageTagOf(sanitizeImageRef(payload.ImageRef)) != saasHive.TrackedChannel {
		switchTag = saasHive.TrackedChannel
		s.mu.Lock()
		s.heartbeatSwitchTag[payload.HiveID] = switchTag
		s.mu.Unlock()
		s.logger.Info("heartbeat: re-armed channel switch from tracked_channel",
			"hive_id", payload.HiveID, "channel", switchTag,
			"reported_image", payload.ImageRef)
	}
	if switchTag != "" {
		// Clear once the spoke reports it runs the target tag. The reported
		// deployment ImageRef is the authoritative signal and works for every
		// switch shape — "<branch>-latest" AND bare channel tags ("stable"),
		// whose completion the branch inference below can never detect: a
		// channel image heartbeats its baked-in branch ("v4"), which maps to
		// "v4-latest", not "stable", so a completed channel switch would be
		// re-instructed forever. The branch inference stays as the fallback
		// for older spokes whose heartbeat omits image_ref.
		switchDone := payload.ImageRef != "" && imageTagOf(payload.ImageRef) == switchTag
		if switchDone || (payload.GitBranch != "" && branchToTag(payload.GitBranch)+"-latest" == switchTag) {
			s.mu.Lock()
			delete(s.heartbeatSwitchTag, payload.HiveID)
			for i := range s.registry.Hives {
				if s.registry.Hives[i].ID == payload.HiveID {
					s.clearUpgradeLatch(i)
					break
				}
			}
			s.mu.Unlock()
			s.logger.Info("heartbeat: spoke branch switch complete",
				"hive_id", payload.HiveID, "branch", payload.GitBranch)
		} else if spokeUpgradesPausedNow {
			// Kill switch: keep the armed tag but do not put it on the wire.
			s.logger.Debug("heartbeat: switch instruction withheld — spoke upgrades are paused",
				"hive_id", payload.HiveID, "tag", switchTag,
				"paused_by", spokePauseSw.By, "paused_at", spokePauseSw.At)
		} else {
			resp.SwitchToTag = switchTag
			s.logger.Info("heartbeat: instructing spoke to switch branch image",
				"hive_id", payload.HiveID, "tag", switchTag)
		}
	}
	if hbTarget != "" && (sameCommit(payload.GitHash, hbTarget) || (latestSHA != "" && sameCommit(payload.GitHash, latestSHA)) || commitAtOrAheadOfTarget(payload.GitHash, hbTarget, s.logger)) {
		// Spoke already at the target, at latest, or at a commit AHEAD of the
		// target — clear the fallback entry. sameCommit tolerates short/full
		// SHA length mismatch (hub stores a 7-char short SHA; a spoke may
		// report a longer one) so a hive already at HEAD is not told to
		// upgrade forever. The at-or-ahead ancestry check is what actually
		// drains a STALE pin for a floating-tag hive: its re-pull landed past
		// the target while the branch advanced again, so its hash matches
		// neither target nor latest, and without this the hub re-sends the
		// same unreachable UpgradeTo on every beat — the spoke re-rolls its
		// pod and re-latches "Upgrading" forever (the vllmd-13 wedge).
		s.mu.Lock()
		delete(s.heartbeatUpgrade, payload.HiveID)
		s.mu.Unlock()
		hbTarget = ""
	}

	// Chase-latest target for a spoke-managed hive, resolved through the tag
	// its Deployment actually tracks (#5994). A spoke on :stable can only ever
	// land on the digest :stable carries, so answering every beat with branch
	// HEAD told 90 of 97 spokes to reach a commit the soak policy is
	// deliberately withholding — and re-told them on the next beat, forever.
	//
	// resp.LatestSHA above is deliberately NOT changed: it answers "how far has
	// this branch moved", which the dashboard's behind-count needs, and is a
	// different question from "what can this spoke reach".
	//
	// Computed before the chain rather than inside it so the fall-through order
	// is unchanged: a spoke-managed hive that needs no upgrade still yields to
	// the armed kubectl-fallback target below, exactly as it did when this was
	// a single inline condition.
	spokeManagedTarget, spokeManagedChannel := "", ""
	if spokeManaged && !spokeUpgradesPausedNow && payload.GitHash != "" {
		trackedChannel := ""
		if saasHive != nil {
			trackedChannel = saasHive.TrackedChannel
		}
		reach := s.reachableUpgradeTarget(branch, payload.ImageRef, trackedChannel)
		switch {
		case !reach.Resolved:
			// The spoke tracks a channel we could not resolve. Instruct nothing
			// rather than guessing branch HEAD — see reachableUpgradeTarget.
			s.logger.Warn("heartbeat: upgrade instruction withheld — the spoke's release channel did not resolve to a commit",
				"hive_id", payload.HiveID, "channel", reach.Channel, "branch", branch,
				"image_ref", payload.ImageRef)
		case reach.SHA == "" || sameCommit(payload.GitHash, reach.SHA):
			// Nothing verified to move to, or already there.
		case reach.Channel != "" && commitAtOrAheadOfTarget(payload.GitHash, reach.SHA, s.logger):
			// Ahead of the channel it tracks — instructing it would downgrade.
			s.logger.Debug("heartbeat: no upgrade — spoke is at or ahead of its release channel",
				"hive_id", payload.HiveID, "channel", reach.Channel,
				"current", payload.GitHash, "channel_sha", reach.SHA)
		default:
			spokeManagedTarget, spokeManagedChannel = reach.SHA, reach.Channel
		}
	}

	if spokeUpgradesPausedNow {
		// Kill switch: no UpgradeTo of any flavour — spoke-managed chase-latest
		// or the armed kubectl fallback. Heartbeat is otherwise answered
		// normally; the armed target survives for resume.
		if hbTarget != "" || spokeManaged {
			s.logger.Debug("heartbeat: upgrade instruction withheld — spoke upgrades are paused",
				"hive_id", payload.HiveID,
				"paused_by", spokePauseSw.By, "paused_at", spokePauseSw.At)
		}
	} else if spokeManagedTarget != "" {
		resp.UpgradeTo = spokeManagedTarget
		s.logger.Info("heartbeat: instructing spoke-managed hive to upgrade",
			"hive_id", payload.HiveID,
			"from", payload.GitHash,
			"to", spokeManagedTarget,
			"channel", spokeManagedChannel,
		)
	} else if hbTarget != "" && !sameCommit(payload.GitHash, hbTarget) {
		resp.UpgradeTo = hbTarget
		s.logger.Info("heartbeat: instructing hub-managed hive to upgrade (kubectl fallback)",
			"hive_id", payload.HiveID,
			"from", payload.GitHash,
			"to", hbTarget,
		)
	}

	if s.restartSpokeForHeartbeat(payload.HiveID) {
		resp.RestartSpoke = true
		s.logger.Info("heartbeat: delivering spoke restart", "hive_id", payload.HiveID, "reporter", payload.Reporter)
	}

	if ghCfg := s.pendingAppIdentityForHeartbeat(&payload); ghCfg != nil {
		resp.GitHubAppConfig = ghCfg
		s.logger.Info("heartbeat: delivering github app config to spoke",
			"hive_id", payload.HiveID,
			"app_id", ghCfg.AppID,
			"installation_id", ghCfg.InstallationID,
		)
	} else if keyCfg := s.appKeySyncForHeartbeat(&payload); keyCfg != nil {
		// No one-shot config queued (webhook install, manual push), so fall
		// through to the standing per-cluster reconcile: a spoke holding the
		// wrong App key — or none — is corrected onto its cluster's key. This is
		// deliberately the LOWER-priority branch: a config queued for a specific
		// hive is a targeted operator/webhook action and must not be displaced
		// by the fleet-wide default.
		resp.GitHubAppConfig = keyCfg
	}

	// Optional SECOND App key (#4815), targeted at this hive alone.
	//
	// Runs INDEPENDENTLY of the two branches above rather than inside either,
	// because the primary reconcile's steady state is "push nothing" — a healthy
	// hive already holding the right primary key gets no GitHubAppConfig at all,
	// and nesting this there would make the secondary key undeliverable to
	// exactly the healthy hives that are supposed to receive it. It attaches to
	// whatever config those branches produced, or creates an otherwise-empty one
	// whose zero AppID/InstallationID and empty PrivateKey the spoke reads as
	// "not speaking to those fields".
	//
	// Every hive without a SecondaryAppID — the whole fleet today — returns nil
	// from the resolver and this block is a no-op: no new field, no new state,
	// no change to the payload.
	if secCfg := s.secondaryAppKeyForHeartbeat(&payload); secCfg != nil {
		if resp.GitHubAppConfig == nil {
			resp.GitHubAppConfig = &HeartbeatGitHubAppConfig{}
		}
		resp.GitHubAppConfig.SecondaryKey = secCfg
	}

	// SECURITY (C1/N3, CWE-200/639): the fleet-wide additional-key broadcast that
	// once ran here was removed. It attached every OTHER tenant's App private key
	// to every heartbeat, and — because the handler trusts the body-supplied
	// hive_id (all hives share one bearer) — let any caller pull the whole fleet's
	// keys by asserting any hive_id. The spoke's PRIMARY (cluster) key and any
	// targeted operator/webhook identity are delivered above via
	// appKeySyncForHeartbeat and pendingAppIdentityForHeartbeat; a hive is never
	// handed a key for an App it is not currently assigned. See the removed
	// attachMissingAppKeys/additionalAppKeysForSpoke in cluster_app_key.go.

	s.hubBannersMu.RLock()
	if banner, ok := s.hubBanners[payload.HiveID]; ok {
		resp.HubBanner = &HubBanner{
			ID:      banner.ID,
			Message: banner.Message,
			Color:   banner.Color,
		}
	}
	s.hubBannersMu.RUnlock()

	// User-journey nudge. An admin-sent banner always wins: a human took the
	// trouble to target this hive, so an automated reminder must not displace
	// it. Journey nudges therefore only fill an empty slot (or replace a
	// previous journey banner, which is this subsystem's own to update).
	if resp.HubBanner == nil || isJourneyBanner(resp.HubBanner.ID) {
		if jb := s.journeyBannerFor(payload.HiveID, time.Now()); jb != nil {
			resp.HubBanner = jb
		}
	}

	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(resp)
	_, _ = w.Write(data)
}

const (
	// expectedHeartbeatSecs mirrors cmd/hive's heartbeatSendInterval (2 min). It
	// is redeclared here because that constant lives in package main and cannot be
	// imported; the value is the sampling cadence for session-time accounting.
	expectedHeartbeatSecs = 120.0
	// maxSessionCreditSecs caps how much time a single beat may credit a user, so
	// a hub or spoke outage (a long gap between two beats) never back-credits hours
	// of "time in hive" that nobody was actually logged in for. Two beats' worth is
	// generous enough to absorb ordinary jitter while bounding the fault.
	maxSessionCreditSecs = 2 * expectedHeartbeatSecs
	// maxActiveSessionUsers bounds the per-beat work from a spoke that reports an
	// implausibly large active-session list (defensive; a real hive has a handful).
	maxActiveSessionUsers = 200
)

// creditActiveSessionTime accumulates per-user "time in hive" from one heartbeat's
// active-session report. Each distinct, valid username that had a live session on
// this beat is credited the elapsed time since this hive's PREVIOUS beat — the
// sampling interval — clamped so an outage gap is never back-credited.
//
// Correctness:
//   - hadPrev==false (first-ever beat) → no prior sample point → credit nothing.
//   - the interval is (now - prevEntry.LastHeartbeat); keyed off the PERSISTED
//     registry LastHeartbeat, so a hub restart resumes from the last stored beat
//     and the next beat credits only the (clamped) gap — never a giant catch-up.
//   - unknown users (no SaaSUser record) are skipped, never auto-created.
//   - load-modify-write per user so a concurrent contact/quota edit is not clobbered.
//
// engagedUsers is the subset of activeUsers whose browser reported focused,
// recent-input presence (heartbeat EngagedSessionUsers). Those users are
// additionally credited EngagedSeconds and stamped LastEngagedAt — the honest
// engagement signals — while SessionSeconds keeps its back-compat open-tab
// meaning. A nil/empty engagedUsers (old spoke, or genuinely nobody engaged)
// credits engaged time to no one and is never an error.
//
// It runs post-unlock and does its own file I/O; callers must NOT hold s.mu.
func (s *HubServer) creditActiveSessionTime(prevEntry *RegistryEntry, hadPrev bool, activeUsers, engagedUsers []string) {
	// Mark everyone reported active as live NOW, independent of the time-credit
	// path below: the "logged in right now" avatar treatment must light up on the
	// FIRST beat that sees a session (there is no prior sample yet), and must not
	// depend on a valid prevEntry. Freshness is applied on read.
	s.markUsersLive(activeUsers)
	s.markUsersEngaged(engagedUsers)
	if !hadPrev || prevEntry == nil || prevEntry.LastHeartbeat == "" || len(activeUsers) == 0 {
		return
	}
	prev, err := time.Parse(time.RFC3339, prevEntry.LastHeartbeat)
	if err != nil {
		return
	}
	elapsed := time.Since(prev).Seconds()
	if elapsed <= 0 {
		return // clock skew or a duplicate beat at the same instant — credit nothing.
	}
	if elapsed > maxSessionCreditSecs {
		elapsed = maxSessionCreditSecs
	}
	creditSecs := int64(elapsed)
	if creditSecs <= 0 {
		return
	}
	// Dedupe + validate the reported usernames with the same gate the leaderboard
	// path uses, and bound the work.
	engaged := make(map[string]struct{}, len(engagedUsers))
	for _, name := range engagedUsers {
		engaged[name] = struct{}{}
	}
	nowRFC3339 := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[string]struct{}, len(activeUsers))
	for _, name := range activeUsers {
		if len(seen) >= maxActiveSessionUsers {
			break
		}
		if name == "" || !isValidName(name) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		u := loadSaaSUser(name)
		if u == nil {
			continue // an active session for a user the hub has no record of — skip.
		}
		u.SessionSeconds += creditSecs
		// Engaged time accrues ONLY for users whose browser proved a human is
		// there this beat. An idle/hidden tab therefore keeps accumulating
		// session_seconds (back-compat) but never engaged_seconds.
		if _, isEngaged := engaged[name]; isEngaged {
			u.EngagedSeconds += creditSecs
			u.LastEngagedAt = nowRFC3339
		}
		if err := saveSaaSUser(u); err != nil {
			s.logger.Warn("creditActiveSessionTime: save failed", "user", name, "error", err)
		}
	}
}

// applyUserLastActions folds a heartbeat's per-user last-real-action
// timestamps (spoke audit log; see HeartbeatPayload.UserLastActions) into the
// user records, keeping the MAXIMUM per user so an older spoke's replayed log,
// a spoke restart, or a second hive can never move a user's LastActionAt
// backwards. Unknown users are skipped, never auto-created. nil/empty input
// (old spoke) is a no-op. Does per-user file I/O; callers must NOT hold s.mu.
func (s *HubServer) applyUserLastActions(lastActions map[string]string) {
	if len(lastActions) == 0 {
		return
	}
	applied := 0
	for name, ts := range lastActions {
		if applied >= maxActiveSessionUsers {
			break
		}
		if name == "" || !isValidName(name) {
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		applied++
		u := loadSaaSUser(name)
		if u == nil {
			continue
		}
		if u.LastActionAt != "" {
			if prev, err := time.Parse(time.RFC3339, u.LastActionAt); err == nil && !t.After(prev) {
				continue
			}
		}
		u.LastActionAt = t.UTC().Format(time.RFC3339)
		if err := saveSaaSUser(u); err != nil {
			s.logger.Warn("applyUserLastActions: save failed", "user", name, "error", err)
		}
	}
}

// liveHiveUserStaleness is how long after a hive's last active-session report a
// user is still considered "logged into their hive". Two beats absorbs one missed
// heartbeat before the avatar treatment drops.
const liveHiveUserStaleness = time.Duration(maxSessionCreditSecs) * time.Second

// markUsersLive stamps each reported active user as live as of now. Cheap map
// write under its own mutex; validation/freshness are applied on read.
func (s *HubServer) markUsersLive(activeUsers []string) {
	if len(activeUsers) == 0 {
		return
	}
	now := time.Now()
	s.liveHiveUsersMu.Lock()
	if s.liveHiveUsers == nil {
		s.liveHiveUsers = make(map[string]time.Time)
	}
	for _, name := range activeUsers {
		if name == "" || !isValidName(name) {
			continue
		}
		s.liveHiveUsers[name] = now
	}
	s.liveHiveUsersMu.Unlock()
}

// markUsersEngaged is markUsersLive's twin for the ENGAGED set (focused tab +
// recent input, per the spoke's presence report). Same mutex, same validation,
// same freshness-on-read model.
func (s *HubServer) markUsersEngaged(engagedUsers []string) {
	if len(engagedUsers) == 0 {
		return
	}
	now := time.Now()
	s.liveHiveUsersMu.Lock()
	if s.engagedHiveUsers == nil {
		s.engagedHiveUsers = make(map[string]time.Time)
	}
	for _, name := range engagedUsers {
		if name == "" || !isValidName(name) {
			continue
		}
		s.engagedHiveUsers[name] = now
	}
	s.liveHiveUsersMu.Unlock()
}

// liveHiveUsernames returns the usernames with a live hive session seen within
// the staleness window, and opportunistically prunes stale entries so the map
// cannot grow without bound. Admin-facing (presence data) — the caller gates it.
func (s *HubServer) liveHiveUsernames() []string {
	cutoff := time.Now().Add(-liveHiveUserStaleness)
	s.liveHiveUsersMu.Lock()
	defer s.liveHiveUsersMu.Unlock()
	out := make([]string, 0, len(s.liveHiveUsers))
	for name, seen := range s.liveHiveUsers {
		if seen.Before(cutoff) {
			delete(s.liveHiveUsers, name)
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// engagedHiveUsernames returns the usernames reported ENGAGED (focused +
// recent input) within the staleness window — liveHiveUsernames' honest
// subset. Same pruning-on-read; admin-facing, the caller gates it.
func (s *HubServer) engagedHiveUsernames() []string {
	cutoff := time.Now().Add(-liveHiveUserStaleness)
	s.liveHiveUsersMu.Lock()
	defer s.liveHiveUsersMu.Unlock()
	out := make([]string, 0, len(s.engagedHiveUsers))
	for name, seen := range s.engagedHiveUsers {
		if seen.Before(cutoff) {
			delete(s.engagedHiveUsers, name)
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// isUserEngagedNow reports whether username has a fresh engaged-presence
// report — the "connected AND a human is actually there" gate for the `live`
// status tier.
func (s *HubServer) isUserEngagedNow(username string) bool {
	cutoff := time.Now().Add(-liveHiveUserStaleness)
	s.liveHiveUsersMu.RLock()
	defer s.liveHiveUsersMu.RUnlock()
	seen, ok := s.engagedHiveUsers[username]
	return ok && !seen.Before(cutoff)
}

func (s *HubServer) storePendingGitHubAppConfig(hiveID string, cfg *HeartbeatGitHubAppConfig) {
	s.pendingGitHubAppConfigsMu.Lock()
	defer s.pendingGitHubAppConfigsMu.Unlock()
	s.pendingGitHubAppConfigs[hiveID] = cfg
}

func (s *HubServer) consumePendingGitHubAppConfig(hiveID string) *HeartbeatGitHubAppConfig {
	s.pendingGitHubAppConfigsMu.Lock()
	defer s.pendingGitHubAppConfigsMu.Unlock()
	cfg, ok := s.pendingGitHubAppConfigs[hiveID]
	if !ok {
		return nil
	}
	delete(s.pendingGitHubAppConfigs, hiveID)
	return cfg
}

// registryResponse is the /api/registry wire shape: the persisted Registry plus
// read-time derivations. The embedding is deliberate — it flattens, so every
// existing field keeps its exact place and an old client sees no change.
type registryResponse struct {
	Registry
	// RepoOverlaps lists work items claimed by more than one spoke (#5691).
	// Omitted entirely when the fleet is clean, so its presence is the signal.
	RepoOverlaps []RepoOverlap `json:"repoOverlaps,omitempty"`
}

func (s *HubServer) handleRegistry(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	offlineEvents := s.markStaleHives()
	s.mu.Unlock()
	s.flushOfflineEvents(offlineEvents)
	s.mu.RLock()
	hostedNames := make(map[string]bool)
	for _, h := range s.registry.Hives {
		if h.IsPublic && h.HiveType == "hosted" {
			hostedNames[h.Name] = true
		}
	}
	filtered := Registry{UpdatedAt: s.registry.UpdatedAt}
	for _, h := range s.registry.Hives {
		if !h.IsPublic {
			continue
		}
		if h.HiveType != "hosted" && !h.Online && hostedNames[h.Name] {
			continue
		}
		filtered.Hives = append(filtered.Hives, h)
	}
	// #5691: repo-ownership overlaps, derived from the repo lists already in the
	// response. Computed over the FILTERED set so the answer is consistent with
	// the hives the caller can actually see — naming a conflict with a hive that
	// is not in the payload would be unreadable — and computed at read time
	// rather than stored, which is why it rides a response wrapper instead of
	// Registry itself: Registry is what saveRegistryNow persists, and derived
	// state has no business on disk.
	resp := registryResponse{Registry: filtered, RepoOverlaps: computeRepoOverlaps(filtered.Hives)}
	data, _ := json.Marshal(resp)
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *HubServer) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	merged := s.mergeLeaderboards()
	s.mu.RUnlock()

	data, _ := json.Marshal(map[string]any{"leaderboard": merged})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *HubServer) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	// Same derived HEARTBEAT sub-key as /api/heartbeat (task-status is the spoke's
	// other beat channel); the master secret is never accepted here.
	//
	// N1: like /api/heartbeat, the bearer is captured here and verified below,
	// once the claimed hive_id is known — the per-hive bearer is derived from it.
	presentedBearer := ""
	if s.hubSecret != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		presentedBearer = strings.TrimPrefix(auth, "Bearer ")
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var payload struct {
		HiveID       string             `json:"hive_id"`
		Leaderboard  []LeaderboardEntry `json:"leaderboard"`
		Contributors ContributorSummary `json:"contributors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	payload.HiveID = sanitizeHeartbeatField(payload.HiveID)
	if payload.HiveID == "" {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if !isValidName(payload.HiveID) {
		http.Error(w, "invalid hive_id", http.StatusBadRequest)
		return
	}
	// N1: bind the bearer to the claimed hive, same as /api/heartbeat.
	if s.hubSecret != "" {
		if !s.verifyHeartbeatBearer(presentedBearer, payload.HiveID) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// #3234: record WHICH format authenticated. The fleet-wide compat lane is
		// now DELETED (F2), so this can only ever record the per-hive format; it is
		// retained to back GET /api/saas/admin/auth-rollout, where an operator can
		// confirm no hive regressed after the cutover.
		s.noteHeartbeatAuthPath(payload.HiveID, s.heartbeatBearerIsPerHive(presentedBearer, payload.HiveID))
	}
	for i, lb := range payload.Leaderboard {
		payload.Leaderboard[i].GitHubUsername = sanitizeField(lb.GitHubUsername)
	}

	hiveName := ""
	s.mu.Lock()
	for i, h := range s.registry.Hives {
		if h.ID == payload.HiveID {
			if !h.Online {
				s.mu.Unlock()
				http.Error(w, "hive is offline — heartbeat first", http.StatusForbidden)
				return
			}
			hiveName = h.Name
			s.registry.Hives[i].ContributorCount = payload.Contributors.Registered
			s.registry.Hives[i].ActiveContributors = payload.Contributors.Active
			for j := range payload.Leaderboard {
				payload.Leaderboard[j].HiveName = hiveName
			}
			s.registry.Hives[i].Leaderboard = payload.Leaderboard
			break
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *HubServer) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	totalAgents := 0
	totalContributors := 0
	totalIssues := 0
	totalPRs := 0
	onlineCount := 0
	publicCount := 0
	for _, h := range s.registry.Hives {
		if !h.IsPublic {
			continue
		}
		publicCount++
		totalAgents += h.AgentCount
		totalContributors += h.ActiveContributors
		totalIssues += h.ActionableIssues
		totalPRs += h.ActionablePRs
		if h.Online {
			onlineCount++
		}
	}
	s.mu.RUnlock()

	data, _ := json.Marshal(map[string]any{
		"hives":        publicCount,
		"online":       onlineCount,
		"agents":       totalAgents,
		"contributors": totalContributors,
		"issues":       totalIssues,
		"prs":          totalPRs,
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// FleetStats is the aggregated, fleet-wide contribution total exposed at
// GET /api/fleet-stats and rendered by the public landing page's hero-proof
// strip. It sums the spoke-reported counts across every PUBLIC, non-stale
// hive, so the numbers are a true fleet total (every managed repo of every
// live spoke) rather than a single org's figures.
type FleetStats struct {
	ReposManaged int `json:"repos_managed"`
	PRsMerged    int `json:"prs_merged"`
	PRsRejected  int `json:"prs_rejected"`
	CVEsClosed   int `json:"cves_closed"`
	// Hives is the number of ONLINE, ASSIGNED hives fleet-wide (the loop below
	// skips offline and unassigned ones) — i.e. "hives up". TotalHives is every
	// ASSIGNED hive, online or not. The landing page shows "hives up / total"
	// (Hives/TotalHives) so the ratio reflects real fleet uptime rather than the
	// public-list size, and unclaimed inventory doesn't inflate it. AvailableHives
	// is the count of UNASSIGNED placeholders a user could still request. All
	// three are anonymous scalars — no name/org/repo/owner/URL is derivable.
	Hives          int `json:"hives"`
	TotalHives     int `json:"total_hives"`
	AvailableHives int `json:"available_hives"`
	// AgentsRunning and Contributors are fleet-wide ANONYMOUS counts, summed
	// over ALL online hives rather than only the public ones.
	//
	// The landing page used to compute these in the browser from /api/registry,
	// which returns public hives only — so "agents running" and "contributors"
	// silently described about a third of the fleet (roughly 18 of 51 spokes are
	// public). Computing them here counts every hive while disclosing none: like
	// every other field on this struct they are scalars, and no name, org, repo,
	// owner or URL is derivable from them.
	AgentsRunning int `json:"agents_running"`
	Contributors  int `json:"contributors"`
	// ContributorsTotal is the fleet-wide count of REGISTERED (unique) known
	// contributors, summed from each hive's ContributorCount rather than
	// ActiveContributors. Contributors above tracks who is CURRENTLY active,
	// which legitimately drops to 0 whenever nobody happens to be connected —
	// on the hub landing page that reads as "this fleet has no
	// contributors", which is false. ContributorsTotal is stable across quiet
	// periods because a contributor stays registered whether or not they are
	// active right now, so the public tile should prefer it.
	ContributorsTotal int    `json:"contributors_total"`
	UpdatedAt         string `json:"updated_at"`
	// Reporting is the number of eligible hives that actually contributed a
	// fresh count to the totals above; Eligible is how many were considered.
	// Without this pair the totals are indistinguishable from a healthy fleet:
	// summing 2 of 50 hives yields a small, confident, WRONG number and the
	// page has no way to know. The landing page compares the two and refuses
	// to present a total assembled from too little of the fleet.
	Reporting int `json:"reporting"`
	Eligible  int `json:"eligible"`
	// Stale counts eligible hives whose newest successful collect is older
	// than fleetStatsMaxAge — surfaced so a fleet quietly failing to collect
	// is visible as degradation rather than as a shrinking total.
	Stale int `json:"stale"`
	// Trustworthy is false when too little of the fleet reported for the
	// totals to stand as a fleet-wide figure. The landing page shows a
	// "collecting" state instead of the numbers when this is false.
	Trustworthy bool `json:"trustworthy"`
	// Stale is true when the counts above did NOT come from the current
	// collection but from the persisted last-known-good aggregate. The page
	// still shows the numbers — hiding them entirely is what made a
	// recollecting fleet look like a dead one — but it must label them, so
	// this flag and AsOf travel with the payload. Stale-and-says-so is the
	// goal; wrong-and-silent is the thing to avoid.
	StaleData bool `json:"stale_data,omitempty"`
	// AsOf is the RFC3339 time the returned counts were actually collected.
	// Equal to UpdatedAt for a fresh aggregate; older when StaleData is true.
	AsOf string `json:"as_of,omitempty"`
}

// fleetStatsMaxAge is how old a spoke's last successful collect may be and
// still count toward the public total. The spoke recollects every 30 minutes,
// so a contribution older than this means that hive's collector has been
// failing for many consecutive cycles and its number is no longer current.
// Generous enough to ride out a restart or a transient GitHub outage without
// blanking the strip, short enough that a genuinely broken collector drops out
// rather than freezing a stale figure on the public page forever.
const fleetStatsMaxAge = 6 * time.Hour

// isAvailableRegistryEntry reports whether a registry entry is a genuinely
// unclaimed pool placeholder — the "available"/"unassigned" bucket on the
// landing page and dashboard. It mirrors the frontend's isPlaceholderHive and
// drift.go's isPlaceholderEntry EXACTLY:
//
//	provStatus == "available"  OR  org has the "available-" prefix
//
// The "hosted-available-" ID prefix is deliberately NOT consulted. A claimed
// placeholder KEEPS that ID forever (it is minted at pool-creation time and
// never rewritten on assignment), so an ID-prefix fallback counted every
// claimed placeholder as still available — 38 reported vs the true 17. The
// claim paths rewrite the org OFF the "available-" prefix and now set an
// explicit statusAssigned, so both remaining signals flip correctly on claim.
// Keeping this in lockstep with the JS and drift.go is what lets the landing
// tiles, the dashboard's Assigned/Unassigned split, and server-side drift never
// disagree about what is claimed.
//
// statusAssigned short-circuits BEFORE the org-prefix fallback. Org here is
// spoke-reported and keeps reading "available-<id>" until the spoke adopts the
// pushed config on a later heartbeat, so the fallback alone counted a
// freshly-approved hive as inventory for that whole window — parking it under
// "Unassigned hives" right after an approval.
func isAvailableRegistryEntry(h RegistryEntry) bool {
	if h.ProvStatus == statusAvailable {
		return true
	}
	if h.ProvStatus == statusAssigned {
		return false
	}
	return strings.HasPrefix(h.Org, placeholderOrgPrefix)
}

// computeFleetStats aggregates fleet-wide contribution counts across public,
// non-stale hives. A hive that never reported a given count (nil pointer) is
// skipped for that count — the aggregate reflects only hives with real data,
// so a not-yet-computed zero is never fabricated into the total. Distinct
// repos are counted across hives (deduplicated by org/repo reference) so two
// hives on the same repo don't double-count it.
//
// The PR and CVE counters are deduplicated the same way, for the same reason:
// they are ORG-scoped, so several hives in one org each report that org's whole
// output and summing them multiplies it. See fleetCountAccumulator.
//
// Caller must hold s.mu (read or write).
func (s *HubServer) computeFleetStats() FleetStats {
	var fs FleetStats
	var counts fleetCountAccumulator
	repoSet := make(map[string]struct{})
	for _, h := range s.registry.Hives {
		// An UNASSIGNED hive is a pre-provisioned placeholder nobody has claimed
		// yet. It is counted as AVAILABLE and excluded from the assigned totals
		// below: "hives up / total" and the contribution sums are about the
		// working fleet, not idle inventory a user could still request.
		//
		// A placeholder is NOT detectable by an empty Org: an unclaimed slot
		// keeps its "hosted-available-" ID prefix and frequently a leftover pool
		// org (live example: id="hosted-available-oke-01-placeholder-bb95",
		// org="TradingAsBuddies"). Testing Org=="" therefore matched zero
		// placeholders AND counted every one as assigned.
		//
		// The ID/org PREFIX alone over-counts the other way: a CLAIMED placeholder
		// KEEPS its "hosted-available-" ID and pool org after assignment (the ID is
		// minted at pool creation and never changes on claim). ~21 of the 38
		// "hosted-available-" hives are actually claimed, so the prefix reported 38
		// "available" when the truth is 17 unclaimed / 33 assigned.
		//
		// Detect it the way the rest of the codebase does — see isPlaceholderEntry
		// in drift.go and the dashboard's Assigned/Unassigned sections: the
		// authoritative signal is ProvStatus==statusAvailable, with the
		// "hosted-available-" ID prefix / "available-" org prefix used ONLY as a
		// fallback when ProvStatus is unknown (empty — a hive with no SaaSHive
		// record or one that predates the field). A claimed placeholder therefore
		// counts as ASSIGNED, not available.
		if isAvailableRegistryEntry(h) {
			fs.AvailableHives++
			continue
		}
		// TotalHives is every ASSIGNED hive (online or not) — the denominator of
		// "hives up / total". Counted before the online skip below so a down hive
		// still contributes to the total.
		fs.TotalHives++
		// ALL assigned hives count, public and private alike.
		//
		// These totals are ANONYMOUS SUMS: every field on FleetStats is a
		// scalar — counts, a timestamp, booleans. No name, org, repo, owner or
		// URL reaches this struct, and repoSet below is internal with only its
		// len() published. So a private hive contributing to a count discloses
		// nothing about itself.
		//
		// Excluding them made the published figures describe about a THIRD of
		// reality: 51 spokes exist and roughly 18 are public. "Hives", "repos
		// managed" and the PR/CVE counts all understated the fleet by that
		// margin, which is the opposite of what a fleet total is for.
		//
		// The PUBLIC LIST at the bottom of the landing page is a separate
		// concern and is deliberately untouched: it names hives and links to
		// them, so it stays opt-in via IsPublic. Aggregate counts and named
		// listings are different disclosures and are now treated as such.
		if !h.Online {
			continue
		}
		fs.Hives++
		fs.AgentsRunning += h.AgentCount
		fs.Contributors += h.ActiveContributors
		// Registered (not active) so the total does not crater to 0 whenever
		// nobody happens to be connected at collection time.
		fs.ContributorsTotal += h.ContributorCount
		for _, r := range h.Repos {
			if r == "" {
				continue
			}
			// Normalize a bare "repo" to "org/repo" so the same repo reported
			// with and without an org prefix isn't counted twice.
			key := r
			if !strings.Contains(r, "/") && h.Org != "" {
				key = h.Org + "/" + r
			}
			repoSet[key] = struct{}{}
		}
		// Only hives that could meaningfully report are counted as eligible.
		// A placeholder with no repos has nothing to contribute and must not
		// drag the reporting fraction down as though data were missing.
		if len(h.Repos) == 0 {
			continue
		}
		fs.Eligible++

		hasCount := h.PRsMerged90d != nil || h.PRsRejected90d != nil || h.CVEsClosed != nil
		if !hasCount {
			continue
		}
		// Age out contributions whose last successful collect is too old. A
		// zero timestamp means the spoke predates the field, so it is trusted
		// rather than dropped — an upgrade must not blank the strip.
		if !h.FleetStatsCollectedAt.IsZero() && time.Since(h.FleetStatsCollectedAt) > fleetStatsMaxAge {
			fs.Stale++
			continue
		}
		fs.Reporting++
		// Accumulate into the (org, ai-author) group rather than straight into
		// the total. Only the group's MAX is summed at the end, so an org-wide
		// counter reported by several hives lands in the public figure once.
		//
		// Reporting/Eligible/Stale above stay PER-HIVE on purpose: they measure
		// collection coverage ("how much of the fleet answered"), which is a
		// question about hives, not about work. Deduping them would understate
		// how much of the fleet is healthy.
		group := fleetCountGroup(h)
		counts.add(group, h.PRsMerged90d, h.PRsRejected90d, h.CVEsClosed)
	}
	fs.PRsMerged, fs.PRsRejected, fs.CVEsClosed = counts.totals()
	fs.ReposManaged = len(repoSet)
	return fs
}

// fleetCountAccumulator sums org-scoped contribution counters while collapsing
// hives that report the SAME underlying work down to a single contribution.
//
// Needed because FleetStatsCollector counts AI-author PRs across a whole ORG
// (see NewFleetStatsCollector), so every hive in an org reports that org's
// entire output as its own. Summing those with += multiplies one org's number
// by its hive count — measured live, three hives each reported prsMerged90d
// = 3746 and the public landing-page total carried 11238 for work done once.
//
// Grouping is by fleetCountGroup, the same (org, ai-author) key the quadrant's
// population dedupe uses, so the two surfaces cannot drift into disagreeing
// about which hives are duplicates of each other.
type fleetCountAccumulator struct {
	// merged/rejected/cves map a group key to the largest value seen for it.
	// Absent key means no hive in that group has reported that counter, which
	// is NOT the same as a reported zero — see add.
	merged   map[string]int
	rejected map[string]int
	cves     map[string]int
	// ungrouped holds the running sum for hives whose group is unknown. They
	// cannot be shown to duplicate anyone, so each counts on its own.
	ungroupedMerged   int
	ungroupedRejected int
	ungroupedCVEs     int
}

// add folds one reporting hive's counters in. Each counter is handled
// independently because a hive may report some and not others.
//
// nil means "this hive never computed that counter" and is skipped entirely, so
// dedupe can never turn an unreported counter into a participating zero — the
// nil-vs-zero discipline the *int pointers exist to preserve. A group whose
// members are all nil for a counter contributes nothing to it, exactly as
// before dedupe.
func (a *fleetCountAccumulator) add(group string, merged, rejected, cves *int) {
	if group == "" {
		if merged != nil {
			a.ungroupedMerged += *merged
		}
		if rejected != nil {
			a.ungroupedRejected += *rejected
		}
		if cves != nil {
			a.ungroupedCVEs += *cves
		}
		return
	}
	// Keep the MAXIMUM, not the first or the mean. The counter is org-wide and
	// identical by construction, so in the healthy case every choice agrees;
	// they differ only when the group's collectors ran at different moments,
	// and over a fixed trailing window the freshest collect is the largest.
	// Taking the max therefore lets the most complete measurement stand instead
	// of letting collection timing pick the answer.
	//
	// Staleness needs no separate group-level rule: the ageing check above runs
	// PER HIVE before this point, so a stale member is already dropped and the
	// group is represented by its surviving fresh members. A group whose
	// members are ALL stale contributes no value at all and is counted in
	// fs.Stale, which is the conservative reading — the group is excluded
	// exactly when nothing fresh remains to speak for it.
	a.merged = maxInto(a.merged, group, merged)
	a.rejected = maxInto(a.rejected, group, rejected)
	a.cves = maxInto(a.cves, group, cves)
}

// maxInto records v as group's representative if it beats what is already
// stored, lazily allocating m. A nil v records nothing, so an unreported
// counter never creates a zero-valued entry.
func maxInto(m map[string]int, group string, v *int) map[string]int {
	if v == nil {
		return m
	}
	if m == nil {
		m = make(map[string]int)
	}
	if cur, seen := m[group]; !seen || *v > cur {
		m[group] = *v
	}
	return m
}

// totals sums each group's single representative alongside the ungrouped hives.
func (a *fleetCountAccumulator) totals() (merged, rejected, cves int) {
	return a.ungroupedMerged + sumValues(a.merged),
		a.ungroupedRejected + sumValues(a.rejected),
		a.ungroupedCVEs + sumValues(a.cves)
}

func sumValues(m map[string]int) int {
	var total int
	for _, v := range m {
		total += v
	}
	return total
}

// FleetStatsTrustworthy reports whether ANY hive contributed a fresh count.
//
// It used to require half the eligible fleet, on the reasoning that "a total
// built from a small minority is worse than no total at all". That reasoning
// treats the aggregate as an ESTIMATE — a sample you extrapolate from, where
// too small a sample gives a confidently wrong answer.
//
// It is not an estimate. Every hive counts its OWN merged PRs and the loop
// above sums them with +=; ReposManaged is len(repoSet), so shared repos are
// already deduplicated. Seven hives reporting 12,819 PRs is not a guess at a
// fleet total, it is an exact count of what those seven did. The number is
// correct; only its COMPLETENESS is partial.
//
// So the honest presentation is the number plus its coverage, not silence.
// Suppressing it was strictly worse: a blank strip reads as "this fleet did
// nothing", which is the one interpretation that is actually false. The page
// now always renders the coverage alongside, so a partial total can never be
// mistaken for a complete one.
//
// This also removes a trap. The last-known-good fallback was only RECORDED
// when this returned true, and served only when it returned false — so on a
// fleet that had never once cleared the bar it was never written, and the
// fallback designed for exactly that situation had nothing to serve. The
// safety net could only be filled by the condition it existed to protect
// against. Live for months: coverage was 2/18, 7/18 and 10/50 on the day this
// was found, and the strip had been blank throughout.
func (fs FleetStats) FleetStatsTrustworthy() bool {
	return fs.Reporting > 0
}

// fleetStatsLKG returns a copy of the persisted last-known-good aggregate, or
// nil if the fleet has never produced one. A copy (not the pointer) so callers
// cannot mutate registry state they do not hold the write lock for.
func (s *HubServer) fleetStatsLKG() *FleetStatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.registry.FleetStatsLKG == nil {
		return nil
	}
	snapshot := *s.registry.FleetStatsLKG
	return &snapshot
}

// recordFleetStatsLKG stores fs as the new last-known-good aggregate and asks
// for a registry flush, so the figure survives a hub restart. Only ever called
// with an aggregate that already cleared the coverage bar — a degraded total
// must never become the fallback, or the cache would launder exactly the
// unbacked number the coverage gate exists to suppress.
func (s *HubServer) recordFleetStatsLKG(fs FleetStats, collectedAt time.Time) {
	s.mu.Lock()
	s.registry.FleetStatsLKG = &FleetStatsSnapshot{
		ReposManaged:      fs.ReposManaged,
		PRsMerged:         fs.PRsMerged,
		PRsRejected:       fs.PRsRejected,
		CVEsClosed:        fs.CVEsClosed,
		Hives:             fs.Hives,
		Reporting:         fs.Reporting,
		Eligible:          fs.Eligible,
		ContributorsTotal: fs.ContributorsTotal,
		CollectedAt:       collectedAt.UTC(),
	}
	s.mu.Unlock()
	s.requestSave()
}

func (s *HubServer) handleFleetStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	offlineEvents := s.markStaleHives()
	s.mu.Unlock()
	s.flushOfflineEvents(offlineEvents)

	s.mu.RLock()
	fs := s.computeFleetStats()
	s.mu.RUnlock()
	now := time.Now().UTC()
	fs.UpdatedAt = now.Format(time.RFC3339)
	fs.Trustworthy = fs.FleetStatsTrustworthy()

	// Last-known-good handling. A fleet total does not have to be current to be
	// useful, but it does have to say how current it is. When this collection
	// clears the coverage bar we record it as the new LKG; when it does not, we
	// serve the stored LKG and mark it stale so the page can label it. Only a
	// fleet that has never once reported well enough shows no number at all.
	if fs.Trustworthy {
		fs.AsOf = fs.UpdatedAt
		s.recordFleetStatsLKG(fs, now)
	} else if lkg := s.fleetStatsLKG(); lkg != nil {
		// Keep the LIVE coverage figures (Reporting/Eligible/Stale) so the page
		// can still show recollection progress, but serve the cached counts.
		fs.ReposManaged = lkg.ReposManaged
		fs.PRsMerged = lkg.PRsMerged
		fs.PRsRejected = lkg.PRsRejected
		fs.CVEsClosed = lkg.CVEsClosed
		fs.ContributorsTotal = lkg.ContributorsTotal
		fs.StaleData = true
		fs.AsOf = lkg.CollectedAt.UTC().Format(time.RFC3339)
		s.logger.Warn("fleet stats: serving last-known-good totals; live coverage is below the publish threshold",
			"reporting", fs.Reporting,
			"eligible", fs.Eligible,
			"lkg_collected_at", fs.AsOf,
			"lkg_based_on_reporting", lkg.Reporting,
			"lkg_based_on_eligible", lkg.Eligible,
		)
	}

	// Make the degraded case loud on the server too. The landing page hides the
	// numbers, but a silent hide is how this regressed unnoticed before: with
	// no log line, a fleet-wide collector failure looks identical to a quiet
	// day. This is the operator-visible signal that the data, not the fleet,
	// is what shrank.
	// The total is always published now, so this is no longer a publish gate —
	// it is the operator signal that the DATA shrank, not the fleet. Keep it
	// loud: a silent hide is how the blank strip regressed unnoticed before,
	// where a fleet-wide collector failure looked identical to a quiet day.
	if fs.Eligible > 0 && fs.Reporting*2 < fs.Eligible {
		s.logger.Warn("fleet stats: fewer than half the eligible hives are reporting; the published total is partial",
			"reporting", fs.Reporting,
			"eligible", fs.Eligible,
			"stale", fs.Stale,
		)
	}
	if fs.Reporting == 0 && fs.Eligible > 0 {
		s.logger.Warn("fleet stats: NO hive reported a fresh count; the strip will show nothing",
			"eligible", fs.Eligible,
			"stale", fs.Stale,
		)
	}

	data, _ := json.Marshal(fs)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// removeRegistryEntry removes a hive from the in-memory registry by ID and
// flushes the registry to disk before returning.
//
// The flush is synchronous rather than going through requestSave because
// requestSave defers the write by registrySaveDelay. A hub restart inside that
// window would reload the pre-delete registry from disk and the deleted hive
// would reappear in "My Hives" — the in-memory removal alone is not durable.
// Deletion is rare and user-initiated, so paying the write cost inline is fine.
func (s *HubServer) removeRegistryEntry(id, by string) {
	// The hive is going away, so its uncollectible-upgrade de-dup entry must go
	// with it. An uncollectible hive is by definition never armed, so this is
	// the ONLY path that can ever retire its entry (see forgetUncollectibleUpgrade).
	s.forgetUncollectibleUpgrade(id)
	s.mu.Lock()
	removed := false
	for i, h := range s.registry.Hives {
		if h.ID == id {
			s.registry.Hives = append(s.registry.Hives[:i], s.registry.Hives[i+1:]...)
			removed = true
			break
		}
	}
	s.mu.Unlock()
	if !removed {
		return
	}
	if err := s.saveRegistryNow(); err != nil {
		// The in-memory removal already happened, so the listing is correct for
		// this process. Log loudly: only a restart before the next periodic save
		// would resurrect the entry.
		s.logger.Error("registry entry removed in memory but persist failed", "id", id, "error", err)
		s.requestSave()
	}
	s.logger.Info("audit: registry entry removed", "id", id, "by", by)
}

// regPath returns this server's registry file path: the per-instance field
// captured at construction, or — for servers built directly in tests as bare
// &HubServer{...} literals without one — the package-level default. The
// fallback cannot reintroduce the registryPath data race: only NewHubServer
// starts a saveLoop goroutine (the sole reader that outlives its test), and
// NewHubServer always sets the field, so the global is only ever read from a
// test's own goroutine, synchronously with any test redirecting it.
func (s *HubServer) regPath() string {
	if s.registryPath != "" {
		return s.registryPath
	}
	return registryPath
}

// saveRegistryNow marshals and writes the registry to disk immediately, via a
// temp file plus atomic rename so a crash mid-write cannot truncate it.
func (s *HubServer) saveRegistryNow() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.registry, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal hub registry: %w", err)
	}
	path := s.regPath()
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write hub registry: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename hub registry: %w", err)
	}
	return nil
}

func (s *HubServer) handleRegistryDelete(w http.ResponseWriter, r *http.Request) {
	if !isHubAdmin(s.getAuthUser(r)) {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	s.forgetUncollectibleUpgrade(id)
	s.mu.Lock()
	removed := false
	for i, h := range s.registry.Hives {
		if h.ID == id {
			s.registry.Hives = append(s.registry.Hives[:i], s.registry.Hives[i+1:]...)
			removed = true
			break
		}
	}
	s.mu.Unlock()
	if removed {
		// Synchronous flush for the same durability reason as removeRegistryEntry.
		if err := s.saveRegistryNow(); err != nil {
			s.logger.Error("admin registry removal persist failed", "id", id, "error", err)
			s.requestSave()
		}
		s.logger.Info("audit: admin removed registry entry", "id", id, "admin", primaryHubAdmin())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"removed": removed, "id": id})
}

func (s *HubServer) handleHubVersion(w http.ResponseWriter, r *http.Request) {
	// The hub secret is never returned from this browser-reachable endpoint; the
	// raw shared secret has no legitimate consumer over HTTP.
	// latest_shas is the SPOKE-image-verified target actually advertised to
	// spokes (see the heartbeat response in handleHeartbeat). It legitimately
	// lags head_shas while a new commit's spoke image builds, so a bare
	// "latest_shas is behind HEAD" reading cannot distinguish a healthy
	// mid-build window from a wedged poller. head_shas + image_statuses make
	// that difference explicit:
	//
	//   head == latest                      → fully rolled out
	//   head > latest, status "building"    → normal, transient, no action
	//   head > latest, status "failed"      → the image build failed
	//   head > latest, status "ready"       → poller fault: the image exists
	//                                         but the advertised SHA never
	//                                         advanced
	//   head_shas empty / far behind GitHub → poller not running or erroring
	//
	// latest_hub_shas is the HUB image for the same commits, a SEPARATE build
	// that can land in either order. upgrade_state is computed from it, not
	// from latest_shas, so "upgrade_state: current" beside an older
	// latest_shas is a legitimate state (hub image published, spoke image
	// still building) rather than a contradiction.
	resp := map[string]any{
		"git_hash":         s.hubGitHash,
		"git_branch":       s.hubGitBranch,
		"latest_sha":       getLatestSHA(),
		"latest_shas":      getLatestSHAs(),
		"latest_hub_shas":  getLatestHubSHAs(),
		"head_shas":        getHeadSHAs(),
		"image_statuses":   getImageStatuses(),
		"image_build_urls": getImageBuildURLs(),
		"upgrade_state":    s.hubUpgradeState(),
	}
	data, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// markStaleHives flips Online off for hives that stopped beating and evicts
// ones silent past staleRemoveAge.
//
// It runs with s.mu held, so it does no I/O: online→offline transitions are
// COLLECTED and returned, and the caller appends them to the timeline after
// unlocking (see flushOfflineEvents). This is the only place a hive going
// offline can be observed — a silent hive by definition sends no heartbeat.
func (s *HubServer) markStaleHives() []offlineSweepEvent {
	now := time.Now()
	var offline []offlineSweepEvent
	kept := s.registry.Hives[:0]
	for i := range s.registry.Hives {
		if s.registry.Hives[i].LastHeartbeat == "" {
			s.registry.Hives[i].Online = false
			kept = append(kept, s.registry.Hives[i])
			continue
		}
		t, err := time.Parse(time.RFC3339, s.registry.Hives[i].LastHeartbeat)
		if err != nil {
			s.registry.Hives[i].Online = false
			kept = append(kept, s.registry.Hives[i])
			continue
		}
		age := now.Sub(t)
		if age > staleRemoveAge {
			s.logger.Info("removing stale hive", "id", s.registry.Hives[i].ID, "last_heartbeat", s.registry.Hives[i].LastHeartbeat)
			continue
		}
		wasOnline := s.registry.Hives[i].Online
		s.registry.Hives[i].Online = age <= maxHeartbeatAge
		if wasOnline && !s.registry.Hives[i].Online {
			offline = append(offline, offlineEventFor(&s.registry.Hives[i], age))
		}
		kept = append(kept, s.registry.Hives[i])
	}
	s.registry.Hives = kept
	return offline
}

func (s *HubServer) mergeLeaderboards() []LeaderboardEntry {
	merged := map[string]*LeaderboardEntry{}
	for _, h := range s.registry.Hives {
		if !h.IsPublic {
			continue
		}
		for _, lb := range h.Leaderboard {
			if lb.GitHubUsername == "" || lb.TrustTier == "agent" {
				continue
			}
			if existing, ok := merged[lb.GitHubUsername]; ok {
				existing.TasksCompleted += lb.TasksCompleted
				existing.TasksFailed += lb.TasksFailed
				if lb.Active {
					existing.Active = true
					existing.CurrentTask = lb.CurrentTask
					existing.HiveName = lb.HiveName
				}
			} else {
				entry := lb
				merged[lb.GitHubUsername] = &entry
			}
		}
	}
	result := make([]LeaderboardEntry, 0, len(merged))
	for _, v := range merged {
		if v.TasksCompleted == 0 && v.TasksFailed == 0 && !v.Active {
			continue
		}
		result = append(result, *v)
	}
	return result
}

func (s *HubServer) serveStatic(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	}
}

func (s *HubServer) loadRegistry() {
	data, err := os.ReadFile(s.regPath())
	if err != nil {
		s.logger.Info("no existing hub registry, starting fresh")
		return
	}
	if err := json.Unmarshal(data, &s.registry); err != nil {
		s.logger.Warn("failed to parse hub registry", "error", err)
	} else {
		s.logger.Info("hub registry loaded", "hives", len(s.registry.Hives))
	}
}

// recoverArmedUpgrades rebuilds s.heartbeatUpgrade — the in-memory map that
// actually DELIVERS upgrade instructions on each heartbeat — from the durable
// registry latch (Upgrading/UpgradeTarget), which survives a hub restart.
//
// Without this, a hub restart between arming an upgrade (handleUpgradeHive,
// bulk upgrade, auto-upgrade fallback) and its completion silently dropped the
// instruction: the registry kept saying "Upgrading" while the hub sent the
// spoke nothing, forever, because the map died with the old process (#2476 —
// four hives sat latched 15+ minutes with zero instructions after a hub
// self-upgrade). The heartbeat path clears both the map entry and the registry
// latch when the spoke reports the target SHA, so re-arming here is idempotent
// and self-terminating.
//
// Called from NewHubServer immediately after loadRegistry; takes s.mu like
// every other heartbeatUpgrade writer so it is also safe to invoke later.
func (s *HubServer) recoverArmedUpgrades() {
	type armed struct{ id, target string }
	var recovered []armed
	stamped := 0
	skippedUncollectible := 0
	s.mu.Lock()
	if s.heartbeatUpgrade == nil {
		s.heartbeatUpgrade = make(map[string]string)
	}
	for i := range s.registry.Hives {
		h := &s.registry.Hives[i]
		if !h.Upgrading {
			continue
		}
		// A true Upgrading flag must always carry a start time — the dashboard
		// badge and the stuck-upgrade alert both gate their elapsed clock on a
		// non-zero UpgradeStartedAt. The registry persists the field (json
		// "upgradeStartedAt"), so a normal restart rehydrates the REAL clock
		// and this guard does not fire — re-stamping a live clock here would
		// reset every in-flight upgrade's elapsed time on each hub roll, the
		// exact #2725 reset bug. Only a latch that never had a clock (armed
		// before the field existed, or spoke-reported upgrading persisted
		// before the observed-upgrade stamp) gets one now, instead of the
		// badge rendering without its counter until the next re-arm.
		if h.UpgradeStartedAt.IsZero() {
			h.UpgradeStartedAt = time.Now()
			stamped++
		}
		if h.UpgradeTarget == "" {
			continue
		}
		// A recorded terminal failure must stay terminal — re-arming it would
		// resurrect an upgrade the orphan sweep already gave up on and reported.
		if h.UpgradeFailed {
			continue
		}
		// Do not re-arm an instruction this hive cannot collect. Startup
		// recovery reads the DURABLE registry latch, so an uncollectible
		// upgrade abandoned in memory by triggerAutoUpgrades() would be
		// resurrected by the next hub restart if the latch had not yet been
		// persisted — restoring the wedge across exactly the event (a hub
		// roll) that this function exists to survive. Same predicate as every
		// other arming site.
		if !upgradeCollectible(h.LastHeartbeat, time.Now()) {
			skippedUncollectible++
			continue
		}
		s.heartbeatUpgrade[h.ID] = h.UpgradeTarget
		recovered = append(recovered, armed{id: h.ID, target: h.UpgradeTarget})
	}
	s.mu.Unlock()
	if stamped > 0 {
		// Persist the repaired clocks (saveCh is buffered, so a request made
		// before saveLoop starts is not lost) — otherwise a crash-looping hub
		// would re-stamp them to now on every restart.
		s.requestSave()
	}
	if skippedUncollectible > 0 {
		s.logger.Info("skipped recovering armed upgrades for hives that cannot collect them",
			"count", skippedUncollectible)
	}
	for _, a := range recovered {
		s.logger.Info("recovered armed upgrade from registry after restart",
			"hive_id", a.id, "target", a.target)
	}
}

func (s *HubServer) requestSave() {
	select {
	case s.saveCh <- struct{}{}:
	default:
	}
}

func (s *HubServer) saveLoop() {
	defer close(s.saveLoopDone)
	for {
		select {
		case <-s.saveLoopStop:
			// A request already queued when stop wins the select must still be
			// flushed — both channels can be ready at once and select picks
			// randomly, so drain deterministically.
			select {
			case <-s.saveCh:
				s.saveLoopFlush()
			default:
			}
			return
		case <-s.saveCh:
		}
		// Debounce, but stay cancelable: a stop during the delay flushes the
		// pending request synchronously instead of abandoning it, so shutdown
		// keeps the durability the delayed write was promising.
		select {
		case <-s.saveLoopStop:
			s.saveLoopFlush()
			return
		case <-time.After(registrySaveDelay):
		}
		s.saveLoopFlush()
	}
}

// saveLoopFlush is one debounced write, with saveLoop's historical
// warn-and-continue error handling (a failed periodic save is retried by the
// next requestSave; it must not kill the loop).
func (s *HubServer) saveLoopFlush() {
	if err := s.saveRegistryNow(); err != nil {
		s.logger.Warn("hub registry save failed", "path", s.regPath(), "error", err)
	}
}

// StopSaveLoop stops the debounced registry-save goroutine and waits for it to
// exit, flushing any pending save request first (#4774). After it returns, no
// further registry write can come from this server — which is what a test
// holding the registry in a t.TempDir needs before the framework removes it.
// Safe to call multiple times, and a no-op on bare &HubServer{} literals that
// never started the loop.
func (s *HubServer) StopSaveLoop() {
	if s.saveLoopStop == nil {
		return
	}
	s.saveLoopStopOnce.Do(func() { close(s.saveLoopStop) })
	<-s.saveLoopDone
}

func (s *HubServer) findContributeHive() *RegistryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.Online && h.IsPublic && h.DashboardURL != "" && !isPrivateURL(context.Background(), h.DashboardURL) && h.Owner != "" {
			cp := h
			return &cp
		}
	}
	return nil
}

func (s *HubServer) handleContributeProxy(w http.ResponseWriter, r *http.Request) {
	hive := s.findContributeHive()
	if hive == nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"no hives available for contribution"}`, http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(hive.DashboardURL)
	if err != nil {
		http.Error(w, `{"error":"invalid hive dashboard URL"}`, http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	r.URL.Path = "/api/contribute/register"
	r.Host = target.Host
	s.logger.Info("proxying contribute registration", "hive", hive.ID, "target", target.String())
	proxy.ServeHTTP(w, r)
}

func (s *HubServer) handleContributeStatus(w http.ResponseWriter, r *http.Request) {
	hive := s.findContributeHive()
	if hive == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"available": false,
			"message":   "no hives currently available for contribution",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"available": true,
		"hive_id":   hive.ID,
		"hive_name": hive.Name,
		"org":       hive.Org,
	})
}

func (s *HubServer) handleContributeWSProxy(w http.ResponseWriter, r *http.Request) {
	hive := s.findContributeHive()
	if hive == nil {
		http.Error(w, "no hives available", http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(hive.DashboardURL)
	if err != nil {
		http.Error(w, "invalid hive URL", http.StatusInternalServerError)
		return
	}
	proxy := newContributeWSReverseProxy(target)
	r.URL.Path = "/api/contribute/ws"
	r.Host = target.Host
	s.logger.Info("proxying contribute WS", "hive", hive.ID, "target", target.String())
	proxy.ServeHTTP(w, r)
}

var newContributeWSReverseProxy = httputil.NewSingleHostReverseProxy

// privateURLDNSTimeout bounds DNS resolution inside the SSRF guard so a
// slow or malicious DNS server cannot block the caller indefinitely.
const privateURLDNSTimeout = 5 * time.Second

// hostResolver is the DNS seam used by isPrivateURL. Production ALWAYS uses
// defaultHostResolver — this indirection exists only so tests can supply a
// deterministic resolution result and stay hermetic in a network-isolated CI
// sandbox (audit finding L3, 2026-08-17 security review). Tests that override
// it must restore it via t.Cleanup. The fail-closed contract below (a resolver
// error means "private") is unchanged and must stay that way: swapping the
// resolver changes only WHERE answers come from, never how failures are
// treated. Mirrors the identical seam in pkg/dashboard.
type hostResolver func(ctx context.Context, host string) ([]string, error)

var privateURLResolver hostResolver = defaultHostResolver

func defaultHostResolver(ctx context.Context, host string) ([]string, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, privateURLDNSTimeout)
	defer cancel()
	return (&net.Resolver{}).LookupHost(resolveCtx, host)
}

func isPrivateURL(ctx context.Context, rawURL string) bool {
	for _, scheme := range []string{"https://", "http://", "wss://", "ws://"} {
		if strings.HasPrefix(rawURL, scheme) {
			rawURL = strings.TrimPrefix(rawURL, scheme)
			break
		}
	}
	host := rawURL
	if idx := strings.IndexAny(host, ":/"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.ToLower(host)
	blocked := []string{"localhost", "127.", "10.", "172.16.", "172.17.", "172.18.", "172.19.",
		"172.20.", "172.21.", "172.22.", "172.23.", "172.24.", "172.25.", "172.26.", "172.27.",
		"172.28.", "172.29.", "172.30.", "172.31.", "192.168.", "169.254.", "[::1]", "[::ffff:", "0.0.0.0", "0."}
	for _, p := range blocked {
		if strings.HasPrefix(host, p) {
			return true
		}
	}

	// Resolve hostname to catch DNS names that map to private IPs (DNS rebinding).
	addrs, err := privateURLResolver(ctx, host)
	if err != nil {
		// If DNS fails, treat as private (fail-closed) to prevent bypass.
		return true
	}
	for _, addr := range addrs {
		for _, p := range blocked {
			if strings.HasPrefix(addr, p) {
				return true
			}
		}
	}

	return false
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// maxFleetCount bounds a single hive's reported fleet contribution count, so a
// malicious or buggy spoke can't inflate the public fleet total with an absurd
// value. Ten million PRs from one hive is already far beyond plausible.
const maxFleetCount = 10_000_000

// maxQuadrantSpend bounds a hive's reported per-window token spend for the same
// reason maxFleetCount bounds counts, but the stakes differ: quadrant scores are
// fleet-RELATIVE percentiles, so one absurd value does not merely look wrong on
// its own row — it stretches the distribution and pushes every honest hive
// toward the same end of the axis. A trillion tokens in one budget window is
// orders of magnitude beyond any real usage.
const maxQuadrantSpend int64 = 1_000_000_000_000

// clampFleetCount validates a spoke-reported fleet count pointer. nil stays nil
// (the hive reported nothing — it must NOT be aggregated as a zero). A negative
// value is treated as nil (garbage → don't count). Otherwise it is clamped to
// [0, maxFleetCount] and returned as a fresh pointer (never aliasing the
// caller's payload).
func clampFleetCount(v *int) *int {
	if v == nil || *v < 0 {
		return nil
	}
	c := clampInt(*v, 0, maxFleetCount)
	return &c
}

// clampQuadrantSpend sanitises a reported token spend the same way
// clampFleetCount handles counts: a negative is nonsense from the spoke and
// becomes nil ("not reported") rather than zero, so it can never be mistaken
// for a hive that genuinely consumed nothing.
//
// The upper bound is deliberately generous — this is a token count over a
// budget window, not a small tally — but it exists so a corrupt payload cannot
// dominate a fleet-relative percentile and flatten every other hive's score.
func clampQuadrantSpend(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	c := clampInt64(*v, 0, maxQuadrantSpend)
	return &c
}

// maxRemediationAgents bounds the remediation-hint detector collections
// (#5577) per hive, mirroring the maxAgents cap on the agent list itself: no
// hive has more agents than that, so anything larger is a broken or hostile
// payload.
const maxRemediationAgents = 50

// maxAgentErrorStreak bounds a single agent's reported consecutive-failure
// count. The verdict only needs "≥ threshold"; the spoke already caps at
// 10 000, so anything past this is garbage, not a bigger emergency.
const maxAgentErrorStreak = 10_000

// sanitizeAgentErrorStreaks validates a spoke-reported error-streak map. nil
// stays nil ("not measured" — carried forward, never treated as all-clear); a
// non-nil map stays non-nil even when everything inside is dropped, so a
// measured all-clear still overwrites a stale streak. Agent names pass the
// identifier sanitizer, non-positive streaks are dropped (a zero streak is
// simply absence), and both entry count and per-agent value are clamped.
func sanitizeAgentErrorStreaks(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for name, streak := range in {
		if len(out) >= maxRemediationAgents {
			break
		}
		name = sanitizeHeartbeatField(name)
		if name == "" || streak <= 0 {
			continue
		}
		out[name] = clampInt(streak, 1, maxAgentErrorStreak)
	}
	return out
}

// sanitizeAgentNameList validates a spoke-reported agent-name list with the
// same nil-vs-empty discipline as sanitizeAgentErrorStreaks: nil stays nil,
// a measured empty list stays non-nil. Names pass the identifier sanitizer,
// empties are dropped, and the list is capped.
func sanitizeAgentNameList(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, name := range in {
		if len(out) >= maxRemediationAgents {
			break
		}
		name = sanitizeHeartbeatField(name)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// parseHeartbeatTime converts an RFC3339 timestamp from a heartbeat into a
// time.Time, yielding the zero time for both an empty string (a spoke too old
// to report it) and an unparseable one. Callers treat the zero time as "not
// reported"; this mirrors how FleetStatsCollectedAt is handled at the upsert.
func parseHeartbeatTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func clampInt64(v, min, max int64) int64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func sanitizeHeartbeatField(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '/' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// maxProseFieldRunes bounds a sanitized prose field. Prose fields carry full
// sentences (an App-permission diagnosis, an upgrade failure cause), so the cap
// is generous — but a hostile or broken spoke must still not be able to bloat
// the registry with megabytes of "detail".
const maxProseFieldRunes = 500

// sanitizeProseField sanitizes a spoke-reported HUMAN-READABLE string — a
// sentence the dashboard renders verbatim to a person, such as
// GitHubAppPermIssue, UpgradeError, AdvisoryError, a leaderboard task title, or
// a health-check detail.
//
// This is DELIBERATELY separate from sanitizeHeartbeatField. That allowlist is
// for identifiers (hive IDs, branches, owners, agent states) where a space is
// never legal — but applied to prose it deletes every space and renders
// "The GitHub App is configured but has no installation." as
// "TheGitHubAppisconfiguredbuthasnoinstallation.", which is exactly the
// mangling this variant exists to end. Identifiers keep the strict sanitizer;
// only fields whose value is a sentence go through this one.
//
// What it keeps: letters, digits, spaces, and normal punctuation — the text
// stays a readable sentence.
//
// What it still neutralizes:
//   - '<', '>' and '&' are dropped, so no tag and no entity can ever be formed
//     even if a future render path forgets to escape;
//   - backticks are dropped (they are fenced-code/template metacharacters in
//     every surface this text could be pasted into);
//   - newlines, carriage returns and tabs become single spaces (these fields
//     are one-line UI strings and log fields);
//   - all other control characters (including ESC, so ANSI sequences cannot
//     ride along) are dropped;
//   - runs of whitespace collapse to one space and the result is trimmed;
//   - length is capped at maxProseFieldRunes.
func sanitizeProseField(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c == '\n' || c == '\r' || c == '\t':
			b.WriteRune(' ')
		case c < 0x20 || c == 0x7f:
			// Other control characters (incl. ESC): dropped entirely.
		case c == '<' || c == '>' || c == '&' || c == '`':
			// HTML/markup-dangerous: dropped.
		default:
			b.WriteRune(c)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if runes := []rune(out); len(runes) > maxProseFieldRunes {
		out = string(runes[:maxProseFieldRunes])
	}
	return out
}

func sanitizePublicURLSelfCheck(in *PublicURLSelfCheck) *PublicURLSelfCheck {
	if in == nil {
		return nil
	}
	status := sanitizeHeartbeatField(in.Status)
	if status != PublicURLSelfCheckOK && status != PublicURLSelfCheckFail && status != PublicURLSelfCheckUnknown {
		return nil
	}
	out := &PublicURLSelfCheck{
		Status:     status,
		CheckedAt:  sanitizeHeartbeatField(in.CheckedAt),
		Error:      sanitizeProseField(in.Error),
		HTTPStatus: clampInt(in.HTTPStatus, 0, 599),
	}
	return out
}

func sanitizeRouteExistenceCheck(in *RouteExistenceCheck) *RouteExistenceCheck {
	if in == nil {
		return nil
	}
	status := sanitizeHeartbeatField(in.Status)
	if status != RouteExistenceFound && status != RouteExistenceMissing && status != RouteExistenceUnknown {
		return nil
	}
	return &RouteExistenceCheck{
		Status:    status,
		CheckedAt: sanitizeHeartbeatField(in.CheckedAt),
		Host:      sanitizeHeartbeatField(in.Host),
		Kind:      sanitizeHeartbeatField(in.Kind),
		Error:     sanitizeProseField(in.Error),
	}
}

// sanitizeImageRef sanitizes a container image reference reported by a spoke.
//
// This is DELIBERATELY separate from sanitizeHeartbeatField rather than a
// widening of it. That allowlist is applied to roughly a dozen fields — hive
// IDs, versions, branches, governor mode, owner, agent names and states,
// leaderboard usernames — none of which may legally contain a colon, so
// admitting one globally would loosen all of them at once to fix a single
// field.
//
// An image ref is the one heartbeat field where a colon is structural: it
// separates repo from tag ("hive:v2-latest"), introduces a digest algorithm
// ("@sha256:..."), and delimits a registry port ("registry:5000/..."). The
// shared sanitizer stripped it, turning "ghcr.io/hivecommons/hive:v2-latest"
// into "ghcr.io/hivecommons/hivev2-latest" — a ref with no tag separator,
// which the pinned-image drift rule then reported as a CRITICAL pin on eleven
// perfectly healthy rolling hives.
//
// The allowlist is exactly the character set legal in an OCI reference:
// alphanumerics plus the "-_./" of a repository path, ":" for tag/port/digest
// algorithm, and "@" for a digest. Everything else — quotes, angle brackets,
// whitespace, control characters — is still dropped, so a hostile spoke cannot
// inject markup into a hub-rendered string.
func sanitizeImageRef(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '/' || c == ':' || c == '@' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// sanitizeRepoEntry makes a repo entry satisfy isValidRepoRef.
//
// A repo may legitimately be a bare name ("cuga-agent") or a cross-org
// reference ("laredo/cuga-agent") — both are supported config. What is NOT
// valid is a pasted URL ("github.ibm.com/enricom-ibm/jackrabbit"), which has
// two slashes: the hub rejects the spoke's heartbeat with "invalid repo name",
// /api/livez then fails on the stale heartbeat, and the pod crash-loops.
//
// Strip the scheme and a leading hostname (a first segment containing a dot),
// then keep at most the last two segments so a valid cross-org ref survives.
func sanitizeRepoEntry(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	// Drop a leading hostname: "github.ibm.com/org/repo" -> "org/repo".
	if len(parts) > 2 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	// Anything still deeper than owner/repo keeps only its last two segments.
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

// forgeAPIURLForHost turns a forge kind and host into the API base URL the
// spoke should use. GitHub Enterprise serves its v3 API at
// https://<host>/api/v3 — the working reference is hosted-open-source-osscar,
// which runs against github.ibm.com with exactly that value.
//
// GitLab and Gitea deliberately return an empty URL. Their adapters own the
// /api/v4 and /api/v1 suffixes respectively, so adding either suffix here would
// duplicate the API-path contract. More importantly, treating either host as
// GitHub Enterprise would aim the spoke at a nonexistent /api/v3 endpoint.
//
// Public github.com (and an empty host) return "", meaning "leave the spoke's
// github.api_url alone" — its own default is already https://api.github.com,
// and pushing a value here would overwrite a hand-tuned config. An empty or
// legacy kind retains the original host-based GitHub/GHE inference.
func forgeAPIURLForHost(kind, host string) string {
	switch ForgeKind(strings.ToLower(strings.TrimSpace(kind))) {
	case ForgeGitLab, ForgeGitea:
		return ""
	}
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || host == "github.com" || host == "api.github.com" {
		return ""
	}
	return "https://" + host + "/api/v3"
}
