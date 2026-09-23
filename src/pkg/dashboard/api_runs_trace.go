package dashboard

import (
	"context"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/agent"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

var runTraceGit agent.CommitMessageReader

type runTraceCommitReader struct {
	client *ghpkg.Client
	repo   string
}

func (r runTraceCommitReader) CommitMessage(ctx context.Context, sha string) (string, error) {
	owner, name := r.client.SplitRepo(r.repo)
	return r.client.FullCommitMessage(ctx, owner, name, sha)
}

type runTraceAuditReader struct {
	log *AuditLog
}

func (r runTraceAuditReader) RecentAudit(n int) []agent.AuditRecord {
	if r.log == nil {
		return nil
	}
	entries := r.log.Recent(n)
	out := make([]agent.AuditRecord, 0, len(entries))
	for _, e := range entries {
		out = append(out, agent.AuditRecord{
			Timestamp: e.Timestamp,
			User:      e.User,
			Action:    e.Action,
			Detail:    e.Detail,
			Agent:     e.Agent,
		})
	}
	return out
}

func (s *Server) handleRunTrace(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		jsonError(w, "run key required", http.StatusBadRequest)
		return
	}
	sha := strings.TrimSpace(r.URL.Query().Get("sha"))
	if sha == "" {
		jsonError(w, "sha required", http.StatusBadRequest)
		return
	}
	reader := runTraceGit
	if reader == nil {
		if s.deps != nil && s.deps.GHClient != nil {
			repo := runRepoFromKey(key)
			reader = runTraceCommitReader{client: s.deps.GHClient, repo: repo}
		} else {
			reader = agent.LocalGit{}
		}
	}
	links, err := agent.ArtifactResolver{
		Git:      reader,
		Timeline: s.LifecycleTimeline(),
		Audit:    runTraceAuditReader{log: s.audit},
		Sink:     s.AgentAuditSink(),
	}.ResolveCommit(context.Background(), sha)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if links.Run != "" && links.Run != key {
		jsonError(w, "trace run does not match requested run", http.StatusNotFound)
		return
	}
	jsonResponse(w, links)
}

func runRepoFromKey(key string) string {
	if repo, _, ok := strings.Cut(key, "#"); ok {
		return repo
	}
	if repo, _, ok := strings.Cut(key, "!"); ok {
		return repo
	}
	return key
}
