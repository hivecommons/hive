package mention

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

type Responder struct {
	store      *Store
	github     func() GitHub
	agents     AgentFunc
	reviewBots configReviewBots
	logger     *slog.Logger
	threadMu   sync.Mutex
	threads    map[string]*sync.Mutex
}

type configReviewBots interface {
	MaxAttempts() int
}

func NewResponder(store *Store, github func() GitHub, agents AgentFunc, reviewBots configReviewBots, logger *slog.Logger) *Responder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Responder{store: store, github: github, agents: agents, reviewBots: reviewBots, logger: logger, threads: map[string]*sync.Mutex{}}
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
	count, err := gh.CountAppAuthoredComments(context.Background(), ctx.Repo, ctx.Number)
	if err != nil {
		r.logger.Warn("mention: completion reply thread count failed", "agent", agentName, "repo", ctx.Repo, "number", ctx.Number, "error", err)
		return
	}
	if r.reviewBots != nil && count >= r.reviewBots.MaxAttempts() {
		return
	}
	if err := gh.CreateIssueComment(context.Background(), ctx.Repo, ctx.Number, completionReply(agentName, kickObserverDetailReason(detail))); err != nil {
		r.logger.Warn("mention: completion reply failed", "agent", agentName, "repo", ctx.Repo, "number", ctx.Number, "error", err)
		return
	}
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
	return source == SourceMention || strings.HasPrefix(source, SourceMention+":")
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

func completionReply(agentName, detail string) string {
	if detail != "" {
		return fmt.Sprintf("Agent `%s` finished the mention-summoned run. The full log is retained on the hive dashboard. (%s)", agentName, detail)
	}
	return fmt.Sprintf("Agent `%s` finished the mention-summoned run. The full log is retained on the hive dashboard.", agentName)
}
