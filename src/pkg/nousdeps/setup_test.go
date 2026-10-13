package nousdeps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGit is a Runner that simulates the git steps fetchRepo issues: the
// checkout appears when "fetch" runs and "rev-parse HEAD" reports what the
// configured remote serves for the requested commit. It also answers the
// toolchain probes.
type fakeGit struct {
	mu sync.Mutex
	// serve maps a requested commit to the commit the remote actually has
	// at that ref; identity for an honest remote.
	serve map[string]string
	// failFetch makes the fetch step fail.
	failFetch bool
	// heads is the simulated HEAD per checkout dir.
	heads map[string]string
	calls []string
}

func newFakeGit() *fakeGit {
	return &fakeGit{serve: map[string]string{}, heads: map[string]string{}}
}

func (g *fakeGit) run(_ context.Context, dir, name string, args ...string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, name+" "+strings.Join(args, " "))
	switch {
	case name == "go" && args[0] == "version":
		return "go version go1.26.9 linux/amd64", nil
	case strings.HasSuffix(name, "python3") && args[0] == "--version":
		if strings.Contains(name, "venv") {
			return "", errors.New("no venv")
		}
		return "Python 3.12.3", nil
	case name == "git" && args[0] == "--version":
		return "git version 2.45.0", nil
	case name == "git" && args[0] == "-C":
		return testCommit2, nil // the installed Nous commit
	case name != "git":
		return "", fmt.Errorf("unexpected command %s", name)
	}
	switch args[0] {
	case "rev-parse":
		if h, ok := g.heads[dir]; ok {
			return h, nil
		}
		return "", errors.New("fatal: not a git repository")
	case "init", "remote", "checkout":
		return "", nil
	case "fetch":
		if g.failFetch {
			return "", errors.New("fatal: could not read from remote repository")
		}
		want := args[len(args)-1]
		got, ok := g.serve[want]
		if !ok {
			got = want
		}
		g.heads[dir] = got
		if err := os.WriteFile(filepath.Join(dir, "README"), []byte(got), 0o644); err != nil {
			return "", err
		}
		return "", nil
	}
	return "", fmt.Errorf("unexpected git %v", args)
}

// rewriteTransport sends every https request to the test server over plain
// http, so the allowlist and redirect checks see the real hostnames while no
// network is touched.
type rewriteTransport struct{ target string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = rt.target
	clone.Host = req.URL.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func writeCampaign(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "campaign.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func fixedNow() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }

func newTestServer(t *testing.T, trace []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host == "huggingface.co" && r.URL.Path == "/trace.jsonl":
			_, _ = w.Write(trace)
		case r.Host == "huggingface.co" && r.URL.Path == "/redirect-inside":
			http.Redirect(w, r, "https://huggingface.co/trace.jsonl", http.StatusFound)
		case r.Host == "huggingface.co" && r.URL.Path == "/redirect-outside":
			http.Redirect(w, r, "https://evil.example/trace.jsonl", http.StatusFound)
		case r.Host == "huggingface.co" && r.URL.Path == "/big":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 64))
		case r.Host == "huggingface.co" && r.URL.Path == "/missing":
			http.NotFound(w, r)
		default:
			http.Error(w, "unexpected "+r.Host+r.URL.Path, http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func baseOptions(t *testing.T, srv *httptest.Server, git *fakeGit) Options {
	t.Helper()
	root := t.TempDir()
	return Options{
		Root:      root,
		RunID:     "exp-1",
		Allowlist: NewAllowlist([]string{"github.com", "huggingface.co"}),
		NousDir:   "/opt/nous",
		Run:       git.run,
		HTTP:      &http.Client{Transport: rewriteTransport{target: strings.TrimPrefix(srv.URL, "http://")}},
		Now:       fixedNow,
		Getenv:    func(string) string { return "" },
	}
}

func TestSetupFetchesVerifiesAndRecords(t *testing.T) {
	trace := []byte(`{"req":1}` + "\n")
	srv := newTestServer(t, trace)
	git := newFakeGit()
	opts := baseOptions(t, srv, git)
	var logs []string
	opts.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	opts.CampaignPath = writeCampaign(t, `
campaign:
  name: blis-routing-v1
  dependencies:
    - repo: inference-sim/inference-sim
      commit: `+testCommit+`
    - url: https://huggingface.co/trace.jsonl
      sha256: `+sha256Hex(trace)+`
`)

	rec, err := Setup(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Schema != RecordSchema || rec.Campaign != "blis-routing-v1" || rec.RunID != "exp-1" || rec.RecordedAt != "2026-10-13T12:00:00Z" {
		t.Fatalf("record header = %+v", rec)
	}
	if len(rec.Dependencies) != 2 {
		t.Fatalf("dependencies = %+v", rec.Dependencies)
	}
	repo, file := rec.Dependencies[0], rec.Dependencies[1]
	wantRepo := filepath.Join(opts.Root, CacheSubdir, "repos", "github.com", "inference-sim", "inference-sim", testCommit)
	if repo.Kind != KindRepo || repo.Role != RoleTarget || repo.Pin != testCommit || repo.Resolved != testCommit || repo.Path != wantRepo || repo.Cached {
		t.Fatalf("repo = %+v", repo)
	}
	wantFile := filepath.Join(opts.Root, CacheSubdir, "files", sha256Hex(trace), "trace.jsonl")
	if file.Kind != KindFile || file.Role != RoleData || file.Resolved != sha256Hex(trace) || file.Path != wantFile || file.Bytes != int64(len(trace)) || file.Cached {
		t.Fatalf("file = %+v", file)
	}
	if got, _ := os.ReadFile(wantFile); !bytes.Equal(got, trace) {
		t.Fatalf("cached file = %q", got)
	}
	if rec.Toolchains[ToolchainNous] != testCommit2 || rec.Toolchains[ToolchainPython] != "Python 3.12.3" || rec.Toolchains[ToolchainGo] == "" || rec.Toolchains[ToolchainGit] != "git version 2.45.0" {
		t.Fatalf("toolchains = %v", rec.Toolchains)
	}
	if len(rec.FetchAllowlist) != 2 {
		t.Fatalf("allowlist = %v", rec.FetchAllowlist)
	}

	// The record and env file land under <root>/<run-id>/ and round-trip.
	back, err := ReadRecord(opts.Root, "exp-1")
	if err != nil || back.RecordPath != RecordPath(opts.Root, "exp-1") || len(back.Dependencies) != 2 {
		t.Fatalf("ReadRecord = %+v, %v", back, err)
	}
	env, err := os.ReadFile(EnvPath(opts.Root, "exp-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"NOUS_DEP_INFERENCE_SIM='" + wantRepo + "'",
		"NOUS_DEP_TRACE_JSONL='" + wantFile + "'",
		EnvTargetRepoPath + "='" + wantRepo + "'",
	} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env file missing %q:\n%s", want, env)
		}
	}
	if len(logs) != 2 || strings.Contains(logs[0], "cached") {
		t.Fatalf("logs = %q", logs)
	}

	// Second run: both hit the cache, nothing is re-fetched.
	before := len(git.calls)
	rec2, err := Setup(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !rec2.Dependencies[0].Cached || !rec2.Dependencies[1].Cached {
		t.Fatalf("second run not cached: %+v", rec2.Dependencies)
	}
	for _, c := range git.calls[before:] {
		if strings.HasPrefix(c, "git fetch") {
			t.Fatalf("cached repo was re-fetched: %v", git.calls[before:])
		}
	}
}

func TestSetupNoDependenciesStillRecordsToolchains(t *testing.T) {
	srv := newTestServer(t, nil)
	opts := baseOptions(t, srv, newFakeGit())
	opts.CampaignPath = writeCampaign(t, "campaign:\n  name: plain\n")
	rec, err := Setup(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Dependencies) != 0 || rec.Toolchains[ToolchainNous] != testCommit2 {
		t.Fatalf("record = %+v", rec)
	}
	env, _ := os.ReadFile(EnvPath(opts.Root, "exp-1"))
	if strings.Count(string(env), "\n") != 1 || !strings.HasPrefix(string(env), "#") {
		t.Fatalf("env file for no deps = %q", env)
	}
}

func TestSetupRefusals(t *testing.T) {
	trace := []byte("data\n")
	for _, tc := range []struct {
		name     string
		campaign string
		prepare  func(*fakeGit)
		want     error
		msg      string
	}{
		{
			name:     "host outside allowlist refused before any fetch",
			campaign: "dependencies:\n  - url: https://evil.example/trace.jsonl\n    sha256: " + sha256Hex(trace) + "\n",
			want:     ErrNotAllowlisted,
			msg:      `"evil.example"`,
		},
		{
			name:     "repo host outside allowlist",
			campaign: "dependencies:\n  - repo: https://gitlab.com/o/r\n    commit: " + testCommit + "\n",
			want:     ErrNotAllowlisted,
		},
		{
			name:     "second dependency bad means nothing is fetched",
			campaign: "dependencies:\n  - repo: o/r\n    commit: " + testCommit + "\n  - url: https://evil.example/x\n    sha256: " + sha256Hex(trace) + "\n",
			want:     ErrNotAllowlisted,
		},
		{
			name:     "unpinned refused",
			campaign: "dependencies:\n  - repo: o/r\n    commit: v1.2.3\n",
			want:     ErrUnpinned,
		},
		{
			name:     "sha256 mismatch",
			campaign: "dependencies:\n  - url: https://huggingface.co/trace.jsonl\n    sha256: " + testSHA256 + "\n",
			want:     ErrPinMismatch,
			msg:      "wanted sha256",
		},
		{
			name:     "redirect to allowlisted host is followed and verified",
			campaign: "dependencies:\n  - url: https://huggingface.co/redirect-inside\n    sha256: " + sha256Hex(trace) + "\n",
		},
		{
			name:     "redirect outside the allowlist refused",
			campaign: "dependencies:\n  - url: https://huggingface.co/redirect-outside\n    sha256: " + sha256Hex(trace) + "\n",
			want:     ErrNotAllowlisted,
			msg:      "redirect to https://evil.example/trace.jsonl refused",
		},
		{
			name:     "http error",
			campaign: "dependencies:\n  - url: https://huggingface.co/missing\n    sha256: " + sha256Hex(trace) + "\n",
			msg:      "HTTP 404",
		},
		{
			name:     "remote serves a different commit",
			campaign: "dependencies:\n  - repo: o/r\n    commit: " + testCommit + "\n",
			prepare:  func(g *fakeGit) { g.serve[testCommit] = testCommit2 },
			want:     ErrPinMismatch,
			msg:      "wanted commit",
		},
		{
			name:     "git fetch fails",
			campaign: "dependencies:\n  - repo: o/r\n    commit: " + testCommit + "\n",
			prepare:  func(g *fakeGit) { g.failFetch = true },
			msg:      "could not read from remote",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, trace)
			git := newFakeGit()
			if tc.prepare != nil {
				tc.prepare(git)
			}
			opts := baseOptions(t, srv, git)
			opts.CampaignPath = writeCampaign(t, tc.campaign)
			_, err := Setup(context.Background(), opts)
			if tc.want == nil && tc.msg == "" {
				if err != nil {
					t.Fatalf("Setup() = %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Setup() = nil, want refusal")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Setup() = %v, want %v", err, tc.want)
			}
			if tc.msg != "" && !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("Setup() = %v, want message containing %q", err, tc.msg)
			}
			if _, statErr := os.Stat(RecordPath(opts.Root, opts.RunID)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a refused setup must leave no record (stat err = %v)", statErr)
			}
			if tc.want == ErrNotAllowlisted {
				for _, c := range git.calls {
					if strings.HasPrefix(c, "git fetch") {
						t.Fatalf("allowlist refusal happened after a fetch: %v", git.calls)
					}
				}
			}
			// Nothing half-written survives in the cache.
			if entries, _ := filepath.Glob(filepath.Join(opts.Root, CacheSubdir, "files", "*", "*"+tmpSuffix)); len(entries) != 0 {
				t.Fatalf("temp download left behind: %v", entries)
			}
		})
	}
}

func TestSetupFileSizeCap(t *testing.T) {
	srv := newTestServer(t, nil)
	opts := baseOptions(t, srv, newFakeGit())
	opts.MaxFileBytes = 16
	body := bytes.Repeat([]byte("x"), 64)
	opts.CampaignPath = writeCampaign(t, "dependencies:\n  - url: https://huggingface.co/big\n    sha256: "+sha256Hex(body)+"\n")
	_, err := Setup(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("Setup() = %v, want size-cap error", err)
	}
}

func TestSetupStaleCheckoutIsReplaced(t *testing.T) {
	srv := newTestServer(t, nil)
	git := newFakeGit()
	opts := baseOptions(t, srv, git)
	opts.CampaignPath = writeCampaign(t, "dependencies:\n  - repo: o/r\n    commit: "+testCommit+"\n")
	dest := filepath.Join(opts.Root, CacheSubdir, "repos", "github.com", "o", "r", testCommit)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dest, "stale")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git.heads[dest] = testCommit2 // wrong commit sitting in the cache dir
	rec, err := Setup(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Dependencies[0].Cached || rec.Dependencies[0].Resolved != testCommit {
		t.Fatalf("stale checkout reused: %+v", rec.Dependencies[0])
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale content survived the re-fetch")
	}
}

func TestOptionsDefaults(t *testing.T) {
	env := map[string]string{"HIVE_NOUS_DATA_DIR": "/mnt/nous", EnvCacheDir: "/var/cache/agent", EnvNousDir: "/srv/nous-install"}
	o := Options{Getenv: func(k string) string { return env[k] }}
	o.defaults()
	if o.Root != "/mnt/nous" || o.RunID != "governor" || o.CacheDir != "/var/cache/agent/deps" || o.NousDir != "/srv/nous-install" {
		t.Fatalf("defaults from env = %+v", o)
	}
	if o.MaxFileBytes != DefaultMaxFileBytes || o.HTTP == nil || o.HTTP.Timeout != DefaultFetchTimeout || o.Run == nil || o.Now == nil || o.Logf == nil {
		t.Fatalf("zero-value defaults = %+v", o)
	}
	o = Options{Getenv: func(string) string { return "" }}
	o.defaults()
	if o.Root != "/data/nous" || o.CacheDir != filepath.Join("/data/nous", CacheSubdir) || o.NousDir != DefaultNousDir {
		t.Fatalf("defaults without env = %+v", o)
	}
	o = Options{}
	o.defaults()
	if o.Getenv == nil {
		t.Fatal("Getenv not defaulted")
	}
}

func TestExecRunner(t *testing.T) {
	out, err := ExecRunner(context.Background(), t.TempDir(), "git", "--version")
	if err != nil || !strings.HasPrefix(out, "git version") {
		t.Fatalf("ExecRunner(git --version) = %q, %v", out, err)
	}
	if _, err := ExecRunner(context.Background(), "", "definitely-not-a-command-nousdeps"); err == nil {
		t.Fatal("want error for a missing command")
	}
	if _, err := ExecRunner(context.Background(), t.TempDir(), "git", "rev-parse", "HEAD"); err == nil || !strings.Contains(err.Error(), "git rev-parse HEAD") {
		t.Fatalf("want wrapped error naming the command, got %v", err)
	}
}

func TestProbeToolchainsRecordsUnavailable(t *testing.T) {
	failing := func(context.Context, string, string, ...string) (string, error) { return "", errors.New("nope") }
	got := probeToolchains(context.Background(), Options{Run: failing, NousDir: "/opt/nous"})
	for _, k := range []string{ToolchainNous, ToolchainPython, ToolchainGo, ToolchainGit} {
		if got[k] != toolchainUnavailable {
			t.Fatalf("%s = %q, want %q", k, got[k], toolchainUnavailable)
		}
	}
}

func TestRecordEnvFileQuotesSafely(t *testing.T) {
	rec := Record{Dependencies: []Resolved{{Name: "it's", Role: RoleData, Path: "/p/it's here"}}}
	got := rec.EnvFileContent()
	if !strings.Contains(got, `NOUS_DEP_IT_S='/p/it'\''s here'`) {
		t.Fatalf("env file = %q", got)
	}
	if strings.Contains(got, EnvTargetRepoPath) {
		t.Fatal("no target → no NOUS_TARGET_REPO_PATH")
	}
}

func TestReadRecordErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadRecord(root, "none"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing record err = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RecordPath(root, "bad"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(root, "bad"); err == nil {
		t.Fatal("want parse error")
	}
}

func TestWriteRecordFailsOnUnwritableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file-not-dir")
	if err := os.WriteFile(root, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeRecord(Options{Root: root, RunID: "x"}, Record{})
	if err == nil {
		t.Fatal("want error when the campaign dir cannot be created")
	}
}
