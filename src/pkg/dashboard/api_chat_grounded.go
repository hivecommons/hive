package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Grounded chat answers. A question the deterministic handlers cannot answer
// (a slash command with arguments, or free text) is answered by one
// read-only model turn over a compact snapshot of the hive state the
// dashboard already holds in memory (s.status): agents, open PRs, actionable
// issues, items parked for a decision, governor, alerts, version and the
// last upgrade. The model is told to answer from the snapshot only, to say
// "I don't have that data" rather than guess, and to cite the items it used.
//
// Hard rules: the path is read-only and never triggers an action; the
// snapshot is scoped to the command's dataset and capped in size; the turn
// has a timeout and an in-flight cap; failure falls back to the command's
// usage (never the bare dump); every grounded answer writes one audit line.
// Gated by chat.grounded_answers.enabled (default on).

const (
	// chatGroundedProfile names the turn in runner errors and the audit log.
	chatGroundedProfile = "hive-chat-grounded"
	// chatGroundedTimeout bounds one grounding turn end to end.
	chatGroundedTimeout = 25 * time.Second
	// chatGroundedMaxInflight caps concurrent grounding turns per dashboard so
	// a burst of chat questions cannot crowd out agent inference.
	chatGroundedMaxInflight = 2
	// chatGroundedMaxItems caps each list (agents, PRs, issues, decisions) in
	// the snapshot; chatGroundedMaxSnapshotBytes caps the serialised whole.
	chatGroundedMaxItems         = 40
	chatGroundedMaxSnapshotBytes = 24 * 1024
	// chatGroundedMaxQuestion bounds the question echoed into the prompt and
	// the audit line.
	chatGroundedMaxQuestion = 500
	// chatGroundedMaxAnswer bounds what is returned to the browser.
	chatGroundedMaxAnswer = 6000
	// chatGroundedTextCap truncates free-text fields (doing, titles, reasons).
	chatGroundedTextCap = 160
	// chatGroundedAuditAction is the audit log action for one grounded answer.
	chatGroundedAuditAction = "chat_grounded_answer"
)

var (
	errChatGroundingDisabled    = errors.New("chat.grounded_answers.enabled is off")
	errChatGroundingUnavailable = errors.New("no inference backend is wired for chat")
	errChatGroundingBusy        = errors.New("too many chat questions in flight")
	errChatGroundingNoStatus    = errors.New("hive status is still initializing")
)

// chatGroundedTurnRunner is the inference seam: the hive's configured
// reviewer-endpoint turn runner, or a fake in tests.
type chatGroundedTurnRunner = delegatedecision.TurnRunner

// chatSnapshot is the structured hive state the grounding turn sees. Every
// field is derived from the in-memory status payload; nothing is fetched.
type chatSnapshot struct {
	GeneratedAt string                 `json:"generated_at"`
	Scope       string                 `json:"scope"`
	HiveID      string                 `json:"hive_id,omitempty"`
	Version     *chatSnapshotVersion   `json:"version,omitempty"`
	Governor    *chatSnapshotGovernor  `json:"governor,omitempty"`
	Totals      *chatSnapshotTotals    `json:"totals,omitempty"`
	Agents      []chatSnapshotAgent    `json:"agents,omitempty"`
	PRs         []chatSnapshotPR       `json:"open_prs,omitempty"`
	Issues      []chatSnapshotIssue    `json:"actionable_issues,omitempty"`
	Decisions   []chatSnapshotDecision `json:"awaiting_decision,omitempty"`
	Alerts      []SystemAlert          `json:"alerts,omitempty"`
	Truncated   []string               `json:"truncated,omitempty"`
}

type chatSnapshotVersion struct {
	Release     string `json:"release,omitempty"`
	Commit      string `json:"commit,omitempty"`
	Channel     string `json:"channel,omitempty"`
	LastUpgrade string `json:"last_upgrade_completed_at,omitempty"`
	UpgradeTo   string `json:"last_upgrade_target,omitempty"`
}

type chatSnapshotGovernor struct {
	Active     bool     `json:"active"`
	Mode       string   `json:"mode,omitempty"`
	NextKickIn string   `json:"next_kick_in,omitempty"`
	NextAgents []string `json:"next_agents,omitempty"`
}

type chatSnapshotTotals struct {
	Agents           int `json:"agents"`
	AgentsPaused     int `json:"agents_paused"`
	OpenIssues       int `json:"open_issues"`
	OpenPRs          int `json:"open_prs"`
	ActionableIssues int `json:"actionable_issues"`
	ActionablePRs    int `json:"actionable_prs"`
	AwaitingDecision int `json:"awaiting_decision"`
	WorkerBeads      int `json:"worker_beads"`
	SupervisorBeads  int `json:"supervisor_beads"`
}

type chatSnapshotAgent struct {
	Name           string `json:"name"`
	Role           string `json:"role,omitempty"`
	State          string `json:"state"`
	Busy           string `json:"busy,omitempty"`
	Paused         bool   `json:"paused,omitempty"`
	PausedReason   string `json:"paused_reason,omitempty"`
	BlockedReason  string `json:"blocked_reason,omitempty"`
	AwaitingCI     bool   `json:"awaiting_ci,omitempty"`
	NeedsLogin     bool   `json:"needs_login,omitempty"`
	Doing          string `json:"doing,omitempty"`
	LastKickAt     string `json:"last_kick_at,omitempty"`
	KickOutcome    string `json:"last_kick_outcome,omitempty"`
	NextKickIn     string `json:"next_kick_in,omitempty"`
	LastCompleted  string `json:"last_completed,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	InferenceError string `json:"inference_error,omitempty"`
	Restarts       int    `json:"restarts,omitempty"`
	Backend        string `json:"backend,omitempty"`
	Model          string `json:"model,omitempty"`
}

type chatSnapshotPR struct {
	Ref          string   `json:"ref"`
	Title        string   `json:"title"`
	Author       string   `json:"author,omitempty"`
	HiveAgent    string   `json:"hive_agent,omitempty"`
	Draft        bool     `json:"draft,omitempty"`
	Stage        []string `json:"stage_labels,omitempty"`
	Labels       []string `json:"other_labels,omitempty"`
	ReviewState  string   `json:"review_state,omitempty"`
	Reviewers    []string `json:"requested_reviewers,omitempty"`
	CI           string   `json:"ci,omitempty"`
	Failing      []string `json:"failing_checks,omitempty"`
	Mergeable    string   `json:"mergeable,omitempty"`
	MergeVerdict string   `json:"merge_verdict,omitempty"`
	Age          string   `json:"age,omitempty"`
	Updated      string   `json:"updated,omitempty"`
}

type chatSnapshotIssue struct {
	Ref       string   `json:"ref"`
	Title     string   `json:"title"`
	Priority  string   `json:"priority,omitempty"`
	Stage     []string `json:"stage_labels,omitempty"`
	Labels    []string `json:"other_labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
	Age       string   `json:"age,omitempty"`
}

type chatSnapshotDecision struct {
	Ref   string `json:"ref"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Stage string `json:"stage"`
	Age   string `json:"age,omitempty"`
}

// chatGroundedEnabled reads chat.grounded_answers.enabled (default on).
func (s *Server) chatGroundedEnabled() bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return true
	}
	return s.deps.Config.Chat.GroundedAnswersEnabled()
}

// chatGroundedRunner is the wired turn runner, nil when none.
func (s *Server) chatGroundedRunner() chatGroundedTurnRunner {
	if s == nil || s.deps == nil || s.deps.ChatGrounder == nil {
		return nil
	}
	return s.deps.ChatGrounder
}

// chatGroundedInflight is the per-process in-flight cap. Lazily created so
// a bare &Server{} in tests still works.
func (s *Server) chatGroundedSlots() chan struct{} {
	s.chatGroundedOnce.Do(func() { s.chatGroundedInflight = make(chan struct{}, chatGroundedMaxInflight) })
	return s.chatGroundedInflight
}

// chatGroundedAnswer runs the grounding turn for route. It returns the
// answer, the item refs the snapshot carried, and an error naming why the
// turn did not run or failed. It never mutates state.
func (s *Server) chatGroundedAnswer(ctx context.Context, route chatRoute) (string, int, error) {
	if !s.chatGroundedEnabled() {
		return "", 0, errChatGroundingDisabled
	}
	runner := s.chatGroundedRunner()
	if runner == nil {
		return "", 0, errChatGroundingUnavailable
	}
	status := s.chatStatusSnapshot()
	if status == nil {
		return "", 0, errChatGroundingNoStatus
	}
	slots := s.chatGroundedSlots()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return "", 0, errChatGroundingBusy
	}

	snap := chatBuildSnapshot(status, route.Scope, time.Now(), s.chatVersionFacts())
	raw, items := chatEncodeSnapshot(snap, chatGroundedMaxSnapshotBytes)

	turnCtx, cancel := context.WithTimeout(ctx, chatGroundedTimeout)
	defer cancel()
	question := chatTruncate(route.Question, chatGroundedMaxQuestion)
	reply, err := runner.RunDeciderTurn(turnCtx, chatGroundedProfile, chatGroundedSystemPrompt(), chatGroundedUserPrompt(route.Scope, question, raw))
	if err != nil {
		return "", items, err
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return "", items, errors.New("empty reply")
	}
	return chatTruncate(reply, chatGroundedMaxAnswer), items, nil
}

// chatGroundedSystemPrompt is the grounding contract.
func chatGroundedSystemPrompt() string {
	return strings.TrimSpace(`
You are Hive Chat, a read-only assistant for one hive (a fleet of coding agents working GitHub repositories).
You are given a JSON snapshot of the hive's current state and one question from the hive's owner.

Rules:
- Answer ONLY from the snapshot. Do not use outside knowledge about agents, repositories or versions.
- If the snapshot does not contain what the question needs, reply exactly "I don't have that data" followed by one sentence naming what is missing. Never guess, never invent items, numbers or times.
- Filter and reason over the snapshot to answer the actual question (for example "stuck" means paused, blocked, needs_login, awaiting_ci, or a last_kick_outcome that is not success; "waiting on review" means review_state is empty or pending and the PR is not a draft).
- Be concise: a short direct answer, then a compact list only when the question asks for items. Never dump the whole snapshot.
- End with one line "Sources:" listing the agent names and item refs (owner/repo#N) you used.
- You cannot take actions. If asked to do something (kick, pause, merge, close, label), say that chat is read-only and name the dashboard action instead.
`)
}

// chatGroundedUserPrompt is the question with its snapshot.
func chatGroundedUserPrompt(scope, question, snapshot string) string {
	if scope == "" {
		scope = "all"
	}
	return fmt.Sprintf("Scope: %s\nQuestion: %s\n\nHive state snapshot (JSON):\n%s", scope, question, snapshot)
}

// chatVersionFacts collects the version / upgrade facts the version endpoint
// serves, without the network-backed latest-release lookup.
func (s *Server) chatVersionFacts() *chatSnapshotVersion {
	v := &chatSnapshotVersion{}
	if versionShort != "" && versionShort != "unknown" {
		v.Commit = versionShort
	}
	if release, _ := fleetReportBuildInfo(); release != "" && release != "unknown" {
		v.Release = release
	}
	v.Channel = versionChannel
	if versionImageSource != nil {
		if ref := versionImageSource(); ref != "" {
			v.Channel = spoke.ImageReleaseChannel(ref)
		}
	}
	if rec := readUpgradeOutcome(); rec != nil {
		if !rec.CompletedAt.IsZero() {
			v.LastUpgrade = rec.CompletedAt.UTC().Format(time.RFC3339)
		}
		v.UpgradeTo = chatShortSHA(rec.TargetSHA)
	}
	return v
}

// chatBuildSnapshot derives the scoped snapshot from the status payload.
// Scope "" includes everything; a command scope keeps the shared header
// (totals, governor, version, alerts) and that command's dataset.
func chatBuildSnapshot(status *StatusPayload, scope string, now time.Time, version *chatSnapshotVersion) chatSnapshot {
	snap := chatSnapshot{GeneratedAt: now.UTC().Format(time.RFC3339), Scope: scope, HiveID: status.HiveID, Version: version}
	if snap.Scope == "" {
		snap.Scope = "all"
	}
	snap.Governor = &chatSnapshotGovernor{
		Active:     status.Governor.Active,
		Mode:       status.Governor.Mode,
		NextKickIn: status.Governor.NextKickIn,
		NextAgents: status.Governor.NextAgents,
	}
	snap.Alerts = status.SystemAlerts

	totals := &chatSnapshotTotals{
		Agents:           len(status.Agents),
		OpenIssues:       status.OpenItems.Issues,
		OpenPRs:          status.OpenItems.PRs,
		ActionableIssues: status.ActionableNow.Issues,
		ActionablePRs:    status.ActionableNow.PRs,
		WorkerBeads:      status.Beads.Workers,
		SupervisorBeads:  status.Beads.Supervisor,
	}
	for _, a := range status.Agents {
		if a.Paused {
			totals.AgentsPaused++
		}
	}

	wantAgents := scope == "" || scope == "agents" || scope == "governor" || scope == "status"
	wantPRs := scope == "" || scope == "prs" || scope == "status"
	wantIssues := scope == "" || scope == "beads" || scope == "status"

	if wantAgents {
		agents := append([]FrontendAgent(nil), status.Agents...)
		sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
		for _, a := range agents {
			snap.Agents = append(snap.Agents, chatSnapshotAgentFrom(a))
		}
	}

	for _, repo := range status.Repos {
		for _, entry := range repo.OpenPrs {
			pr, verdict, ok := chatPRFromEntry(entry)
			if !ok {
				continue
			}
			ref := chatRef(pr.Repo, repo, pr.Number)
			if stage := chatStageLabels(pr.Labels); chatHasDecisionStage(stage) {
				snap.Decisions = append(snap.Decisions, chatSnapshotDecision{Ref: ref, Kind: "pr", Title: chatTruncate(pr.Title, chatGroundedTextCap), Stage: strings.Join(stage, ","), Age: chatAge(now, pr.CreatedAt)})
			}
			if wantPRs {
				snap.PRs = append(snap.PRs, chatSnapshotPRFrom(ref, pr, verdict, now))
			}
		}
		for _, entry := range repo.ActionableIssues {
			is, ok := chatIssueFromEntry(entry)
			if !ok {
				continue
			}
			ref := chatRef(is.Repo, repo, is.Number)
			stage := chatStageLabels(is.Labels)
			if chatHasDecisionStage(stage) {
				snap.Decisions = append(snap.Decisions, chatSnapshotDecision{Ref: ref, Kind: "issue", Title: chatTruncate(is.Title, chatGroundedTextCap), Stage: strings.Join(stage, ","), Age: chatAge(now, is.CreatedAt)})
			}
			if wantIssues {
				snap.Issues = append(snap.Issues, chatSnapshotIssue{
					Ref: ref, Title: chatTruncate(is.Title, chatGroundedTextCap), Priority: is.Priority,
					Stage: stage, Labels: chatOtherLabels(is.Labels), Assignees: is.Assignees, Age: chatAge(now, is.CreatedAt),
				})
			}
		}
	}
	totals.AwaitingDecision = len(snap.Decisions)
	snap.Totals = totals

	snap.Agents, snap.Truncated = chatCapList(snap.Agents, "agents", snap.Truncated)
	snap.PRs, snap.Truncated = chatCapList(snap.PRs, "open_prs", snap.Truncated)
	snap.Issues, snap.Truncated = chatCapList(snap.Issues, "actionable_issues", snap.Truncated)
	snap.Decisions, snap.Truncated = chatCapList(snap.Decisions, "awaiting_decision", snap.Truncated)
	return snap
}

func chatSnapshotAgentFrom(a FrontendAgent) chatSnapshotAgent {
	state := strings.TrimSpace(a.State)
	if state == "" {
		state = "unknown"
	}
	if a.Starting {
		state = "starting"
	}
	out := chatSnapshotAgent{
		Name: a.Name, Role: a.Role, State: state, Busy: a.Busy, Paused: a.Paused,
		PausedReason: chatTruncate(a.PausedReason, chatGroundedTextCap), AwaitingCI: a.AwaitingCI, NeedsLogin: a.NeedsLogin,
		Doing:      chatTruncate(firstNonEmpty(a.Doing, a.StructuredStatus, a.LiveSummary), chatGroundedTextCap),
		LastKickAt: a.LastKickAt, NextKickIn: a.NextKickIn, LastError: chatTruncate(a.LastError, chatGroundedTextCap),
		InferenceError: chatTruncate(a.InferenceLastError, chatGroundedTextCap), Restarts: a.Restarts,
		Backend: a.CLI, Model: a.Model,
	}
	if a.KickOutcome != "" {
		out.KickOutcome = a.KickOutcome
		if a.KickOutcomeReason != "" {
			out.KickOutcome += ": " + chatTruncate(a.KickOutcomeReason, chatGroundedTextCap)
		}
	}
	if a.Condition != "" && a.Condition != "ok" && a.Condition != "healthy" {
		out.BlockedReason = a.Condition
		if a.ConditionMessage != "" {
			out.BlockedReason += ": " + chatTruncate(a.ConditionMessage, chatGroundedTextCap)
		}
	}
	if a.LastAction != nil && a.LastAction.Action != "" {
		out.LastCompleted = fmt.Sprintf("%s %s#%d at %s", a.LastAction.Action, a.LastAction.Repo, a.LastAction.Number, a.LastAction.At)
	}
	return out
}

func chatSnapshotPRFrom(ref string, pr github.PullRequest, verdict *github.MergeVerdict, now time.Time) chatSnapshotPR {
	out := chatSnapshotPR{
		Ref: ref, Title: chatTruncate(pr.Title, chatGroundedTextCap), Author: pr.Author, HiveAgent: pr.HiveAgent, Draft: pr.Draft,
		Stage: chatStageLabels(pr.Labels), Labels: chatOtherLabels(pr.Labels),
		ReviewState: pr.ReviewState, Reviewers: pr.RequestedReviewers,
		CI: pr.CIStatus, Failing: pr.FailingChecks, Mergeable: pr.MergeableState,
		Age: chatAge(now, pr.CreatedAt), Updated: chatAge(now, pr.UpdatedAt),
	}
	if pr.CIChecksRunning && out.CI == "" {
		out.CI = "running"
	}
	if verdict != nil {
		out.MergeVerdict = string(verdict.State)
		if verdict.Reason != "" {
			out.MergeVerdict += ": " + chatTruncate(verdict.Reason, chatGroundedTextCap)
		}
	}
	return out
}

// chatPRFromEntry accepts the typed FrontendPR / github.PullRequest the
// status builder stores, or a generic map (tests, older snapshots).
func chatPRFromEntry(entry any) (github.PullRequest, *github.MergeVerdict, bool) {
	switch v := entry.(type) {
	case FrontendPR:
		return v.PullRequest, v.MergeVerdict, true
	case *FrontendPR:
		if v == nil {
			return github.PullRequest{}, nil, false
		}
		return v.PullRequest, v.MergeVerdict, true
	case github.PullRequest:
		return v, nil, true
	case *github.PullRequest:
		if v == nil {
			return github.PullRequest{}, nil, false
		}
		return *v, nil, true
	}
	var fp FrontendPR
	if !chatRoundTrip(entry, &fp) || fp.Number == 0 {
		return github.PullRequest{}, nil, false
	}
	return fp.PullRequest, fp.MergeVerdict, true
}

func chatIssueFromEntry(entry any) (github.Issue, bool) {
	switch v := entry.(type) {
	case github.Issue:
		return v, true
	case *github.Issue:
		if v == nil {
			return github.Issue{}, false
		}
		return *v, true
	}
	var is github.Issue
	if !chatRoundTrip(entry, &is) || is.Number == 0 {
		return github.Issue{}, false
	}
	return is, true
}

func chatRoundTrip(in any, out any) bool {
	raw, err := json.Marshal(in)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

func chatRef(itemRepo string, repo FrontendRepo, number int) string {
	name := strings.TrimSpace(itemRepo)
	if name == "" {
		name = firstNonEmpty(repo.Full, repo.Name)
	}
	return fmt.Sprintf("%s#%d", name, number)
}

func chatStageLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if strings.HasPrefix(l, "stage/") {
			out = append(out, l)
		}
	}
	return out
}

func chatOtherLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if !strings.HasPrefix(l, "stage/") {
			out = append(out, l)
		}
	}
	return out
}

func chatHasDecisionStage(stage []string) bool {
	for _, l := range stage {
		if l == config.DefaultNeedsDecisionLabel || l == config.PRNeedsDecisionStageLabel {
			return true
		}
	}
	return false
}

func chatAge(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func chatShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func chatTruncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func chatCapList[T any](list []T, name string, truncated []string) ([]T, []string) {
	if len(list) <= chatGroundedMaxItems {
		return list, truncated
	}
	return list[:chatGroundedMaxItems], append(truncated, fmt.Sprintf("%s: showing %d of %d", name, chatGroundedMaxItems, len(list)))
}

// chatEncodeSnapshot serialises snap, halving its lists until the JSON fits
// maxBytes. Returns the JSON and the number of listed items it carries.
func chatEncodeSnapshot(snap chatSnapshot, maxBytes int) (string, int) {
	for {
		raw, err := json.Marshal(snap)
		items := len(snap.Agents) + len(snap.PRs) + len(snap.Issues) + len(snap.Decisions)
		if err != nil {
			return "{}", 0
		}
		if len(raw) <= maxBytes || items == 0 {
			return string(raw), items
		}
		halve := func(n int) int { return n / 2 }
		if n := halve(len(snap.PRs)); n < len(snap.PRs) {
			snap.Truncated = append(snap.Truncated, fmt.Sprintf("open_prs: cut to %d for size", n))
			snap.PRs = snap.PRs[:n]
		}
		if n := halve(len(snap.Issues)); n < len(snap.Issues) {
			snap.Truncated = append(snap.Truncated, fmt.Sprintf("actionable_issues: cut to %d for size", n))
			snap.Issues = snap.Issues[:n]
		}
		if n := halve(len(snap.Agents)); n < len(snap.Agents) && len(snap.PRs)+len(snap.Issues) == 0 {
			snap.Truncated = append(snap.Truncated, fmt.Sprintf("agents: cut to %d for size", n))
			snap.Agents = snap.Agents[:n]
		}
		if n := halve(len(snap.Decisions)); n < len(snap.Decisions) && len(snap.PRs)+len(snap.Issues) == 0 {
			snap.Decisions = snap.Decisions[:n]
		}
	}
}

// chatAuditGrounded writes the one audit line per grounded attempt.
func (s *Server) chatAuditGrounded(r *http.Request, route chatRoute, items int, err error) {
	if s == nil || s.audit == nil {
		return
	}
	outcome := "answered"
	if err != nil {
		outcome = "fallback: " + err.Error()
	}
	scope := route.Scope
	if scope == "" {
		scope = "all"
	}
	s.auditFromRequest(r, chatGroundedAuditAction, auditDetail(
		"scope", scope,
		"question", chatTruncate(route.Question, chatGroundedMaxQuestion),
		"snapshot_items", fmt.Sprint(items),
		"outcome", outcome,
	), "")
}
