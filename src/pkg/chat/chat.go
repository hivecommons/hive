package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	httpTimeoutS             = 10
	defaultSendInterval      = 1200 * time.Millisecond
	defaultHeartbeatInterval = 15 * time.Minute
	defaultMessageLimit      = 1900
)

// AgentIdentity holds the chat display metadata for an agent.
type AgentIdentity struct {
	Emoji string
	Color int
}

var chatMu sync.RWMutex

var agentIdentities = map[string]AgentIdentity{
	"governor": {Emoji: "🚦", Color: 0xf1c40f},
	"pipeline": {Emoji: "⚙️", Color: 0x95a5a6},
}

var aliases = map[string]string{
	"s": "status", "st": "status",
	"g": "governor", "gov": "governor",
	"h": "help", "?": "help",
	"k": "kick", "p": "pause", "r": "resume",
	"sc": "scanner", "ar": "architect", "ou": "outreach",
	"su": "supervisor", "ci": "ci-maintainer", "se": "sec-check",
	"sg": "strategist", "te": "quality", "qa": "quality",
	"tm": "telemetry", "op": "operations",
}

func SetAgentIdentities(identities map[string]AgentIdentity) {
	merged := map[string]AgentIdentity{
		"governor": {Emoji: "🚦", Color: 0xf1c40f},
		"pipeline": {Emoji: "⚙️", Color: 0x95a5a6},
	}
	for k, v := range identities {
		merged[k] = v
	}
	chatMu.Lock()
	agentIdentities = merged
	chatMu.Unlock()
}

func SetAgentAliases(agentAliases map[string]string) {
	chatMu.Lock()
	defer chatMu.Unlock()
	for k, v := range agentAliases {
		aliases[k] = v
	}
}

type CommandHandler func(ctx context.Context, args string) (string, error)

// ErrTopicUnsupported is returned by backends that cannot update channel topics.
var ErrTopicUnsupported = errors.New("chat topic updates unsupported")

// Message is a transport-agnostic inbound chat message.
type Message struct {
	ID       string
	Text     string
	AuthorID string
	FromBot  bool
}

// Backend is the transport seam used by Service.
type Backend interface {
	Name() string
	// Send posts one outbound message through the transport.
	Send(content string) error
	// SetTopic updates transport channel metadata; unsupported backends return ErrTopicUnsupported.
	SetTopic(topic string) error
	// Listen runs the inbound loop until ctx is done and delivers each message to Service.
	Listen(ctx context.Context, deliver func(Message))
}

type Config struct {
	DashboardURL      string
	DashboardToken    string
	AllowedUsers      []string
	MessageLimit      int
	SendInterval      time.Duration
	HeartbeatInterval time.Duration
}

type Service struct {
	backend           Backend
	dashboardURL      string
	dashboardToken    string
	allowedUsers      map[string]struct{}
	commands          map[string]CommandHandler
	agentNames        []string
	mu                sync.RWMutex
	logger            *slog.Logger
	client            *http.Client
	messageLimit      int
	sendInterval      time.Duration
	heartbeatInterval time.Duration
	sseReconnectBase  time.Duration
	sseReconnectMax   time.Duration

	msgQueue  chan msgItem
	lastState *statusSnapshot
	lastTopic string
}

type msgItem struct {
	content string
}

func NewService(backend Backend, cfg Config, logger *slog.Logger) *Service {
	allowed := make(map[string]struct{}, len(cfg.AllowedUsers))
	for _, id := range cfg.AllowedUsers {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = struct{}{}
		}
	}
	messageLimit := cfg.MessageLimit
	if messageLimit == 0 {
		messageLimit = defaultMessageLimit
	}
	sendInterval := cfg.SendInterval
	if sendInterval == 0 {
		sendInterval = defaultSendInterval
	}
	heartbeatInterval := cfg.HeartbeatInterval
	if heartbeatInterval == 0 {
		heartbeatInterval = defaultHeartbeatInterval
	}
	return &Service{
		backend:        backend,
		dashboardURL:   cfg.DashboardURL,
		dashboardToken: cfg.DashboardToken,
		allowedUsers:   allowed,
		commands:       make(map[string]CommandHandler),
		logger:         logger,
		client: &http.Client{
			Timeout: httpTimeoutS * time.Second,
		},
		messageLimit:      messageLimit,
		sendInterval:      sendInterval,
		heartbeatInterval: heartbeatInterval,
		sseReconnectBase:  sseReconnectBase,
		sseReconnectMax:   sseReconnectMax,
		msgQueue:          make(chan msgItem, 100),
	}
}

func (s *Service) SetAgentNames(names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentNames = names
}

func (s *Service) RegisterCommand(name string, handler CommandHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands[name] = handler
}

func (s *Service) Start(ctx context.Context) error {
	if s.backend == nil {
		return fmt.Errorf("chat backend not configured")
	}

	s.logger.Info("chat service starting", "backend", s.backend.Name())

	s.registerBuiltinCommands()

	go s.drainLoop(ctx)
	go s.backend.Listen(ctx, func(msg Message) { s.Deliver(ctx, msg) })
	go s.sseLoop(ctx)
	go s.heartbeatLoop(ctx)

	s.enqueue("⚙️ **[pipeline]** Hive v2 Discord bot online")
	return nil
}

func (s *Service) enqueue(content string) {
	select {
	case s.msgQueue <- msgItem{content: content}:
	default:
		s.logger.Warn("discord message queue full, dropping message")
	}
}

func (s *Service) DrainLoop(ctx context.Context) {
	s.drainLoop(ctx)
}

func (s *Service) drainLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.msgQueue:
			if err := s.backend.Send(item.content); err != nil {
				s.logger.Warn("discord send failed", "error", err)
			}
			time.Sleep(s.sendInterval)
		}
	}
}

func resolveAlias(s string) string {
	chatMu.RLock()
	defer chatMu.RUnlock()
	if v, ok := aliases[s]; ok {
		return v
	}
	return s
}

func getIdentity(name string) AgentIdentity {
	chatMu.RLock()
	defer chatMu.RUnlock()
	if id, ok := agentIdentities[name]; ok {
		return id
	}
	return agentIdentities["pipeline"]
}
