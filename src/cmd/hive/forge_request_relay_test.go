package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// fakeRelayWriter records the writes the relay makes.
type fakeRelayWriter struct {
	specs    []forge.ChangeRequestSpec
	issues   []string
	labels   [][]string
	comments []string
	removed  []string
	crErr    error
	issueErr error
	writeErr error
}

func (f *fakeRelayWriter) CreateIssueComment(_ context.Context, _ string, _ int, body string) error {
	f.comments = append(f.comments, body)
	return f.writeErr
}

func (f *fakeRelayWriter) AddLabels(_ context.Context, _ string, _ int, labels []string) error {
	f.labels = append(f.labels, labels)
	return f.writeErr
}

func (f *fakeRelayWriter) RemoveLabel(_ context.Context, _ string, _ int, label string) error {
	f.removed = append(f.removed, label)
	return f.writeErr
}

func (f *fakeRelayWriter) CreateIssue(_ context.Context, _ string, title, _ string, labels []string) (forge.IssueRef, error) {
	f.issues = append(f.issues, title)
	f.labels = append(f.labels, labels)
	if f.issueErr != nil && !errors.Is(f.issueErr, forge.ErrLabelsNotApplied) {
		return forge.IssueRef{}, f.issueErr
	}
	return forge.IssueRef{Number: 77, URL: "https://fj.example/acme/widgets/issues/77"}, f.issueErr
}

func (f *fakeRelayWriter) CreateChangeRequest(_ context.Context, _ string, spec forge.ChangeRequestSpec) (forge.ChangeRequestRef, error) {
	f.specs = append(f.specs, spec)
	if f.crErr != nil && !errors.Is(f.crErr, forge.ErrLabelsNotApplied) {
		return forge.ChangeRequestRef{}, f.crErr
	}
	return forge.ChangeRequestRef{Number: 42, URL: "https://fj.example/acme/widgets/pulls/42"}, f.crErr
}

type relayHarness struct {
	relay  *forgeRequestRelay
	writer *fakeRelayWriter
	cfg    *config.Config
}

func newRelayHarness(t *testing.T) *relayHarness {
	t.Helper()
	cfg := forgejoOnlyConfig("https://fj.example")
	h := &relayHarness{writer: &fakeRelayWriter{}, cfg: cfg}
	prev := forgeRelayWriterForRepo
	t.Cleanup(func() { forgeRelayWriterForRepo = prev })
	forgeRelayWriterForRepo = func(*config.Config, string) (forgeRelayWriter, error) { return h.writer, nil }

	uid := os.Getuid()
	ownerOnly := func(agent string, fileUID int) error {
		if agent != "worker" || fileUID != uid {
			return fmt.Errorf("agent %q (uid %d) may not write", agent, fileUID)
		}
		return nil
	}
	h.relay = newForgeRequestRelay(func() *config.Config { return h.cfg },
		ownerOnly,
		func(agent string, fileUID int, _ string) error { return ownerOnly(agent, fileUID) },
		discardLogger())
	h.relay.prDir = t.TempDir()
	h.relay.issueDir = t.TempDir()
	return h
}

func writeRelayRequest(t *testing.T, dir, name string, req any) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRelayResult(t *testing.T, reqPath string, into any) {
	t.Helper()
	b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("no result for %s: %v", reqPath, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if got := err == nil; got != want {
		t.Errorf("%s exists = %v, want %v", path, got, want)
	}
}

func goodPR() github.PRRequest {
	return github.PRRequest{Repo: "widgets", Head: "fix-1", Title: "fix: one", Body: "Fixes #1", Agent: "worker"}
}

func TestForgeRelayOpensHeldPR(t *testing.T) {
	h := newRelayHarness(t)
	path := writeRelayRequest(t, h.relay.prDir, "a.json", goodPR())
	h.relay.scanOnce(context.Background())

	if len(h.writer.specs) != 1 {
		t.Fatalf("specs = %v", h.writer.specs)
	}
	spec := h.writer.specs[0]
	if spec.Head != "fix-1" || spec.Title != "fix: one" || spec.Body != "Fixes #1" {
		t.Errorf("spec = %+v", spec)
	}
	if len(spec.Labels) != 1 || spec.Labels[0] != github.ReadyForReviewLabelName() {
		t.Errorf("labels = %v, want only the ready-for-review gate", spec.Labels)
	}
	var resp github.PRResponse
	readRelayResult(t, path, &resp)
	if !resp.OK || resp.Number != 42 || resp.Error != "" {
		t.Errorf("resp = %+v", resp)
	}
	assertExists(t, path, false)
}

func TestForgeRelayPRLabelFailureIsReported(t *testing.T) {
	h := newRelayHarness(t)
	h.writer.crErr = fmt.Errorf("%w: boom", forge.ErrLabelsNotApplied)
	path := writeRelayRequest(t, h.relay.prDir, "a.json", goodPR())
	h.relay.scanOnce(context.Background())

	var resp github.PRResponse
	readRelayResult(t, path, &resp)
	if !resp.OK || resp.Number != 42 || !strings.Contains(resp.Error, "labels were not applied") {
		t.Errorf("resp = %+v, want an OK open that names the missing gate label", resp)
	}
	assertExists(t, path, false)
}

func TestForgeRelayPRRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*relayHarness, *github.PRRequest)
		suffix string
		errHas string
	}{
		"wrong agent": {func(_ *relayHarness, r *github.PRRequest) { r.Agent = "intruder" }, ".denied", "authorization denied"},
		"nil authz":   {func(h *relayHarness, _ *github.PRRequest) { h.relay.prAuthz = nil }, ".denied", "fail closed"},
		"unmanaged":   {func(_ *relayHarness, r *github.PRRequest) { r.Repo = "other" }, ".rejected", "not managed"},
		"paused": {func(h *relayHarness, _ *github.PRRequest) {
			h.cfg.Project.PausedRepos = []config.RepoPause{{Repo: "widgets"}}
		}, ".rejected", "repo rejected"},
		"detached head": {func(_ *relayHarness, r *github.PRRequest) { r.Head = "HEAD" }, ".rejected", "head rejected"},
		"empty body":    {func(_ *relayHarness, r *github.PRRequest) { r.Body = " " }, ".rejected", "title and a body"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRelayHarness(t)
			req := goodPR()
			tc.mutate(h, &req)
			path := writeRelayRequest(t, h.relay.prDir, "a.json", req)
			h.relay.scanOnce(context.Background())

			if len(h.writer.specs) != 0 {
				t.Fatalf("opened a PR on a refused request: %v", h.writer.specs)
			}
			var resp github.PRResponse
			readRelayResult(t, path, &resp)
			if resp.OK || !strings.Contains(resp.Error, tc.errHas) {
				t.Errorf("resp = %+v, want an error containing %q", resp, tc.errHas)
			}
			assertExists(t, path+tc.suffix, true)
		})
	}
}

func TestForgeRelayPRTransientFailureRetriesThenQuarantines(t *testing.T) {
	h := newRelayHarness(t)
	h.writer.crErr = errors.New("forgejo API returned status 502")
	path := writeRelayRequest(t, h.relay.prDir, "a.json", goodPR())
	for i := 1; i < forgeRelayMaxAttempts; i++ {
		h.relay.scanOnce(context.Background())
		assertExists(t, path, true)
	}
	h.relay.scanOnce(context.Background())
	assertExists(t, path, false)
	assertExists(t, path+".failed", true)
	if len(h.writer.specs) != forgeRelayMaxAttempts {
		t.Errorf("attempts = %d, want %d", len(h.writer.specs), forgeRelayMaxAttempts)
	}
}

func TestForgeRelayMalformedAndUnsafeFiles(t *testing.T) {
	h := newRelayHarness(t)
	bad := filepath.Join(h.relay.prDir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A fresh unparsable file may be a write in progress: it is left alone.
	h.relay.scanOnce(context.Background())
	assertExists(t, bad, true)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(bad, old, old); err != nil {
		t.Fatal(err)
	}
	h.relay.scanOnce(context.Background())
	assertExists(t, bad+".bad", true)

	target := writeRelayRequest(t, t.TempDir(), "t.json", goodPR())
	link := filepath.Join(h.relay.prDir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	h.relay.scanOnce(context.Background())
	assertExists(t, link+".rejected", true)
	if len(h.writer.specs) != 0 {
		t.Errorf("opened a PR from an unsafe file: %v", h.writer.specs)
	}
}

func TestForgeRelayIssueKinds(t *testing.T) {
	h := newRelayHarness(t)
	dir := h.relay.issueDir
	create := writeRelayRequest(t, dir, "1.json", github.IssueRequest{Repo: "widgets", Title: "New", Body: "b", Labels: []string{"kind/bug"}, Agent: "worker"})
	comment := writeRelayRequest(t, dir, "2.json", github.IssueRequest{Kind: "comment", Repo: "widgets", Number: 5, Body: "hi", Agent: "worker"})
	label := writeRelayRequest(t, dir, "3.json", github.IssueRequest{Kind: "label", Repo: "widgets", Number: 5, Labels: []string{"kind/bug"}, RemoveLabels: []string{"triage"}, Agent: "worker"})
	h.relay.scanOnce(context.Background())

	var resp github.IssueResponse
	readRelayResult(t, create, &resp)
	if !resp.OK || resp.Number != 77 {
		t.Errorf("create resp = %+v", resp)
	}
	if len(h.writer.issues) != 1 || h.writer.issues[0] != "New" {
		t.Errorf("issues = %v", h.writer.issues)
	}
	if got := h.writer.labels[0]; len(got) != 2 || got[0] != "kind/bug" || got[1] != config.OriginLabelForAgent("worker") {
		t.Errorf("create labels = %v, want kind/bug and the agent's origin label", got)
	}
	if len(h.writer.comments) != 1 || h.writer.comments[0] != "hi" {
		t.Errorf("comments = %v", h.writer.comments)
	}
	if len(h.writer.removed) != 1 || h.writer.removed[0] != "triage" {
		t.Errorf("removed = %v", h.writer.removed)
	}
	for _, p := range []string{create, comment, label} {
		assertExists(t, p, false)
	}
}

func TestForgeRelayIssueCreateLabelFailureKeepsNumber(t *testing.T) {
	h := newRelayHarness(t)
	h.writer.issueErr = fmt.Errorf("%w: boom", forge.ErrLabelsNotApplied)
	path := writeRelayRequest(t, h.relay.issueDir, "1.json", github.IssueRequest{Repo: "widgets", Title: "New", Agent: "worker"})
	h.relay.scanOnce(context.Background())

	var resp github.IssueResponse
	readRelayResult(t, path, &resp)
	if !resp.OK || resp.Number != 77 || resp.Error == "" {
		t.Errorf("resp = %+v, want OK with the created number and the label error", resp)
	}
	assertExists(t, path, false)
}

func TestForgeRelayIssueRefusals(t *testing.T) {
	cases := map[string]struct {
		req    github.IssueRequest
		nilAz  bool
		suffix string
		errHas string
	}{
		"gate label":      {github.IssueRequest{Kind: "label", Repo: "widgets", Number: 5, RemoveLabels: []string{"hold"}, Agent: "worker"}, false, ".bad", "hive-controlled"},
		"ready label":     {github.IssueRequest{Kind: "label", Repo: "widgets", Number: 5, RemoveLabels: []string{github.ReadyForReviewLabelName()}, Agent: "worker"}, false, ".bad", "hive-controlled"},
		"lgtm":            {github.IssueRequest{Kind: "label", Repo: "widgets", Number: 5, Labels: []string{"lgtm"}, Agent: "worker"}, false, ".bad", "hive-controlled"},
		"approved":        {github.IssueRequest{Kind: "label", Repo: "widgets", Number: 5, Labels: []string{"approved-direction"}, Agent: "worker"}, false, ".bad", "hive-controlled"},
		"needs-human off": {github.IssueRequest{Kind: "label", Repo: "widgets", Number: 5, RemoveLabels: []string{"needs-human"}, Agent: "worker"}, false, ".bad", "hive-controlled"},
		"stage on create": {github.IssueRequest{Repo: "widgets", Title: "t", Labels: []string{"stage/010-actionable"}, Agent: "worker"}, false, ".bad", "hive-controlled"},
		"no title":        {github.IssueRequest{Repo: "widgets", Agent: "worker"}, false, ".bad", "requires repo and title"},
		"no body":         {github.IssueRequest{Kind: "comment", Repo: "widgets", Number: 5, Agent: "worker"}, false, ".bad", "requires repo, number, and body"},
		"no labels":       {github.IssueRequest{Kind: "label", Repo: "widgets", Number: 5, Agent: "worker"}, false, ".bad", "at least one label"},
		"unsupported":     {github.IssueRequest{Kind: "close", Repo: "widgets", Number: 5, Agent: "worker"}, false, ".rejected", "not supported"},
		"wrong agent":     {github.IssueRequest{Repo: "widgets", Title: "t", Agent: "intruder"}, false, ".denied", "authorization denied"},
		"nil authz":       {github.IssueRequest{Repo: "widgets", Title: "t", Agent: "worker"}, true, ".denied", "fail closed"},
		"unmanaged":       {github.IssueRequest{Repo: "other", Title: "t", Agent: "worker"}, false, ".rejected", "not managed"},
		"out of scope":    {github.IssueRequest{Repo: "widgets", Title: "t", Agent: "worker"}, false, ".rejected", "repo rejected"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRelayHarness(t)
			if tc.nilAz {
				h.relay.issueAuthz = nil
			}
			if name == "out of scope" {
				h.cfg.Project.PausedRepos = []config.RepoPause{{Repo: "widgets"}}
			}
			path := writeRelayRequest(t, h.relay.issueDir, "1.json", tc.req)
			h.relay.scanOnce(context.Background())

			if len(h.writer.issues)+len(h.writer.comments)+len(h.writer.labels)+len(h.writer.removed) != 0 {
				t.Fatalf("wrote on a refused request: %+v", h.writer)
			}
			var resp github.IssueResponse
			readRelayResult(t, path, &resp)
			if resp.OK || !strings.Contains(resp.Error, tc.errHas) {
				t.Errorf("resp = %+v, want an error containing %q", resp, tc.errHas)
			}
			assertExists(t, path+tc.suffix, true)
		})
	}
}

func TestForgeRelayIssueTransientFailureRetries(t *testing.T) {
	h := newRelayHarness(t)
	h.writer.writeErr = errors.New("status 502")
	path := writeRelayRequest(t, h.relay.issueDir, "1.json", github.IssueRequest{Kind: "comment", Repo: "widgets", Number: 5, Body: "hi", Agent: "worker"})
	h.relay.scanOnce(context.Background())
	assertExists(t, path, true)
	var resp github.IssueResponse
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "502") {
		t.Errorf("resp = %+v", resp)
	}
}

func TestForgeRelayWriterForRepo(t *testing.T) {
	cfg := forgejoOnlyConfig("https://fj.example")
	w, err := forgeRelayWriterForRepo(cfg, "widgets")
	if err != nil || w == nil {
		t.Fatalf("forgejo writer: %v, %v", w, err)
	}
	cfg.Project.Forge = config.ForgeGitLab
	if _, err := forgeRelayWriterForRepo(cfg, "widgets"); err == nil || !strings.Contains(err.Error(), "cannot create") {
		t.Errorf("gitlab writer: err = %v, want a refusal (no create seams)", err)
	}
	if _, err := forgeRelayWriterForRepo(&config.Config{}, "widgets"); !errors.Is(err, errForgeNotOffGitHub) {
		t.Errorf("github writer: err = %v, want errForgeNotOffGitHub", err)
	}
}

func TestStartForgeRequestRelayStopsOnCancel(t *testing.T) {
	h := newRelayHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := startForgeRequestRelay(ctx, h.relay)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not stop on cancel")
	}
}

func TestForgeRelayPermanentForgeRefusalIsFinal(t *testing.T) {
	h := newRelayHarness(t)
	h.writer.crErr = errors.New("forgejo API /repos/acme/widgets/pulls returned status 422: head not found")
	path := writeRelayRequest(t, h.relay.prDir, "a.json", goodPR())
	h.relay.scanOnce(context.Background())
	assertExists(t, path+".rejected", true)

	h.writer.writeErr = errors.New("forgejo API returned status 404: no issue")
	ipath := writeRelayRequest(t, h.relay.issueDir, "1.json", github.IssueRequest{Kind: "comment", Repo: "widgets", Number: 5, Body: "hi", Agent: "worker"})
	h.relay.scanOnce(context.Background())
	assertExists(t, ipath+".rejected", true)
}

func TestIsPermanentForgeRefusal(t *testing.T) {
	for msg, want := range map[string]bool{
		"x returned status 422: bad":  true,
		"x returned status 404":       true,
		"x returned status 429: slow": false,
		"x returned status 408":       false,
		"x returned status 502":       false,
		"dial tcp: refused":           false,
	} {
		if got := isPermanentForgeRefusal(errors.New(msg)); got != want {
			t.Errorf("%q = %v, want %v", msg, got, want)
		}
	}
}

// TestForgeRelayIdlesUnlessForgeOnly is the negative test: on a hive with a
// GitHub repo the relay touches no request, so the GitHub relays own them.
func TestForgeRelayIdlesUnlessForgeOnly(t *testing.T) {
	h := newRelayHarness(t)
	h.cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "gh-repo", Forge: config.ForgeGitHub}}
	pr := writeRelayRequest(t, h.relay.prDir, "a.json", goodPR())
	is := writeRelayRequest(t, h.relay.issueDir, "1.json", github.IssueRequest{Repo: "widgets", Title: "t", Agent: "worker"})
	h.relay.scanOnce(context.Background())
	assertExists(t, pr, true)
	assertExists(t, is, true)
	if len(h.writer.specs)+len(h.writer.issues) != 0 {
		t.Errorf("relay wrote on a hive with a GitHub repo: %+v", h.writer)
	}
	h.relay.cfg = func() *config.Config { return nil }
	h.relay.scanOnce(context.Background())
	assertExists(t, pr, true)
}

func TestRepoRefusalRefusesGitHubRepo(t *testing.T) {
	cfg := forgejoOnlyConfig("https://fj.example")
	cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "widgets", Forge: config.ForgeGitHub}}
	if got := repoRefusal(cfg, "worker", "widgets"); !strings.Contains(got, "is on GitHub") {
		t.Errorf("repoRefusal = %q, want the GitHub refusal", got)
	}
	if got := repoRefusal(forgejoOnlyConfig("https://fj.example"), "worker", "acme/widgets"); got != "" {
		t.Errorf("owner-qualified managed repo refused: %q", got)
	}
}
