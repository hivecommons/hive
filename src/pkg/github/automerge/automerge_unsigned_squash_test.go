package automerge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/fleetreport/slogfeed"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

func TestUnsignedSquashRefusalIsNotSelfReported(t *testing.T) {
	message := "Fix"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/pulls/7/commits" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `[{"commit":{"message":%q}}]`, message)
	}))
	defer api.Close()

	var logs bytes.Buffer
	observations := 0
	logger := slog.New(slogfeed.NewWithObserver(slog.NewTextHandler(&logs, nil), func(_, _, _, _ string) { observations++ }))
	client := hgithub.NewClient("token", "acme", []string{"widget"}, logger, api.URL)
	client.SetCreditIssueAuthors(func(string) bool { return false })
	client.SetDCORequired(func(string) (bool, bool) { return true, true })
	engine := New(client, Options{Logger: logger})
	pr := &gh.PullRequest{Number: gh.Ptr(7)}
	for range 5 {
		_, _, refused := engine.mergeOptions(context.Background(), "acme", "widget", pr, "unsigned-head", "squash")
		if refused != autoMergeReasonSquashUnsigned {
			t.Fatalf("unsigned squash refusal = %q", refused)
		}
	}
	if observations != 0 || !strings.Contains(logs.String(), "level=INFO msg=\"automerge refused squash without Signed-off-by\"") {
		t.Fatalf("observations = %d, logs = %q", observations, logs.String())
	}

	message = "Fix\n\nSigned-off-by: Author <author@example.com>"
	got, _, refused := engine.mergeOptions(context.Background(), "acme", "widget", pr, "signed-head", "squash")
	if refused != "" || !strings.Contains(got, "Signed-off-by: Author <author@example.com>") {
		t.Fatalf("signed squash message = %q, refusal = %q", got, refused)
	}
}

// In a repo with no DCO gate (no dco.required entry, no dco/DCO context on
// the head) an unsigned squash is not refused: Renovate PRs and unsigned
// human commits merge as they did before #18203
// (projectbluefin/bluefin-report#91, projectbluefin/utah-packages#1109).
func TestUnsignedSquashMergesWithoutDCOGate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		override    func(string) (bool, bool)
		headContext string
		wantRefused string
	}{
		{name: "no gate merges", headContext: "Tests Passed"},
		{name: "a dco status on the head refuses", headContext: "dco", wantRefused: autoMergeReasonSquashUnsigned},
		{name: "dco.required false overrides the detected gate", override: func(string) (bool, bool) { return false, true }, headContext: "dco"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/acme/widget/pulls/7/commits":
					fmt.Fprint(w, `[{"sha":"c0","commit":{"message":"chore(deps): update widget"},"parents":[{"sha":"p"}]}]`)
				case "/repos/acme/widget/commits/head1/status":
					fmt.Fprintf(w, `{"sha":"head1","total_count":1,"statuses":[{"context":%q,"state":"success"}]}`, tc.headContext)
				case "/repos/acme/widget/commits/head1/check-runs":
					fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
			client := hgithub.NewClient("token", "acme", []string{"widget"}, logger, api.URL)
			client.SetCreditIssueAuthors(func(string) bool { return false })
			client.SetDCORequired(tc.override)
			engine := New(client, Options{Logger: logger})
			pr := &gh.PullRequest{Number: gh.Ptr(7)}
			message, _, refused := engine.mergeOptions(context.Background(), "acme", "widget", pr, "head1", "squash")
			if refused != tc.wantRefused || message != "" {
				t.Fatalf("mergeOptions = message %q, refused %q; want refused %q", message, refused, tc.wantRefused)
			}
		})
	}
}
