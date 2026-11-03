package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// ApplyPRTerminalAudit applies a successful hive-mediated merge or close to
// both local snapshots immediately, without waiting for a webhook or GET.
// The shared patch path remembers the close for in-flight status publishes.
func (s *Server) ApplyPRTerminalAudit(rec github.AuditRecord) {
	if s == nil || rec.Repo == "" || rec.Target <= 0 {
		return
	}
	if rec.Action != github.AuditActionPRMerged && rec.Action != github.AuditActionPRClosed {
		return
	}
	repo := rec.Repo
	if !strings.Contains(repo, "/") && s.deps != nil && s.deps.Config != nil && s.deps.Config.Project.Org != "" {
		repo = s.deps.Config.Project.Org + "/" + repo
	}
	now := time.Now()
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{
		Repo: repo, Number: rec.Target, IsPR: true, State: "closed",
		Merged: rec.Action == github.AuditActionPRMerged, UpdatedAt: now, FetchedAt: now,
	}})
}
