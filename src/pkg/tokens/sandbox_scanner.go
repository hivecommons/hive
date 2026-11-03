package tokens

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// BackendSandbox names sessions reconstructed from sandbox run records
// (crux/agent sandbox-runs/runs.jsonl). A sandboxed kick runs in a podman
// container or a Kubernetes Job whose CLI home never reaches the pod, so the
// usage it reported in its transcript is the only record the hive has.
const BackendSandbox = "sandbox"

// sandboxSessionIDPrefix keeps these session ids distinct from every scanner
// that keys on a file name.
const sandboxSessionIDPrefix = "sandbox-"

// sandboxRunRecord is the subset of a crux/agent SandboxRunHistoryEntry this
// scanner reads. It is declared here rather than imported so pkg/tokens stays
// independent of the runtime module; the JSON keys are the contract.
type sandboxRunRecord struct {
	Agent   string `json:"agent"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
	Run     struct {
		StartedAt time.Time `json:"startedAt"`
		EndedAt   time.Time `json:"endedAt"`
		Usage     struct {
			Model       string `json:"model"`
			Input       int64  `json:"input"`
			Output      int64  `json:"output"`
			CacheRead   int64  `json:"cacheRead"`
			CacheCreate int64  `json:"cacheCreate"`
		} `json:"usage"`
	} `json:"run"`
}

// ScanSandboxRuns reads a sandbox run history (one JSON record per line) and
// returns one session per run that reported token usage. Runs whose
// transcript carried no usage contribute nothing: the collector must not
// invent tokens, and a zero-token session would be dropped downstream anyway.
// A missing file is not an error — the hive simply has no sandboxed agents.
func ScanSandboxRuns(path string) (*AggregateSummary, error) {
	agg := &AggregateSummary{
		ByAgent:       make(map[string]int64),
		ByModel:       make(map[string]int64),
		ByAgentDetail: make(map[string]*AgentModelBucket),
		ByModelDetail: make(map[string]*AgentModelBucket),
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return agg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening sandbox run history: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	const maxLine = 1 << 20
	scanner.Buffer(make([]byte, 0, maxLine), maxLine)
	var sessions []SessionSummary
	for scanner.Scan() {
		var rec sandboxRunRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil || rec.Agent == "" {
			continue
		}
		if sess, ok := sandboxRunSession(rec); ok {
			sessions = append(sessions, sess)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading sandbox run history: %w", err)
	}
	MergeAggregates(agg, &AggregateSummary{Sessions: sessions})
	return agg, nil
}

func sandboxRunSession(rec sandboxRunRecord) (SessionSummary, bool) {
	u := rec.Run.Usage
	total := u.Input + u.Output + u.CacheRead + u.CacheCreate
	if total <= 0 || rec.Run.StartedAt.IsZero() {
		return SessionSummary{}, false
	}
	model := u.Model
	if model == "" {
		model = rec.Model
	}
	if model == "" {
		model = "unknown"
	}
	started := rec.Run.StartedAt.UnixMilli()
	ended := rec.Run.EndedAt.UnixMilli()
	if ended < started {
		ended = started
	}
	// One kick is one user prompt and one assistant turn.
	const messagesPerRun = 2
	return SessionSummary{
		SessionID:    sandboxSessionIDPrefix + rec.Agent + "-" + strconv.FormatInt(started, 10),
		Agent:        rec.Agent,
		Model:        model,
		InputTokens:  u.Input,
		OutputTokens: u.Output,
		CacheRead:    u.CacheRead,
		CacheCreate:  u.CacheCreate,
		TotalTokens:  total,
		Messages:     messagesPerRun,
		FirstActive:  started,
		LastActive:   ended,
		Backend:      BackendSandbox,
		Usage: []UsageEvent{{
			TimestampMs: ended,
			Model:       model,
			Coalesced:   1,
			Input:       u.Input,
			Output:      u.Output,
			CacheRead:   u.CacheRead,
			CacheCreate: u.CacheCreate,
		}},
	}, true
}
