package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

func TestEnsureFleetReportCreatesWhenNoFingerprintMatch(t *testing.T) {
	var creates atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/labels":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "fleet-report"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues":
			creates.Add(1)
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "hive-fleet-fingerprint:fp") {
				t.Fatalf("create body missing fingerprint: %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "html_url": "https://github.com/hivecommons/hive/issues/9", "user": map[string]any{"login": "bot"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	// The create runs with open-twin consolidation on; a report body cites
	// no files, so there is nothing to fold into and it stays a plain create.
	c.appBotLogin = "hive-app[bot]"
	res, err := c.EnsureFleetReport(context.Background(), fleetreport.Report{Fingerprint: "fp", Title: "Fleet report", Body: "<!-- hive-fleet-fingerprint:fp -->\nbody", Labels: []string{"fleet-report"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Number != 9 || creates.Load() != 1 {
		t.Fatalf("result = %#v creates=%d", res, creates.Load())
	}
}

func TestEnsureFleetReportCommentsAndReactsOnFingerprintMatch(t *testing.T) {
	var comments, reactions atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"number": 7, "html_url": "https://github.com/hivecommons/hive/issues/7"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/7/comments":
			comments.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/7/reactions":
			reactions.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	res, err := c.EnsureFleetReport(context.Background(), fleetreport.Report{Fingerprint: "fp", Body: "evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Commented || !res.ReactionSent || comments.Load() != 1 || reactions.Load() != 1 {
		t.Fatalf("result=%#v comments=%d reactions=%d", res, comments.Load(), reactions.Load())
	}
}

func TestPostFleetReportRecoveryClosesOnlyWhenOwner(t *testing.T) {
	var patches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/5/comments":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/issues/5":
			patches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 5})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	if err := c.PostFleetReportRecovery(context.Background(), 5, fleetreport.Report{Body: "recovered"}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.PostFleetReportRecovery(context.Background(), 5, fleetreport.Report{Body: "recovered"}, true); err != nil {
		t.Fatal(err)
	}
	if patches.Load() != 1 {
		t.Fatalf("patches=%d, want 1", patches.Load())
	}
}

func TestEnsureFleetReportRejectsMissingFingerprint(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:1", "hivecommons", nil, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.EnsureFleetReport(ctx, fleetreport.Report{}); err == nil {
		t.Fatal("expected missing fingerprint error")
	}
}

// selfReportFakeGitHub is a scripted GitHub for the join-first self-report
// path (#11868). Search queries are answered in order of what they ask for:
// the fingerprint marker, the hive/selfreport+class label query, then the
// class-keyword query; each can be told to hit or miss.
type selfReportFakeGitHub struct {
	t                *testing.T
	fingerprintHit   *map[string]any
	labelHit         *map[string]any
	keywordHit       *map[string]any
	searches         []string
	comments         []string
	labelsAdded      []string
	labelsCreated    []string
	bodyPatches      []string
	creates          atomic.Int64
	reactions        atomic.Int64
	existingLabels   []string
	refuseWithStatus int
}

func (f *selfReportFakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.refuseWithStatus != 0 && strings.HasPrefix(r.URL.Path, "/repos/") {
			w.WriteHeader(f.refuseWithStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
			return
		}
		switch {
		case r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			f.searches = append(f.searches, q)
			var hit *map[string]any
			switch {
			case strings.Contains(q, "hive-fleet-fingerprint:"):
				hit = f.fingerprintHit
			case strings.Contains(q, "label:hive/selfreport"):
				hit = f.labelHit
			case strings.Contains(q, "in:title,body"):
				hit = f.keywordHit
			}
			items := []map[string]any{}
			if hit != nil {
				items = append(items, *hit)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/hivecommons/hive/labels/"):
			name := strings.TrimPrefix(r.URL.Path, "/repos/hivecommons/hive/labels/")
			for _, l := range f.existingLabels {
				if l == name {
					_ = json.NewEncoder(w).Encode(map[string]any{"name": name})
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/labels":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.labelsCreated = append(f.labelsCreated, body["name"].(string))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var names []string
			_ = json.NewDecoder(r.Body).Decode(&names)
			f.labelsAdded = append(f.labelsAdded, names...)
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			raw, _ := io.ReadAll(r.Body)
			f.comments = append(f.comments, string(raw))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reactions"):
			f.reactions.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/hivecommons/hive/issues/"):
			raw, _ := io.ReadAll(r.Body)
			f.bodyPatches = append(f.bodyPatches, string(raw))
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues":
			f.creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 99, "html_url": "https://github.com/hivecommons/hive/issues/99", "user": map[string]any{"login": "bot"}})
		default:
			f.t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	})
}

func selfReportTestReport(fp string) fleetreport.Report {
	return fleetreport.Report{
		Fingerprint: fp, Class: fleetreport.ClassRestartStorm, Trigger: fleetreport.TriggerRuntimeError,
		Title: "[selfreport] restart_storm: agent restarted N times", Body: "<!-- hive-fleet-fingerprint:" + fp + " -->\nbody",
		Labels:   []string{"fleet-report", fleetreport.LabelSelfReport},
		Evidence: []fleetreport.Evidence{{Class: fleetreport.ClassRestartStorm, Count: 4, Message: "restart storm"}},
	}
}

func selfReportTestOptions() FleetReportOptions {
	return FleetReportOptions{
		Repo: "hivecommons/hive", HiveSlug: "hosted-aslom", Version: "v5.1.0", Cluster: "oke-11",
		Now: func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }, JoinExisting: true, SeenCommentAllowed: true,
	}
}

func TestEnsureFleetReportWithOptionsJoinsViaLabelAndClass(t *testing.T) {
	human := map[string]any{"number": 7, "state": "open", "html_url": "https://github.com/hivecommons/hive/issues/7", "body": "Restart storm on hosted hives after upgrade", "user": map[string]any{"login": "maintainer"}}
	f := &selfReportFakeGitHub{t: t, labelHit: &human}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	res, err := c.EnsureFleetReportWithOptions(context.Background(), selfReportTestReport("fp-join"), selfReportTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || !res.Joined || !res.Commented || res.Number != 7 || f.creates.Load() != 0 {
		t.Fatalf("result = %#v creates=%d", res, f.creates.Load())
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "hive:selfreport-seen hive=hosted-aslom fp=fp-join") || !strings.Contains(f.comments[0], "Also seen on **hosted-aslom** (v5.1.0, oke-11)") {
		t.Fatalf("comments = %q", f.comments)
	}
	if f.reactions.Load() != 1 {
		t.Fatalf("reactions = %d", f.reactions.Load())
	}
	joinedLabels := strings.Join(f.labelsAdded, ",")
	if !strings.Contains(joinedLabels, "hive/hosted-aslom") || !strings.Contains(joinedLabels, fleetreport.LabelSelfReport) || !res.LabelAdded {
		t.Fatalf("labels added = %v (res=%#v)", f.labelsAdded, res)
	}
	if len(f.labelsCreated) == 0 || !strings.Contains(strings.Join(f.labelsCreated, ","), "hive/hosted-aslom") {
		t.Fatalf("hive label not created when missing: %v", f.labelsCreated)
	}
	if len(f.bodyPatches) != 1 || !strings.Contains(f.bodyPatches[0], "Affected hives (1):** hosted-aslom") || !strings.Contains(f.bodyPatches[0], "Restart storm on hosted hives after upgrade") || !res.BodyUpdated {
		t.Fatalf("body patches = %q", f.bodyPatches)
	}
	if len(f.searches) != 2 {
		t.Fatalf("expected fingerprint + label searches, got %q", f.searches)
	}
}

func TestEnsureFleetReportWithOptionsJoinsHumanIssueViaKeywords(t *testing.T) {
	human := map[string]any{"number": 8, "state": "open", "html_url": "https://github.com/hivecommons/hive/issues/8", "body": "Seeing a restart storm on my hive", "user": map[string]any{"login": "maintainer"}}
	f := &selfReportFakeGitHub{t: t, keywordHit: &human}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	res, err := c.EnsureFleetReportWithOptions(context.Background(), selfReportTestReport("fp-kw"), selfReportTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Joined || res.Number != 8 || f.creates.Load() != 0 {
		t.Fatalf("result = %#v creates=%d", res, f.creates.Load())
	}
	if len(f.searches) != 3 || !strings.Contains(f.searches[2], `"restart storm"`) {
		t.Fatalf("searches = %q", f.searches)
	}
}

func TestEnsureFleetReportWithOptionsSkipsSeenCommentInsideGapButStillLabels(t *testing.T) {
	mine := map[string]any{"number": 7, "state": "open", "html_url": "https://github.com/hivecommons/hive/issues/7", "body": "<!-- hive-fleet-fingerprint:fp-x -->\nbody\n" + fleetreport.RenderAffectedBlock([]string{"other-hive"}, 1), "user": map[string]any{"login": "bot"}}
	f := &selfReportFakeGitHub{t: t, fingerprintHit: &mine, existingLabels: []string{"hive/hosted-aslom", fleetreport.LabelSelfReport}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	opts := selfReportTestOptions()
	opts.SeenCommentAllowed = false
	res, err := c.EnsureFleetReportWithOptions(context.Background(), selfReportTestReport("fp-x"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commented || len(f.comments) != 0 {
		t.Fatalf("comment posted inside the seen gap: %#v %q", res, f.comments)
	}
	if len(f.labelsCreated) != 0 {
		t.Fatalf("existing labels re-created: %v", f.labelsCreated)
	}
	if len(f.bodyPatches) != 1 || !strings.Contains(f.bodyPatches[0], "Affected hives (2):** hosted-aslom, other-hive") {
		t.Fatalf("body patches = %q", f.bodyPatches)
	}
}

func TestEnsureFleetReportWithOptionsClosedFingerprintWritesNothing(t *testing.T) {
	closed := map[string]any{"number": 5, "state": "closed", "closed_at": "2026-09-23T10:00:00Z", "html_url": "https://github.com/hivecommons/hive/issues/5", "body": "<!-- hive-fleet-fingerprint:fp-closed -->", "user": map[string]any{"login": "bot"}}
	f := &selfReportFakeGitHub{t: t, fingerprintHit: &closed}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	res, err := c.EnsureFleetReportWithOptions(context.Background(), selfReportTestReport("fp-closed"), selfReportTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if res.ClosedAt.IsZero() || res.Number != 5 || res.Created || res.Joined || res.Commented || f.creates.Load() != 0 || len(f.comments) != 0 || len(f.bodyPatches) != 0 {
		t.Fatalf("closed fingerprint wrote upstream: res=%#v comments=%q patches=%q", res, f.comments, f.bodyPatches)
	}
}

func TestEnsureFleetReportWithOptionsCreateGateBlocksOnlyCreates(t *testing.T) {
	f := &selfReportFakeGitHub{t: t}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	c.appBotLogin = "hive-app[bot]"
	gateErr := errors.New("deferred")
	opts := selfReportTestOptions()
	opts.CreateGate = func() error { return gateErr }
	_, err := c.EnsureFleetReportWithOptions(context.Background(), selfReportTestReport("fp-new"), opts)
	if !errors.Is(err, gateErr) || f.creates.Load() != 0 {
		t.Fatalf("gate not honoured: err=%v creates=%d", err, f.creates.Load())
	}
	opts.CreateGate = nil
	res, err := c.EnsureFleetReportWithOptions(context.Background(), selfReportTestReport("fp-new"), opts)
	if err != nil || !res.Created || res.Number != 99 || f.creates.Load() != 1 {
		t.Fatalf("create without gate: res=%#v err=%v", res, err)
	}
}

func TestEnsureFleetReportWithOptionsSurfacesForbiddenForRelay(t *testing.T) {
	f := &selfReportFakeGitHub{t: t, refuseWithStatus: http.StatusForbidden}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	_, err := c.EnsureFleetReportWithOptions(context.Background(), selfReportTestReport("fp-403"), selfReportTestOptions())
	var ghErr *gh.ErrorResponse
	if err == nil || !errors.As(err, &ghErr) || ghErr.Response.StatusCode != http.StatusForbidden {
		t.Fatalf("expected a 403 ErrorResponse for the hub-relay fallback, got %v", err)
	}
}

func TestPostFleetReportClearedCommentsOnConfiguredRepo(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	if err := c.PostFleetReportCleared(context.Background(), "acme/fork", 12, "Cleared on **h**"); err != nil || path != "POST /repos/acme/fork/issues/12/comments" {
		t.Fatalf("err=%v path=%q", err, path)
	}
	if err := c.PostFleetReportCleared(context.Background(), "", 13, "Cleared"); err != nil || path != "POST /repos/hivecommons/hive/issues/13/comments" {
		t.Fatalf("default repo: err=%v path=%q", err, path)
	}
	var nilClient *Client
	if err := nilClient.PostFleetReportCleared(context.Background(), "", 1, "x"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client err=%v", err)
	}
}
