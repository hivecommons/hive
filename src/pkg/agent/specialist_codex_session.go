package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	SpecialistTaskResponseSchema      = "hive.specialist-task-response.v1"
	maxSpecialistDispatchMessageBytes = 3 << 20
	maxSpecialistTaskResponseBytes    = 256 << 10
	maxSpecialistCodexRolloutBytes    = 32 << 20
	maxSpecialistCodexRolloutLine     = 4 << 20
	maxSpecialistCodexRolloutFiles    = 512
)

var ErrSpecialistTaskResponsePending = errors.New("specialist task response is pending")

// SpecialistTaskResponseRequest binds observation to the exact message that
// Hive delivered to one existing specialist session. Repository output cannot
// select a different turn, session, role, or work order.
type SpecialistTaskResponseRequest struct {
	TaskID     string         `json:"task_id"`
	Specialist SpecialistRole `json:"specialist"`
	SessionID  string         `json:"session_id"`
	Message    string         `json:"message"`
}

// SpecialistTaskCompletionVerificationRequest asks the ordinary Manager to
// prove that governed mailbox provenance came from its exact retained child
// transport evidence. It grants no launch or observation capability.
type SpecialistTaskCompletionVerificationRequest struct {
	TaskID        string                             `json:"task_id"`
	Specialist    SpecialistRole                     `json:"specialist"`
	RequestSHA256 string                             `json:"request_sha256"`
	SessionID     string                             `json:"session_id"`
	Message       string                             `json:"message"`
	Provenance    SpecialistContainedChildProvenance `json:"provenance"`
}

// SpecialistTaskResponse is raw proposal text observed from Codex's private,
// controller-owned session journal. It is transport evidence only. The repair
// broker still validates the model envelope and constructs the authoritative
// SpecialistReceipt itself.
type SpecialistTaskResponse struct {
	SchemaVersion         string         `json:"schema_version"`
	WorkOrderID           string         `json:"work_order_id"`
	Specialist            SpecialistRole `json:"specialist"`
	SessionID             string         `json:"session_id"`
	TurnID                string         `json:"turn_id"`
	ProviderSHA256        string         `json:"provider_sha256"`
	ExecutorBackend       string         `json:"executor_backend,omitempty"`
	ExecutorModel         string         `json:"executor_model,omitempty"`
	ExecutorConfigSHA256  string         `json:"executor_config_sha256,omitempty"`
	AuthorizationSHA256   string         `json:"authorization_sha256,omitempty"`
	ContainmentProfile    string         `json:"containment_profile,omitempty"`
	DispatchSHA256        string         `json:"dispatch_sha256"`
	ResponseSHA256        string         `json:"response_sha256"`
	IntentSHA256          string         `json:"intent_sha256,omitempty"`
	StartedSHA256         string         `json:"started_sha256,omitempty"`
	CompletionSpoolSHA256 string         `json:"-"`
	Response              string         `json:"response"`
	CompletedAt           time.Time      `json:"completed_at"`
}

type codexRolloutEvent struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMeta struct {
	CWD string `json:"cwd"`
}

type codexTaskEvent struct {
	Type             string      `json:"type"`
	TurnID           string      `json:"turn_id"`
	CompletedAt      json.Number `json:"completed_at"`
	LastAgentMessage *string     `json:"last_agent_message"`
}

type codexMessageEvent struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Metadata struct {
		TurnID string `json:"turn_id"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

type codexResponseCandidate struct {
	turnID      string
	response    string
	completedAt time.Time
}

// ObserveSpecialistTaskResponse performs one bounded observation of Codex's
// private journals. Callers own polling and deadlines so they can also recover
// legacy mailbox completions without racing a second delivery.
func (m *Manager) ObserveSpecialistTaskResponse(ctx context.Context, request SpecialistTaskResponseRequest) (SpecialistTaskResponse, error) {
	if m.specialistChildDispatcherConfigured() {
		return m.observeSpecialistChildResponse(ctx, request)
	}
	if !m.legacySpecialistManagerConfigured() {
		return SpecialistTaskResponse{}, errors.New("ordinary manager has no explicitly injected contained specialist child executor")
	}
	if ctx == nil {
		return SpecialistTaskResponse{}, errors.New("specialist task response observation requires a context")
	}
	select {
	case <-ctx.Done():
		return SpecialistTaskResponse{}, ctx.Err()
	default:
	}
	if err := validateSpecialistTaskResponseRequest(request); err != nil {
		return SpecialistTaskResponse{}, err
	}

	m.mu.RLock()
	if m.specialistsDown {
		m.mu.RUnlock()
		return SpecialistTaskResponse{}, errors.New("specialist manager is shut down")
	}
	agent := m.agents[string(request.Specialist)]
	leased := m.specialistLeases[string(request.Specialist)]
	workRoot := m.workDir
	provider := m.specialistProvider
	sessionID := ""
	if agent != nil {
		sessionID = agent.tmuxSession
	}
	m.mu.RUnlock()
	if agent == nil || sessionID != request.SessionID {
		return SpecialistTaskResponse{}, errors.New("specialist response session does not match the configured role")
	}
	if leased != "" && leased != request.TaskID {
		return SpecialistTaskResponse{}, fmt.Errorf("specialist %s is leased to a different task", request.Specialist)
	}
	if provider == nil || !digestPattern.MatchString(provider.SHA256) {
		return SpecialistTaskResponse{}, errors.New("specialist response has no sealed provider identity")
	}
	if err := revalidateSpecialistProvider(provider); err != nil {
		return SpecialistTaskResponse{}, fmt.Errorf("revalidate specialist response provider: %w", err)
	}
	roleWorkDir := filepath.Join(workRoot, string(request.Specialist))
	root := filepath.Join(workRoot, ".codex-homes")
	candidate, err := findSpecialistCodexResponse(ctx, root, roleWorkDir, request.Message)
	if err != nil {
		return SpecialistTaskResponse{}, err
	}
	dispatchDigest := sha256.Sum256([]byte(request.Message))
	responseDigest := sha256.Sum256([]byte(candidate.response))
	return SpecialistTaskResponse{
		SchemaVersion:  SpecialistTaskResponseSchema,
		WorkOrderID:    request.TaskID,
		Specialist:     request.Specialist,
		SessionID:      request.SessionID,
		TurnID:         candidate.turnID,
		ProviderSHA256: provider.SHA256,
		DispatchSHA256: hex.EncodeToString(dispatchDigest[:]),
		ResponseSHA256: hex.EncodeToString(responseDigest[:]),
		Response:       candidate.response,
		CompletedAt:    candidate.completedAt,
	}, nil
}

// VerifySpecialistTaskCompletion is deliberately child-only and pane-blind.
// Governed replay must call it before trusting an existing mailbox receipt.
func (m *Manager) VerifySpecialistTaskCompletion(ctx context.Context, request SpecialistTaskCompletionVerificationRequest) (SpecialistTaskResponse, error) {
	if !m.specialistChildDispatcherConfigured() {
		return SpecialistTaskResponse{}, errors.New("contained specialist child dispatcher is not configured")
	}
	return m.verifySpecialistChildCompletion(ctx, request)
}

func validateSpecialistTaskResponseRequest(request SpecialistTaskResponseRequest) error {
	if !workOrderIDPattern.MatchString(request.TaskID) {
		return errors.New("specialist response task ID must be an immutable work-order ID")
	}
	if !isAllowedSpecialistRole(request.Specialist) {
		return fmt.Errorf("unsupported specialist role %q", request.Specialist)
	}
	if !safeIdentityPattern.MatchString(request.SessionID) {
		return errors.New("specialist response session ID is invalid")
	}
	if strings.TrimSpace(request.Message) == "" || len(request.Message) > maxSpecialistDispatchMessageBytes || strings.IndexByte(request.Message, 0) >= 0 {
		return errors.New("specialist response dispatch message is missing or exceeds its size limit")
	}
	return nil
}

func findSpecialistCodexResponse(ctx context.Context, root, roleWorkDir, exactMessage string) (codexResponseCandidate, error) {
	root = filepath.Clean(root)
	roleWorkDir = filepath.Clean(roleWorkDir)
	if !filepath.IsAbs(root) || !filepath.IsAbs(roleWorkDir) {
		return codexResponseCandidate{}, errors.New("specialist Codex journal paths must be absolute")
	}
	if err := rejectSpecialistProviderLinkedPath(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codexResponseCandidate{}, ErrSpecialistTaskResponsePending
		}
		return codexResponseCandidate{}, fmt.Errorf("validate specialist Codex journal root: %w", err)
	}
	files, err := specialistCodexRolloutFiles(ctx, root)
	if err != nil {
		return codexResponseCandidate{}, err
	}
	var matches []codexResponseCandidate
	matchedDeliveries := 0
	for _, file := range files {
		candidate, matched, err := parseSpecialistCodexRollout(file, roleWorkDir, exactMessage)
		if errors.Is(err, ErrSpecialistTaskResponsePending) {
			if matched {
				matchedDeliveries++
			}
			continue
		}
		if err != nil {
			return codexResponseCandidate{}, err
		}
		if matched {
			matchedDeliveries++
		}
		if candidate.response != "" {
			matches = append(matches, candidate)
		}
	}
	if matchedDeliveries > 1 || len(matches) > 1 {
		return codexResponseCandidate{}, errors.New("specialist dispatch appears in more than one Codex turn")
	}
	if len(matches) == 0 {
		return codexResponseCandidate{}, ErrSpecialistTaskResponsePending
	}
	return matches[0], nil
}

func specialistCodexRolloutFiles(ctx context.Context, root string) ([]string, error) {
	var files []string
	entries := 0
	homes, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("inventory specialist Codex homes: %w", err)
	}
	for _, homeEntry := range homes {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if !strings.HasPrefix(homeEntry.Name(), "manager-") || len(homeEntry.Name()) <= len("manager-") {
			continue
		}
		home := filepath.Join(root, homeEntry.Name())
		if err := requireSpecialistCodexJournalDirectory(home); err != nil {
			return nil, fmt.Errorf("validate specialist Codex home: %w", err)
		}
		sessions := filepath.Join(home, "sessions")
		if err := requireSpecialistCodexJournalDirectory(sessions); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("validate specialist Codex sessions: %w", err)
		}
		years, err := os.ReadDir(sessions)
		if err != nil {
			return nil, err
		}
		for _, year := range years {
			if !validCodexDateComponent(year.Name(), 4) {
				continue
			}
			yearPath := filepath.Join(sessions, year.Name())
			if err := requireSpecialistCodexJournalDirectory(yearPath); err != nil {
				return nil, err
			}
			months, err := os.ReadDir(yearPath)
			if err != nil {
				return nil, err
			}
			for _, month := range months {
				if !validCodexDateComponent(month.Name(), 2) {
					continue
				}
				monthPath := filepath.Join(yearPath, month.Name())
				if err := requireSpecialistCodexJournalDirectory(monthPath); err != nil {
					return nil, err
				}
				days, err := os.ReadDir(monthPath)
				if err != nil {
					return nil, err
				}
				for _, day := range days {
					if !validCodexDateComponent(day.Name(), 2) {
						continue
					}
					dayPath := filepath.Join(monthPath, day.Name())
					if err := requireSpecialistCodexJournalDirectory(dayPath); err != nil {
						return nil, err
					}
					rollouts, err := os.ReadDir(dayPath)
					if err != nil {
						return nil, err
					}
					for _, rollout := range rollouts {
						entries++
						if entries > maxSpecialistCodexRolloutFiles*4 {
							return nil, errors.New("specialist Codex journal inventory exceeds its bound")
						}
						if !strings.HasPrefix(rollout.Name(), "rollout-") || !strings.HasSuffix(rollout.Name(), ".jsonl") {
							continue
						}
						path := filepath.Join(dayPath, rollout.Name())
						info, err := os.Lstat(path)
						reparse, reparseErr := specialistProviderPathIsReparsePoint(path)
						if err != nil || reparseErr != nil || info.Mode()&os.ModeSymlink != 0 || reparse || !info.Mode().IsRegular() {
							return nil, errors.New("specialist Codex rollout is not an ordinary file")
						}
						if len(files) >= maxSpecialistCodexRolloutFiles {
							return nil, errors.New("specialist Codex rollout count exceeds its bound")
						}
						files = append(files, path)
					}
				}
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

func requireSpecialistCodexJournalDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	reparse, reparseErr := specialistProviderPathIsReparsePoint(path)
	if reparseErr != nil {
		return reparseErr
	}
	if info.Mode()&os.ModeSymlink != 0 || reparse || !info.IsDir() {
		return errors.New("specialist Codex journal contains a linked or non-directory ancestor")
	}
	return nil
}

func validCodexDateComponent(value string, width int) bool {
	if len(value) != width {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func parseSpecialistCodexRollout(path, roleWorkDir, exactMessage string) (codexResponseCandidate, bool, error) {
	data, err := readStableSpecialistCodexRollout(path)
	if err != nil {
		return codexResponseCandidate{}, false, err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return codexResponseCandidate{}, false, ErrSpecialistTaskResponsePending
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), maxSpecialistCodexRolloutLine)
	lineNumber := 0
	sessionMetaCount := 0
	sessionCWD := ""
	startedTurns := make(map[string]bool)
	matchedTurn := ""
	matchedAt := time.Time{}
	var candidate codexResponseCandidate
	completionCount := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(line) == 0 {
			return codexResponseCandidate{}, false, fmt.Errorf("specialist Codex journal %s has an empty record", filepath.Base(path))
		}
		var event codexRolloutEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return codexResponseCandidate{}, false, fmt.Errorf("decode specialist Codex journal %s line %d: %w", filepath.Base(path), lineNumber, err)
		}
		switch event.Type {
		case "session_meta":
			var meta codexSessionMeta
			if err := json.Unmarshal(event.Payload, &meta); err != nil {
				return codexResponseCandidate{}, false, errors.New("decode specialist Codex session metadata")
			}
			sessionMetaCount++
			sessionCWD = filepath.Clean(meta.CWD)
		case "event_msg":
			var task codexTaskEvent
			decoder := json.NewDecoder(bytes.NewReader(event.Payload))
			decoder.UseNumber()
			if err := decoder.Decode(&task); err != nil {
				continue
			}
			switch task.Type {
			case "task_started":
				if safeIdentityPattern.MatchString(task.TurnID) {
					startedTurns[task.TurnID] = true
				}
			case "task_complete":
				if matchedTurn == "" || task.TurnID != matchedTurn || !startedTurns[matchedTurn] {
					continue
				}
				completionCount++
				if completionCount > 1 {
					return codexResponseCandidate{}, true, errors.New("specialist Codex turn has duplicate completion records")
				}
				if task.LastAgentMessage == nil || strings.TrimSpace(*task.LastAgentMessage) == "" || len(*task.LastAgentMessage) > maxSpecialistTaskResponseBytes || strings.IndexByte(*task.LastAgentMessage, 0) >= 0 {
					return codexResponseCandidate{}, true, errors.New("specialist Codex completion has no bounded final response")
				}
				completedAt, err := validateCodexCompletionTime(event.Timestamp, task.CompletedAt, matchedAt)
				if err != nil {
					return codexResponseCandidate{}, true, err
				}
				candidate = codexResponseCandidate{turnID: matchedTurn, response: *task.LastAgentMessage, completedAt: completedAt}
			}
		case "response_item":
			var message codexMessageEvent
			if err := json.Unmarshal(event.Payload, &message); err != nil || message.Type != "message" || message.Role != "user" || len(message.Content) != 1 || message.Content[0].Type != "input_text" || message.Content[0].Text != exactMessage {
				continue
			}
			if !safeIdentityPattern.MatchString(message.Metadata.TurnID) || !startedTurns[message.Metadata.TurnID] {
				return codexResponseCandidate{}, true, errors.New("specialist dispatch is not bound to a started Codex turn")
			}
			if matchedTurn != "" {
				return codexResponseCandidate{}, true, errors.New("specialist Codex turn contains duplicate exact dispatches")
			}
			matchedTurn = message.Metadata.TurnID
			matchedAt, err = time.Parse(time.RFC3339Nano, event.Timestamp)
			if err != nil {
				return codexResponseCandidate{}, true, errors.New("specialist dispatch timestamp is invalid")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return codexResponseCandidate{}, matchedTurn != "", fmt.Errorf("scan specialist Codex journal: %w", err)
	}
	if sessionMetaCount != 1 || sessionCWD != roleWorkDir {
		if matchedTurn != "" {
			return codexResponseCandidate{}, true, errors.New("specialist Codex turn is not bound to the selected role work directory")
		}
		return codexResponseCandidate{}, false, nil
	}
	if matchedTurn == "" || candidate.response == "" {
		return codexResponseCandidate{}, matchedTurn != "", ErrSpecialistTaskResponsePending
	}
	return candidate, true, nil
}

func validateCodexCompletionTime(timestamp string, completed json.Number, matchedAt time.Time) (time.Time, error) {
	value, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return time.Time{}, errors.New("specialist completion timestamp is invalid")
	}
	seconds, err := strconv.ParseInt(string(completed), 10, 64)
	if err != nil || value.Unix() != seconds || value.Before(matchedAt) {
		return time.Time{}, errors.New("specialist completion timestamps are inconsistent")
	}
	return value.UTC(), nil
}

func readStableSpecialistCodexRollout(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxSpecialistCodexRolloutBytes {
		return nil, errors.New("specialist Codex journal is not a bounded regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("specialist Codex journal is writable by another principal")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSpecialistCodexRolloutBytes+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(path)
	if readErr != nil || statErr != nil || closeErr != nil || lstatErr != nil {
		return nil, ErrSpecialistTaskResponsePending
	}
	if int64(len(data)) > maxSpecialistCodexRolloutBytes || !os.SameFile(info, opened) || !os.SameFile(info, after) || after.Mode()&os.ModeSymlink != 0 || opened.Size() != info.Size() || after.Size() != info.Size() {
		return nil, ErrSpecialistTaskResponsePending
	}
	return data, nil
}
