package main

// Tests for the phase-2 glue of the release sentinel (hivecommons/hive#9585):
// retag after merge behind its own toggle, and pre-tag release-workflow
// failures. The state machine and the git retagger are tested in
// pkg/releasesentinel.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/releasesentinel"
)

// releaseSentinelFixSHA is the merge commit of the marked fix PR.
const releaseSentinelFixSHA = "dddddddddddddddddddddddddddddddddddddddd"

type spyRetagger struct {
	reqs []releasesentinel.RetagRequest
}

func (s *spyRetagger) Retag(_ context.Context, req releasesentinel.RetagRequest) error {
	s.reqs = append(s.reqs, req)
	return nil
}

// stubRetaggerFactory replaces newReleaseSentinelRetagger for one test and
// counts how often the glue asked for a retagger.
func stubRetaggerFactory(t *testing.T, r releasesentinel.Retagger, err error) *int {
	t.Helper()
	calls := 0
	old := newReleaseSentinelRetagger
	newReleaseSentinelRetagger = func(*config.Config, *github.Client, string, string) (releasesentinel.Retagger, error) {
		calls++
		return r, err
	}
	t.Cleanup(func() { newReleaseSentinelRetagger = old })
	return &calls
}

// phase2Server serves acme/widgets: tag v1.0.0 on releaseSentinelTestSHA with
// one failed code run, default branch main, a merged fix PR #42 carrying the
// v1.0.0 marker (merged an hour in the future, so after any round start), and
// a "Tagged Release" workflow whose latest run failed with annotation.
func phase2Server(t *testing.T, withTag bool, annotation string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	js := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		}
	}
	if withTag {
		mux.HandleFunc("/repos/acme/widgets/tags", js(`[{"name":"v1.0.0","commit":{"sha":"`+releaseSentinelTestSHA+`"}}]`))
	} else {
		mux.HandleFunc("/repos/acme/widgets/tags", js(`[]`))
	}
	mux.HandleFunc("/repos/acme/widgets/actions/runs", js(`{"total_count":1,"workflow_runs":[{"id":5,"name":"CI","workflow_id":1,"event":"push","head_sha":"`+
		releaseSentinelTestSHA+`","status":"completed","conclusion":"failure","html_url":"https://example.test/runs/5"}]}`))
	mux.HandleFunc("/repos/acme/widgets/actions/runs/5/jobs", js(`{"total_count":1,"jobs":[{"id":9,"name":"release","conclusion":"failure","steps":[{"name":"build","conclusion":"failure"}]}]}`))
	mux.HandleFunc("/repos/acme/widgets/check-runs/9/annotations", js(fmt.Sprintf(`[{"annotation_level":"failure","message":%q}]`, annotation)))
	mux.HandleFunc("/repos/acme/widgets", js(`{"name":"widgets","default_branch":"main"}`))
	mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		merged := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `[{"number":42,"body":"fix\n\nRelease-Sentinel: v1.0.0","merged_at":%q,"merge_commit_sha":%q,"base":{"ref":"main"}}]`, merged, releaseSentinelFixSHA)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/42/commits", js(`[{"sha":"`+releaseSentinelFixSHA+`"}]`))
	mux.HandleFunc("/repos/acme/widgets/actions/workflows", js(`{"total_count":1,"workflows":[{"id":1,"name":"Tagged Release","path":".github/workflows/tagged-release.yml"}]}`))
	mux.HandleFunc("/repos/acme/widgets/actions/workflows/1/runs", js(`{"total_count":1,"workflow_runs":[{"id":5,"head_sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","status":"completed","conclusion":"failure"}]}`))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunReleaseSentinel_RetagToggleOffNeverBuildsARetagger(t *testing.T) {
	isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	t.Setenv(config.ReleaseSentinelRetagEnabledEnvVar, "")
	spy := &spyRetagger{}
	calls := stubRetaggerFactory(t, spy, nil)
	srv := phase2Server(t, true, "go test failed")
	logger, _ := captureLogger()
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	kicker := &fakeKicker{}

	// Sentinel on, retag off: two passes (round, then round in flight with a
	// merged marked PR available) must never build or use a retagger.
	runReleaseSentinel(context.Background(), enabledSentinelConfig(), client, kicker, nil, nil, logger)
	releaseSentinelLastRun = time.Time{}
	runReleaseSentinel(context.Background(), enabledSentinelConfig(), client, kicker, nil, nil, logger)

	// Config on, env off: still off.
	cfg := enabledSentinelConfig()
	cfg.ReleaseSentinel.RetagEnabled = true
	t.Setenv(config.ReleaseSentinelRetagEnabledEnvVar, "false")
	releaseSentinelLastRun = time.Time{}
	runReleaseSentinel(context.Background(), cfg, client, kicker, nil, nil, logger)

	if *calls != 0 || len(spy.reqs) != 0 {
		t.Fatalf("retag off: factory calls=%d retags=%d", *calls, len(spy.reqs))
	}
	if len(kicker.kicks) != 1 || strings.Contains(kicker.kicks[0].message, releasesentinel.FixMarkerKey) {
		t.Fatalf("retag-off kick promised a retag: %+v", kicker.kicks)
	}
}

func TestRunReleaseSentinel_RetagAfterMergedFix(t *testing.T) {
	path := isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	t.Setenv(config.ReleaseSentinelRetagEnabledEnvVar, "")
	spy := &spyRetagger{}
	calls := stubRetaggerFactory(t, spy, nil)
	srv := phase2Server(t, true, "go test failed")
	logger, buf := captureLogger()
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	kicker := &fakeKicker{}
	cfg := enabledSentinelConfig()
	cfg.ReleaseSentinel.RetagEnabled = true

	runReleaseSentinel(context.Background(), cfg, client, kicker, func(string) bool { return true }, nil, logger)
	if len(kicker.kicks) != 1 || !strings.Contains(kicker.kicks[0].message, "Release-Sentinel: v1.0.0") {
		t.Fatalf("round 1 kick does not ask for the marker: %+v", kicker.kicks)
	}
	if len(spy.reqs) != 0 {
		t.Fatal("retagged before any round was in flight")
	}

	releaseSentinelLastRun = time.Time{}
	runReleaseSentinel(context.Background(), cfg, client, kicker, nil, nil, logger)
	if *calls != 2 || len(spy.reqs) != 1 {
		t.Fatalf("factory calls=%d retags=%d, want 2/1", *calls, len(spy.reqs))
	}
	req := spy.reqs[0]
	if req.Tag != "v1.0.0" || req.OldSHA != releaseSentinelTestSHA || req.NewSHA != releaseSentinelFixSHA || req.Branch != "main" ||
		req.Repo != "acme/widgets" || len(req.FixCommits) != 1 || req.AllowIntervening {
		t.Fatalf("retag request = %+v", req)
	}
	if !bytes.Contains(buf.Bytes(), []byte("action=retagged")) {
		t.Fatalf("no retag transition logged:\n%s", buf.String())
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"retag_pr": 42`) || !strings.Contains(string(data), releaseSentinelFixSHA) {
		t.Fatalf("state file = %s, %v", data, err)
	}
}

func TestRunReleaseSentinel_RetagUnavailableFallsBackToPlainSentinel(t *testing.T) {
	isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	t.Setenv(config.ReleaseSentinelRetagEnabledEnvVar, "true")
	stubRetaggerFactory(t, nil, errors.New("no app"))
	srv := phase2Server(t, true, "go test failed")
	logger, buf := captureLogger()
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	kicker := &fakeKicker{}
	runReleaseSentinel(context.Background(), enabledSentinelConfig(), client, kicker, nil, nil, logger)
	if !bytes.Contains(buf.Bytes(), []byte("retag enabled but unavailable")) {
		t.Fatalf("unavailable retag not logged:\n%s", buf.String())
	}
	if len(kicker.kicks) != 1 || strings.Contains(kicker.kicks[0].message, releasesentinel.FixMarkerKey) {
		t.Fatalf("kick promised a retag the hive cannot do: %+v", kicker.kicks)
	}
}

func TestNewReleaseSentinelRetagger_NeedsAppAuth(t *testing.T) {
	logger, _ := captureLogger()
	client := github.NewClientForTest("http://127.0.0.1:1", "acme", nil, logger)
	if r, err := newReleaseSentinelRetagger(enabledSentinelConfig(), client, "acme", "widgets"); err == nil || r != nil {
		t.Fatalf("static-token client produced a retagger: %v %v", r, err)
	}
}

func TestRunReleaseSentinel_PreTagPolicyFailureEscalatesWithoutKick(t *testing.T) {
	path := isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	srv := phase2Server(t, false, "GraphQL: GitHub Actions is not permitted to create or approve pull requests")
	logger, buf := captureLogger()
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	kicker := &fakeKicker{}
	notifier := notify.New(config.NotificationsConfig{}, logger)
	cfg := enabledSentinelConfig()
	cfg.ReleaseSentinel.ReleaseWorkflows = []string{"Tagged Release"}

	runReleaseSentinel(context.Background(), cfg, client, kicker, nil, notifier, logger)
	if len(kicker.kicks) != 0 {
		t.Fatalf("pre-tag policy failure kicked an agent: %+v", kicker.kicks)
	}
	for _, want := range []string{"escalated a pre-tag release workflow failure", "escalated a release to a human", "reason=policy"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Fatalf("log missing %q:\n%s", want, buf.String())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"kind": "pre_tag"`) {
		t.Fatalf("state file = %s, %v", data, err)
	}
}

func TestRunReleaseSentinel_PreTagFixableIsLoggedNotKicked(t *testing.T) {
	isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	srv := phase2Server(t, false, "go test failed")
	logger, buf := captureLogger()
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	kicker := &fakeKicker{}
	cfg := enabledSentinelConfig()
	cfg.ReleaseSentinel.ReleaseWorkflows = []string{"tagged-release.yml"}
	runReleaseSentinel(context.Background(), cfg, client, kicker, nil, nil, logger)
	if len(kicker.kicks) != 0 || !bytes.Contains(buf.Bytes(), []byte("left a fixable pre-tag release workflow failure")) {
		t.Fatalf("kicks=%d log:\n%s", len(kicker.kicks), buf.String())
	}
}
