package scheduler

import (
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/resolve"
	"github.com/hivecommons/hive/pkg/timeline"
)

type Scheduler struct {
	surgeDuration        func() (time.Duration, bool)
	cfg                  *config.Config
	primer               *knowledge.Primer
	inception            *knowledge.InceptionEngine
	lastActionable       *github.ActionableResult
	logger               *slog.Logger
	promptResolver       *promptsrc.Resolver
	auditFunc            AuditFunc
	advisoryFunc         AdvisoryFunc
	classifier           ioscan.Classifier
	classifierThresholds ioscan.Thresholds
	classifierBudget     int
	inflight             InflightLookup
	runAdmitter          RunAdmitter
	triageCommenter      TriageCommenter
	questionAutocloser   QuestionAutocloser
	lifecycle            timeline.Recorder
	scanned              bool
	deferredKicks        map[string]func(string)
	firstScanDeferred    map[string]bool
	mu                   sync.RWMutex
}

// SetRunTriageDeps attaches the narrow surfaces the scheduler needs for the
// optional runs triage pass. Nil dependencies make the pass classify only and
// leave issues on the existing direct-fix path.
func (s *Scheduler) SetRunTriageDeps(admitter RunAdmitter, commenter TriageCommenter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runAdmitter = admitter
	s.triageCommenter = commenter
}

// SetLifecycleRecorder attaches the lifecycle timeline sink. Once set, every
// classifier pass records a KindClassified stage (lane/tier/model) for each
// classified issue — this is the point where lane routing decides an issue's
// lane, so it is the honest producer for the "classified" stage (#5656).
// A nil recorder (or never calling this) keeps classification silent.
func (s *Scheduler) SetLifecycleRecorder(r timeline.Recorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycle = r
}

// lifecycleRecorder returns the attached recorder, or nil if none is set.
func (s *Scheduler) lifecycleRecorder() timeline.Recorder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lifecycle
}

// recordClassified records one KindClassified journey stage per classified
// issue. The timeline store dedupes by (ref, kind), so per-cycle reruns of the
// classifier refresh the stage rather than appending. No I/O beyond the
// store's own throttled persistence; a nil recorder is a no-op.
func (s *Scheduler) recordClassified(issues []github.Issue) {
	rec := s.lifecycleRecorder()
	if rec == nil {
		return
	}
	for _, issue := range issues {
		ref := issueKey(issue)
		if ref == "" {
			continue
		}
		rec.Record(timeline.Event{
			IssueRef: ref,
			Kind:     timeline.KindClassified,
			Attrs: map[string]string{
				"lane":  issue.Lane,
				"tier":  issue.ComplexityTier,
				"model": issue.ModelRec,
			},
		})
	}
}

// registryFor builds the variable-resolution registry for agentName's kick:
// the current config's hive-level `variables:` block with that agent's own
// `variables:` merged over it (agent wins). It is rebuilt per call (cheap:
// env/static factories only), so a live config reload that changes variable
// definitions is picked up on the next kick without extra wiring.
func (s *Scheduler) registryFor(agentName string) *resolve.Registry {
	return s.cfg.ResolveRegistryForAgent(agentName, s.logger)
}

func New(cfg *config.Config, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:    cfg,
		logger: logger,
	}
}

// SetGitHubPromptResolver attaches a resolver used to fetch an agent's kick
// prompt from a GitHub repo (agent.prompt_source). When nil, agents with a
// prompt_source silently fall back to their inline kick template.
func (s *Scheduler) SetGitHubPromptResolver(r *promptsrc.Resolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptResolver = r
}

// gitHubPromptResolver returns the attached resolver, or nil if none is set.
func (s *Scheduler) gitHubPromptResolver() *promptsrc.Resolver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.promptResolver
}

// SetPrimer attaches a knowledge primer to the scheduler. When set, kick
// messages include relevant facts from the wiki layers.
func (s *Scheduler) SetPrimer(p *knowledge.Primer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primer = p
}

// GetPrimer returns the attached primer, or nil if none is set.
func (s *Scheduler) GetPrimer() *knowledge.Primer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.primer
}

// SetInception attaches an inception engine so kick templates can inject
// ideation state via ${INCEPTION_*} variables.
func (s *Scheduler) SetInception(ie *knowledge.InceptionEngine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inception = ie
}

// GetInception returns the attached inception engine, or nil if none is set.
func (s *Scheduler) GetInception() *knowledge.InceptionEngine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inception
}

// SetLastActionable caches the latest actionable result so manual kicks
// (via the dashboard API) can prime knowledge from the same issue set.
func (s *Scheduler) SetLastActionable(a *github.ActionableResult) {
	s.mu.Lock()
	s.lastActionable = a
	firstScan := !s.scanned
	s.scanned = true
	deferred := s.deferredKicks
	if firstScan && len(deferred) > 0 {
		s.firstScanDeferred = make(map[string]bool, len(deferred))
		for agentName := range deferred {
			s.firstScanDeferred[agentName] = true
		}
	}
	s.deferredKicks = nil
	s.mu.Unlock()

	if firstScan {
		for agentName, deliver := range deferred {
			if deliver == nil {
				continue
			}
			agentName, deliver := agentName, deliver
			if s.logger != nil {
				s.logger.Info("deferred manual kick delivered", "agent", agentName)
			}
			go func() {
				deliver(s.BuildAgentMessageFromLastActionable(agentName))
			}()
		}
	}
}

// FilterFirstScanDeferredAgents removes agents that already have a deferred
// manual kick draining for this first scan. That deferred kick carries the same
// freshly populated work list, so sending the governor's normal kick as well
// would double-deliver the first-scan prompt.
func (s *Scheduler) FilterFirstScanDeferredAgents(agents []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.firstScanDeferred) == 0 {
		return agents
	}
	defer func() { s.firstScanDeferred = nil }()
	if len(agents) == 0 {
		return agents
	}
	filtered := agents[:0]
	for _, targetKey := range agents {
		agentName, _ := config.SplitCadenceTargetKey(targetKey)
		if s.firstScanDeferred[agentName] {
			if s.logger != nil {
				s.logger.Info("governor kick suppressed by deferred manual kick", "agent", agentName, "target", targetKey)
			}
			continue
		}
		filtered = append(filtered, targetKey)
	}
	return filtered
}

// GetLastActionable returns the most recently cached actionable result.
func (s *Scheduler) GetLastActionable() *github.ActionableResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastActionable
}

// FirstScanDone reports whether the governor has published its first
// actionable snapshot. The snapshot may legitimately be empty; this separates
// "no work" from "the first scan has not populated the kick cache yet".
func (s *Scheduler) FirstScanDone() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scanned
}

// DeferKickUntilFirstScan stores one pending manual kick per agent until the
// first governor scan has populated the scheduler's actionable snapshot.
func (s *Scheduler) DeferKickUntilFirstScan(agentName string, deliver func(msg string)) {
	if deliver == nil {
		return
	}
	s.mu.Lock()
	if s.scanned {
		s.mu.Unlock()
		go deliver(s.BuildAgentMessageFromLastActionable(agentName))
		return
	}
	if s.deferredKicks == nil {
		s.deferredKicks = make(map[string]func(string))
	}
	s.deferredKicks[agentName] = deliver
	s.mu.Unlock()
	if s.logger != nil {
		s.logger.Info("manual kick deferred until first governor scan", "agent", agentName)
	}
}
