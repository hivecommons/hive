package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/intent"
)

func TestPinOnlyChangedFileEvidence(t *testing.T) {
	patch := "@@ -1 +1 @@\n-FROM image@sha256:" + strings.Repeat("a", 64) + "\n+FROM image@sha256:" + strings.Repeat("b", 64)
	for _, present := range []bool{true, false} {
		t.Run(map[bool]string{true: "patch present", false: "patch omitted"}[present], func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/pulls/1/files" {
					http.NotFound(w, r)
					return
				}
				file := map[string]any{"filename": "Containerfile", "status": "modified", "additions": 1, "deletions": 1}
				if present {
					file["patch"] = patch
				}
				_ = json.NewEncoder(w).Encode([]map[string]any{file})
			}))
			defer api.Close()
			client := gh.NewClient(api.Client())
			client.BaseURL, _ = url.Parse(api.URL + "/")
			pr := &gh.PullRequest{Number: gh.Ptr(1), ChangedFiles: gh.Ptr(1)}
			files, err := ListPRChangedFiles(context.Background(), client, "o", "r", pr)
			if err != nil {
				t.Fatal(err)
			}
			ok, why := intent.ClassifyPinOnly(files, []string{"Containerfile"}, intent.PinOnlySpec{Paths: []string{"Containerfile"}, Kinds: []string{"image-digest"}})
			if ok != present {
				t.Fatalf("classification = %v, %s", ok, why)
			}
			pr.ChangedFiles = gh.Ptr(2)
			if _, err := ListPRChangedFiles(context.Background(), client, "o", "r", pr); !errors.Is(err, ErrIncompletePRFileList) {
				t.Fatalf("incomplete list: %v", err)
			}
		})
	}
}
