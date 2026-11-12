package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestBuildReferenceRepoPolicies(t *testing.T) {
	ctx := context.Background()
	deps := referenceRepoDeps{
		botLogin: "hive[bot]",
		discover: func(_ context.Context, org string) (int64, error) {
			if org == "hivecommons" {
				return 1, nil
			}
			return 0, github.ErrNoInstallationForOrg
		},
		auth: func() *github.AppAuth { return nil },
	}
	refs := []config.ReferenceRepo{
		{Repo: "hivecommons/docs"},
		{Repo: "hivecommons/hive", Write: []string{"issues", "prs"}},
	}
	pols, err := buildReferenceRepoPolicies(ctx, refs, deps, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(pols) != 2 {
		t.Fatalf("got %d policies", len(pols))
	}
	if pols[0].Token != nil || pols[0].Issues || pols[0].PRs || pols[0].Login != "" {
		t.Errorf("read-only policy carries write state: %+v", pols[0])
	}
	if pols[1].Token == nil || !pols[1].Issues || !pols[1].PRs || pols[1].Login != "hive[bot]" {
		t.Errorf("write policy incomplete: %+v", pols[1])
	}

	_, err = buildReferenceRepoPolicies(ctx, []config.ReferenceRepo{{Repo: "kubernetes/website", Write: []string{"issues"}}}, deps, testLogger())
	if !errors.Is(err, github.ErrReferenceRepoNoCredential) {
		t.Fatalf("uninstalled org must refuse with ErrReferenceRepoNoCredential, got %v", err)
	}
}
