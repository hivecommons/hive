package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// Signed-off-by through the hive's squash merges (hivecommons/hive#18203).

func TestSquashSignoffMessage(t *testing.T) {
	// The #18079 shape: one signed branch commit whose GitHub default squash
	// body came out as Co-authored-by lines only (f33758f5a).
	scanner := "[scanner] fix: de-flake two tests\n\n" +
		"Signed-off-by: scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>\n\n" +
		"Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
	t.Run("empty message is rebuilt from the branch and keeps the sign-off", func(t *testing.T) {
		got, err := squashSignoffMessage([]*gh.RepositoryCommit{commitWithMessage(scanner)}, "")
		if err != nil {
			t.Fatalf("squashSignoffMessage: %v", err)
		}
		want := "Signed-off-by: scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>\n\n" +
			"Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
		if got != want {
			t.Fatalf("message =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("a message without the sign-off gains it as a trailer", func(t *testing.T) {
		got, err := squashSignoffMessage([]*gh.RepositoryCommit{commitWithMessage(scanner)}, "Co-authored-by: dana <4+dana@users.noreply.github.com>")
		if err != nil {
			t.Fatalf("squashSignoffMessage: %v", err)
		}
		want := "Co-authored-by: dana <4+dana@users.noreply.github.com>\n\n" +
			"Signed-off-by: scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>"
		if got != want {
			t.Fatalf("message =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("every distinct branch sign-off is kept once", func(t *testing.T) {
		commits := []*gh.RepositoryCommit{
			commitWithMessage("One\n\nSigned-off-by: A <a@example.com>"),
			commitWithMessage("Two\n\nsigned-off-by: a <A@example.com>\nSigned-off-by: B <b@example.com>"),
		}
		got, err := squashSignoffMessage(commits, "Squash body")
		if err != nil {
			t.Fatalf("squashSignoffMessage: %v", err)
		}
		want := "Squash body\n\nSigned-off-by: A <a@example.com>\nSigned-off-by: B <b@example.com>"
		if got != want {
			t.Fatalf("message =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("merge-from-base commits do not count", func(t *testing.T) {
		mergeFromBase := &gh.RepositoryCommit{
			Commit:  &gh.Commit{Message: gh.Ptr("Merge v5\n\nSigned-off-by: M <m@example.com>")},
			Parents: []*gh.Commit{{SHA: gh.Ptr("p1")}, {SHA: gh.Ptr("p2")}},
		}
		_, err := squashSignoffMessage([]*gh.RepositoryCommit{commitWithMessage("Fix"), mergeFromBase}, "")
		if !errors.Is(err, ErrSquashUnsigned) {
			t.Fatalf("err = %v, want ErrSquashUnsigned", err)
		}
	})
	t.Run("no sign-off on the branch refuses the squash", func(t *testing.T) {
		_, err := squashSignoffMessage([]*gh.RepositoryCommit{commitWithMessage("Fix\n\nCo-authored-by: x <x@example.com>")}, "Co-authored-by: x <x@example.com>")
		if !errors.Is(err, ErrSquashUnsigned) {
			t.Fatalf("err = %v, want ErrSquashUnsigned", err)
		}
	})
}

func TestMergeCreditorSquashSignoff(t *testing.T) {
	ctx := context.Background()
	off := func(string) bool { return false }
	t.Run("applies with issue-author credit off", func(t *testing.T) {
		api := &mergeCreditAPI{commits: []string{"Fix\n\nSigned-off-by: K <k@example.com>"}}
		m, _ := newMergeCreditorForTest(t, api, off)
		got, err := m.SquashSignoff(ctx, "acme", "widget", 7, "", "", "squash", "")
		if err != nil || got != "Signed-off-by: K <k@example.com>" {
			t.Fatalf("SquashSignoff = %q, %v", got, err)
		}
	})
	t.Run("a signed message costs no request", func(t *testing.T) {
		api := &mergeCreditAPI{}
		m, _ := newMergeCreditorForTest(t, api, off)
		msg := "Body\n\nSigned-off-by: K <k@example.com>"
		if got, err := m.SquashSignoff(ctx, "acme", "widget", 7, "", "", "squash", msg); err != nil || got != msg || api.listGets.Load() != 0 {
			t.Fatalf("SquashSignoff = %q, %v after %d commit lists", got, err, api.listGets.Load())
		}
	})
	t.Run("merge and rebase are untouched", func(t *testing.T) {
		api := &mergeCreditAPI{}
		m, _ := newMergeCreditorForTest(t, api, off)
		for _, method := range []string{"merge", "rebase"} {
			if got, err := m.SquashSignoff(ctx, "acme", "widget", 7, "", "", method, ""); err != nil || got != "" {
				t.Fatalf("%s: SquashSignoff = %q, %v", method, got, err)
			}
		}
		if api.listGets.Load() != 0 {
			t.Fatalf("merge/rebase read the commits %d times", api.listGets.Load())
		}
	})
	t.Run("unsigned branch is refused where DCO is required", func(t *testing.T) {
		api := &mergeCreditAPI{commits: []string{"Fix"}}
		m, _ := newMergeCreditorForTest(t, api, off)
		m.dcoRequired = func(string) (bool, bool) { return true, true }
		if _, err := m.SquashSignoff(ctx, "acme", "widget", 7, "", "", "squash", ""); !errors.Is(err, ErrSquashUnsigned) {
			t.Fatalf("err = %v, want ErrSquashUnsigned", err)
		}
	})
	t.Run("nil creditor passes the message through", func(t *testing.T) {
		var m *MergeCreditor
		if got, err := m.SquashSignoff(ctx, "acme", "widget", 7, "", "", "squash", "x"); err != nil || got != "x" {
			t.Fatalf("nil: %q, %v", got, err)
		}
	})
}

// MergePRVia refuses to squash a branch with no sign-off in a repo whose
// head carries Prow's dco status, and sends no merge.
func TestMergePRViaRefusesUnsignedSquash(t *testing.T) {
	green := greenFixture()
	green.statuses = []ciStatus{{context: "dco", state: "failure"}}
	merges := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42":
			json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "state": "open", "title": "Fix the widget", "body": "",
				"user": map[string]any{"login": "karibhou"},
				"head": map[string]any{"sha": "abc123", "ref": "fix/widget"},
				"base": map[string]any{"ref": "v5"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42/commits":
			json.NewEncoder(w).Encode([]map[string]any{{"sha": "c0", "commit": map[string]any{"message": "Fix the widget"}, "parents": []map[string]any{{"sha": "p"}}}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/o/r/pulls/42/merge":
			merges++
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true}`)
		default:
			if green.serveCI(w, r) {
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	_, err := c.MergePRVia(context.Background(), "o/r", 42, "squash", "", PRAuditPathForge)
	if !errors.Is(err, ErrSquashUnsigned) || !strings.Contains(err.Error(), "o/r#42") {
		t.Fatalf("MergePRVia err = %v, want ErrSquashUnsigned for o/r#42", err)
	}
	if merges != 0 {
		t.Fatalf("merges = %d, want 0", merges)
	}
}

// MergePRVia squashes a branch with no sign-off in a repo with no DCO gate
// (no dco.required entry, no dco/DCO context required or reported) and sends
// the message as built, as before #18203 (projectbluefin/bluefin-report#91).
func TestMergePRViaMergesUnsignedSquashWithoutDCOGate(t *testing.T) {
	green := greenFixture()
	merges := 0
	var mergeBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42":
			json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "state": "open", "title": "chore(deps): update widget", "body": "",
				"user": map[string]any{"login": "renovate[bot]"},
				"head": map[string]any{"sha": "abc123", "ref": "renovate/widget"},
				"base": map[string]any{"ref": "main"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42/commits":
			json.NewEncoder(w).Encode([]map[string]any{{"sha": "c0", "commit": map[string]any{"message": "chore(deps): update widget"}, "parents": []map[string]any{{"sha": "p"}}}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/o/r/pulls/42/merge":
			merges++
			mergeBody = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&mergeBody)
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true}`)
		default:
			if green.serveCI(w, r) {
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetCreditIssueAuthors(func(string) bool { return false })
	res, err := c.MergePRVia(context.Background(), "o/r", 42, "squash", "", PRAuditPathForge)
	if err != nil || !res.Merged || merges != 1 {
		t.Fatalf("MergePRVia = %+v, %v after %d merges, want one merge", res, err, merges)
	}
	if msg, _ := mergeBody["commit_message"].(string); msg != "" {
		t.Fatalf("commit_message = %q, want GitHub's default (empty)", msg)
	}
}

// dcoGateAPI is a fake GitHub for the DCO gate behind the unsigned-squash
// refusal: the PR's commits, base main's required checks and head1's
// statuses and check runs. reads counts the gate's requests.
type dcoGateAPI struct {
	commits  []string
	required []string // nil: main is not protected and has no rulesets
	statuses []string
	checks   []string
	headHTTP int // non-zero: head1's status read fails with it
	reads    atomic.Int32
}

func (a *dcoGateAPI) creditor(t *testing.T, override func(string) (bool, bool)) *MergeCreditor {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/repos/acme/widget/pulls/7/commits":
			out := make([]map[string]any, 0, len(a.commits))
			for i, msg := range a.commits {
				out = append(out, map[string]any{"sha": fmt.Sprintf("c%d", i), "commit": map[string]any{"message": msg}, "parents": []map[string]any{{"sha": "p"}}})
			}
			enc(out)
		case "/repos/acme/widget/branches/main/protection/required_status_checks":
			a.reads.Add(1)
			if a.required == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Branch not protected"}`)
				return
			}
			enc(map[string]any{"contexts": a.required})
		case "/repos/acme/widget/rules/branches/main":
			a.reads.Add(1)
			enc([]any{})
		case "/repos/acme/widget/commits/head1/status":
			a.reads.Add(1)
			if a.headHTTP != 0 {
				http.Error(w, `{"message":"boom"}`, a.headHTTP)
				return
			}
			statuses := []map[string]any{}
			for _, name := range a.statuses {
				statuses = append(statuses, map[string]any{"context": name, "state": "success"})
			}
			enc(map[string]any{"sha": "head1", "total_count": len(statuses), "statuses": statuses})
		case "/repos/acme/widget/commits/head1/check-runs":
			a.reads.Add(1)
			runs := []map[string]any{}
			for i, name := range a.checks {
				runs = append(runs, map[string]any{"id": i + 1, "name": name, "status": "completed", "conclusion": "success"})
			}
			enc(map[string]any{"total_count": len(runs), "check_runs": runs})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", []string{"widget"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m := NewMergeCreditor(c.client, func(string) bool { return false }, c.logger)
	m.dcoRequired = override
	return m
}

// The unsigned-squash refusal applies only where the repo requires DCO: the
// dco.required override when set, else a dco/DCO context required on the
// base or reported on the head.
func TestSquashSignoffDCOGate(t *testing.T) {
	ctx := context.Background()
	unset := func(string) (bool, bool) { return false, false }
	required := func(string) (bool, bool) { return true, true }
	notRequired := func(string) (bool, bool) { return false, true }
	squash := func(m *MergeCreditor, message string) (string, error) {
		return m.SquashSignoff(ctx, "acme", "widget", 7, "main", "head1", "squash", message)
	}
	t.Run("no gate: an unsigned squash merges with the message as built", func(t *testing.T) {
		api := &dcoGateAPI{commits: []string{"chore(deps): update widget"}, statuses: []string{"tide"}, checks: []string{"Tests Passed"}}
		m := api.creditor(t, unset)
		for _, message := range []string{"", "Co-authored-by: x <x@example.com>"} {
			got, err := squash(m, message)
			if err != nil || got != message {
				t.Fatalf("SquashSignoff(%q) = %q, %v; want the message unchanged", message, got, err)
			}
		}
	})
	t.Run("no gate: the sign-offs that exist are still carried", func(t *testing.T) {
		api := &dcoGateAPI{commits: []string{"Fix", "Review\n\nSigned-off-by: K <k@example.com>"}}
		m := api.creditor(t, unset)
		got, err := squash(m, "Co-authored-by: x <x@example.com>")
		if err != nil || got != "Co-authored-by: x <x@example.com>\n\nSigned-off-by: K <k@example.com>" {
			t.Fatalf("SquashSignoff = %q, %v", got, err)
		}
		if n := api.reads.Load(); n != 0 {
			t.Fatalf("a signed branch read the DCO gate %d times", n)
		}
	})
	t.Run("dco.required true refuses without reading the repo", func(t *testing.T) {
		api := &dcoGateAPI{commits: []string{"Fix"}}
		if _, err := squash(api.creditor(t, required), ""); !errors.Is(err, ErrSquashUnsigned) {
			t.Fatalf("err = %v, want ErrSquashUnsigned", err)
		}
		if n := api.reads.Load(); n != 0 {
			t.Fatalf("override read the DCO gate %d times", n)
		}
	})
	t.Run("dco.required false wins over a detected gate", func(t *testing.T) {
		api := &dcoGateAPI{commits: []string{"Fix"}, required: []string{"DCO"}, statuses: []string{"dco"}, checks: []string{"DCO"}}
		got, err := squash(api.creditor(t, notRequired), "")
		if err != nil || got != "" {
			t.Fatalf("SquashSignoff = %q, %v; want a merge", got, err)
		}
		if n := api.reads.Load(); n != 0 {
			t.Fatalf("override read the DCO gate %d times", n)
		}
	})
	for _, tc := range []struct {
		name string
		api  *dcoGateAPI
	}{
		{name: "DCO required on the base refuses", api: &dcoGateAPI{commits: []string{"Fix"}, required: []string{"build", "DCO"}}},
		{name: "Prow's dco status on the head refuses", api: &dcoGateAPI{commits: []string{"Fix"}, statuses: []string{"tide", "dco"}}},
		{name: "the DCO App's check run on the head refuses", api: &dcoGateAPI{commits: []string{"Fix"}, required: []string{"build"}, checks: []string{"build", "DCO"}}},
		{name: "a head that cannot be read keeps the refusal", api: &dcoGateAPI{commits: []string{"Fix"}, headHTTP: http.StatusNotFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := squash(tc.api.creditor(t, unset), ""); !errors.Is(err, ErrSquashUnsigned) {
				t.Fatalf("err = %v, want ErrSquashUnsigned", err)
			}
		})
	}
}
