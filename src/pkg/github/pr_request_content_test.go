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

func TestAddedInternalMetadata(t *testing.T) {
	filed := "*" + "Filed" + " by outreach agent (ACMM L6 — full mode)*"
	hive := "— " + "hive" + ": agent=outreach backend=agy"
	tests := []struct {
		name     string
		patch    string
		wantLine int
		wantKind string
	}{
		{
			name:     "agent attribution added",
			patch:    "@@ -8,2 +8,3 @@\n context\n+" + filed + "\n context\n",
			wantLine: 9,
			wantKind: "agent attribution",
		},
		{
			name:     "hive run trailer added",
			patch:    "@@ -0,0 +1,2 @@\n+# Page\n+" + hive + "\n",
			wantLine: 2,
			wantKind: "hive run",
		},
		{
			name:  "removed attribution allowed",
			patch: "@@ -1,2 +1 @@\n-" + filed + "\n clean\n",
		},
		{
			name:  "context attribution allowed",
			patch: "@@ -1,2 +1,3 @@\n " + filed + "\n+real content\n context\n",
		},
		{
			name:  "ordinary authorship prose allowed",
			patch: "@@ -1 +1 @@\n+Filed by the release engineering team.\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, kind, found := addedInternalMetadata(tc.patch)
			if tc.wantKind == "" {
				if found {
					t.Fatalf("unexpected match at line %d (%s)", line, kind)
				}
				return
			}
			if !found || line != tc.wantLine || kind != tc.wantKind {
				t.Fatalf("got (%d, %q, %v), want (%d, %q, true)", line, kind, found, tc.wantLine, tc.wantKind)
			}
		})
	}
}

func TestPRRequestWatcherRejectsInternalMetadata(t *testing.T) {
	created := 0
	filed := "*" + "Filed" + " by outreach agent (ACMM L6 — full mode)*"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = io.WriteString(w, `{"default_branch":"docs"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/compare/docs...outreach/page":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]string{{
				"filename": "docs/page.md",
				"patch":    "@@ -1 +1,3 @@\n # Page\n+copy\n+" + filed,
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls":
			created++
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.prAuthz = func(string, int) error { return nil }
	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "outreach/page", Title: "docs: add page", Body: "adds the docs page", Agent: "outreach",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("unsafe content must not create a PR; got %d creates", created)
	}
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Fatalf("unsafe request was not quarantined as .rejected: %v", err)
	}
	result, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"docs/page.md:3", "PR body or commit trailer"} {
		if !strings.Contains(string(result), want) {
			t.Errorf("result %q does not contain %q", result, want)
		}
	}
}

func TestValidatePRRequestContentUsesExplicitBase(t *testing.T) {
	var comparePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		comparePath = r.URL.Path
		_, _ = io.WriteString(w, `{"files":[]}`)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.Default())

	err := c.validatePRRequestContent(context.Background(), PRRequest{Repo: "o/r", Base: "release", Head: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	if comparePath != "/repos/o/r/compare/release...fix" {
		t.Fatalf("compare path = %q", comparePath)
	}
}
