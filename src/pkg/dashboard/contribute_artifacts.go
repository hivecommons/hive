package dashboard

import (
	"context"
	"github.com/hivecommons/hive/pkg/agent"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"strconv"
	"strings"
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
		missing := agent.MissingTrailers(commit.Message)
		if len(missing) == 0 {
			continue
		}
		trailers := agent.ParseTrailers(commit.Message)
		h.server.AgentAuditSink().Record("system", agent.AuditArtifactLinkMissing, task.TaskID,
			agent.Fields(
				"run", firstRunNonEmpty(trailers[agent.TrailerRun], run),
				"plan", trailers[agent.TrailerPlan],
				"clause", trailers[agent.TrailerSpec],
				"sha", commit.SHA,
				"missing", strings.Join(missing, ","),
				"pr", strconv.Itoa(ref.Number),
			))
	}
}
