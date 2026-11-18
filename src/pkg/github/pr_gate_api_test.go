package github

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/prgate"
)

const prGateTestHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func prGateTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClientForTest(srv.URL, "example-org", []string{"example-repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestInstallationPermissionMapCarriesStatuses(t *testing.T) {
	for name, want := range map[string]string{
		"installation_statuses_write.json":   "write",
		"installation_statuses_missing.json": "read",
	} {
		raw, err := os.ReadFile("testdata/pr_gate/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var inst gh.Installation
		if err := json.Unmarshal(raw, &inst); err != nil {
			t.Fatal(err)
		}
		if got := installationPermissionMap(inst.Permissions)[PermissionStatuses]; got != want {
			t.Errorf("%s: statuses = %q, want %q", name, got, want)
		}
	}
	if _, ok := installationPermissionMap(&gh.InstallationPermissions{})[PermissionStatuses]; ok {
		t.Error("an ungranted statuses permission must be dropped from the map")
	}
}

func TestReadPRGateGrantWithoutAppCredentialIsNotApplicable(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "example-org", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := c.ReadPRGateGrant(t.Context())
	if err != nil || got.Applicable {
		t.Fatalf("grant = %+v, err = %v, want not applicable", got, err)
	}
}

func TestPostPRGateStatusWritesTheContextOnTheExactHead(t *testing.T) {
	var gotPath string
	var body struct {
		Context, State, Description string
	}
	c := prGateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	})
	if err := c.PostPRGateStatus(t.Context(), "example-org/example-repo", 7, prGateTestHead, prgate.ContextVerifyShadow, "success", "ok"); err != nil {
		t.Fatal(err)
	}
	if want := "POST /repos/example-org/example-repo/statuses/" + prGateTestHead; gotPath != want {
		t.Errorf("request = %q, want %q", gotPath, want)
	}
	if body.Context != prgate.ContextVerifyShadow || body.State != "success" || body.Description != "ok" {
		t.Errorf("body = %+v", body)
	}
}

func TestPostPRGateStatusRefusesAnyOtherContextBeforeTheNetwork(t *testing.T) {
	calls := 0
	c := prGateTestClient(t, func(http.ResponseWriter, *http.Request) { calls++ })
	for _, name := range []string{"DCO", "dco", "prow/lgtm", "build-and-test", "coverage", ""} {
		err := c.PostPRGateStatus(t.Context(), "example-org/example-repo", 7, prGateTestHead, name, "success", "x")
		if !errors.Is(err, prgate.ErrContextNotAllowed) {
			t.Errorf("context %q: err = %v, want ErrContextNotAllowed", name, err)
		}
	}
	if calls != 0 {
		t.Errorf("%d requests reached GitHub", calls)
	}
}

// A 403 "Resource not accessible by integration" is how GitHub reports an
// installation without Commit statuses write: recorded fixture.
func TestPostPRGateStatusClassifiesForbidden(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantForbidden bool
	}{
		{"403 resource not accessible", http.StatusForbidden, true},
		{"500 is not a permission fault", http.StatusInternalServerError, false},
		{"422 is not a permission fault", http.StatusUnprocessableEntity, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := prGateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message": "Resource not accessible by integration"}`))
			})
			err := c.PostPRGateStatus(t.Context(), "example-org/example-repo", 7, prGateTestHead, prgate.ContextVerifyShadow, "success", "x")
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrPRGateStatusForbidden); got != tc.wantForbidden {
				t.Errorf("forbidden = %t, want %t (err %v)", got, tc.wantForbidden, err)
			}
		})
	}
}

func TestReadPRGateSubjectReadsLiveHead(t *testing.T) {
	c := prGateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number": 7, "state": "open", "draft": true, "title": "✨ add thing", "head": {"sha": "` + prGateTestHead + `"}}`))
	})
	got, err := c.ReadPRGateSubject(t.Context(), "example-org/example-repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := PRGateSubject{HeadSHA: prGateTestHead, Title: "✨ add thing", State: "open", Draft: true}
	if got != want {
		t.Errorf("subject = %+v, want %+v", got, want)
	}
}
