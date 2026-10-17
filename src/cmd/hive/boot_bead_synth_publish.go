package main

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// beadSynthPublisherSettings maps knowledge.bead_synthesizer.publish to the
// publisher's runtime config and cadence. ok is false when publishing is
// disabled. An empty repo selects the primary repo (or the first repo).
func beadSynthPublisherSettings(cfg *config.Config) (pc knowledge.BeadSynthPublisherConfig, cadence string, ok bool) {
	if cfg == nil || !cfg.Knowledge.BeadSynthesizer.Publish.Enabled {
		return pc, "", false
	}
	pub := cfg.Knowledge.BeadSynthesizer.Publish.WithDefaults()
	repo := strings.TrimSpace(pub.Repo)
	if repo == "" {
		repo = strings.TrimSpace(cfg.Project.PrimaryRepo)
	}
	if repo == "" && len(cfg.Project.Repos) > 0 {
		repo = cfg.Project.Repos[0]
	}
	return knowledge.BeadSynthPublisherConfig{
		Repo:       repo,
		Org:        cfg.Project.Org,
		Path:       pub.Path,
		BaseBranch: pub.BaseBranch,
		Layer:      cfg.Knowledge.BeadSynthesizer.TargetLayer,
	}, pub.Cadence, true
}

// installBeadSynthPublisher attaches the repository publisher to the bead
// synthesizer when publishing is enabled. Publishing always goes through a
// PR: mode=commit is not honored here. Failures are logged and leave the
// local vault write untouched.
func (b *boot) installBeadSynthPublisher() {
	if b.beadSynth == nil {
		return
	}
	pc, cadence, ok := beadSynthPublisherSettings(b.cfg)
	if !ok {
		return
	}
	if mode := b.cfg.Knowledge.BeadSynthesizer.Publish.WithDefaults().Mode; mode != config.DefaultBeadSynthPublishMode {
		b.logger.Warn("bead-synth publish: direct commit is not permitted; publishing via PR", "mode", mode)
	}
	publisher, err := knowledge.NewBeadSynthPublisher(liveBeadSynthRepoClient{current: b.currentGitHubClient}, pc, b.logger)
	if err == nil {
		err = b.beadSynth.SetFactPublisher(publisher, cadence)
	}
	if err != nil {
		b.logger.Warn("bead-synth publish disabled", "error", err)
		return
	}
	b.logger.Info("bead-synth publish enabled", "repo", pc.Repo, "path", pc.Path, "cadence", cadence)
}

// liveBeadSynthRepoClient resolves the hive's current GitHub client on every
// call, so publishing follows App credential rebuilds (#9621).
type liveBeadSynthRepoClient struct {
	current func() *github.Client
}

func (l liveBeadSynthRepoClient) client() (*github.Client, error) {
	if l.current == nil {
		return nil, github.ErrNoGitHubClient
	}
	if c := l.current(); c != nil {
		return c, nil
	}
	return nil, github.ErrNoGitHubClient
}

func (l liveBeadSynthRepoClient) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.DefaultBranch(ctx, owner, repo)
}

func (l liveBeadSynthRepoClient) OpenPRForHead(ctx context.Context, repo, branch string) (int, string, bool, error) {
	c, err := l.client()
	if err != nil {
		return 0, "", false, err
	}
	return c.OpenPRForHead(ctx, repo, branch)
}

func (l liveBeadSynthRepoClient) GetFileContentRef(ctx context.Context, owner, repo, path, ref string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.GetFileContentRef(ctx, owner, repo, path, ref)
}

func (l liveBeadSynthRepoClient) LatestCommitHash(ctx context.Context, owner, repo, branch string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.LatestCommitHash(ctx, owner, repo, branch)
}

func (l liveBeadSynthRepoClient) ResetBranch(ctx context.Context, repo, branch, base string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.ResetBranch(ctx, repo, branch, base)
}

func (l liveBeadSynthRepoClient) CommitFilesOnBranch(ctx context.Context, repo, branch, expectedHeadSHA, headline, body string, files map[string]string) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}
	return c.CommitFilesOnBranch(ctx, repo, branch, expectedHeadSHA, headline, body, files)
}

func (l liveBeadSynthRepoClient) CreatePR(ctx context.Context, repo, head, base, title, body string) (github.CreatePRResult, error) {
	c, err := l.client()
	if err != nil {
		return github.CreatePRResult{}, err
	}
	return c.CreatePR(ctx, repo, head, base, title, body)
}
