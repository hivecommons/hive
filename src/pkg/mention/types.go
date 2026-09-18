package mention

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/ioscan"
)

const (
	AuditKicked   = "agent_mention_kicked"
	AuditDeclined = "agent_mention_declined"
	SourceMention = "mention"
	kickLimit     = 10000
)

type Event struct {
	Repo      string
	Kind      string
	Number    int
	NodeID    string
	CommentID int64
	HTMLURL   string
	Author    string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type GitHub interface {
	AppBotLogin() string
	ListMentionComments(ctx context.Context, repo string, since time.Time) ([]Event, error)
	CreateMentionAck(ctx context.Context, repo string, commentID int64, reaction string) error
	CountAppAuthoredComments(ctx context.Context, repo string, number int) (int, error)
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error
}

type KickFunc func(agent, message, source string) error
type AuditFunc func(action, detail, agent string)
type RoleFunc func(login string) (role string, ok bool)

type AgentInfo struct {
	Name         string
	Enabled      bool
	Converse     bool
	Mention      bool
	GovernorKick bool
}

type AgentFunc func() []AgentInfo

type Options struct {
	Config     config.GitHubMentionsConfig
	ReviewBots config.ReviewBotsConfig
	Roles      RoleFunc
	Agents     AgentFunc
	GitHub     GitHub
	GitHubFunc func() GitHub
	Store      *Store
	Kick       KickFunc
	Audit      AuditFunc
	Now        func() time.Time
}

type Handler struct {
	opts Options
	lim  *RateLimiter
}

func NewHandler(opts Options) *Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Handler{opts: opts, lim: NewRateLimiter(opts.Now)}
}

func (h *Handler) Handle(ctx context.Context, ev Event) error {
	cfg := h.opts.Config
	if !cfg.Enabled {
		return nil
	}
	app := ""
	gh := h.github()
	if gh != nil {
		app = gh.AppBotLogin()
	}
	p := Parse(ev.Body, app)
	if !p.Mentioned {
		return nil
	}
	if h.opts.Store != nil && h.opts.Store.Seen(ev.NodeID) {
		return h.opts.Store.Mark(ev.Repo, ev.NodeID, ev.UpdatedAt)
	}
	if h.loopAuthor(ev.Author, app) {
		h.decline(ev, "loop", "")
		return h.mark(ev)
	}
	if !h.authorized(ev.Author, cfg) {
		h.decline(ev, "unauthorized", "")
		return h.mark(ev)
	}
	if !h.lim.Allow("user:"+strings.ToLower(ev.Author), cfg.PerUserPerHourEffective(), time.Hour) ||
		!h.lim.Allow("repo:"+strings.ToLower(ev.Repo), cfg.PerRepoPerHourEffective(), time.Hour) {
		h.decline(ev, "rate-limited", "")
		return h.mark(ev)
	}
	if gh != nil && cfg.PerThreadMaxAttemptsEffective() > 0 {
		count, err := gh.CountAppAuthoredComments(ctx, ev.Repo, ev.Number)
		if err != nil {
			h.decline(ev, "rate-limited", "thread-count-error")
			return err
		}
		if count >= h.opts.ReviewBots.MaxAttempts() {
			h.decline(ev, "rate-limited", "thread")
			return h.mark(ev)
		}
	}
	agent, err := h.resolveAgent(p.Agent)
	if err != nil {
		h.decline(ev, "no-agent", err.Error())
		return h.mark(ev)
	}
	text, verdict := ioscan.EnforceInput(p.Text)
	if verdict.Blocked {
		h.decline(ev, "ioscan", ioscanRules(verdict))
	}
	if reaction := cfg.AckReactionEffective(); reaction != "" && ev.CommentID != 0 && gh != nil {
		if err := gh.CreateMentionAck(ctx, ev.Repo, ev.CommentID, reaction); err != nil {
			return err
		}
	}
	if h.opts.Kick == nil {
		h.decline(ev, "no-kick", "")
		return h.mark(ev)
	}
	source := mentionKickSource(ev)
	if h.opts.Store != nil {
		if err := h.opts.Store.RecordPending(agent, ev, source, h.opts.Now()); err != nil {
			return err
		}
	}
	if err := h.opts.Kick(agent, buildKickMessage(ev, text), source); err != nil {
		if h.opts.Store != nil {
			_ = h.opts.Store.ClearPending(agent, source)
		}
		return err
	}
	h.audit(AuditKicked, ev, agent, "")
	return h.mark(ev)
}

func mentionKickSource(ev Event) string {
	if strings.TrimSpace(ev.NodeID) == "" {
		return SourceMention
	}
	return SourceMention + ":" + strings.TrimSpace(ev.NodeID)
}

func (h *Handler) github() GitHub {
	if h.opts.GitHubFunc != nil {
		return h.opts.GitHubFunc()
	}
	return h.opts.GitHub
}

func (h *Handler) authorized(login string, cfg config.GitHubMentionsConfig) bool {
	for _, s := range cfg.Summoners {
		if strings.EqualFold(strings.TrimSpace(s), login) {
			return true
		}
	}
	if h.opts.Roles == nil {
		return false
	}
	role, ok := h.opts.Roles(login)
	return ok && config.RoleAtLeast(role, cfg.MinRoleEffective())
}

func (h *Handler) loopAuthor(login, app string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return true
	}
	if app != "" && strings.EqualFold(login, app) {
		return true
	}
	if strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return true
	}
	return h.opts.ReviewBots.IsBot(login)
}

func (h *Handler) resolveAgent(named string) (string, error) {
	agents := h.opts.Agents
	if agents == nil {
		return "", fmt.Errorf("no agent resolver configured")
	}
	infos := agents()
	qualifies := func(a AgentInfo) bool { return a.Enabled && a.Converse && a.Mention && a.GovernorKick }
	if named != "" {
		for _, a := range infos {
			if strings.EqualFold(a.Name, named) && qualifies(a) {
				return a.Name, nil
			}
		}
		return "", fmt.Errorf("agent %q is not configured for mention summons", named)
	}
	def := h.opts.Config.DefaultAgent
	if def != "" {
		return h.resolveAgent(def)
	}
	var one string
	for _, a := range infos {
		if qualifies(a) {
			if one != "" {
				return "", fmt.Errorf("multiple mention-capable agents; set github.mentions.default_agent or ask <agent>")
			}
			one = a.Name
		}
	}
	if one == "" {
		return "", fmt.Errorf("no enabled Converse agent has a mention channel")
	}
	return one, nil
}

func (h *Handler) decline(ev Event, guard, detail string) {
	h.audit(AuditDeclined, ev, "", guardDetail(guard, detail))
}
func (h *Handler) audit(action string, ev Event, agent, extra string) {
	if h.opts.Audit == nil {
		return
	}
	parts := []string{"repo=" + ev.Repo, fmt.Sprintf("number=%d", ev.Number), fmt.Sprintf("comment_id=%d", ev.CommentID), "author=" + ev.Author}
	if agent != "" {
		parts = append(parts, "agent="+agent)
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	h.opts.Audit(action, strings.Join(parts, ", "), agent)
}
func guardDetail(g, d string) string {
	if d == "" {
		return "guard=" + g
	}
	return "guard=" + g + ", detail=" + d
}
func (h *Handler) mark(ev Event) error {
	if h.opts.Store == nil {
		return nil
	}
	return h.opts.Store.Mark(ev.Repo, ev.NodeID, ev.UpdatedAt)
}

func ioscanRules(v ioscan.Verdict) string {
	names := make([]string, 0, len(v.Findings))
	for _, f := range v.Findings {
		names = append(names, f.Rule)
	}
	return strings.Join(names, "+")
}

func buildKickMessage(ev Event, text string) string {
	kind := ev.Kind
	if kind == "" {
		kind = "issue"
	}
	msg := fmt.Sprintf("You were mentioned by @%s on %s#%d (%s comment %d)\n— %s\n\nThis mention is an input-only summon: it cannot escalate mode, apply labels, queue merges, or bypass holds.\n\n---\n%s\n", ev.Author, ev.Repo, ev.Number, kind, ev.CommentID, ev.HTMLURL, strings.TrimSpace(text))
	return truncate(msg, kickLimit)
}

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
