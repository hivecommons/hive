package snapshot

import (
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
)

var safeClassRe = regexp.MustCompile(`[^a-zA-Z0-9-]`)

type Builder struct {
	outputDir string
	logger    *slog.Logger
}

func NewBuilder(outputDir string, logger *slog.Logger) *Builder {
	return &Builder{
		outputDir: outputDir,
		logger:    logger,
	}
}

func (b *Builder) Build(status *dashboard.StatusPayload) error {
	if err := os.MkdirAll(b.outputDir, 0755); err != nil {
		return fmt.Errorf("creating snapshot dir: %w", err)
	}

	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")

	statusPath := filepath.Join(b.outputDir, fmt.Sprintf("status-%s.json", ts))
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling status: %w", err)
	}

	if err := os.WriteFile(statusPath, data, 0644); err != nil {
		return fmt.Errorf("writing status snapshot: %w", err)
	}

	latestPath := filepath.Join(b.outputDir, "latest.json")
	tmpLatest := latestPath + ".tmp"
	if err := os.WriteFile(tmpLatest, data, 0644); err != nil {
		return fmt.Errorf("writing latest snapshot: %w", err)
	}
	if err := os.Rename(tmpLatest, latestPath); err != nil {
		return fmt.Errorf("renaming latest snapshot: %w", err)
	}

	indexPath := filepath.Join(b.outputDir, "index.html")
	if err := b.buildIndexHTML(indexPath, status, ts); err != nil {
		return fmt.Errorf("building index HTML: %w", err)
	}

	b.logger.Info("snapshot built", "path", statusPath)
	return nil
}

func (b *Builder) Cleanup(maxAge time.Duration) error {
	entries, err := os.ReadDir(b.outputDir)
	if err != nil {
		return err
	}

	cutoff := time.Now().Add(-maxAge)
	removed := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if name == "latest.json" || name == "index.html" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(b.outputDir, name)); err != nil {
				b.logger.Warn("snapshot cleanup: remove failed", "file", name, "error", err)
				continue
			}
			removed++
		}
	}

	if removed > 0 {
		b.logger.Info("snapshot cleanup", "removed", removed)
	}

	return nil
}

func (b *Builder) buildIndexHTML(path string, status *dashboard.StatusPayload, ts string) error {
	page := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Hive Dashboard Snapshot — %s</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; background: #0a0a0a; color: #e0e0e0; }
  h1 { color: #f59e0b; }
  .card { background: #1a1a1a; border: 1px solid #333; border-radius: 8px; padding: 1rem; margin: 1rem 0; }
  .label { color: #888; font-size: 0.875rem; }
  .value { font-size: 1.25rem; font-weight: 600; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 1rem; }
  .agent { display: flex; justify-content: space-between; padding: 0.5rem 0; border-bottom: 1px solid #222; }
  .state-running { color: #22c55e; }
  .state-idle { color: #6b7280; }
  .state-failed { color: #ef4444; }
  pre { background: #111; padding: 1rem; border-radius: 4px; overflow-x: auto; max-width: 100%%; }
  @media (max-width: 600px) {
    body { margin: 0.75rem; }
    h1 { font-size: 1.3rem; }
    h2 { font-size: 1.1rem; }
    .grid { grid-template-columns: 1fr; }
    .agent { flex-wrap: wrap; gap: 0.25rem; }
    .agent span:last-child { width: 100%%; font-size: 0.75rem; }
    .value { font-size: 1.1rem; }
    pre { font-size: 0.75rem; max-height: 60vh; }
  }
  @media (max-width: 400px) {
    body { margin: 0.5rem; }
    .card { padding: 0.75rem; }
    h1 { font-size: 1.1rem; }
  }
</style>
</head>
<body>
<h1>Hive Dashboard Snapshot</h1>
<p class="label">Generated: %s</p>

<div class="grid">
  <div class="card">
    <div class="label">Governor Mode</div>
    <div class="value">%s</div>
  </div>
  <div class="card">
    <div class="label">Issues</div>
    <div class="value">%d</div>
  </div>
  <div class="card">
    <div class="label">PRs</div>
    <div class="value">%d</div>
  </div>
</div>

<h2>Agents</h2>
<div class="card">`,
		html.EscapeString(ts), html.EscapeString(ts),
		html.EscapeString(string(status.Governor.Mode)),
		status.Governor.Issues,
		status.Governor.PRs,
	)

	for _, agent := range status.Agents {
		stateClass := "state-" + safeClassRe.ReplaceAllString(agent.State, "")
		page += fmt.Sprintf(`
  <div class="agent">
    <span>%s</span>
    <span class="%s">%s</span>
    <span class="label">%s / %s</span>
  </div>`, html.EscapeString(agent.Name), stateClass, html.EscapeString(agent.State),
			html.EscapeString(agent.CLI), html.EscapeString(agent.Model))
	}

	page += `
</div>

<h2>Raw Status</h2>
<pre id="raw"></pre>
<script>
  fetch('latest.json')
    .then(r => r.json())
    .then(d => document.getElementById('raw').textContent = JSON.stringify(d, null, 2));
</script>
</body>
</html>`

	return os.WriteFile(path, []byte(page), 0644)
}
