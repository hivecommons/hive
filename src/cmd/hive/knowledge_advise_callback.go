package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/config"
)

// POST /v1/knowledge/advise (hivecommons/hive#17157, slice 5 of #17054): the
// hive side of the mid-task knowledge advisor for a sidecar runtime. The
// runtime sends the scope an agent just entered (and the redacted pane tail
// when the policy asks for it); the hive runs the same adviser chain it runs
// in-process (phase 1, wrapped by the ranking pass when configured) and sends
// back advice lines plus the delivery policy. Candidates come from the hive's
// own knowledge store under knowledge.agent_scopes; the request cannot widen
// that scope or pick entries, no inference credential or model setting leaves
// the hive, and the runtime types only the advice fields, never a model reason.
//
// The route is served only while knowledge.sidecar_advise is on (default off).
// Off, it answers 404 not_configured and the hive does not advertise it.

const (
	// knowledgeAdviseMaxEvalsPerScope is the owner's "at most twice per kick"
	// (hivecommons/hive#17054, condition 4): a scope is evaluated on first
	// sighting and once more on its first edit or next gh command. The
	// runtime already behaves that way; the hive refuses a third call for
	// one scope in one kick from a runtime that does not.
	knowledgeAdviseMaxEvalsPerScope = 2
	// knowledgeAdviseMaxScopesPerKick bounds how many distinct scopes one
	// kick may have evaluated, so a misbehaving runtime cannot walk past the
	// per-scope limit by inventing keys.
	knowledgeAdviseMaxScopesPerKick = 128
	// knowledgeAdviseMaxKeyRunes, knowledgeAdviseMaxPathRunes,
	// knowledgeAdviseMaxKeywords and knowledgeAdviseMaxKeywordRunes bound
	// the free-text scope fields. A real scope key is at most three path
	// segments and a path one relative file path.
	knowledgeAdviseMaxKeyRunes     = 256
	knowledgeAdviseMaxPathRunes    = 512
	knowledgeAdviseMaxKeywords     = 16
	knowledgeAdviseMaxKeywordRunes = 64
	// knowledgeAdviseMaxAdvice caps the advice entries one response carries.
	knowledgeAdviseMaxAdvice = 8
	// knowledgeScopeKindPath and knowledgeScopeKindGH are the scope kinds the
	// runtime produces (crux/agent knowledge_advisor.go).
	knowledgeScopeKindPath = "path"
	knowledgeScopeKindGH   = "gh"
)

// sidecarKnowledgeAdvise implements cruxhost.KnowledgeAdviseService over the
// adviser the hive installed through SetKnowledgeAdviser.
type sidecarKnowledgeAdvise struct {
	cfg func() *config.Config
	// adviser is the chain installed on the sidecar host; nil until boot
	// installs it, which keeps the route off.
	adviser func() crux.KnowledgeAdviser
	// status reads one agent from the sidecar runtime: how the hive learns
	// that the agent belongs to the calling runtime and which kick it is in.
	status func(agent string) (*crux.Process, error)

	mu       sync.Mutex
	inFlight map[string]bool
	kicks    map[string]*knowledgeAdviseKick
}

// knowledgeAdviseKick is what the hive has evaluated for one agent's current
// kick, so the per-scope limit survives across calls. It is replaced when the
// agent's kick changes.
type knowledgeAdviseKick struct {
	at    time.Time
	evals map[string]int
}

func newSidecarKnowledgeAdvise(cfg func() *config.Config, adviser func() crux.KnowledgeAdviser, status func(string) (*crux.Process, error)) *sidecarKnowledgeAdvise {
	return &sidecarKnowledgeAdvise{
		cfg:      cfg,
		adviser:  adviser,
		status:   status,
		inFlight: make(map[string]bool),
		kicks:    make(map[string]*knowledgeAdviseKick),
	}
}

// Enabled reports whether the route is on: knowledge, the advisor and
// knowledge.sidecar_advise are all set and an adviser is installed. It reads
// the live config, so a reload turns the route on or off.
func (s *sidecarKnowledgeAdvise) Enabled() bool {
	return s.cfg().KnowledgeSidecarAdviseEnabled() && s.adviser() != nil
}

// AdviseKnowledge answers one runtime request. Every refusal is an error the
// callback server maps to a status (ErrForbidden 403, ErrInvalidRequest 400,
// ErrRateLimited 429, ErrNotConfigured 404), and the runtime reads any of them
// as no advice.
func (s *sidecarKnowledgeAdvise) AdviseKnowledge(_ context.Context, req callback.KnowledgeAdviseRequest) (callback.KnowledgeAdviseResponse, error) {
	if !s.Enabled() {
		return callback.KnowledgeAdviseResponse{}, callback.ErrNotConfigured
	}
	adviser := s.adviser()
	if adviser == nil {
		return callback.KnowledgeAdviseResponse{}, callback.ErrNotConfigured
	}
	if !req.PolicyOnly {
		if err := validateKnowledgeAdviseScope(req.Scope); err != nil {
			return callback.KnowledgeAdviseResponse{}, err
		}
		if req.KickAt.IsZero() {
			return callback.KnowledgeAdviseResponse{}, fmt.Errorf("%w: kick_at is required", callback.ErrInvalidRequest)
		}
	}

	// Identity: the agent must belong to the calling runtime and, for an
	// evaluation, the kick must be the one the agent is in right now.
	proc, err := s.status(req.Agent)
	if err != nil || proc == nil {
		return callback.KnowledgeAdviseResponse{}, fmt.Errorf("%w: agent is not owned by this runtime", callback.ErrForbidden)
	}
	if !req.PolicyOnly && (proc.LastKick == nil || !proc.LastKick.Equal(req.KickAt)) {
		return callback.KnowledgeAdviseResponse{}, fmt.Errorf("%w: kick is not the agent's current kick", callback.ErrForbidden)
	}

	policy := adviser.Policy(req.Agent)
	resp := callback.KnowledgeAdviseResponse{Advice: []callback.KnowledgeAdvice{}, Policy: wireKnowledgePolicy(policy)}
	if req.PolicyOnly || !policy.Enabled {
		return resp, nil
	}

	scope, err := cruxKnowledgeScope(req.Scope, resp.Policy)
	if err != nil {
		return callback.KnowledgeAdviseResponse{}, err
	}
	release, err := s.begin(req.Agent, req.KickAt, scope.Key)
	if err != nil {
		return callback.KnowledgeAdviseResponse{}, err
	}
	defer release()

	for _, adv := range adviser.Advise(req.Agent, scope) {
		if adv.Slug == "" {
			continue
		}
		resp.Advice = append(resp.Advice, callback.KnowledgeAdvice{Slug: adv.Slug, Title: adv.Title, Summary: adv.Summary, Ref: adv.Ref})
		if len(resp.Advice) >= knowledgeAdviseMaxAdvice {
			break
		}
	}
	return resp, nil
}

// begin admits one evaluation: at most one call in flight per agent, and at
// most knowledgeAdviseMaxEvalsPerScope evaluations of a scope per kick. The
// returned func ends the in-flight window. Both limits sit in front of the
// adviser chain, so they also bound what reaches the ranking pass and its
// hive-wide budget and pacer.
func (s *sidecarKnowledgeAdvise) begin(agent string, kickAt time.Time, scopeKey string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[agent] {
		return nil, fmt.Errorf("%w: a call is already in flight for this agent", callback.ErrRateLimited)
	}
	kick := s.kicks[agent]
	if kick == nil || !kick.at.Equal(kickAt) {
		kick = &knowledgeAdviseKick{at: kickAt, evals: make(map[string]int)}
		s.kicks[agent] = kick
	}
	used, seen := kick.evals[scopeKey]
	if used >= knowledgeAdviseMaxEvalsPerScope {
		return nil, fmt.Errorf("%w: this scope was already evaluated %d times in this kick", callback.ErrRateLimited, used)
	}
	if !seen && len(kick.evals) >= knowledgeAdviseMaxScopesPerKick {
		return nil, fmt.Errorf("%w: too many scopes evaluated in this kick", callback.ErrRateLimited)
	}
	kick.evals[scopeKey] = used + 1
	s.inFlight[agent] = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.inFlight, agent)
	}, nil
}

// wireKnowledgePolicy is the adviser's policy on the wire. A pane tail the
// policy wants always has an explicit length, the same default the runtime
// would apply, so both ends enforce one number.
func wireKnowledgePolicy(p crux.KnowledgeAdvisorPolicy) callback.KnowledgeAdvisorPolicy {
	out := callback.KnowledgeAdvisorPolicy{
		Enabled:       p.Enabled,
		MaxPerKick:    p.MaxPerKick,
		CooldownMS:    int(p.Cooldown / time.Millisecond),
		WantsPaneTail: p.WantsPaneTail,
		PaneLines:     p.PaneLines,
	}
	if out.WantsPaneTail && out.PaneLines <= 0 {
		out.PaneLines = config.DefaultAdvisorLLMPaneLines
	}
	return out
}

// validateKnowledgeAdviseScope checks the shape of a scope before anything is
// read from the store: a known kind, a key, bounded text, and a path that is
// relative and stays inside the work tree. Nothing here widens the scope; the
// store still filters by the agent's knowledge.agent_scopes.
func validateKnowledgeAdviseScope(s callback.KnowledgeScope) error {
	switch s.Kind {
	case knowledgeScopeKindPath, knowledgeScopeKindGH:
	default:
		return fmt.Errorf("%w: scope kind must be %q or %q", callback.ErrInvalidRequest, knowledgeScopeKindPath, knowledgeScopeKindGH)
	}
	switch s.Action {
	case "", crux.KnowledgeActionRead, crux.KnowledgeActionEdit, crux.KnowledgeActionRun:
	default:
		return fmt.Errorf("%w: unknown scope action", callback.ErrInvalidRequest)
	}
	if s.Key == "" || utf8.RuneCountInString(s.Key) > knowledgeAdviseMaxKeyRunes || strings.ContainsAny(s.Key, "\r\n\x00") {
		return fmt.Errorf("%w: scope key is empty, too long or multi-line", callback.ErrInvalidRequest)
	}
	if utf8.RuneCountInString(s.Path) > knowledgeAdviseMaxPathRunes || strings.ContainsAny(s.Path, "\r\n\x00") ||
		strings.HasPrefix(s.Path, "/") || strings.HasPrefix(s.Path, "~") || hasParentSegment(s.Path) {
		return fmt.Errorf("%w: scope path must be a short relative path inside the work tree", callback.ErrInvalidRequest)
	}
	if len(s.Keywords) > knowledgeAdviseMaxKeywords {
		return fmt.Errorf("%w: too many scope keywords", callback.ErrInvalidRequest)
	}
	for _, kw := range s.Keywords {
		if utf8.RuneCountInString(kw) > knowledgeAdviseMaxKeywordRunes || strings.ContainsAny(kw, "\r\n\x00") {
			return fmt.Errorf("%w: scope keyword is too long or multi-line", callback.ErrInvalidRequest)
		}
	}
	return nil
}

func hasParentSegment(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// cruxKnowledgeScope turns a validated wire scope into the in-process one. The
// pane tail is dropped when the policy does not want one, rejected (400) when
// it is longer than the policy's pane_lines, and redacted again here, so the
// hive never trusts the runtime's own scrub. Free-text fields get the same
// redaction before they reach a log line or the model prompt.
func cruxKnowledgeScope(w callback.KnowledgeScope, policy callback.KnowledgeAdvisorPolicy) (crux.KnowledgeScope, error) {
	sc := crux.KnowledgeScope{
		Kind:   w.Kind,
		Key:    logscrub.RedactPaneTokens(w.Key),
		Path:   logscrub.RedactPaneTokens(w.Path),
		Action: w.Action,
	}
	for _, kw := range w.Keywords {
		sc.Keywords = append(sc.Keywords, logscrub.RedactPaneTokens(kw))
	}
	if policy.WantsPaneTail && w.PaneTail != "" {
		if n := knowledgePaneLineCount(w.PaneTail); n > policy.PaneLines {
			return crux.KnowledgeScope{}, fmt.Errorf("%w: pane tail has %d lines, pane_lines is %d", callback.ErrInvalidRequest, n, policy.PaneLines)
		}
		sc.PaneTail = logscrub.RedactPaneTokens(w.PaneTail)
	}
	return sc, nil
}

// knowledgePaneLineCount counts lines the way the runtime cuts a pane tail:
// trailing newlines do not count.
func knowledgePaneLineCount(tail string) int {
	tail = strings.TrimRight(tail, "\n")
	if tail == "" {
		return 0
	}
	return strings.Count(tail, "\n") + 1
}
