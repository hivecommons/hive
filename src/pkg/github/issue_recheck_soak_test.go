package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/gfi"
)

// soakIssueBody is shaped like hivecommons/hive#13267.
const soakIssueBody = "Tracking post-hoc soak and smoke evidence for the `promote-stable.yml` emergency promotion of candidate `sha256:0cd6229ac1db1102b12268e41bede470c9802b9b8964b5053c84115bbbcb2ff2` (v5 `7d1b5a3ae`, includes #13230 and #13247) over the previous stable `sha256:c60c98014d952a377612b9cb65c4eb58ac80ebd43db8fbe2164e98a9b55a1831`.\n\n" +
	"Checks that passed before promotion: v5 docker.yml run 39482742988.\n\n" +
	"To close:\n" +
	"- [ ] 24h after promotion: hub dashboard shows hosted stable hives healthy on the new digest (no restart loops, heartbeat OK)\n" +
	"- [ ] Spot-check one stable spoke: `/api/version` matches `7d1b5a3ae`\n" +
	"- [ ] Record the evidence here and in the promote-stable run summary\n"

func TestParseSoakCandidate(t *testing.T) {
	c := parseSoakCandidate(soakIssueBody)
	if c.Digest != "sha256:0cd6229ac1db1102b12268e41bede470c9802b9b8964b5053c84115bbbcb2ff2" || c.ShortSHA != "7d1b5a3ae" {
		t.Fatalf("candidate = %+v", c)
	}
	if c := parseSoakCandidate("nothing here"); c.known() {
		t.Fatalf("bare body candidate = %+v", c)
	}
	if c := parseSoakCandidate("image sha256:abcdef0123456789 rolled"); c.Digest != "sha256:abcdef0123456789" || c.ShortSHA != "" {
		t.Fatalf("fallback digest = %+v", c)
	}
}

func TestEvaluateAndTickSoakChecklist(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cand := parseSoakCandidate(soakIssueBody)
	fresh := now.Add(-time.Minute)
	healthy := []SoakHive{
		{Channel: "stable", SHA: "7d1b5a3ae0123", LastHeartbeat: fresh},
		{Channel: "stable", Digest: cand.Digest, LastHeartbeat: fresh, Restarts: 1},
		{Channel: "edge", SHA: "deadbeef00", LastHeartbeat: now.Add(-time.Hour), RestartLoop: true},
	}
	ev := evaluateSoak("hub", cand, healthy, now)
	if ev.Stable != 2 || ev.Healthy != 2 || ev.OnCand != 2 || !ev.FleetHealthy || !ev.VersionMatched {
		t.Fatalf("healthy evidence = %+v", ev)
	}
	body, ticked := tickSoakChecklist(soakIssueBody, ev)
	if ticked != 2 || !strings.Contains(body, "- [x] 24h after promotion") || !strings.Contains(body, "- [x] Spot-check one stable spoke") || !strings.Contains(body, "- [ ] Record the evidence") {
		t.Fatalf("ticked=%d body=\n%s", ticked, body)
	}

	// One stable hive in a restart loop: the version line is still proved,
	// the fleet-health line is not.
	looping := append([]SoakHive{{Channel: "stable", SHA: "7d1b5a3ae", LastHeartbeat: fresh, RestartLoop: true}}, healthy...)
	ev = evaluateSoak("hub", cand, looping, now)
	if ev.FleetHealthy || !ev.VersionMatched {
		t.Fatalf("looping evidence = %+v", ev)
	}
	body, ticked = tickSoakChecklist(soakIssueBody, ev)
	if ticked != 1 || !strings.Contains(body, "- [ ] 24h after promotion") || !strings.Contains(body, "- [x] Spot-check") {
		t.Fatalf("looping ticked=%d body=\n%s", ticked, body)
	}

	// Stale heartbeat and the old digest prove nothing.
	stale := []SoakHive{{Channel: "Stable", Digest: "sha256:c60c98014d952a377612b9cb65c4eb58ac80ebd43db8fbe2164e98a9b55a1831", LastHeartbeat: now.Add(-time.Hour)}}
	ev = evaluateSoak("hub", cand, stale, now)
	if ev.Stable != 1 || ev.Healthy != 0 || ev.OnCand != 0 || ev.FleetHealthy || ev.VersionMatched {
		t.Fatalf("stale evidence = %+v", ev)
	}
	if _, ticked := tickSoakChecklist(soakIssueBody, ev); ticked != 0 {
		t.Fatalf("stale ticked %d", ticked)
	}
	// No stable hive at all proves nothing either.
	if ev := evaluateSoak("hub", cand, nil, now); ev.FleetHealthy || ev.VersionMatched {
		t.Fatalf("empty evidence = %+v", ev)
	}
}

func TestHTTPSoakProbe(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/hives" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"hives":[{"channel":"stable","git_sha":"7d1b5a3ae","last_heartbeat":"2026-10-22T11:59:00Z","restart_count":4}]}`)
	}))
	defer hub.Close()
	source, hives, err := NewHTTPSoakProbe(hub.URL, "", hub.Client())(context.Background())
	if err != nil || source != "hub /api/hub/hives" || len(hives) != 1 {
		t.Fatalf("hub probe = %q %+v %v", source, hives, err)
	}
	if h := hives[0]; h.SHA != "7d1b5a3ae" || h.Restarts != 4 || !h.LastHeartbeat.Equal(time.Date(2026, 10, 22, 11, 59, 0, 0, time.UTC)) {
		t.Fatalf("hub hive = %+v", h)
	}

	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"hash":"7d1b5a3ae0","short":"7d1b5a3","channel":"stable","imageRef":"ghcr.io/hivecommons/hive@sha256:0cd6229ac1db1102b12268e41bede470c9802b9b8964b5053c84115bbbcb2ff2"}`)
	}))
	defer spoke.Close()
	source, hives, err = NewHTTPSoakProbe("", spoke.URL, spoke.Client())(context.Background())
	if err != nil || source != "spoke /api/version" || len(hives) != 1 {
		t.Fatalf("spoke probe = %q %+v %v", source, hives, err)
	}
	if h := hives[0]; h.SHA != "7d1b5a3ae0" || h.Channel != "stable" || !strings.HasPrefix(h.Digest, "sha256:0cd6229a") || !h.LastHeartbeat.IsZero() {
		t.Fatalf("spoke hive = %+v", h)
	}

	if _, _, err := NewHTTPSoakProbe(hub.URL+"/missing", "", hub.Client())(context.Background()); err == nil {
		t.Fatal("404 hub probe returned no error")
	}
	if _, _, err := NewHTTPSoakProbe("", "", nil)(context.Background()); err == nil {
		t.Fatal("unconfigured probe returned no error")
	}
}

// An ops-tracking row on the hive's own project whose deadline has passed
// gets ONE evidence comment, the provable checklist lines ticked and the
// recheck-done marker; `blocked` stays on. Re-runs do not post again.
func TestReconcileBlockedIssuesRecheckSoak(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prevNow, prevRepo, prevProbe := blockedSweepNow, recheckSoakRepo, recheckSoakProbe
	t.Cleanup(func() { blockedSweepNow, recheckSoakRepo, recheckSoakProbe = prevNow, prevRepo, prevProbe })
	blockedSweepNow = func() time.Time { return now }
	recheckSoakRepo = "o/r"

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"channel":"stable","sha":"7d1b5a3ae","last_heartbeat":%q},{"channel":"edge","sha":"0000000"}]`, now.Add(-2*time.Minute).Format(time.RFC3339))
	}))
	defer hub.Close()
	SetRecheckSoakProbe(NewHTTPSoakProbe(hub.URL, "", hub.Client()))

	past := now.Add(-time.Hour)
	deadlineBody := soakIssueBody + "\n" + gfi.OpsTrackingMarker + "\n" + gfi.RecheckAfterMarker(past)
	var mu sync.Mutex
	var calls []string
	bodies := map[int]string{70: deadlineBody, 71: deadlineBody}
	comments := map[int][]string{71: {recheckEvidenceMarkerPrefix + " at=2026-10-22T11:00:00Z -->\nearlier evidence"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		var n int
		_, _ = fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/"), "%d", &n)
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			out := []map[string]any{}
			for i, b := range comments[n] {
				out = append(out, map[string]any{"id": i + 1, "body": b})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			comments[n] = append(comments[n], payload.Body)
			_, _ = io.WriteString(w, `{"id":9}`)
		case r.Method == "GET" && r.URL.Path == fmt.Sprintf("/repos/o/r/issues/%d", n):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "open", "body": bodies[n]})
		case r.Method == "PATCH" && r.URL.Path == fmt.Sprintf("/repos/o/r/issues/%d", n):
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			bodies[n] = payload.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "body": payload.Body})
		case r.Method == "DELETE":
			_, _ = io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	reason := "blocked until " + past.Format(time.RFC3339)
	rows := func() []RepoWorkIssue {
		mu.Lock()
		defer mu.Unlock()
		out := []RepoWorkIssue{}
		for _, n := range []int{70, 71} {
			out = append(out, RepoWorkIssue{Repo: "o/r", Number: n, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: reason, RecheckAfter: past, OpsTracking: true, RecheckDone: blockedRecheckDone(bodies[n])})
		}
		// Not the hive's own project: left alone as before.
		return append(out, RepoWorkIssue{Repo: "x/y", Number: 72, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: reason, RecheckAfter: past, OpsTracking: true})
	}
	result := &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: rows()}}}
	res := ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.SoakTriaged != 2 || res.Unblocked != 0 || res.Errors != 0 {
		t.Fatalf("result = %+v", res)
	}
	// The next enumeration reads the stamped bodies; nothing more happens.
	result = &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: rows()}}}
	if res := ReconcileBlockedIssues(context.Background(), c, result, nil); res.SoakTriaged != 0 || res.Errors != 0 {
		t.Fatalf("second pass result = %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, "\n")
	if n := strings.Count(joined, "POST /repos/o/r/issues/70/comments"); n != 1 {
		t.Errorf("issue 70 evidence posted %d times, want 1:\n%s", n, joined)
	}
	if strings.Contains(joined, "POST /repos/o/r/issues/71/comments") {
		t.Errorf("issue 71 already had evidence; posted again:\n%s", joined)
	}
	for _, never := range []string{"DELETE /repos/o/r/issues/70/labels/blocked", "DELETE /repos/o/r/issues/71/labels/blocked", "/repos/x/y/issues/72/labels", "PATCH /repos/x/y/"} {
		if strings.Contains(joined, never) {
			t.Errorf("unexpected call %q in:\n%s", never, joined)
		}
	}
	ev := comments[70]
	if len(ev) != 1 || !strings.HasPrefix(ev[0], recheckEvidenceMarkerPrefix+" at="+now.Format(time.RFC3339)+" -->") || !strings.Contains(ev[0], "Running the candidate: 1/1") {
		t.Errorf("issue 70 evidence = %q", ev)
	}
	for _, n := range []int{70, 71} {
		b := bodies[n]
		if strings.Count(b, RecheckDoneMarker) != 1 || !strings.Contains(b, "- [x] 24h after promotion") || !strings.Contains(b, "- [x] Spot-check") || !strings.Contains(b, "- [ ] Record the evidence") {
			t.Errorf("issue %d body =\n%s", n, b)
		}
	}
}

// A probe failure posts nothing and leaves the body unstamped so the next
// cycle retries.
func TestReconcileBlockedIssuesRecheckSoakProbeFailure(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prevNow, prevRepo, prevProbe := blockedSweepNow, recheckSoakRepo, recheckSoakProbe
	t.Cleanup(func() { blockedSweepNow, recheckSoakRepo, recheckSoakProbe = prevNow, prevRepo, prevProbe })
	blockedSweepNow = func() time.Time { return now }
	recheckSoakRepo = "o/r"
	SetRecheckSoakProbe(func(context.Context) (string, []SoakHive, error) {
		return "hub /api/hub/hives", nil, fmt.Errorf("hub down")
	})
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 80, "body": soakIssueBody})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	past := now.Add(-time.Minute)
	result := &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: []RepoWorkIssue{
		{Repo: "o/r", Number: 80, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked until " + past.Format(time.RFC3339), RecheckAfter: past, OpsTracking: true},
	}}}}
	res := ReconcileBlockedIssues(context.Background(), testClient(t, srv.URL), result, nil)
	if res.SoakTriaged != 0 || res.Errors != 1 {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, call := range calls {
		if !strings.HasPrefix(call, "GET ") {
			t.Errorf("unexpected write %q", call)
		}
	}
}
