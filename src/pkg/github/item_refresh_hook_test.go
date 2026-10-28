package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// Used by sweep tests to pin every unblock branch and ensure untouched issues
// and help-wanted removals do not generate refreshes.
func expectIssueRefreshes(t *testing.T, c *Client, want map[int]int) {
	t.Helper()
	got := map[int]int{}
	c.SetItemRefreshHook(func(repo string, number int, isPR bool) {
		if repo != "o/r" || isPR {
			t.Errorf("refresh = %s#%d pr=%v, want o/r issue", repo, number, isPR)
		}
		got[number]++
	})
	t.Cleanup(func() {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("issue refreshes = %v, want %v", got, want)
		}
	})
}

func TestItemRefreshHookAfterUnblock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		clear  bool
		want   int
	}{
		{"removed", http.StatusOK, false, 1},
		{"already absent", http.StatusNotFound, false, 1},
		{"write failed", http.StatusForbidden, false, 0},
		{"hook cleared", http.StatusOK, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/repos/o/r/issues/7/labels/blocked" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()
			c := testClient(t, srv.URL)
			calls := 0
			c.SetItemRefreshHook(func(repo string, n int, isPR bool) {
				calls++
				if repo != "o/r" || n != 7 || isPR {
					t.Errorf("refresh = %s#%d pr=%v", repo, n, isPR)
				}
			})
			if tc.clear {
				c.SetItemRefreshHook(nil)
			}
			err := c.unblockIssue(context.Background(), "o/r", 7)
			if (err != nil) != (tc.status == http.StatusForbidden) {
				t.Fatalf("unblock error = %v", err)
			}
			if calls != tc.want {
				t.Fatalf("refresh calls = %d, want %d", calls, tc.want)
			}
		})
	}
	var c *Client
	c.SetItemRefreshHook(nil)
	if err := c.unblockIssue(context.Background(), "o/r", 7); err != ErrNoGitHubClient {
		t.Fatalf("nil client error = %v", err)
	}
}

func TestBacklogLiftRefreshesBeforeComment(t *testing.T) {
	for _, failLabel := range []bool{false, true} {
		t.Run(map[bool]string{false: "comment fails", true: "label fails"}[failLabel], func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodDelete:
					if failLabel {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					_, _ = w.Write([]byte(`[]`))
				case http.MethodPost:
					if calls != 1 {
						t.Error("refresh must be queued before the comment")
					}
					w.WriteHeader(http.StatusForbidden)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := testClient(t, srv.URL)
			c.SetItemRefreshHook(func(string, int, bool) { calls++ })
			err := (githubIssueBacklogSink{c: c}).LiftBlock(context.Background(), "o/r", ReviewBacklogRef{Number: 7}, "unblocked")
			if err == nil {
				t.Fatal("expected write error")
			}
			want := 1
			if failLabel {
				want = 0
			}
			if calls != want {
				t.Fatalf("refresh calls = %d, want %d", calls, want)
			}
		})
	}
}
