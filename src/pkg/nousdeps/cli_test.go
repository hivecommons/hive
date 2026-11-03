package nousdeps

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		msg  string
	}{
		{"no args", nil, "--campaign is required"},
		{"help", []string{"-h"}, "usage: hive nous-setup"},
		{"unknown flag", []string{"--bogus"}, "flag provided but not defined"},
		{"non-positive timeout", []string{"--campaign", "x.yaml", "--timeout", "0s"}, "--timeout must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(tc.args, &stdout, &stderr); code != exitUsage {
				t.Fatalf("Run(%v) = %d, want %d; stderr %q", tc.args, code, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.msg) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.msg)
			}
		})
	}
}

func TestRunWithSuccessAndJSON(t *testing.T) {
	trace := []byte("row\n")
	srv := newTestServer(t, trace)
	git := newFakeGit()
	base := baseOptions(t, srv, git)
	base.Allowlist = nil // let the flag / env tier decide
	campaign := writeCampaign(t, "dependencies:\n  - repo: o/r\n    commit: "+testCommit+"\n  - url: https://huggingface.co/trace.jsonl\n    sha256: "+sha256Hex(trace)+"\n")

	var stdout, stderr bytes.Buffer
	code := RunWith(context.Background(), []string{
		"--campaign", campaign, "--run-id", "exp-2", "--root", base.Root,
		"--cache-dir", filepath.Join(base.Root, "custom-cache"),
		"--allowlist", "github.com,huggingface.co", "--json",
	}, &stdout, &stderr, base)
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var rec Record
	if err := json.Unmarshal(stdout.Bytes(), &rec); err != nil {
		t.Fatalf("stdout is not a record: %v\n%s", err, stdout.String())
	}
	if rec.RunID != "exp-2" || len(rec.Dependencies) != 2 || !strings.HasPrefix(rec.Dependencies[0].Path, filepath.Join(base.Root, "custom-cache")) {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.Contains(stderr.String(), "allowlist: github.com, huggingface.co") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(RecordPath(base.Root, "exp-2")); err != nil {
		t.Fatal(err)
	}

	// Plain (non-JSON) output names the record path.
	stdout.Reset()
	stderr.Reset()
	code = RunWith(context.Background(), []string{"--campaign", campaign, "--run-id", "exp-2", "--root", base.Root, "--allowlist", "github.com,huggingface.co"}, &stdout, &stderr, base)
	if code != exitOK || !strings.Contains(stdout.String(), "2 dependencies verified; recorded in "+RecordPath(base.Root, "exp-2")) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestRunWithAllowlistFromEnvDeniesByDefault(t *testing.T) {
	trace := []byte("row\n")
	srv := newTestServer(t, trace)
	base := baseOptions(t, srv, newFakeGit())
	base.Allowlist = nil
	// The hive exported the default list; huggingface.co is not on it.
	base.Getenv = func(k string) string {
		if k == "HIVE_NOUS_FETCH_ALLOWLIST" {
			return "github.com"
		}
		return ""
	}
	campaign := writeCampaign(t, "dependencies:\n  - url: https://huggingface.co/trace.jsonl\n    sha256: "+sha256Hex(trace)+"\n")
	var stdout, stderr bytes.Buffer
	code := RunWith(context.Background(), []string{"--campaign", campaign, "--root", base.Root}, &stdout, &stderr, base)
	if code != exitFail {
		t.Fatalf("code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), "refusing to start campaign") || !strings.Contains(stderr.String(), `"huggingface.co"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(RecordPath(base.Root, "governor")); !os.IsNotExist(err) {
		t.Fatalf("record must not exist after refusal (err = %v)", err)
	}

	// Explicitly empty env list (deny all) refuses even github.com.
	base.Getenv = func(k string) string {
		if k == "HIVE_NOUS_FETCH_ALLOWLIST" {
			return ","
		}
		return ""
	}
	campaign = writeCampaign(t, "dependencies:\n  - repo: o/r\n    commit: "+testCommit+"\n")
	stderr.Reset()
	if code := RunWith(context.Background(), []string{"--campaign", campaign, "--root", base.Root}, &stdout, &stderr, base); code != exitFail || !strings.Contains(stderr.String(), "fetch_allowlist is empty") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestRunWithUsesProcessEnvWhenNoGetenv(t *testing.T) {
	t.Setenv("HIVE_NOUS_FETCH_ALLOWLIST", "nowhere.example")
	t.Setenv("HIVE_NOUS_DATA_DIR", t.TempDir())
	campaign := writeCampaign(t, "dependencies:\n  - repo: o/r\n    commit: "+testCommit+"\n")
	var stdout, stderr bytes.Buffer
	code := RunWith(context.Background(), []string{"--campaign", campaign}, &stdout, &stderr, Options{Run: newFakeGit().run, HTTP: &http.Client{}})
	if code != exitFail || !strings.Contains(stderr.String(), "allowed: nowhere.example") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}
