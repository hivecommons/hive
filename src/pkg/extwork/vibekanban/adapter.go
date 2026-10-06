// Package vibekanban binds Hive's external-execution contract to a local
// vibe-kanban MCP server. The adapter is default-off at Hive wiring time and
// creates only board/workspace state through the configured local MCP command.
package vibekanban

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
)

const (
	// Engine is the extwork registry name.
	Engine = "vibe-kanban"
	// Capability is the contributor-protocol capability for vibe-kanban-bound work.
	Capability = "ext-exec/vibe-kanban"

	// SettingMCPCommand is the registry setting for the MCP stdio command.
	SettingMCPCommand = "mcp_command"
	// SettingProjectID is the registry setting for the target vibe-kanban project.
	SettingProjectID = "project_id"
	// SettingStateDir is the registry setting for adapter dispatch records.
	SettingStateDir = "state_dir"
	// SettingWorkflowVersion pins the operator-declared adapter contract.
	SettingWorkflowVersion = extwork.SettingWorkflowVersion

	defaultTodoStatus       = "To do"
	defaultInProgressStatus = "In progress"
	defaultInReviewStatus   = "In review"
	defaultDoneStatus       = "Done"
	defaultFailedStatus     = "Cancelled"
	hiveTagName             = "hive"
	recordFilePerm          = 0o600
	recordDirPerm           = 0o755
)

// Config configures an Adapter.
type Config struct {
	MCPCommand      string
	ProjectID       string
	StateDir        string
	WorkflowVersion string
	Client          ToolClient
	Now             func() time.Time
}

// ToolClient is the minimal MCP tool surface used by the adapter. Tests use an
// in-process fake; production uses a stdio JSON-RPC MCP client.
type ToolClient interface {
	CallTool(ctx context.Context, name string, args map[string]any) (map[string]any, error)
}

// Adapter implements extwork.Adapter for vibe-kanban.
type Adapter struct {
	projectID string
	stateDir  string
	version   string
	client    ToolClient
	now       func() time.Time
}

// New validates cfg and builds an adapter.
func New(cfg Config) (*Adapter, error) {
	if strings.TrimSpace(cfg.ProjectID) == "" {
		return nil, errors.New("vibe-kanban project id is required")
	}
	if strings.TrimSpace(cfg.WorkflowVersion) == "" {
		return nil, errors.New("vibe-kanban workflow version is required")
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return nil, errors.New("vibe-kanban state dir is required")
	}
	client := cfg.Client
	if client == nil {
		if strings.TrimSpace(cfg.MCPCommand) == "" {
			return nil, errors.New("vibe-kanban MCP command is required")
		}
		client = NewStdioClient(strings.TrimSpace(cfg.MCPCommand))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{projectID: strings.TrimSpace(cfg.ProjectID), stateDir: cfg.StateDir, version: strings.TrimSpace(cfg.WorkflowVersion), client: client, now: now}, nil
}

// Factory is the extwork.Registry constructor.
func Factory(settings map[string]string) (extwork.Adapter, error) {
	return New(Config{
		MCPCommand:      settings[SettingMCPCommand],
		ProjectID:       settings[SettingProjectID],
		StateDir:        settings[SettingStateDir],
		WorkflowVersion: settings[SettingWorkflowVersion],
	})
}

// Engine returns "vibe-kanban".
func (a *Adapter) Engine() string { return Engine }

// WorkflowVersion returns the pinned adapter contract version.
func (a *Adapter) WorkflowVersion() string { return a.version }

// Incarnation implements extwork.Pinner. The configured project is the native
// scope whose cards/workspaces may be adopted after restart.
func (a *Adapter) Incarnation(context.Context) (string, error) { return a.projectID, nil }

// Admit implements extwork.Gate. vibe-kanban receives only report-only starts;
// shadow mode is handled by the binding without reaching Start.
func (a *Adapter) Admit(adm extwork.Admission) error {
	if adm.Authority.Capability != Capability {
		return fmt.Errorf("%w: authority capability %q is not %s", extwork.ErrRefused, adm.Authority.Capability, Capability)
	}
	if adm.Engine != Engine {
		return fmt.Errorf("%w: admission engine %q is not %s", extwork.ErrRefused, adm.Engine, Engine)
	}
	if adm.WorkflowVersion != a.version {
		return fmt.Errorf("%w: admission workflow version %q is not the pinned %q", extwork.ErrRefused, adm.WorkflowVersion, a.version)
	}
	switch adm.Authority.Mode {
	case extwork.ModeReportOnly, extwork.ModeShadow:
		return nil
	default:
		return fmt.Errorf("%w: mode %q", extwork.ErrRefused, adm.Authority.Mode)
	}
}

// Start creates or reuses the correlated board card, then starts a workspace
// through vibe-kanban's start_workspace MCP tool.
func (a *Adapter) Start(ctx context.Context, req extwork.StartRequest) (extwork.StartResult, error) {
	adm := req.Admission
	if err := adm.Validate(); err != nil {
		return extwork.StartResult{}, err
	}
	if err := a.Admit(adm); err != nil {
		return extwork.StartResult{}, err
	}
	if adm.RequestDigest != extwork.RequestDigest(req.Payload) {
		return extwork.StartResult{}, extwork.ErrPayloadDigest
	}
	if adm.EngineIncarnation != "" && adm.EngineIncarnation != a.projectID {
		return extwork.StartResult{}, fmt.Errorf("%w: project %q, pinned %q", extwork.ErrIncarnationMismatch, a.projectID, adm.EngineIncarnation)
	}
	key := adm.ExecutionKey()
	if rec, ok, err := a.loadByWorkKey(adm.WorkKey); err != nil {
		return extwork.StartResult{}, err
	} else if ok && rec.ExecutionKey == string(key) {
		if rec.ProjectID != a.projectID {
			return extwork.StartResult{}, fmt.Errorf("%w: record project %q, configured %q", extwork.ErrIncarnationMismatch, rec.ProjectID, a.projectID)
		}
		if rec.RequestDigest != adm.RequestDigest {
			return extwork.StartResult{}, fmt.Errorf("%w: work key %s already dispatched under this execution key with a different payload", extwork.ErrConflict, adm.WorkKey)
		}
		if rec.WorkspaceID != "" {
			return extwork.StartResult{RemoteRunID: rec.WorkspaceID, RemoteIncarnation: rec.ProjectID, Deduplicated: true}, nil
		}
		return a.startWorkspace(ctx, rec, req.Payload, true)
	} else if ok {
		return extwork.StartResult{}, fmt.Errorf("%w: work key %s is already mapped to execution %s", extwork.ErrConflict, adm.WorkKey, rec.ExecutionKey)
	}

	now := a.now().UTC()
	rec := dispatchRecord{
		WorkKey:       adm.WorkKey,
		ExecutionKey:  string(key),
		AssignmentID:  adm.AssignmentID,
		RequestDigest: adm.RequestDigest,
		ProjectID:     a.projectID,
		Stage:         adm.Stage,
		CreatedAt:     now,
		UpdatedAt:     now,
		Status:        defaultTodoStatus,
	}
	issueID, title, err := a.ensureCard(ctx, adm, req.Payload)
	if err != nil {
		return extwork.StartResult{}, err
	}
	rec.CardID = issueID
	rec.CardTitle = title
	if err := a.saveRecord(rec); err != nil {
		return extwork.StartResult{}, err
	}
	return a.startWorkspace(ctx, rec, req.Payload, false)
}

// Observe reattaches by reading the local dispatch record and then querying the
// card by the hive-work-key trailer. The native MCP surface does not expose a
// reliable workspace→issue edge, so the local record is the correlation source.
func (a *Adapter) Observe(ctx context.Context, key extwork.ExecutionKey, incarnation string) (extwork.Observation, error) {
	rec, ok, err := a.loadByExecutionKey(key)
	if err != nil {
		return extwork.Observation{State: extwork.StateUnknown}, err
	}
	if !ok {
		return extwork.Observation{State: extwork.StateUnknown}, extwork.ErrNotFound
	}
	if rec.ProjectID != a.projectID {
		return extwork.Observation{State: extwork.StateUnknown}, fmt.Errorf("%w: record project %q, configured %q", extwork.ErrIncarnationMismatch, rec.ProjectID, a.projectID)
	}
	if incarnation != "" && incarnation != rec.ProjectID {
		return extwork.Observation{State: extwork.StateUnknown}, fmt.Errorf("%w: project %q, pinned %q", extwork.ErrIncarnationMismatch, rec.ProjectID, incarnation)
	}
	issue, err := a.findCard(ctx, rec.WorkKey)
	if err != nil {
		return extwork.Observation{State: extwork.StateUnknown}, err
	}
	status := stringField(issue, "status")
	if status == "" {
		status = rec.Status
	}
	obs := extwork.Observation{State: stateFromStatus(status), RemoteRunID: rec.WorkspaceID, RemoteIncarnation: rec.ProjectID, Stage: rec.Stage, Detail: status, ObservedAt: a.now().UTC()}
	if obs.State == extwork.StateTerminal {
		if strings.EqualFold(status, defaultFailedStatus) {
			obs.ResultClass = "failed"
		} else {
			obs.ResultClass = "done"
		}
	}
	return obs, nil
}

// Cancel moves the correlated card to the configured failed/cancelled column.
func (a *Adapter) Cancel(ctx context.Context, key extwork.ExecutionKey, incarnation string) (extwork.CancelFacts, error) {
	rec, ok, err := a.loadByExecutionKey(key)
	if err != nil {
		return extwork.CancelFacts{}, err
	}
	if !ok {
		return extwork.CancelFacts{}, extwork.ErrNotFound
	}
	if rec.ProjectID != a.projectID {
		return extwork.CancelFacts{}, fmt.Errorf("%w: record project %q, configured %q", extwork.ErrIncarnationMismatch, rec.ProjectID, a.projectID)
	}
	if incarnation != "" && incarnation != rec.ProjectID {
		return extwork.CancelFacts{}, fmt.Errorf("%w: project %q, pinned %q", extwork.ErrIncarnationMismatch, rec.ProjectID, incarnation)
	}
	_, err = a.client.CallTool(ctx, "update_issue", map[string]any{"issue_id": rec.CardID, "status": defaultFailedStatus})
	if err != nil {
		return extwork.CancelFacts{Requested: true, Detail: err.Error()}, err
	}
	rec.Status = defaultFailedStatus
	rec.UpdatedAt = a.now().UTC()
	_ = a.saveRecord(rec)
	return extwork.CancelFacts{Requested: true, Acknowledged: true, Detail: "vibe-kanban card moved to cancelled"}, nil
}

// OpenArtifact is unsupported: vibe-kanban's MCP surface does not expose run
// artifacts under the board/workspace tools used here.
func (a *Adapter) OpenArtifact(context.Context, extwork.ExecutionKey, string, string) (io.ReadCloser, error) {
	return nil, extwork.ErrNotFound
}

type dispatchRecord struct {
	WorkKey       string    `json:"work_key"`
	ExecutionKey  string    `json:"execution_key"`
	AssignmentID  string    `json:"assignment_id"`
	RequestDigest string    `json:"request_digest"`
	ProjectID     string    `json:"project_id"`
	CardID        string    `json:"card_id"`
	CardTitle     string    `json:"card_title"`
	WorkspaceID   string    `json:"workspace_id,omitempty"`
	Stage         string    `json:"stage,omitempty"`
	Status        string    `json:"status,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (a *Adapter) ensureCard(ctx context.Context, adm extwork.Admission, payload []byte) (string, string, error) {
	if existing, err := a.findCard(ctx, adm.WorkKey); err == nil {
		id := stringField(existing, "id")
		if id != "" {
			return id, stringField(existing, "title"), nil
		}
	} else if !errors.Is(err, extwork.ErrNotFound) {
		return "", "", err
	}
	title := cardTitle(adm)
	desc := cardDescription(adm, payload)
	created, err := a.client.CallTool(ctx, "create_issue", map[string]any{"project_id": a.projectID, "title": title, "description": desc})
	if err != nil {
		return "", "", fmt.Errorf("%w: create_issue: %v", extwork.ErrTransport, err)
	}
	issueID := stringField(created, "issue_id")
	if issueID == "" {
		return "", "", fmt.Errorf("%w: create_issue returned no issue_id", extwork.ErrRefused)
	}
	_, err = a.client.CallTool(ctx, "update_issue", map[string]any{"issue_id": issueID, "status": defaultTodoStatus})
	if err != nil {
		return "", "", err
	}
	_ = a.addHiveTag(ctx, issueID)
	return issueID, title, nil
}

func (a *Adapter) addHiveTag(ctx context.Context, issueID string) error {
	tags, err := a.client.CallTool(ctx, "list_tags", map[string]any{"project_id": a.projectID})
	if err != nil {
		return err
	}
	for _, raw := range sliceField(tags, "tags") {
		m, _ := raw.(map[string]any)
		if strings.EqualFold(stringField(m, "name"), hiveTagName) {
			if tagID := stringField(m, "id"); tagID != "" {
				_, err := a.client.CallTool(ctx, "add_issue_tag", map[string]any{"issue_id": issueID, "tag_id": tagID})
				return err
			}
		}
	}
	return nil
}

func (a *Adapter) startWorkspace(ctx context.Context, rec dispatchRecord, payload []byte, dedup bool) (extwork.StartResult, error) {
	prompt := workspacePrompt(rec, payload)
	res, err := a.client.CallTool(ctx, "start_workspace", map[string]any{"issue_id": rec.CardID, "prompt": prompt})
	if err != nil {
		return extwork.StartResult{}, fmt.Errorf("%w: start_workspace: %v", extwork.ErrRefused, err)
	}
	workspaceID := firstString(res, "workspace_id", "id")
	if workspaceID == "" {
		return extwork.StartResult{}, fmt.Errorf("%w: start_workspace returned no workspace id", extwork.ErrRefused)
	}
	rec.WorkspaceID = workspaceID
	rec.Status = defaultInProgressStatus
	rec.UpdatedAt = a.now().UTC()
	if err := a.saveRecord(rec); err != nil {
		return extwork.StartResult{RemoteRunID: workspaceID, RemoteIncarnation: rec.ProjectID, Deduplicated: dedup}, err
	}
	return extwork.StartResult{RemoteRunID: workspaceID, RemoteIncarnation: rec.ProjectID, Deduplicated: dedup}, nil
}

func (a *Adapter) findCard(ctx context.Context, workKey string) (map[string]any, error) {
	found, err := a.client.CallTool(ctx, "list_issues", map[string]any{"project_id": a.projectID, "search": trailer(workKey), "limit": 5})
	if err != nil {
		return nil, fmt.Errorf("%w: list_issues: %v", extwork.ErrTransport, err)
	}
	issues := sliceField(found, "issues")
	if len(issues) == 0 {
		return nil, extwork.ErrNotFound
	}
	m, ok := issues[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: list_issues returned malformed issue", extwork.ErrTransport)
	}
	return m, nil
}

func cardTitle(adm extwork.Admission) string {
	return fmt.Sprintf("Hive %s %s", adm.WorkKey, adm.Stage)
}

func cardDescription(adm extwork.Admission, payload []byte) string {
	return strings.Join([]string{
		"Dispatched by hive as external report-only work. Hive owns admission and acceptance.",
		"",
		"The workspace prompt/assignment is carried below; do not treat board state as acceptance.",
		"",
		"```json",
		string(payload),
		"```",
		"",
		trailer(adm.WorkKey),
		fmt.Sprintf("[hive-execution-key: %s]", adm.ExecutionKey()),
		fmt.Sprintf("[hive-assignment-id: %s]", adm.AssignmentID),
	}, "\n")
}

func workspacePrompt(rec dispatchRecord, payload []byte) string {
	return strings.Join([]string{
		"Hive external assignment. Follow the bundled prompt and assignment exactly; do not publish or merge from vibe-kanban.",
		"",
		fmt.Sprintf("Hive work key: %s", rec.WorkKey),
		fmt.Sprintf("Hive execution key: %s", rec.ExecutionKey),
		"",
		string(payload),
	}, "\n")
}

func trailer(workKey string) string { return fmt.Sprintf("[hive-work-key: %s]", workKey) }

func stateFromStatus(status string) extwork.State {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case strings.ToLower(defaultInProgressStatus):
		return extwork.StateRunning
	case strings.ToLower(defaultInReviewStatus):
		return extwork.StateWaiting
	case strings.ToLower(defaultDoneStatus), strings.ToLower(defaultFailedStatus):
		return extwork.StateTerminal
	case strings.ToLower(defaultTodoStatus), "backlog", "":
		return extwork.StateAccepted
	default:
		return extwork.StateUnknown
	}
}

func (a *Adapter) recordPath(workKey string) string {
	sum := sha256.Sum256([]byte(workKey))
	return filepath.Join(a.stateDir, hex.EncodeToString(sum[:])+".dispatch.json")
}

func (a *Adapter) saveRecord(rec dispatchRecord) error {
	if strings.TrimSpace(rec.WorkKey) == "" {
		return errors.New("vibe-kanban dispatch record missing work key")
	}
	if err := os.MkdirAll(a.stateDir, recordDirPerm); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := a.recordPath(rec.WorkKey)
	tmp, err := os.CreateTemp(a.stateDir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(recordFilePerm); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	keep = true
	return nil
}

func (a *Adapter) loadByWorkKey(workKey string) (dispatchRecord, bool, error) {
	raw, err := os.ReadFile(a.recordPath(workKey))
	if errors.Is(err, os.ErrNotExist) {
		return dispatchRecord{}, false, nil
	}
	if err != nil {
		return dispatchRecord{}, false, err
	}
	var rec dispatchRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return dispatchRecord{}, false, fmt.Errorf("corrupt vibe-kanban dispatch record for %s: %w", workKey, err)
	}
	return rec, true, nil
}

func (a *Adapter) loadByExecutionKey(key extwork.ExecutionKey) (dispatchRecord, bool, error) {
	entries, err := os.ReadDir(a.stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return dispatchRecord{}, false, nil
	}
	if err != nil {
		return dispatchRecord{}, false, err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".dispatch.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(a.stateDir, ent.Name()))
		if err != nil {
			return dispatchRecord{}, false, err
		}
		var rec dispatchRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return dispatchRecord{}, false, fmt.Errorf("corrupt vibe-kanban dispatch record %s: %w", ent.Name(), err)
		}
		if rec.ExecutionKey == string(key) {
			return rec, true, nil
		}
	}
	return dispatchRecord{}, false, nil
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v := stringField(m, key); v != "" {
			return v
		}
	}
	return ""
}

func sliceField(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	v, _ := m[key].([]any)
	return v
}
