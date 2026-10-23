package dashboard

import (
	"context"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/agentaudit"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func (h *ContributeWSHub) validatePRArtifactTrailers(task *WSTaskAssign, prURL string) {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil || task == nil || prURL == "" {
		return
	}
	ref, err := ghpkg.ParsePRURL(prURL)
	if err != nil {
		return
	}
	ctx := h.server.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	commits, err := h.server.deps.GHClient.ListPRCommits(ctx, ref.Owner+"/"+ref.Repo, ref.Number)
	if err != nil {
		h.logger.Warn("[contribute-ws] artifact trailer validation skipped", "pr_url", prURL, "error", err)
		return
	}
	run := task.identityKey()
	for _, commit := range commits {
		missing := agentaudit.MissingTrailers(commit.Message)
		if len(missing) == 0 {
			continue
		}
		trailers := agentaudit.ParseTrailers(commit.Message)
		h.server.AgentAuditSink().Record("system", agentaudit.AuditArtifactLinkMissing, task.TaskID,
			agentaudit.Fields(
				"run", firstRunNonEmpty(trailers[agentaudit.TrailerRun], run),
				"plan", trailers[agentaudit.TrailerPlan],
				"clause", trailers[agentaudit.TrailerSpec],
				"sha", commit.SHA,
				"missing", strings.Join(missing, ","),
				"pr", strconv.Itoa(ref.Number),
			))
	}
}
