package main

// Tests for the cmd/hive glue of the release sentinel (hivecommons/hive#9585):
// the default-OFF gate, the poll throttle, the kick dispatcher and the
// notification escalator. The state machine itself is tested in
// pkg/releasesentinel.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/releasesentinel"
)

const releaseSentinelTestSHA = "cccccccccccccccccccccccccccccccccccccccc"

// isolateReleaseSentinel redirects the state file and resets the throttle.
func isolateReleaseSentinel(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release-sentinel.json")
	oldPath, oldRun := releaseSentinelStatePath, releaseSentinelLastRun
	releaseSentinelStatePath = path
	releaseSentinelLastRun = time.Time{}
	t.Cleanup(func() {
		releaseSentinelStatePath = oldPath
		releaseSentinelLastRun = oldRun
	})
	return path
}

// releaseCIServer serves a repo whose v1.0.0 tag has one failed run with the
// given annotation, and counts API hits (atomically: handlers run on the
// server goroutines).
func releaseCIServer(t *testing.T, annotation string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	count := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			h(w, r)
		}
	}
	mux.HandleFunc("/repos/acme/widgets/tags", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"name":"v1.0.0","commit":{"sha":"`+releaseSentinelTestSHA+`"}}]`)
	}))
	mux.HandleFunc("/repos/acme/widgets/actions/runs", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":1,"workflow_runs":[{"id":5,"name":"Tagged Release","workflow_id":1,"event":"push","head_sha":"`+
			releaseSentinelTestSHA+`","status":"completed","conclusion":"failure","html_url":"https://example.test/runs/5"}]}`)
	}))
	mux.HandleFunc("/repos/acme/widgets/actions/runs/5/jobs", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":1,"jobs":[{"id":9,"name":"release","conclusion":"failure","steps":[{"name":"build","conclusion":"failure"}]}]}`)
	}))
	mux.HandleFunc("/repos/acme/widgets/check-runs/9/annotations", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"annotation_level":"failure","message":%q}]`, annotation)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func enabledSentinelConfig() *config.Config {
	return &config.Config{
		Project:         config.ProjectConfig{Org: "acme", PrimaryRepo: "widgets"},
		ReleaseSentinel: config.ReleaseSentinelConfig{Enabled: true},
	}
}

func TestRunReleaseSentinel_DefaultOffTouchesNothing(t *testing.T) {
	path := isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	var hits atomic.Int64
	srv := releaseCIServer(t, "boom", &hits)
	logger, _ := captureLogger()
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	kicker := &fakeKicker{}

	cfg := enabledSentinelConfig()
	cfg.ReleaseSentinel.Enabled = false
	runReleaseSentinel(context.Background(), cfg, client, kicker, nil, nil, logger)
	runReleaseSentinel(context.Background(), nil, client, kicker, nil, nil, logger)

	// The env override can switch a configured-on sentinel off, too.
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "false")
	runReleaseSentinel(context.Background(), enabledSentinelConfig(), client, kicker, nil, nil, logger)

	if hits.Load() != 0 || len(kicker.kicks) != 0 || !releaseSentinelLastRun.IsZero() {
		t.Fatalf("disabled sentinel did work: hits=%d kicks=%d", hits.Load(), len(kicker.kicks))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled sentinel wrote state (stat err: %v)", err)
	}
}

func TestRunReleaseSentinel_NilClientIsNoop(t *testing.T) {
	isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	logger, _ := captureLogger()
	runReleaseSentinel(context.Background(), enabledSentinelConfig(), nil, &fakeKicker{}, nil, nil, logger)
	if !releaseSentinelLastRun.IsZero() {
		t.Fatal("a pass without a GitHub client consumed the poll budget")
	}
}

func TestRunReleaseSentinel_DispatchesRoundAndThrottles(t *testing.T) {
	path := isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	var hits atomic.Int64
	srv := releaseCIServer(t, "go test failed, ping @someone", &hits)
	logger, buf := captureLogger()
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	kicker := &fakeKicker{}

	runReleaseSentinel(context.Background(), enabledSentinelConfig(), client, kicker, func(string) bool { return true }, nil, logger)
	if len(kicker.kicks) != 1 || kicker.kicks[0].agent != releasesentinel.DefaultAgent {
		t.Fatalf("kicks = %+v, want one to %s", kicker.kicks, releasesentinel.DefaultAgent)
	}
	msg := kicker.kicks[0].message
	if !strings.Contains(msg, "RELEASE REPAIR") || !strings.Contains(msg, "v1.0.0") || !strings.Contains(msg, "round 1/5") {
		t.Fatalf("kick message:\n%s", msg)
	}
	if strings.Contains(msg, "@someone") {
		t.Fatalf("kick carries a raw mention:\n%s", msg)
	}
	if !bytes.Contains(buf.Bytes(), []byte("release sentinel transition")) {
		t.Fatalf("no transition log:\n%s", buf.String())
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"state": "fixing"`) {
		t.Fatalf("state file = %s, %v", data, err)
	}

	// Inside the poll interval: no API traffic, no second kick.
	before := hits.Load()
	runReleaseSentinel(context.Background(), enabledSentinelConfig(), client, kicker, nil, nil, logger)
	if hits.Load() != before || len(kicker.kicks) != 1 {
		t.Fatalf("throttled pass did work: hits %d->%d kicks=%d", before, hits.Load(), len(kicker.kicks))
	}

	// Past the interval, the same failure on the same SHA is the round in
	// flight: still no second kick.
	releaseSentinelLastRun = time.Now().Add(-2 * releasesentinel.DefaultPollInterval)
	runReleaseSentinel(context.Background(), enabledSentinelConfig(), client, kicker, nil, nil, logger)
	if hits.Load() == before || len(kicker.kicks) != 1 {
		t.Fatalf("second pass: hits %d->%d kicks=%d", before, hits.Load(), len(kicker.kicks))
	}
}

func TestRunReleaseSentinel_PolicyFailureEscalatesWithoutKick(t *testing.T) {
	isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	var hits atomic.Int64
	srv := releaseCIServer(t, "GraphQL: GitHub Actions is not permitted to create or approve pull requests", &hits)
	logger, buf := captureLogger()
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	kicker := &fakeKicker{}
	notifier := notify.New(config.NotificationsConfig{}, logger)

	runReleaseSentinel(context.Background(), enabledSentinelConfig(), client, kicker, nil, notifier, logger)
	if len(kicker.kicks) != 0 {
		t.Fatalf("policy failure kicked an agent: %+v", kicker.kicks)
	}
	if !bytes.Contains(buf.Bytes(), []byte("escalated a release to a human")) || !bytes.Contains(buf.Bytes(), []byte("reason=policy")) {
		t.Fatalf("no policy escalation logged:\n%s", buf.String())
	}
}

func TestRunReleaseSentinel_ErrorsAreLogged(t *testing.T) {
	isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	logger, buf := captureLogger()

	// No usable repo.
	client := github.NewClientForTest("http://127.0.0.1:1", "", nil, logger)
	runReleaseSentinel(context.Background(), &config.Config{ReleaseSentinel: config.ReleaseSentinelConfig{Enabled: true}}, client, &fakeKicker{}, nil, nil, logger)
	if !bytes.Contains(buf.Bytes(), []byte("no owner/repo to watch")) {
		t.Fatalf("missing repo not logged:\n%s", buf.String())
	}

	// API failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	releaseSentinelLastRun = time.Time{}
	cfg := enabledSentinelConfig()
	cfg.ReleaseSentinel.PollInterval = "1m"
	runReleaseSentinel(context.Background(), cfg, github.NewClientForTest(srv.URL, "acme", nil, logger), &fakeKicker{}, nil, nil, logger)
	if !bytes.Contains(buf.Bytes(), []byte("release sentinel pass failed")) {
		t.Fatalf("API failure not logged:\n%s", buf.String())
	}
}

func TestRunReleaseSentinel_NoTagIsQuiet(t *testing.T) {
	path := isolateReleaseSentinel(t)
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	logger, _ := captureLogger()
	runReleaseSentinel(context.Background(), enabledSentinelConfig(), github.NewClientForTest(srv.URL, "acme", nil, logger), &fakeKicker{}, nil, nil, logger)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a repo with no release tag wrote state (stat err: %v)", err)
	}
}

func TestReleaseSentinelDispatcher(t *testing.T) {
	req := releasesentinel.RepairRequest{Repo: "acme/widgets", Tag: "v1.0.0", SHA: releaseSentinelTestSHA, Agent: "ci-maintainer", Round: 1, MaxRounds: 5}
	if err := (releaseSentinelDispatcher{}).DispatchRepair(context.Background(), req); err == nil {
		t.Fatal("nil kicker delivered")
	}
	k := &fakeKicker{}
	d := releaseSentinelDispatcher{kicker: k, available: func(string) bool { return false }}
	if err := d.DispatchRepair(context.Background(), req); err == nil || len(k.kicks) != 0 {
		t.Fatalf("unavailable agent: err=%v kicks=%d", err, len(k.kicks))
	}
	k.err = errors.New("pane gone")
	d.available = nil
	if err := d.DispatchRepair(context.Background(), req); !errors.Is(err, k.err) {
		t.Fatalf("kick error not surfaced: %v", err)
	}
}

func TestReleaseSentinelEscalator_NilSafe(t *testing.T) {
	// Neither a nil logger nor a nil notifier may panic: escalation must
	// never be the thing that breaks the eval tick.
	releaseSentinelEscalator{}.Escalate(context.Background(), releasesentinel.Escalation{Reason: releasesentinel.EscalationRoundCap})
}
