package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/testutil"
)

// bin/gh-app-token.sh mints two very different things from the same GitHub App
// key: the FULL installation token (cached at /var/run/hive-metrics/
// gh-app-token.cache, hub-only, 0600) and the short-lived per-tier SCOPED
// tokens handed to contributor agents (#1860). Keeping those two apart is the
// whole point of the scoped-token scheme (#1861): an agent must never end up
// holding installation-wide credentials.
//
// The script is the only implementation of that split for native installs —
// dashboard/server.js shells out to `--scoped <tier>` and forwards the result
// to the contributor over the wire — so these tests exercise the real script
// rather than a paraphrase of it, following entrypoint_boot_test.go.

const ghAppTokenScriptPath = "../../../bin/gh-app-token.sh"

// scriptCacheToken is what the tests pre-seed the shared cache with. Any test
// that finds this string in a scoped mint's output has caught the full
// installation token leaking into an agent-facing path.
const scriptCacheToken = "ghs_FULL_PRIVILEGE_FROM_CACHE"

type scriptRun struct {
	stdout     string
	stderr     string
	exitCode   int
	cachePath  string
	scopedBody string // request body the scoped mint sent, if any
	modeAtLock string // mode the cache already had when the script chmod-ed it
}

// runGHAppTokenScript executes the real script against a temp filesystem with
// curl stubbed out, and reports what it printed, what it cached, and what it
// asked GitHub for.
//
// warmCache seeds the shared cache with scriptCacheToken before the run.
func runGHAppTokenScript(t *testing.T, warmCache bool, args ...string) scriptRun {
	t.Helper()

	src, err := os.ReadFile(ghAppTokenScriptPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "gh-app-token.sh not readable from this package: %v", err)
	}
	for _, tool := range []string{"bash", "openssl", "jq", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
	realChmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Skipf("chmod not available: %v", err)
	}

	root := t.TempDir()
	stubDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(root, "run", "hive-metrics", "gh-app-token.cache")
	bodyPath := filepath.Join(root, "scoped-request-body.json")
	modePath := filepath.Join(root, "mode-before-chmod")

	// Point the hard-coded cache path at the temp root. If this substring ever
	// stops matching, the tests below would silently run against the real
	// /var/run path (or nothing at all), so fail loudly instead.
	text := string(src)
	const cacheLiteral = "/var/run/hive-metrics/gh-app-token.cache"
	if !strings.Contains(text, cacheLiteral) {
		t.Fatalf("gh-app-token.sh no longer references %s; this test would cover nothing", cacheLiteral)
	}
	scriptPath := filepath.Join(root, "gh-app-token.sh")
	if err := os.WriteFile(scriptPath, []byte(strings.ReplaceAll(text, cacheLiteral, cachePath)), 0o755); err != nil {
		t.Fatal(err)
	}

	// curl stub: the scoped mint is the only caller that sends a -d body, so
	// that is how the two endpoints are told apart. The body is recorded so
	// tests can pin the per-tier permissions actually requested.
	curlStub := "#!/bin/bash\nbody=\"\"\nprev=\"\"\nfor a in \"$@\"; do\n" +
		"  [ \"$prev\" = \"-d\" ] && body=\"$a\"\n  prev=\"$a\"\ndone\n" +
		"if [ -n \"$body\" ]; then\n" +
		"  printf '%s' \"$body\" > " + bodyPath + "\n" +
		"  echo '{\"token\":\"ghs_SCOPED\",\"expires_at\":\"2026-08-16T12:00:00Z\"}'\n" +
		"else\n" +
		"  echo '{\"token\":\"ghs_FRESH_FULL\",\"expires_at\":\"2026-08-16T12:00:00Z\"}'\n" +
		"fi\n"
	if err := os.WriteFile(filepath.Join(stubDir, "curl"), []byte(curlStub), 0o755); err != nil {
		t.Fatal(err)
	}

	// The production script uses GNU stat flags because it runs on Linux.
	// Provide the tiny subset it needs so this package test is hermetic on
	// developer Macs too.
	statStub := "#!/bin/bash\nif [ \"$1\" = \"-c\" ]; then\n" +
		"  case \"$2\" in\n" +
		"    %Y) python3 -c 'import os,sys; print(int(os.stat(sys.argv[1]).st_mtime))' \"$3\" ;;\n" +
		"    %a) python3 -c 'import os,stat,sys; print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode))[2:])' \"$3\" ;;\n" +
		"    *) echo \"unsupported stat format: $2\" >&2; exit 1 ;;\n" +
		"  esac\n" +
		"else\n" +
		"  echo \"unsupported stat invocation: $*\" >&2; exit 1\n" +
		"fi\n"
	if err := os.WriteFile(filepath.Join(stubDir, "stat"), []byte(statStub), 0o755); err != nil {
		t.Fatal(err)
	}

	// chmod stub: records the mode the cache file already had when the script
	// got around to locking it down. That is the only way to observe the
	// create-then-chmod window from outside the race.
	chmodStub := "#!/bin/bash\nfor a in \"$@\"; do\n" +
		"  if [ -e \"$a\" ]; then stat -c %a \"$a\" > " + modePath + "; fi\ndone\n" +
		"exec " + realChmod + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "chmod"), []byte(chmodStub), 0o755); err != nil {
		t.Fatal(err)
	}

	// A throwaway key: never used against GitHub, only so `openssl dgst -sign`
	// succeeds and the script reaches the code paths under test.
	key, err := rsa.GenerateKey(rand.Reader, testRSAKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "gh-app-key.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	if warmCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cachePath, []byte(scriptCacheToken), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_APP_ID=12345",
		"GH_APP_INSTALLATION_ID=67890",
		"GH_APP_KEY_FILE="+keyPath,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	run := scriptRun{stdout: stdout.String(), stderr: stderr.String(), cachePath: cachePath}
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("running gh-app-token.sh: %v (stderr: %s)", runErr, run.stderr)
		}
		run.exitCode = exitErr.ExitCode()
	}
	if b, err := os.ReadFile(bodyPath); err == nil {
		run.scopedBody = string(b)
	}
	if b, err := os.ReadFile(modePath); err == nil {
		run.modeAtLock = strings.TrimSpace(string(b))
	}
	return run
}

func (r scriptRun) cacheContents(t *testing.T) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(r.cachePath)
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

// A warm shared cache must not satisfy a scoped request.
//
// The cache-validity check used to run ahead of the --scoped branch, so any
// scoped mint made while the cache was warm — which, on a running hub, is
// essentially always — short-circuited and printed the FULL unscoped
// installation token instead of the tier-scoped one. dashboard/server.js
// JSON.parses that output, so on native installs the failure surfaced as
// "Failed to mint token" and contributors were never assigned work; any caller
// that did not happen to parse it would have received installation-wide
// credentials in place of a newcomer's issues-only token.
func TestScopedMintIgnoresTheSharedFullTokenCache(t *testing.T) {
	run := runGHAppTokenScript(t, true, "--scoped", "newcomer")

	if run.exitCode != 0 {
		t.Fatalf("scoped mint failed: exit=%d stdout=%q stderr=%q", run.exitCode, run.stdout, run.stderr)
	}
	if strings.Contains(run.stdout, scriptCacheToken) {
		t.Fatalf("scoped mint served the full installation token from the shared cache: %q", run.stdout)
	}

	var got struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(run.stdout)), &got); err != nil {
		t.Fatalf("scoped output is not the JSON its caller parses (%v): %q", err, run.stdout)
	}
	if got.Token != "ghs_SCOPED" {
		t.Fatalf("token = %q, want the freshly scoped token", got.Token)
	}
	if got.ExpiresAt == "" {
		t.Fatal("expires_at is empty; the refresh timer has nothing to schedule against")
	}

	if cached, ok := run.cacheContents(t); !ok || cached != scriptCacheToken {
		t.Fatalf("scoped mint disturbed the shared cache: %q (present=%v)", cached, ok)
	}
}

// A scoped request must not mint or cache a full-privilege token as a side
// effect. Asking for a newcomer's issues-only token is not a reason to put
// installation-wide credentials on disk.
func TestScopedMintDoesNotPopulateTheSharedCache(t *testing.T) {
	run := runGHAppTokenScript(t, false, "--scoped", "contributor")

	if run.exitCode != 0 {
		t.Fatalf("scoped mint failed: exit=%d stdout=%q stderr=%q", run.exitCode, run.stdout, run.stderr)
	}
	if cached, ok := run.cacheContents(t); ok {
		t.Fatalf("scoped mint wrote the shared full-token cache: %q", cached)
	}
}

// The permissions each tier asks GitHub for are the scoped-token scheme's
// actual security boundary, so pin them.
func TestScopedMintRequestsTierPermissions(t *testing.T) {
	cases := []struct {
		tier string
		want string
	}{
		{"newcomer", `{"permissions":{"issues":"write","metadata":"read"}}`},
		{"contributor", `{"permissions":{"issues":"write","contents":"write","pull_requests":"write","metadata":"read"}}`},
		{"trusted", `{"permissions":{"issues":"write","contents":"write","pull_requests":"write","metadata":"read","checks":"read"}}`},
		{"advisor", `{"permissions":{"issues":"read","metadata":"read"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			run := runGHAppTokenScript(t, true, "--scoped", tc.tier)
			if run.exitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", run.exitCode, run.stderr)
			}
			if run.scopedBody != tc.want {
				t.Fatalf("request body =\n  %s\nwant\n  %s", run.scopedBody, tc.want)
			}
		})
	}
}

// An unknown tier must fail rather than fall through to something broader.
// While the cache check ran first, `--scoped bogus` on a warm cache exited 0
// with the full token; the tier validation was unreachable.
func TestScopedMintRejectsUnknownTier(t *testing.T) {
	run := runGHAppTokenScript(t, true, "--scoped", "bogus")

	if run.exitCode == 0 {
		t.Fatalf("unknown tier accepted: stdout=%q", run.stdout)
	}
	if strings.Contains(run.stdout, scriptCacheToken) {
		t.Fatalf("unknown tier leaked the cached full token: %q", run.stdout)
	}
	if !strings.Contains(run.stderr, "unknown tier: bogus") {
		t.Fatalf("stderr = %q, want an unknown-tier error", run.stderr)
	}
}

// Repo scoping narrows a tier token to named repositories; the list has to
// reach GitHub as a JSON array.
func TestScopedMintPassesRepositoryRestriction(t *testing.T) {
	run := runGHAppTokenScript(t, true, "--scoped", "advisor", "kubestellar/hive,kubestellar/ui")

	if run.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", run.exitCode, run.stderr)
	}
	want := `{"permissions":{"issues":"read","metadata":"read"},"repositories":["kubestellar/hive","kubestellar/ui"]}`
	if run.scopedBody != want {
		t.Fatalf("request body =\n  %s\nwant\n  %s", run.scopedBody, want)
	}
}

// The full token belongs in the cache file, not in stdout — that is what the
// token-handling hardening in #1757 established, and what every in-tree caller
// (hive-config.sh, entrypoint.sh) relies on by discarding stdout and reading
// the file. The warm-cache path had kept echoing it.
func TestBareInvocationNeverPrintsTheToken(t *testing.T) {
	for _, warm := range []bool{true, false} {
		name := "cold cache"
		if warm {
			name = "warm cache"
		}
		t.Run(name, func(t *testing.T) {
			run := runGHAppTokenScript(t, warm)
			if run.exitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", run.exitCode, run.stderr)
			}
			for _, secret := range []string{scriptCacheToken, "ghs_FRESH_FULL"} {
				if strings.Contains(run.stdout, secret) {
					t.Fatalf("token printed to stdout: %q", run.stdout)
				}
			}
			if !strings.Contains(run.stdout, "token cached at") {
				t.Fatalf("stdout = %q, want the cache-location line", run.stdout)
			}
		})
	}
}

// --export is the one documented way to get the token out of the script, and
// it has to work on both paths — a cold cache used to print only the
// cache-location line, so `eval "$(gh-app-token.sh --export)"` exported
// nothing on the very first call after boot.
func TestExportEmitsShellExportOnBothPaths(t *testing.T) {
	t.Run("warm cache", func(t *testing.T) {
		run := runGHAppTokenScript(t, true, "--export")
		if got, want := strings.TrimSpace(run.stdout), "export GH_TOKEN="+scriptCacheToken; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})
	t.Run("cold cache", func(t *testing.T) {
		run := runGHAppTokenScript(t, false, "--export")
		if got, want := strings.TrimSpace(run.stdout), "export GH_TOKEN=ghs_FRESH_FULL"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})
}

// The shared cache holds an installation-wide token on a box that also runs
// agent UIDs, so it must never exist at a readable mode — not even for the
// instant between creation and chmod. Writing at the process umask and
// tightening afterwards left exactly that window open (#1861, item 2).
func TestSharedCacheIsCreatedPrivate(t *testing.T) {
	run := runGHAppTokenScript(t, false)
	if run.exitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", run.exitCode, run.stderr)
	}

	info, err := os.Stat(run.cachePath)
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache mode = %#o, want 0600", got)
	}

	// The chmod stub recorded the mode the file already had when the script
	// locked it down; anything wider than 0600 there is the race window.
	if run.modeAtLock == "" {
		t.Fatal("chmod stub recorded nothing; the cache is no longer chmod-ed and this test would cover nothing")
	}
	if run.modeAtLock != "600" {
		t.Fatalf("cache existed at mode %s before chmod; it must be created 0600", run.modeAtLock)
	}
}
