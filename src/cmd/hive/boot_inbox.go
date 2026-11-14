package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// inboxSweepTimeout bounds the auto-routing sweep run after a connector sync.
const inboxSweepTimeout = 2 * time.Minute

// bootInbox builds the Inbox service. Sources are the configured knowledge
// connectors whose type is a voice source (inbox.sources, default
// plaud/wisprflow); with none configured the service still exists and lists
// nothing, so the dashboard shows the empty state. Items are read from the connector vaults under <baseDir>/connectors;
// route state persists at <baseDir>/connector-state/_inbox.json (connector
// names cannot start with "_", so it never collides with a connector).
func (b *boot) bootInbox(baseDir string) {
	srcs := b.inboxSources(baseDir, b.cfg.Knowledge.Connectors, b.knowledgeConnectors)
	token := func(ctx context.Context, _ string) (string, error) {
		if b.appAuth == nil {
			return "", nil
		}
		return b.appAuth.Token(ctx)
	}
	kick := func(agent, prompt string) error {
		if b.agentMgr == nil {
			return fmt.Errorf("agent runtime is not available")
		}
		started, err := b.agentMgr.SendKickAsync(agent, prompt)
		if err != nil {
			return err
		}
		if !started {
			return fmt.Errorf("agent %s did not accept the kick", agent)
		}
		return nil
	}
	store := inbox.NewStore(filepath.Join(baseDir, "connector-state", "_inbox.json"))
	svc := inbox.NewService(b.cfg.Inbox, store, srcs, dashboard.NewInboxDestinations(b.cfg, token, kick), b.logger)
	svc.SetReadiness(dashboard.InboxReadiness(b.cfg))
	b.inboxSvc = svc
	b.logger.Info("inbox started", "sources", len(srcs), "rules", len(b.cfg.Inbox.Rules))
}

// inboxSources returns one source per configured voice type (inbox.sources)
// that entries provide. syncer, when non-nil, lets Plaud read recordings
// directly from its connector.
func (b *boot) inboxSources(baseDir string, entries []config.KnowledgeConnector, syncer *connector.Syncer) []inbox.Source {
	configured := map[string]bool{}
	for _, c := range entries {
		configured[c.Type] = true
	}
	var srcs []inbox.Source
	for _, typ := range b.cfg.Inbox.EffectiveSources() {
		if configured[typ] {
			srcs = append(srcs, b.inboxSource(baseDir, typ, entries, syncer))
		}
	}
	return srcs
}

// refreshInboxSources rebuilds the Inbox sources from the live connector set
// (#18091). bootKnowledgeConnectors runs it once the boot syncer exists (the
// Inbox boots first, so its Plaud source would otherwise stay fact-only), and
// reloadKnowledgeConnectors runs it after every dashboard save that reached
// the runtime, so a voice connector added after boot feeds GET /api/inbox
// without a restart. A nil syncer keeps the fact-backed sources.
func (b *boot) refreshInboxSources(entries []config.KnowledgeConnector, syncer *connector.Syncer) {
	if b.inboxSvc == nil {
		return
	}
	srcs := b.inboxSources(knowledge.BaseDir(), entries, syncer)
	b.inboxSvc.SetSources(srcs)
	b.logger.Info("inbox sources refreshed", "sources", len(srcs))
}

// inboxSource returns the fact-backed source for typ. For Plaud it prefers
// reading recordings directly from the connector while it is signed in.
func (b *boot) inboxSource(baseDir, typ string, entries []config.KnowledgeConnector, syncer *connector.Syncer) inbox.Source {
	facts := &inbox.FactSource{ConnectorsDir: filepath.Join(baseDir, "connectors"), SourceType: typ}
	if typ != inbox.PlaudType || syncer == nil {
		return facts
	}
	for _, c := range entries {
		if c.Type != typ {
			continue
		}
		conn, ok := syncer.Connector(c.Name)
		reader, isReader := conn.(connector.PlaudReader)
		if !ok || !isReader {
			continue
		}
		direct := &inbox.PlaudSource{Name: c.Name, Reader: reader}
		if auth, ok := conn.(connector.Authenticator); ok {
			direct.Ready = func(ctx context.Context) bool { return auth.AuthStatus(ctx, false).Connected }
		}
		return &inbox.PreferDirect{Direct: direct, Facts: facts}
	}
	return facts
}

// inboxAfterSync runs the auto-routing rules once a voice connector synced.
func (b *boot) inboxAfterSync(st connector.Status) {
	svc := b.inboxSvc
	if svc == nil {
		return
	}
	for _, typ := range b.cfg.Inbox.EffectiveSources() {
		if typ != st.Type {
			continue
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), inboxSweepTimeout)
			defer cancel()
			if n, err := svc.Sweep(ctx); err != nil {
				b.logger.Warn("inbox sweep", "error", err, "routed", n)
			}
		}()
		return
	}
}
