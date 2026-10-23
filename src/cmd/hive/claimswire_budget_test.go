package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Relay completion is an early release, not TTL expiry: it still needs a
// forge marker, but must not bypass the shared unattended-comment budget.
func TestClaimHooks_LeaseRevokedSharesClaimCommentBudget(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		name := "available"
		if exhausted {
			name = "exhausted"
		}
		t.Run(name, func(t *testing.T) {
			posted := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/widgets/issues/7/comments" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "hive:release") {
					posted <- string(body)
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":1}`))
			}))
			defer srv.Close()
			gh := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			claim := claims.Claim{Repo: "acme/widgets", Issue: 7, Holder: "relay", HolderID: "relay#session", Kind: claims.KindContributor}
			if exhausted {
				// Use the same entry point as OnClaimed. Respect installations
				// overriding/disabling the process-wide cap; don't change a
				// sync.Once-backed setting underneath other tests.
				var err error
				for i := 0; i < 100; i++ {
					err = gh.CreateIssueCommentBudgeted(context.Background(), claim.Repo, claim.Issue, claims.ClaimComment(claim))
					if errors.Is(err, github.ErrIssueCommentBudget) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if err == nil {
					t.Skip("comment budget disabled or greater than test's bounded fill")
				}
			}

			cfg := &config.Config{}
			cfg.Governor.Claims.Label = boolPtr(false)
			cfg.Governor.Claims.Comment = boolPtr(true)
			failed := make(chan string, 1)
			logger := slog.New(slog.NewTextHandler(claimBudgetLogWriter(func(p []byte) { failed <- string(p) }), nil))
			hooks := githubClaimHooks(context.Background(), cfg, func() *github.Client { return gh }, logger)
			hooks.OnReleased(claim, "lease revoked")
			select {
			case body := <-posted:
				if exhausted {
					t.Fatalf("release bypassed exhausted claim comment budget: %s", body)
				}
				if !strings.Contains(body, "hive:release who=relay kind=contributor reason=lease_revoked at=") {
					t.Fatalf("missing lease release marker: %s", body)
				}
			case message := <-failed:
				if !exhausted || !strings.Contains(message, github.ErrIssueCommentBudget.Error()) || !strings.Contains(message, "release comment") {
					t.Fatalf("unexpected hook failure: %s", message)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("release hook neither posted nor reported budget suppression")
			}
		})
	}
}

type claimBudgetLogWriter func([]byte)

func (w claimBudgetLogWriter) Write(p []byte) (int, error) {
	w(p)
	return len(p), nil
}
