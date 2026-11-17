package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/advisoryengine"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hivetool"
)

// globalAdvisoryEngine is the process-wide shadow advisory engine
// (hivecommons/hive#18464 phase 1). It is built on first use with the
// engine enabled, so a hive with advisory_engine off never constructs it.
var globalAdvisoryEngine struct {
	once   sync.Once
	engine *advisoryengine.Engine
	client atomic.Pointer[github.Client]
}

// advisoryAuditor is the dashboard audit sink the shadow results go to.
type advisoryAuditor interface {
	AuditLog(user, action, detail, agent string)
}

// observeAdvisoryEngine hands the items the lanes were just kicked on to the
// shadow engine. With advisory_engine off it returns before doing anything.
func observeAdvisoryEngine(ctx context.Context, cfg *config.Config, ghClient *github.Client, auditor advisoryAuditor, actionable *github.ActionableResult, logger *slog.Logger) {
	if cfg.AdvisoryEngineMode() == config.AdvisoryEngineModeOff || ghClient == nil || actionable == nil {
		return
	}
	g := &globalAdvisoryEngine
	g.client.Store(ghClient)
	g.once.Do(func() {
		reader := advisoryGitHubReader{client: &g.client}
		g.engine = advisoryengine.New(advisoryengine.Deps{
			Reader: reader,
			RepoPrivate: func(ctx context.Context, repo string) (bool, error) {
				return githubRepoPrivate(g.client.Load())(ctx, repo)
			},
			Record: func(r advisoryengine.Result) {
				if auditor != nil {
					auditor.AuditLog("governor", "advisory_engine_shadow", advisoryAuditDetail(r), "")
				}
			},
			Logger: logger,
		})
	})
	g.engine.Observe(ctx, cfg, advisoryItems(actionable))
}

// advisoryItems projects the actionable GitHub issues, in lane order.
func advisoryItems(a *github.ActionableResult) []advisoryengine.Item {
	var out []advisoryengine.Item
	for _, is := range a.Issues.Items {
		if st := strings.TrimSpace(is.SourceType); st != "" && st != "github" {
			continue
		}
		out = append(out, advisoryengine.Item{
			Repo: is.Repo, Number: is.Number, Title: is.Title, Body: is.Body,
			Labels: is.Labels, Lane: is.Lane, UpdatedAt: is.UpdatedAt,
		})
	}
	return out
}

func advisoryAuditDetail(r advisoryengine.Result) string {
	d := fmt.Sprintf("mode=%s repo=%s number=%d lane=%s private=%t provider=%s model=%s requests=%d outcome=%s verified=%d/%d turns=%d status=%s elapsed=%s",
		r.Mode, r.Repo, r.Number, r.Lane, r.Private, r.Provider, r.Model, r.Requests, r.Outcome, r.Verified, r.Citations, r.Turns, r.Status, r.Elapsed.Round(time.Second))
	for _, kv := range [][2]string{{"skipped", r.Skipped}, {"withheld", r.Withheld}, {"error", r.Error}} {
		if kv[1] != "" {
			d += fmt.Sprintf(" %s=%q", kv[0], kv[1])
		}
	}
	// The engine already empties Summary for private repositories; the
	// check here keeps the audit log metadata-only even if that changes.
	if s := strings.Join(strings.Fields(r.Summary), " "); s != "" && !r.Private {
		if len(s) > 600 {
			s = s[:600] + "…"
		}
		d += fmt.Sprintf(" summary=%q", s)
	}
	return d
}

// advisoryGitHubReader is the engine's only route to GitHub: hivetool's
// GET-only RESTReader over the current client's transport, wrapped so any
// non-GET request is refused before it leaves the process.
type advisoryGitHubReader struct {
	client *atomic.Pointer[github.Client]
}

func (r advisoryGitHubReader) rest() (*hivetool.RESTReader, error) {
	gh := r.client.Load().GoGitHub()
	if gh == nil || gh.BaseURL == nil {
		return nil, errors.New("advisory engine: no GitHub client")
	}
	return &hivetool.RESTReader{BaseURL: gh.BaseURL.String(), Client: advisoryengine.ReadOnlyHTTPClient(gh.Client())}, nil
}

// headRef maps the engine's pinned "HEAD" onto the default branch.
func headRef(ref string) string {
	if ref == "HEAD" {
		return ""
	}
	return ref
}

func (r advisoryGitHubReader) Issue(ctx context.Context, repo string, n int) (hivetool.Issue, error) {
	rr, err := r.rest()
	if err != nil {
		return hivetool.Issue{}, err
	}
	return rr.Issue(ctx, repo, n)
}

func (r advisoryGitHubReader) List(ctx context.Context, repo, ref, dir string) ([]hivetool.Entry, error) {
	rr, err := r.rest()
	if err != nil {
		return nil, err
	}
	return rr.List(ctx, repo, headRef(ref), dir)
}

func (r advisoryGitHubReader) File(ctx context.Context, repo, ref, file string) ([]byte, error) {
	rr, err := r.rest()
	if err != nil {
		return nil, err
	}
	return rr.File(ctx, repo, headRef(ref), file)
}

func (r advisoryGitHubReader) Checks(ctx context.Context, repo, ref string) ([]hivetool.CheckRun, error) {
	rr, err := r.rest()
	if err != nil {
		return nil, err
	}
	return rr.Checks(ctx, repo, ref)
}
