package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// origin/<source> is the provenance axis of the label vocabulary: it records
// where a hive-authored issue came from. It is deliberately distinct from
// agent/<lane> (which agent works the item — a routing signal) and from
// stage/* (where the item is in the pipeline). No code path reads an
// origin/* label as a gate; the family is informational and shares one
// neutral colour so operators recognise it at a glance.
const (
	OriginLabelPrefix   = config.OriginLabelPrefix
	OriginReviewerLabel = config.OriginReviewerLabel
	// LegacyOriginReviewLabel is the retired first origin/* spelling of
	// OriginReviewerLabel; its repo definition is deleted once open items
	// have been migrated.
	LegacyOriginReviewLabel = config.LegacyOriginReviewLabel
	// LegacyFromReviewLabel is the retired pre-origin/* name of
	// OriginReviewerLabel.
	LegacyFromReviewLabel = config.LegacyReviewBacklogLabel
	// OriginLabelColor is the shared colour of every origin/* label; it
	// matches the purple from-review used before the rename.
	OriginLabelColor = "8250df"

	// OriginLabelMigrationID names the current migration. v1 moved
	// from-review → origin/review; v2 moves both retired names →
	// origin/reviewer; v3 also backfills origin/<lane> on open hive-authored
	// issues that carry agent/<lane> and no origin label, and
	// origin/selfreport on open hive/selfreport issues. The marker and
	// report file names carry the version so a hive that already ran an
	// earlier version runs this one exactly once; the rename step is
	// idempotent and re-running it finds nothing.
	OriginLabelMigrationID       = "origin-provenance-labels-v3"
	originLabelMigrationFileStem = "origin-label-migration-v3"

	// OriginSelfReportLabel is the provenance label of fleet runtime
	// self-reports; it accompanies the deprecated hive/selfreport.
	OriginSelfReportLabel = OriginLabelPrefix + config.OriginSourceSelfReport
	// legacySelfReportLabel is the deprecated selfreport marker the backfill
	// keys on (fleetreport.LabelSelfReport; spelled out to avoid the import).
	legacySelfReportLabel = "hive/selfreport"
	// agentLaneLabelPrefixForOrigin is the routing axis the backfill derives
	// origin/<lane> from.
	agentLaneLabelPrefixForOrigin = "agent/"
)

// retiredOriginLabelDefinitions lists retired origin/* names whose repo
// label definition the migration deletes after swapping open items. The
// pre-origin/* from-review is deliberately not listed: it predates the
// family and is left defined so closed items keep their history.
var retiredOriginLabelDefinitions = []string{LegacyOriginReviewLabel}

// IsOriginLabel reports whether name belongs to the origin/* family.
func IsOriginLabel(name string) bool { return config.IsOriginLabel(name) }

// CanonicalOriginLabel maps a retired provenance label (from-review,
// origin/review) to its origin/* replacement; see config.CanonicalOriginLabel.
func CanonicalOriginLabel(name string) (string, bool) {
	return config.CanonicalOriginLabel(name)
}

// originLabelDefinition returns the colour and description for an origin/*
// label. ok is false for any other label.
func originLabelDefinition(name string) (issueLabelDefinition, bool) {
	if !IsOriginLabel(name) {
		return issueLabelDefinition{}, false
	}
	// The vocabulary (config.KnownOriginSources) has a description per
	// source; lanes not listed there still get the lane text.
	desc, _ := config.OriginLabelDescription(name)
	return issueLabelDefinition{color: OriginLabelColor, description: desc}, true
}

// issueLabelDefinitionFor resolves the colour/description Hive uses when it
// has to create a label it owns: escalation labels first, then the origin/*
// family.
func issueLabelDefinitionFor(name string) (issueLabelDefinition, bool) {
	if def, ok := escalationIssueLabelDefinition(name); ok {
		return def, true
	}
	return originLabelDefinition(name)
}

// ensureOriginLabel creates an origin/* label on the repo or recolours an
// existing one to the shared family colour. It is idempotent: a label that
// already has the right colour is left alone, and a create that races
// another writer (422) is treated as success.
func (c *Client) ensureOriginLabel(ctx context.Context, owner, repo, name string) error {
	def, ok := originLabelDefinition(name)
	if !ok {
		return fmt.Errorf("%q is not an %s label", name, OriginLabelPrefix)
	}
	// Never recreate a retired spelling the migration has deleted.
	if canonical, renamed := CanonicalOriginLabel(name); renamed {
		return fmt.Errorf("%q is retired; use %s", name, canonical)
	}
	existing, _, err := c.client.Issues.GetLabel(ctx, owner, repo, name)
	if err == nil {
		if strings.EqualFold(strings.TrimSpace(existing.GetColor()), def.color) {
			return nil
		}
		_, _, err = c.client.Issues.EditLabel(ctx, owner, repo, name, &gh.Label{
			Name:        gh.Ptr(name),
			Color:       gh.Ptr(def.color),
			Description: gh.Ptr(def.description),
		})
		return err
	}
	if !githubStatusError(err, http.StatusNotFound) {
		return err
	}
	_, _, err = c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
		Name:        gh.Ptr(name),
		Color:       gh.Ptr(def.color),
		Description: gh.Ptr(def.description),
	})
	if githubStatusError(err, http.StatusUnprocessableEntity) {
		return nil
	}
	return err
}

// OriginLabelMigrationOptions configures the one-time from-review /
// origin/review → origin/reviewer swap on open items.
type OriginLabelMigrationOptions struct {
	DataDir    string
	MarkerPath string
	ReportPath string
	Logger     *slog.Logger
}

// OriginLabelMigrationReport is written next to the marker so an operator
// can see what the reconciler touched.
type OriginLabelMigrationReport struct {
	MigrationID string                     `json:"migration_id"`
	GeneratedAt string                     `json:"generated_at"`
	ReportPath  string                     `json:"report_path"`
	Renames     map[string]string          `json:"renames"`
	Migrated    []OriginLabelMigrationItem `json:"migrated"`
	// Backfilled lists open hive-authored issues that had no origin/* label
	// and received one derived from agent/<lane> or hive/selfreport.
	Backfilled []OriginLabelBackfillItem `json:"backfilled,omitempty"`
	// DefinitionsDeleted lists "<repo>:<label>" for each retired origin/*
	// label definition removed from a repo after its open items were swapped.
	DefinitionsDeleted []string `json:"definitions_deleted,omitempty"`
	Errors             []string `json:"errors,omitempty"`
}

// OriginLabelMigrationItem is one open issue or PR whose label was swapped.
type OriginLabelMigrationItem struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Type   string `json:"type"`
	From   string `json:"from"`
	To     string `json:"to"`
	// AlreadyHadNew is true when the item carried both names; only the old
	// one was removed.
	AlreadyHadNew bool `json:"already_had_new,omitempty"`
}

// OriginLabelBackfillItem is one open issue that gained its first origin/*
// label from an existing marker.
type OriginLabelBackfillItem struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Added is the origin/* label applied.
	Added string `json:"added"`
	// DerivedFrom is the label the source was read from (agent/<lane> or
	// hive/selfreport).
	DerivedFrom string `json:"derived_from"`
}

// MigrateOriginLabels swaps retired provenance labels for their origin/*
// replacement on every open item in the hive's repos, exactly once per data
// dir (a versioned marker file records completion, so a hive that ran the v1
// from-review swap still runs this one). The new label is ensured with the
// shared family colour before it is added and the old label is removed from
// the item. Retired origin/* definitions (origin/review) are then deleted
// from each repo whose items all migrated; from-review is left defined so
// closed items keep their history. A nil report with nil error means the
// migration already ran.
func (c *Client) MigrateOriginLabels(ctx context.Context, opts OriginLabelMigrationOptions) (*OriginLabelMigrationReport, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir = defaultHiveHoldMigrationDataDir
	}
	markerPath := opts.MarkerPath
	if markerPath == "" {
		markerPath = filepath.Join(dataDir, originLabelMigrationFileStem+".done")
	}
	reportPath := opts.ReportPath
	if reportPath == "" {
		reportPath = filepath.Join(dataDir, originLabelMigrationFileStem+".json")
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
	report := &OriginLabelMigrationReport{
		MigrationID: OriginLabelMigrationID,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		ReportPath:  reportPath,
		Renames:     config.OriginLabelRenames,
	}
	ensuredRepos := map[string]bool{}
	oldLabels := make([]string, 0, len(config.OriginLabelRenames))
	for oldLabel := range config.OriginLabelRenames {
		oldLabels = append(oldLabels, oldLabel)
	}
	sort.Strings(oldLabels)
	for _, oldLabel := range oldLabels {
		newLabel := config.OriginLabelRenames[oldLabel]
		items, err := c.openItemsWithLabel(ctx, oldLabel)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		failedRepos := map[string]bool{}
		for _, item := range items {
			migrated := OriginLabelMigrationItem{Repo: item.Repo, Number: item.Number, Type: item.Type, From: oldLabel, To: newLabel}
			if err := c.migrateOriginLabelOnItem(ctx, item, oldLabel, newLabel, ensuredRepos, &migrated); err != nil {
				failedRepos[item.Repo] = true
				report.Errors = append(report.Errors, err.Error())
				continue
			}
			report.Migrated = append(report.Migrated, migrated)
		}
		if !slices.Contains(retiredOriginLabelDefinitions, oldLabel) {
			continue
		}
		for _, repo := range c.getRepos() {
			owner, repoName := c.splitRepo(repo)
			fullRepo := owner + "/" + repoName
			if failedRepos[fullRepo] {
				continue
			}
			deleted, err := c.deleteLabelDefinition(ctx, owner, repoName, oldLabel)
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("deleting label %s in %s: %v", oldLabel, fullRepo, err))
				continue
			}
			if deleted {
				report.DefinitionsDeleted = append(report.DefinitionsDeleted, fullRepo+":"+oldLabel)
			}
		}
	}
	c.backfillOriginLabels(ctx, report, migratedItemKeys(report))
	if err := writeOriginLabelMigrationReport(reportPath, report); err != nil {
		return report, err
	}
	if len(report.Errors) > 0 {
		logger.Warn("origin label migration finished with errors; it will retry at next startup",
			"migrated", len(report.Migrated), "errors", len(report.Errors), "report", reportPath)
		return report, fmt.Errorf("origin label migration: %d error(s); see %s", len(report.Errors), reportPath)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return report, err
	}
	if err := os.WriteFile(markerPath, []byte(report.GeneratedAt+"\n"), 0o600); err != nil {
		return report, err
	}
	logger.Info("origin label migration complete", "migrated", len(report.Migrated), "backfilled", len(report.Backfilled), "definitions_deleted", len(report.DefinitionsDeleted), "report", reportPath)
	return report, nil
}

func (c *Client) migrateOriginLabelOnItem(ctx context.Context, item HiveHoldMigrationItem, oldLabel, newLabel string, ensuredRepos map[string]bool, out *OriginLabelMigrationItem) error {
	owner, repoName := c.splitRepo(item.Repo)
	hasNew := false
	for _, l := range item.Labels {
		if strings.EqualFold(strings.TrimSpace(l), newLabel) {
			hasNew = true
			break
		}
	}
	out.AlreadyHadNew = hasNew
	if !hasNew {
		if !ensuredRepos[item.Repo] {
			if err := c.ensureOriginLabel(ctx, owner, repoName, newLabel); err != nil {
				return fmt.Errorf("ensuring %s in %s: %w", newLabel, item.Repo, err)
			}
			ensuredRepos[item.Repo] = true
		}
		if err := c.AddLabels(ctx, item.Repo, item.Number, []string{newLabel}); err != nil {
			return fmt.Errorf("adding %s to %s#%d: %w", newLabel, item.Repo, item.Number, err)
		}
	}
	if err := c.RemoveLabel(ctx, item.Repo, item.Number, oldLabel); err != nil {
		return fmt.Errorf("removing %s from %s#%d: %w", oldLabel, item.Repo, item.Number, err)
	}
	c.recordWriteAudit(AuditActionOriginLabelMigrated, hiveWriteMeta(),
		WriteTarget{Repo: item.Repo, Number: item.Number},
		"from", oldLabel, "to", newLabel, "migration", OriginLabelMigrationID)
	return nil
}

// originBackfillSource derives the origin/* label an un-labelled open issue
// should carry from the markers it already has: hive/selfreport →
// origin/selfreport (the self-report axis is more specific than any lane),
// else the first agent/<lane> → origin/<lane>. ok is false when the issue
// already has an origin label or carries no usable marker.
func originBackfillSource(labels []string) (added, derivedFrom string, ok bool) {
	if _, has := config.FirstOriginLabel(labels); has {
		return "", "", false
	}
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), legacySelfReportLabel) {
			return OriginSelfReportLabel, legacySelfReportLabel, true
		}
	}
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if lane, has := strings.CutPrefix(strings.ToLower(l), agentLaneLabelPrefixForOrigin); has && lane != "" {
			return config.OriginLabel(lane), l, true
		}
	}
	return "", "", false
}

// backfillOriginLabels adds the first origin/* label to open issues the hive
// filed before the axis was universal (#12687 follow-up). "Hive-authored" is
// judged by the App-bot login when one is configured; without a bot identity
// the agent/<lane> or hive/selfreport marker alone is the evidence, since
// only the hive writes those. Pull requests are skipped: origin/* is an
// issue axis. Failures are recorded on the report (and so retried next
// startup) rather than aborting the rest of the sweep.
func (c *Client) backfillOriginLabels(ctx context.Context, report *OriginLabelMigrationReport, skip map[string]bool) {
	botLogins := c.hiveBotLogins()
	ensured := map[string]bool{}
	for _, repo := range c.getRepos() {
		owner, repoName := c.splitRepo(repo)
		fullRepo := owner + "/" + repoName
		items, err := c.openIssuesForOriginBackfill(ctx, owner, repoName)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("listing open issues in %s for origin backfill: %v", fullRepo, err))
			continue
		}
		for _, item := range items {
			if skip[migrationItemKey(fullRepo, item.number)] {
				continue
			}
			if len(botLogins) > 0 && !isHiveBotLogin(botLogins, item.author) {
				continue
			}
			added, derivedFrom, ok := originBackfillSource(item.labels)
			if !ok {
				continue
			}
			if !ensured[fullRepo+"\x00"+added] {
				if err := c.ensureOriginLabel(ctx, owner, repoName, added); err != nil {
					report.Errors = append(report.Errors, fmt.Sprintf("ensuring %s in %s: %v", added, fullRepo, err))
					continue
				}
				ensured[fullRepo+"\x00"+added] = true
			}
			if err := c.AddLabels(ctx, fullRepo, item.number, []string{added}); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("adding %s to %s#%d: %v", added, fullRepo, item.number, err))
				continue
			}
			c.recordWriteAudit(AuditActionOriginLabelMigrated, hiveWriteMeta(),
				WriteTarget{Repo: fullRepo, Number: item.number},
				"from", derivedFrom, "to", added, "migration", OriginLabelMigrationID)
			report.Backfilled = append(report.Backfilled, OriginLabelBackfillItem{Repo: fullRepo, Number: item.number, Added: added, DerivedFrom: derivedFrom})
		}
	}
}

// migratedItemKeys returns the items the rename phase just gave an origin
// label, so the backfill (whose listing may predate those writes) skips them.
func migratedItemKeys(report *OriginLabelMigrationReport) map[string]bool {
	out := map[string]bool{}
	for _, m := range report.Migrated {
		out[migrationItemKey(m.Repo, m.Number)] = true
	}
	return out
}

type originBackfillIssue struct {
	number int
	author string
	labels []string
}

// openIssuesForOriginBackfill lists every open issue (not PR) in a repo with
// its author and labels.
func (c *Client) openIssuesForOriginBackfill(ctx context.Context, owner, repoName string) ([]originBackfillIssue, error) {
	var out []originBackfillIssue
	opts := &gh.IssueListByRepoOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repoName, opts)
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue
			}
			out = append(out, originBackfillIssue{number: issue.GetNumber(), author: issue.GetUser().GetLogin(), labels: extractLabels(issue.Labels)})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func writeOriginLabelMigrationReport(path string, report *OriginLabelMigrationReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
