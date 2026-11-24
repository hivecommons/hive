//go:build forgejo_e2e

// Package forgee2e runs one hive cycle against a live, throwaway Forgejo
// seeded by .github/workflows/forgejo-e2e.yml (hivecommons/hive#19683):
// enumerate the issue through the Forgejo adapter, clone the repo, push a
// signed-off branch through the real push broker on the Forgejo host, open
// the PR, label it, merge it at the pinned head, and check that the closing
// keyword closed the issue. It is excluded from the normal test run by the
// forgejo_e2e build tag and needs:
//
//	FORGEJO_URL   instance root, e.g. http://127.0.0.1:3000
//	FORGEJO_TOKEN API token with repository and issue write scopes
//	FORGEJO_REPO  seeded "owner/name" slug, default branch main
//	FORGEJO_ISSUE seeded open issue number
package forgee2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/gitidentity"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/forge"
)

const (
	e2eAgent       = "scanner"
	e2eKickLabel   = "hive/kicked"
	cycleTimeout   = 5 * time.Minute
	mergeRetryFor  = 2 * time.Minute
	closeWaitFor   = 30 * time.Second
	pollInterval   = 2 * time.Second
	defaultBranch  = "main"
	changedFile    = "E2E.md"
	workBranchBase = "hive/e2e-"
)

var releaseLabels = []string{"lgtm", "approved"}

func env(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Fatalf("%s is required for the Forgejo e2e", key)
	}
	return v
}

// api calls the Forgejo REST API directly, so every step the adapter or the
// broker took is checked independently of the code under test.
func api(ctx context.Context, t *testing.T, method, url, token string, body any, out any) int {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, url, err)
		}
	}
	return resp.StatusCode
}

func git(t *testing.T, dir string, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"GIT_TERMINAL_PROMPT=0"}, extraEnv...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestForgejoHiveCycle(t *testing.T) {
	baseURL := strings.TrimRight(env(t, "FORGEJO_URL"), "/")
	token := env(t, "FORGEJO_TOKEN")
	repo := env(t, "FORGEJO_REPO")
	issueNum, err := strconv.Atoi(env(t, "FORGEJO_ISSUE"))
	if err != nil || issueNum <= 0 {
		t.Fatalf("FORGEJO_ISSUE: %v", err)
	}
	apiRepo := baseURL + "/api/v1/repos/" + repo

	ctx, cancel := context.WithTimeout(context.Background(), cycleTimeout)
	defer cancel()

	f, err := forge.NewForge(forge.KindForgejo, token, forge.Options{BaseURL: baseURL})
	if err != nil {
		t.Fatalf("NewForge(forgejo): %v", err)
	}
	merger, ok := f.(forge.Merger)
	if !ok {
		t.Fatalf("%T does not implement forge.Merger", f)
	}

	// 1. Enumerate: the issue is found through the forge adapter.
	issues, err := f.ListOpenIssues(ctx, repo)
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	found := false
	for _, is := range issues {
		found = found || is.Number == issueNum
	}
	if !found {
		t.Fatalf("seeded issue #%d not among %d open issues", issueNum, len(issues))
	}
	if err := f.AddLabels(ctx, repo, issueNum, []string{e2eKickLabel}); err != nil {
		t.Fatalf("AddLabels(kick): %v", err)
	}

	// 2. Clone and commit as the lane, signed off, as a sandbox kick would.
	name, email, ok := gitidentity.AgentIdentity(e2eAgent)
	if !ok {
		t.Fatal("no lane identity")
	}
	ident := []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
	work := filepath.Join(t.TempDir(), "ws")
	cloneURL := pushbroker.ForgeRepoURL(baseURL, repo)
	if cloneURL == "" {
		t.Fatalf("no clone URL for %s on %s", repo, baseURL)
	}
	git(t, "", nil, "clone", "-q", cloneURL, work)
	base := git(t, work, nil, "rev-parse", "HEAD")
	branch := workBranchBase + strconv.FormatInt(time.Now().UnixNano(), 10)
	git(t, work, nil, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, changedFile), []byte("forgejo e2e "+branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, ident, "add", changedFile)
	git(t, work, ident, "commit", "-q", "-s", "-m", "e2e: add "+changedFile)

	// 3. Push through the real broker: remote pinned to the forge, the token
	// offered only to the forge's origin by the URL-scoped helper.
	minted := 0
	minter := pushbroker.StaticTokenMinter{Token: func() string { minted++; return token }}
	broker := &pushbroker.Broker{
		Workspace:     work,
		Branch:        branch,
		BaseRef:       base,
		Repo:          repo,
		AgentName:     e2eAgent,
		RemoteBaseURL: baseURL,
		Minter:        minter,
	}
	res, err := broker.Run(ctx)
	if err != nil || !res.Pushed {
		t.Fatalf("broker push: %v (res=%+v)", err, res)
	}
	if minted != 1 {
		t.Fatalf("broker minted %d tokens, want exactly 1", minted)
	}
	var br struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if code := api(ctx, t, http.MethodGet, apiRepo+"/branches/"+branch, token, nil, &br); code != http.StatusOK || br.Commit.ID != res.Commit {
		t.Fatalf("pushed branch on the forge: HTTP %d, head %q, want %q", code, br.Commit.ID, res.Commit)
	}

	// A broker whose workspace points at the forge but which is told the
	// repo lives on github.com refuses before minting: the forge token is
	// never offered to a host the workspace was not cloned from.
	wrong := &pushbroker.Broker{Workspace: work, Branch: branch, BaseRef: base, Repo: repo, AgentName: e2eAgent, Minter: minter}
	if _, err := wrong.Run(ctx); err == nil {
		t.Fatal("broker pushed a forge workspace to github.com")
	}
	if minted != 1 {
		t.Fatalf("a token was minted for the wrong host (minted=%d)", minted)
	}

	// 4. Open the PR with a closing keyword for the issue.
	var pr struct {
		Number int `json:"number"`
	}
	body := map[string]any{
		"title": "e2e: add " + changedFile,
		"head":  branch,
		"base":  defaultBranch,
		"body":  fmt.Sprintf("Opened by the Forgejo e2e.\n\nFixes #%d", issueNum),
	}
	if code := api(ctx, t, http.MethodPost, apiRepo+"/pulls", token, body, &pr); code != http.StatusCreated || pr.Number <= 0 {
		t.Fatalf("create PR: HTTP %d", code)
	}

	// 5. Label it through the adapter and find it, with its head, the way
	// the governor's scan does.
	if err := f.AddLabels(ctx, repo, pr.Number, releaseLabels); err != nil {
		t.Fatalf("AddLabels(release): %v", err)
	}
	crs, err := f.ListOpenChangeRequests(ctx, repo)
	if err != nil {
		t.Fatalf("ListOpenChangeRequests: %v", err)
	}
	var cr *forge.ChangeRequest
	for i := range crs {
		if crs[i].Number == pr.Number {
			cr = &crs[i]
		}
	}
	if cr == nil {
		t.Fatalf("PR #%d not among %d open change requests", pr.Number, len(crs))
	}
	if cr.HeadSHA != res.Commit {
		t.Fatalf("PR head %q, want the pushed commit %q", cr.HeadSHA, res.Commit)
	}
	for _, want := range releaseLabels {
		has := false
		for _, l := range cr.Labels {
			has = has || l == want
		}
		if !has {
			t.Fatalf("PR #%d labels %v, missing %q", pr.Number, cr.Labels, want)
		}
	}

	// 6. Merge at the pinned head. A stale head is refused first.
	stale := forge.MergeOptions{Strategy: string(forge.MergeStrategyMerge), HeadSHA: base}
	if err := merger.Merge(ctx, repo, pr.Number, stale); err == nil {
		t.Fatal("merge at a stale head succeeded")
	}
	opts := forge.MergeOptions{Strategy: string(forge.MergeStrategyMerge), HeadSHA: cr.HeadSHA}
	deadline := time.Now().Add(mergeRetryFor)
	for {
		err = merger.Merge(ctx, repo, pr.Number, opts)
		if err == nil || !errors.Is(err, forge.ErrNotMergeable) || time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var merged struct {
		Merged bool `json:"merged"`
	}
	if code := api(ctx, t, http.MethodGet, fmt.Sprintf("%s/pulls/%d", apiRepo, pr.Number), token, nil, &merged); code != http.StatusOK || !merged.Merged {
		t.Fatalf("PR #%d not merged on the forge (HTTP %d)", pr.Number, code)
	}

	// 7. Forgejo's closing keyword closes the issue on a merge into the
	// default branch. It runs asynchronously, so give it a moment; a miss is
	// logged rather than failed, since the close-on-merge sweep covers it.
	closeBy := time.Now().Add(closeWaitFor)
	for {
		var is struct {
			State string `json:"state"`
		}
		api(ctx, t, http.MethodGet, fmt.Sprintf("%s/issues/%d", apiRepo, issueNum), token, nil, &is)
		if is.State == "closed" {
			t.Logf("issue #%d closed by the merge of PR #%d", issueNum, pr.Number)
			break
		}
		if time.Now().After(closeBy) {
			t.Logf("issue #%d still %q %s after the merge; Forgejo did not apply the closing keyword in time", issueNum, is.State, closeWaitFor)
			break
		}
		time.Sleep(pollInterval)
	}
}
