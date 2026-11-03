//go:build forgejo_smoke

// Forgejo smoke: runs the minimal governor loop (read issue → kick → merge via
// forge.Merger) against a live, throwaway Forgejo instance seeded by
// .github/workflows/forgejo-smoke.yml. It is excluded from the normal test
// run by the forgejo_smoke build tag and needs:
//
//	FORGEJO_URL   instance root, e.g. http://127.0.0.1:3000
//	FORGEJO_TOKEN API token with repository and issue write scopes
//	FORGEJO_REPO  seeded "owner/name" slug
//	FORGEJO_ISSUE seeded issue number
//	FORGEJO_PR    seeded pull request number
package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forge"
)

const smokeKickLabel = "hive/kicked"

func smokeEnv(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Fatalf("%s is required for the Forgejo smoke", key)
	}
	return v
}

func smokeEnvInt(t *testing.T, key string) int {
	t.Helper()
	n, err := strconv.Atoi(smokeEnv(t, key))
	if err != nil || n <= 0 {
		t.Fatalf("%s must be a positive integer: %v", key, err)
	}
	return n
}

// smokeIssueComments reads the issue's comments straight from the API, so the
// kick is verified independently of the adapter that wrote it.
func smokeIssueComments(ctx context.Context, baseURL, token, repo string, number int) ([]string, error) {
	u := fmt.Sprintf("%s/api/v1/repos/%s/issues/%d/comments", strings.TrimRight(baseURL, "/"), repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	var comments []struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(comments))
	for _, c := range comments {
		out = append(out, c.Body)
	}
	return out, nil
}

func TestForgejoSmoke(t *testing.T) {
	baseURL := smokeEnv(t, "FORGEJO_URL")
	token := smokeEnv(t, "FORGEJO_TOKEN")
	repo := smokeEnv(t, "FORGEJO_REPO")
	issueNum := smokeEnvInt(t, "FORGEJO_ISSUE")
	prNum := smokeEnvInt(t, "FORGEJO_PR")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	f, err := forge.NewForge(forge.KindGitea, token, forge.Options{BaseURL: baseURL})
	if err != nil {
		t.Fatalf("NewForge(%q): %v", forge.KindGitea, err)
	}
	if f.Kind() != forge.KindGitea {
		t.Fatalf("Kind() = %q, want %q", f.Kind(), forge.KindGitea)
	}
	merger, ok := f.(forge.Merger)
	if !ok {
		t.Fatalf("%T does not implement forge.Merger", f)
	}

	r, err := f.GetRepo(ctx, repo)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if r.FullName != repo {
		t.Fatalf("GetRepo FullName = %q, want %q", r.FullName, repo)
	}

	// 1. Read: the governor's scan finds the seeded issue and pull request.
	issues, err := f.ListOpenIssues(ctx, repo)
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	foundIssue := false
	for _, is := range issues {
		if is.Number == issueNum {
			foundIssue = true
		}
	}
	if !foundIssue {
		t.Fatalf("seeded issue #%d not among %d open issues", issueNum, len(issues))
	}
	crs, err := f.ListOpenChangeRequests(ctx, repo)
	if err != nil {
		t.Fatalf("ListOpenChangeRequests: %v", err)
	}
	var cr *forge.ChangeRequest
	for i := range crs {
		if crs[i].Number == prNum {
			cr = &crs[i]
		}
	}
	if cr == nil {
		t.Fatalf("seeded PR #%d not among %d open change requests", prNum, len(crs))
	}
	if cr.HeadSHA == "" {
		t.Fatalf("seeded PR #%d has no head SHA", prNum)
	}

	// 2. Kick: the governor's forge writes on a kick are "post the evidence,
	// then label it" (forge.IssueWriter).
	kickBody := fmt.Sprintf("hive forgejo smoke kick %d", time.Now().UnixNano())
	if err := f.CreateIssueComment(ctx, repo, issueNum, kickBody); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if err := f.AddLabels(ctx, repo, issueNum, []string{smokeKickLabel}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	comments, err := smokeIssueComments(ctx, baseURL, token, repo, issueNum)
	if err != nil {
		t.Fatalf("reading kick comment back: %v", err)
	}
	kicked := false
	for _, c := range comments {
		if c == kickBody {
			kicked = true
		}
	}
	if !kicked {
		t.Fatalf("kick comment not found on issue #%d", issueNum)
	}
	issues, err = f.ListOpenIssues(ctx, repo)
	if err != nil {
		t.Fatalf("ListOpenIssues after kick: %v", err)
	}
	labelled := false
	for _, is := range issues {
		if is.Number != issueNum {
			continue
		}
		for _, l := range is.Labels {
			if l == smokeKickLabel {
				labelled = true
			}
		}
	}
	if !labelled {
		t.Fatalf("issue #%d missing %q label after kick", issueNum, smokeKickLabel)
	}

	// 3. Merge via forge.Merger, through the hold gate.
	opts := forge.MergeOptions{Strategy: string(forge.MergeStrategyMerge), HeadSHA: cr.HeadSHA}
	if err := f.SetHold(ctx, repo, prNum, true); err != nil {
		t.Fatalf("SetHold(true): %v", err)
	}
	if err := merger.Merge(ctx, repo, prNum, opts); !errors.Is(err, forge.ErrHoldActive) {
		t.Fatalf("Merge with hold = %v, want ErrHoldActive", err)
	}
	if err := f.SetHold(ctx, repo, prNum, false); err != nil {
		t.Fatalf("SetHold(false): %v", err)
	}
	// Forgejo computes mergeability asynchronously after the PR is opened, so
	// a freshly seeded PR can briefly report not-mergeable.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err = merger.Merge(ctx, repo, prNum, opts)
		if err == nil || !errors.Is(err, forge.ErrNotMergeable) || time.Now().After(deadline) {
			break
		}
		t.Logf("PR #%d not mergeable yet, retrying: %v", prNum, err)
		select {
		case <-ctx.Done():
			t.Fatalf("context done while waiting for mergeability: %v", ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	crs, err = f.ListOpenChangeRequests(ctx, repo)
	if err != nil {
		t.Fatalf("ListOpenChangeRequests after merge: %v", err)
	}
	for _, c := range crs {
		if c.Number == prNum {
			t.Fatalf("PR #%d still open after merge", prNum)
		}
	}
	if err := merger.Merge(ctx, repo, prNum, opts); !errors.Is(err, forge.ErrAlreadyMerged) {
		t.Fatalf("second Merge = %v, want ErrAlreadyMerged", err)
	}
}
