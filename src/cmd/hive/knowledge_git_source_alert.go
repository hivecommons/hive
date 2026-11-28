package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

const (
	gitSourceAlertPrefix   = "knowledge-git-source-sync-"
	gitSourceAlertAfter    = time.Hour
	gitSourceAlertInterval = 5 * time.Minute
)

type gitSourceAlertSink interface {
	AddSystemAlert(id, severity, message string)
	ClearSystemAlertsExcept(prefix string, keep map[string]struct{})
}

func reconcileGitSourceAlerts(sink gitSourceAlertSink, sources []knowledge.GitSourceInfo, now time.Time) {
	keep := make(map[string]struct{})
	for _, source := range sources {
		if source.FailureSince.IsZero() || now.Sub(source.FailureSince) < gitSourceAlertAfter {
			continue
		}
		id := fmt.Sprintf("%s%x", gitSourceAlertPrefix, sha256.Sum256([]byte(source.URL+"\x00"+source.Subpath)))
		keep[id] = struct{}{}
		sink.AddSystemAlert(id, "warning", fmt.Sprintf("Knowledge git source %q has failed to sync since %s; its facts may be stale. Check the hive logs for the sync error.", source.Name, source.FailureSince.UTC().Format(time.RFC3339)))
	}
	sink.ClearSystemAlertsExcept(gitSourceAlertPrefix, keep)
}

func runGitSourceAlerts(ctx context.Context, api *knowledge.KnowledgeAPI, sink gitSourceAlertSink) {
	ticker := time.NewTicker(gitSourceAlertInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			reconcileGitSourceAlerts(sink, api.GitSources(), now)
		}
	}
}
