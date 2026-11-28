package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// repoAutoMergeProvider answers settings_read section auto_merge with the
// repo_auto_merge block GET /api/config/auto-merge reports, JSON-decoded,
// and records writes. A write saves the switch the way
// POST /api/repos/auto-merge does: it records the repository's override.
// readErr stands in for the section refusing the caller (it is owner only);
// writeErr for the endpoint refusing the write.
type repoAutoMergeProvider struct {
	level    int
	order    []string
	values   map[string]*bool
	noBlock  bool
	readErr  error
	writeErr error
	requests []WriteRequest
}

func newRepoAutoMergeProvider() *repoAutoMergeProvider {
	off := false
	return &repoAutoMergeProvider{level: 6, order: []string{"org/a", "org/b"}, values: map[string]*bool{"org/b": &off}}
}

// enabled resolves a repository's switch as config.RepoAutoMergeEnabled
// does for a valid merge_strategy.
func (p *repoAutoMergeProvider) enabled(repo string) bool {
	if p.level < 6 {
		return false
	}
	v := p.values[repo]
	return v == nil || *v
}

func (p *repoAutoMergeProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != "auto_merge" {
		return map[string]any{}, nil
	}
	if p.readErr != nil {
		return nil, p.readErr
	}
	section := map[string]any{"max_merges": 3, "project_repos": p.order}
	if !p.noBlock {
		repos := make([]map[string]any, 0, len(p.order))
		for _, repo := range p.order {
			entry := map[string]any{"repo": repo, "enabled": p.enabled(repo)}
			if v := p.values[repo]; v != nil {
				entry["value"] = *v
			}
			repos = append(repos, entry)
		}
		section["repo_auto_merge"] = map[string]any{"acmm_level": p.level, "min_acmm_level": 6, "repos": repos}
	}
	b, err := json.Marshal(section)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *repoAutoMergeProvider) set(repo string, on bool) { p.values[repo] = &on }

func (p *repoAutoMergeProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	if p.writeErr != nil {
		return nil, p.writeErr
	}
	p.requests = append(p.requests, req)
	body := req.Body.(map[string]any)
	repo, _ := body["repo"].(string)
	on, _ := body["enabled"].(bool)
	p.set(repo, on)
	return map[string]any{"ok": true, "repo": repo, "enabled": on, "changed": true, "persisted": true, "minStatusSeq": 7}, nil
}

func repoAutoMergeHandler(p *repoAutoMergeProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func repoAutoMergePreview(t *testing.T, h *Handler, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpRepoAutoMerge, "args": args})
	if err != nil {
		t.Fatalf("preview %v: %v", args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

func repoAutoMergePreviewErr(t *testing.T, h *Handler, args map[string]any) error {
	t.Helper()
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpRepoAutoMerge, "args": args})
	if err == nil {
		t.Fatalf("preview %v was offered for confirm", args)
	}
	return err
}

func TestRepoAutoMergeRegistered(t *testing.T) {
	op, ok := DefaultWriteRegistry().Get(WriteOpRepoAutoMerge)
	if !ok {
		t.Fatalf("%s is not registered", WriteOpRepoAutoMerge)
	}
	schema := op.InputSchema()
	props, _ := schema["properties"].(map[string]any)
	if len(props) != 2 || props["repo"] == nil || props["enabled"] == nil || schema["additionalProperties"] != false {
		t.Fatalf("schema = %#v", schema)
	}
	if req, _ := schema["required"].([]string); strings.Join(req, ",") != "repo,enabled" {
		t.Fatalf("required = %#v", schema["required"])
	}
	if d := op.Description(); !strings.Contains(d, "POST /api/repos/auto-merge") || !strings.Contains(d, "Owner only") {
		t.Fatalf("description does not name the endpoint and the owner-only rule: %s", d)
	}
}

// Switching a repository on shows the switch before and after, sends what
// the repository card sends, and names the change as widening. A bare
// repository name resolves to the project repository.
func TestRepoAutoMergePreviewEnableIsWidening(t *testing.T) {
	p := newRepoAutoMergeProvider()
	preview, _ := repoAutoMergePreview(t, repoAutoMergeHandler(p), map[string]any{"repo": "B", "enabled": true})
	if preview.Request.Method != http.MethodPost || preview.Request.Path != "/api/repos/auto-merge" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	if got, want := compactJSON(preview.Request.Body), `{"enabled":true,"repo":"org/b"}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if preview.Target != "org/b" || preview.Details["before"] != false || preview.Details["after"] != true {
		t.Fatalf("target %q, details %#v", preview.Target, preview.Details)
	}
	if effects := strings.Join(preview.Effects, "\n"); !strings.Contains(effects, "auto-merge for org/b: off → on") {
		t.Fatalf("effects must show the switch before and after: %v", preview.Effects)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "Widening:") || !strings.Contains(preview.WideningDisclosure, "the hive may merge PRs in org/b once its own gates and GitHub's rules pass") {
		t.Fatalf("enabling must be disclosed as widening: %s", preview.WideningDisclosure)
	}
	if preview.StateFingerprint == "" {
		t.Fatal("the preview carries no state fingerprint for the stale check")
	}
	if len(p.requests) != 0 {
		t.Fatalf("a preview wrote: %+v", p.requests)
	}
}

// Switching off a repository on the level 6 default is narrowing.
func TestRepoAutoMergePreviewDisableIsNarrowing(t *testing.T) {
	p := newRepoAutoMergeProvider()
	preview, _ := repoAutoMergePreview(t, repoAutoMergeHandler(p), map[string]any{"repo": "org/a", "enabled": false})
	if got, want := compactJSON(preview.Request.Body), `{"enabled":false,"repo":"org/a"}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if effects := strings.Join(preview.Effects, "\n"); !strings.Contains(effects, "auto-merge for org/a: on (not set, the level 6 default) → off") {
		t.Fatalf("effects must show the switch before and after: %v", preview.Effects)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "Narrowing:") {
		t.Fatalf("disabling must be disclosed as narrowing: %s", preview.WideningDisclosure)
	}
}

// A switch already as requested is refused before a confirmation is issued.
func TestRepoAutoMergeNothingWouldChange(t *testing.T) {
	p := newRepoAutoMergeProvider()
	h := repoAutoMergeHandler(p)
	for _, args := range []map[string]any{{"repo": "org/a", "enabled": true}, {"repo": "org/b", "enabled": false}} {
		if err := repoAutoMergePreviewErr(t, h, args); !strings.Contains(err.Error(), "nothing would change") {
			t.Fatalf("preview %v err = %v, want nothing would change", args, err)
		}
	}
}

// write_confirm sends the previewed request and reads the switch back.
func TestRepoAutoMergeConfirmWritesAndReadsBack(t *testing.T) {
	p := newRepoAutoMergeProvider()
	h := repoAutoMergeHandler(p)
	_, id := repoAutoMergePreview(t, h, map[string]any{"repo": "org/b", "enabled": true})
	out, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != `{"enabled":true,"repo":"org/b"}` {
		t.Fatalf("executed %+v", p.requests)
	}
	res := out.(map[string]any)
	if res["type"] != "write_result" || res["after_error"] != nil {
		t.Fatalf("result = %#v", res)
	}
	if got, want := compactJSON(res["after"]), `{"enabled":true,"repo":"org/b","value":true}`; got != want {
		t.Fatalf("after = %s, want %s", got, want)
	}
	if !p.enabled("org/b") || !p.enabled("org/a") {
		t.Fatal("the confirm left a switch other than requested")
	}
}

// A dashboard toggle between preview and confirm makes the preview's before
// untrue, so the confirm is refused and nothing is written. A toggle that
// already did what was asked is refused as nothing would change.
func TestRepoAutoMergeRefusesConfirmAfterDashboardToggle(t *testing.T) {
	p := newRepoAutoMergeProvider()
	h := repoAutoMergeHandler(p)
	_, id := repoAutoMergePreview(t, h, map[string]any{"repo": "org/a", "enabled": false})
	p.set("org/a", false) // the repository card switched it off ...
	p.set("org/a", true)  // ... and on again, recording an override
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}

	_, id = repoAutoMergePreview(t, h, map[string]any{"repo": "org/b", "enabled": true})
	p.set("org/b", true) // the repository card switched it on first
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err == nil || !strings.Contains(err.Error(), "nothing would change") {
		t.Fatalf("confirm err = %v, want the stale check to say nothing would change", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a refused confirmation wrote: %+v", p.requests)
	}
}

// A repository the hive does not manage is refused at the preview.
func TestRepoAutoMergeRefusesUnknownRepo(t *testing.T) {
	h := repoAutoMergeHandler(newRepoAutoMergeProvider())
	for _, repo := range []string{"org/zzz", "other/a", "zzz"} {
		err := repoAutoMergePreviewErr(t, h, map[string]any{"repo": repo, "enabled": false})
		if !strings.Contains(err.Error(), "is not a repository in project.repos") {
			t.Fatalf("preview of %s err = %v", repo, err)
		}
	}
}

// Below level 6 auto-merge is off for every repository: switching on is
// refused as the endpoint would refuse it, and switching off changes nothing.
func TestRepoAutoMergeBelowLevelSix(t *testing.T) {
	p := newRepoAutoMergeProvider()
	p.level = 5
	h := repoAutoMergeHandler(p)
	if err := repoAutoMergePreviewErr(t, h, map[string]any{"repo": "org/a", "enabled": true}); !strings.Contains(err.Error(), "requires autonomy level 6") || !strings.Contains(err.Error(), "level 5") {
		t.Fatalf("enable below level 6 err = %v", err)
	}
	if err := repoAutoMergePreviewErr(t, h, map[string]any{"repo": "org/a", "enabled": false}); !strings.Contains(err.Error(), "nothing would change") {
		t.Fatalf("disable below level 6 err = %v", err)
	}
}

// The merge settings read is owner only, so a caller who is not the
// verified owner cannot preview; a hive that does not report the switch is
// refused rather than guessed at.
func TestRepoAutoMergeRefusesUnreadableSwitch(t *testing.T) {
	p := newRepoAutoMergeProvider()
	p.readErr = errors.New("Hive API returned HTTP 403: owner access required")
	err := repoAutoMergePreviewErr(t, repoAutoMergeHandler(p), map[string]any{"repo": "org/a", "enabled": false})
	if !strings.Contains(err.Error(), "verified owner") || !strings.Contains(err.Error(), "owner access required") {
		t.Fatalf("non-owner preview err = %v", err)
	}

	p = newRepoAutoMergeProvider()
	p.noBlock = true
	if err := repoAutoMergePreviewErr(t, repoAutoMergeHandler(p), map[string]any{"repo": "org/a", "enabled": false}); !strings.Contains(err.Error(), "older build") {
		t.Fatalf("older hive preview err = %v", err)
	}
}

func TestRepoAutoMergeRefusesBadArguments(t *testing.T) {
	h := repoAutoMergeHandler(newRepoAutoMergeProvider())
	for _, args := range []map[string]any{
		{"repo": "org/a"},
		{"enabled": true},
		{"repo": "org/a", "enabled": "yes"},
		{"repo": "org/a", "enabled": true, "reason": "x"},
	} {
		repoAutoMergePreviewErr(t, h, args)
	}
}

// The endpoint's refusal reaches the caller as a hive refusal.
func TestRepoAutoMergePassesHandlerRefusalThrough(t *testing.T) {
	p := newRepoAutoMergeProvider()
	h := repoAutoMergeHandler(p)
	_, id := repoAutoMergePreview(t, h, map[string]any{"repo": "org/b", "enabled": true})
	body := []byte(`{"error":"owner access required","ok":false}`)
	p.writeErr = &HiveRefusalError{StatusCode: http.StatusForbidden, Message: string(body), Body: body}
	_, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if refusal, ok := HiveRefusalFromError(err); !ok || refusal.StatusCode != http.StatusForbidden {
		t.Fatalf("confirm err = %v, want the endpoint's 403 as a hive refusal", err)
	}
}
