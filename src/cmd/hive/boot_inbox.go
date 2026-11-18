package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// inboxSweepTimeout bounds the auto-routing sweep run after a connector sync.
const inboxSweepTimeout = 2 * time.Minute

// inboxRecordingSweepInterval is how often idle recordings are finalized and
// audio past inbox.recording.retention_days is deleted.
const inboxRecordingSweepInterval = 10 * time.Minute

// inboxRFCCheckInterval is how often delegated RFC shares are checked for
// their published issue.
const inboxRFCCheckInterval = 10 * time.Minute

// bootInbox builds the Inbox service. Sources are the configured knowledge
// connectors whose type is a voice source (inbox.sources, default
// plaud/wisprflow); with none configured the service still exists and lists
// nothing, so the dashboard shows the empty state. Items are read from the connector vaults under <baseDir>/connectors;
// route state persists at <baseDir>/connector-state/_inbox.json (connector
// names cannot start with "_", so it never collides with a connector), and
// machine transcripts under <baseDir>/connector-state/transcripts.
func (b *boot) bootInbox(baseDir string) {
	srcs := b.inboxSources(baseDir, b.cfg.Knowledge.Connectors, b.knowledgeConnectors)
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
	svc := inbox.NewService(b.cfg.Inbox, store, srcs, dashboard.NewInboxDestinations(b.cfg, kick), b.logger)
	svc.SetReadiness(dashboard.InboxReadiness(b.cfg))
	// Machine transcripts stay on this spoke's own data volume; runs only
	// happen once inbox.transcription is enabled and consented to.
	svc.EnableTranscription(inbox.NewTranscriptStore(filepath.Join(baseDir, "connector-state", "transcripts")))
	// Runs the previous process left queued or running are marked
	// interrupted before anything is queued; the boot transcription sweep
	// (startInboxRecordingSweep) then queues them with every recording
	// still pending, so no recording waits for a click after a restart.
	if n, err := svc.RecoverTranscriptions(); err != nil || n > 0 {
		b.logger.Info("inbox transcriptions recovered after restart", "interrupted", n, "error", err)
	}
	// Files and links dropped onto the Inbox stay on this spoke's data
	// volume (0700 dirs, 0600 files); their extracted text becomes a fact in
	// the personal connectors vault.
	uploads := &inbox.UploadSource{
		Store:         inbox.NewUploadStore(filepath.Join(baseDir, "connector-state", "uploads")),
		ConnectorsDir: filepath.Join(baseDir, "connectors"),
		FactWritten:   b.inboxUploadFactWritten,
		Logger:        b.logger,
		// Links are fetched by the hive with the SSRF guard (own DNS
		// lookup, blocked ranges, every redirect re-checked).
		Fetcher: &inbox.LinkFetcher{UserAgent: "hive-inbox/" + version},
	}
	svc.EnableUploads(uploads)
	// Files mailed to hive@<host> through hive-mx (#18395) are spooled on
	// this spoke's data volume (0700 dirs, 0600 files).
	svc.EnableEmail(&inbox.EmailSource{Dir: filepath.Join(baseDir, "connector-state", "email", "items")})
	b.startInboxUploadRetention(uploads)
	// Dashboard recordings (● Record): audio under connector-state/recordings
	// until inbox.recording.retention_days, facts in the personal layer.
	svc.EnableRecordings(&inbox.RecordingSource{
		Store:         inbox.NewRecordingStore(filepath.Join(baseDir, "connector-state", "recordings")),
		FactDir:       filepath.Join(baseDir, "connectors", inbox.RecordingLayer),
		RetentionDays: func() int { return svc.Config().Recording.EffectiveRetentionDays() },
	})
	b.inboxSvc = svc
	b.startInboxRecordingSweep(svc)
	// A delegated RFC share completes when its issue (carrying the item's
	// provenance reference) shows up on the target repo; the finder reads
	// b.ghClient at call time because that client is installed after boot.
	svc.SetRFCFinder(func(ctx context.Context, repo, marker string) (string, error) {
		if b.ghClient == nil {
			return "", nil
		}
		return b.ghClient.FindIssueByBody(ctx, repo, marker)
	})
	if b.ctx != nil {
		go func() {
			t := time.NewTicker(inboxRFCCheckInterval)
			defer t.Stop()
			for {
				select {
				case <-b.ctx.Done():
					return
				case <-t.C:
					ctx, cancel := context.WithTimeout(b.ctx, inboxSweepTimeout)
					svc.CheckRFCPublications(ctx)
					cancel()
				}
			}
		}()
	}
	b.wireMailAllowlist(baseDir)
	b.logger.Info("inbox started", "sources", len(srcs), "rules", len(b.cfg.Inbox.Rules))
	if tx := b.cfg.Inbox.Transcription; tx.Enabled {
		// Transcription reaches the provider from the hive process itself,
		// so a locked-down spoke must allow this egress
		// (network-requirements.md). auto is logged as what it resolves to.
		st := svc.TranscriptionSettings()
		b.logger.Info("inbox transcription enabled", "mode", st.Mode, "provider", st.Provider, "resolved_provider", st.ResolvedProvider, "egress", strings.Join(st.EgressHosts, ","))
	}
}

// inboxUploadRetentionInterval is how often upload retention runs.
const inboxUploadRetentionInterval = time.Hour

// startInboxUploadRetention deletes upload originals past
// inbox.uploads.*_retention_days, once at boot and then hourly.
func (b *boot) startInboxUploadRetention(u *inbox.UploadSource) {
	if b.ctx == nil {
		return
	}
	go func() {
		t := time.NewTicker(inboxUploadRetentionInterval)
		defer t.Stop()
		for {
			if n, err := u.SweepRetention(); err != nil || n > 0 {
				b.logger.Info("inbox upload retention", "removed", n, "error", err)
			}
			select {
			case <-b.ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// inboxUploadFactWritten connects the connectors vault of dir (if no
// connector did yet) and reindexes it, so a dropped file is searchable at once.
func (b *boot) inboxUploadFactWritten(dir string) {
	api := b.knowledgeAPI
	if api == nil {
		return
	}
	if api.GetVaultStore(dir) == nil {
		if err := api.ConnectVault(dir, "connectors-"+filepath.Base(dir)); err != nil && api.GetVaultStore(dir) == nil {
			b.logger.Warn("inbox uploads: vault not connected", "dir", dir, "error", err)
			return
		}
	}
	if store := api.GetVaultStore(dir); store != nil {
		store.Reindex()
	}
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
	if b.meetingsInboxSrc != nil {
		srcs = append(srcs, b.meetingsInboxSrc)
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

// inboxSource returns the fact-backed source for typ. For Plaud and the
// meeting platforms (zoom, teams, discord, google-meet, webex, slack-huddles, fireflies, fathom, limitless, granola, otter) it prefers reading recordings directly
// from the connector while it is signed in.
func (b *boot) inboxSource(baseDir, typ string, entries []config.KnowledgeConnector, syncer *connector.Syncer) inbox.Source {
	facts := &inbox.FactSource{ConnectorsDir: filepath.Join(baseDir, "connectors"), SourceType: typ}
	if syncer == nil {
		return facts
	}
	for _, c := range entries {
		if c.Type != typ {
			continue
		}
		conn, ok := syncer.Connector(c.Name)
		if !ok {
			continue
		}
		var direct inbox.DirectSource
		ready := func(context.Context) bool { return true }
		if auth, ok := conn.(connector.Authenticator); ok {
			ready = func(ctx context.Context) bool { return auth.AuthStatus(ctx, false).Connected }
		}
		switch r := conn.(type) {
		case connector.PlaudReader:
			direct = &inbox.PlaudSource{Name: c.Name, Reader: r, Ready: ready}
		case connector.MeetingReader:
			direct = &inbox.MeetingSource{SourceType: typ, Name: c.Name, Reader: r, Ready: ready}
		default:
			continue
		}
		return &inbox.PreferDirect{Direct: direct, Facts: facts}
	}
	return facts
}

// inboxTranscribeSweep queues machine transcriptions for opted-in sources
// (inbox.transcription.mode: auto). Runs are audited as inbox_transcribe
// with the AutoTranscribeActor actor.
func (b *boot) inboxTranscribeSweep(ctx context.Context, svc *inbox.Service) {
	opts := inbox.TranscribeOptions{Actor: inbox.AutoTranscribeActor, Audit: func(a inbox.TranscribeAudit) {
		if b.dashSrv != nil {
			b.dashSrv.AuditLog(a.Actor, "inbox_transcribe", a.Detail(), "")
		}
	}}
	if n, err := svc.TranscribeSweep(ctx, opts); err != nil || n > 0 {
		b.logger.Info("inbox transcription sweep", "queued", n, "error", err)
	}
}

// inboxAfterSync runs the auto-routing rules once a voice connector synced,
// then the transcription sweep.
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
			b.inboxTranscribeSweep(ctx, svc)
		}()
		return
	}
}

// startInboxRecordingSweep runs the recording sweep at boot and then every
// inboxRecordingSweepInterval until the boot context ends. Its changes are
// logged and audited with the inbox.RecordingSweepActor actor.
func (b *boot) startInboxRecordingSweep(svc *inbox.Service) {
	ctx := b.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	opts := inbox.RecordingOptions{Actor: inbox.RecordingSweepActor, Audit: func(a inbox.RecordingAudit) {
		if b.dashSrv != nil {
			b.dashSrv.AuditLog(a.Actor, a.Action, a.Detail(), "")
		}
	}, TranscribeAudit: func(a inbox.TranscribeAudit) {
		if b.dashSrv != nil {
			b.dashSrv.AuditLog(a.Actor, "inbox_transcribe", a.Detail(), "")
		}
	}}
	sweep := func() {
		if _, err := svc.RecordingSweep(opts); err != nil {
			b.logger.Warn("inbox recording sweep", "error", err)
		}
		// Recordings finished while transcription was off are transcribed
		// once it is enabled with consent; the first pass, at boot, also
		// queues the runs RecoverTranscriptions found interrupted.
		tctx, cancel := context.WithTimeout(ctx, inboxSweepTimeout)
		b.inboxTranscribeSweep(tctx, svc)
		cancel()
	}
	go func() {
		sweep()
		ticker := time.NewTicker(inboxRecordingSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}
