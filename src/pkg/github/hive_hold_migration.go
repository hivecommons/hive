package github

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	HiveHoldMigrationID = "8851-hold-provenance-v1"

	defaultHiveHoldMigrationDataDir = "/data"
	hiveHoldMigrationLabelColor     = "8250df"
	hiveHoldMigrationBatchWindow    = 2 * time.Second
)

type HiveHoldMigrationOptions struct {
	HiveID     string
	AuditPath  string
	DataDir    string
	MarkerPath string
	ReportPath string
	Logger     *slog.Logger
}

type HiveHoldMigrationReport struct {
	MigrationID     string                   `json:"migration_id"`
	HiveID          string                   `json:"hive_id"`
	ProvenanceLabel string                   `json:"provenance_label"`
	HoldLabel       string                   `json:"hold_label"`
	GeneratedAt     string                   `json:"generated_at"`
	ReportPath      string                   `json:"report_path"`
	AuditHolds      []HiveHoldMigrationItem  `json:"audit_holds"`
	ProvenanceOnly  []HiveHoldMigrationItem  `json:"provenance_only"`
	Ambiguous       []HiveHoldMigrationItem  `json:"ambiguous"`
	Errors          []string                 `json:"errors,omitempty"`
	Summary         HiveHoldMigrationSummary `json:"summary"`
	Notes           []string                 `json:"notes"`
}

type HiveHoldMigrationSummary struct {
	AuditHolds     int `json:"audit_holds"`
	ProvenanceOnly int `json:"provenance_only"`
	Ambiguous      int `json:"ambiguous"`
	Errors         int `json:"errors"`
}

type HiveHoldMigrationItem struct {
	Repo   string   `json:"repo"`
	Number int      `json:"number"`
	Type   string   `json:"type"`
	Title  string   `json:"title,omitempty"`
	Labels []string `json:"labels,omitempty"`
	Reason string   `json:"reason"`
}

type hiveHoldAuditEntry struct {
	Timestamp string `json:"ts"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
}

type hiveHoldAuditRecord struct {
	entry hiveHoldAuditEntry
	ts    time.Time
	seq   int
}

func (c *Client) MigrateHiveHoldLabel(ctx context.Context, opts HiveHoldMigrationOptions) (*HiveHoldMigrationReport, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	if c.client == nil {
		return nil, ErrNoGitHubClient
	}
	hiveID := strings.TrimSpace(opts.HiveID)
	oldLabel := HiveProvenanceLabel(hiveID)
	newLabel := CanonicalHiveHoldLabel(hiveID)
	if oldLabel == "" || newLabel == "hold" {
		return nil, nil
	}
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir = defaultHiveHoldMigrationDataDir
	}
	markerPath := opts.MarkerPath
	if markerPath == "" {
		markerPath = filepath.Join(dataDir, "hive-hold-migration-"+safeMigrationFilePart(hiveID)+".done")
	}
	reportPath := opts.ReportPath
	if reportPath == "" {
		reportPath = filepath.Join(dataDir, "hive-hold-migration-"+safeMigrationFilePart(hiveID)+".json")
	}
	if _, err := os.Stat(markerPath); err == nil {
		return nil, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = c.logger
	}
	if logger == nil {
		logger = slog.Default()
	}
	report := &HiveHoldMigrationReport{
		MigrationID:     HiveHoldMigrationID,
		HiveID:          hiveID,
		ProvenanceLabel: oldLabel,
		HoldLabel:       newLabel,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		ReportPath:      reportPath,
		Notes: []string{
			"Items with an active dashboard audit hold were copied to the new hold label.",
			"Items whose hive provenance label was co-added with an agent/* label were treated as provenance-only and left without the new hold label.",
			"Ambiguous items were kept held under the new hold label and need operator review; the hive/* provenance label is never removed.",
		},
	}
	auditHolds := activeHiveHoldAuditHolds(opts.AuditPath, oldLabel)
	items, err := c.openItemsWithLabel(ctx, oldLabel)
	if err != nil {
		return report, err
	}
	ensuredRepos := map[string]bool{}
	for _, item := range items {
		key := migrationItemKey(item.Repo, item.Number)
		if auditHolds[key] {
			item.Reason = "active repo_item_hold_add audit row without a later matching remove"
			report.AuditHolds = append(report.AuditHolds, item)
			if err := c.ensureMigrationHold(ctx, item.Repo, item.Number, newLabel, ensuredRepos); err != nil {
				report.Errors = append(report.Errors, err.Error())
			}
			continue
		}
		coAdded, err := c.hiveLabelCoAddedWithAgent(ctx, item.Repo, item.Number, oldLabel)
		if err != nil {
			item.Reason = "timeline unavailable; kept held for operator review: " + err.Error()
			report.Ambiguous = append(report.Ambiguous, item)
			if err := c.ensureMigrationHold(ctx, item.Repo, item.Number, newLabel, ensuredRepos); err != nil {
				report.Errors = append(report.Errors, err.Error())
			}
			continue
		}
		if coAdded {
			item.Reason = "hive provenance label was co-added with an agent/* label"
			report.ProvenanceOnly = append(report.ProvenanceOnly, item)
			continue
		}
		item.Reason = "no active audit hold and no co-added agent/* timeline event"
		report.Ambiguous = append(report.Ambiguous, item)
		if err := c.ensureMigrationHold(ctx, item.Repo, item.Number, newLabel, ensuredRepos); err != nil {
			report.Errors = append(report.Errors, err.Error())
		}
	}
	report.Summary = HiveHoldMigrationSummary{
		AuditHolds:     len(report.AuditHolds),
		ProvenanceOnly: len(report.ProvenanceOnly),
		Ambiguous:      len(report.Ambiguous),
		Errors:         len(report.Errors),
	}
	if err := writeHiveHoldMigrationReport(reportPath, report); err != nil {
		return report, err
	}
	if len(report.Errors) > 0 {
		return report, fmt.Errorf("hive hold migration had %d write errors; report: %s", len(report.Errors), reportPath)
	}
	if err := os.WriteFile(markerPath, []byte(report.GeneratedAt+"\n"), 0o644); err != nil {
		return report, err
	}
	logger.Info("hive hold label migration complete", "old_label", oldLabel, "new_label", newLabel, "audit_holds", report.Summary.AuditHolds, "provenance_only", report.Summary.ProvenanceOnly, "ambiguous", report.Summary.Ambiguous, "report", reportPath)
	return report, nil
}

func (c *Client) openItemsWithLabel(ctx context.Context, label string) ([]HiveHoldMigrationItem, error) {
	var out []HiveHoldMigrationItem
	for _, repo := range c.getRepos() {
		owner, repoName := c.splitRepo(repo)
		fullRepo := owner + "/" + repoName
		opts := &gh.IssueListByRepoOptions{
			State:       "open",
			Labels:      []string{label},
			ListOptions: gh.ListOptions{PerPage: 100},
		}
		for {
			issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repoName, opts)
			if err != nil {
				return nil, fmt.Errorf("listing %s items labeled %q: %w", repo, label, err)
			}
			for _, issue := range issues {
				itemType := "issue"
				if issue.IsPullRequest() {
					itemType = "pr"
				}
				out = append(out, HiveHoldMigrationItem{
					Repo:   fullRepo,
					Number: issue.GetNumber(),
					Type:   itemType,
					Title:  issue.GetTitle(),
					Labels: extractLabels(issue.Labels),
				})
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			opts.ListOptions.Page = resp.NextPage
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out, nil
}

func (c *Client) ensureMigrationHold(ctx context.Context, repo string, number int, label string, ensuredRepos map[string]bool) error {
	if !ensuredRepos[repo] {
		if err := c.EnsureIssueLabel(ctx, repo, label, hiveHoldMigrationLabelColor, "Hive dashboard hold: agents will not act on this item until an operator removes this label."); err != nil {
			return fmt.Errorf("ensuring %s in %s: %w", label, repo, err)
		}
		ensuredRepos[repo] = true
	}
	if err := c.AddLabels(ctx, repo, number, []string{label}); err != nil {
		return fmt.Errorf("adding %s to %s#%d: %w", label, repo, number, err)
	}
	return nil
}

func (c *Client) hiveLabelCoAddedWithAgent(ctx context.Context, repo string, number int, hiveLabel string) (bool, error) {
	owner, repoName := c.splitRepo(repo)
	opts := &gh.ListOptions{PerPage: 100}
	var hiveTimes []time.Time
	var agentTimes []time.Time
	for {
		events, resp, err := c.client.Issues.ListIssueEvents(ctx, owner, repoName, number, opts)
		if err != nil {
			if resp != nil && resp.Response != nil && resp.Response.StatusCode == http.StatusForbidden {
				return false, fmt.Errorf("GitHub event API forbidden or rate-limited")
			}
			return false, err
		}
		for _, event := range events {
			if event.GetEvent() != "labeled" || event.GetLabel() == nil {
				continue
			}
			name := strings.TrimSpace(event.GetLabel().GetName())
			t := event.GetCreatedAt().Time
			switch {
			case strings.EqualFold(name, hiveLabel):
				hiveTimes = append(hiveTimes, t)
			case strings.HasPrefix(strings.ToLower(name), "agent/"):
				agentTimes = append(agentTimes, t)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	for _, hiveTime := range hiveTimes {
		for _, agentTime := range agentTimes {
			if absDuration(hiveTime.Sub(agentTime)) <= hiveHoldMigrationBatchWindow {
				return true, nil
			}
		}
	}
	return false, nil
}

func activeHiveHoldAuditHolds(auditPath, label string) map[string]bool {
	if auditPath == "" {
		auditPath = filepath.Join(defaultHiveHoldMigrationDataDir, "audit.jsonl")
	}
	var records []hiveHoldAuditRecord
	active := map[string]bool{}
	for _, path := range migrationAuditFiles(auditPath) {
		readMigrationAuditFile(path, func(entry hiveHoldAuditEntry) {
			ts, _ := time.Parse(time.RFC3339, entry.Timestamp)
			records = append(records, hiveHoldAuditRecord{entry: entry, ts: ts, seq: len(records)})
		})
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].ts.IsZero() || records[j].ts.IsZero() {
			return records[i].seq < records[j].seq
		}
		return records[i].ts.Before(records[j].ts)
	})
	for _, record := range records {
		fields := parseAuditDetail(record.entry.Detail)
		repo := fields["repo"]
		number, _ := strconv.Atoi(fields["number"])
		if repo == "" || number <= 0 {
			continue
		}
		key := migrationItemKey(repo, number)
		switch record.entry.Action {
		case "repo_item_hold_add":
			if strings.EqualFold(fields["label"], label) {
				active[key] = true
			}
		case "repo_item_hold_remove":
			if auditRemoveMentionsLabel(fields, label) {
				delete(active, key)
			}
		}
	}
	return active
}

func migrationAuditFiles(auditPath string) []string {
	dir := filepath.Dir(auditPath)
	base := filepath.Base(auditPath)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)
	patterns := []string{
		auditPath,
		auditPath + ".*",
		filepath.Join(dir, prefix+"-*"+ext),
		filepath.Join(dir, prefix+"-*"+ext+".gz"),
	}
	seen := map[string]bool{}
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			if !seen[match] {
				seen[match] = true
				files = append(files, match)
			}
		}
	}
	sort.Strings(files)
	return files
}

func readMigrationAuditFile(path string, visit func(hiveHoldAuditEntry)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	var reader io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		var entry hiveHoldAuditEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil {
			visit(entry)
		}
	}
}

func parseAuditDetail(detail string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(detail, ", ") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func auditRemoveMentionsLabel(fields map[string]string, label string) bool {
	for _, key := range []string{"label", "labels"} {
		for _, item := range strings.Split(fields[key], ",") {
			if strings.EqualFold(strings.TrimSpace(item), label) {
				return true
			}
		}
	}
	return false
}

func writeHiveHoldMigrationReport(path string, report *HiveHoldMigrationReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func migrationItemKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

func safeMigrationFilePart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
