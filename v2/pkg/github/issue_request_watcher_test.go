package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func newIssueRequestTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client := NewClientForTest(server.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.issueAuthz = func(string, int) error { return nil }
	return client, server
}

func withIssueRequestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := issueRequestDirForTest
	issueRequestDirForTest = dir
	t.Cleanup(func() { issueRequestDirForTest = old })
	return dir
}

func TestIssueRequestWatcherDeduplicatesManagedVisualFinding(t *testing.T) {
	created := 0
	client, server := newIssueRequestTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{
              "number":96,
              "html_url":"https://github.com/o/r/issues/96",
              "title":"[Visual Hive] Repo map finding: storybook-discovery:.storybook/QualificationMissing.stories.tsx",
              "body":"Storybook story .storybook/QualificationMissing.stories.tsx is outside discovery patterns in .storybook/main.ts.",
              "labels":[{"name":"hive/managed"},{"name":"visual-hive"}]
            }]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			created++
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"number":999,"html_url":"https://github.com/o/r/issues/999"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()
	dir := withIssueRequestDir(t)
	path, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Agent: "scanner", Labels: []string{"bug"},
		Title: "[scanner] Storybook excludes QualificationMissing story from discovery",
		Body:  "`.storybook/main.ts` excludes `.storybook/QualificationMissing.stories.tsx` from discovery.",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.ProcessIssueRequestsOnce(context.Background())
	if created != 0 {
		t.Fatalf("managed duplicate caused %d create calls, want zero", created)
	}
	result := readIssueResponse(t, path)
	if !result.OK || result.Number != 96 || !result.AlreadyExisted || !result.DeduplicatedAgainstManaged {
		t.Fatalf("unexpected managed dedupe result: %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("successful request was not consumed")
	}
}

func TestIssueRequestWatcherCreatesNovelIssueAndReusesExactRetry(t *testing.T) {
	created := 0
	var persistedBody string
	client, server := newIssueRequestTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			w.Header().Set("Content-Type", "application/json")
			if persistedBody == "" {
				_, _ = io.WriteString(w, `[]`)
			} else {
				encoded, _ := json.Marshal(persistedBody)
				_, _ = io.WriteString(w, `[{"number":41,"html_url":"https://github.com/o/r/issues/41","title":"novel","body":`+string(encoded)+`}]`)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			created++
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			persistedBody = body.Body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"number":41,"html_url":"https://github.com/o/r/issues/41"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()
	dir := withIssueRequestDir(t)
	req := IssueRequest{Repo: "o/r", Agent: "scanner", Title: "novel", Body: "A new failure in `src/new.ts`.", Labels: []string{"bug"}}
	first, _ := WriteIssueRequest(dir, req)
	client.ProcessIssueRequestsOnce(context.Background())
	if result := readIssueResponse(t, first); !result.OK || result.AlreadyExisted {
		t.Fatalf("unexpected first result: %+v", result)
	}
	req.Agent = "quality"
	second, _ := WriteIssueRequest(dir, req)
	client.ProcessIssueRequestsOnce(context.Background())
	result := readIssueResponse(t, second)
	if !result.OK || !result.AlreadyExisted || result.DeduplicatedAgainstManaged || result.Number != 41 {
		t.Fatalf("unexpected retry result: %+v", result)
	}
	if created != 1 {
		t.Fatalf("cross-agent exact retry caused %d creates, want one", created)
	}
}

func TestIssueRequestWatcherDoesNotCollapseDistinctSameFileBug(t *testing.T) {
	client, server := newIssueRequestTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = io.WriteString(w, `[{"number":12,"title":"[Visual Hive] button contrast regression","body":"Contrast failed in src/App.tsx","labels":[{"name":"hive/managed"},{"name":"visual-hive"}]}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/issues/42"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()
	dir := withIssueRequestDir(t)
	path, _ := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Agent: "scanner", Title: "runtime crash", Body: "A nil dereference occurs in `src/App.tsx`."})
	client.ProcessIssueRequestsOnce(context.Background())
	result := readIssueResponse(t, path)
	if !result.OK || result.AlreadyExisted || result.Number != 42 {
		t.Fatalf("distinct same-file defect was collapsed: %+v", result)
	}
}

func TestIssueRequestWatcherNilAuthorizerFailsClosed(t *testing.T) {
	created := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { created++ }))
	defer server.Close()
	client := NewClientForTest(server.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	dir := withIssueRequestDir(t)
	path, _ := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Agent: "scanner", Title: "x", Body: "y"})
	client.ProcessIssueRequestsOnce(context.Background())
	if created != 0 {
		t.Fatalf("nil authorizer allowed %d GitHub calls", created)
	}
	if _, err := os.Stat(path + ".denied"); err != nil {
		t.Fatalf("denied request not quarantined: %v", err)
	}
}

func TestIssueRequestWatcherRejectsRepositoryOutsideHiveScope(t *testing.T) {
	requests := 0
	client, server := newIssueRequestTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer server.Close()
	dir := withIssueRequestDir(t)
	path, _ := WriteIssueRequest(dir, IssueRequest{Repo: "other/repository", Agent: "scanner", Title: "x", Body: "y"})
	client.ProcessIssueRequestsOnce(context.Background())
	if requests != 0 {
		t.Fatalf("out-of-scope request made %d GitHub calls", requests)
	}
	result := readIssueResponse(t, path)
	if result.OK || !strings.Contains(result.Error, "outside this Hive's configured project scope") {
		t.Fatalf("unexpected out-of-scope result: %+v", result)
	}
	if _, err := os.Stat(path + ".denied"); err != nil {
		t.Fatalf("out-of-scope request was not quarantined: %v", err)
	}
}

func readIssueResponse(t *testing.T, requestPath string) IssueResponse {
	t.Helper()
	data, err := os.ReadFile(strings.TrimSuffix(requestPath, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	var result IssueResponse
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
