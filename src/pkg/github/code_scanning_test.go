package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/codescanning"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/writeback"
)

// codeScanningServer is a GitHub stand-in for one repo, o/r: code scanning
// alerts, issues and labels.
type codeScanningServer struct {
	mu        sync.Mutex
	alerts    []map[string]any
	alertsErr int    // status for the alerts listing; 0 = 200
	alertsMsg string // error message with alertsErr
	issues    []map[string]any
	creates   []map[string]any
	edits     int
	comments  int
	posted    map[int][]string // comment bodies by issue number
	requests  int
	alertGets int
	labels    map[string]bool
}

func (s *codeScanningServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	if s.labels == nil {
		s.labels = map[string]bool{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests++
		w.Header().Set("Content-Type", "application/json")
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		p := r.URL.Path
		switch {
		case p == "/repos/o/r/code-scanning/alerts":
			s.alertGets++
			if s.alertsErr != 0 {
				w.WriteHeader(s.alertsErr)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": s.alertsMsg})
				return
			}
			if r.URL.Query().Get("state") != "open" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < 1 {
				page = 1
			}
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			lo, hi := (page-1)*per, page*per
			if lo > len(s.alerts) {
				lo = len(s.alerts)
			}
			if hi >= len(s.alerts) {
				hi = len(s.alerts)
			} else {
				w.Header().Set("Link", `<http://`+r.Host+p+`?state=open&per_page=`+strconv.Itoa(per)+`&page=`+strconv.Itoa(page+1)+`>; rel="next"`)
			}
			_ = json.NewEncoder(w).Encode(s.alerts[lo:hi])
		case p == "/repos/o/r" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "o/r", "fork": false})
		case strings.HasPrefix(p, "/repos/o/r/labels/"):
			name := strings.TrimPrefix(p, "/repos/o/r/labels/")
			if s.labels[name] {
				_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "color": "8250df"})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case p == "/repos/o/r/labels" && r.Method == http.MethodPost:
			s.labels[in["name"].(string)] = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(in)
		case p == "/repos/o/r/issues" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(s.issues)
		case p == "/repos/o/r/issues" && r.Method == http.MethodPost:
			n := 500 + len(s.creates)
			var labels []any
			for _, l := range in["labels"].([]any) {
				labels = append(labels, map[string]any{"name": l})
			}
			is := map[string]any{"number": n, "state": "open", "title": in["title"], "body": in["body"], "labels": labels, "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}}
			s.creates = append(s.creates, in)
			s.issues = append(s.issues, is)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(is)
		case strings.HasPrefix(p, "/repos/o/r/issues/") && strings.HasSuffix(p, "/comments") && r.Method == http.MethodPost:
			s.comments++
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, "/repos/o/r/issues/"), "/comments"))
			if s.posted == nil {
				s.posted = map[int][]string{}
			}
			body, _ := in["body"].(string)
			s.posted[n] = append(s.posted[n], body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		case strings.HasPrefix(p, "/repos/o/r/issues/") && strings.HasSuffix(p, "/comments") && r.Method == http.MethodGet:
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(p, "/repos/o/r/issues/"), "/comments"))
			out := []map[string]any{}
			for i, b := range s.posted[n] {
				out = append(out, map[string]any{"id": i + 1, "body": b, "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(p, "/repos/o/r/issues/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(p, "/repos/o/r/issues/"))
			for _, is := range s.issues {
				if is["number"] == n {
					if r.Method == http.MethodPatch {
						s.edits++
						for k, v := range in {
							is[k] = v
						}
					}
					_ = json.NewEncoder(w).Encode(is)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func codeScanningFixture(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile("../codescanning/testdata/exult-open-alerts.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func codeScanningConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "o"
	cfg.Project.Repos = []string{"r"}
	cfg.CodeScanning.Enabled = true
	return cfg
}

func newCodeScanningClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, testLogger())
	c.SetAppBotLogin("hive-app[bot]")
	return c
}

func TestCodeScanningSweepFilesGroupedIssuesThroughTheGateAndDedupes(t *testing.T) {
	resetRESTAccountingForTest(t, time.Now())
	s := &codeScanningServer{alerts: codeScanningFixture(t)}
	c := newCodeScanningClient(t, s.start(t))
	cfg := codeScanningConfig()
	now := time.Now()

	res := c.RunCodeScanningSweep(context.Background(), cfg, now)
	if res.Filed != codescanning.DefaultMaxNewIssues || len(s.creates) != codescanning.DefaultMaxNewIssues {
		t.Fatalf("first pass filed %d (%d creates), want the cap %d", res.Filed, len(s.creates), codescanning.DefaultMaxNewIssues)
	}
	first := s.creates[0]
	if !strings.HasPrefix(first["title"].(string), "[governor] Code scanning: ") {
		t.Fatalf("title %q lacks the filer tag", first["title"])
	}
	labels := first["labels"].([]any)
	if len(labels) != 2 || labels[0] != "origin/governor" || labels[1] != CodeScanningLabel {
		t.Fatalf("labels %v", labels)
	}
	if !strings.Contains(first["body"].(string), codescanning.MarkerPrefix) {
		t.Fatal("body lacks the dedupe marker")
	}
	st := c.CodeScanningStatus()
	if len(st.Repos) != 1 || st.Repos[0].Repo != "o/r" || st.Repos[0].BySeverity["critical"] != 11 || st.Repos[0].BySeverity["high"] != 73 {
		t.Fatalf("status %+v", st)
	}
	if st.Repos[0].SecurityURL != "https://github.com/o/r/security/code-scanning" || st.Repos[0].Tracked == 0 || st.Repos[0].Untracked == 0 {
		t.Fatalf("status counts %+v", st.Repos[0])
	}

	// Later passes (a restart reads the same markers back) never file a
	// duplicate, and settle at every qualifying alert tracked.
	for pass := 0; pass < 10 && res.Filed > 0; pass++ {
		res = c.RunCodeScanningSweep(context.Background(), cfg, now.Add(time.Duration(pass+1)*time.Hour))
	}
	seen := map[int]bool{}
	for _, is := range s.issues {
		m, _ := codescanning.ParseMarker(is["body"].(string))
		for _, n := range m.Alerts {
			if seen[n] {
				t.Fatalf("alert #%d filed twice", n)
			}
			seen[n] = true
		}
	}
	if len(seen) != 84 || c.CodeScanningStatus().Repos[0].Untracked != 0 {
		t.Fatalf("tracked %d alerts, untracked %d; want 84 and 0", len(seen), c.CodeScanningStatus().Repos[0].Untracked)
	}

	// The write-back gate stops every write when the repo is off.
	s.alerts = append(s.alerts, map[string]any{"number": 9001, "state": "open", "rule": map[string]any{"id": "cpp/new-rule", "security_severity_level": "critical"}, "tool": map[string]any{"name": "CodeQL"}, "most_recent_instance": map[string]any{"location": map[string]any{"path": "new.cc", "start_line": 3}}})
	c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"o/r": {Mode: writeback.RepoModeOff}}}, writeback.Deps{}, ""))
	before := len(s.creates)
	res = c.RunCodeScanningSweep(context.Background(), cfg, now.Add(20*time.Hour))
	if len(s.creates) != before || res.Filed != 0 {
		t.Fatalf("a refused gate still filed %d issues", len(s.creates)-before)
	}
}

func TestCodeScanningSweepReportsMissingPermissionOnceAndBacksOff(t *testing.T) {
	resetRESTAccountingForTest(t, time.Now())
	s := &codeScanningServer{alertsErr: http.StatusForbidden, alertsMsg: "Resource not accessible by integration"}
	c := newCodeScanningClient(t, s.start(t))
	cfg := codeScanningConfig()
	now := time.Now()
	c.RunCodeScanningSweep(context.Background(), cfg, now)
	st := c.CodeScanningStatus()
	if len(st.Repos) != 1 || st.Repos[0].NoticeKind != codescanning.NoticeMissingPermission || !strings.Contains(st.Repos[0].Notice, "security_events") {
		t.Fatalf("status %+v", st)
	}
	if len(st.Notices) != 1 {
		t.Fatalf("notices %v, want one", st.Notices)
	}
	gets := s.alertGets
	c.RunCodeScanningSweep(context.Background(), cfg, now.Add(time.Hour))
	if s.alertGets != gets {
		t.Fatalf("the sweep retried a refused repo inside the back-off (%d extra reads)", s.alertGets-gets)
	}
	if len(s.creates) != 0 {
		t.Fatal("filed an issue without reading alerts")
	}
	c.RunCodeScanningSweep(context.Background(), cfg, now.Add(codeScanningNoAccessBackoff+time.Minute))
	if s.alertGets == gets {
		t.Fatal("the sweep never retried after the back-off")
	}

	// GitHub's 404 for "no analysis" is a different notice.
	s2 := &codeScanningServer{alertsErr: http.StatusNotFound, alertsMsg: "no analysis found"}
	c2 := newCodeScanningClient(t, s2.start(t))
	c2.RunCodeScanningSweep(context.Background(), cfg, now)
	if k := c2.CodeScanningStatus().Repos[0].NoticeKind; k != codescanning.NoticeNotEnabled {
		t.Fatalf("no-analysis notice kind %q", k)
	}
}

func TestCodeScanningSweepMakesNoCallsWhenOffOrOutsideNormalBudget(t *testing.T) {
	resetRESTAccountingForTest(t, time.Now())
	s := &codeScanningServer{alerts: codeScanningFixture(t)}
	c := newCodeScanningClient(t, s.start(t))
	c.RunCodeScanningSweep(context.Background(), &config.Config{Project: config.ProjectConfig{Org: "o", Repos: []string{"r"}}}, time.Now())
	if s.requests != 0 {
		t.Fatalf("disabled sweep made %d requests", s.requests)
	}
	c.SetAPIBudgetThresholds(800, 250)
	c.rateLimits.mu.Lock()
	c.rateLimits.windows = map[string]rateLimitWindow{githubCoreRateLimitResource: {
		limit: 7000, remaining: 600, reset: time.Now().Add(30 * time.Minute), observedAt: time.Now(),
	}}
	c.rateLimits.mu.Unlock()
	res := c.RunCodeScanningSweep(context.Background(), codeScanningConfig(), time.Now())
	if s.requests != 0 || !res.BudgetSkipped {
		t.Fatalf("conserve budget: %d requests, skipped=%v", s.requests, res.BudgetSkipped)
	}
}

func TestCodeScanningAlertListingIsBounded(t *testing.T) {
	resetRESTAccountingForTest(t, time.Now())
	s := &codeScanningServer{alerts: codeScanningFixture(t)}
	c := newCodeScanningClient(t, s.start(t))
	old := codeScanningPerPage
	codeScanningPerPage = 50
	t.Cleanup(func() { codeScanningPerPage = old })
	f := codeScanningForge{c: c}
	alerts, complete, err := f.ListAlerts(context.Background(), "o/r", "open", 1)
	if err != nil || complete || len(alerts) != codeScanningPerPage {
		t.Fatalf("one page of %d: got %d alerts complete=%v err=%v", len(s.alerts), len(alerts), complete, err)
	}
	alerts, complete, err = f.ListAlerts(context.Background(), "o/r", "open", 10)
	if err != nil || !complete || len(alerts) != len(s.alerts) {
		t.Fatalf("full listing: %d alerts complete=%v err=%v", len(alerts), complete, err)
	}
}

// A closing note carries codescanning.CloseMarker. When an earlier pass
// posted it and the close itself failed, the retry closes without posting
// the note a second time.
func TestCodeScanningCloseNotePostedOnceAcrossRetries(t *testing.T) {
	srv := &codeScanningServer{issues: []map[string]any{{"number": 7, "state": "open", "title": "t", "body": "b", "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}}}}
	c := newCodeScanningClient(t, srv.start(t))
	f := codeScanningForge{c: c}
	note := codescanning.CloseComment(map[int]codescanning.AlertStatus{12: codescanning.StatusFixed})
	if !strings.Contains(note, codescanning.CloseMarker) {
		t.Fatalf("close note lacks its marker: %q", note)
	}
	ctx := context.Background()
	if err := f.CloseIssue(ctx, "o/r", 7, note, "completed"); err != nil {
		t.Fatal(err)
	}
	// A retry (as after a refused close) must not post the note again.
	if err := f.CloseIssue(ctx, "o/r", 7, note, "completed"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if got := len(srv.posted[7]); got != 1 {
		t.Fatalf("closing note posted %d times, want 1", got)
	}
	if srv.issues[0]["state"] != "closed" {
		t.Fatalf("issue state = %v, want closed", srv.issues[0]["state"])
	}
}
