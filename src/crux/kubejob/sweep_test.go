package kubejob

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sweepAPI serves a fixed Job list and records deletes.
type sweepAPI struct {
	mu       sync.Mutex
	list     string
	listErr  int
	selector string
	deleted  []string
	delErr   map[string]bool
}

func (f *sweepAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
			if f.listErr > 0 {
				w.WriteHeader(f.listErr)
				_, _ = w.Write([]byte(`{"message":"jobs.batch is forbidden"}`))
				return
			}
			f.selector = r.URL.Query().Get("labelSelector")
			_, _ = w.Write([]byte(f.list))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/jobs/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if f.delErr[name] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.deleted = append(f.deleted, name)
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Success"}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/networkpolicies/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/secrets/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

type sweepItem struct {
	name, agent string
	managed     bool
	age         time.Duration
	ready       int
	finished    string
}

func sweepList(t *testing.T, now time.Time, items ...sweepItem) string {
	t.Helper()
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		labels := map[string]string{agentLabel: it.agent}
		if it.managed {
			labels[managedByLabel] = managedByValue
		}
		status := map[string]any{"ready": it.ready}
		if it.finished != "" {
			status["conditions"] = []map[string]string{{"type": it.finished, "status": condStatusTrue}}
		}
		out = append(out, map[string]any{
			"metadata": map[string]any{
				"name":              it.name,
				"creationTimestamp": now.Add(-it.age).UTC().Format(time.RFC3339),
				"labels":            labels,
			},
			"status": status,
		})
	}
	raw, err := json.Marshal(map[string]any{"items": out})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSweepOrphansDeletesOrphansOnly(t *testing.T) {
	const kickTimeout = 45 * time.Minute
	now := time.Unix(1700000000, 0)
	api := &sweepAPI{}
	api.list = sweepList(t, now,
		// Pending for 11 days: the incident this sweep exists for.
		sweepItem{name: "hive-kick-reviewer-1", agent: "hive-reviewer-sandbox", managed: true, age: 11 * 24 * time.Hour},
		// Running, older than the timeout, no live kick: an orphan.
		sweepItem{name: "hive-kick-deps-2", agent: "hive-deps-sandbox", managed: true, age: time.Hour, ready: 1},
		// An orphaned publish preflight.
		sweepItem{name: "hive-kick-deps-3", agent: "hive-deps-verify", managed: true, age: 2 * time.Hour, ready: 1},
		// Young: may still belong to a kick that is about to be re-adopted.
		sweepItem{name: "hive-kick-deps-4", agent: "hive-deps-sandbox", managed: true, age: 10 * time.Minute},
		// Owned by a live kick.
		sweepItem{name: "hive-kick-live-5", agent: "hive-live-sandbox", managed: true, age: time.Hour, ready: 1},
		// Finished: left to its TTL for inspection.
		sweepItem{name: "hive-kick-done-6", agent: "hive-done-sandbox", managed: true, age: 2 * time.Hour, finished: condFailed},
		// No managed-by label: never ours to touch.
		sweepItem{name: "someone-else-7", agent: "hive-other-sandbox", managed: false, age: 30 * 24 * time.Hour},
		// A contained run: belongs to the runs service.
		sweepItem{name: "hive-kick-crux-run-8", agent: "crux-run-abc", managed: true, age: 30 * 24 * time.Hour},
	)
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	l.Now = func() time.Time { return now }

	swept, err := l.SweepOrphans(context.Background(), SweepOptions{
		MaxAge: kickTimeout,
		Live:   func(agent string) bool { return agent == "hive-live-sandbox" },
	})
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	want := map[string]string{
		"hive-kick-reviewer-1": SweepReasonPendingPastDeadline,
		"hive-kick-deps-2":     SweepReasonOrphaned,
		"hive-kick-deps-3":     SweepReasonOrphaned,
	}
	if len(swept) != len(want) {
		t.Fatalf("swept = %+v, want %v", swept, want)
	}
	for _, s := range swept {
		if want[s.Name] != s.Reason {
			t.Errorf("swept %s with reason %q, want %q", s.Name, s.Reason, want[s.Name])
		}
	}
	if strings.Join(api.deleted, ",") != "hive-kick-reviewer-1,hive-kick-deps-2,hive-kick-deps-3" {
		t.Fatalf("deleted = %v", api.deleted)
	}
	if api.selector != managedByLabel+"="+managedByValue {
		t.Fatalf("labelSelector = %q", api.selector)
	}
}

func TestSweepOrphansPendingPastDeadlineEvenIfLive(t *testing.T) {
	now := time.Unix(1700000000, 0)
	api := &sweepAPI{list: sweepList(t, now, sweepItem{name: "hive-kick-a-1", agent: "hive-a-sandbox", managed: true, age: 48 * time.Hour})}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	l.Now = func() time.Time { return now }
	swept, err := l.SweepOrphans(context.Background(), SweepOptions{Live: func(string) bool { return true }})
	if err != nil || len(swept) != 1 || swept[0].Reason != SweepReasonPendingPastDeadline {
		t.Fatalf("swept = %+v, err = %v", swept, err)
	}
}

func TestSweepOrphansReportsErrors(t *testing.T) {
	now := time.Unix(1700000000, 0)

	api := &sweepAPI{listErr: http.StatusForbidden}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	if _, err := l.SweepOrphans(context.Background(), SweepOptions{}); err == nil || !strings.Contains(err.Error(), "listing sandbox jobs") {
		t.Fatalf("list error = %v", err)
	}

	bad := &sweepAPI{list: "not json"}
	srvBad := httptest.NewServer(bad.handler(t))
	defer srvBad.Close()
	if _, err := newLauncher(srvBad, Options{}).SweepOrphans(context.Background(), SweepOptions{}); err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("decode error = %v", err)
	}

	del := &sweepAPI{
		list: sweepList(t, now,
			sweepItem{name: "hive-kick-a-1", agent: "hive-a-sandbox", managed: true, age: 48 * time.Hour},
			sweepItem{name: "hive-kick-b-2", agent: "hive-b-sandbox", managed: true, age: 48 * time.Hour},
		),
		delErr: map[string]bool{"hive-kick-a-1": true},
	}
	srvDel := httptest.NewServer(del.handler(t))
	defer srvDel.Close()
	ld := newLauncher(srvDel, Options{})
	ld.Now = func() time.Time { return now }
	swept, err := ld.SweepOrphans(context.Background(), SweepOptions{})
	if err == nil || !strings.Contains(err.Error(), "hive-kick-a-1") {
		t.Fatalf("delete error = %v", err)
	}
	if len(swept) != 1 || swept[0].Name != "hive-kick-b-2" {
		t.Fatalf("a failed delete must not stop the sweep: %+v", swept)
	}

	noNS := &Launcher{NamespacePath: "/nonexistent/namespace"}
	if _, err := noNS.SweepOrphans(context.Background(), SweepOptions{}); err == nil {
		t.Fatal("missing namespace must be an error")
	}
}

func TestActiveDeadlineSeconds(t *testing.T) {
	if got, want := ActiveDeadlineSeconds(0), int64((DefaultKickTimeout+DeadlineMargin)/time.Second); got != want {
		t.Fatalf("ActiveDeadlineSeconds(0) = %d, want %d", got, want)
	}
	if got, want := ActiveDeadlineSeconds(10*time.Minute), int64((10*time.Minute+DeadlineMargin)/time.Second); got != want {
		t.Fatalf("ActiveDeadlineSeconds(10m) = %d, want %d", got, want)
	}
}

func TestActiveDeadlinePrefersLaterOfConfigAndContext(t *testing.T) {
	l := &Launcher{Options: Options{ActiveDeadlineSeconds: 100}}
	if got := l.activeDeadline(context.Background()); got != 100 {
		t.Fatalf("configured deadline = %d, want 100", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if got := l.activeDeadline(ctx); got < int64((2*time.Hour)/time.Second) {
		t.Fatalf("context deadline not honoured: %d", got)
	}
	empty := &Launcher{}
	if got := empty.activeDeadline(context.Background()); got != ActiveDeadlineSeconds(DefaultKickTimeout) {
		t.Fatalf("default deadline = %d", got)
	}
}

// Every Job hive creates carries activeDeadlineSeconds, so one hive lost
// track of fails on its own (hivecommons/hive#18525).
func TestRunSetsActiveDeadlineOnJob(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	const deadline = 1234
	l := newLauncher(srv, Options{ActiveDeadlineSeconds: deadline})
	if _, err := l.Run(context.Background(), specFor("/data/agents/sandbox/deps-123")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	spec, _ := api.created["spec"].(map[string]any)
	if got, _ := spec["activeDeadlineSeconds"].(float64); got != deadline {
		t.Fatalf("activeDeadlineSeconds = %v, want %d", spec["activeDeadlineSeconds"], deadline)
	}

	m := (&Launcher{}).manifest("hive", "j", "sub", specFor("/data/x"))
	if got := m["spec"].(map[string]any)["activeDeadlineSeconds"]; got != ActiveDeadlineSeconds(DefaultKickTimeout) {
		t.Fatalf("default manifest deadline = %v", got)
	}
}
