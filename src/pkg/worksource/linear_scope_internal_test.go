package worksource

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func scopeTestServer(t *testing.T, probes, fulls *int32, probeStatus int, probeUpdatedAt, fullBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		b := make([]byte, 4096)
		for {
			n, err := r.Body.Read(b)
			buf.Write(b[:n])
			if err != nil {
				break
			}
		}
		if strings.Contains(buf.String(), "description") {
			atomic.AddInt32(fulls, 1)
			fmt.Fprint(w, fullBody)
			return
		}
		atomic.AddInt32(probes, 1)
		if probeStatus != http.StatusOK {
			w.WriteHeader(probeStatus)
			return
		}
		fmt.Fprintf(w, `{"data":{"issue":{"updatedAt":%q}}}`, probeUpdatedAt)
	}))
}

func seedScopeCache(src *LinearSource, checkedAt time.Time) LinearScopeIssue {
	issue := LinearScopeIssue{
		Identifier: "ONB-1",
		State:      "Todo",
		Text:       "cached text",
		UpdatedAt:  time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC),
	}
	src.storeScopeIssue("ONB-1", issue, checkedAt)
	return issue
}

const scopeFullBody = `{"data":{"issue":{"identifier":"ONB-1","title":"T","updatedAt":"2026-10-28T10:00:00Z","description":"fresh","state":{"name":"Done"},"comments":{"nodes":[]}}}}`

func TestLinearScopeContractIssueRecheck(t *testing.T) {
	cases := []struct {
		name        string
		age         time.Duration
		probeStatus int
		probeUpdate string
		wantProbes  int32
		wantFulls   int32
		wantText    string
	}{
		{"within recheck interval asks nothing", time.Minute, http.StatusOK, "", 0, 0, "cached text"},
		{"unchanged issue reuses cache after probe", 10 * time.Minute, http.StatusOK, "2026-10-27T10:00:00Z", 1, 0, "cached text"},
		{"changed issue refetches", 10 * time.Minute, http.StatusOK, "2026-10-28T10:00:00Z", 1, 1, "fresh"},
		{"failed probe falls back to full fetch", 10 * time.Minute, http.StatusInternalServerError, "", 1, 1, "fresh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var probes, fulls int32
			srv := scopeTestServer(t, &probes, &fulls, tc.probeStatus, tc.probeUpdate, scopeFullBody)
			defer srv.Close()
			src := NewLinearSource(LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)
			seededAt := time.Now().Add(-tc.age)
			seeded := seedScopeCache(src, seededAt)

			got, err := src.ScopeContractIssue(context.Background(), "onb-1")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Text, tc.wantText) {
				t.Fatalf("text = %q, want it to contain %q", got.Text, tc.wantText)
			}
			if p, f := atomic.LoadInt32(&probes), atomic.LoadInt32(&fulls); p != tc.wantProbes || f != tc.wantFulls {
				t.Fatalf("probes/fulls = %d/%d, want %d/%d", p, f, tc.wantProbes, tc.wantFulls)
			}
			if tc.wantFulls == 0 && got != seeded {
				t.Fatalf("issue = %+v, want cached %+v", got, seeded)
			}
			checkedAt := src.scopeIssues["ONB-1"].checkedAt
			if tc.wantProbes == 0 {
				// A cache hit inside the recheck interval asks Linear nothing,
				// so it has no fresh observation to record.
				if !checkedAt.Equal(seededAt) {
					t.Fatalf("checkedAt = %v, want the seeded %v left alone", checkedAt, seededAt)
				}
			} else if checkedAt.Before(seededAt) || time.Since(checkedAt) > time.Minute {
				t.Fatalf("checkedAt = %v was not refreshed past seeded %v", checkedAt, seededAt)
			}
		})
	}
}

func TestLinearScopeContractIssueErrors(t *testing.T) {
	cases := []struct {
		name       string
		identifier string
		body       string
		want       string
	}{
		{"empty identifier", "  ", `{}`, "identifier is required"},
		{"malformed issue payload", "ONB-1", `{"data":{"issue":"nope"}}`, "decode response"},
		{"missing issue", "ONB-1", `{"data":{"issue":null}}`, "ONB-1 not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			src := NewLinearSource(LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)
			if _, err := src.ScopeContractIssue(context.Background(), tc.identifier); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLinearScopeContractIssueDefaultsAndCommentCap(t *testing.T) {
	var nodes []string
	for i := 1; i <= 7; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"body":"comment-%d","createdAt":"2026-10-%02dT09:00:00Z","user":null}`, i, 10+i))
	}
	body := `{"data":{"issue":{"title":"T","updatedAt":"2026-10-28T10:00:00Z","state":{"name":"Todo"},"comments":{"nodes":[` + strings.Join(nodes, ",") + `]}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	src := NewLinearSource(LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)

	issue, err := src.ScopeContractIssue(context.Background(), "onb-9")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Identifier != "ONB-9" || !strings.HasPrefix(issue.Text, "ONB-9: T") {
		t.Fatalf("identifier not defaulted to the requested one: %+v", issue)
	}
	for _, dropped := range []string{"comment-1", "comment-2"} {
		if strings.Contains(issue.Text, dropped) {
			t.Fatalf("%s should have been dropped (only the newest %d are kept):\n%s", dropped, linearScopeIssueComments, issue.Text)
		}
	}
	for _, kept := range []string{"comment-3", "comment-7", "unknown ("} {
		if !strings.Contains(issue.Text, kept) {
			t.Fatalf("text missing %q:\n%s", kept, issue.Text)
		}
	}
}

func TestLinearScopeSource(t *testing.T) {
	ws := config.WorkSourceConfig{Type: "github", RunStages: true}
	ws.Linear.APIKey = "key"
	ws.Linear.Teams = []config.LinearTeamSourceConfig{{Key: "ENG", Repo: "acme/app"}}
	ws.Linear.AssignedOnly = true
	ws.Wavefront.Enabled = true

	src, err := LinearScopeSource(ws, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if src == nil {
		t.Fatal("nil Linear source")
	}
}

func TestLinearScopeContractIssueStaleCacheOnRefetchError(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	src := NewLinearSource(LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)
	seeded := seedScopeCache(src, time.Now().Add(-time.Hour))
	for i := 0; i < 2; i++ {
		got, err := src.ScopeContractIssue(context.Background(), "ONB-1")
		if err != nil || got.Text != seeded.Text {
			t.Fatalf("call %d: got %+v, err %v; want stale cached issue", i, got, err)
		}
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("server requests = %d, want 2 (probe+fetch once, then recheck window)", n)
	}
}
