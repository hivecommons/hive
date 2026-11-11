// Package benchmarks turns review evidence bundles into the records published
// at https://hivecommons.dev/benchmarks/ (schema: hivecommons/benchmarks
// schema/result.schema.json; RFC hivecommons/hive#17694).
//
// Privacy contract: a record never carries a login, a prompt, finding text or
// code. The hive is identified only by HiveHash. Repository and org names are
// present only when Options.RepoPublic reports the repo public AND the owner
// has not set benchmarks.repo_consent: false; otherwise task.repo is absent.
package benchmarks

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// SchemaVersion is the published record schema version.
const SchemaVersion = "1"

// hashDomain salts the hive id so the published hash cannot be joined with
// any other sha256(hive_id) in the wild.
const hashDomain = "hivecommons-benchmarks/v1/"

// hiveHashLen is the published hex prefix length of the hive hash.
const hiveHashLen = 16

// recordIDLen is the hex length of a record id.
const recordIDLen = 24

// Exporter identifies the producer in source.exporter.
const Exporter = "hive"

// HiveHash is the anonymous, stable identifier of a hive on the leaderboard.
// Owners see theirs in Settings → Benchmarks and in /api/status.
func HiveHash(hiveID string) string {
	sum := sha256.Sum256([]byte(hashDomain + strings.TrimSpace(hiveID)))
	return hex.EncodeToString(sum[:])[:hiveHashLen]
}

// Record is one benchmark result: one agent role on one PR.
type Record struct {
	SchemaVersion  string   `json:"schema_version"`
	ID             string   `json:"id"`
	PublishedAt    string   `json:"published_at"`
	Source         Source   `json:"source"`
	Agent          Agent    `json:"agent"`
	Model          Model    `json:"model"`
	SandboxRuntime string   `json:"sandbox_runtime"`
	Task           Task     `json:"task"`
	Outcome        Outcome  `json:"outcome"`
	Quality        *Quality `json:"quality,omitempty"`
	Cost           Cost     `json:"cost"`
}

// Source says which hive emitted the record.
type Source struct {
	HiveID      string `json:"hive_id"`
	Org         string `json:"org,omitempty"`
	Cluster     string `json:"cluster,omitempty"`
	HiveVersion string `json:"hive_version"`
	Exporter    string `json:"exporter,omitempty"`
}

// Agent is the hive agent (or the review swarm) the record scores.
type Agent struct {
	Name    string `json:"name"`
	Role    string `json:"role,omitempty"`
	Harness string `json:"harness"`
}

// Model is the model the agent ran on.
type Model struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// Task describes the unit of work.
type Task struct {
	Kind           string `json:"kind"`
	Repo           string `json:"repo,omitempty"`
	RepoConsent    bool   `json:"repo_consent"`
	RepoVisibility string `json:"repo_visibility,omitempty"`
	ACMMLevel      *int   `json:"acmm_level,omitempty"`
}

// Outcome is what happened to the PR.
type Outcome struct {
	PROpened           bool     `json:"pr_opened"`
	PRMerged           bool     `json:"pr_merged"`
	PRClosedUnmerged   bool     `json:"pr_closed_unmerged"`
	MergeMethod        string   `json:"merge_method,omitempty"`
	CIGreenFirstTry    *bool    `json:"ci_green_first_try"`
	CIStatusAtMerge    string   `json:"ci_status_at_merge"`
	ReviewVerdict      string   `json:"review_verdict"`
	ReviewConfidence   *float64 `json:"review_confidence,omitempty"`
	ReviewerModels     []string `json:"reviewer_models,omitempty"`
	HumanInterventions int      `json:"human_interventions"`
	Retries            int      `json:"retries"`
	WallClockS         *int64   `json:"wall_clock_s,omitempty"`
	OpenedAt           string   `json:"opened_at,omitempty"`
	MergedAt           string   `json:"merged_at,omitempty"`
}

// Quality carries post-merge signals and the governing ACMM level.
type Quality struct {
	RevertedWithin14d   *bool `json:"reverted_within_14d,omitempty"`
	FollowedUpWithin14d *bool `json:"followed_up_within_14d,omitempty"`
	IssueReopened       *bool `json:"issue_reopened,omitempty"`
	ACMMLevel           *int  `json:"acmm_level,omitempty"`
	SentinelFindings    *int  `json:"sentinel_findings,omitempty"`
}

// Cost basis values (schema cost.basis). A spoke never emits
// fleet_model_median — the index builder imputes that from published records.
const (
	CostBasisReported       = "reported"
	CostBasisTokensXPricing = "tokens_x_pricing"
	CostBasisUnknown        = "unknown"
)

// Cost is the record's estimated spend. USDEst is hive's launch attribution
// priced with its public list-price table; it is an estimate, never a bill.
type Cost struct {
	Basis            string   `json:"basis"`
	USDEst           *float64 `json:"usd_est,omitempty"`
	InputTokens      *int64   `json:"input_tokens,omitempty"`
	OutputTokens     *int64   `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64   `json:"cache_write_tokens,omitempty"`
	// Launches counts the kicks whose spend was attributed to the record.
	Launches       int    `json:"launches,omitempty"`
	PriceTableDate string `json:"price_table_date,omitempty"`
}

// CostRef is one work reference a kick may have named. Ref carries the
// spelling the kick path uses: issue refs are "repo#N" (governor), PR refs
// are "owner/repo#N" (review swarm); Convert asks for both.
type CostRef struct {
	Ref  string
	Kind string // "issue" or "pr"
}

// CostLookup sums the attributed spend of every ref and reports whether any
// usage landed. Spend of zero counts as no attribution.
type CostLookup func(refs []CostRef) (Cost, bool)

// recordID derives the stable id of one (hive, PR, role) triple so re-exports
// overwrite rather than duplicate.
func recordID(hiveHash, repo string, number int, role string) string {
	sum := sha256.Sum256([]byte(hiveHash + "|" + repo + "|" + itoa(number) + "|" + role))
	return hex.EncodeToString(sum[:])[:recordIDLen]
}
