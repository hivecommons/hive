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
	OriginLabelPrefix = config.OriginLabelPrefix
	OriginReviewLabel = config.OriginReviewLabel
	// LegacyFromReviewLabel is the retired name of OriginReviewLabel.
	LegacyFromReviewLabel = config.LegacyReviewBacklogLabel
	// OriginLabelColor is the shared colour of every origin/* label; it
	// matches the purple from-review used before the rename.
	OriginLabelColor = "8250df"

	OriginLabelMigrationID       = "origin-provenance-labels-v1"
	originLabelMigrationFileStem = "origin-label-migration"
)

// originLabelDescriptions documents the known origin/* sources. Unknown
// sources still get the shared colour and a generic description, so a
// future filer only has to pick a name.
var originLabelDescriptions = map[string]string{
	OriginReviewLabel: "Filed by Hive from a review-swarm backlog finding (provenance only, not a routing or hold signal)",
}

// IsOriginLabel reports whether name belongs to the origin/* family.
func IsOriginLabel(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(name, OriginLabelPrefix) && len(name) > len(OriginLabelPrefix)
}

// CanonicalOriginLabel maps a retired provenance label (from-review) to its
// origin/* replacement; see config.CanonicalOriginLabel.
func CanonicalOriginLabel(name string) (string, bool) {
	return config.CanonicalOriginLabel(name)
}

// originLabelDefinition returns the colour and description for an origin/*
// label. ok is false for any other label.
func originLabelDefinition(name string) (issueLabelDefinition, bool) {
	if !IsOriginLabel(name) {
		return issueLabelDefinition{}, false
	}
	desc, known := originLabelDescriptions[strings.TrimSpace(name)]
	if !known {
		desc = "Hive provenance: where this issue came from (origin/<source>; informational only)"
	}
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

// OriginLabelMigrationOptions configures the one-time from-review →
// origin/review swap on open items.
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
	Errors      []string                   `json:"errors,omitempty"`
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

// MigrateOriginLabels swaps retired provenance labels for their origin/*
// replacement on every open item in the hive's repos, exactly once per data
// dir (a marker file records completion). The new label is ensured with the
// shared family colour before it is added; the old label is removed from the
// item but left defined on the repo so closed items keep their history. A
// nil report with nil error means the migration already ran.
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
	for oldLabel, newLabel := range config.OriginLabelRenames {
		items, err := c.openItemsWithLabel(ctx, oldLabel)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		for _, item := range items {
			migrated := OriginLabelMigrationItem{Repo: item.Repo, Number: item.Number, Type: item.Type, From: oldLabel, To: newLabel}
			if err := c.migrateOriginLabelOnItem(ctx, item, oldLabel, newLabel, ensuredRepos, &migrated); err != nil {
				report.Errors = append(report.Errors, err.Error())
				continue
			}
			report.Migrated = append(report.Migrated, migrated)
		}
	}
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
	logger.Info("origin label migration complete", "migrated", len(report.Migrated), "report", reportPath)
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
