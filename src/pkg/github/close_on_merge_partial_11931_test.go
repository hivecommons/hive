package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestIssueCitedPaths11931(t *testing.T) {
	body := "## Finding\n\n**Affected area**: `.github/workflows/mission-safety-scan.yml` L48 and `scan-missions.yml:60`.\n" +
		"Also `./scripts/scan-pr.mjs#L12`, the `.github/workflows/` dir, and `Makefile`.\n" +
		"Not paths: `hive/likely-done`, `owner/repo#12`, `opts.Logger`, `v1.2.3`, `https://x.invalid/a.go`, `go test ./...`, `continue-on-error`.\n" +
		"Duplicate: `scan-missions.yml`."
	want := []string{
		".github/workflows/mission-safety-scan.yml",
		"scan-missions.yml",
		"scripts/scan-pr.mjs",
		".github/workflows/",
		"Makefile",
	}
	if got := issueCitedPaths(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("issueCitedPaths = %#v, want %#v", got, want)
	}
	if got := issueCitedPaths("no code spans here"); got != nil {
		t.Fatalf("issueCitedPaths(no spans) = %#v, want nil", got)
	}
}

func TestUntouchedCitedPaths11931(t *testing.T) {
	cited := []string{".github/workflows/scan.yml", "scan-pr.mjs", "pkg/x.go", "docs/", "other.go"}
	changed := []string{"scripts/scan-pr.mjs", "src/pkg/x.go", "docs/a/b.md"}
	want := []string{".github/workflows/scan.yml", "other.go"}
	if got := untouchedCitedPaths(cited, changed); !reflect.DeepEqual(got, want) {
		t.Fatalf("untouchedCitedPaths = %#v, want %#v", got, want)
	}
	if got := untouchedCitedPaths([]string{"x.go"}, []string{"ax.go"}); !reflect.DeepEqual(got, []string{"x.go"}) {
		t.Fatalf("suffix match must be slash-bounded, got %#v", got)
	}
}

func TestPRBodyHasRemainderSignal11931(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"Fixes #7\n\nTightens the regexes.", false},
		{"Fixes #7\n\n## What remains\n\n- the workflow soft-bypass", true},
		{"Fixes #7\n\nThe workflow change is out of scope: this agent tier cannot edit workflows.", true},
		{"Fixes #7\n\nThe remainder needs a maintainer.", true},
		{"Fixes #7\n\n`remainder` only in code", false},
	} {
		if got := prBodyHasRemainderSignal(tc.body); got != tc.want {
			t.Errorf("prBodyHasRemainderSignal(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestCloseOnMergeCoveragePartial11931(t *testing.T) {
	for _, tc := range []struct {
		name string
		cov  closeOnMergeCoverage
		want bool
	}{
		{"bare reference is partial", closeOnMergeCoverage{Closing: false}, true},
		{"closing with no cited paths is done", closeOnMergeCoverage{Closing: true}, false},
		{"remainder paragraph is partial", closeOnMergeCoverage{Closing: true, Remainder: true}, true},
		{"cited path touched", closeOnMergeCoverage{Closing: true, Cited: []string{"a.go", "b.go"}, FilesKnown: true, Untouched: []string{"b.go"}}, false},
		{"no cited path touched", closeOnMergeCoverage{Closing: true, Cited: []string{"a.go"}, FilesKnown: true, Untouched: []string{"a.go"}}, true},
		{"file list unknown falls back", closeOnMergeCoverage{Closing: true, Cited: []string{"a.go"}}, false},
	} {
		if got := tc.cov.partial(); got != tc.want {
			t.Errorf("%s: partial = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCloseOnMergeLabelChoice11931(t *testing.T) {
	const issueBody = "## Finding\n\n**Affected area**: `.github/workflows/mission-safety-scan.yml`"
	for _, tc := range []struct {
		name          string
		prBody        string
		files         []string
		filesStatus   int
		wantAction    CloseOnMergeAction
		wantLabel     string
		wantRemoveLD  bool
		wantComment   bool
		wantFilesRead bool
	}{
		{name: "bare ref", prBody: "Refs #7", wantAction: CloseOnMergeSkipped, wantLabel: PartiallyAddressedLabel, wantRemoveLD: true},
		{name: "closing but cited file untouched", prBody: "Fixes #7", files: []string{"scripts/scan.mjs"}, wantAction: CloseOnMergeSkipped, wantLabel: PartiallyAddressedLabel, wantRemoveLD: true, wantFilesRead: true},
		{name: "closing and cited file changed", prBody: "Fixes #7", files: []string{".github/workflows/mission-safety-scan.yml"}, wantAction: CloseOnMergeAwaiting, wantLabel: LikelyDoneLabel, wantComment: true, wantFilesRead: true},
		{name: "closing with remainder paragraph", prBody: "Fixes #7\n\n## What remains\n\n- the workflow", wantAction: CloseOnMergeSkipped, wantLabel: PartiallyAddressedLabel, wantRemoveLD: true},
		{name: "files fetch failure falls back", prBody: "Fixes #7", filesStatus: http.StatusUnprocessableEntity, wantAction: CloseOnMergeAwaiting, wantLabel: LikelyDoneLabel, wantComment: true, wantFilesRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var added []string
			var removed []string
			var posted []string
			filesRead := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "body": issueBody,
						"user": map[string]string{"login": "human", "type": "User"}, "labels": []map[string]string{{"name": "needs-human"}, {"name": LikelyDoneLabel}}})
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
					_ = json.NewEncoder(w).Encode([]map[string]string{})
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/timeline":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/42/files":
					filesRead = true
					if tc.filesStatus != 0 {
						w.WriteHeader(tc.filesStatus)
						return
					}
					out := make([]map[string]string, 0, len(tc.files))
					for _, f := range tc.files {
						out = append(out, map[string]string{"filename": f})
					}
					_ = json.NewEncoder(w).Encode(out)
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
					var req map[string]string
					_ = json.NewDecoder(r.Body).Decode(&req)
					posted = append(posted, req["body"])
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": req["body"]})
				case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
					_ = json.NewEncoder(w).Encode(map[string]string{"name": "x"})
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/labels":
					var req []string
					_ = json.NewDecoder(r.Body).Decode(&req)
					added = append(added, req...)
					_ = json.NewEncoder(w).Encode([]map[string]string{})
				case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/7/labels/"):
					removed = append(removed, r.URL.Path)
					_ = json.NewEncoder(w).Encode([]map[string]string{})
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"r"})
			res := c.closeIssueForMergedPRClaim(context.Background(), "o/r", 7, mergedTestPR(42, "hive[bot]", "scanner/fix-7", tc.prBody), true, CloseOnMergeOptions{
				Identity: HiveIdentity{AppLogin: "hive[bot]"},
			})
			if res.Action != tc.wantAction {
				t.Fatalf("action = %s (%s), want %s", res.Action, res.Reason, tc.wantAction)
			}
			if !reflect.DeepEqual(added, []string{tc.wantLabel}) {
				t.Fatalf("labels added = %v, want [%s]", added, tc.wantLabel)
			}
			removedLD := false
			for _, p := range removed {
				if strings.Contains(p, "likely-done") {
					removedLD = true
				}
			}
			if removedLD != tc.wantRemoveLD {
				t.Fatalf("likely-done removed = %v (%v), want %v", removedLD, removed, tc.wantRemoveLD)
			}
			if (len(posted) == 1) != tc.wantComment || len(posted) > 1 {
				t.Fatalf("posted comments = %#v, want awaiting comment %v", posted, tc.wantComment)
			}
			if filesRead != tc.wantFilesRead {
				t.Fatalf("files read = %v, want %v", filesRead, tc.wantFilesRead)
			}
		})
	}
}

func TestPartiallyAddressedSuppressesLikelyDoneLabel11931(t *testing.T) {
	issue := Issue{Repo: "o/r", Number: 7, Labels: []string{PartiallyAddressedLabel, LikelyDoneLabel}}
	annotateIssueWithClaim(&issue, IssueClaim{Repo: "o/r", Issue: 7, PRRepo: "o/r", PRNumber: 42, MergedPR: true})
	if issueHasLabel(issue.Labels, LikelyDoneLabel) {
		t.Fatalf("likely-done survived partially-addressed suppression: %v", issue.Labels)
	}
	if !issueHasLabel(issue.Labels, PartiallyAddressedLabel) {
		t.Fatalf("partially-addressed label was removed: %v", issue.Labels)
	}
}

func TestRefsRemainderCommentListsUntouchedPaths11931(t *testing.T) {
	merged := []mergedPRRef{{Number: 42, Title: "fix", URL: "https://github.com/o/r/pull/42", Remainder: "workflow change"}}
	body := renderRefsRemainderComment(merged, true, []string{".github/workflows/a.yml", "b.go"})
	if !strings.HasPrefix(body, taskListSweepMarker) || !strings.Contains(body, PartiallyAddressedLabel) {
		t.Fatalf("partial comment missing marker or label:\n%s", body)
	}
	if !strings.Contains(body, "Not yet touched: `.github/workflows/a.yml`, `b.go`\n") {
		t.Fatalf("partial comment missing untouched paths:\n%s", body)
	}
	if blocks := sweepCommentPRBlocks(body); strings.Contains(blocks[42], "Not yet touched") {
		t.Fatalf("untouched line leaked into the PR block: %q", blocks[42])
	}
	plain := renderRefsRemainderComment(merged, false, nil)
	if strings.Contains(plain, "Not yet touched") || !strings.Contains(plain, "without closing it") {
		t.Fatalf("plain comment changed:\n%s", plain)
	}
	progress := renderProgressComment([]taskListItem{{Text: "one"}}, merged, []string{"b.go"})
	if !strings.Contains(progress, "Not yet touched: `b.go`") {
		t.Fatalf("progress comment missing untouched paths:\n%s", progress)
	}
}
