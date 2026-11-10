package adminmcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fenceReposFixture is GET /api/config/auto-merge for a hive whose
// hive-wide fence scope is all, with org/b overriding it and only org/a and
// org/b holding human-merge paths.
func fenceReposFixture() map[string]any {
	f := autoMergeFixture()
	cl := f["contributor_lane"].(map[string]any)
	cl["workflow_fence_scope"] = "all"
	cl["workflow_fence_scope_repos"] = []any{map[string]any{"repo": "org/b", "workflow_fence_scope": "human_merge_paths"}}
	cl["workflow_fence_scope_repos_settable"] = true
	return f
}

func fenceReposArgs(repos map[string]any) map[string]any {
	return map[string]any{"contributor_lane": map[string]any{"workflow_fence_scope_repos": repos}}
}

// Setting a repository's own scope sends only that repository, names what
// loosens in it, and says which named repositories keep the full fence for
// want of human-merge paths. On v6 tip auto_merge.update refuses the field as
// unsupported.
func TestAutoMergeUpdateSetsRepoFenceScopeAndNamesWidening(t *testing.T) {
	p := &autoMergeProvider{settings: fenceReposFixture()}
	h := autoMergeHandler(p)
	preview, id := autoMergePreview(t, h, fenceReposArgs(map[string]any{"a": "human_merge_paths", "org/c": "Human_Merge_Paths"}))

	want := `{"contributor_lane":{"workflow_fence_scope_repos":{"org/a":"human_merge_paths","org/c":"human_merge_paths"}}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "Widening:") ||
		!strings.Contains(preview.WideningDisclosure, "workflow edits outside human-merge paths in org/a merge without an owner Approve") ||
		!strings.Contains(preview.WideningDisclosure, "workflow edits outside human-merge paths in org/c merge without an owner Approve (org/c has no human_merge_paths entry yet, so it keeps the full fence until one is set)") {
		t.Fatalf("disclosure = %q", preview.WideningDisclosure)
	}
	effects := strings.Join(preview.Effects, "\n")
	for _, want := range []string{
		"no human_merge_paths entry keeps the full fence",
		"Of the repositories named here, org/c has no human_merge_paths entry.",
		"The hive-wide workflow_fence_scope (all) and every other repository keep their values",
	} {
		if !strings.Contains(effects, want) {
			t.Fatalf("effects missing %q:\n%s", want, effects)
		}
	}
	if got := compactJSON(preview.Details["changes"]); !strings.Contains(got, `{"after":"human_merge_paths","before":"hive-wide (all)","field":"contributor_lane.workflow_fence_scope_repos[org/a]"}`) {
		t.Fatalf("changes = %s", got)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != want {
		t.Fatalf("executed %+v", p.requests)
	}
}

// null returns a repository to the hive-wide scope. That narrows when the
// hive-wide scope is all, and widens when it is human_merge_paths.
func TestAutoMergeUpdateClearsRepoFenceScope(t *testing.T) {
	p := &autoMergeProvider{settings: fenceReposFixture()}
	preview, _ := autoMergePreview(t, autoMergeHandler(p), fenceReposArgs(map[string]any{"org/b": nil}))
	if got := compactJSON(preview.Request.Body); got != `{"contributor_lane":{"workflow_fence_scope_repos":{"org/b":null}}}` {
		t.Fatalf("body = %s", got)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "No widening") {
		t.Fatalf("org/b human_merge_paths → hive-wide all is not widening: %q", preview.WideningDisclosure)
	}

	f := fenceReposFixture()
	cl := f["contributor_lane"].(map[string]any)
	cl["workflow_fence_scope"] = "human_merge_paths"
	cl["workflow_fence_scope_repos"] = []any{map[string]any{"repo": "org/b", "workflow_fence_scope": "all"}}
	preview, _ = autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: f}), fenceReposArgs(map[string]any{"org/b": nil}))
	if !strings.Contains(preview.WideningDisclosure, "contributor_lane.workflow_fence_scope_repos[org/b] all → hive-wide (human_merge_paths): workflow edits outside human-merge paths in org/b merge without an owner Approve") {
		t.Fatalf("disclosure = %q", preview.WideningDisclosure)
	}
}

func TestAutoMergeUpdateRepoFenceScopeRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		settings map[string]any
		repos    map[string]any
		want     string
	}{
		"unknown scope":     {fenceReposFixture(), map[string]any{"org/a": "release_only"}, "must be one of all, human_merge_paths"},
		"unwatched":         {fenceReposFixture(), map[string]any{"org/z": "all"}, "is not a repository in project.repos"},
		"clear what is not": {fenceReposFixture(), map[string]any{"org/c": nil}, "has no override to remove"},
		"already so":        {fenceReposFixture(), map[string]any{"org/b": "human_merge_paths"}, "nothing would change"},
		"older hive": {func() map[string]any {
			f := fenceReposFixture()
			delete(f["contributor_lane"].(map[string]any), "workflow_fence_scope_repos_settable")
			return f
		}(), map[string]any{"org/a": "all"}, "older build"},
	} {
		p := &autoMergeProvider{settings: tc.settings}
		_, err := autoMergeHandler(p).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAutoMergeUpdate, "args": fenceReposArgs(tc.repos)})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// The overrides, the hive-wide scope and the human-merge paths the preview
// was built from are all part of its basis: a change to any refuses the
// confirm.
func TestAutoMergeUpdateRepoFenceScopeStalePreview(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"override added": func(s map[string]any) { s["contributor_lane"].(map[string]any)["workflow_fence_scope_repos"] = []any{} },
		"hive-wide changed": func(s map[string]any) {
			s["contributor_lane"].(map[string]any)["workflow_fence_scope"] = "human_merge_paths"
		},
		"paths changed": func(s map[string]any) { s["human_merge_paths"] = map[string][]string{"org/a": {"CODEOWNERS"}} },
	} {
		p := &autoMergeProvider{settings: fenceReposFixture()}
		h := autoMergeHandler(p)
		_, id := autoMergePreview(t, h, fenceReposArgs(map[string]any{"org/a": "human_merge_paths"}))
		mutate(p.settings)
		if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
			t.Fatalf("%s: confirm err = %v, want ErrConfirmationStale", name, err)
		}
		if len(p.requests) != 0 {
			t.Fatalf("%s: a stale confirm wrote %+v", name, p.requests)
		}
	}
}

func TestAutoMergeUpdateRepoFenceScopeRefusesNonOwner(t *testing.T) {
	ownerOnly := fmt.Errorf("%w: Hive API returned HTTP 403: owner access required", ErrForbidden)
	p := &autoMergeProvider{settings: fenceReposFixture(), readErr: ownerOnly}
	_, err := autoMergeHandler(p).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAutoMergeUpdate, "args": fenceReposArgs(map[string]any{"org/a": "human_merge_paths"})})
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner preview err = %v", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("wrote without owner access: %+v", p.requests)
	}
}

// ---- sentinel.settings repo_sensitive_paths ---------------------------------

func sentinelRepoPathsGovernor() map[string]any {
	g := surfaceGovernorWithRepoOverrides()
	g["repos"] = []string{"acme/api", "acme/website", "acme/docs"}
	g["security"].(map[string]any)["sentinel"].(map[string]any)["repoSensitivePathsSettable"] = true
	return g
}

// A per-repository list sends only that repository and names, per repository,
// what Sentinel stops flagging there. On v6 tip sentinel.settings refuses
// repo_sensitive_paths as an unsupported field.
func TestSentinelSettingsSetsRepoSensitivePathsAndNamesWidening(t *testing.T) {
	provider := &surfaceProvider{governor: sentinelRepoPathsGovernor()}
	h := surfaceHandler(provider)
	p, id := capPreview(t, h, WriteOpSentinelSettings, map[string]any{"repo_sensitive_paths": map[string]any{
		"docs": []any{"OWNERS"}, "acme/website": []any{},
	}})
	want := `{"sentinel":{"repoSensitivePaths":{"acme/docs":["OWNERS"],"acme/website":[]}}}`
	if got := compactJSON(p.Request.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	for _, want := range []string{
		"Sentinel stops flagging .github/workflows/**, **/.github/workflows/**, hive.yaml in acme/docs",
		"Sentinel stops flagging OWNERS in acme/website (its own list is empty, so the sensitive_path rule flags no path there)",
	} {
		if !strings.HasPrefix(p.WideningDisclosure, "Widening:") || !strings.Contains(p.WideningDisclosure, want) {
			t.Fatalf("disclosure missing %q: %q", want, p.WideningDisclosure)
		}
	}
	// acme/api is untouched and still named as already in force where it
	// leaves something out; it covers the hive-wide list, so it is not.
	if strings.Contains(p.WideningDisclosure, "acme/api") {
		t.Fatalf("an untouched repository was named: %q", p.WideningDisclosure)
	}
	if !strings.Contains(strings.Join(p.Effects, "\n"), "Sets project.repo_policies[].sentinel_sensitive_paths for acme/docs, acme/website only.") {
		t.Fatalf("effects = %v", p.Effects)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(provider.requests) != 1 || compactJSON(provider.requests[0].Body) != want {
		t.Fatalf("executed %+v", provider.requests)
	}
}

// null clears a repository's list: it goes back to the hive-wide list, which
// flags more there, so it is not widening.
func TestSentinelSettingsClearsRepoSensitivePaths(t *testing.T) {
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: sentinelRepoPathsGovernor()}), WriteOpSentinelSettings, map[string]any{"repo_sensitive_paths": map[string]any{"acme/website": nil}})
	if got := compactJSON(p.Request.Body); got != `{"sentinel":{"repoSensitivePaths":{"acme/website":null}}}` {
		t.Fatalf("body = %s", got)
	}
	if !strings.HasPrefix(p.WideningDisclosure, "No widening") || !strings.Contains(p.WideningDisclosure, "Sentinel flags .github/workflows/**, **/.github/workflows/**, hive.yaml in acme/website") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
}

func TestSentinelSettingsRepoSensitivePathsRefusals(t *testing.T) {
	older := sentinelRepoPathsGovernor()
	delete(older["security"].(map[string]any)["sentinel"].(map[string]any), "repoSensitivePathsSettable")
	for name, tc := range map[string]struct {
		governor map[string]any
		repos    map[string]any
		want     string
	}{
		"blank pattern":     {sentinelRepoPathsGovernor(), map[string]any{"acme/docs": []any{"OWNERS", "  "}}, "empty entry"},
		"malformed glob":    {sentinelRepoPathsGovernor(), map[string]any{"acme/docs": []any{"[x"}}, "not a valid glob"},
		"unwatched":         {sentinelRepoPathsGovernor(), map[string]any{"acme/zzz": []any{"OWNERS"}}, "is not a repository in project.repos"},
		"clear what is not": {sentinelRepoPathsGovernor(), map[string]any{"acme/docs": nil}, "has no list of its own to remove"},
		"already so":        {sentinelRepoPathsGovernor(), map[string]any{"acme/website": []any{"OWNERS"}}, "changes nothing"},
		"older hive":        {older, map[string]any{"acme/docs": []any{"OWNERS"}}, "older build"},
	} {
		h := surfaceHandler(&surfaceProvider{governor: tc.governor})
		_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpSentinelSettings, "args": map[string]any{"repo_sensitive_paths": tc.repos}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// Another repository's list changing after the preview refuses the confirm.
func TestSentinelSettingsRepoSensitivePathsStalePreview(t *testing.T) {
	provider := &surfaceProvider{governor: sentinelRepoPathsGovernor()}
	h := surfaceHandler(provider)
	_, id := capPreview(t, h, WriteOpSentinelSettings, map[string]any{"repo_sensitive_paths": map[string]any{"acme/docs": []any{"OWNERS"}}})
	sn := provider.governor["security"].(map[string]any)["sentinel"].(map[string]any)
	sn["repoSensitivePaths"] = sn["repoSensitivePaths"].([]any)[:1]
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", provider.requests)
	}
}

type repoOverrideDeniedProvider struct{ *surfaceProvider }

func (p repoOverrideDeniedProvider) Read(context.Context, string, map[string]any) (any, error) {
	return nil, fmt.Errorf("%w: Hive API returned HTTP 403: owner access required", ErrForbidden)
}

func TestSentinelSettingsRepoSensitivePathsRefusesNonOwner(t *testing.T) {
	inner := &surfaceProvider{governor: sentinelRepoPathsGovernor()}
	h := NewHandler(repoOverrideDeniedProvider{inner}, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpSentinelSettings, "args": map[string]any{"repo_sensitive_paths": map[string]any{"acme/docs": []any{}}}})
	if err == nil {
		t.Fatal("a caller who cannot read the settings got a preview")
	}
	if len(inner.requests) != 0 {
		t.Fatalf("wrote without owner access: %+v", inner.requests)
	}
}
