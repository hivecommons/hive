package github

// Owner decision (hivecommons/hive, 2026-10-19): the claim "who" modifier
// for a human principal is the generic `hive/claimed-by-human`, never
// `hive/claimed-by-<login>`. The per-login spelling minted one label per
// person in every repo and put identities in the label vocabulary; the
// login belongs in the claim record, the `hive:claim who=<login>` marker
// comment and the dashboard. Claim writers now emit the generic label and
// the issue reconciler swaps the live holder's legacy label per cycle, but
// an issue whose claim already lapsed — or one outside the enumeration —
// would keep wearing a login. This one-shot sweep (versioned /data marker,
// like the hold, provenance and needs-decision migrations) lists every
// `hive/claimed-by-*` label defined in each configured repo, picks the ones
// that are neither the generic label, the agent fallback, a backend name
// nor one of this hive's agent names, moves every open item carrying one to
// the generic label, records the login it carried, and deletes the per-login
// definition once nothing open carries it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/claims"
)

const (
	// ClaimedByHumanMigrationID names the current migration; the marker and
	// report file names carry the version so a later re-cut runs once more
	// on hives that completed this one.
	ClaimedByHumanMigrationID       = "claimed-by-human-label-v1"
	claimedByHumanMigrationFileStem = "claimed-by-human-label-v1"
	claimedByHumanRESTCaller        = "hive:claimed_by_human_migration"
)

// ClaimedByHumanMigrationOptions configures the one-shot per-login →
// generic claimed-by label sweep.
type ClaimedByHumanMigrationOptions struct {
	// AgentNames are this hive's agent names. A `hive/claimed-by-<agent>`
	// label is a hive-agent claim's label and is never migrated.
	AgentNames []string
	// LookupClaim, when set, returns the live ledger claim for an issue. An
	// item whose live claim is a hive-agent claim named by the label is left
	// alone even when the agent is not in AgentNames (another hive's agent
	// on a shared repo); a live human-principal claim confirms the login.
	LookupClaim func(repo string, number int) (claims.Claim, bool)
	// KeepDefinitions leaves each per-login label defined on the repo after
	// the sweep. Default false: delete it once nothing open carries it.
	KeepDefinitions bool
	DataDir         string
	MarkerPath      string
	ReportPath      string
	Logger          *slog.Logger
}

// ClaimedByHumanMigrationReport is written next to the marker.
type ClaimedByHumanMigrationReport struct {
	MigrationID string                        `json:"migration_id"`
	GeneratedAt string                        `json:"generated_at"`
	ReportPath  string                        `json:"report_path"`
	To          string                        `json:"to"`
	Migrated    []ClaimedByHumanMigrationItem `json:"migrated"`
	// Skipped lists "<repo>#<n>:<label>" for open items whose label names
	// the live hive-agent holder; the definition is kept in that repo.
	Skipped []string `json:"skipped,omitempty"`
	// DefinitionsDeleted lists "<repo>:<label>" for each per-login label
	// definition removed once nothing open carried it.
	DefinitionsDeleted []string `json:"definitions_deleted,omitempty"`
	Errors             []string `json:"errors,omitempty"`
}

// ClaimedByHumanMigrationItem is one open item moved to the generic label.
// Login is the claimant the retired label named — the only place it is
// recorded after the swap besides the claim record and marker comment.
type ClaimedByHumanMigrationItem struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Type   string `json:"type"`
	Label  string `json:"label"`
	Login  string `json:"login"`
	// LiveClaim is true when the ledger held a live human-principal claim
	// for the item at migration time (the login is then also in the claim
	// record).
	LiveClaim bool `json:"live_claim,omitempty"`
	// Added is false when the item already carried the generic label.
	Added bool `json:"added"`
}

// legacyClaimedByLoginLabels filters the labels defined in a repo down to
// the retired per-login spellings this sweep migrates. The generic label,
// the agent fallback, backend-named labels and this hive's agent names are
// excluded; the rest is returned in a stable order.
func legacyClaimedByLoginLabels(defined []string, agentNames []string) []string {
	agents := map[string]bool{}
	for _, a := range agentNames {
		if s := claims.ClaimedBySuffix(a, claims.KindAgent); s != "" {
			agents[strings.ToLower(s)] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range defined {
		login := claims.ClaimedByLabelLogin(l)
		if login == "" || agents[strings.ToLower(login)] || seen[strings.ToLower(l)] {
			continue
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// MigrateClaimedByHumanLabels moves every open item carrying a retired
// per-login `hive/claimed-by-<login>` label in the hive's repos to the
// generic `hive/claimed-by-human`, once per data dir, and deletes each
// per-login definition once nothing open carries it. A nil report with nil
// error means the migration already ran. The marker is written only when
// every item migrated, so a failed item retries at the next startup.
func (c *Client) MigrateClaimedByHumanLabels(ctx context.Context, opts ClaimedByHumanMigrationOptions) (*ClaimedByHumanMigrationReport, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir = defaultHiveHoldMigrationDataDir
	}
	markerPath := opts.MarkerPath
	if markerPath == "" {
		markerPath = filepath.Join(dataDir, claimedByHumanMigrationFileStem+".done")
	}
	reportPath := opts.ReportPath
	if reportPath == "" {
		reportPath = filepath.Join(dataDir, claimedByHumanMigrationFileStem+".json")
	}
	if _, err := os.Stat(markerPath); err == nil {
		return nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = c.logger
	}
	if logger == nil {
		logger = slog.Default()
	}
	report := &ClaimedByHumanMigrationReport{
		MigrationID: ClaimedByHumanMigrationID,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		ReportPath:  reportPath,
		To:          claims.ClaimedByHumanLabel,
	}
	ctx = WithRESTCaller(ctx, claimedByHumanRESTCaller)
	for _, repo := range c.getRepos() {
		owner, repoName := c.splitRepo(repo)
		fullRepo := owner + "/" + repoName
		defined, err := c.listRepoLabelNames(ctx, owner, repoName)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("listing labels in %s: %v", fullRepo, err))
			continue
		}
		for _, label := range legacyClaimedByLoginLabels(defined, opts.AgentNames) {
			login := claims.ClaimedByLabelLogin(label)
			items, err := c.openRepoItemsWithLabel(ctx, owner, repoName, label)
			if err != nil {
				report.Errors = append(report.Errors, err.Error())
				continue
			}
			keepDefinition := false
			for _, item := range items {
				live, ok := claims.Claim{}, false
				if opts.LookupClaim != nil {
					live, ok = opts.LookupClaim(item.Repo, item.Number)
				}
				if ok && live.Kind == claims.KindAgent && strings.EqualFold(claims.ClaimedByLabel(live), label) {
					report.Skipped = append(report.Skipped, fmt.Sprintf("%s#%d:%s", item.Repo, item.Number, label))
					keepDefinition = true
					continue
				}
				added := false
				if !issueHasLabel(item.Labels, claims.ClaimedByHumanLabel) {
					if err := c.AddLabels(ctx, item.Repo, item.Number, []string{claims.ClaimedByHumanLabel}); err != nil {
						keepDefinition = true
						report.Errors = append(report.Errors, fmt.Sprintf("adding %s to %s#%d: %v", claims.ClaimedByHumanLabel, item.Repo, item.Number, err))
						continue
					}
					added = true
				}
				if err := c.RemoveLabel(ctx, item.Repo, item.Number, label); err != nil {
					keepDefinition = true
					report.Errors = append(report.Errors, fmt.Sprintf("removing %s from %s#%d: %v", label, item.Repo, item.Number, err))
					continue
				}
				liveHuman := ok && live.Kind != claims.KindAgent
				report.Migrated = append(report.Migrated, ClaimedByHumanMigrationItem{Repo: item.Repo, Number: item.Number, Type: item.Type, Label: label, Login: login, LiveClaim: liveHuman, Added: added})
				c.recordWriteAudit(AuditActionClaimedByLabelMigrated, hiveWriteMeta(),
					WriteTarget{Repo: item.Repo, Number: item.Number},
					"label", label, "to", claims.ClaimedByHumanLabel, "login", login, "live_claim", fmt.Sprint(liveHuman), "migration", ClaimedByHumanMigrationID)
			}
			if opts.KeepDefinitions || keepDefinition {
				continue
			}
			deleted, err := c.deleteLabelDefinition(ctx, owner, repoName, label)
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("deleting label %s in %s: %v", label, fullRepo, err))
				continue
			}
			if deleted {
				report.DefinitionsDeleted = append(report.DefinitionsDeleted, fullRepo+":"+label)
			}
		}
	}
	if err := writeClaimedByHumanMigrationReport(reportPath, report); err != nil {
		return report, err
	}
	if len(report.Errors) > 0 {
		logger.Warn("claimed-by-human label migration finished with errors; it will retry at next startup",
			"migrated", len(report.Migrated), "errors", len(report.Errors), "report", reportPath)
		return report, fmt.Errorf("claimed-by-human label migration: %d error(s); see %s", len(report.Errors), reportPath)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return report, err
	}
	if err := os.WriteFile(markerPath, []byte(report.GeneratedAt+"\n"), 0o600); err != nil {
		return report, err
	}
	logger.Info("claimed-by-human label migration complete", "to", claims.ClaimedByHumanLabel, "migrated", len(report.Migrated), "skipped", len(report.Skipped), "definitions_deleted", len(report.DefinitionsDeleted), "report", reportPath)
	return report, nil
}

// listRepoLabelNames returns every label name defined in a repo.
func (c *Client) listRepoLabelNames(ctx context.Context, owner, repoName string) ([]string, error) {
	var out []string
	opts := &gh.ListOptions{PerPage: 100}
	for {
		labels, resp, err := c.client.Issues.ListLabels(ctx, owner, repoName, opts)
		if err != nil {
			return nil, err
		}
		for _, l := range labels {
			if l != nil && l.GetName() != "" {
				out = append(out, l.GetName())
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func writeClaimedByHumanMigrationReport(path string, report *ClaimedByHumanMigrationReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
