package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// doneVerdictServer serves one issue o/r#N with labels and a label-event
// history, and records label removals.
type doneVerdictServer struct {
	mu      sync.Mutex
	labels  []string
	events  []map[string]any
	removed []string
	added   []string
}

func (d *doneVerdictServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			_ = json.NewEncoder(w).Encode(d.events)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "x"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode(wireLabelsOf(d.labels))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1023, "state": "open", "labels": wireLabelsOf(d.labels), "user": map[string]any{"login": "castrojo"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			d.added = append(d.added, body...)
			d.labels = append(d.labels, body...)
			_ = json.NewEncoder(w).Encode(wireLabelsOf(d.labels))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/labels/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/labels/")+len("/labels/"):]
			d.removed = append(d.removed, name)
			d.labels = removeIssueLabel(d.labels, name)
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func labeledEvent(label, at string) map[string]any {
	return map[string]any{"event": "labeled", "created_at": at, "label": map[string]any{"name": label}}
}

// projectbluefin/testsuite#1023: hive/already-done (11-04 01:47Z), then
// hive/verified-open (03:13Z). The later verdict stands.
func TestReconcileDoneVerdictLabelsKeepsTheLaterVerdict(t *testing.T) {
	both := []string{"kind/bug", AlreadyDoneLabel, VerifiedOpenLabel, "triage/accepted"}
	for _, tc := range []struct {
		name    string
		events  []map[string]any
		labels  []string
		removed string
	}{
		{name: "verified-open later", labels: both, events: []map[string]any{labeledEvent(AlreadyDoneLabel, "2026-11-04T01:47:00Z"), labeledEvent(VerifiedOpenLabel, "2026-11-04T03:13:00Z")}, removed: AlreadyDoneLabel},
		{name: "already-done later", labels: both, events: []map[string]any{labeledEvent(VerifiedOpenLabel, "2026-11-04T01:47:00Z"), labeledEvent(AlreadyDoneLabel, "2026-11-04T03:13:00Z")}, removed: VerifiedOpenLabel},
		{name: "re-added verified-open counts its latest add", labels: both, events: []map[string]any{labeledEvent(VerifiedOpenLabel, "2026-11-01T00:00:00Z"), labeledEvent(AlreadyDoneLabel, "2026-11-02T00:00:00Z"), labeledEvent(VerifiedOpenLabel, "2026-11-03T00:00:00Z")}, removed: AlreadyDoneLabel},
		{name: "re-added already-done counts its latest add", labels: both, events: []map[string]any{labeledEvent(AlreadyDoneLabel, "2026-11-01T00:00:00Z"), labeledEvent(VerifiedOpenLabel, "2026-11-02T00:00:00Z"), labeledEvent(AlreadyDoneLabel, "2026-11-03T00:00:00Z")}, removed: VerifiedOpenLabel},
		{name: "an add missing from the events changes nothing", labels: both, events: []map[string]any{labeledEvent(VerifiedOpenLabel, "2026-11-04T03:13:00Z")}},
		{name: "only one verdict worn: no read, no write", labels: []string{AlreadyDoneLabel}, events: []map[string]any{labeledEvent(AlreadyDoneLabel, "2026-11-04T01:47:00Z")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &doneVerdictServer{labels: append([]string(nil), tc.labels...), events: tc.events}
			c := testClient(t, d.start(t).URL)
			changed := reconcileDoneVerdictLabels(context.Background(), c, IssueStageInput{Repo: "o/r", Number: 1023, Labels: tc.labels}, nil)
			d.mu.Lock()
			defer d.mu.Unlock()
			if got := strings.Join(d.removed, ","); got != tc.removed || changed != (tc.removed != "") {
				t.Fatalf("removed = %q (changed=%v), want %q", got, changed, tc.removed)
			}
		})
	}
}

// The agent's verified-open verdict takes the done labels off as it lands.
func TestLabelRelayVerifiedOpenRemovesDoneLabels(t *testing.T) {
	d := &doneVerdictServer{labels: []string{"kind/bug", AlreadyDoneLabel, LikelyDoneLabel}}
	c := issueTestClient(t, d.start(t).URL)
	dir := withIssueDir(t)
	reqPath, err := WriteIssueRequest(dir, IssueRequest{Kind: "label", Repo: "o/r", Number: 1023, Agent: "scanner", Labels: []string{VerifiedOpenLabel}})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Fatalf("result = %+v, want ok", resp)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Join(d.added, ",") != VerifiedOpenLabel {
		t.Fatalf("added = %v", d.added)
	}
	if got := strings.Join(d.removed, ","); got != AlreadyDoneLabel+","+LikelyDoneLabel {
		t.Fatalf("removed = %q, want already-done and likely-done", got)
	}
}

// The stage sweep runs the reconcile on every issue it walks.
func TestSyncIssueStageLabelsSettlesDoneVerdictPair(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	fake.events = map[int][]map[string]any{1023: {labeledEvent(AlreadyDoneLabel, "2026-11-04T01:47:00Z"), labeledEvent(VerifiedOpenLabel, "2026-11-04T03:13:00Z")}}
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 1023, Labels: []string{AlreadyDoneLabel, VerifiedOpenLabel, "triage/accepted"}}, workBucketFiltered, "already done")
	syncSeeded(t, fake, c, result, stagelabels.NewStore("", stageNow), stageNow())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !issueHasLabel(fake.removed[1023], AlreadyDoneLabel) || issueHasLabel(fake.removed[1023], VerifiedOpenLabel) {
		t.Fatalf("removed = %v, want hive/already-done only", fake.removed[1023])
	}
}
