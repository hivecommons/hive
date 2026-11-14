package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#18136: the owner-only project.reference_repos editor.

const referenceReposPath = "/api/config/reference-repos"

func decodeReferenceRepos(t *testing.T, body []byte) referenceReposSectionResponse {
	t.Helper()
	var got referenceReposSectionResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return got
}

func TestReferenceReposGet_OwnerOnlyAndEmptyArray(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, referenceReposPath); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: expected 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, referenceReposPath, `{"reference_repos":[]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT: expected 403, got %d", rec.Code)
	}
	rec := doOwnerGet(s, referenceReposPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"reference_repos":[]`) {
		t.Fatalf("default body must carry an empty array, not null: %s", rec.Body.String())
	}
	got := decodeReferenceRepos(t, rec.Body.Bytes())
	if !reflect.DeepEqual(got.WriteKinds, []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}) {
		t.Fatalf("write_kinds = %v", got.WriteKinds)
	}
}

func TestReferenceReposPut_RoundTripAndApplyHook(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	var applied []config.ReferenceRepo
	s.deps.ApplyReferenceRepos = func(_ context.Context, refs []config.ReferenceRepo) error {
		applied = refs
		return nil
	}

	rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": []map[string]any{
		{"repo": " hivecommons/docs "},
		{"repo": "hivecommons/hive", "write": []string{"PRs", "issues", "prs"}},
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	want := []config.ReferenceRepo{
		{Repo: "hivecommons/docs"},
		{Repo: "hivecommons/hive", Write: []string{"issues", "prs"}},
	}
	if got := decodeReferenceRepos(t, rec.Body.Bytes()).ReferenceRepos; !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT echoed %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(cfg.Project.ReferenceRepos, want) {
		t.Fatalf("config holds %+v, want %+v", cfg.Project.ReferenceRepos, want)
	}
	if !reflect.DeepEqual(applied, want) {
		t.Fatalf("the proxy hook saw %+v, want %+v", applied, want)
	}
	// The kick and status read the live config: in force immediately.
	if rr, ok := cfg.ReferenceRepoFor("HiveCommons/Hive"); !ok || !rr.WritesPRs() || !rr.WritesIssues() {
		t.Fatalf("saved entry not visible to ReferenceRepoFor: %+v %v", rr, ok)
	}
	if back := decodeReferenceRepos(t, doOwnerGet(s, referenceReposPath).Body.Bytes()); !reflect.DeepEqual(back.ReferenceRepos, want) {
		t.Fatalf("GET after PUT = %+v, want %+v", back.ReferenceRepos, want)
	}

	// Clearing is explicit and reaches the proxy as an empty list.
	if rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": []any{}}); rec.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(cfg.Project.ReferenceRepos) != 0 || applied == nil || len(applied) != 0 {
		t.Fatalf("clear left config=%+v applied=%+v", cfg.Project.ReferenceRepos, applied)
	}
}

func TestReferenceReposPut_RejectsWithLoaderMessage(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	// a/b carries an operator-set token file so the "token without write"
	// case reaches the loader's message rather than the dashboard's
	// operator-only refusal (tested separately).
	prior := []config.ReferenceRepo{{Repo: "keep/me"}, {Repo: "a/b", Write: []string{config.ReferenceWriteIssues}, WriteTokenFile: "/t"}}
	cfg.Project.ReferenceRepos = append([]config.ReferenceRepo{}, prior...)

	cases := []struct {
		name  string
		entry config.ReferenceRepo
	}{
		{"also authorized", config.ReferenceRepo{Repo: cfg.Project.Org + "/" + cfg.Project.Repos[0]}},
		{"bare name", config.ReferenceRepo{Repo: "docs"}},
		{"unknown write", config.ReferenceRepo{Repo: "a/b", Write: []string{"merge"}}},
		{"token without write", config.ReferenceRepo{Repo: "a/b", WriteTokenFile: "/t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := cfg.Project
			candidate.ReferenceRepos = []config.ReferenceRepo{tc.entry}
			loaderErr := config.ValidateReferenceRepos(candidate)
			if loaderErr == nil {
				t.Fatalf("the loader accepts %+v; the case is wrong", tc.entry)
			}
			rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": []config.ReferenceRepo{tc.entry}})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), jsonEscaped(t, loaderErr.Error())) {
				t.Fatalf("response %s does not carry the loader's message %q", rec.Body.String(), loaderErr)
			}
			if !reflect.DeepEqual(cfg.Project.ReferenceRepos, prior) {
				t.Fatalf("a refused PUT changed the config: %+v", cfg.Project.ReferenceRepos)
			}
		})
	}

	if rec := doPut(s, referenceReposPath, map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing key: expected 400, got %d", rec.Code)
	}
}

// A write-mode entry with no credential is refused with the message boot
// would exit 22 with, and nothing is stored, so the next boot still starts.
func TestReferenceReposPut_WriteWithoutCredentialRefused(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	// The production hook (cmd/hive applyReferenceRepos) runs the same
	// verification boot runs; a hive with no App auth has no discover seam.
	var bootMsg string
	s.deps.ApplyReferenceRepos = func(ctx context.Context, refs []config.ReferenceRepo) error {
		for _, rr := range refs {
			if _, err := github.VerifyReferenceRepoWriteCredential(ctx, rr, "", nil, nil); err != nil {
				bootMsg = err.Error()
				return err
			}
		}
		return nil
	}

	rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": []map[string]any{
		{"repo": "upstream/project", "write": []string{"issues"}},
	}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if bootMsg == "" || !strings.Contains(rec.Body.String(), jsonEscaped(t, bootMsg)) {
		t.Fatalf("response %s does not carry the boot message %q", rec.Body.String(), bootMsg)
	}
	if !strings.Contains(bootMsg, github.ErrReferenceRepoNoCredential.Error()) {
		t.Fatalf("boot message %q does not name the missing credential", bootMsg)
	}
	if len(cfg.Project.ReferenceRepos) != 0 {
		t.Fatalf("a refused PUT stored %+v", cfg.Project.ReferenceRepos)
	}
}

// jsonEscaped returns s as it appears inside a JSON string.
func jsonEscaped(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Trim(string(b), `"`)
}

// write_token_file is operator-only (hivecommons/hive#18154 review): the
// path names a file on the hive host that the credential check reads and
// sends to GitHub, so the dashboard may keep a path the config file already
// holds for the same repo but never add or change one.
func TestReferenceReposPut_TokenFileIsOperatorOnly(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	prior := []config.ReferenceRepo{{Repo: "a/b", Write: []string{config.ReferenceWriteIssues}, WriteTokenFile: "/etc/hive/ab.token"}}
	cfg.Project.ReferenceRepos = append([]config.ReferenceRepo{}, prior...)

	refused := []struct {
		name string
		body []map[string]any
	}{
		{"new repo with a token file", []map[string]any{
			{"repo": "a/b", "write": []string{"issues"}, "write_token_file": "/etc/hive/ab.token"},
			{"repo": "c/d", "write": []string{"prs"}, "write_token_file": "/etc/passwd"},
		}},
		{"changed path on a file-set repo", []map[string]any{
			{"repo": "a/b", "write": []string{"issues"}, "write_token_file": "/etc/hive/other.token"},
		}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": tc.body})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "write_token_file") || !strings.Contains(rec.Body.String(), "config file") {
				t.Fatalf("refusal must name write_token_file and the config file: %s", rec.Body.String())
			}
			if !reflect.DeepEqual(cfg.Project.ReferenceRepos, prior) {
				t.Fatalf("a refused PUT changed the config: %+v", cfg.Project.ReferenceRepos)
			}
		})
	}

	// The operator's path round-trips unchanged, next to a new App-backed entry.
	rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": []map[string]any{
		{"repo": "a/b", "write": []string{"issues"}, "write_token_file": "/etc/hive/ab.token"},
		{"repo": "c/d", "write": []string{"prs"}},
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	want := []config.ReferenceRepo{
		{Repo: "a/b", Write: []string{config.ReferenceWriteIssues}, WriteTokenFile: "/etc/hive/ab.token"},
		{Repo: "c/d", Write: []string{config.ReferenceWritePRs}},
	}
	if !reflect.DeepEqual(cfg.Project.ReferenceRepos, want) {
		t.Fatalf("config holds %+v, want %+v", cfg.Project.ReferenceRepos, want)
	}
}

// A derived entry (hivecommons/hive#18139) echoed back by the editor is never
// stored: the hook and the file see only the hand-written list, and the
// derived set is merged back into memory after the save so /api/status and
// the kick keep it.
func TestReferenceReposPut_DerivedEntriesStrippedAndRemerged(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	cfg.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	derived := config.ReferenceRepo{Repo: "acme/docs", Write: []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}, DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialWriteTier}
	cfg.Project.ReferenceRepos = []config.ReferenceRepo{{Repo: "keep/me"}, derived}

	var applied []config.ReferenceRepo
	s.deps.ApplyReferenceRepos = func(_ context.Context, refs []config.ReferenceRepo) error {
		applied = refs
		return nil
	}
	remerged := 0
	s.deps.RemergeDerivedReferenceRepos = func() {
		remerged++
		// What cmd/hive's remergeLastDerivedReferenceRepos does: the last
		// derived set goes back under whatever hand-written list is in cfg.
		cfg.Project.ReferenceRepos = append(config.HandWrittenReferenceRepos(cfg.Project.ReferenceRepos), derived)
	}

	// The editor sends the list as GET returned it, plus one new entry.
	got := decodeReferenceRepos(t, doOwnerGet(s, referenceReposPath).Body.Bytes()).ReferenceRepos
	if len(got) != 2 || got[1].DerivedFrom != derived.DerivedFrom {
		t.Fatalf("GET must show the derived entry with its provenance: %+v", got)
	}
	rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": append(got, config.ReferenceRepo{Repo: "new/one"})})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	handWritten := []config.ReferenceRepo{{Repo: "keep/me"}, {Repo: "new/one"}}
	if !reflect.DeepEqual(applied, handWritten) {
		t.Fatalf("the proxy hook must see only hand-written entries, got %+v", applied)
	}
	if remerged != 1 {
		t.Fatalf("remerge hook called %d times, want 1", remerged)
	}
	if !reflect.DeepEqual(cfg.Project.ReferenceRepos, append(handWritten, derived)) {
		t.Fatalf("memory must hold the hand-written list with the derived entry merged back, got %+v", cfg.Project.ReferenceRepos)
	}
	if back := decodeReferenceRepos(t, rec.Body.Bytes()).ReferenceRepos; len(back) != 3 || back[2].DerivedFrom != derived.DerivedFrom {
		t.Fatalf("PUT must echo the merged list, got %+v", back)
	}
	data, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{derived.Repo, derived.DerivedFrom, "derived_from", "credential_state"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("saved config leaks derived state %q:\n%s", leak, data)
		}
	}
	if !strings.Contains(string(data), "new/one") || !strings.Contains(string(data), "keep/me") {
		t.Fatalf("saved config must hold the hand-written entries:\n%s", data)
	}
}

// A derived entry (hivecommons/hive#18161) is the request issue's grant: the
// editor may echo it back as served but any edit, removal, replacement by a
// hand-written entry or unknown derived entry is a 409 naming the issue, and
// nothing changes. Hand-written entries stay editable next to it.
func TestReferenceReposPut_DerivedEntriesConflict(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	derived := config.ReferenceRepo{Repo: "acme/docs", Write: []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}, DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialWriteTier}
	live := []config.ReferenceRepo{{Repo: "keep/me"}, derived}
	cfg.Project.ReferenceRepos = append([]config.ReferenceRepo{}, live...)
	applied := 0
	s.deps.ApplyReferenceRepos = func(context.Context, []config.ReferenceRepo) error { applied++; return nil }

	echo := map[string]any{"repo": derived.Repo, "write": derived.Write, "derived_from": derived.DerivedFrom}
	cases := []struct {
		name string
		body []map[string]any
	}{
		{"removed", []map[string]any{{"repo": "keep/me"}}},
		{"write set edited", []map[string]any{{"repo": "keep/me"}, {"repo": derived.Repo, "write": []string{"issues"}, "derived_from": derived.DerivedFrom}}},
		{"source changed", []map[string]any{{"repo": "keep/me"}, {"repo": derived.Repo, "write": derived.Write, "derived_from": "cncf/techdocs#1"}}},
		{"replaced by a hand-written entry", []map[string]any{{"repo": "keep/me"}, {"repo": "ACME/docs", "write": []string{"issues"}}}},
		{"unknown derived entry", []map[string]any{{"repo": "keep/me"}, echo, {"repo": "zeta/site", "derived_from": "cncf/techdocs#396"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": tc.body})
			if rec.Code != http.StatusConflict {
				t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "cncf/techdocs#") {
				t.Fatalf("409 must name the request issue: %s", rec.Body.String())
			}
			if !reflect.DeepEqual(cfg.Project.ReferenceRepos, live) || applied != 0 {
				t.Fatalf("a refused PUT changed state: %+v applied=%d", cfg.Project.ReferenceRepos, applied)
			}
		})
	}

	// Hand-written edits around the echoed derived entry still save.
	rec := doPut(s, referenceReposPath, map[string]any{"reference_repos": []map[string]any{{"repo": "keep/me", "write": []string{"issues"}}, echo, {"repo": "new/one"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if applied != 1 {
		t.Fatalf("proxy hook calls = %d, want 1", applied)
	}
	want := []config.ReferenceRepo{{Repo: "keep/me", Write: []string{config.ReferenceWriteIssues}}, {Repo: "new/one"}}
	if !reflect.DeepEqual(cfg.Project.ReferenceRepos, want) {
		t.Fatalf("config holds %+v, want %+v (no remerge hook in this test)", cfg.Project.ReferenceRepos, want)
	}
}
