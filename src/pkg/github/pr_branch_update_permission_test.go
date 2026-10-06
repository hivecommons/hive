package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestViewerCanUpdateBranch(t *testing.T) {
	for _, tt := range []struct {
		name     string
		response string
		status   int
		allowed  bool
		wantErr  bool
	}{
		{name: "allowed", response: `{"data":{"repository":{"pullRequest":{"viewerCanUpdateBranch":true}}}}`, allowed: true},
		{name: "denied", response: `{"data":{"repository":{"pullRequest":{"viewerCanUpdateBranch":false}}}}`},
		{name: "missing repository", response: `{"data":{"repository":null}}`, wantErr: true},
		{name: "missing PR", response: `{"data":{"repository":{"pullRequest":null}}}`, wantErr: true},
		{name: "missing capability", response: `{"data":{"repository":{"pullRequest":{}}}}`, wantErr: true},
		{name: "null capability", response: `{"data":{"repository":{"pullRequest":{"viewerCanUpdateBranch":null}}}}`, wantErr: true},
		{name: "null data", response: `{"data":null}`, wantErr: true},
		{name: "empty response", response: `{}`, wantErr: true},
		{name: "partial data with error", response: `{"data":{"repository":{"pullRequest":{"viewerCanUpdateBranch":true}}},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`, wantErr: true},
		{name: "HTTP forbidden", status: http.StatusForbidden, response: `{"message":"forbidden"}`, wantErr: true},
		{name: "server failure", status: http.StatusInternalServerError, response: `{"message":"unavailable"}`, wantErr: true},
		{name: "malformed response", response: `{`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var req struct {
					Query     string `json:"query"`
					Variables struct {
						Owner  string `json:"owner"`
						Name   string `json:"name"`
						Number int    `json:"number"`
					} `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if !strings.Contains(req.Query, "viewerCanUpdateBranch") || strings.Contains(req.Query, "mutation") {
					t.Errorf("not a capability-only query: %s", req.Query)
				}
				if req.Variables.Owner != "contributor" || req.Variables.Name != "project" || req.Variables.Number != 42 {
					t.Errorf("wrong PR variables: %+v", req.Variables)
				}
				w.Header().Set("Content-Type", "application/json")
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				_, _ = w.Write([]byte(tt.response))
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "contributor", nil)
			// Both qualified and bare names use the same authenticated lookup.
			for _, repo := range []string{"contributor/project", "project"} {
				allowed, err := c.ViewerCanUpdateBranch(context.Background(), repo, 42)
				if allowed != tt.allowed || (err != nil) != tt.wantErr {
					t.Fatalf("allowed=%v err=%v; want allowed=%v error=%v", allowed, err, tt.allowed, tt.wantErr)
				}
			}
			if calls != 2 {
				t.Errorf("got %d API calls, want exactly one read per lookup", calls)
			}
		})
	}
}

func TestViewerCanUpdateBranchRejectsInvalidInputsWithoutGitHubCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid input reached GitHub: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "contributor", nil)
	for _, tt := range []struct {
		repo   string
		number int
	}{
		{"project", 0}, {"project", -1}, {"", 42}, {"<owner>/<repo>", 42}, {"owner/repo/extra", 42},
	} {
		allowed, err := c.ViewerCanUpdateBranch(context.Background(), tt.repo, tt.number)
		if allowed || err == nil {
			t.Errorf("%q #%d: allowed=%v err=%v", tt.repo, tt.number, allowed, err)
		}
	}
}

func TestViewerCanUpdateBranchWithoutClient(t *testing.T) {
	for _, c := range []*Client{nil, {}} {
		allowed, err := c.ViewerCanUpdateBranch(context.Background(), "owner/repo", 42)
		if allowed || !errors.Is(err, ErrNoGitHubClient) {
			t.Errorf("allowed=%v err=%v; want ErrNoGitHubClient", allowed, err)
		}
	}
}
