package main

import (
	"log/slog"
	"path/filepath"

	"github.com/hivecommons/hive/pkg/agentaudit"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/publish"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/notify"
)

// outcomeStateDir holds the durable outcome ledger the publisher books
// campaign predictions on, beside the mutation ledger and journal.
const outcomeStateDir = "/data/convergence/outcome"

// outcomeLedgerFile is the ledger file name under outcomeStateDir.
const outcomeLedgerFile = "ledger.json"

// notifierChannel adapts the hive notifier to the publisher's private notify
// channel; sensitive findings are announced at default priority.
type notifierChannel struct{ n *notify.Notifier }

func (c notifierChannel) Send(title, message string) {
	if c.n == nil {
		return
	}
	c.n.Send(title, message, notify.PriorityDefault)
}

// publicationPolicy is the live authorization snapshot the publisher judges
// every publication against: the operator opt-in (which also requires a
// named campaign owner), the current ACMM level, and the resolved
// convergence mode. cfg is the live pointer, so a dashboard flip takes effect
// on the next publication without a restart.
func publicationPolicy(cfg *config.Config) publish.Policy {
	if cfg == nil {
		return publish.Policy{}
	}
	return publish.Policy{
		Enabled:   cfg.Publication.Enabled && cfg.Publication.Owner != "",
		ACMMLevel: cfg.ACMMLevelOrZero(),
		Mode:      cfg.ConvergenceMode(),
	}
}

// privateChannelFor maps publication.private_channel to a channel: a private
// repository filed through the issue seam, or the operator notifier. An
// empty or unroutable channel is nil, which the publisher refuses for every
// sensitive finding rather than filing it publicly.
func privateChannelFor(cfg *config.Config, seam forge.IssueSeam, notifier *notify.Notifier) publish.PrivateChannel {
	if cfg == nil {
		return nil
	}
	kind, target := cfg.Publication.PrivateChannelKind()
	switch kind {
	case "repo":
		if seam == nil {
			return nil
		}
		return publish.RepoChannel{Repo: target, Issues: seam}
	case "notify":
		if notifier == nil {
			return nil
		}
		return publish.NotifyChannel{Notifier: notifierChannel{n: notifier}}
	}
	return nil
}

// buildFindingPublisher composes the authorized issue publisher (#8353) from
// the already-wired mutation boundary, the GitHub client, the notifier, and
// the audit sink. It performs no forge I/O: the publisher is inert until
// publication.enabled is set, the campaign owner is named, the ACMM level is
// at least config.PublicationMinACMMLevel, and the convergence mode is
// enforce. A missing boundary means no journal to publish through, so no
// publisher is built.
func buildFindingPublisher(cfg *config.Config, boundary *mutation.Boundary, seam forge.IssueSeam, notifier *notify.Notifier, audit agentaudit.AuditSink, logger *slog.Logger) (*publish.Publisher, *outcome.Ledger) {
	if cfg == nil || boundary == nil {
		return nil, nil
	}
	owner := cfg.Publication.Owner
	ledger, err := outcome.Open(filepath.Join(outcomeStateDir, outcomeLedgerFile), outcome.Options{Principals: []string{owner}})
	if err != nil {
		logger.Error("outcome ledger unavailable; campaign outcomes will not be booked", "error", err)
	}
	p := &publish.Publisher{
		Executor:   boundary.Executor,
		Issues:     seam,
		Private:    privateChannelFor(cfg, seam, notifier),
		Classify:   publish.ConservativeClassifier,
		PolicyFunc: func() publish.Policy { return publicationPolicy(cfg) },
		Actor:      owner,
		Audit:      audit,
		Logger:     logger,
	}
	return p, ledger
}

// wireFindingPublisher installs the publisher on the boot. It is nil-safe on
// every dependency so a test harness or a hive booted without GitHub cannot
// panic here; such a hive gets a publisher that refuses with a typed error.
func (b *boot) wireFindingPublisher() {
	boundary, _ := b.mutationBoundary.(*mutation.Boundary)
	var seam forge.IssueSeam
	if b.cfg != nil && b.cfg.Project.ForgeKind() == config.ForgeGitHub && b.ghClient != nil {
		seam = forge.NewGitHubIssueSeam(b.ghClient, b.cfg.Project.Org)
	}
	var audit agentaudit.AuditSink
	if b.dashSrv != nil {
		audit = b.dashSrv.AgentAuditSink()
	}
	b.findingPublisher, b.outcomeLedger = buildFindingPublisher(b.cfg, boundary, seam, b.notifier, audit, b.logger)
	if b.findingPublisher == nil {
		return
	}
	kind, _ := b.cfg.Publication.PrivateChannelKind()
	b.logger.Info("finding publisher wired",
		"enabled", b.cfg.Publication.Enabled,
		"owner", b.cfg.Publication.Owner,
		"private_channel", kind,
		"min_acmm_level", config.PublicationMinACMMLevel,
		"issue_seam", seam != nil,
		"outcome_ledger", b.outcomeLedger != nil)
}
