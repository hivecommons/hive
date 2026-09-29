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

	"github.com/hivecommons/hive/pkg/agentaudit"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/persona"
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

type PersonaStore interface {
	GetPersona(ctx context.Context, author string) (persona.Record, bool, error)
	PutPersona(ctx context.Context, author string, record persona.Record) error
}

// PersonaLearningFunc returns the live persona learning configuration. It is
// aliased here so transports can accept it without importing pkg/persona.
type PersonaLearningFunc = func() persona.LearningConfig

// AuditSink is the audit seam transports pass through to the chat service.
type AuditSink = agentaudit.AuditSink

// ErrTopicUnsupported is returned by backends that cannot update channel topics.
var ErrTopicUnsupported = errors.New("chat topic updates unsupported")

// Message is a transport-agnostic inbound chat message.
type Message struct {
	// ID must be stable across redeliveries and unique within this service.
	// Empty IDs are delivered without deduplication.
	ID       string
	Text     string
	AuthorID string
	// TrustedRole is set only by authenticated in-process transports (dashboard)
	// after their own role floor has admitted the caller. External transports
	// leave it empty and continue through the explicit chat allowlist.
	TrustedRole string
	FromBot     bool
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

// CommandRefuser is an optional Backend extension. The spine refuses commands
// from authors outside the allowlist without replying on the channel, so a
// public transport never becomes an oracle for who is allowlisted. A transport
// whose author is already authenticated and is the only reader of the reply
// (dashboard chat) implements this to show the refusal instead of leaving the
// operator waiting; the decision itself stays in the spine.
type CommandRefuser interface {
	CommandRefused(msg Message, reason string)
}

type Config struct {
	DashboardURL      string
	DashboardToken    string
	AllowedUsers      []string
	MessageLimit      int
	SendInterval      time.Duration
	HeartbeatInterval time.Duration
	// PersonaStore holds each author's persona. cmd/hive passes a view of the
	// shared durable FilePersonaStore; nil falls back to an in-memory map that
	// is lost on restart (hivecommons/hive#9175).
	PersonaStore PersonaStore
	// PersonaLearning returns the live persona learning configuration
	// (hivecommons/hive#8363). Nil or a disabled result means no signals are
	// counted and no suggestions are made. It is a func so a Features panel
	// toggle takes effect without restarting the chat service.
	PersonaLearning PersonaLearningFunc
	// AuditSink receives "persona adjusted" events with their evidence. Nil
	// means adjustments are applied without an audit row.
	AuditSink AuditSink
}

type Service struct {
	inbound           recentMessages
	backend           Backend
	dashboardURL      string
	dashboardToken    string
	allowedUsers      map[string]allowedUser
	allowedUsersMu    sync.RWMutex
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
	sseIdleTimeout    time.Duration
	topicDebounce     time.Duration
	personaStore      PersonaStore
	personaLearning   func() persona.LearningConfig
	audit             agentaudit.AuditSink
	now               func() time.Time

	msgQueue           chan msgItem
	lastState          *statusSnapshot
	lastRuns           map[string]runSnapshot
	lastTopic          string
	pendingTopic       string
	topicTimer         *time.Timer
	pendingInterviews  map[pendingInterviewKey]*pendingInterview
	pendingPersonas    map[pendingPersonaKey]*pendingPersonaSetup
	pendingCheckpoints map[pendingCheckpointKey]*pendingCheckpoint
	// expandedRuns and shownSummaries are the per-author, per-run marks the
	// persona signals are derived from; see persona_learning.go.
	expandedRuns   map[personaRunKey]struct{}
	shownSummaries map[personaRunKey]struct{}
}

type msgItem struct {
	content string
}

func NewService(backend Backend, cfg Config, logger *slog.Logger) *Service {
	// Every spine log line carries the transport it runs on; the spine itself
	// is transport-agnostic, so message text never names one
	// (hivecommons/hive#9129).
	if backend != nil && logger != nil {
		logger = logger.With("backend", backend.Name())
	}
	allowed := parseAllowedUsers(cfg.AllowedUsers)
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
	personaStore := cfg.PersonaStore
	if personaStore == nil {
		personaStore = newLocalPersonaStore()
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
		messageLimit:       messageLimit,
		sendInterval:       sendInterval,
		heartbeatInterval:  heartbeatInterval,
		sseReconnectBase:   sseReconnectBase,
		sseReconnectMax:    sseReconnectMax,
		sseIdleTimeout:     sseIdleTimeout,
		topicDebounce:      time.Duration(topicDebounceMS) * time.Millisecond,
		personaStore:       personaStore,
		personaLearning:    cfg.PersonaLearning,
		audit:              cfg.AuditSink,
		now:                time.Now,
		msgQueue:           make(chan msgItem, 100),
		pendingInterviews:  make(map[pendingInterviewKey]*pendingInterview),
		pendingPersonas:    make(map[pendingPersonaKey]*pendingPersonaSetup),
		pendingCheckpoints: make(map[pendingCheckpointKey]*pendingCheckpoint),
		expandedRuns:       make(map[personaRunKey]struct{}),
		shownSummaries:     make(map[personaRunKey]struct{}),
	}
}

// allowedUser is one chat allowlist entry: the identity as configured (role
// suffix stripped) and its role. Per-author prompts and persona lookups use id
// unchanged; lookups by message author go through allowedUserRole.
type allowedUser struct {
	id   string
	role string
}

// parseAllowedUsers keys the allowlist by config.IdentityMatchKey so the spine
// resolves an author the same way the dashboard's AuthorizedRole does
// (case-insensitive, "github:alice" == "alice"). As there, the first entry for
// an identity wins.
func parseAllowedUsers(entries []string) map[string]allowedUser {
	allowed := make(map[string]allowedUser, len(entries))
	for i, entry := range entries {
		id, role := parseAllowedUser(entry, i)
		if id == "" {
			continue
		}
		key := config.IdentityMatchKey(id)
		if _, dup := allowed[key]; !dup {
			allowed[key] = allowedUser{id: id, role: role}
		}
	}
	return allowed
}

// SetAllowedUsers replaces the live allowlist used by all inbound chat gates.
// The heartbeat is authoritative for dashboard access on a spoke, so this must
// update the running service rather than only the boot configuration snapshot.
func (s *Service) SetAllowedUsers(entries []string) {
	allowed := parseAllowedUsers(entries)
	s.allowedUsersMu.Lock()
	s.allowedUsers = allowed
	s.allowedUsersMu.Unlock()
}

func (s *Service) allowedUserRole(author string) (string, bool) {
	s.allowedUsersMu.RLock()
	user, ok := s.allowedUsers[config.IdentityMatchKey(author)]
	s.allowedUsersMu.RUnlock()
	return user.role, ok
}

func (s *Service) allowedUserCount() int {
	s.allowedUsersMu.RLock()
	count := len(s.allowedUsers)
	s.allowedUsersMu.RUnlock()
	return count
}

// allowedUsersSnapshot maps each configured identity to its role.
func (s *Service) allowedUsersSnapshot() map[string]string {
	s.allowedUsersMu.RLock()
	users := make(map[string]string, len(s.allowedUsers))
	for _, user := range s.allowedUsers {
		users[user.id] = user.role
	}
	s.allowedUsersMu.RUnlock()
	return users
}

func parseAllowedUser(entry string, index int) (string, string) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", ""
	}
	role := ""
	if idx := strings.LastIndex(entry, ":"); idx >= 0 {
		head, tail := strings.TrimSpace(entry[:idx]), strings.TrimSpace(entry[idx+1:])
		if config.ValidRole(tail) {
			entry = head
			role = strings.ToLower(tail)
		}
	}
	if role == "" {
		if index == 0 {
			role = config.RoleOwner
		} else {
			role = config.RoleRead
		}
	}
	return entry, role
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

	s.logger.Info("chat service starting")

	s.registerBuiltinCommands()

	go s.drainLoop(ctx)
	go s.backend.Listen(ctx, func(msg Message) { s.Deliver(ctx, msg) })
	go s.sseLoop(ctx)
	go s.heartbeatLoop(ctx)
	context.AfterFunc(ctx, s.stopTopicTimer)

	s.enqueue(fmt.Sprintf("⚙️ **[pipeline]** Hive chat bot online (%s)", s.backend.Name()))
	return nil
}

func (s *Service) enqueue(content string) {
	content = logscrub.ScrubString(content)
	select {
	case s.msgQueue <- msgItem{content: content}:
	default:
		s.logger.Warn("chat: message queue full, dropping message")
	}
}

func (s *Service) DrainLoop(ctx context.Context) {
	s.drainLoop(ctx)
}

// drainLoop delivers queued messages one chunk at a time, paced by
// sendInterval. Splitting and retry follow the contract documented in
// send.go; a chunk is only skipped once its retries are exhausted.
func (s *Service) drainLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.msgQueue:
			for _, chunk := range SplitMessage(item.content, s.messageLimit) {
				if !s.sendChunk(ctx, chunk) || !sleepContext(ctx, s.sendInterval) {
					return
				}
			}
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
