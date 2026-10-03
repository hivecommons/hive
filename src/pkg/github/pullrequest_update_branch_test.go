package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
