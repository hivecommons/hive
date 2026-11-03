package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const missingMergeCheckErr = `Required status check "guard" is expected`

func TestMissingMergeCheckAlertExactCurrentHead(t *testing.T) {
	tests := []struct {
		name           string
		head           string
		checkHead      string
		status         string
		conclusion     string
		app            int
		requiredApp    int
		ruleApp        int
		protectionCode int
		rulesCode      int
		checkCode      int
		statusCode     int
		finalCode      int
		finalHead      string
		finalBase      string
		checksMissing  bool
		statusSuccess  bool
		statusHead     string
		statusPaginate bool
		statusPageFail bool
		statusTruncate bool
		secondMissing  bool
		paginate       bool
		truncated      bool
		pageError      bool
		pageLimit      bool
		newerPending   bool
		reraise        bool
		closed         bool
		wantClear      bool
	}{
		{name: "check now succeeds", wantClear: true},
		{name: "new current head succeeds after old refusal", head: "newer", checkHead: "newer", wantClear: true},
		{name: "old head success insufficient", checkHead: "old"},
		{name: "pending is not resolution", status: "in_progress", conclusion: ""},
		{name: "failed is not resolution", conclusion: "failure"},
		{name: "neutral follows existing policy", conclusion: "neutral", wantClear: true},
		{name: "scope skipped follows existing policy", conclusion: "skipped", wantClear: true},
		{name: "required App matches", requiredApp: 42, wantClear: true},
		{name: "different App cannot substitute", requiredApp: 99},
		{name: "ruleset producer matches", ruleApp: 42, wantClear: true},
		{name: "ruleset producer mismatch", ruleApp: 99},
		{name: "conflicting producer policy unknown", requiredApp: 42, ruleApp: 99},
		{name: "protection permission error", protectionCode: http.StatusForbidden},
		{name: "rules unavailable", rulesCode: http.StatusBadGateway},
		{name: "check read error", checkCode: http.StatusForbidden},
		{name: "status read error", statusCode: http.StatusBadGateway},
		{name: "PR recheck error", finalCode: http.StatusForbidden},
		{name: "head changes during observation", finalHead: "changed"},
		{name: "base changes during observation", finalBase: "other"},
		{name: "missing check retained", checksMissing: true},
		{name: "all originally missing checks must resolve", secondMissing: true},
		{name: "status success", checksMissing: true, statusSuccess: true, wantClear: true},
		{name: "old combined status head rejected", checksMissing: true, statusSuccess: true, statusHead: "old"},
		{name: "successful status on later page", checksMissing: true, statusSuccess: true, statusPaginate: true, wantClear: true},
		{name: "status pagination error retained", checksMissing: true, statusSuccess: true, statusPaginate: true, statusPageFail: true},
		{name: "truncated status evidence retained", checksMissing: true, statusSuccess: true, statusTruncate: true},
		{name: "status cannot substitute required App", checksMissing: true, statusSuccess: true, requiredApp: 42},
		{name: "positive evidence after first page", paginate: true, wantClear: true},
		{name: "partial pagination error retained", paginate: true, pageError: true},
		{name: "truncated evidence retained", truncated: true},
		{name: "bounded pagination retained", pageLimit: true},
		{name: "latest pending attempt overrides old success", newerPending: true},
		{name: "new refusal during observation retained", reraise: true},
		{name: "closed PR still resolves", closed: true, wantClear: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head, checkHead := tt.head, tt.checkHead
			if head == "" {
				head = "new"
			}
			if checkHead == "" {
				checkHead = head
			}
			status, conclusion, app := tt.status, tt.conclusion, tt.app
			if status == "" {
				status = "completed"
			}
			if conclusion == "" && status == "completed" {
				conclusion = "success"
			}
			if app == 0 {
				app = 42
			}
			var c *Client
			prReads, checkReads := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodGet {
					t.Errorf("alert observer attempted mutation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/7"):
					prReads++
					currentHead, base, state := head, "v5", "open"
					if tt.closed {
						state = "closed"
					}
					if prReads > 1 {
						if tt.finalCode != 0 {
							w.WriteHeader(tt.finalCode)
							return
						}
						if tt.finalHead != "" {
							currentHead = tt.finalHead
						}
						if tt.finalBase != "" {
							base = tt.finalBase
						}
						if tt.reraise {
							c.raiseMergeFailureAlert("o/r", 7, missingMergeCheckErr, head)
						}
					}
					_, _ = fmt.Fprintf(w, `{"number":7,"state":%q,"head":{"sha":%q},"base":{"ref":%q}}`, state, currentHead, base)
				case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
					if tt.protectionCode != 0 {
						w.WriteHeader(tt.protectionCode)
						return
					}
					_, _ = fmt.Fprintf(w, `{"contexts":["guard"],"checks":[{"context":"guard","app_id":%d}]}`, tt.requiredApp)
				case strings.Contains(r.URL.Path, "/rules/branches/"):
					if tt.rulesCode != 0 {
						w.WriteHeader(tt.rulesCode)
						return
					}
					if tt.ruleApp != 0 {
						_, _ = fmt.Fprintf(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"guard","integration_id":%d}]}}]`, tt.ruleApp)
					} else {
						_, _ = io.WriteString(w, `[]`)
					}
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					checkReads++
					if !strings.HasSuffix(r.URL.Path, "/commits/"+head+"/check-runs") {
						t.Errorf("read evidence for wrong head: %s", r.URL.Path)
					}
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					if tt.checkCode != 0 || tt.pageError && page == 2 {
						code := tt.checkCode
						if code == 0 {
							code = http.StatusBadGateway
						}
						w.WriteHeader(code)
						return
					}
					if (tt.paginate && page != 2) || tt.pageLimit {
						next := page + 1
						if next < 2 {
							next = 2
						}
						w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, next))
						_, _ = io.WriteString(w, `{"total_count":2,"check_runs":[{"id":1,"name":"unrelated"}]}`)
						return
					}
					checks := fmt.Sprintf(`{"id":2,"name":"guard","head_sha":%q,"status":%q,"conclusion":%q,"app":{"id":%d}}`, checkHead, status, conclusion, app)
					if tt.newerPending {
						checks += fmt.Sprintf(`,{"id":3,"name":"guard","head_sha":%q,"status":"queued","app":{"id":%d}}`, head, app)
					}
					total := 1
					if tt.checksMissing {
						checks, total = "", 0
					}
					if tt.newerPending || tt.paginate || tt.truncated {
						total = 2
					}
					_, _ = fmt.Fprintf(w, `{"total_count":%d,"check_runs":[%s]}`, total, checks)
				case strings.HasSuffix(r.URL.Path, "/status"):
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					if tt.statusPaginate && page != 2 {
						w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
						_, _ = fmt.Fprintf(w, `{"sha":%q,"total_count":1,"statuses":[]}`, head)
						return
					}
					if tt.statusPageFail && page == 2 {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					if tt.statusCode != 0 {
						w.WriteHeader(tt.statusCode)
						return
					}
					statusHead := head
					if tt.statusHead != "" {
						statusHead = tt.statusHead
					}
					statuses, total := "", 0
					if tt.statusSuccess {
						statuses, total = `{"id":8,"context":"guard","state":"success"}`, 1
					}
					if tt.statusTruncate {
						total++
					}
					_, _ = fmt.Fprintf(w, `{"sha":%q,"total_count":%d,"statuses":[%s]}`, statusHead, total, statuses)
				default:
					t.Errorf("unexpected observer request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c = testMergeClient(t, srv.URL)
			sink := &recordingMergeAlertSink{}
			c.SetMergeFailureAlertSink(sink)
			trigger := missingMergeCheckErr
			if tt.secondMissing {
				trigger = "ci gate: required check(s) not yet reported on old: guard, lint"
			}
			c.raiseMergeFailureAlert("o/r", 7, trigger, "old")
			c.revalidateMergeFailureAlerts(context.Background(), nil)
			if got := len(sink.clears) == 1; got != tt.wantClear {
				t.Fatalf("cleared=%v, want %v; PR reads=%d check pages=%d", got, tt.wantClear, prReads, checkReads)
			}
			if checkReads > mergeAlertEvidenceMaxPages {
				t.Fatalf("unbounded evidence reads: %d", checkReads)
			}
		})
	}
}

func TestMissingMergeCheckAlertPreservesOtherPRs(t *testing.T) {
	var blocked atomic.Bool
	blocked.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			number := 7
			if strings.HasSuffix(r.URL.Path, "/8") {
				number = 8
			}
			_, _ = fmt.Fprintf(w, `{"number":%d,"state":"open","head":{"sha":"head%d"},"base":{"ref":"v5"}}`, number, number)
		case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
			_, _ = io.WriteString(w, `{"contexts":["guard"]}`)
		case strings.Contains(r.URL.Path, "/rules/branches/"):
			_, _ = io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			head := "head7"
			if strings.Contains(r.URL.Path, "/head8/") {
				head = "head8"
				if blocked.Load() {
					_, _ = io.WriteString(w, `{"total_count":0,"check_runs":[]}`)
					return
				}
			}
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":42,"name":"guard","head_sha":%q,"status":"completed","conclusion":"success","app":{"id":42}}]}`, head)
		case strings.HasSuffix(r.URL.Path, "/status"):
			head := "head7"
			if strings.Contains(r.URL.Path, "/head8/") {
				head = "head8"
			}
			_, _ = fmt.Fprintf(w, `{"sha":%q,"total_count":0,"statuses":[]}`, head)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)
	c.raiseMergeFailureAlert("O/R", 7, missingMergeCheckErr, "head7")
	c.raiseMergeFailureAlert("o/r", 8, missingMergeCheckErr, "head8")
	if sink.adds[0].id != sink.adds[1].id {
		t.Fatal("aggregate banner identity changed")
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 0 {
		t.Fatal("resolved PR hid another PR's missing check")
	}
	c.RecordPRMergedAudit("O/R", 7, "squash", "head7", PRAuditPathSweep)
	if len(sink.clears) != 0 {
		t.Fatal("another PR's merge hid unresolved blocker")
	}
	blocked.Store(false)
	now = now.Add(mergeAlertRevalidateInterval)
	c.revalidateMergeFailureAlerts(context.Background(), nowFn)
	if len(sink.clears) != 1 {
		t.Fatalf("all originating blockers resolved but aggregate remains: %v", sink.clears)
	}
	if len(c.mergeAlertIDsByRepo) != 0 {
		t.Fatal("resolved source entries retained")
	}
}
