// Package managedagents binds the report-only extwork contract to Claude
// Managed Agents' self_hosted environment. It is linked only by the
// extwork_claude_managed_agents build tag.
package managedagents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/tokens"
)

const (
	// Engine is the registry name for the Claude Managed Agents host.
	Engine = "claude-managed-agents"
	// Capability is the contributor-relay token for hosted work. A relay must
	// declare it before an external task can be offered.
	Capability = "ext-exec/claude-managed-agents"
	// ManagedAgentsBetaHeader is the vendor beta contract pinned by this
	// prototype.
	ManagedAgentsBetaHeader = "managed-agents-2026-04-01"
	// AnthropicVersion is the stable API version used with the beta surface.
	AnthropicVersion = "2023-06-01"

	SettingEndpoint          = extwork.SettingEndpoint
	SettingWorkflowVersion   = extwork.SettingWorkflowVersion
	SettingAgentID           = "agent_id"
	SettingAgentVersion      = "agent_version"
	SettingToolsetVersion    = "toolset_version"
	SettingEnvironmentID     = "environment_id"
	SettingEnvironmentType   = "environment_type"
	SettingNetworkPolicy     = "network_policy"
	SettingAPIKeyEnv         = "api_key_env"
	SettingAPIKeyFile        = "api_key_file"
	SettingModel             = "model"
	SettingStateDir          = "state_dir"
	SettingMetricsDir        = "metrics_dir"
	SettingDailyBudgetCents  = "daily_budget_cents"
	SettingDirectGitHubWrite = "direct_github_write"
	SettingGitHubWritePath   = "github_write_path"

	defaultHTTPTimeout = 15 * time.Second
	maxErrorBodyBytes  = 4096
	maxSessionList     = 100
)

// ErrBudgetExceeded means the durable spoke ledger has no remaining USD cents
// for a new hosted session.
var ErrBudgetExceeded = errors.New("managed agents: daily budget exceeded")

// Config contains only secret references and pinned hosted-runtime identity;
// it never contains an API key value.
type Config struct {
	Endpoint         string
	WorkflowVersion  string
	AgentID          string
	AgentVersion     int
	ToolsetVersion   string
	EnvironmentID    string
	EnvironmentType  string
	NetworkPolicy    string
	APIKeyEnv        string
	APIKeyFile       string
	Model            string
	StateDir         string
	MetricsDir       string
	DailyBudgetCents int64
	// GitHubWritePath is "none" for the private spoke prototype. "proxy" is
	// accepted for a future Hive-owned write broker; every other value,
	// including direct_github_write, is refused by New.
	GitHubWritePath   string
	DirectGitHubWrite bool

	HTTPClient *http.Client
	Ledger     *tokens.ManagedAgentsLedger
	Now        func() time.Time
}

// Adapter implements extwork.Adapter for one pinned self_hosted environment.
type Adapter struct {
	base            *url.URL
	workflowVersion string
	agentID         string
	agentVersion    int
	toolsetVersion  string
	environmentID   string
	environmentType string
	networkPolicy   string
	apiKeyEnv       string
	apiKeyFile      string
	model           string
	githubWritePath string
	client          *http.Client
	ledger          *tokens.ManagedAgentsLedger
	state           *sessionStateStore
	now             func() time.Time
}

// New validates the private-spoke posture and builds an adapter. It performs
// no vendor request and does not read the configured key until a request is
// actually dispatched.
func New(cfg Config) (*Adapter, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("managed agents endpoint: %w", err)
	}
	if base.Scheme == "http" && !loopbackHost(base.Hostname()) {
		return nil, errors.New("managed agents endpoint must use https unless the host is loopback")
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("managed agents endpoint must be an http(s) URL with a host and no user info, query, or fragment")
	}
	workflowVersion := strings.TrimSpace(cfg.WorkflowVersion)
	if workflowVersion == "" {
		return nil, errors.New("managed agents workflow version is required")
	}
	agentID := strings.TrimSpace(cfg.AgentID)
	if agentID == "" {
		return nil, errors.New("managed agents agent_id is required")
	}
	if cfg.AgentVersion < 1 {
		return nil, errors.New("managed agents agent_version must be positive")
	}
	toolsetVersion := strings.TrimSpace(cfg.ToolsetVersion)
	if toolsetVersion == "" {
		return nil, errors.New("managed agents toolset_version is required")
	}
	environmentID := strings.TrimSpace(cfg.EnvironmentID)
	if environmentID == "" {
		return nil, errors.New("managed agents environment_id is required")
	}
	environmentType := strings.ToLower(strings.TrimSpace(cfg.EnvironmentType))
	if environmentType != "self_hosted" {
		return nil, errors.New("managed agents environment_type must be self_hosted")
	}
	networkPolicy := strings.ToLower(strings.TrimSpace(cfg.NetworkPolicy))
	if networkPolicy != "forced-egress" && networkPolicy != "proxy-only" {
		return nil, errors.New("managed agents network_policy must be forced-egress or proxy-only")
	}
	apiKeyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	apiKeyFile := strings.TrimSpace(cfg.APIKeyFile)
	if apiKeyEnv == "" && apiKeyFile == "" {
		return nil, errors.New("managed agents api_key_env or api_key_file is required")
	}
	if apiKeyFile != "" {
		if !filepath.IsAbs(apiKeyFile) || !secretPath(apiKeyFile) {
			return nil, errors.New("managed agents api_key_file must be an absolute path under /data/secrets or /secrets")
		}
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, errors.New("managed agents model is required for usage attribution")
	}
	budgetCents := cfg.DailyBudgetCents
	if budgetCents == 0 {
		budgetCents = tokens.DefaultManagedAgentsDailyBudgetCents
	}
	if budgetCents < 1 || budgetCents > tokens.DefaultManagedAgentsDailyBudgetCents {
		return nil, fmt.Errorf("managed agents daily budget must be between 1 and %d cents", tokens.DefaultManagedAgentsDailyBudgetCents)
	}
	writePath := strings.ToLower(strings.TrimSpace(cfg.GitHubWritePath))
	if writePath == "" {
		writePath = "none"
	}
	if cfg.DirectGitHubWrite {
		writePath = "direct"
	}
	if writePath != "none" && writePath != "proxy" {
		return nil, fmt.Errorf("managed agents github_write_path %q is refused: only none or proxy is allowed", writePath)
	}
	stateDir := strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		return nil, errors.New("managed agents state_dir is required")
	}
	metricsDir := strings.TrimSpace(cfg.MetricsDir)
	if metricsDir == "" {
		return nil, errors.New("managed agents metrics_dir is required for ledger metering")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout, Transport: &http.Transport{Proxy: nil}}
	}
	ledger := cfg.Ledger
	if ledger == nil {
		ledger, err = tokens.NewManagedAgentsLedger(metricsDir, budgetCents)
		if err != nil {
			return nil, err
		}
	}
	state, err := newSessionStateStore(stateDir)
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{
		base:            base,
		workflowVersion: workflowVersion,
		agentID:         agentID,
		agentVersion:    cfg.AgentVersion,
		toolsetVersion:  toolsetVersion,
		environmentID:   environmentID,
		environmentType: environmentType,
		networkPolicy:   networkPolicy,
		apiKeyEnv:       apiKeyEnv,
		apiKeyFile:      apiKeyFile,
		model:           model,
		githubWritePath: writePath,
		client:          client,
		ledger:          ledger,
		state:           state,
		now:             now,
	}, nil
}

func secretPath(path string) bool {
	clean := filepath.Clean(path)
	return clean == "/data/secrets" || strings.HasPrefix(clean, "/data/secrets/") || clean == "/secrets" || strings.HasPrefix(clean, "/secrets/")
}

// Factory builds the adapter from non-secret registry settings.
func Factory(settings map[string]string) (extwork.Adapter, error) {
	agentVersion, err := parsePositiveInt(settings[SettingAgentVersion])
	if err != nil {
		return nil, fmt.Errorf("managed agents agent_version: %w", err)
	}
	budgetCents, err := parseOptionalInt64(settings[SettingDailyBudgetCents])
	if err != nil {
		return nil, fmt.Errorf("managed agents daily_budget_cents: %w", err)
	}
	direct, err := parseOptionalBool(settings[SettingDirectGitHubWrite])
	if err != nil {
		return nil, fmt.Errorf("managed agents direct_github_write: %w", err)
	}
	adapter, err := New(Config{
		Endpoint:          settings[SettingEndpoint],
		WorkflowVersion:   settings[SettingWorkflowVersion],
		AgentID:           settings[SettingAgentID],
		AgentVersion:      agentVersion,
		ToolsetVersion:    settings[SettingToolsetVersion],
		EnvironmentID:     settings[SettingEnvironmentID],
		EnvironmentType:   settings[SettingEnvironmentType],
		NetworkPolicy:     settings[SettingNetworkPolicy],
		APIKeyEnv:         settings[SettingAPIKeyEnv],
		APIKeyFile:        settings[SettingAPIKeyFile],
		Model:             settings[SettingModel],
		StateDir:          settings[SettingStateDir],
		MetricsDir:        settings[SettingMetricsDir],
		DailyBudgetCents:  budgetCents,
		DirectGitHubWrite: direct,
		GitHubWritePath:   settings[SettingGitHubWritePath],
	})
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

func parsePositiveInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("is required")
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, errors.New("must be a positive integer")
	}
	return value, nil
}

func parseOptionalInt64(raw string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, errors.New("must be an integer")
	}
	return value, nil
}

func parseOptionalBool(raw string) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, errors.New("must be true or false")
	}
	return value, nil
}

// Engine returns the registry name.
func (a *Adapter) Engine() string { return Engine }

// WorkflowVersion returns the pinned adapter version.
func (a *Adapter) WorkflowVersion() string { return a.workflowVersion }

// Admit refuses direct GitHub write paths before a task is offered or
// persisted. Read-only report-only generation remains admissible on a private
// spoke; the adapter never downgrades a write-capable stage to local work.
func (a *Adapter) Admit(adm extwork.Admission) error {
	if err := adm.Validate(); err != nil {
		return err
	}
	if adm.Engine != Engine {
		return fmt.Errorf("%w: admission engine %q is not %s", extwork.ErrRefused, adm.Engine, Engine)
	}
	if adm.Authority.Capability != Capability {
		return fmt.Errorf("%w: authority capability %q is not %s", extwork.ErrRefused, adm.Authority.Capability, Capability)
	}
	if adm.Authority.Mode != extwork.ModeShadow && adm.Authority.Mode != extwork.ModeReportOnly {
		return fmt.Errorf("%w: mode %q is not a hosted report-only posture", extwork.ErrRefused, adm.Authority.Mode)
	}
	return nil
}

// Incarnation implements extwork.Pinner. The authenticated environment read is
// also the H4 pre-dispatch check: a cloud environment or a recreated identity
// is refused before session creation.
func (a *Adapter) Incarnation(ctx context.Context) (string, error) {
	env, err := a.environment(ctx)
	if err != nil {
		return "", err
	}
	return environmentIncarnation(env), nil
}

type environmentView struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

func environmentIncarnation(env environmentView) string {
	if strings.TrimSpace(env.Version) == "" {
		return strings.TrimSpace(env.ID)
	}
	return strings.TrimSpace(env.ID) + "@" + strings.TrimSpace(env.Version)
}

func (a *Adapter) environment(ctx context.Context) (environmentView, error) {
	var env environmentView
	if err := a.doJSON(ctx, http.MethodGet, a.endpoint("environments", a.environmentID), nil, &env); err != nil {
		return environmentView{}, err
	}
	if strings.TrimSpace(env.ID) == "" {
		return environmentView{}, fmt.Errorf("%w: environment response lacks id", extwork.ErrRefused)
	}
	if env.ID != a.environmentID {
		return environmentView{}, fmt.Errorf("%w: environment id %q is not pinned %q", extwork.ErrIncarnationMismatch, env.ID, a.environmentID)
	}
	if strings.ToLower(strings.TrimSpace(env.Type)) != a.environmentType {
		return environmentView{}, fmt.Errorf("%w: environment type %q is not %s", extwork.ErrRefused, env.Type, a.environmentType)
	}
	return env, nil
}

type bundleView struct {
	Admission struct {
		WorkKey          string `json:"work_key"`
		AssignmentID     string `json:"assignment_id"`
		Generation       uint64 `json:"generation"`
		Stage            string `json:"stage"`
		ContractRevision string `json:"contract_revision"`
		InputRevision    string `json:"input_revision"`
	} `json:"admission"`
	Summary string            `json:"summary"`
	Repo    string            `json:"repo"`
	Hints   map[string]string `json:"hints"`
}

func (a *Adapter) sessionRequest(adm extwork.Admission, payload []byte, remaining int64) (map[string]any, error) {
	var bundle bundleView
	if err := json.Unmarshal(payload, &bundle); err != nil {
		return nil, fmt.Errorf("%w: decode bounded bundle: %v", extwork.ErrRefused, err)
	}
	if bundle.Admission.WorkKey != adm.WorkKey || bundle.Admission.AssignmentID != adm.AssignmentID || bundle.Admission.Generation != adm.Generation || bundle.Admission.Stage != adm.Stage || bundle.Admission.ContractRevision != adm.ContractRevision || bundle.Admission.InputRevision != adm.InputRevision {
		return nil, fmt.Errorf("%w: bounded bundle identities do not match admission", extwork.ErrRefused)
	}
	if strings.TrimSpace(bundle.Summary) == "" || strings.TrimSpace(bundle.Repo) == "" {
		return nil, fmt.Errorf("%w: bounded bundle needs summary and repo", extwork.ErrRefused)
	}
	if remaining < 1 {
		return nil, ErrBudgetExceeded
	}
	prompt := bundle.Summary + "\n\nRepository workspace: " + bundle.Repo + "\nWork in the private self_hosted workspace only. Return the bounded stage receipt and patch artifacts; do not call GitHub directly."
	metadata := map[string]string{
		"hive_execution_key":   string(adm.ExecutionKey()),
		"hive_work_key":        adm.WorkKey,
		"hive_assignment_id":   adm.AssignmentID,
		"hive_request_digest":  adm.RequestDigest,
		"hive_toolset_version": a.toolsetVersion,
		"hive_repo":            bundle.Repo,
		"hive_stage":           adm.Stage,
	}
	return map[string]any{
		"agent": map[string]any{
			"type":    "agent",
			"id":      a.agentID,
			"version": a.agentVersion,
		},
		"environment_id": a.environmentID,
		"initial_events": []map[string]any{{
			"type": "user.message",
			"content": []map[string]string{{
				"type": "text",
				"text": prompt,
			}},
		}},
		"metadata": metadata,
		"budget": map[string]any{
			"type": "limit",
			"max_list_cost": map[string]string{
				"amount":   strconv.FormatInt(remaining, 10),
				"currency": "USD",
			},
		},
	}, nil
}

// Start admits a keyed session. The state file plus vendor metadata reconcile a
// crash after the vendor accepted a request but before Hive persisted its
// native session ID; a changed payload under the same key is a conflict.
func (a *Adapter) Start(ctx context.Context, req extwork.StartRequest) (extwork.StartResult, error) {
	adm := req.Admission
	if err := a.Admit(adm); err != nil {
		return extwork.StartResult{}, err
	}
	if adm.Authority.Mode != extwork.ModeReportOnly {
		return extwork.StartResult{}, fmt.Errorf("%w: shadow mode cannot start a hosted session", extwork.ErrRefused)
	}
	if adm.WorkflowVersion != a.workflowVersion {
		return extwork.StartResult{}, fmt.Errorf("%w: admission workflow version %q is not pinned %q", extwork.ErrRefused, adm.WorkflowVersion, a.workflowVersion)
	}
	if adm.RequestDigest != extwork.RequestDigest(req.Payload) {
		return extwork.StartResult{}, extwork.ErrPayloadDigest
	}
	incarnation, err := a.Incarnation(ctx)
	if err != nil {
		return extwork.StartResult{}, err
	}
	if existing, err := a.lookup(ctx, adm.ExecutionKey(), incarnation, adm.RequestDigest); err == nil {
		return extwork.StartResult{RemoteRunID: existing.ID, RemoteIncarnation: incarnation, Deduplicated: true}, nil
	} else if !errors.Is(err, extwork.ErrNotFound) {
		return extwork.StartResult{}, err
	}
	// A prior process may have reserved this execution key before dying. No
	// vendor session was found above, so that reservation is safe to discard.
	if err := a.ledger.ReleaseSession(string(adm.ExecutionKey())); err != nil {
		return extwork.StartResult{}, fmt.Errorf("%w: clear stale managed agents reservation: %v", extwork.ErrTransport, err)
	}
	remaining, err := a.ledger.ReserveSession(string(adm.ExecutionKey()))
	if errors.Is(err, tokens.ErrManagedAgentsBudgetExceeded) {
		return extwork.StartResult{}, ErrBudgetExceeded
	}
	if err != nil {
		return extwork.StartResult{}, fmt.Errorf("%w: reserve managed agents daily budget: %v", extwork.ErrTransport, err)
	}
	body, err := a.sessionRequest(adm, req.Payload, remaining)
	if err != nil {
		_ = a.ledger.ReleaseSession(string(adm.ExecutionKey()))
		return extwork.StartResult{}, err
	}
	var created sessionView
	if err := a.doJSON(ctx, http.MethodPost, a.endpoint("sessions"), body, &created); err != nil {
		_ = a.ledger.ReleaseSession(string(adm.ExecutionKey()))
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusConflict {
			return extwork.StartResult{}, fmt.Errorf("%w: %v", extwork.ErrConflict, err)
		}
		return extwork.StartResult{}, err
	}
	if strings.TrimSpace(created.ID) == "" {
		_ = a.ledger.ReleaseSession(string(adm.ExecutionKey()))
		return extwork.StartResult{}, fmt.Errorf("%w: session response lacks id", extwork.ErrRefused)
	}
	if created.EnvironmentID != "" && created.EnvironmentID != a.environmentID {
		_ = a.ledger.ReleaseSession(string(adm.ExecutionKey()))
		return extwork.StartResult{}, fmt.Errorf("%w: session environment %q is not pinned %q", extwork.ErrIncarnationMismatch, created.EnvironmentID, a.environmentID)
	}
	if err := a.ledger.MoveSessionReservation(string(adm.ExecutionKey()), created.ID); err != nil {
		return extwork.StartResult{RemoteRunID: created.ID, RemoteIncarnation: incarnation}, fmt.Errorf("%w: bind managed agents budget reservation: %v", extwork.ErrTransport, err)
	}
	if err := a.state.put(adm.ExecutionKey(), sessionBinding{RequestDigest: adm.RequestDigest, SessionID: created.ID, Incarnation: incarnation}); err != nil {
		return extwork.StartResult{RemoteRunID: created.ID, RemoteIncarnation: incarnation}, fmt.Errorf("%w: persist managed agents session binding: %v", extwork.ErrTransport, err)
	}
	return extwork.StartResult{RemoteRunID: created.ID, RemoteIncarnation: incarnation}, nil
}

func (a *Adapter) lookup(ctx context.Context, key extwork.ExecutionKey, incarnation, requestDigest string) (sessionView, error) {
	if incarnation == "" {
		live, err := a.Incarnation(ctx)
		if err != nil {
			return sessionView{}, err
		}
		incarnation = live
	}
	binding, ok := a.state.get(key)
	if ok {
		if requestDigest != "" && binding.RequestDigest != requestDigest {
			return sessionView{}, fmt.Errorf("%w: execution key is bound to a different payload", extwork.ErrConflict)
		}
		view, err := a.getSession(ctx, binding.SessionID)
		if err == nil {
			if err := a.validateSession(view, binding, incarnation, requestDigest); err != nil {
				return sessionView{}, err
			}
			return view, nil
		}
		if !errors.Is(err, extwork.ErrNotFound) {
			return sessionView{}, err
		}
		// The vendor deleted the recorded session. A bounded metadata scan is
		// still attempted before Hive considers the keyed run absent.
	}
	var page sessionListView
	u := a.endpoint("sessions")
	q := uQuery(u, map[string]string{"agent_id": a.agentID, "limit": strconv.Itoa(maxSessionList)})
	if err := a.doJSON(ctx, http.MethodGet, q, nil, &page); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			return sessionView{}, extwork.ErrNotFound
		}
		return sessionView{}, err
	}
	for _, view := range page.Data {
		if sessionMetadata(view, "hive_execution_key") != string(key) {
			continue
		}
		if requestDigest != "" && sessionMetadata(view, "hive_request_digest") != requestDigest {
			return sessionView{}, fmt.Errorf("%w: vendor metadata binds execution key to a different payload", extwork.ErrConflict)
		}
		binding := sessionBinding{
			RequestDigest: requestDigest,
			SessionID:     view.ID,
			Incarnation:   incarnation,
		}
		if requestDigest == "" {
			binding.RequestDigest = sessionMetadata(view, "hive_request_digest")
		}
		if binding.SessionID == "" {
			continue
		}
		if err := a.validateSession(view, binding, incarnation, requestDigest); err != nil {
			return sessionView{}, err
		}
		if err := a.ledger.MoveSessionReservation(string(key), view.ID); err != nil {
			return sessionView{}, fmt.Errorf("%w: bind reconciled managed agents budget reservation: %v", extwork.ErrTransport, err)
		}
		if err := a.state.put(key, binding); err != nil {
			return sessionView{}, fmt.Errorf("%w: persist reconciled managed agents session: %v", extwork.ErrTransport, err)
		}
		return view, nil
	}
	return sessionView{}, extwork.ErrNotFound
}

func (a *Adapter) validateSession(view sessionView, binding sessionBinding, incarnation, requestDigest string) error {
	if view.EnvironmentID != "" && view.EnvironmentID != a.environmentID {
		return fmt.Errorf("%w: session environment %q is not pinned %q", extwork.ErrIncarnationMismatch, view.EnvironmentID, a.environmentID)
	}
	if requestDigest != "" && sessionMetadata(view, "hive_request_digest") != "" && sessionMetadata(view, "hive_request_digest") != requestDigest {
		return fmt.Errorf("%w: session metadata request digest differs", extwork.ErrConflict)
	}
	if incarnation != "" && binding.Incarnation != "" && binding.Incarnation != incarnation {
		return fmt.Errorf("%w: session incarnation %q, pinned %q", extwork.ErrIncarnationMismatch, binding.Incarnation, incarnation)
	}
	return nil
}

type sessionBinding struct {
	RequestDigest string `json:"request_digest"`
	SessionID     string `json:"session_id"`
	Incarnation   string `json:"incarnation"`
}

type sessionStateStore struct {
	path     string
	mu       sync.Mutex
	bindings map[string]sessionBinding
}

func newSessionStateStore(dir string) (*sessionStateStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create managed agents state directory: %w", err)
	}
	s := &sessionStateStore{path: filepath.Join(dir, "sessions.json"), bindings: map[string]sessionBinding{}}
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed agents state: %w", err)
	}
	var disk struct {
		Bindings map[string]sessionBinding `json:"bindings"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		return nil, fmt.Errorf("decode managed agents state: %w", err)
	}
	if disk.Bindings != nil {
		s.bindings = disk.Bindings
	}
	return s, nil
}

func (s *sessionStateStore) get(key extwork.ExecutionKey) (sessionBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.bindings[string(key)]
	return value, ok
}

func (s *sessionStateStore) put(key extwork.ExecutionKey, binding sessionBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[string(key)] = binding
	return s.persistLocked()
}

func (s *sessionStateStore) persistLocked() error {
	raw, err := json.MarshalIndent(struct {
		Bindings map[string]sessionBinding `json:"bindings"`
	}{Bindings: s.bindings}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicWriteFile(s.path, raw, 0o600)
}

type sessionView struct {
	ID                 string              `json:"id"`
	Status             string              `json:"status"`
	EnvironmentID      string              `json:"environment_id"`
	EnvironmentVersion string              `json:"environment_version"`
	Metadata           map[string]any      `json:"metadata"`
	Usage              *sessionUsageView   `json:"usage"`
	StopReason         json.RawMessage     `json:"stop_reason"`
	ResultClass        string              `json:"result_class"`
	Receipt            *extwork.ReceiptRef `json:"receipt"`
}

type sessionListView struct {
	Data []sessionView `json:"data"`
}

type sessionUsageView struct {
	ListCost      moneyView      `json:"list_cost"`
	ActiveSeconds int64          `json:"active_seconds"`
	ServerToolUse map[string]any `json:"server_tool_use"`
}

type moneyView struct {
	Amount   json.RawMessage `json:"amount"`
	Currency string          `json:"currency"`
}

func (a *Adapter) getSession(ctx context.Context, id string) (sessionView, error) {
	if !safeID(id) {
		return sessionView{}, fmt.Errorf("%w: invalid managed agents session ID", extwork.ErrRefused)
	}
	var view sessionView
	if err := a.doJSON(ctx, http.MethodGet, a.endpoint("sessions", id), nil, &view); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			return sessionView{}, extwork.ErrNotFound
		}
		return sessionView{}, err
	}
	if view.ID == "" {
		view.ID = id
	}
	return view, nil
}

func sessionMetadata(view sessionView, key string) string {
	value, ok := view.Metadata[key]
	if !ok {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func safeID(value string) bool {
	return value != "" && !strings.ContainsAny(value, "/\\") && value != "." && value != ".."
}

// Observe reads the native session and its durable event history. Model request
// IDs are deduplicated in pkg/tokens; cumulative list cost is reconciled into
// the same ledger. A budget hit turns the run visibly waiting and interrupts a
// still-running session.
func (a *Adapter) Observe(ctx context.Context, key extwork.ExecutionKey, incarnation string) (extwork.Observation, error) {
	live, err := a.Incarnation(ctx)
	if err != nil {
		return extwork.Observation{State: extwork.StateUnknown}, err
	}
	if incarnation != "" && live != incarnation {
		return extwork.Observation{State: extwork.StateUnknown}, fmt.Errorf("%w: environment incarnation %q, pinned %q", extwork.ErrIncarnationMismatch, live, incarnation)
	}
	view, err := a.lookup(ctx, key, live, "")
	if err != nil {
		return extwork.Observation{State: extwork.StateUnknown}, err
	}
	budgetHit, err := a.meterSession(ctx, view)
	if err != nil {
		return extwork.Observation{State: extwork.StateUnknown, RemoteRunID: view.ID}, err
	}
	state := mapSessionState(view.Status, stopReasonType(view.StopReason))
	detail := stopReasonType(view.StopReason)
	if budgetHit && state == extwork.StateRunning {
		facts, interruptErr := a.sendInterrupt(ctx, view.ID)
		if interruptErr != nil {
			return extwork.Observation{State: extwork.StateUnknown, RemoteRunID: view.ID}, fmt.Errorf("%w: daily budget stop was not delivered: %v", extwork.ErrTransport, interruptErr)
		}
		state = extwork.StateWaiting
		detail = fmt.Sprintf("daily ledger budget reached; interrupt requested (acknowledged=%t stopped=%t)", facts.Acknowledged, facts.Stopped)
	} else if budgetHit {
		detail = "daily ledger budget reached"
	}
	if state == extwork.StateTerminal {
		if err := a.ledger.ReleaseSession(view.ID); err != nil {
			return extwork.Observation{State: extwork.StateUnknown, RemoteRunID: view.ID}, fmt.Errorf("%w: release managed agents budget reservation: %v", extwork.ErrTransport, err)
		}
	}
	return extwork.Observation{
		State:             state,
		RemoteRunID:       view.ID,
		RemoteIncarnation: live,
		Stage:             sessionMetadata(view, "hive_stage"),
		ResultClass:       view.ResultClass,
		Receipt:           receiptFromSession(view),
		Detail:            detail,
		ObservedAt:        a.now(),
	}, nil
}

func (a *Adapter) meterSession(ctx context.Context, view sessionView) (bool, error) {
	budgetHit := false
	if view.Usage != nil {
		hit, err := a.recordUsage(view.ID, view.Usage)
		if err != nil {
			return false, err
		}
		budgetHit = budgetHit || hit
	}
	var page eventPage
	if err := a.doJSON(ctx, http.MethodGet, uQuery(a.endpoint("sessions", view.ID, "events"), map[string]string{"beta": "true"}), nil, &page); err != nil {
		return false, err
	}
	for _, raw := range page.Events {
		hit, err := a.recordEvent(view.ID, raw)
		if err != nil {
			return false, err
		}
		budgetHit = budgetHit || hit
	}
	return budgetHit, nil
}

type eventPage struct {
	Data   []json.RawMessage `json:"data"`
	Events []json.RawMessage `json:"events"`
}

func (p *eventPage) UnmarshalJSON(raw []byte) error {
	var envelope struct {
		Data   []json.RawMessage `json:"data"`
		Events []json.RawMessage `json:"events"`
	}
	if len(raw) > 0 && raw[0] == '[' {
		var events []json.RawMessage
		if err := json.Unmarshal(raw, &events); err != nil {
			return err
		}
		p.Events = events
		return nil
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	p.Data = envelope.Data
	p.Events = envelope.Events
	if len(p.Events) == 0 {
		p.Events = p.Data
	}
	return nil
}

func (a *Adapter) recordUsage(sessionID string, usage *sessionUsageView) (bool, error) {
	cost, err := parseCents(usage.ListCost.Amount)
	if err != nil {
		return false, err
	}
	web := int64(0)
	for _, key := range []string{"web_search_requests", "web_searches"} {
		if raw, ok := usage.ServerToolUse[key]; ok {
			if n, ok := raw.(float64); ok {
				web = int64(n)
			}
		}
	}
	return a.ledger.RecordSessionUsage(sessionID, usage.ListCost.Currency, cost, usage.ActiveSeconds, web)
}

func (a *Adapter) recordEvent(sessionID string, raw json.RawMessage) (bool, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(raw, &event); err != nil {
		return false, fmt.Errorf("%w: decode managed agents event: %v", extwork.ErrTransport, err)
	}
	var eventType string
	_ = json.Unmarshal(event["type"], &eventType)
	if eventType == "session.usage" {
		usageRaw := event["usage"]
		var usage sessionUsageView
		if err := json.Unmarshal(usageRaw, &usage); err != nil {
			return false, fmt.Errorf("%w: decode session usage event: %v", extwork.ErrTransport, err)
		}
		return a.recordUsage(sessionID, &usage)
	}
	if eventType != "span.model_request_end" {
		return false, nil
	}
	var eventID string
	_ = json.Unmarshal(event["id"], &eventID)
	if strings.TrimSpace(eventID) == "" {
		hash := sha256.Sum256(raw)
		eventID = "sha256:" + hex.EncodeToString(hash[:])
	}
	var processedAtRaw string
	_ = json.Unmarshal(event["processed_at"], &processedAtRaw)
	processedAt := parseEventTime(processedAtRaw, a.now())
	model := rawString(event["model"])
	if model == "" {
		var start map[string]json.RawMessage
		_ = json.Unmarshal(event["model_request_start"], &start)
		model = rawString(start["model"])
	}
	if model == "" {
		model = a.model
	}
	usageRaw := event["model_usage"]
	if len(usageRaw) == 0 {
		usageRaw = event["usage"]
	}
	var usage map[string]json.RawMessage
	if len(usageRaw) > 0 {
		if err := json.Unmarshal(usageRaw, &usage); err != nil {
			return false, fmt.Errorf("%w: decode model usage event: %v", extwork.ErrTransport, err)
		}
	}
	input := rawInt64(usage["input_tokens"])
	output := rawInt64(usage["output_tokens"])
	cacheRead := rawInt64(usage["cache_read_input_tokens"])
	cacheCreate := rawInt64(usage["cache_creation_input_tokens"])
	_, err := a.ledger.RecordModelRequest(sessionID, eventID, a.agentID, model, processedAt, input, output, cacheRead, cacheCreate)
	return false, err
}

func parseEventTime(raw string, fallback time.Time) time.Time {
	if value, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
		return value
	}
	return fallback
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	return ""
}

func rawInt64(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return max(number, 0)
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		number, _ = strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		return max(number, 0)
	}
	return 0
}

func parseCents(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("managed agents list cost %q is not whole USD cents", text)
		}
		return value, nil
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return number, nil
	}
	return 0, errors.New("managed agents list cost amount is not an integer")
}

func stopReasonType(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var object struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Type != "" {
		return object.Type
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return ""
}

func mapSessionState(status, stopReason string) extwork.State {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "created", "queued", "starting":
		return extwork.StateAccepted
	case "running":
		return extwork.StateRunning
	case "rescheduling":
		return extwork.StateWaiting
	case "idle":
		if stopReason == "budget_reached" || stopReason == "requires_action" {
			return extwork.StateWaiting
		}
		return extwork.StateTerminal
	case "terminated", "archived":
		return extwork.StateTerminal
	default:
		return extwork.StateUnknown
	}
}

func receiptFromSession(view sessionView) *extwork.ReceiptRef {
	if view.Receipt != nil {
		return view.Receipt
	}
	path := sessionMetadata(view, "receipt_path")
	digest := sessionMetadata(view, "receipt_digest")
	sizeRaw := sessionMetadata(view, "receipt_size")
	if path == "" || digest == "" || sizeRaw == "" {
		return nil
	}
	size, err := strconv.ParseInt(sizeRaw, 10, 64)
	if err != nil || size < 0 {
		return nil
	}
	return &extwork.ReceiptRef{Path: path, Digest: digest, Size: size}
}

// Cancel sends exactly one user.interrupt event and preserves the distinction
// between delivery acknowledgement and observed termination.
func (a *Adapter) Cancel(ctx context.Context, key extwork.ExecutionKey, incarnation string) (extwork.CancelFacts, error) {
	if incarnation != "" {
		live, err := a.Incarnation(ctx)
		if err != nil {
			return extwork.CancelFacts{}, err
		}
		if live != incarnation {
			return extwork.CancelFacts{}, fmt.Errorf("%w: environment incarnation %q, pinned %q", extwork.ErrIncarnationMismatch, live, incarnation)
		}
	}
	view, err := a.lookup(ctx, key, incarnation, "")
	if err != nil {
		return extwork.CancelFacts{}, err
	}
	return a.sendInterrupt(ctx, view.ID)
}

func (a *Adapter) sendInterrupt(ctx context.Context, sessionID string) (extwork.CancelFacts, error) {
	body := map[string]any{"events": []map[string]string{{"type": "user.interrupt"}}}
	var reply struct {
		Acknowledged *bool  `json:"acknowledged"`
		Stopped      *bool  `json:"stopped"`
		Detail       string `json:"detail"`
	}
	if err := a.doJSON(ctx, http.MethodPost, uQuery(a.endpoint("sessions", sessionID, "events"), map[string]string{"beta": "true"}), body, &reply); err != nil {
		return extwork.CancelFacts{Requested: true, Detail: err.Error()}, err
	}
	acknowledged := true
	if reply.Acknowledged != nil {
		acknowledged = *reply.Acknowledged
	}
	stopped := false
	if reply.Stopped != nil {
		stopped = *reply.Stopped
	}
	return extwork.CancelFacts{Requested: true, Acknowledged: acknowledged, Stopped: stopped, Detail: reply.Detail}, nil
}

// OpenArtifact resolves a session-scoped output through the vendor Files API.
// The binding still enforces the relative-path, size, digest, and receipt
// schema rules after it receives this stream.
func (a *Adapter) OpenArtifact(ctx context.Context, key extwork.ExecutionKey, incarnation, artifactPath string) (io.ReadCloser, error) {
	clean, err := extwork.CleanArtifactPath(artifactPath)
	if err != nil {
		return nil, err
	}
	if incarnation != "" {
		live, err := a.Incarnation(ctx)
		if err != nil {
			return nil, err
		}
		if live != incarnation {
			return nil, fmt.Errorf("%w: environment incarnation %q, pinned %q", extwork.ErrIncarnationMismatch, live, incarnation)
		}
	}
	view, err := a.lookup(ctx, key, incarnation, "")
	if err != nil {
		return nil, err
	}
	var files fileListView
	u := uQuery(a.endpoint("files"), map[string]string{"scope_id": view.ID, "beta": "true"})
	if err := a.doJSON(ctx, http.MethodGet, u, nil, &files); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			return nil, extwork.ErrNotFound
		}
		return nil, err
	}
	var match string
	for _, file := range files.Data {
		if !safeID(file.ID) || !fileMatchesPath(file.Filename, clean) {
			continue
		}
		if match != "" {
			return nil, fmt.Errorf("%w: artifact path %q is ambiguous", extwork.ErrRefused, clean)
		}
		match = file.ID
	}
	if match == "" {
		return nil, extwork.ErrNotFound
	}
	return a.openAuthenticated(ctx, a.endpoint("files", match, "content"))
}

type fileListView struct {
	Data []struct {
		ID       string `json:"id"`
		Filename string `json:"filename"`
	} `json:"data"`
}

func fileMatchesPath(filename, wanted string) bool {
	filename = strings.TrimPrefix(strings.TrimSpace(filename), "/")
	filename = strings.TrimPrefix(filename, "mnt/session/")
	return filename == wanted || strings.TrimPrefix(filename, "outputs/") == wanted || pathBase(filename) == pathBase(wanted)
}

func pathBase(value string) string {
	value = strings.TrimRight(value, "/")
	if index := strings.LastIndexByte(value, '/'); index >= 0 {
		return value[index+1:]
	}
	return value
}

func (a *Adapter) openAuthenticated(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	key, err := a.apiKey()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	a.setHeaders(req, key)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", extwork.ErrTransport, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, extwork.ErrNotFound
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: artifact fetch replied %d", extwork.ErrTransport, resp.StatusCode)
	}
	return resp.Body, nil
}

func (a *Adapter) endpoint(parts ...string) string {
	all := append([]string{"v1"}, parts...)
	return a.base.JoinPath(all...).String()
}

func uQuery(rawURL string, values map[string]string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	for key, value := range values {
		q.Set(key, value)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

type apiError struct {
	status int
	typ    string
	body   string
}

func (e *apiError) Error() string {
	if e.typ == "" {
		return fmt.Sprintf("managed agents replied %d: %s", e.status, e.body)
	}
	return fmt.Sprintf("managed agents replied %d %s: %s", e.status, e.typ, e.body)
}

func (a *Adapter) apiKey() (string, error) {
	if a.apiKeyFile != "" {
		raw, err := os.ReadFile(a.apiKeyFile)
		if err != nil {
			return "", fmt.Errorf("%w: read managed agents API key reference", extwork.ErrRefused)
		}
		if key := strings.TrimSpace(string(raw)); key != "" {
			return key, nil
		}
		return "", fmt.Errorf("%w: managed agents API key file is empty", extwork.ErrRefused)
	}
	if key := strings.TrimSpace(os.Getenv(a.apiKeyEnv)); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("%w: managed agents API key environment variable %q is empty", extwork.ErrRefused, a.apiKeyEnv)
}

func (a *Adapter) setHeaders(req *http.Request, key string) {
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", AnthropicVersion)
	req.Header.Set("anthropic-beta", ManagedAgentsBetaHeader)
	req.Header.Set("Accept", "application/json")
}

func (a *Adapter) doJSON(ctx context.Context, method, rawURL string, body any, out any) error {
	key, err := a.apiKey()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	a.setHeaders(req, key)
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", extwork.ErrTransport, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		var typed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &typed)
		return &apiError{status: resp.StatusCode, typ: typed.Type, body: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: decode response: %v", extwork.ErrTransport, err)
	}
	return nil
}

func atomicWriteFile(path string, raw []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".managed-agents-state-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmp)
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
