package commands

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestBackfillFetcherDefaultSleepHonoursContext(t *testing.T) {
	f := newBackfillFetcher("https://api.example.test/", "tok")
	if f.base != "https://api.example.test" {
		t.Fatalf("base = %q, want trailing slash trimmed", f.base)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.sleep(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("cancelled sleep err = %v", err)
	}
	if err := f.sleep(context.Background(), time.Nanosecond); err != nil {
		t.Fatalf("short sleep err = %v", err)
	}
}

func TestBackfillFetcherGetErrorBranches(t *testing.T) {
	t.Run("rate limit beyond max wait", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "86400")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		f := newBackfillFetcher(srv.URL, "")
		f.sleep = func(context.Context, time.Duration) error { t.Fatal("must not sleep"); return nil }
		var out []backfillPR
		err := f.get(context.Background(), "/repos/o/r/pulls", nil, &out)
		if err == nil || !strings.Contains(err.Error(), "rate limit exhausted") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("sleep error surfaces", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		f := newBackfillFetcher(srv.URL, "")
		f.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
		var out []backfillPR
		if err := f.get(context.Background(), "/repos/o/r/pulls", nil, &out); err != context.DeadlineExceeded {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("long error body truncated", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, strings.Repeat("x", 500), http.StatusBadGateway)
		}))
		defer srv.Close()
		f := newBackfillFetcher(srv.URL, "")
		var out []backfillPR
		err := f.get(context.Background(), "/repos/o/r/pulls", nil, &out)
		if err == nil || !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "…") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{not json`))
		}))
		defer srv.Close()
		f := newBackfillFetcher(srv.URL, "")
		var out []backfillPR
		if err := f.get(context.Background(), "/repos/o/r/pulls", nil, &out); err == nil {
			t.Fatal("expected decode error")
		}
	})
	t.Run("unreachable server", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		f := newBackfillFetcher(srv.URL, "")
		var out []backfillPR
		if err := f.get(context.Background(), "/repos/o/r/pulls", nil, &out); err == nil {
			t.Fatal("expected transport error")
		}
	})
	t.Run("bad url", func(t *testing.T) {
		f := newBackfillFetcher("http://[::1", "")
		var out []backfillPR
		if err := f.get(context.Background(), "/x", nil, &out); err == nil {
			t.Fatal("expected request build error")
		}
	})
}

func TestBackfillMergedBy(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		switch r.URL.Path {
		case "/repos/o/r/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"merge_commit_sha":"abc123","merged_by":{"login":"octocat"}}`))
		default:
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	f := newBackfillFetcher(srv.URL, "")
	pr := &backfillPR{Number: 7}
	if err := f.mergedBy(context.Background(), "o/r", pr); err != nil {
		t.Fatal(err)
	}
	if pr.MergedBy == nil || pr.MergedBy.Login != "octocat" || pr.MergeCommitSHA != "abc123" {
		t.Fatalf("pr = %+v", pr)
	}
	kept := &backfillPR{Number: 7, MergeCommitSHA: "keep"}
	if err := f.mergedBy(context.Background(), "o/r", kept); err != nil || kept.MergeCommitSHA != "keep" {
		t.Fatalf("existing merge sha overwritten: %+v err=%v", kept, err)
	}
	if err := f.mergedBy(context.Background(), "o/r", &backfillPR{Number: 8}); err == nil {
		t.Fatal("expected error from single-PR endpoint")
	}
}

func TestWriteBackfillBundleErrors(t *testing.T) {
	dir := t.TempDir()
	if err := writeBackfillBundle(filepath.Join(dir, "x.json"), &evidence.Bundle{}, nil); err == nil {
		t.Fatal("invalid bundle must not be written")
	}
	now := time.Now().UTC().Truncate(time.Second)
	pr := backfillPR{Number: 3, CreatedAt: now.Add(-time.Hour)}
	pr.User.Login = "alice"
	pr.Head.SHA = strings.Repeat("a", 40)
	pr.Base.SHA = strings.Repeat("b", 40)
	b := backfillBundle("o/r", pr, now, &evidenceBackfillOptions{}, nil)

	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeBackfillBundle(filepath.Join(blocker, "sub", "x.json"), b, nil); err == nil {
		t.Fatal("mkdir under a file must fail")
	}
	if err := writeBackfillBundle(filepath.Join(dir, "d"), b, nil); err != nil {
		t.Fatal(err)
	}
	// Renaming a file over a non-empty directory fails.
	if err := os.MkdirAll(filepath.Join(dir, "d2", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBackfillBundle(filepath.Join(dir, "d2"), b, nil); err == nil {
		t.Fatal("rename over directory must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "d2.tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp file not cleaned up: %v", err)
	}
}

func TestAddBacklinkDeduplicates(t *testing.T) {
	target := &evidence.Bundle{}
	if !addBacklink(target, "a", true) || addBacklink(target, "a", true) {
		t.Fatal("revert backlink should be added once")
	}
	if !addBacklink(target, "b", false) || addBacklink(target, "b", false) {
		t.Fatal("followup backlink should be added once")
	}
	if len(target.RevertedBy) != 1 || len(target.FollowedUpBy) != 1 {
		t.Fatalf("target = %+v", target)
	}
}

func TestValidateBackfillOptionsBadUntil(t *testing.T) {
	_, _, err := validateBackfillOptions(&evidenceBackfillOptions{repo: "o/r", since: "2026-01-01", until: "soon"})
	if err == nil || !strings.Contains(err.Error(), "--until") {
		t.Fatalf("err = %v", err)
	}
	_, _, err = validateBackfillOptions(&evidenceBackfillOptions{repo: "o/r/extra", since: "2026-01-01"})
	if err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("err = %v", err)
	}
	since, until, err := validateBackfillOptions(&evidenceBackfillOptions{repo: " o/r ", since: "2026-01-01", until: "2026-02-01T00:00:00Z"})
	if err != nil || since.Format("2006-01-02") != "2026-01-01" || until.Format("2006-01-02") != "2026-02-01" {
		t.Fatalf("since=%v until=%v err=%v", since, until, err)
	}
}

func TestEvidenceBackfillRejectsInvalidOutput(t *testing.T) {
	api, _ := backfillAPI(t)
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	cmd := NewRootCommand(strings.NewReader(""), &strings.Builder{}, &strings.Builder{})
	cmd.SetArgs([]string{"--output", "xml", "evidence", "backfill", "--repo", "acme/widgets", "--since", "2026-01-01", "--github-api", api.URL})
	err := cmd.Execute()
	if ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("err = %v", err)
	}
}

func TestEvidenceBackfillSigningKey(t *testing.T) {
	api, _ := backfillAPI(t)
	root := t.TempDir()
	bad := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(bad, []byte("not a key!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runBackfill(t, api, "--since", "2026-01-01", "--root", root, "--signing-key-file", bad)
	if err == nil || !strings.Contains(err.Error(), "--signing-key-file") {
		t.Fatalf("err = %v", err)
	}

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "good.key")
	if err := os.WriteFile(good, []byte(hex.EncodeToString(priv.Seed())), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, _, err := runBackfill(t, api, "--since", "2026-01-01", "--until", "2027-01-01", "--root", root, "--signing-key-file", good, "--skip-mergers")
	if err != nil {
		t.Fatal(err)
	}
	if summary["written"].(float64) == 0 || summary["until"] == "now" {
		t.Fatalf("summary = %v", summary)
	}
	var signed int
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, _ error) error {
		if d == nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		b, err := ghpkg.LoadReviewEvidence(path)
		if err != nil {
			t.Fatal(err)
		}
		if !b.Backfilled {
			t.Errorf("%s: backfilled marker missing", path)
		}
		if b.Signature != "" {
			signed++
		}
		if b.Merge != nil && b.Merge.Actor != "unrecorded" {
			t.Errorf("%s: --skip-mergers must leave actor unrecorded, got %q", path, b.Merge.Actor)
		}
		return nil
	})
	if signed == 0 {
		t.Fatal("no bundle carried a signature")
	}
}

func TestEvidenceBackfillMergerErrorIsWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/widgets/pulls" && r.URL.Query().Get("page") == "1":
			data, err := os.ReadFile(filepath.Join("testdata", "evidence_backfill", "pulls_page1.json"))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(data)
		case r.URL.Path == "/repos/acme/widgets/pulls":
			_, _ = w.Write([]byte("[]"))
		default:
			http.Error(w, `{"message":"flaky"}`, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	summary, stderr, err := runBackfill(t, srv, "--since", "2026-01-01", "--root", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if summary["merger_errors"].(float64) == 0 || !strings.Contains(stderr, "merger not fetched") {
		t.Fatalf("merger_errors = %v stderr = %q", summary["merger_errors"], stderr)
	}
}

func TestEvidenceBackfillReportsWriteErrors(t *testing.T) {
	api, _ := backfillAPI(t)
	root := filepath.Join(t.TempDir(), "root-is-a-file")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, stderr, err := runBackfill(t, api, "--since", "2026-01-01", "--root", root, "--skip-mergers")
	if err == nil || !strings.Contains(err.Error(), "not written") {
		t.Fatalf("err = %v", err)
	}
	if summary["write_errors"].(float64) == 0 || !strings.Contains(stderr, "warning:") {
		t.Fatalf("write_errors = %v stderr = %q", summary["write_errors"], stderr)
	}
}
