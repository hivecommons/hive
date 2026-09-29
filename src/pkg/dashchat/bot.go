package dashchat

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hivecommons/hive/pkg/chat"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	outboxCap               = 200
	inboxCap                = 100
	dashboardMessageLimit   = 4000
	dashboardSendInterval   = 50 * time.Millisecond
	dashboardHeartbeatEvery = 15 * time.Minute
)

var (
	ErrInputRejected = errors.New("dashboard chat input rejected by safety scanner")
	ErrInboxFull     = errors.New("dashboard chat inbox is full")
)

type Config struct {
	DashboardURL   string
	DashboardToken string
	AllowedUsers   []string
	// PersonaStore persists each author's persona; nil keeps personas in
	// memory for this process only (hivecommons/hive#9175).
	PersonaStore chat.PersonaStore
	// PersonaLearning and AuditSink feed persona learning on the shared chat
	// spine (hivecommons/hive#8363); both are optional.
	PersonaLearning chat.PersonaLearningFunc
	AuditSink       chat.AuditSink
}

type Outbound struct {
	Seq      uint64 `json:"seq"`
	Text     string `json:"text"`
	Role     string `json:"role"`
	AuthorID string `json:"author_id,omitempty"`
}

// Poll is one outbox read. Seq is a per-process counter, so the client cursor
// only means something together with Epoch: a new Epoch tells the browser the
// hive restarted and every retained seq belongs to a fresh numbering
// (hivecommons/hive#9135). Next is the last seq assigned; Gap reports that
// entries newer than the caller's cursor were already evicted from the ring,
// so Messages does not start where the caller left off.
type Poll struct {
	Messages []Outbound `json:"messages"`
	Next     uint64     `json:"next"`
	Epoch    string     `json:"epoch"`
	Gap      bool       `json:"gap,omitempty"`
}

type Bot struct {
	*backend
	service *chat.Service
}

type inbound struct {
	msg chat.Message
}

type backend struct {
	logger *slog.Logger
	inbox  chan inbound
	epoch  string

	mu     sync.Mutex
	next   uint64
	outbox []Outbound
}

func SetAgentIdentities(identities map[string]chat.AgentIdentity) {
	chat.SetAgentIdentities(identities)
}
func SetAgentAliases(agentAliases map[string]string) { chat.SetAgentAliases(agentAliases) }

func NewBot(cfg Config, logger *slog.Logger) *Bot {
	if logger == nil {
		logger = slog.Default()
	}
	b := &backend{logger: logger, inbox: make(chan inbound, inboxCap), epoch: uuid.NewString()}
	service := chat.NewService(b, chat.Config{
		DashboardURL:      cfg.DashboardURL,
		DashboardToken:    cfg.DashboardToken,
		AllowedUsers:      cfg.AllowedUsers,
		PersonaStore:      cfg.PersonaStore,
		PersonaLearning:   cfg.PersonaLearning,
		AuditSink:         cfg.AuditSink,
		MessageLimit:      dashboardMessageLimit,
		SendInterval:      dashboardSendInterval,
		HeartbeatInterval: dashboardHeartbeatEvery,
	}, logger)
	return &Bot{backend: b, service: service}
}

func (b *Bot) SetAgentNames(names []string)   { b.service.SetAgentNames(names) }
func (b *Bot) SetAllowedUsers(users []string) { b.service.SetAllowedUsers(users) }
func (b *Bot) RegisterCommand(name string, handler chat.CommandHandler) {
	b.service.RegisterCommand(name, handler)
}
func (b *Bot) Start(ctx context.Context) error               { return b.service.Start(ctx) }
func (b *Bot) Deliver(ctx context.Context, msg chat.Message) { b.service.Deliver(ctx, msg) }
func (b *Bot) SendMessage(content string) error              { return b.Send(content) }

func (b *backend) Name() string { return "dashboard" }

func (b *backend) Send(content string) error {
	b.appendOutbox("bot", "", content)
	return nil
}

func (b *backend) SetTopic(string) error { return chat.ErrTopicUnsupported }

// CommandRefused implements chat.CommandRefuser: the spine refused a command
// before dispatch, so the operator who typed it sees why instead of polling an
// outbox that never answers. The allowlist decision is the spine's; this only
// renders it.
func (b *backend) CommandRefused(msg chat.Message, reason string) {
	command := strings.Fields(msg.Text)
	label := "command"
	if len(command) > 0 {
		label = "`" + command[0] + "`"
	}
	b.appendOutbox("bot", "", "❌ "+label+" from `"+msg.AuthorID+"` refused: "+reason+".")
}

func (b *backend) Listen(ctx context.Context, deliver func(chat.Message)) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-b.inbox:
			deliver(item.msg)
		}
	}
}

func (b *backend) Submit(user, text string) (uint64, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		user = "local"
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, ErrInputRejected
	}
	safe, verdict := ioscan.EnforceInput(text)
	if verdict.Blocked {
		return 0, ErrInputRejected
	}
	seq := b.appendOutbox("user", user, safe)
	msg := chat.Message{ID: uuid.NewString(), Text: safe, AuthorID: user, FromBot: false}
	select {
	case b.inbox <- inbound{msg: msg}:
		return seq, nil
	default:
		return seq, ErrInboxFull
	}
}

// Drain returns the retained outbox entries newer than since.
func (b *backend) Drain(since uint64) []Outbound { return b.Poll(since).Messages }

// Poll returns the retained outbox entries newer than since together with the
// cursor metadata the browser needs to resume without replay or silent loss.
func (b *backend) Poll(since uint64) Poll {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Outbound, 0, len(b.outbox))
	for _, msg := range b.outbox {
		if msg.Seq > since {
			out = append(out, msg)
		}
	}
	res := Poll{Messages: out, Next: b.next, Epoch: b.epoch}
	// Seqs start at 1, so oldest-1 cannot underflow; since+1 could wrap for a
	// hostile MaxUint64 cursor and report a phantom gap.
	if len(b.outbox) > 0 && since < b.outbox[0].Seq-1 {
		res.Gap = true
	}
	return res
}

func (b *backend) appendOutbox(role, author, text string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	entry := Outbound{Seq: b.next, Text: logscrub.ScrubString(text), Role: role, AuthorID: author}
	if len(b.outbox) == outboxCap {
		copy(b.outbox, b.outbox[1:])
		b.outbox[len(b.outbox)-1] = entry
		return entry.Seq
	}
	b.outbox = append(b.outbox, entry)
	return entry.Seq
}
