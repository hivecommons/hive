package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/nousartifacts"
)

// nousStatusArtifactRepoKey is the GET /api/nous/status key under which the
// optional GitHub artifact mirror reports itself (RFC #11585 part 8, #11701).
const nousStatusArtifactRepoKey = "artifact_repo"

// artifactForge adapts *github.Client to nousartifacts.Forge. Every call
// goes through the hive's App-identity client (ETag cache, accounting,
// effects boundary) — never a raw http.Client and never the owner's token.
type artifactForge struct{ gh *github.Client }

func (f artifactForge) DefaultBranch(ctx context.Context, repo string) (string, error) {
	owner, name := f.gh.SplitRepo(repo)
	return f.gh.DefaultBranch(ctx, owner, name)
}

func (f artifactForge) BranchHead(ctx context.Context, repo, branch string) (string, bool, error) {
	return f.gh.BranchHead(ctx, repo, branch)
}

func (f artifactForge) CreateBranch(ctx context.Context, repo, branch, fromSHA string) error {
	return f.gh.CreateBranch(ctx, repo, branch, fromSHA)
}

func (f artifactForge) CommitFiles(ctx context.Context, repo, branch, expectedHeadSHA, headline, body string, files []nousartifacts.File) (string, error) {
	ghFiles := make([]github.ArtifactFile, 0, len(files))
	for _, file := range files {
		ghFiles = append(ghFiles, github.ArtifactFile{Path: file.Path, Content: file.Content})
	}
	return f.gh.CommitFilesOnBranch(ctx, repo, branch, expectedHeadSHA, headline, body, ghFiles)
}

func (f artifactForge) CreatePullRequest(ctx context.Context, repo, head, base, title, body string) (nousartifacts.PullRequest, error) {
	res, err := f.gh.CreatePR(ctx, repo, head, base, title, body)
	if err != nil {
		return nousartifacts.PullRequest{}, err
	}
	return nousartifacts.PullRequest{Number: res.Number, URL: res.URL, AlreadyExisted: res.AlreadyExisted}, nil
}

func (f artifactForge) RepoIsPrivate(ctx context.Context, repo string) (bool, error) {
	return f.gh.RepoIsPrivate(ctx, repo)
}

// nousArtifactBudgetGate reports whether the API budget governor allows the
// optional mirror to spend requests: only Normal mode does. Conserve and
// Critical skip the pass (the syncer records the reason in its status).
func nousArtifactBudgetGate(gh *github.Client) func() (bool, string) {
	return func() (bool, string) {
		mode, _ := gh.APIBudgetMode()
		if mode == github.APIBudgetNormal {
			return true, ""
		}
		return false, "api budget mode " + mode.String()
	}
}

// publishNousArtifactStatus writes the mirror status under
// status.artifact_repo. Disabled hives get {"enabled": false} so the panel
// can tell "off" from "unknown"; nothing else in the status map changes.
func publishNousArtifactStatus(state *dashboard.NousState, st nousartifacts.Status) {
	if state == nil {
		return
	}
	state.Mu.Lock()
	defer state.Mu.Unlock()
	if state.Status == nil {
		state.Status = map[string]interface{}{}
	}
	state.Status[nousStatusArtifactRepoKey] = st
}

// nousArtifactTicker is the periodic trigger; tests replace it with a
// channel they control so no test ever sleeps.
type nousArtifactTicker interface {
	C() <-chan time.Time
	Stop()
}

type realArtifactTicker struct{ t *time.Ticker }

func (r realArtifactTicker) C() <-chan time.Time { return r.t.C }
func (r realArtifactTicker) Stop()               { r.t.Stop() }

func newRealArtifactTicker(interval time.Duration) nousArtifactTicker {
	return realArtifactTicker{t: time.NewTicker(interval)}
}

// startNousArtifactSync wires the optional GitHub artifact mirror. When
// nous.artifact_repo (or HIVE_NOUS_ARTIFACT_REPO) is unset it publishes a
// disabled status and returns nil — zero behaviour change. Otherwise it
// builds the syncer on the hive's GitHub client (budgetOK nil means the
// client's API budget governor), exposes the owner-only
// "Sync now" seam on the dashboard state, runs one pass immediately and
// then one per sync interval until ctx ends.
func startNousArtifactSync(
	ctx context.Context,
	state *dashboard.NousState,
	resolved config.ResolvedNousArtifactRepo,
	root string,
	gh *github.Client,
	budgetOK func() (bool, string),
	audit func(user, action, detail, agent, repo string, target int),
	newTicker func(time.Duration) nousArtifactTicker,
	logger *slog.Logger,
) *nousartifacts.Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	if !resolved.Enabled {
		publishNousArtifactStatus(state, nousartifacts.Status{Enabled: false})
		return nil
	}
	if gh == nil {
		logger.Warn("nous artifact mirror configured but no GitHub client is available; mirror disabled", "repo", resolved.Repo)
		publishNousArtifactStatus(state, nousartifacts.Status{Enabled: false, Repo: resolved.Repo, LastError: "github client unavailable"})
		return nil
	}
	syncer := nousartifacts.New(nousartifacts.Settings{
		Repo:       resolved.Repo,
		Branch:     resolved.Branch,
		PathPrefix: resolved.Path,
		Root:       root,
	}, artifactForge{gh: gh}, logger)
	if budgetOK == nil {
		budgetOK = nousArtifactBudgetGate(gh)
	}
	syncer.BudgetOK = budgetOK
	if audit != nil {
		syncer.Audit = func(action, detail, repo string, pr int) {
			audit(nousartifacts.AuditUser, action, detail, "", repo, pr)
		}
	}
	syncer.Publish = func(st nousartifacts.Status) { publishNousArtifactStatus(state, st) }
	publishNousArtifactStatus(state, syncer.Status())

	if state != nil {
		state.ArtifactSync = func(ctx context.Context) (interface{}, error) {
			res := syncer.RunOnce(ctx, true)
			if res.Skipped != "" {
				return res, errors.New(res.Skipped)
			}
			return res, nil
		}
	}

	if newTicker == nil {
		newTicker = newRealArtifactTicker
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		syncer.RunOnce(ctx, false)
		ticker := newTicker(resolved.SyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C():
				syncer.RunOnce(ctx, false)
			}
		}
	}()
	logger.Info("nous artifact mirror enabled", "repo", resolved.Repo, "branch", resolved.Branch, "path", resolved.Path, "interval", resolved.SyncInterval)
	return syncer
}
