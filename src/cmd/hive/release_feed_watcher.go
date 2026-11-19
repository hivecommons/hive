package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/releasefeed"
	"github.com/hivecommons/hive/pkg/resolve"
)

// releaseFeedStatePath is where the watcher keeps last-seen versions and
// validators across restarts. A var so tests can point it at a temp dir.
var releaseFeedStatePath = "/data/release-feeds.json"

// releaseFeedKickHeader opens a targeted release-feed kick when the agent's
// own template does not place ${NEW_RELEASES} itself.
const releaseFeedKickHeader = "Upstream dependencies you track published new versions. Act on them now (for example, open the dependency bump); detection was done by the hive's release-feed watcher, so do not poll the feeds yourself.\n\n"

// releaseFeedKicker gates and delivers a release-feed kick. Gating mirrors the
// webhook CI-failure kicker: agent known, enabled and not paused in config or
// at runtime, and governor eligibility (cadence pause, on-demand, budget). A
// refused kick returns releasefeed.ErrGated so the releases stay pending.
type releaseFeedKicker struct {
	cfg      func() *config.Config
	gov      func() *governor.Governor
	isPaused func(agent string) bool
	// render returns the agent's full kick prompt, whose ${NEW_RELEASES}
	// the watcher's Block fills; nil or "" falls back to a targeted kick.
	render func(agent string) string
	// enqueue hands the message to the runtime's kick queue.
	enqueue func(agent, message string) error
}

func (k *releaseFeedKicker) Kick(_ context.Context, agent string, releases []releasefeed.Release) error {
	if reason := k.gate(agent); reason != "" {
		return fmt.Errorf("%w: %s", releasefeed.ErrGated, reason)
	}
	if k.enqueue == nil {
		return fmt.Errorf("%w: no kick sender", releasefeed.ErrGated)
	}
	block := releasefeed.FormatBlock(releases)
	message := ""
	if k.render != nil {
		message = k.render(agent)
	}
	if !strings.Contains(message, releasefeed.BlockHeader) {
		message = releaseFeedKickHeader + block
	}
	// Release names and URLs are upstream text an agent may quote into an
	// issue or PR, so they go through the same mention sanitizer as every
	// other hive-authored kick.
	message = advisory.NeutralizeMentions(message)
	if err := k.enqueue(agent, message); err != nil {
		return err
	}
	if gov := k.governor(); gov != nil {
		gov.RecordKickWithReason(agent, releasefeed.Reason(releases))
	}
	return nil
}

func (k *releaseFeedKicker) gate(agent string) string {
	cfg := k.config()
	if cfg == nil {
		return "no config"
	}
	ac, ok := cfg.Agents[agent]
	if !ok {
		return "unknown agent"
	}
	if !ac.Enabled || ac.Paused || (k.isPaused != nil && k.isPaused(agent)) {
		return "agent paused"
	}
	if gov := k.governor(); gov != nil && !gov.AgentEligibleForCELKick(agent) {
		return "governor gated"
	}
	return ""
}

func (k *releaseFeedKicker) config() *config.Config {
	if k.cfg != nil {
		return k.cfg()
	}
	return nil
}

func (k *releaseFeedKicker) governor() *governor.Governor {
	if k.gov != nil {
		return k.gov()
	}
	return nil
}

// releaseFeedSubscriptions maps governor.release_feeds onto watcher sources.
func releaseFeedSubscriptions(cfg *config.Config) map[string][]releasefeed.Source {
	out := map[string][]releasefeed.Source{}
	if cfg == nil || cfg.Governor.ReleaseFeeds == nil {
		return out
	}
	for agent, a := range cfg.Governor.ReleaseFeeds.Agents {
		if len(a.Sources) == 0 {
			continue
		}
		srcs := make([]releasefeed.Source, 0, len(a.Sources))
		for _, s := range a.Sources {
			srcs = append(srcs, releasefeed.Source{
				Kind: s.Kind, Repo: s.Repo, Package: s.Package, URL: s.URL,
				VersionField: s.VersionField, Name: s.Name, AuthVar: s.AuthVar,
			})
		}
		out[agent] = srcs
	}
	return out
}

// releaseFeedToken resolves a source's auth variable through the agent's
// effective variables (per-agent over hive-level), the same registry that
// fills its kick prompt. An unresolved variable yields "". The value is
// returned to the fetcher only and never logged.
func releaseFeedToken(cfg *config.Config, agent, name string) string {
	if cfg == nil || !config.ValidVariableName(name) {
		return ""
	}
	raw := "${" + name + "}"
	v := cfg.ResolveRegistryForAgent(agent, nil).Expand(context.Background(), raw, resolve.ScopeTemplate, nil)
	if v == raw {
		return ""
	}
	return strings.TrimSpace(v)
}

// releaseFeedConfig is the config the watcher goroutine reads: the immutable
// snapshot the governor loop publishes after every reload (b.sweepConfig),
// never b.cfg itself, which a reload overwrites in place under cfgReloadMu.
// Nil (not yet published) reads as disabled.
func (b *boot) releaseFeedConfig() *config.Config { return b.sweepConfig.Load() }

// newReleaseFeedWatcher builds the watcher from boot state. Config, governor,
// scheduler and runtime are resolved per call so a reload or late wiring is
// picked up without restarting it.
func (b *boot) newReleaseFeedWatcher() *releasefeed.Watcher {
	return releasefeed.New(releasefeed.Options{
		Enabled:       func() bool { return b.releaseFeedConfig().ReleaseFeedsEnabled() },
		Subscriptions: func() map[string][]releasefeed.Source { return releaseFeedSubscriptions(b.releaseFeedConfig()) },
		Interval:      func() time.Duration { return b.releaseFeedConfig().ReleaseFeedInterval() },
		Cooldown:      func(agent string) time.Duration { return b.releaseFeedConfig().ReleaseFeedCooldown(agent) },
		Token:         func(agent, name string) string { return releaseFeedToken(b.releaseFeedConfig(), agent, name) },
		Store:         releasefeed.NewFileStore(releaseFeedStatePath),
		Kicker:        b.newReleaseFeedKicker(),
		Logger:        b.logger,
	})
}

// newReleaseFeedKicker wires the kicker to boot state.
func (b *boot) newReleaseFeedKicker() *releaseFeedKicker {
	return &releaseFeedKicker{
		cfg:      b.releaseFeedConfig,
		gov:      func() *governor.Governor { return b.gov },
		isPaused: func(agent string) bool { return b.agentMgr != nil && b.agentMgr.IsPaused(agent) },
		render: func(agent string) string {
			if b.sched == nil {
				return ""
			}
			return b.sched.BuildAgentMessageFromLastActionable(agent)
		},
		enqueue: func(agent, message string) error {
			if b.agentMgr == nil {
				return errNoAgentRuntime
			}
			// The kick queue (#18285) delivers once the agent is idle, so a
			// release never interrupts a running turn. A runtime without it
			// (CRUX_MODE=sidecar) takes the plain kick.
			if q, ok := b.agentMgr.(crux.KickQueuer); ok {
				_, err := q.EnqueueKick(agent, "", message)
				return err
			}
			return b.agentMgr.SendKick(agent, message)
		},
	}
}

// startReleaseFeedWatcher starts the watcher goroutine once and installs its
// ${NEW_RELEASES} source on the scheduler. The goroutine idles (one cheap
// config check per interval) while governor.release_feeds lists no source,
// so enabling it by a config reload needs no restart.
func (b *boot) startReleaseFeedWatcher() {
	if b.releaseFeed != nil || b.ctx == nil || b.cfg == nil {
		return
	}
	w := b.newReleaseFeedWatcher()
	b.releaseFeed = w
	if b.sched != nil {
		b.sched.SetNewReleasesSource(w.Block)
	}
	go w.Run(b.ctx)
}
