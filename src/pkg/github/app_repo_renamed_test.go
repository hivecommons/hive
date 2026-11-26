package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// Tests for the same-account rename rule (#19915). The invariant: a configured
// repo is dropped from the not-covered set ONLY when GitHub resolves it to a
// different name under the same account AND the installation listing contains
// that new name. Every other outcome, including a failed lookup, keeps the
// repo missing so a real scope gap can never be hidden.

func renameResolver(answers map[string]string) repoNameResolver {
	return func(_ context.Context, owner, name string) (string, error) {
		got, ok := answers[owner+"/"+name]
		if !ok {
			return "", errors.New("404 Not Found")
		}
		return got, nil
	}
}

func coverageOfRepos(full ...string) InstallationCoverage {
	cov := InstallationCoverage{Repos: map[string]struct{}{}}
	for _, f := range full {
		cov.Repos[f] = struct{}{}
	}
	return cov
}

func TestWithoutRenames(t *testing.T) {
	cases := []struct {
		name        string
		cov         InstallationCoverage
		missing     []string
		answers     map[string]string
		wantMissing []string
		wantRenamed []RepoMove
		why         string
	}{
		{
			name:        "renamed within the account and covered under the new name",
			cov:         coverageOfRepos("tuna-os/installer", "tuna-os/tunaos"),
			missing:     []string{"tuna-os/bootc-installer"},
			answers:     map[string]string{"tuna-os/bootc-installer": "tuna-os/installer"},
			wantMissing: nil,
			wantRenamed: []RepoMove{{Configured: "tuna-os/bootc-installer", CoveredAs: "tuna-os/installer"}},
			why:         "the live #19915 shape: the installation covers the repo under its new name, so nothing is unticked",
		},
		{
			name:        "resolved name differs only in case",
			cov:         coverageOfRepos("acme/new-name"),
			missing:     []string{"acme/old-name"},
			answers:     map[string]string{"acme/old-name": "Acme/New-Name"},
			wantMissing: nil,
			wantRenamed: []RepoMove{{Configured: "acme/old-name", CoveredAs: "acme/new-name"}},
			why:         "GitHub names are case-insensitive and the listing is lowercased",
		},
		{
			name:        "resolves to itself: an ordinary unticked repo",
			cov:         coverageOfRepos("acme/widgets"),
			missing:     []string{"acme/gadgets"},
			answers:     map[string]string{"acme/gadgets": "acme/gadgets"},
			wantMissing: []string{"acme/gadgets"},
			why:         "a public repo the App can read but is not ticked must stay not-covered",
		},
		{
			name:        "renamed but the new name is not ticked either",
			cov:         coverageOfRepos("acme/widgets"),
			missing:     []string{"acme/old"},
			answers:     map[string]string{"acme/old": "acme/new"},
			wantMissing: []string{"acme/old"},
			why:         "only the listing proves coverage; a readable rename does not",
		},
		{
			name:        "transferred to another account",
			cov:         coverageOfRepos("acme/widgets", "other/gadgets"),
			missing:     []string{"acme/gadgets"},
			answers:     map[string]string{"acme/gadgets": "other/gadgets"},
			wantMissing: []string{"acme/gadgets"},
			why:         "an account change is a transfer, which is MovedTo's verdict, not this one",
		},
		{
			name:        "lookup fails",
			cov:         coverageOfRepos("acme/widgets"),
			missing:     []string{"acme/gone"},
			answers:     map[string]string{},
			wantMissing: []string{"acme/gone"},
			why:         "a failed lookup must never hide a real gap",
		},
		{
			name:        "truncated listing",
			cov:         InstallationCoverage{Repos: map[string]struct{}{"acme/new": {}}, Truncated: true},
			missing:     []string{"acme/old"},
			answers:     map[string]string{"acme/old": "acme/new"},
			wantMissing: []string{"acme/old"},
			why:         "a truncated listing proves nothing, so the input passes through unchanged",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMissing, gotRenamed := tc.cov.withoutRenames(context.Background(), tc.missing, renameResolver(tc.answers))
			if !reflect.DeepEqual(gotMissing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v (%s)", gotMissing, tc.wantMissing, tc.why)
			}
			if !reflect.DeepEqual(gotRenamed, tc.wantRenamed) {
				t.Errorf("renamed = %v, want %v (%s)", gotRenamed, tc.wantRenamed, tc.why)
			}
		})
	}
}

// TestWithoutRenames_ProbeLimit pins that repos past renameProbeLimit are not
// looked up and keep their not-covered verdict.
func TestWithoutRenames_ProbeLimit(t *testing.T) {
	const extra = 5
	total := renameProbeLimit + extra
	cov := coverageOfRepos()
	var missing []string
	for i := 0; i < total; i++ {
		missing = append(missing, fmt.Sprintf("acme/old-%03d", i))
		cov.Repos[fmt.Sprintf("acme/new-%03d", i)] = struct{}{}
	}
	calls := 0
	resolve := func(_ context.Context, owner, name string) (string, error) {
		calls++
		return owner + "/new-" + name[len("old-"):], nil
	}

	gotMissing, gotRenamed := cov.withoutRenames(context.Background(), missing, resolve)

	if calls != renameProbeLimit {
		t.Errorf("lookups = %d, want %d", calls, renameProbeLimit)
	}
	if len(gotRenamed) != renameProbeLimit {
		t.Errorf("renamed = %d, want %d", len(gotRenamed), renameProbeLimit)
	}
	if len(gotMissing) != extra {
		t.Errorf("still missing = %d, want %d: repos past the limit keep their verdict", len(gotMissing), extra)
	}
}

// TestResolveRenames_NoKeyPassesThrough pins that without credentials nothing
// is dropped.
func TestResolveRenames_NoKeyPassesThrough(t *testing.T) {
	var a *AppAuth
	missing := []string{"acme/old"}
	got, renamed := a.ResolveRenames(context.Background(), coverageOfRepos("acme/new"), missing)
	if !reflect.DeepEqual(got, missing) || renamed != nil {
		t.Errorf("nil AppAuth: got (%v, %v), want (%v, nil)", got, renamed, missing)
	}
}
