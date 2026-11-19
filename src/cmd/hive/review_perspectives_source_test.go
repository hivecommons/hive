package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/perspsrc"
	"github.com/hivecommons/hive/pkg/review"
)

type fakePerspectivesFetcher struct {
	doc   string
	err   error
	calls int
}

func (f *fakePerspectivesFetcher) GetFileContentRef(_ context.Context, _, _, _, _ string) (string, error) {
	f.calls++
	return f.doc, f.err
}

const (
	perspectivesDocA = "kind: ReviewPerspectives\nperspectives:\n  - name: correctness\n  - name: spyre-numerics\n    focus: Check numerics on the Spyre path.\n"
	perspectivesDocB = "kind: ReviewPerspectives\nperspectives:\n  - name: correctness\n"
)

func perspectivesSourceFixture(f *fakePerspectivesFetcher) (*config.Config, *perspsrc.Resolver, *github.Client, *slog.Logger) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	cfg.Review.PerspectivesSource = &config.DefinitionSourceConfig{Owner: "acme", Repo: "review", Path: "perspectives.yaml"}
	resolver := perspsrc.NewResolver(f, func(string) bool { return true }, logger)
	client := github.NewClientForTest("http://127.0.0.1:0", "acme", nil, logger)
	return cfg, resolver, client, logger
}

func relayAccepts(cfg *config.Config, logger *slog.Logger, name string) bool {
	return reviewPerspectiveSet(cfg, logger).Known(review.Perspective(name))
}

func TestApplyReviewPerspectivesSourceReinstallsRelaySet(t *testing.T) {
	f := &fakePerspectivesFetcher{doc: perspectivesDocA}
	cfg, resolver, client, logger := perspectivesSourceFixture(f)

	applyReviewPerspectivesSource(context.Background(), cfg, resolver, client, logger)
	if !relayAccepts(cfg, logger, "spyre-numerics") {
		t.Fatal("relay set should accept spyre-numerics from doc A")
	}

	f.doc = perspectivesDocB
	applyReviewPerspectivesSource(context.Background(), cfg, resolver, client, logger)
	if relayAccepts(cfg, logger, "spyre-numerics") {
		t.Fatal("relay set still accepts spyre-numerics after doc B removed it")
	}
	if !relayAccepts(cfg, logger, "correctness") {
		t.Fatal("relay set lost correctness")
	}
}

func TestApplyReviewPerspectivesSourceMalformedKeepsRelaySet(t *testing.T) {
	f := &fakePerspectivesFetcher{doc: perspectivesDocA}
	cfg, resolver, client, logger := perspectivesSourceFixture(f)
	applyReviewPerspectivesSource(context.Background(), cfg, resolver, client, logger)

	f.doc = "kind: Nope\n"
	applyReviewPerspectivesSource(context.Background(), cfg, resolver, client, logger)
	if !relayAccepts(cfg, logger, "spyre-numerics") {
		t.Fatal("malformed document dropped the last-known-good set")
	}
	if cfg.Review.LinkedPerspectives == nil || cfg.Review.LinkedPerspectives.Warning == "" {
		t.Fatalf("want a warning on the kept set, got %+v", cfg.Review.LinkedPerspectives)
	}

	f.doc, f.err = "", errors.New("boom")
	applyReviewPerspectivesSource(context.Background(), cfg, resolver, client, logger)
	if !relayAccepts(cfg, logger, "spyre-numerics") {
		t.Fatal("fetch failure dropped the last-known-good set")
	}
}

func TestApplyReviewPerspectivesSourceUnsetMakesNoFetch(t *testing.T) {
	f := &fakePerspectivesFetcher{doc: perspectivesDocA}
	cfg, resolver, client, logger := perspectivesSourceFixture(f)
	cfg.Review.PerspectivesSource = nil
	cfg.Review.Perspectives = []string{"correctness", "security"}

	applyReviewPerspectivesSource(context.Background(), cfg, resolver, client, logger)
	if f.calls != 0 {
		t.Fatalf("unset perspectives_source fetched %d times", f.calls)
	}
	if cfg.Review.LinkedPerspectives != nil {
		t.Fatalf("LinkedPerspectives = %+v, want nil", cfg.Review.LinkedPerspectives)
	}
	if !relayAccepts(cfg, logger, "security") || relayAccepts(cfg, logger, "spyre-numerics") {
		t.Fatal("boot relay set changed")
	}
}
