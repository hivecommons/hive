package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/config"
	ghpkg "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/knowledge"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
	"github.com/kubestellar/hive/v2/pkg/tokens"
)

type Dependencies struct {
	Config            *config.Config
	AgentMgr          *agent.Manager
	Governor          *governor.Governor
	GHClient          *ghpkg.Client
	GHAppAuth         *ghpkg.AppAuth
	Tokens            *tokens.Collector
	Knowledge         *knowledge.KnowledgeAPI
	Inception         *knowledge.InceptionEngine
	Nous              *NousState
	Scheduler         *scheduler.Scheduler
	MetricsCollector  *MetricsCollector
	BeadSynthesizer   *knowledge.BeadSynthesizer
	BeadStores        map[string]*beads.Store
	ConfigCoordinator *ConfigCoordinator
	Logger            *slog.Logger
	Ctx               context.Context
	RefreshFunc       func()
	PersistFunc       func()
	SkipReloadFunc    func()
	ReInitFunc        func()
	SetUserClient     func(token string)
	EnumerateFunc     func()
	AdvisoryResetFunc func(newPrimaryRepo string)
	ReinitGitHubFunc  func(appID, installationID int64, keyFile string) error
}

type NousState struct {
	Mode         string                   `json:"mode"`
	Scope        string                   `json:"scope"`
	Phase        string                   `json:"phase"`
	Status       map[string]interface{}   `json:"status"`
	Ledger       []map[string]interface{} `json:"ledger"`
	Principles   []NousPrinciple          `json:"principles"`
	Config       map[string]interface{}   `json:"config"`
	GatePending  map[string]interface{}   `json:"gate_pending,omitempty"`
	GateResponse map[string]interface{}   `json:"gate_response,omitempty"`
	SnapshotDir  string                   `json:"-"`
	Mu           sync.Mutex               `json:"-"`
}

type NousPrinciple struct {
	ID         string  `json:"id"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`
}

const NousBaselineTarget = 672

type NousSnapshot struct {
	Timestamp     string            `json:"timestamp"`
	Mode          string            `json:"mode"`
	QueueIssues   int               `json:"queue_issues"`
	QueuePRs      int               `json:"queue_prs"`
	QueueHold     int               `json:"queue_hold"`
	SLAViolations int               `json:"sla_violations"`
	AgentsKicked  []string          `json:"agents_kicked,omitempty"`
	AgentStates   map[string]string `json:"agent_states,omitempty"`
	TotalTokens   int64             `json:"total_tokens"`
}

func (ns *NousState) RecordSnapshot(govState governor.State, actionable *ghpkg.ActionableResult, agentsKicked []string, agentStatuses map[string]*agent.AgentProcess, tokenSummary *tokens.AggregateSummary) error {
	if ns.SnapshotDir == "" {
		return nil
	}

	snap := NousSnapshot{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Mode:         string(govState.Mode),
		QueueIssues:  govState.QueueIssues,
		QueuePRs:     govState.QueuePRs,
		QueueHold:    govState.QueueHold,
		AgentsKicked: agentsKicked,
		AgentStates:  make(map[string]string),
	}
	if actionable != nil {
		snap.SLAViolations = actionable.Issues.SLAViolations
	}
	for name, proc := range agentStatuses {
		snap.AgentStates[name] = string(proc.State)
	}
	if tokenSummary != nil {
		snap.TotalTokens = tokenSummary.TotalTokens
	}

	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}

	filename := fmt.Sprintf("%s/%d.json", ns.SnapshotDir, time.Now().UnixMilli())
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}

	ns.refreshStatus()
	return nil
}

func (ns *NousState) refreshStatus() {
	ns.Mu.Lock()
	defer ns.Mu.Unlock()

	count := 0
	if entries, err := os.ReadDir(ns.SnapshotDir); err == nil {
		count = len(entries)
	}

	if count > 0 && ns.Phase == "collecting" {
		ns.Phase = "observing"
	}

	ns.Status["snapshots"] = count
	ns.Status["snapshotCount"] = count
	ns.Status["baseline_pct"] = float64(count) * 100 / NousBaselineTarget
	ns.Status["baseline_target"] = NousBaselineTarget
	ns.Status["snapshotTarget"] = NousBaselineTarget
	ns.Status["phase"] = ns.Phase
	ns.Status["principleCount"] = len(ns.Principles)
}
