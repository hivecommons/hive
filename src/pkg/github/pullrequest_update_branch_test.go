package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// GitHub answers PUT .../update-branch with 202 Accepted; go-github reports
// that as *AcceptedError. UpdateBranch must treat it as success — otherwise
// every successful hygiene push reads as a failure (#10437).
func TestUpdateBranch_AcceptedIsSuccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/pulls/7/update-branch") {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		calls++
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "acme", []string{"widgets"})
	if err := c.UpdateBranch(context.Background(), "widgets", 7); err != nil {
		t.Fatalf("UpdateBranch on 202 Accepted = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("update-branch calls = %d, want 1", calls)
	}
}

func TestUpdateBranch_ConflictIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Merge conflict"}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "acme", []string{"widgets"})
	err := c.UpdateBranch(context.Background(), "widgets", 7)
	if err == nil || !strings.Contains(err.Error(), "widgets#7") {
		t.Fatalf("UpdateBranch on 422 = %v, want an error naming acme/widgets#7", err)
	}
}

func TestIsUpdateBranchStaleHead(t *testing.T) {
	resp := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"expected head mismatch", &gh.ErrorResponse{Response: resp(http.StatusUnprocessableEntity), Message: "expected head sha didn't match current head ref."}, true},
		{"wrapped mismatch", fmt.Errorf("updating branch: %w", &gh.ErrorResponse{Response: resp(http.StatusUnprocessableEntity), Message: "expected_head_sha does not match"}), true},
		{"merge conflict", &gh.ErrorResponse{Response: resp(http.StatusUnprocessableEntity), Message: "Merge conflict"}, false},
		{"not a 422", &gh.ErrorResponse{Response: resp(http.StatusForbidden), Message: "expected head sha"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUpdateBranchStaleHead(tc.err); got != tc.want {
				t.Fatalf("IsUpdateBranchStaleHead(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
