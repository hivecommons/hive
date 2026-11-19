package hivecap

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/logscrub"
)

// The mid-task knowledge adviser for a sidecar runtime (hivecommons/hive#17157).
// In-process, hive hands the manager a crux.KnowledgeAdviser that searches the
// knowledge store in its own address space (SetKnowledgeAdviser). A sidecar
// runtime has no such store, and must never hold the inference credentials the
// ranking pass spends, so the adviser here sends the observed scope (and the
// redacted pane tail, when the policy asks for it) to POST /v1/knowledge/advise
// and receives advice lines back. Everything that decides what is advised stays
// on the hive.

const (
	// DefaultKnowledgeAdviseTimeout bounds one advise call. The hive may run
	// a model pass behind it (up to its own decision timeout), so this is
	// longer than callback.DefaultTimeout; the runtime calls it off the poll
	// goroutine, one call per agent at a time.
	DefaultKnowledgeAdviseTimeout = 30 * time.Second
	// DefaultKnowledgePolicyTTL is how long a per-agent policy answer is
	// kept before it is fetched again in the background, so a hive config
	// change reaches a running agent within it.
	DefaultKnowledgePolicyTTL = 30 * time.Second
	// maxKnowledgeAdviceLines caps how many advice entries one response is
	// allowed to hand the runtime, whatever the hive sent.
	maxKnowledgeAdviceLines = 8
)

// KnowledgeAdviser implements crux.KickBoundKnowledgeAdviser over POST
// /v1/knowledge/advise. It fails closed everywhere: while the hive does not
// advertise the route, Policy reports the advisor disabled (so the runtime
// parses no pane and types nothing); any transport error, non-2xx or
// malformed body is no advice, logged once per kick. The runtime, not this
// adapter, types the lines, and it types only the four KnowledgeAdvice fields.
type KnowledgeAdviser struct {
	caps    callback.KnowledgeAdvisers
	enabled func() bool
	timeout time.Duration
	// TTL is how long a policy answer is kept; non-positive selects
	// DefaultKnowledgePolicyTTL.
	TTL time.Duration
	// Logger receives the one warning per failed kick; nil is slog.Default().
	Logger *slog.Logger
	now    func() time.Time
	// spawn runs a background policy refresh; tests run it inline.
	spawn func(func())

	mu       sync.Mutex
	policies map[string]*knowledgePolicyState
	warned   map[string]time.Time
}

type knowledgePolicyState struct {
	policy     crux.KnowledgeAdvisorPolicy
	fetchedAt  time.Time
	refreshing bool
	failing    bool
}

var _ crux.KickBoundKnowledgeAdviser = (*KnowledgeAdviser)(nil)

// NewKnowledgeAdviser builds the adviser over caps. enabled reports whether
// the hive currently advertises the route (the policy snapshot's
// KnowledgeAdvise flag); nil or a caps that is not a
// callback.KnowledgeAdvisers makes the adviser permanently disabled.
// Non-positive timeout selects DefaultKnowledgeAdviseTimeout.
func NewKnowledgeAdviser(caps any, enabled func() bool, timeout time.Duration) *KnowledgeAdviser {
	if timeout <= 0 {
		timeout = DefaultKnowledgeAdviseTimeout
	}
	k := &KnowledgeAdviser{
		enabled:  enabled,
		timeout:  timeout,
		now:      time.Now,
		spawn:    func(f func()) { go f() },
		policies: map[string]*knowledgePolicyState{},
		warned:   map[string]time.Time{},
	}
	k.caps, _ = caps.(callback.KnowledgeAdvisers)
	return k
}

func (k *KnowledgeAdviser) on() bool {
	return k != nil && k.caps != nil && k.enabled != nil && k.enabled()
}

func (k *KnowledgeAdviser) ttl() time.Duration {
	if k.TTL > 0 {
		return k.TTL
	}
	return DefaultKnowledgePolicyTTL
}

func (k *KnowledgeAdviser) logger() *slog.Logger {
	if k.Logger != nil {
		return k.Logger
	}
	return slog.Default()
}

// Policy answers from the last policy the hive gave for agent and never waits
// on the hive: the manager reads it on every poll tick. The first call for an
// agent, and each call after the TTL, starts a background fetch; until the
// first answer lands (and whenever the last fetch failed, or the hive does not
// advertise the route) the policy is the zero value, so the advisor is off.
func (k *KnowledgeAdviser) Policy(agent string) crux.KnowledgeAdvisorPolicy {
	if !k.on() || agent == "" {
		return crux.KnowledgeAdvisorPolicy{}
	}
	k.mu.Lock()
	st := k.policies[agent]
	if st == nil {
		st = &knowledgePolicyState{}
		k.policies[agent] = st
	}
	refresh := !st.refreshing && (st.fetchedAt.IsZero() || k.now().Sub(st.fetchedAt) >= k.ttl())
	if refresh {
		st.refreshing = true
	}
	policy := st.policy
	k.mu.Unlock()
	if refresh {
		k.spawn(func() { k.refreshPolicy(agent) })
	}
	return policy
}

// refreshPolicy asks the hive for agent's policy without an evaluation.
func (k *KnowledgeAdviser) refreshPolicy(agent string) {
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	resp, err := k.caps.AdviseKnowledge(ctx, callback.KnowledgeAdviseRequest{Agent: agent, PolicyOnly: true})

	k.mu.Lock()
	st := k.policies[agent]
	if st == nil {
		st = &knowledgePolicyState{}
		k.policies[agent] = st
	}
	st.refreshing = false
	st.fetchedAt = k.now()
	wasFailing := st.failing
	st.failing = err != nil
	if err != nil {
		st.policy = crux.KnowledgeAdvisorPolicy{}
	} else {
		st.policy = cruxPolicy(resp.Policy)
	}
	k.mu.Unlock()
	if err != nil && !wasFailing {
		k.logger().Warn("hive knowledge advise policy unavailable; the mid-task advisor stays off for this agent", "agent", agent, "error", err.Error())
	}
}

// Advise is the plain seam; the runtime uses AdviseForKick, which carries the
// kick identity the hive requires. Without one there is no advice.
func (k *KnowledgeAdviser) Advise(string, crux.KnowledgeScope) []crux.KnowledgeAdvice { return nil }

// AdviseForKick sends scope to the hive and returns its advice. The pane tail
// is redacted again here, whatever the runtime did to it, so no token from the
// logscrub set is ever in a request body.
func (k *KnowledgeAdviser) AdviseForKick(agent string, kickAt time.Time, scope crux.KnowledgeScope) []crux.KnowledgeAdvice {
	if !k.on() || agent == "" || kickAt.IsZero() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	resp, err := k.caps.AdviseKnowledge(ctx, callback.KnowledgeAdviseRequest{
		Agent:  agent,
		KickAt: kickAt,
		Scope:  wireScope(scope),
	})
	if err != nil {
		k.warnOnce(agent, kickAt, err)
		return nil
	}
	k.learnPolicy(agent, resp.Policy)
	advice := resp.Advice
	if len(advice) > maxKnowledgeAdviceLines {
		advice = advice[:maxKnowledgeAdviceLines]
	}
	out := make([]crux.KnowledgeAdvice, 0, len(advice))
	for _, a := range advice {
		if a.Slug == "" {
			continue
		}
		out = append(out, crux.KnowledgeAdvice{Slug: a.Slug, Title: a.Title, Summary: a.Summary, Ref: a.Ref})
	}
	return out
}

// learnPolicy keeps the policy an advise response carried as the agent's
// current one.
func (k *KnowledgeAdviser) learnPolicy(agent string, p callback.KnowledgeAdvisorPolicy) {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.policies[agent]
	if st == nil {
		st = &knowledgePolicyState{}
		k.policies[agent] = st
	}
	st.policy = cruxPolicy(p)
	st.fetchedAt = k.now()
	st.failing = false
}

// warnOnce logs the first failure of a kick and swallows the rest: a hive
// that is away costs one line per kick, not one per poll tick.
func (k *KnowledgeAdviser) warnOnce(agent string, kickAt time.Time, err error) {
	k.mu.Lock()
	seen, ok := k.warned[agent]
	k.warned[agent] = kickAt
	k.mu.Unlock()
	if ok && seen.Equal(kickAt) {
		return
	}
	k.logger().Warn("hive knowledge advise failed; no advice for this kick", "agent", agent, "error", err.Error())
}

// wireScope is the scope as it crosses to the hive: every free-text field
// through the same redaction the pane tail gets.
func wireScope(s crux.KnowledgeScope) callback.KnowledgeScope {
	out := callback.KnowledgeScope{
		Kind:     s.Kind,
		Key:      logscrub.RedactPaneTokens(s.Key),
		Path:     logscrub.RedactPaneTokens(s.Path),
		Action:   s.Action,
		PaneTail: logscrub.RedactPaneTokens(s.PaneTail),
	}
	for _, kw := range s.Keywords {
		out.Keywords = append(out.Keywords, logscrub.RedactPaneTokens(kw))
	}
	return out
}

func cruxPolicy(p callback.KnowledgeAdvisorPolicy) crux.KnowledgeAdvisorPolicy {
	return crux.KnowledgeAdvisorPolicy{
		Enabled:       p.Enabled,
		MaxPerKick:    p.MaxPerKick,
		Cooldown:      time.Duration(p.CooldownMS) * time.Millisecond,
		WantsPaneTail: p.WantsPaneTail,
		PaneLines:     p.PaneLines,
	}
}
