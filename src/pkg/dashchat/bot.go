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
	b := &backend{logger: logger, inbox: make(chan inbound, inboxCap)}
	service := chat.NewService(b, chat.Config{
		DashboardURL:      cfg.DashboardURL,
		DashboardToken:    cfg.DashboardToken,
		AllowedUsers:      cfg.AllowedUsers,
		PersonaLearning:   cfg.PersonaLearning,
		AuditSink:         cfg.AuditSink,
		MessageLimit:      dashboardMessageLimit,
		SendInterval:      dashboardSendInterval,
		HeartbeatInterval: dashboardHeartbeatEvery,
	}, logger)
	return &Bot{backend: b, service: service}
}

func (b *Bot) SetAgentNames(names []string) { b.service.SetAgentNames(names) }
func (b *Bot) RegisterCommand(name string, handler chat.CommandHandler) {
	b.service.RegisterCommand(name, handler)
}
func (b *Bot) Start(ctx context.Context) error               { return b.service.Start(ctx) }
func (b *Bot) Deliver(ctx context.Context, msg chat.Message) { b.service.Deliver(ctx, msg) }
func (b *Bot) SendMessage(content string) error              { return b.Send(content) }

func (b *backend) Name() string { return "dashboard" }

func (b *backend) Send(content string) error {
	b.appendOutbox("bot", "", logscrub.ScrubString(content))
	return nil
}

func (b *backend) SetTopic(string) error { return chat.ErrTopicUnsupported }

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

func (b *backend) Drain(since uint64) []Outbound {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Outbound, 0, len(b.outbox))
	for _, msg := range b.outbox {
		if msg.Seq > since {
			out = append(out, msg)
		}
	}
	return out
}

func (b *backend) appendOutbox(role, author, text string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	entry := Outbound{Seq: b.next, Text: text, Role: role, AuthorID: author}
	if len(b.outbox) == outboxCap {
		copy(b.outbox, b.outbox[1:])
		b.outbox[len(b.outbox)-1] = entry
		return entry.Seq
	}
	b.outbox = append(b.outbox, entry)
	return entry.Seq
}
