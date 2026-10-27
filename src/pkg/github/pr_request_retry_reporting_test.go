package github

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailPRRequestReportsMissingHeadOnce(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		wantSpecific bool
		wantGeneric  bool
	}{
		{"missing head", &errMissingHead{repo: "o/r", head: "branch", repoVisible: true}, true, false},
		{"other failure", errors.New("temporary failure"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			c := NewClientForTest("http://127.0.0.1:0", "o", nil, slog.New(slog.NewTextHandler(&logs, nil)))
			path := filepath.Join(t.TempDir(), "request.json")
			c.failPRRequest(path, PRRequest{Repo: "o/r", Head: "branch"}, tc.err, time.Now)
			got := logs.String()
			if strings.Contains(got, "head branch is not on the remote") != tc.wantSpecific || strings.Contains(got, "request failed, will retry with backoff") != tc.wantGeneric {
				t.Fatalf("unexpected logs: %s", got)
			}
		})
	}
}
