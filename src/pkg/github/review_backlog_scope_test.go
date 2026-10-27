package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestReviewBacklogBlocksOnlyRelatedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, files, file        string
		missing, failed, blocked bool
	}{
		{name: "untouched", files: `[{"filename":"other.go"}]`},
		{name: "changed", files: `[{"filename":"pkg/a.go"}]`, blocked: true},
		{name: "dot-relative finding path", files: `[{"filename":"pkg/a.go"}]`, file: "./pkg/a.go", blocked: true},
		{name: "root-relative finding path", files: `[{"filename":"pkg/a.go"}]`, file: "/pkg/a.go", blocked: true},
		{name: "renamed", files: `[{"filename":"new.go","previous_filename":"pkg/a.go"}]`, blocked: true},
		{name: "missing on base", missing: true, blocked: true},
		{name: "lookup failed", failed: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withReviewBacklogState(t)
			file := tc.file
			if file == "" {
				file = "pkg/a.go"
			}
			lookups := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				// Clean so a "./pkg/a.go" finding still resolves the base probe's contents lookup.
				switch path.Clean(r.URL.Path) {
				case "/repos/o/r/issues/5/comments":
					fmt.Fprint(w, `{"id":1}`)
				case "/repos/o/r/pulls/5":
					fmt.Fprint(w, `{"base":{"ref":"v5"}}`)
				case "/repos/o/r/contents/pkg/a.go":
					if tc.missing {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					fmt.Fprint(w, `{"type":"file","path":"pkg/a.go"}`)
				case "/repos/o/r/pulls/5/files":
					lookups++
					if tc.failed {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					fmt.Fprint(w, tc.files)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			sink := &fakeBacklogSink{dest: config.ReviewBacklogLinear}
			c := headsBacklogClient(t, srv.URL, sink)
			report := routingReport(t, 5, "abc", nit("first", "low", file, 7, "evidence"), nit("second", "low", file, 8, "other evidence"))
			if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if len(sink.created) != 2 {
				t.Fatalf("created %d items", len(sink.created))
			}
			for _, item := range sink.created {
				if got := strings.Contains(strings.Join(item.Labels, ","), BlockedLabel); got != tc.blocked {
					t.Fatalf("labels %v, blocked want %v", item.Labels, tc.blocked)
				}
				if got := strings.Contains(item.Body, BlockedOnMarker("o/r", 5)); got != tc.blocked {
					t.Fatalf("block marker = %v, want %v", got, tc.blocked)
				}
				if tc.failed && strings.Contains(item.Body, "is about code that PR is changing") {
					t.Fatal("unknown file asserted changed")
				}
			}
			wantLookups := 1
			if tc.missing {
				wantLookups = 0
			}
			if lookups != wantLookups {
				t.Fatalf("lookups %d, want %d", lookups, wantLookups)
			}
			ledger, err := loadReviewBacklog("")
			if err != nil {
				t.Fatal(err)
			}
			for _, rec := range ledger.Fingerprints[reviewBacklogRepoKey("o/r")] {
				if rec.BlockedOnPR != 5 {
					t.Fatal("lost source PR provenance")
				}
				if got := rec.BlockState == reviewBacklogBlockBlocked; got != tc.blocked {
					t.Fatalf("block state %q", rec.BlockState)
				}
			}
		})
	}
}

func TestReviewBacklogChangedFilesPagination(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"filename":"second.go"}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/o/r/pulls/5/files?page=2>; rel="next"`, r.Host))
		fmt.Fprint(w, `[{"filename":"first.go"}]`)
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	probe := c.newReviewBacklogChangedFilesProbe("o/r", 5)
	if changed, known := probe(context.Background(), "second.go"); !changed || !known {
		t.Fatal("missed second page")
	}
	if changed, known := probe(context.Background(), "untouched.go"); changed || !known {
		t.Fatal("untouched file not known")
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}
