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
)

// The hive/<hive-id> provenance label is retired (hivecommons/hive#12539):
// nothing writes it any more and the origin marker (origin_marker.go) carries
// the same fact in the body. Items labelled before the upgrade still carry
// it, so every boot runs RetireHiveIDLabels: it strips the label from every
// open item in the hive's repos and, once no open item carries it, deletes
// the label definition so the name disappears from the label picker. A
// marker file records completion per label so later boots cost nothing.

const (
	HiveIDLabelRetireID       = "hive-id-label-retire-v1"
	hiveIDLabelRetireFileStem = "hive-id-label-retire"
)

// HiveIDLabelRetireOptions configures RetireHiveIDLabels.
type HiveIDLabelRetireOptions struct {
	// HiveID is this hive's id; HiveProvenanceLabel(HiveID) is always swept.
	HiveID string
	// ExtraLabels are further retired spellings to sweep (the fleet-report
	// slug label when the hub-assigned slug differs from HiveID).
	ExtraLabels []string
	// KeepDefinitions leaves the label defined on each repo after the sweep
	// (closed items keep it in their history). Default false: delete it.
	KeepDefinitions bool
	DataDir         string
	MarkerPath      string
	ReportPath      string
	Logger          *slog.Logger
}

// HiveIDLabelRetireReport is the JSON report RetireHiveIDLabels writes.
type HiveIDLabelRetireReport struct {
	MigrationID string                  `json:"migration_id"`
	GeneratedAt string                  `json:"generated_at"`
	ReportPath  string                  `json:"report_path"`
	Labels      []string                `json:"labels"`
	Stripped    []HiveIDLabelRetireItem `json:"stripped"`
	// DefinitionsDeleted lists "<repo>:<label>" for each label definition
	// removed once nothing open carried it.
	DefinitionsDeleted []string `json:"definitions_deleted,omitempty"`
	Errors             []string `json:"errors,omitempty"`
}

// HiveIDLabelRetireItem is one open item the sweep stripped.
type HiveIDLabelRetireItem struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Type   string `json:"type"`
	Label  string `json:"label"`
	// OriginSource says where the item's origin hive is still recorded after
	// the label is gone: "marker" when the body carries the origin marker,
	// "" when only the legacy label said so (the identity footer and the
	// write audit still attribute it).
	OriginSource OriginSource `json:"origin_source,omitempty"`
}

// retireLabels is the ordered, de-duplicated set of labels the sweep covers.
func (o HiveIDLabelRetireOptions) retireLabels() []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range append([]string{HiveProvenanceLabel(o.HiveID)}, o.ExtraLabels...) {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, ok := legacyHiveIDLabel(l); !ok {
			// Never sweep a hive/* label that is still read as a signal.
			continue
		}
		if seen[strings.ToLower(l)] {
			continue
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	return out
}

// RetireHiveIDLabels strips the retired hive/<hive-id> provenance label(s)
// from every open issue and PR in the hive's repos and, unless
// KeepDefinitions is set, deletes each label definition once the sweep left
// nothing open carrying it. It is idempotent: a marker file records a clean
// completion and later calls return (nil, nil); a run with errors writes the
// report, leaves the marker absent and returns the error so the next boot
// retries. The label is provenance only, so a failure here never holds or
// releases anything.
func (c *Client) RetireHiveIDLabels(ctx context.Context, opts HiveIDLabelRetireOptions) (*HiveIDLabelRetireReport, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	labels := opts.retireLabels()
	if len(labels) == 0 {
		return nil, nil
	}
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir = defaultHiveHoldMigrationDataDir
	}
	markerPath := opts.MarkerPath
	if markerPath == "" {
		markerPath = filepath.Join(dataDir, hiveIDLabelRetireFileStem+"-"+safeMigrationFilePart(opts.HiveID)+".done")
	}
	reportPath := opts.ReportPath
	if reportPath == "" {
		reportPath = filepath.Join(dataDir, hiveIDLabelRetireFileStem+"-"+safeMigrationFilePart(opts.HiveID)+".json")
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
	report := &HiveIDLabelRetireReport{
		MigrationID: HiveIDLabelRetireID,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		ReportPath:  reportPath,
		Labels:      labels,
	}
	for _, label := range labels {
		items, err := c.openItemsWithLabel(ctx, label)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		failedRepos := map[string]bool{}
		for _, item := range items {
			if err := c.RemoveLabel(ctx, item.Repo, item.Number, label); err != nil {
				failedRepos[item.Repo] = true
				report.Errors = append(report.Errors, fmt.Sprintf("removing %s from %s#%d: %v", label, item.Repo, item.Number, err))
				continue
			}
			_, source := OriginHive(item.Body, item.Labels)
			if source == OriginSourceLabel {
				source = OriginSourceNone
			}
			report.Stripped = append(report.Stripped, HiveIDLabelRetireItem{Repo: item.Repo, Number: item.Number, Type: item.Type, Label: label, OriginSource: source})
			c.recordWriteAudit(AuditActionHiveIDLabelRetired, hiveWriteMeta(),
				WriteTarget{Repo: item.Repo, Number: item.Number},
				"label", label, "origin_source", string(source), "migration", HiveIDLabelRetireID)
		}
		if opts.KeepDefinitions {
			continue
		}
		for _, repo := range c.getRepos() {
			owner, repoName := c.splitRepo(repo)
			fullRepo := owner + "/" + repoName
			if failedRepos[fullRepo] {
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
	if err := writeHiveIDLabelRetireReport(reportPath, report); err != nil {
		return report, err
	}
	if len(report.Errors) > 0 {
		logger.Warn("hive/<hive-id> label retire sweep finished with errors; it will retry at next startup",
			"stripped", len(report.Stripped), "errors", len(report.Errors), "report", reportPath)
		return report, fmt.Errorf("hive id label retire: %d error(s); see %s", len(report.Errors), reportPath)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return report, err
	}
	if err := os.WriteFile(markerPath, []byte(report.GeneratedAt+"\n"), 0o600); err != nil {
		return report, err
	}
	logger.Info("hive/<hive-id> label retired", "labels", labels, "stripped", len(report.Stripped), "definitions_deleted", len(report.DefinitionsDeleted), "report", reportPath)
	return report, nil
}

// deleteLabelDefinition removes a label from a repo's label set. A 404 means
// it was never defined there (or is already gone) and is not an error.
func (c *Client) deleteLabelDefinition(ctx context.Context, owner, repo, label string) (bool, error) {
	resp, err := c.client.Issues.DeleteLabel(ctx, owner, repo, label)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		var ghErr *gh.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func writeHiveIDLabelRetireReport(path string, report *HiveIDLabelRetireReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
