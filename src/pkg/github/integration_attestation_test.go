package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

func directSubjectZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for name, content := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestDirectIntegrationAttestationRequiresExactProducerAndCheckout(t *testing.T) {
	for _, scenario := range []string{"valid", "missing", "expired", "duplicate", "wrong-run", "wrong-attempt", "wrong-policy", "wrong-member", "wrong-tree", "wrong-parents", "truncated", "bad-zip", "bad-json", "unknown-field", "extra-file", "oversized", "download-error", "insecure-location"} {
		t.Run(scenario, func(t *testing.T) {
			base, head, tested, tree := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			policy := config.IntegrationPolicy{Repo: "o/r", Base: "v5", Engine: "direct", Workflow: ".github/workflows/test.yml", Blob: strings.Repeat("e", 40), AppID: 42, Jobs: []string{"test"}}
			run := &gh.WorkflowRun{ID: gh.Ptr(int64(10)), RunAttempt: gh.Ptr(1), Event: gh.Ptr("pull_request"), Status: gh.Ptr("completed"), HeadSHA: gh.Ptr(head),
				PullRequests: []*gh.PullRequest{{Number: gh.Ptr(7), Base: &gh.PullRequestBranch{Ref: gh.Ptr("v5"), SHA: gh.Ptr(base)}, Head: &gh.PullRequestBranch{SHA: gh.Ptr(head)}}}}
			subject := DirectIntegrationAttestation{Version: 1, Repo: "o/r", Base: "v5", BaseSHA: base, TestedSHA: tested, TestedTree: tree, RunID: 10, Attempt: 1, Policy: policy.Fingerprint(), Members: []DirectIntegrationMember{{PR: 7, Head: head}}}
			switch scenario {
			case "wrong-attempt":
				subject.Attempt = 2
			case "wrong-policy":
				subject.Policy = "other-policy"
			case "wrong-member":
				subject.Members[0].Head = strings.Repeat("f", 40)
			}
			data, err := json.Marshal(subject)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "bad-json":
				data = []byte("malformed")
			case "unknown-field":
				data = append(data[:len(data)-1], []byte(`,"unreviewed":true}`)...)
			case "oversized":
				data = bytes.Repeat([]byte(" "), integrationSubjectMaxBytes+1)
			}
			files := map[string][]byte{integrationSubjectFile: data}
			if scenario == "extra-file" {
				files["other.json"] = data
			}
			archive := directSubjectZIP(t, files)
			if scenario == "bad-zip" {
				archive = []byte("malformed")
			}
			var api *httptest.Server
			api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/artifacts"):
					if scenario == "truncated" {
						w.Header().Set("Link", `<http://example.invalid?page=2>; rel="next"`)
					}
					if scenario == "missing" {
						_, _ = w.Write([]byte(`{"artifacts":[]}`))
						return
					}
					runID := 10
					if scenario == "wrong-run" {
						runID = 11
					}
					artifact := fmt.Sprintf(`{"id":20,"name":"%s1","expired":%t,"size_in_bytes":%d,"workflow_run":{"id":%d,"head_sha":%q}}`, integrationSubjectArtifactPrefix, scenario == "expired", len(archive), runID, head)
					if scenario == "duplicate" {
						artifact = artifact + "," + artifact
					}
					_, _ = fmt.Fprintf(w, `{"artifacts":[%s]}`, artifact)
				case strings.HasSuffix(r.URL.Path, "/artifacts/20/zip"):
					location := api.URL + "/archive"
					if scenario == "insecure-location" {
						location = "http://example.invalid/archive"
					}
					http.Redirect(w, r, location, http.StatusFound)
				case r.URL.Path == "/archive":
					if r.Header.Get("Authorization") != "" {
						t.Error("GitHub token was sent to artifact storage")
					}
					if scenario == "download-error" {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					_, _ = w.Write(archive)
				case strings.Contains(r.URL.Path, "/git/commits/"):
					actualTree, parent := tree, head
					if scenario == "wrong-tree" {
						actualTree = strings.Repeat("f", 40)
					}
					if scenario == "wrong-parents" {
						parent = strings.Repeat("f", 40)
					}
					_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":%q},"parents":[{"sha":%q},{"sha":%q}]}`, tested, actualTree, base, parent)
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			client := NewClient("token", "o", []string{"r"}, nil, api.URL)
			got, err := client.ReadDirectIntegrationAttestation(context.Background(), policy, run)
			if scenario == "valid" {
				if err != nil || got.TestedSHA != tested || got.TestedTree != tree {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("unbound attestation accepted: %s %+v", scenario, got)
			}
		})
	}
}
