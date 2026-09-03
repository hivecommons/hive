package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/agentparse"
	"github.com/kubestellar/hive/pkg/beads"
	"github.com/kubestellar/hive/pkg/governor"
	"github.com/kubestellar/hive/pkg/knowledge"
)

const (
	inceptionBeadRefPrefix      = "inception/"
	minQuestionsForAdvance      = 5
	minFactsForAdvance          = 3
	targetFactCount             = 8
	factEnrichmentGracePeriod   = 15 * time.Second
	autoFactFallbackTimeout     = 60 * time.Second // minimum time before fact fallback can fire
	autoQuestionFallbackTimeout = 60 * time.Second // minimum time before fallback can fire
	agentIdleThreshold          = 30 * time.Second // agent must be idle this long before fallback
)

var (
	inceptionWatchIntervalS    = 5 * time.Second
	inceptionFastPollInterval  = 2 * time.Second
	plukSubscriberPollInterval = 500 * time.Millisecond
	plukSubscriberSeekEnd      = true
	plukLogFile                = func() string { return fmt.Sprintf("%s/hive-brainstorm.jsonl", plukLogDir) }
)

type inceptionAgentManager interface {
	GetBufferOutput(name string, lines int) ([]string, error)
	SendKick(name string, message string) error
	RestartWithBootstrap(ctx context.Context, name, prompt string) error
}

// InceptionWatcher polls brainstorm beads and bridges them into the inception
// state machine. It detects question beads (capture → clarify) and fact beads
// (structure → scaffold), advancing phases automatically.
type InceptionWatcher struct {
	beadStore *beads.Store
	inception *knowledge.InceptionEngine
	scheduler SchedulerControl
	agentMgr  inceptionAgentManager
	governor  *governor.Governor
	logger    *slog.Logger

	lastQuestionCount int
	lastFactCount     int
	lastSlug          string
	lastKickRetry     time.Time
	kickRetryCount    int
	rateLimitedUntil  time.Time
	ctx               context.Context
	factGraceStart    time.Time
	captureSeenAt     time.Time // when watcher first saw this inception in capture phase
	structureSeenAt   time.Time // when watcher first saw this inception in structure phase

	pollMu              sync.Mutex
	plukMu              sync.Mutex
	plukFactLines       []string
	plukIdleInStructure bool
	plukLastActivity    time.Time // last time pluk saw agent output
	plukEventCount      int       // total pluk events processed
	plukQuestions       []knowledge.Question
	plukBdCreateLines   []string
}

// NewInceptionWatcher creates a watcher that polls the brainstorm bead store.
func NewInceptionWatcher(
	beadStore *beads.Store,
	inception *knowledge.InceptionEngine,
	sched SchedulerControl,
	agentMgr *agent.Manager,
	gov *governor.Governor,
	logger *slog.Logger,
) *InceptionWatcher {
	w := &InceptionWatcher{
		beadStore: beadStore,
		inception: inception,
		scheduler: sched,
		governor:  gov,
		logger:    logger,
	}
	if agentMgr != nil {
		w.agentMgr = agentMgr
	}
	return w
}

// Run starts the polling loop and event subscriber. Blocks until ctx is cancelled.
func (w *InceptionWatcher) Run(ctx context.Context) {
	w.ctx = ctx
	w.logger.Info("inception watcher started")
	watchInterval := inceptionWatchIntervalS
	fastPollInterval := inceptionFastPollInterval
	plukLog := plukLogFile()
	plukPollInterval := plukSubscriberPollInterval
	plukSeekEnd := plukSubscriberSeekEnd
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	// Start pluk event subscriber for brainstorm session
	go w.runPlukSubscriberWithSettings(ctx, plukLog, watchInterval, plukPollInterval, plukSeekEnd)

	lastInterval := watchInterval

	for {
		// Adaptive polling: 2s during capture/structure, 5s otherwise
		state := w.inception.GetState()
		var targetInterval time.Duration
		if state != nil && (state.Phase == knowledge.PhaseCapture || state.Phase == knowledge.PhaseStructure) {
			targetInterval = fastPollInterval
		} else {
			targetInterval = watchInterval
		}
		if targetInterval != lastInterval {
			ticker.Reset(targetInterval)
			lastInterval = targetInterval
		}

		select {
		case <-ctx.Done():
			w.logger.Info("inception watcher stopped")
			return
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

func (w *InceptionWatcher) poll(ctx context.Context) {
	w.pollMu.Lock()
	defer w.pollMu.Unlock()

	if w.inception == nil || w.beadStore == nil {
		return
	}

	if err := w.beadStore.Reload(); err != nil {
		w.logger.Warn("inception watcher: bead store reload failed", "error", err)
	}

	state := w.inception.GetState()
	if state == nil {
		w.lastQuestionCount = 0
		w.lastFactCount = 0
		w.lastSlug = ""
		w.factGraceStart = time.Time{}
		return
	}

	if state.IdeaSlug != w.lastSlug {
		w.lastQuestionCount = 0
		w.lastFactCount = 0
		w.kickRetryCount = 0
		w.lastKickRetry = time.Time{}
		w.rateLimitedUntil = time.Time{}
		w.factGraceStart = time.Time{}
		w.captureSeenAt = time.Now()
		w.structureSeenAt = time.Time{}
		w.plukMu.Lock()
		w.plukFactLines = nil
		w.plukQuestions = nil
		w.plukBdCreateLines = nil
		w.plukIdleInStructure = false
		w.plukLastActivity = time.Time{}
		w.plukMu.Unlock()
		if w.lastSlug != "" {
			w.reapOldInceptionBeads(state.StartedAt)
		}
		w.lastSlug = state.IdeaSlug
	}

	inceptionBeads := w.findInceptionBeads()

	switch state.Phase {
	case knowledge.PhaseCapture:
		if len(inceptionBeads) > 0 {
			w.checkForQuestions(inceptionBeads)
		}
		// Parse questions from agent output — bd create commands and tables.
		if state.Phase == knowledge.PhaseCapture && w.agentMgr != nil {
			w.interceptQuestionsFromBuffer(state)
			w.checkForQuestionsInOutput()
		}
		// Retry kick if agent is stuck.
		if state.Phase == knowledge.PhaseCapture && w.agentMgr != nil {
			w.retryKickIfStale(state)
		}
		// Fallback: auto-generate questions if the agent is idle.
		// Only fires if: (1) minimum time has elapsed, AND (2) pluk shows
		// no agent activity for agentIdleThreshold. This lets the agent
		// take as long as it needs while actively working.
		if state.Phase == knowledge.PhaseCapture {
			if !w.captureSeenAt.IsZero() && time.Since(w.captureSeenAt) > autoQuestionFallbackTimeout {
				w.plukMu.Lock()
				lastActivity := w.plukLastActivity
				w.plukMu.Unlock()
				if lastActivity.IsZero() {
					// No pluk events yet — agent still bootstrapping.
					// Only fall back after 2x the timeout (agent never started).
					if time.Since(w.captureSeenAt) > autoQuestionFallbackTimeout*2 {
						w.logger.Info("question fallback: no pluk events (bootstrap timeout)",
							"elapsed", time.Since(w.captureSeenAt).Round(time.Second),
							"pluk_events", w.plukEventCount,
						)
						w.autoGenerateQuestions(state)
					}
				} else if time.Since(lastActivity) > agentIdleThreshold {
					// Agent was active but went idle — fall back.
					w.logger.Info("question fallback: agent idle",
						"idle_duration", time.Since(lastActivity).Round(time.Second),
						"elapsed", time.Since(w.captureSeenAt).Round(time.Second),
						"pluk_events", w.plukEventCount,
					)
					w.autoGenerateQuestions(state)
				}
				// else: agent is actively producing output — keep waiting.
			}
		}
	case knowledge.PhaseStructure:
		// Check for fact beads — already filtered by StartedAt in
		// findInceptionBeads. User answers gate the phase transition
		// (SubmitAnswers advances to structure), so all beads created
		// after StartedAt in the current inception are valid.
		w.checkForFacts(ctx, inceptionBeads)
		// Intercept fact-like content from buffer
		if state.Phase == knowledge.PhaseStructure && w.agentMgr != nil {
			w.interceptFactsFromBuffer(ctx, state)
		}
		// Retry kick if agent isn't creating facts.
		if state.Phase == knowledge.PhaseStructure && w.agentMgr != nil {
			w.retryKickIfStale(state)
		}
		// Fallback: auto-generate facts if the agent is idle.
		// Same activity-based approach as questions — wait for minimum
		// time, then only fall back if pluk shows no agent activity.
		if state.Phase == knowledge.PhaseStructure {
			if w.structureSeenAt.IsZero() {
				w.structureSeenAt = time.Now()
			}
			if time.Since(w.structureSeenAt) > autoFactFallbackTimeout {
				w.plukMu.Lock()
				lastActivity := w.plukLastActivity
				w.plukMu.Unlock()
				if lastActivity.IsZero() {
					if time.Since(w.structureSeenAt) > autoFactFallbackTimeout*2 {
						w.autoGenerateFacts(ctx, state)
					}
				} else if time.Since(lastActivity) > agentIdleThreshold {
					w.autoGenerateFacts(ctx, state)
				}
			}
		}
	}
}

func (w *InceptionWatcher) reapOldInceptionBeads(beforeTime time.Time) {
	open := beads.StatusOpen
	actor := "brainstorm"
	all := w.beadStore.List(beads.ListFilter{
		Status: &open,
		Actor:  &actor,
	})

	reaped := 0
	for _, b := range all {
		if !strings.HasPrefix(b.ExternalRef, inceptionBeadRefPrefix) {
			continue
		}
		if b.CreatedAt.Before(beforeTime) {
			if err := w.beadStore.Close(b.ID); err == nil {
				reaped++
			}
		}
	}
	if reaped > 0 {
		w.logger.Info("inception watcher: reaped old inception beads", "count", reaped)
	}
}

func inferFactType(title string) string {
	lower := strings.ToLower(title)
	if strings.HasPrefix(lower, "clarification:") {
		return ""
	}
	switch {
	case strings.Contains(lower, "vision") || strings.Contains(lower, "purpose") || strings.Contains(lower, "project goal"):
		return "vision"
	case strings.Contains(lower, "constitution") || strings.Contains(lower, "principles") || strings.Contains(lower, "code style") || strings.Contains(lower, "architecture"):
		return "constitution"
	case strings.Contains(lower, "constraint") || strings.Contains(lower, "boundary") || strings.Contains(lower, "limitation") ||
		strings.Contains(lower, "must not") || strings.Contains(lower, "should not") || strings.Contains(lower, "do not"):
		return "constraint"
	case strings.Contains(lower, "requirement") || containsWord(lower, "must") || strings.Contains(lower, "feature"):
		return "requirement"
	case strings.Contains(lower, "stakeholder") || containsWord(lower, "user") || strings.Contains(lower, "audience"):
		return "stakeholder"
	case strings.Contains(lower, "acceptance") || strings.Contains(lower, "success") || strings.Contains(lower, "criteria") || containsWord(lower, "test"):
		return "acceptance"
	}
	return ""
}

func (w *InceptionWatcher) findInceptionBeads() []*beads.Bead {
	state := w.inception.GetState()
	if state == nil {
		return nil
	}

	// List ALL beads from brainstorm OR inception actors. The agent
	// sometimes uses --actor inception instead of --actor brainstorm,
	// and may omit the inception/ external_ref prefix.
	all := w.beadStore.List(beads.ListFilter{})

	var inception []*beads.Bead
	for _, b := range all {
		if b.CreatedAt.Before(state.StartedAt) {
			continue
		}
		// Match by actor: brainstorm or inception
		if b.Actor != "brainstorm" && b.Actor != "inception" {
			continue
		}
		// Accept beads matching any inception indicator. The agent uses
		// inconsistent formats — cast a wide net.
		hasInceptionRef := strings.HasPrefix(b.ExternalRef, inceptionBeadRefPrefix)
		hasClarificationTitle := strings.Contains(strings.ToLower(b.Title), "clarif") ||
			strings.HasSuffix(strings.TrimSpace(b.Title), "?")
		hasFactKeyword := strings.Contains(strings.ToLower(b.Title), "vision") ||
			strings.Contains(strings.ToLower(b.Title), "requirement") ||
			strings.Contains(strings.ToLower(b.Title), "constraint")
		if hasInceptionRef || hasClarificationTitle || hasFactKeyword || b.Type == beads.TypeAdvisory {
			inception = append(inception, b)
		}
	}
	return inception
}

func (w *InceptionWatcher) checkForQuestions(inceptionBeads []*beads.Bead) {
	var questions []knowledge.Question
	seenIDs := make(map[string]int)
	for _, b := range inceptionBeads {
		cat := b.Meta("category")
		if cat == "" {
			if strings.HasPrefix(b.Title, "Clarification:") || strings.HasPrefix(b.Title, "clarification:") {
				cat = "general"
			} else {
				continue
			}
		}
		qID := b.Meta("question_id")
		if qID == "" {
			qID = cat
		}
		seenIDs[qID]++
		if seenIDs[qID] > 1 {
			qID = fmt.Sprintf("%s-%d", qID, seenIDs[qID])
		}

		title := b.Title
		title = strings.TrimPrefix(title, "Clarification: ")
		title = strings.TrimPrefix(title, "clarification: ")

		questions = append(questions, knowledge.Question{
			ID:       qID,
			Text:     title,
			Default:  b.Meta("default"),
			Category: cat,
		})
	}

	// Deduplicate by ID — the agent output parser can produce duplicate IDs
	seen := make(map[string]bool, len(questions))
	unique := make([]knowledge.Question, 0, len(questions))
	for _, q := range questions {
		if !seen[q.ID] {
			seen[q.ID] = true
			unique = append(unique, q)
		}
	}
	questions = unique

	if len(questions) < minQuestionsForAdvance {
		return
	}
	if len(questions) == w.lastQuestionCount {
		return
	}
	w.lastQuestionCount = len(questions)

	if err := w.inception.SetQuestions(questions); err != nil {
		w.logger.Warn("inception watcher: failed to set questions", "error", err, "count", len(questions))
		return
	}

	w.logger.Info("inception watcher: questions extracted, advancing to clarify",
		"count", len(questions),
	)
}

const (
	outputParseLineCount     = 100
	interceptBufferLineCount = 500
	kickRetryDelayS          = 20 * time.Second
	kickRetryGracePeriodS    = 15 * time.Second
	maxKickRetries           = 5
)

// retryKickIfStale re-sends the inception prompt via SendKick when the
// initial RestartWithBootstrap didn't deliver. Detects stale state by
// checking if the agent is reaping (default mode) instead of processing
// the inception idea.
// plukEvent represents a parsed pluk event from the JSONL stream.
type plukEvent struct {
	Type    string            `json:"type"`
	Session string            `json:"session"`
	Data    map[string]string `json:"data"`
}

const plukLogDir = "/var/run/pluk/logs"

// runPlukSubscriber tails the brainstorm session's pluk JSONL event
// stream and takes immediate action on relevant events. This replaces 5s
// polling with real-time event-driven reactions.
func (w *InceptionWatcher) runPlukSubscriber(ctx context.Context) {
	w.runPlukSubscriberWithSettings(ctx, plukLogFile(), inceptionWatchIntervalS, plukSubscriberPollInterval, plukSubscriberSeekEnd)
}

func (w *InceptionWatcher) runPlukSubscriberWithSettings(ctx context.Context, logFile string, watchInterval, pollInterval time.Duration, seekEnd bool) {
	// Wait for the log file to exist
	for {
		if _, err := os.Stat(logFile); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(watchInterval):
			continue
		}
	}

	w.logger.Info("pluk subscriber started", "logFile", logFile)

	// Open file and seek to end (only new events)
	f, err := os.Open(logFile)
	if err != nil {
		w.logger.Warn("pluk subscriber: cannot open log", "error", err)
		return
	}
	defer func() { _ = f.Close() }()

	// Seek to end — we only care about new events
	if seekEnd {
		if _, err := f.Seek(0, 2); err != nil {
			w.logger.Warn("pluk subscriber: cannot seek to end", "error", err)
		}
	}

	scanner := bufio.NewScanner(f)
	const plukMaxLineBytes = 1 << 20 // 1 MiB — agent output can exceed 64KB default
	scanner.Buffer(make([]byte, 0, plukMaxLineBytes), plukMaxLineBytes)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var event plukEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				continue
			}

			w.handlePlukEvent(event)
		} else {
			// No new data — wait briefly then retry (tail -f behavior)
			time.Sleep(pollInterval)
			// Check if file was rotated by comparing inodes
			openStat, err1 := f.Stat()
			diskStat, err2 := os.Stat(logFile)
			if err1 != nil || err2 != nil {
				w.logger.Warn("pluk log disappeared, stopping subscriber")
				return
			}
			if !os.SameFile(openStat, diskStat) {
				w.logger.Info("pluk log rotated, restarting subscriber")
				return
			}
		}
	}
}

// handlePlukEvent reacts to pluk events from the brainstorm session.
// Called from the Pluk subscriber goroutine — must hold plukMu for field access.
func (w *InceptionWatcher) handlePlukEvent(event plukEvent) {
	w.plukMu.Lock()
	defer w.plukMu.Unlock()

	// Track any agent activity for idle detection
	w.plukLastActivity = time.Now()
	w.plukEventCount++
	if w.plukEventCount%50 == 1 {
		w.logger.Info("pluk event received",
			"count", w.plukEventCount,
			"type", event.Type,
		)
	}

	state := w.inception.GetState()
	if state == nil {
		return
	}

	switch event.Type {
	case "state_change":
		switch event.Data["state"] {
		case "idle":
			phase := state.Phase

			// Agent went idle during capture — if Pluk intercepted questions, apply them now
			if phase == knowledge.PhaseCapture && len(w.plukQuestions) > 0 {
				w.applyPlukQuestions(state)
				return
			}

			// Agent went idle during structure — try to extract facts from
			// buffered Pluk output before falling back to auto-generation.
			if phase == knowledge.PhaseStructure {
				w.plukIdleInStructure = true
				if len(w.plukFactLines) > 0 {
					w.tryExtractFactsFromPluk(w.ctx, state)
				} else {
					// No fact lines from Pluk — auto-generate immediately
					w.autoGenerateFacts(w.ctx, state)
				}
				return
			}

			if w.kickRetryCount < maxKickRetries {
				elapsed := time.Since(state.StartedAt)
				if elapsed > kickRetryGracePeriodS {
					if phase == knowledge.PhaseCapture || phase == knowledge.PhaseStructure {
						w.logger.Info("pluk: agent idle during inception — re-kicking",
							"phase", phase,
							"elapsed", elapsed.Round(time.Second),
						)
						w.kickRetryCount++
						w.lastKickRetry = time.Now()
						msg := w.buildInceptionKickMessage(state)
						go func() {
							if err := w.agentMgr.SendKick("brainstorm", msg); err != nil {
								w.logger.Warn("pluk: re-kick failed", "error", err)
							}
						}()
					}
				}
			}
		case "working":
			w.plukIdleInStructure = false
		}

	case "rate_limit":
		const rateLimitCooldown = 3 * time.Minute
		w.rateLimitedUntil = time.Now().Add(rateLimitCooldown)
		w.logger.Warn("pluk: brainstorm hit rate limit — suppressing retries",
			"phase", state.Phase,
			"cooldown", rateLimitCooldown,
			"message", event.Data["message"],
		)

	case "error":
		w.logger.Warn("pluk: brainstorm error during inception",
			"phase", state.Phase,
			"message", event.Data["message"],
		)

	case "tool_call_completed":
		// bd create/update completed — trigger immediate poll to check for new beads
		if state.Phase == knowledge.PhaseCapture || state.Phase == knowledge.PhaseStructure {
			go w.poll(w.ctx)
		}

	case "raw_output":
		// #4285: pluk puts the pane text on "line" for raw_output, and only
		// "line" — classifier.js builds the event as
		// createEvent(..., 'raw_output', { line }). "message" is a real pluk key,
		// but it belongs to rate_limit and error (handled above), which is
		// evidently where this generalised from. Reading "message" here made the
		// whole raw_output arm dead: every bd-create interception and every
		// keyword re-poll below returned on the empty string.
		// src/deploy/hive-panes.sh has always read data.line and was correct.
		line := event.Data["line"]
		if line == "" {
			return
		}
		lower := strings.ToLower(line)

		// Buffer all bd create lines for parsing (capped to prevent unbounded growth)
		const maxBdCreateLines = 200
		if strings.Contains(lower, "bd create") && strings.Contains(lower, "--title") && len(w.plukBdCreateLines) < maxBdCreateLines {
			w.plukBdCreateLines = append(w.plukBdCreateLines, line)
		}

		switch state.Phase {
		case knowledge.PhaseCapture:
			// Intercept bd create commands — parse question title directly
			if strings.Contains(lower, "bd create") && strings.Contains(lower, "--title") {
				if q := w.parseQuestionFromBdCreate(line); q != nil {
					w.plukQuestions = append(w.plukQuestions, *q)
					if len(w.plukQuestions) >= minQuestionsForAdvance {
						w.applyPlukQuestions(state)
					}
				}
				return
			}
			if strings.Contains(line, "│") {
				for kw := range categoryKeywords {
					if strings.Contains(lower, kw) {
						go w.poll(w.ctx)
						return
					}
				}
			}
		case knowledge.PhaseStructure:
			// Intercept bd create with fact-like titles — extract directly
			if strings.Contains(lower, "bd create") && strings.Contains(lower, "--title") {
				go w.poll(w.ctx)
				return
			}
			if strings.Contains(lower, "bd update") &&
				(strings.Contains(lower, "fact_type") || strings.Contains(lower, "fact_body")) {
				go w.poll(w.ctx)
				return
			}
			// Buffer lines containing fact-like content for extraction
			// when the agent goes idle without creating beads
			const maxPlukFactLines = 200
			if len(w.plukFactLines) < maxPlukFactLines {
				if strings.Contains(lower, "vision") || strings.Contains(lower, "requirement") ||
					strings.Contains(lower, "constraint") || strings.Contains(lower, "constitution") ||
					strings.Contains(lower, "stakeholder") || strings.Contains(lower, "acceptance") ||
					strings.Contains(lower, "architecture") || strings.Contains(lower, "testing") ||
					strings.Contains(lower, "deployment") || containsWord(lower, "must") {
					w.plukFactLines = append(w.plukFactLines, line)
				}
			}
		}
	}
}

func (w *InceptionWatcher) retryKickIfStale(state *knowledge.InceptionState) {
	if w.kickRetryCount >= maxKickRetries {
		return
	}
	if time.Now().Before(w.rateLimitedUntil) {
		return
	}
	if time.Since(state.StartedAt) < kickRetryGracePeriodS {
		return
	}
	if time.Since(w.lastKickRetry) < kickRetryDelayS {
		return
	}

	// Check if agent is doing inception work or stuck in reaping.
	// If the tmux session doesn't exist (on-demand agent not started),
	// GetBufferOutput fails — fall through to RestartWithBootstrap.
	noSession := false
	lines, err := w.agentMgr.GetBufferOutput("brainstorm", outputParseLineCount)
	if err != nil || len(lines) == 0 {
		noSession = true
	}

	isReaping := false
	hasInceptionWork := false
	for _, l := range lines {
		lower := strings.ToLower(l)
		if strings.Contains(lower, "reap:") || strings.Contains(lower, "close stale beads") {
			isReaping = true
		}
		if strings.Contains(lower, "inception") || strings.Contains(lower, "clarif") ||
			strings.Contains(lower, "bd create") || strings.Contains(l, "Clarification:") ||
			strings.Contains(lower, "fact") || strings.Contains(lower, "structur") ||
			strings.Contains(lower, "extract") || strings.Contains(lower, "requirement") {
			hasInceptionWork = true
		}
	}

	if hasInceptionWork {
		return
	}
	if !noSession && !isReaping && time.Since(state.StartedAt) < 2*kickRetryGracePeriodS {
		return
	}

	w.kickRetryCount++
	w.lastKickRetry = time.Now()

	msg := w.buildInceptionKickMessage(state)

	// Try SendKick first (agent already running). If it fails (no session),
	// fall back to RestartWithBootstrap which creates the tmux session.
	if err := w.agentMgr.SendKick("brainstorm", msg); err != nil {
		w.logger.Info("SendKick failed, trying RestartWithBootstrap",
			"attempt", w.kickRetryCount,
			"error", err,
		)
		if err2 := w.agentMgr.RestartWithBootstrap(context.Background(), "brainstorm", msg); err2 != nil {
			w.logger.Warn("inception retry kick failed (both methods)",
				"attempt", w.kickRetryCount,
				"sendKickErr", err,
				"bootstrapErr", err2,
			)
		} else {
			w.logger.Info("inception retry via RestartWithBootstrap succeeded",
				"attempt", w.kickRetryCount,
			)
		}
	} else {
		w.logger.Info("inception retry kick sent",
			"attempt", w.kickRetryCount,
			"isReaping", isReaping,
		)
	}
}

func (w *InceptionWatcher) buildInceptionKickMessage(state *knowledge.InceptionState) string {
	switch state.Phase {
	case knowledge.PhaseCapture:
		return fmt.Sprintf(
			"INCEPTION TASK: You are in the capture phase for idea: %q\n"+
				"Generate at least %d clarification questions using `bd create`.\n"+
				"Each question bead must have external_ref starting with 'inception/'.\n"+
				"DO NOT close any beads with inception/ prefix.\n"+
				"DO NOT run spec-kit during capture phase.",
			state.IdeaText, minQuestionsForAdvance,
		)
	case knowledge.PhaseStructure:
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("INCEPTION TASK: Structure phase for idea: %q\n", state.IdeaText))
		sb.WriteString("The user answered your clarification questions. Create fact beads NOW.\n\n")
		for _, q := range state.Questions {
			ans := state.Answers[q.ID]
			if ans != "" {
				sb.WriteString(fmt.Sprintf("Q: %s\nA: %s\n\n", q.Text, ans))
			}
		}
		sb.WriteString("Create at least 3 fact beads. For EACH fact, run:\n\n")
		sb.WriteString(fmt.Sprintf("bd create --title \"<fact title>\" --type advisory --priority 1 --actor brainstorm --external-ref \"inception/%s\"\n", state.IdeaSlug))
		sb.WriteString("bd update <bead-id> --set-metadata fact_type=\"<vision|constitution|requirement|constraint|stakeholder|acceptance>\"\n")
		sb.WriteString("bd update <bead-id> --set-metadata fact_body=\"<detailed fact content>\"\n\n")
		sb.WriteString("Required facts: 1 vision, 1 constitution, 2+ requirements. Start creating beads IMMEDIATELY.")
		return sb.String()
	default:
		return w.scheduler.BuildAgentMessage("brainstorm", nil, w.scheduler.GetLastActionable())
	}
}

// checkForQuestionsInOutput reads the brainstorm agent's tmux output buffer
// and parses question tables. The model always produces a formatted table of
// questions (with │ delimiters) even when bd create doesn't execute. This
// catches the ~30% of cases where beads aren't created but questions exist.
func (w *InceptionWatcher) checkForQuestionsInOutput() {
	lines, err := w.agentMgr.GetBufferOutput("brainstorm", interceptBufferLineCount)
	if err != nil || len(lines) == 0 {
		return
	}

	questions := parseQuestionTable(lines)
	if len(questions) < minQuestionsForAdvance {
		questions = parseNumberedQuestions(lines)
	}
	// Deduplicate by ID
	{
		seen := make(map[string]bool, len(questions))
		deduped := make([]knowledge.Question, 0, len(questions))
		for _, q := range questions {
			if !seen[q.ID] {
				seen[q.ID] = true
				deduped = append(deduped, q)
			}
		}
		questions = deduped
	}
	if len(questions) < minQuestionsForAdvance {
		return
	}
	if len(questions) == w.lastQuestionCount {
		return
	}
	w.lastQuestionCount = len(questions)

	if err := w.inception.SetQuestions(questions); err != nil {
		w.logger.Warn("inception watcher: failed to set questions from output parse", "error", err, "count", len(questions))
		return
	}

	w.logger.Info("inception watcher: questions extracted from agent output (table parse)",
		"count", len(questions),
	)
}

// categoryKeywords are the valid question categories the agent produces during
// Level-1 ideation. They are passed to the shared agentparse list-parsers.
var categoryKeywords = map[string]bool{
	"users": true, "features": true, "constraints": true,
	"testing": true, "deployment": true, "storage": true,
	"language": true, "general": true,
}

// parseQuestionTable extracts questions from the agent's │-delimited table
// output. It is a thin, behavior-preserving adapter over agentparse.ParseTable:
// the generic parser finds the category column and extracts title/default, and
// this adapter maps each Item into a knowledge.Question (ID == Category, as
// before).
func parseQuestionTable(lines []string) []knowledge.Question {
	items := agentparse.ParseTable(lines, categoryKeywords)
	questions := make([]knowledge.Question, 0, len(items))
	for _, it := range items {
		questions = append(questions, knowledge.Question{
			ID:       it.Category,
			Text:     it.Title,
			Default:  it.Default,
			Category: it.Category,
		})
	}
	return questions
}

// numberedQuestionRe is retained for callers in this file that strip list
// decoration from a single line; it is the same matcher agentparse uses.
var numberedQuestionRe = agentparse.NumberedListRe()

// parseNumberedQuestions extracts questions from numbered or bulleted lists.
// Thin, behavior-preserving adapter over agentparse.ParseNumberedList with the
// "general" fallback category, mapping each Item into a knowledge.Question.
func parseNumberedQuestions(lines []string) []knowledge.Question {
	items := agentparse.ParseNumberedList(lines, categoryKeywords, "general")
	questions := make([]knowledge.Question, 0, len(items))
	for _, it := range items {
		questions = append(questions, knowledge.Question{
			ID:       it.Category,
			Text:     it.Title,
			Default:  "",
			Category: it.Category,
		})
	}
	return questions
}

func (w *InceptionWatcher) checkForFacts(ctx context.Context, inceptionBeads []*beads.Bead) {
	var facts []knowledge.IdeationFact
	for _, b := range inceptionBeads {
		factType := b.Meta("fact_type")
		if factType == "" {
			// Infer fact type from title when metadata is missing
			factType = inferFactType(b.Title)
			if factType == "" {
				continue
			}
		}

		body := b.Meta("fact_body")
		if body == "" {
			body = b.Meta("detail")
		}
		if body == "" {
			body = b.Notes
		}
		if body == "" {
			body = b.Title
		}

		var tags []string
		if t := b.Meta("fact_tags"); t != "" {
			tags = strings.Split(t, ",")
			for i := range tags {
				tags[i] = strings.TrimSpace(tags[i])
			}
		}

		facts = append(facts, knowledge.IdeationFact{
			Title: b.Title,
			Body:  body,
			Type:  knowledge.FactType(factType),
			Tags:  tags,
		})
	}

	if len(facts) < minFactsForAdvance {
		return
	}

	if len(facts) >= targetFactCount {
		w.recordAndAdvanceFacts(ctx, facts)
		return
	}

	if w.factGraceStart.IsZero() {
		w.factGraceStart = time.Now()
		w.logger.Info("inception watcher: facts met minimum, starting enrichment grace period",
			"count", len(facts),
			"target", targetFactCount,
			"grace_seconds", factEnrichmentGracePeriod.Seconds(),
		)
		return
	}

	if time.Since(w.factGraceStart) < factEnrichmentGracePeriod {
		if len(facts) != w.lastFactCount {
			w.lastFactCount = len(facts)
			w.logger.Info("inception watcher: more facts arriving during grace period",
				"count", len(facts),
				"elapsed", time.Since(w.factGraceStart).Round(time.Second),
			)
		}
		return
	}

	w.recordAndAdvanceFacts(ctx, facts)
}

func (w *InceptionWatcher) recordAndAdvanceFacts(ctx context.Context, facts []knowledge.IdeationFact) {
	state := w.inception.GetState()
	if state != nil && len(state.Questions) > 0 && len(state.Answers) > 0 {
		facts = w.supplementFactsFromQA(facts, state)
	}

	if len(facts) == w.lastFactCount {
		return
	}
	w.lastFactCount = len(facts)
	w.factGraceStart = time.Time{}

	if err := w.inception.RecordFacts(ctx, facts); err != nil {
		w.logger.Warn("inception watcher: failed to record facts", "error", err, "count", len(facts))
		return
	}

	w.logger.Info("inception watcher: facts extracted, advancing to scaffold",
		"count", len(facts),
	)
}

func (w *InceptionWatcher) supplementFactsFromQA(existing []knowledge.IdeationFact, state *knowledge.InceptionState) []knowledge.IdeationFact {
	originalCount := len(existing)

	existingTitles := make(map[string]bool)
	hasVision := false
	for _, f := range existing {
		existingTitles[f.Title] = true
		if f.Type == knowledge.FactVision {
			hasVision = true
		}
	}

	categoryToFact := map[string]knowledge.FactType{
		"language":    knowledge.FactConstitution,
		"features":    knowledge.FactRequirement,
		"constraints": knowledge.FactConstraint,
		"users":       knowledge.FactStakeholder,
		"testing":     knowledge.FactAcceptance,
		"deployment":  knowledge.FactConstraint,
		"storage":     knowledge.FactRequirement,
	}

	if !hasVision && state.IdeaText != "" {
		existing = append(existing, knowledge.IdeationFact{
			Title: "Project Vision",
			Body:  state.IdeaText,
			Type:  knowledge.FactVision,
			Tags:  []string{"supplemented"},
		})
	}

	for _, q := range state.Questions {
		answer, ok := state.Answers[q.ID]
		if !ok || strings.TrimSpace(answer) == "" {
			continue
		}
		if existingTitles[q.Text] {
			continue
		}
		existingTitles[q.Text] = true
		factType, mapped := categoryToFact[q.Category]
		if !mapped {
			factType = knowledge.FactRequirement
		}
		existing = append(existing, knowledge.IdeationFact{
			Title: q.Text,
			Body:  answer,
			Type:  factType,
			Tags:  []string{"supplemented"},
		})
	}

	supplemented := len(existing) - originalCount
	if supplemented > 0 {
		w.logger.Info("inception watcher: supplemented facts from Q&A",
			"original", originalCount,
			"supplemented", supplemented,
			"total", len(existing),
		)
	}

	return existing
}

// autoGenerateFacts synthesizes facts from the inception Q&A when the
// brainstorm agent hasn't produced fact beads within the timeout. This
// is a fallback — the agent gets first shot during the timeout window.
func (w *InceptionWatcher) autoGenerateFacts(ctx context.Context, state *knowledge.InceptionState) {
	if len(state.Questions) == 0 || len(state.Answers) == 0 {
		return
	}

	var facts []knowledge.IdeationFact

	// Vision fact from the idea text (greenfield) or repo URL (brownfield)
	visionBody := state.IdeaText
	if visionBody == "" && state.RepoURL != "" {
		visionBody = "Brownfield project from " + state.RepoURL
	}
	if visionBody != "" {
		facts = append(facts, knowledge.IdeationFact{
			Title: "Project Vision",
			Body:  visionBody,
			Type:  knowledge.FactVision,
			Tags:  []string{"auto-generated"},
		})
	}

	// Map question categories to fact types
	categoryToFact := map[string]knowledge.FactType{
		"language":    knowledge.FactConstitution,
		"features":    knowledge.FactRequirement,
		"constraints": knowledge.FactConstraint,
		"users":       knowledge.FactStakeholder,
		"testing":     knowledge.FactAcceptance,
		"deployment":  knowledge.FactConstraint,
		"storage":     knowledge.FactRequirement,
	}

	for _, q := range state.Questions {
		answer, ok := state.Answers[q.ID]
		if !ok || strings.TrimSpace(answer) == "" {
			continue
		}

		factType, mapped := categoryToFact[q.Category]
		if !mapped {
			factType = knowledge.FactRequirement
		}

		facts = append(facts, knowledge.IdeationFact{
			Title: q.Text,
			Body:  answer,
			Type:  factType,
			Tags:  []string{"auto-generated"},
		})
	}

	if len(facts) < minFactsForAdvance {
		return
	}

	if err := w.inception.RecordFacts(ctx, facts); err != nil {
		w.logger.Warn("inception watcher: auto-fact fallback failed", "error", err, "count", len(facts))
		return
	}

	w.inception.IncrementAutoFactCount(len(facts))

	w.logger.Info("inception watcher: auto-generated facts from Q&A (agent timeout fallback)",
		"count", len(facts),
		"timeout", autoFactFallbackTimeout,
	)
}

// autoGenerateQuestions creates default clarification questions from the
// idea text when the brainstorm agent hasn't produced question beads
// within the timeout. Agent gets first shot during the timeout window.
func (w *InceptionWatcher) autoGenerateQuestions(state *knowledge.InceptionState) {
	if len(state.Questions) >= minQuestionsForAdvance {
		return
	}

	// Infer default language from the idea text
	langDefault := "Go"
	ideaLower := strings.ToLower(state.IdeaText)
	switch {
	case containsWord(ideaLower, "java") && !strings.Contains(ideaLower, "javascript"):
		langDefault = "Java"
	case strings.Contains(ideaLower, "python"):
		langDefault = "Python"
	case containsWord(ideaLower, "rust"):
		langDefault = "Rust"
	case strings.Contains(ideaLower, "typescript"):
		langDefault = "TypeScript"
	case strings.Contains(ideaLower, "javascript"):
		langDefault = "JavaScript"
	case strings.Contains(ideaLower, "ruby"):
		langDefault = "Ruby"
	case containsWord(ideaLower, "shell") || containsWord(ideaLower, "bash"):
		langDefault = "Shell/Bash"
	}

	defaults := []knowledge.Question{
		{ID: "language", Text: "What programming language or runtime should this use?", Category: "language", Default: langDefault},
		{ID: "users", Text: "Who are the primary users and how will they interact with it?", Category: "users", Default: "Developers via CLI"},
		{ID: "features", Text: "What are the 2-3 must-have features?", Category: "features", Default: "Core functionality as described"},
		{ID: "constraints", Text: "What constraints or limitations should be respected?", Category: "constraints", Default: "Keep it simple and well-tested"},
		{ID: "testing", Text: "How will you know it is working correctly?", Category: "testing", Default: "Unit tests and integration tests"},
		{ID: "deployment", Text: "How and where will this be deployed?", Category: "deployment", Default: "Docker container"},
	}

	// Merge: keep agent-produced questions, fill remaining slots with defaults
	existing := make(map[string]bool, len(state.Questions))
	for _, q := range state.Questions {
		existing[q.ID] = true
	}
	questions := append([]knowledge.Question{}, state.Questions...)
	for _, d := range defaults {
		if !existing[d.ID] && len(questions) < minQuestionsForAdvance+1 {
			questions = append(questions, d)
		}
	}

	if err := w.inception.SetQuestions(questions); err != nil {
		w.logger.Warn("inception watcher: auto-question fallback failed", "error", err)
		return
	}

	w.inception.IncrementAutoQuestionCount(len(questions))

	w.logger.Info("inception watcher: auto-generated questions (agent timeout fallback)",
		"count", len(questions),
		"timeout", autoQuestionFallbackTimeout,
	)
}

// tryExtractFactsFromPluk attempts to extract structured facts from raw output
// lines buffered by the Pluk subscriber. Called when the agent goes idle during
// structure phase — the agent produced text about facts but didn't create
// beads. This is a Pluk-driven alternative to the 60s auto-fact fallback:
// it fires immediately on idle detection instead of waiting for a timeout.
func (w *InceptionWatcher) tryExtractFactsFromPluk(ctx context.Context, state *knowledge.InceptionState) {
	w.plukMu.Lock()
	factLines := append([]string{}, w.plukFactLines...)
	w.plukMu.Unlock()

	if len(factLines) == 0 || len(state.Answers) == 0 {
		return
	}

	var facts []knowledge.IdeationFact

	// Scan buffered lines for fact-type keywords and extract content
	factTypeKeywords := map[string]knowledge.FactType{
		"vision":       knowledge.FactVision,
		"constitution": knowledge.FactConstitution,
		"requirement":  knowledge.FactRequirement,
		"constraint":   knowledge.FactConstraint,
		"stakeholder":  knowledge.FactStakeholder,
		"acceptance":   knowledge.FactAcceptance,
	}

	seen := make(map[string]bool)
	for _, line := range factLines {
		lower := strings.ToLower(line)
		for keyword, factType := range factTypeKeywords {
			if strings.Contains(lower, keyword) && !seen[keyword] {
				seen[keyword] = true
				body := strings.TrimSpace(line)
				if len(body) > 20 {
					facts = append(facts, knowledge.IdeationFact{
						Title: strings.ToUpper(keyword[:1]) + keyword[1:] + " (from agent output)",
						Body:  body,
						Type:  factType,
						Tags:  []string{"pluk-extracted"},
					})
				}
				break
			}
		}
	}

	if len(facts) < minFactsForAdvance {
		// Not enough facts from Pluk output — fall back to Q&A auto-generation
		// but do it immediately instead of waiting for the 60s timeout
		w.logger.Info("pluk: insufficient facts from output, using Q&A fallback",
			"pluk_facts", len(facts),
			"needed", minFactsForAdvance,
		)
		w.autoGenerateFacts(ctx, state)
		return
	}

	if err := w.inception.RecordFacts(ctx, facts); err != nil {
		w.logger.Warn("pluk: Pluk fact extraction failed", "error", err, "count", len(facts))
		return
	}

	w.inception.IncrementAutoFactCount(len(facts))

	w.logger.Info("pluk: extracted facts from agent output (no bead creation needed)",
		"count", len(facts),
		"source", "pluk-raw-output",
	)
	w.plukMu.Lock()
	w.plukFactLines = nil
	w.plukMu.Unlock()
}

// interceptFactsFromBuffer reads the tmux buffer and buffers fact-like
// lines for extraction. If enough content is found and the agent has gone
// idle (no new lines since last poll), triggers fact extraction immediately.
func (w *InceptionWatcher) interceptFactsFromBuffer(ctx context.Context, state *knowledge.InceptionState) {
	if len(state.FactSlugs) >= minFactsForAdvance {
		return
	}

	lines, err := w.agentMgr.GetBufferOutput("brainstorm", interceptBufferLineCount)
	if err != nil || len(lines) == 0 {
		return
	}

	w.plukMu.Lock()
	defer w.plukMu.Unlock()

	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "vision") || strings.Contains(lower, "requirement") ||
			strings.Contains(lower, "constraint") || strings.Contains(lower, "constitution") ||
			strings.Contains(lower, "stakeholder") || strings.Contains(lower, "acceptance") ||
			strings.Contains(lower, "architecture") || strings.Contains(lower, "testing") ||
			strings.Contains(lower, "deployment") {
			found := false
			for _, existing := range w.plukFactLines {
				if existing == line {
					found = true
					break
				}
			}
			if !found && len(w.plukFactLines) < 200 {
				w.plukFactLines = append(w.plukFactLines, line)
			}
		}
	}
}

// interceptQuestionsFromBuffer reads the tmux output buffer directly and
// parses questions from multiple formats: bd create commands, tool call
// previews, numbered lists, and question-mark lines with category keywords.
func (w *InceptionWatcher) interceptQuestionsFromBuffer(state *knowledge.InceptionState) {
	if len(state.Questions) >= minQuestionsForAdvance {
		return
	}

	lines, err := w.agentMgr.GetBufferOutput("brainstorm", interceptBufferLineCount)
	if err != nil || len(lines) == 0 {
		return
	}

	w.plukMu.Lock()
	defer w.plukMu.Unlock()

	for _, line := range lines {
		lower := strings.ToLower(line)
		var q *knowledge.Question

		switch {
		case strings.Contains(lower, "bd create") && strings.Contains(lower, "--title"):
			q = w.parseQuestionFromBdCreate(line)

		case strings.HasPrefix(strings.TrimSpace(line), "└") && strings.Contains(lower, "--title"):
			q = w.parseQuestionFromBdCreate(line)

		case strings.HasSuffix(strings.TrimSpace(line), "?") && len(line) > 20:
			for kw := range categoryKeywords {
				if strings.Contains(lower, kw) {
					text := strings.TrimSpace(line)
					if m := numberedQuestionRe.FindStringSubmatch(text); m != nil {
						text = strings.TrimSpace(m[2])
					}
					q = &knowledge.Question{
						ID:       kw + "-" + fmt.Sprintf("%d", len(w.plukQuestions)+1),
						Text:     text,
						Category: kw,
						Default:  "Yes, use best practices",
					}
					break
				}
			}
		}

		if q != nil {
			found := false
			for _, existing := range w.plukQuestions {
				if existing.Text == q.Text {
					found = true
					break
				}
			}
			if !found {
				w.plukQuestions = append(w.plukQuestions, *q)
			}
		}
	}

	if len(w.plukQuestions) >= minQuestionsForAdvance {
		w.applyPlukQuestions(state)
	}
}

// isTemplatePlaceholder rejects placeholder titles that the agent outputs
// as part of showing a command template before filling in actual values.
// Thin, behavior-preserving adapter over agentparse.IsTemplatePlaceholder.
func isTemplatePlaceholder(title string) bool {
	return agentparse.IsTemplatePlaceholder(title)
}

// parseQuestionFromBdCreate extracts a question from a bd create command
// in the agent's raw output. Works regardless of --type, --actor, or
// --external-ref values — the title IS the question. Title extraction and
// placeholder rejection are delegated to agentparse.TitleFromBdCreate; category
// inference and prefix handling stay here (inception-specific).
func (w *InceptionWatcher) parseQuestionFromBdCreate(line string) *knowledge.Question {
	title, ok := agentparse.TitleFromBdCreate(line)
	if !ok {
		return nil
	}

	// Infer category from title or known keywords
	lower := strings.ToLower(title)
	cat := "general"
	for kw := range categoryKeywords {
		if strings.Contains(lower, kw) {
			cat = kw
			break
		}
	}

	// Remove "Clarification: " prefix if present
	title = strings.TrimPrefix(title, "Clarification: ")
	title = strings.TrimPrefix(title, "clarification: ")

	qID := cat + "-" + fmt.Sprintf("%d", len(w.plukQuestions)+1)

	w.logger.Info("pluk: intercepted question from bd create",
		"title", title,
		"category", cat,
	)

	return &knowledge.Question{
		ID:       qID,
		Text:     title,
		Category: cat,
		Default:  "Yes, use best practices",
	}
}

// applyPlukQuestions sets intercepted questions on the inception engine,
// bypassing the bead store entirely. Called when Pluk has captured enough
// questions from the agent's bd create output.
func (w *InceptionWatcher) applyPlukQuestions(state *knowledge.InceptionState) {
	if len(w.plukQuestions) < minQuestionsForAdvance {
		return
	}
	if len(state.Questions) >= minQuestionsForAdvance {
		return
	}

	// Deduplicate by text
	seen := make(map[string]bool)
	var unique []knowledge.Question
	for _, q := range w.plukQuestions {
		if !seen[q.Text] {
			seen[q.Text] = true
			unique = append(unique, q)
		}
	}

	if len(unique) < minQuestionsForAdvance {
		return
	}

	if err := w.inception.SetQuestions(unique); err != nil {
		w.logger.Warn("pluk: failed to apply intercepted questions", "error", err, "count", len(unique))
		return
	}

	w.logger.Info("pluk: applied intercepted questions — bypassed bead store",
		"count", len(unique),
		"source", "pluk-bd-create-intercept",
	)
	w.plukQuestions = nil
}

func containsWord(text, word string) bool {
	idx := 0
	for {
		pos := strings.Index(text[idx:], word)
		if pos < 0 {
			return false
		}
		absPos := idx + pos
		endPos := absPos + len(word)
		atStart := absPos == 0 || !isAlpha(text[absPos-1])
		atEnd := endPos >= len(text) || !isAlpha(text[endPos])
		if atStart && atEnd {
			return true
		}
		idx = absPos + 1
	}
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
