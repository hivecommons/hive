package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// autoMergeProvider answers settings_read section auto_merge with a copy of
// settings, decoded from JSON as the endpoint's answer is, and records writes.
type autoMergeProvider struct {
	settings map[string]any
	readErr  error
	reads    int
	requests []WriteRequest
}

func (p *autoMergeProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != "auto_merge" {
		return map[string]any{}, nil
	}
	p.reads++
	if p.readErr != nil {
		return nil, p.readErr
	}
	b, err := json.Marshal(p.settings)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *autoMergeProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return map[string]any{"ok": true}, nil
}

// autoMergeFixture is GET /api/config/auto-merge for a hive with three
// project repositories, two of them squash-only and with human-merge paths.
func autoMergeFixture() map[string]any {
	return map[string]any{
		"self_authored":                true,
		"max_merges":                   0,
		"min_head_age":                 "3m0s",
		"required_checks":              []string{"build"},
		"allow_unprotected_base":       []string{},
		"no_ci_ok":                     []string{},
		"squash_only":                  []string{"org/a", "org/b"},
		"require_current_base":         []string{},
		"human_merge_paths":            map[string][]string{"org/a": {".github/workflows/**", ".claude/hooks/**"}, "org/b": {"CODEOWNERS"}},
		"project_repos":                []string{"org/a", "org/b", "org/c"},
		"trusted_bot_authors":          []string{"dependabot[bot]"},
		"bot_takeover_enabled":         false,
		"require_current_base_default": false,
		"merge_train":                  map[string]any{"enabled": true, "max_rebase_attempts": 2},
		"native_merge_queue":           map[string]any{"enabled": true, "repos": []string{"org/a"}, "bases": []string{"org/a#main"}, "skip_current_base_gate": false, "probe_ttl": "30m0s"},
		"trusted_authors":              map[string]any{"enabled": false, "repos": []string{}, "require_role": "owner", "require_github_permission": true, "exclude_labels": []string{"hold"}},
		"contributor_lane": map[string]any{
			"enabled": true, "soak_minutes": 30, "require_reviewer_verdict": true, "repos": []string{"org/a"},
			"exclude_labels": []string{"hold"}, "require_tiers": []string{"merger", "trusted"}, "registry_tiers_sufficient": true,
			"auto_promotion_scope": "hive", "approve_held_runs_for_registry_trusted": true, "owner_approval_releases_workflow_fence": true,
		},
	}
}

func autoMergeHandler(p *autoMergeProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func autoMergePreview(t *testing.T, h *Handler, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAutoMergeUpdate, "args": args})
	if err != nil {
		t.Fatalf("preview %v: %v", args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

func TestAutoMergeUpdateRegistered(t *testing.T) {
	if _, ok := DefaultWriteRegistry().Get(WriteOpAutoMergeUpdate); !ok {
		t.Fatalf("%s is not registered", WriteOpAutoMergeUpdate)
	}
}

// An argument the endpoint does not take is refused before anything is read,
// at the top level and inside a nested object.
func TestAutoMergeUpdateRejectsUnknownFields(t *testing.T) {
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"top-level typo":      {map[string]any{"squash_repos": map[string]any{"add": []any{"org/c"}}}, `unsupported merge setting "squash_repos"`},
		"nested unknown":      {map[string]any{"contributor_lane": map[string]any{"enabled": true, "workflow_fence_scope": "all"}}, `unsupported merge setting "contributor_lane.workflow_fence_scope"`},
		"dotted top-level":    {map[string]any{"merge_train.enabled": false}, `unsupported merge setting "merge_train.enabled"`},
		"list without add":    {map[string]any{"squash_only": map[string]any{"set": []any{"org/c"}}}, "takes only add and remove"},
		"list given as array": {map[string]any{"squash_only": []any{"org/c"}}, `takes {"add"`},
	} {
		t.Run(name, func(t *testing.T) {
			p := &autoMergeProvider{settings: autoMergeFixture()}
			h := autoMergeHandler(p)
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAutoMergeUpdate, "args": tc.args})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
				t.Fatalf("a refused preview minted a confirmation: %+v", pending)
			}
			if len(p.requests) != 0 {
				t.Fatalf("a refused preview wrote: %+v", p.requests)
			}
		})
	}
}

// The endpoint replaces a list or map whole, so a change to one repository
// must send every other repository's entry back unchanged.
func TestAutoMergeUpdateOneRepoChangeKeepsTheOthers(t *testing.T) {
	p := &autoMergeProvider{settings: autoMergeFixture()}
	h := autoMergeHandler(p)
	preview, id := autoMergePreview(t, h, map[string]any{
		"human_merge_paths": map[string]any{"org/c": []any{"infra/**"}},
		"squash_only":       map[string]any{"add": []any{"c"}},
	})
	if preview.Request.Method != http.MethodPatch || preview.Request.Path != "/api/config/auto-merge" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	want := `{"human_merge_paths":{"org/a":[".github/workflows/**",".claude/hooks/**"],"org/b":["CODEOWNERS"],"org/c":["infra/**"]},"squash_only":["org/a","org/b","org/c"]}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "No widening:") {
		t.Fatalf("adding a guarded path and a squash-only repository is not widening: %s", preview.WideningDisclosure)
	}
	changes, _ := preview.Details["changes"].([]map[string]any)
	if len(changes) != 2 || changes[0]["field"] != "human_merge_paths[org/c]" || changes[1]["field"] != "squash_only" {
		t.Fatalf("changes = %+v, want human_merge_paths[org/c] and squash_only before and after", changes)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != want {
		t.Fatalf("executed %+v, want body %s", p.requests, want)
	}
}

// Taking a pattern away from human_merge_paths lets the hive merge pull
// requests that touch it, so the preview must say so; adding one must not.
func TestAutoMergeUpdateFlagsHumanMergePathRemovalAsWidening(t *testing.T) {
	for name, tc := range map[string]struct {
		args  map[string]any
		widen bool
		names string
	}{
		"drop one pattern":   {map[string]any{"human_merge_paths": map[string]any{"org/a": []any{".github/workflows/**"}}}, true, ".claude/hooks/**"},
		"unset a repository": {map[string]any{"human_merge_paths": map[string]any{"org/b": nil}}, true, "CODEOWNERS"},
		"add a pattern":      {map[string]any{"human_merge_paths": map[string]any{"org/b": []any{"CODEOWNERS", "SECURITY.md"}}}, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			preview, _ := autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: autoMergeFixture()}), tc.args)
			if got := strings.HasPrefix(preview.WideningDisclosure, "Widening:"); got != tc.widen {
				t.Fatalf("widening = %v, want %v: %s", got, tc.widen, preview.WideningDisclosure)
			}
			if tc.widen && !strings.Contains(preview.WideningDisclosure, tc.names) {
				t.Fatalf("disclosure does not name %s: %s", tc.names, preview.WideningDisclosure)
			}
		})
	}
}

// Other changes that let the hive merge more on its own, and two that look
// similar but do not.
func TestAutoMergeUpdateWideningRules(t *testing.T) {
	for name, tc := range map[string]struct {
		args  map[string]any
		widen bool
	}{
		"trusted authors on":         {map[string]any{"trusted_authors": map[string]any{"enabled": true}}, true},
		"trusted role to merger":     {map[string]any{"trusted_authors": map[string]any{"require_role": "merger"}}, true},
		"merge queue gains a repo":   {map[string]any{"native_merge_queue": map[string]any{"repos": map[string]any{"add": []any{"org/b"}}}}, true},
		"lane allow-list emptied":    {map[string]any{"contributor_lane": map[string]any{"repos": map[string]any{"remove": []any{"org/a"}}}}, true},
		"lane allow-list grows":      {map[string]any{"contributor_lane": map[string]any{"repos": map[string]any{"add": []any{"org/b"}}}}, true},
		"required check removed":     {map[string]any{"required_checks": map[string]any{"remove": []any{"build"}}}, true},
		"squash-only repo removed":   {map[string]any{"squash_only": map[string]any{"remove": []any{"org/b"}}}, true},
		"soak shortened":             {map[string]any{"contributor_lane": map[string]any{"soak_minutes": float64(10)}}, true},
		"soak 0 is the default 30":   {map[string]any{"contributor_lane": map[string]any{"soak_minutes": float64(0)}}, false},
		"exclude label added":        {map[string]any{"trusted_authors": map[string]any{"exclude_labels": map[string]any{"add": []any{"wip"}}}}, false},
		"self-authored switched off": {map[string]any{"self_authored": false}, false},
	} {
		t.Run(name, func(t *testing.T) {
			preview, _ := autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: autoMergeFixture()}), tc.args)
			if got := strings.HasPrefix(preview.WideningDisclosure, "Widening:"); got != tc.widen {
				t.Fatalf("widening = %v, want %v: %s", got, tc.widen, preview.WideningDisclosure)
			}
		})
	}
}

// A preview is built from the settings it read. If a setting it would send
// changes before write_confirm, sending the preview's merged value would undo
// that change, so the confirm is refused and nothing is written. A change to
// a setting the preview does not send does not block it.
func TestAutoMergeUpdateRefusesStalePreview(t *testing.T) {
	p := &autoMergeProvider{settings: autoMergeFixture()}
	h := autoMergeHandler(p)
	args := map[string]any{"squash_only": map[string]any{"add": []any{"org/c"}}}

	_, id := autoMergePreview(t, h, args)
	p.settings["squash_only"] = []string{"org/a"} // someone removed org/b meanwhile
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote: %+v", p.requests)
	}

	preview, id := autoMergePreview(t, h, args)
	if got := compactJSON(preview.Request.Body); got != `{"squash_only":["org/a","org/c"]}` {
		t.Fatalf("fresh preview body = %s", got)
	}
	p.settings["max_merges"] = 5 // not sent, so not stale
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm after an unrelated change: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != `{"squash_only":["org/a","org/c"]}` {
		t.Fatalf("executed %+v", p.requests)
	}
}

// Merge settings are owner only. A caller the hive does not verify as owner
// cannot read them, so the preview is refused with no confirmation minted;
// and a confirmation whose re-read is refused writes nothing.
func TestAutoMergeUpdateRefusesNonOwner(t *testing.T) {
	ownerOnly := fmt.Errorf("%w: Hive API returned HTTP 403: owner access required", ErrForbidden)
	p := &autoMergeProvider{settings: autoMergeFixture(), readErr: ownerOnly}
	h := autoMergeHandler(p)
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAutoMergeUpdate, "args": map[string]any{"self_authored": false}})
	if err == nil || !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "verified owner") {
		t.Fatalf("non-owner preview err = %v", err)
	}
	if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
		t.Fatalf("a non-owner preview minted a confirmation: %+v", pending)
	}

	p.readErr = nil
	_, id := autoMergePreview(t, h, map[string]any{"self_authored": false})
	p.readErr = ownerOnly
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err == nil {
		t.Fatal("a confirm whose re-read was refused went through")
	}
	if len(p.requests) != 0 {
		t.Fatalf("wrote without owner access: %+v", p.requests)
	}
}
