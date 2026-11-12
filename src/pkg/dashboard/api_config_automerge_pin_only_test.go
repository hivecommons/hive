package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Tests for auto_merge.human_merge_pin_only on PUT/PATCH
// /api/config/auto-merge: the pin-only exception to human_merge_paths
// (#16377) set from the dashboard instead of hive.yaml.

// autoMergePinOnlyServer is repoPolicyServer with human-merge paths on acme/api, the
// repository the exception is set for.
func autoMergePinOnlyServer(t *testing.T) *Server {
	t.Helper()
	s := repoPolicyServer(t)
	s.deps.Config.AutoMerge.HumanMergePaths = map[string][]string{"acme/api": {".github/workflows/**", "Containerfile"}}
	return s
}

func autoMergePinOnlyEntryBody() map[string]any {
	return map[string]any{
		"enabled":         true,
		"paths":           []string{" .github/workflows/*.yml ", "Containerfile", "Containerfile", ""},
		"kinds":           []string{"action-sha", "image-digest"},
		"trusted_authors": []string{"renovate[bot]"},
	}
}

func TestAutoMergePinOnly_PutGetRoundTrip(t *testing.T) {
	s := autoMergePinOnlyServer(t)
	rec := doPut(s, "/api/config/auto-merge", map[string]any{
		"human_merge_pin_only": map[string]any{"api": autoMergePinOnlyEntryBody()},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// The key is stored as owner/repo, and each list trimmed and de-duplicated.
	policy, ok := s.deps.Config.AutoMerge.HumanMergePinOnlyFor("acme/api")
	if !ok {
		t.Fatalf("acme/api has no enabled exception after the put: %+v", s.deps.Config.AutoMerge.HumanMergePinOnly)
	}
	if strings.Join(policy.Paths, ",") != ".github/workflows/*.yml,Containerfile" ||
		strings.Join(policy.Kinds, ",") != "action-sha,image-digest" ||
		strings.Join(policy.TrustedAuthors, ",") != "renovate[bot]" {
		t.Fatalf("stored policy = %+v", policy)
	}
	if _, ok := s.deps.Config.AutoMerge.HumanMergePinOnly["acme/api"]; !ok {
		t.Fatalf("key not stored as owner/repo: %+v", s.deps.Config.AutoMerge.HumanMergePinOnly)
	}

	rec = doOwnerGet(s, "/api/config/auto-merge")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after put: expected 200, got %d", rec.Code)
	}
	var got struct {
		PinOnly map[string]config.PinOnlyException `json:"human_merge_pin_only"`
		Kinds   []string                           `json:"pin_only_kinds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	entry, ok := got.PinOnly["acme/api"]
	if !ok || !entry.Enabled || strings.Join(entry.Paths, ",") != ".github/workflows/*.yml,Containerfile" || strings.Join(entry.TrustedAuthors, ",") != "renovate[bot]" {
		t.Fatalf("human_merge_pin_only did not round-trip: %+v", got.PinOnly)
	}
	if strings.Join(got.Kinds, ",") != "action-sha,image-digest,version-pin,action-sha-ref" {
		t.Fatalf("pin_only_kinds = %v", got.Kinds)
	}
	// Offering a kind never turns it on: the stored exception still holds
	// only the kinds that were sent.
	if strings.Join(entry.Kinds, ",") != "action-sha,image-digest" {
		t.Fatalf("stored kinds = %v, want only the kinds that were sent", entry.Kinds)
	}

	// Absent leaves it alone; an empty map clears it, like human_merge_paths.
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"max_merges": 2}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated put: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.deps.Config.AutoMerge.HumanMergePinOnlyFor("acme/api"); !ok {
		t.Fatal("a put without human_merge_pin_only changed it")
	}
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"human_merge_pin_only": map[string]any{}}); rec.Code != http.StatusOK {
		t.Fatalf("clear put: %d %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.AutoMerge.HumanMergePinOnly != nil {
		t.Fatalf("an empty map did not clear the exception: %+v", s.deps.Config.AutoMerge.HumanMergePinOnly)
	}
}

// action-sha-ref is a kind the endpoint takes like any other: stored when it
// is sent, and handed to the classifier as part of the exception's spec.
func TestAutoMergePinOnly_ActionSHARefKindRoundTrips(t *testing.T) {
	s := autoMergePinOnlyServer(t)
	body := autoMergePinOnlyEntryBody()
	body["kinds"] = []string{"action-sha", "action-sha-ref"}
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"human_merge_pin_only": map[string]any{"api": body}}); rec.Code != http.StatusOK {
		t.Fatalf("put with action-sha-ref: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	policy, ok := s.deps.Config.AutoMerge.HumanMergePinOnlyFor("acme/api")
	if !ok || strings.Join(policy.Kinds, ",") != "action-sha,action-sha-ref" || strings.Join(policy.Spec().Kinds, ",") != "action-sha,action-sha-ref" {
		t.Fatalf("stored policy = %+v, enabled %v", policy, ok)
	}
	rec := doOwnerGet(s, "/api/config/auto-merge")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after put: expected 200, got %d", rec.Code)
	}
	var got struct {
		PinOnly map[string]config.PinOnlyException `json:"human_merge_pin_only"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if kinds := got.PinOnly["acme/api"].Kinds; strings.Join(kinds, ",") != "action-sha,action-sha-ref" {
		t.Fatalf("GET kinds = %v", kinds)
	}
}

func TestAutoMergePinOnly_InvalidIsRefusedWithNoChange(t *testing.T) {
	s := autoMergePinOnlyServer(t)
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"human_merge_pin_only": map[string]any{"api": autoMergePinOnlyEntryBody()}}); rec.Code != http.StatusOK {
		t.Fatalf("seed put: %d %s", rec.Code, rec.Body.String())
	}
	with := func(key string, value any) map[string]any {
		e := autoMergePinOnlyEntryBody()
		e[key] = value
		return e
	}
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"unknown repo", map[string]any{"human_merge_pin_only": map[string]any{"other/repo": autoMergePinOnlyEntryBody()}}, "human_merge_pin_only: other/repo is not in project.repos"},
		{"twice", map[string]any{"human_merge_pin_only": map[string]any{"api": autoMergePinOnlyEntryBody(), "ACME/api": autoMergePinOnlyEntryBody()}}, "names acme/api twice"},
		{"no trusted authors", map[string]any{"human_merge_pin_only": map[string]any{"api": with("trusted_authors", []string{" "})}}, "enabled policy requires nonempty trusted_authors"},
		{"no paths", map[string]any{"human_merge_pin_only": map[string]any{"api": with("paths", []string{})}}, "enabled policy requires nonempty paths"},
		{"unknown kind", map[string]any{"human_merge_pin_only": map[string]any{"api": with("kinds", []string{"any-line"})}}, "unknown kind"},
		{"unknown kind while disabled", map[string]any{"human_merge_pin_only": map[string]any{"api": map[string]any{"enabled": false, "kinds": []string{"any-line"}}}}, "unknown kind"},
		// The exception only lifts a human-merge-path hold, so on a repository
		// with no such paths it would never apply.
		{"no human-merge paths", map[string]any{"human_merge_pin_only": map[string]any{"api": autoMergePinOnlyEntryBody(), "web": autoMergePinOnlyEntryBody()}}, "acme/web has no human_merge_paths"},
		{"paths cleared in the same write", map[string]any{"human_merge_paths": map[string][]string{}, "human_merge_pin_only": map[string]any{"api": with("kinds", []string{"version-pin"})}}, "acme/api has no human_merge_paths"},
		// A valid key alongside an invalid one must not half-apply.
		{"mixed", map[string]any{"max_merges": 7, "squash_only": []string{"api"}, "human_merge_pin_only": map[string]any{"api": with("kinds", []string{"nope"})}}, "unknown kind"},
	} {
		rec := doPut(s, "/api/config/auto-merge", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: expected 400 containing %q, got %d: %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
	am := s.deps.Config.AutoMerge
	policy, ok := am.HumanMergePinOnlyFor("acme/api")
	if !ok || len(am.HumanMergePinOnly) != 1 || strings.Join(policy.Kinds, ",") != "action-sha,image-digest" {
		t.Fatalf("rejected writes changed human_merge_pin_only: %+v", am.HumanMergePinOnly)
	}
	if am.MaxMerges != 0 || am.SquashOnly != nil || len(am.HumanMergePathsFor("acme/api")) != 2 {
		t.Fatalf("rejected writes changed other settings: %+v", am)
	}
}

// An entry hive.yaml already holds is not re-checked when a write sends it
// back unchanged, so a full-map save from the dashboard still works on a hive
// whose file carries an exception for a repository without human-merge paths.
func TestAutoMergePinOnly_UnchangedEntryIsNotRechecked(t *testing.T) {
	s := autoMergePinOnlyServer(t)
	s.deps.Config.AutoMerge.HumanMergePinOnly = map[string]config.PinOnlyException{
		"acme/web": {Enabled: true, Paths: []string{"Containerfile"}, Kinds: []string{"image-digest"}, TrustedAuthors: []string{"renovate[bot]"}},
	}
	rec := doPut(s, "/api/config/auto-merge", map[string]any{"human_merge_pin_only": map[string]any{
		"acme/web": map[string]any{"enabled": true, "paths": []string{"Containerfile"}, "kinds": []string{"image-digest"}, "trusted_authors": []string{"renovate[bot]"}},
		"api":      autoMergePinOnlyEntryBody(),
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.deps.Config.AutoMerge.HumanMergePinOnly) != 2 {
		t.Fatalf("human_merge_pin_only = %+v", s.deps.Config.AutoMerge.HumanMergePinOnly)
	}
}

func TestAutoMergePinOnly_NonOwnerIsForbidden(t *testing.T) {
	s := autoMergePinOnlyServer(t)
	for _, tc := range []struct {
		name     string
		role     string
		verified bool
	}{
		{"merger", "merger", true},
		{"unverified owner", "owner", false},
		{"no role", "", false},
	} {
		body, _ := json.Marshal(map[string]any{"human_merge_pin_only": map[string]any{"api": autoMergePinOnlyEntryBody()}})
		for _, method := range []string{http.MethodPut, http.MethodPatch} {
			req := httptest.NewRequest(method, "/api/config/auto-merge", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tc.role != "" {
				req.Header.Set("X-Hive-Role", tc.role)
			}
			if tc.verified {
				req.Header.Set(ownerRoleVerifiedHeader, "true")
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s: expected 403, got %d: %s", tc.name, method, rec.Code, rec.Body.String())
			}
		}
	}
	if s.deps.Config.AutoMerge.HumanMergePinOnly != nil {
		t.Fatalf("a non-owner write changed human_merge_pin_only: %+v", s.deps.Config.AutoMerge.HumanMergePinOnly)
	}
}

// The sweep lanes and /approve read the exception through the shared config
// on every evaluation (cmd/hive humanMergePinOnlyFor), and the merge-request
// relay holds a copy the write must refresh. Both see a write at once, with
// no restart, and both lose it when it is cleared.
func TestAutoMergePinOnly_ReachesRunningHiveWithoutRestart(t *testing.T) {
	s := autoMergePinOnlyServer(t)
	client := ghpkg.NewClientForTest("http://127.0.0.1:1", "acme", []string{"api"}, covBLogger())
	s.deps.GHClient = client
	live := s.deps.Config // the pointer the sweeps were built with

	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"human_merge_pin_only": map[string]any{"api": autoMergePinOnlyEntryBody()}}); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if policy, ok := live.AutoMerge.HumanMergePinOnlyFor("acme/api"); !ok || strings.Join(policy.TrustedAuthors, ",") != "renovate[bot]" {
		t.Fatalf("the sweep's config does not hold the exception: %+v", live.AutoMerge.HumanMergePinOnly)
	}
	relay := client.HumanMergePinOnlyFor("acme/api")
	if !relay.Enabled || strings.Join(relay.Paths, ",") != ".github/workflows/*.yml,Containerfile" ||
		strings.Join(relay.Kinds, ",") != "action-sha,image-digest" || strings.Join(relay.TrustedAuthors, ",") != "renovate[bot]" {
		t.Fatalf("the merge-request relay did not pick up the exception: %+v", relay)
	}

	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"human_merge_pin_only": map[string]any{}}); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := live.AutoMerge.HumanMergePinOnlyFor("acme/api"); ok {
		t.Fatal("the sweep still sees a cleared exception")
	}
	if client.HumanMergePinOnlyFor("acme/api").Enabled {
		t.Fatal("the relay still holds a cleared exception")
	}
}

func TestAutoMergePinOnly_PersistsToSavedConfig(t *testing.T) {
	s := autoMergePinOnlyServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"human_merge_pin_only": map[string]any{"api": autoMergePinOnlyEntryBody()}}); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(s.deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	var saved struct {
		AutoMerge config.AutoMergeConfig `yaml:"auto_merge"`
	}
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	if policy, ok := saved.AutoMerge.HumanMergePinOnlyFor("acme/api"); !ok || strings.Join(policy.Kinds, ",") != "action-sha,image-digest" {
		t.Fatalf("saved auto_merge missing human_merge_pin_only: %+v", saved.AutoMerge.HumanMergePinOnly)
	}
}

// The Merge settings page edits the exception next to the human-merge paths
// and stages the whole map under human_merge_pin_only.
func TestAutoMergePinOnlySettingsStaticWiring(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static index: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"id=\"am-human-merge-pin-only\"",
		"class=\"am-pin-only-enabled\"",
		"class=\"am-pin-only-kind\"",
		"data-change-action=\"markDirtyAutoMergePinOnly\"",
		"function markDirtyAutoMergePinOnly()",
		"markDirty('auto-merge', 'human_merge_pin_only'",
		"am.human_merge_pin_only",
		"am.pin_only_kinds",
		// A kind's box is ticked only when the repository's own exception
		// lists it, so a kind the server starts offering (action-sha-ref)
		// is never on by default.
		"${p.kinds.includes(k) ? ' checked' : ''}",
		// The list the page falls back to when the server sends none is
		// the server's own, kind for kind.
		"am.pin_only_kinds : ['" + strings.Join(config.PinOnlyKinds, "', '") + "'];",
		"action-sha-ref adds nothing by itself",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static dashboard missing %q", want)
		}
	}
}
