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
