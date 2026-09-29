package mention

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logscrub"
)

type Responder struct {
	store            *Store
	github           func() GitHub
	agents           AgentFunc
	perThreadMax     int
	perThreadMaxFunc func() int
	logger           *slog.Logger
	threadMu         sync.Mutex
	threads          map[string]*sync.Mutex
}

// NewResponder builds the phase-2 completion responder. perThreadMax is
// github.mentions.per_thread_max (GitHubMentionsConfig.PerThreadMaxEffective);
// a non-positive value falls back to config.DefaultMentionPerThreadMax.
func NewResponder(store *Store, github func() GitHub, agents AgentFunc, perThreadMax int, logger *slog.Logger) *Responder {
	if logger == nil {
		logger = slog.Default()
	}
	if perThreadMax <= 0 {
		perThreadMax = config.DefaultMentionPerThreadMax
	}
	return &Responder{store: store, github: github, agents: agents, perThreadMax: perThreadMax, logger: logger, threads: map[string]*sync.Mutex{}}
}

func (r *Responder) SetPerThreadMaxFunc(fn func() int) {
	if r == nil {
		return
	}
	r.perThreadMaxFunc = fn
}

func (r *Responder) HandleAgentEvent(agentName, event, detail string) {
	if r == nil || r.store == nil {
		return
	}
	if event == "kick-delivered" {
		source := strings.TrimSpace(detail)
		if mentionSource(source) {
			if _, _, err := r.store.PromotePending(agentName, source); err != nil {
				r.logger.Warn("mention: promoting pending mention failed", "agent", agentName, "error", err)
			}
		}
		return
	}
	if event == "kick-dropped" {
		source := strings.TrimSpace(detail)
		if mentionSource(source) {
			if err := r.store.DropSource(agentName, source); err != nil {
				r.logger.Warn("mention: dropping pending mention failed", "agent", agentName, "error", err)
			}
		}
		return
	}
	source := kickObserverDetailSource(detail)
	if event != "kick-log-archived" || !mentionSource(source) {
		return
	}
	if _, _, err := r.store.PromotePending(agentName, source); err != nil {
		r.logger.Warn("mention: promoting archived pending mention failed", "agent", agentName, "error", err)
		return
	}
	ctx, ok, err := r.store.ClaimActiveSource(agentName, source)
	if err != nil {
		r.logger.Warn("mention: claiming active mention failed", "agent", agentName, "error", err)
		return
	}
	if !ok {
		return
	}
	if !r.agentCanConverse(agentName) {
		return
	}
	gh := GitHub(nil)
	if r.github != nil {
		gh = r.github()
	}
	// Agent lifecycle events are not re-driven after this archive callback. If
	// GitHub reply prerequisites fail, fail closed and drop the claimed context
	// rather than leaving a permanent Active entry that can never be retried.
	if gh == nil {
		return
	}
	unlock := r.lockThread(ctx.Repo, ctx.Number)
	defer unlock()
	count, err := gh.CountMentionReplies(context.Background(), ctx.Repo, ctx.Number)
	if err != nil {
		r.logger.Warn("mention: completion reply thread count failed", "agent", agentName, "repo", ctx.Repo, "number", ctx.Number, "error", err)
		return
	}
	limit := r.perThreadMaxEffective()
	if count >= limit {
		r.logger.Info("mention: completion reply skipped at per-thread cap", "agent", agentName, "repo", ctx.Repo, "number", ctx.Number, "count", count, "max", limit)
		return
	}
	if err := gh.CreateIssueComment(context.Background(), ctx.Repo, ctx.Number, completionReply(agentName, kickObserverDetailReason(detail))); err != nil {
		r.logger.Warn("mention: completion reply failed", "agent", agentName, "repo", ctx.Repo, "number", ctx.Number, "error", err)
		return
	}
}

func (r *Responder) perThreadMaxEffective() int {
	limit := r.perThreadMax
	if r.perThreadMaxFunc != nil {
		limit = r.perThreadMaxFunc()
	}
	if limit <= 0 {
		return config.DefaultMentionPerThreadMax
	}
	return limit
}

func (r *Responder) lockThread(repo string, number int) func() {
	key := fmt.Sprintf("%s#%d", repo, number)
	r.threadMu.Lock()
	mu := r.threads[key]
	if mu == nil {
		mu = &sync.Mutex{}
		r.threads[key] = mu
	}
	r.threadMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func mentionSource(source string) bool {
	source = strings.TrimSpace(source)
	return source == SourceMention || strings.HasPrefix(source, SourceMention+":") ||
		source == SourceAction || strings.HasPrefix(source, SourceAction+":")
}

func kickObserverDetailSource(detail string) string {
	if _, source, ok := strings.Cut(detail, " source="); ok {
		return strings.TrimSpace(source)
	}
	return ""
}

func kickObserverDetailReason(detail string) string {
	reason, _, _ := strings.Cut(detail, " source=")
	return strings.TrimSpace(reason)
}

func (r *Responder) agentCanConverse(agentName string) bool {
	if r.agents == nil {
		return false
	}
	for _, a := range r.agents() {
		if a.Name == agentName {
			return a.Enabled && a.Converse
		}
	}
	return false
}

// completionReply renders the mention completion comment. The trailing
// ReplyMarker is what CountMentionReplies counts toward per_thread_max.
func completionReply(agentName, detail string) string {
	detail = logscrub.ScrubString(detail)
	if detail != "" {
		return fmt.Sprintf("Agent `%s` finished the mention-summoned run. The full log is retained on the hive dashboard. (%s)\n\n%s", agentName, detail, ReplyMarker)
	}
	return fmt.Sprintf("Agent `%s` finished the mention-summoned run. The full log is retained on the hive dashboard.\n\n%s", agentName, ReplyMarker)
}
