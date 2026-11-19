package spoke

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testGHEProxyIP = "129.34.104.21"

func gheAliases() []HostAlias {
	return []HostAlias{{IP: testGHEProxyIP, Hostnames: []string{"github.ibm.com", "api.github.ibm.com"}}}
}

// aliasDeploymentStub serves a Deployment whose pod template carries
// liveAliases (raw JSON, "" for none) and applies JSON patches to it, so a
// second Ensure sees the patched state. It records each PATCH.
type aliasDeploymentStub struct {
	mu           sync.Mutex
	live         []HostAlias
	patches      []string
	contentTypes []string
	patchStatus  int
	getStatus    int
}

func stubAliasDeployment(t *testing.T, live []HostAlias) *aliasDeploymentStub {
	t.Helper()
	stub := &aliasDeploymentStub{live: live, patchStatus: http.StatusOK, getStatus: http.StatusOK}
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(nsPath, []byte("hive-hosted-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("fake-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			stub.patches = append(stub.patches, string(b))
			stub.contentTypes = append(stub.contentTypes, r.Header.Get("Content-Type"))
			if stub.patchStatus != http.StatusOK {
				w.WriteHeader(stub.patchStatus)
				_, _ = w.Write([]byte(`forbidden`))
				return
			}
			var ops []struct {
				Value []HostAlias `json:"value"`
			}
			if err := json.Unmarshal(b, &ops); err == nil && len(ops) == 1 {
				stub.live = ops[0].Value
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if stub.getStatus != http.StatusOK {
			w.WriteHeader(stub.getStatus)
			return
		}
		var dep struct {
			Spec struct {
				Template struct {
					Spec struct {
						HostAliases []HostAlias `json:"hostAliases,omitempty"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		dep.Spec.Template.Spec.HostAliases = stub.live
		_ = json.NewEncoder(w).Encode(dep)
	}))
	t.Cleanup(srv.Close)

	oldServer, oldToken, oldCA, oldNS := k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath
	k8sAPIServer = srv.URL
	k8sTokenPath = tokenPath
	k8sCACertPath = filepath.Join(dir, "ca.crt")
	writeTestK8sCACert(t, k8sCACertPath)
	k8sNamespacePath = nsPath
	t.Cleanup(func() {
		k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath = oldServer, oldToken, oldCA, oldNS
	})
	return stub
}

func (s *aliasDeploymentStub) setStatus(get, patch int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getStatus, s.patchStatus = get, patch
}

func (s *aliasDeploymentStub) snapshot() ([]HostAlias, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HostAlias(nil), s.live...), append([]string(nil), s.patches...), append([]string(nil), s.contentTypes...)
}

// A spoke provisioned before #17805 has no hostAliases at all: the first
// check patches them in, the second is a no-op.
func TestEnsureHostAliasesSelf_PreFeatureSpokeGetsAliasesOnce(t *testing.T) {
	stub := stubAliasDeployment(t, nil)
	patched, err := EnsureHostAliasesSelf(slog.Default(), gheAliases())
	if err != nil || !patched {
		t.Fatalf("patched=%v err=%v, want a patch", patched, err)
	}
	live, patches, cts := stub.snapshot()
	if len(patches) != 1 || cts[0] != jsonPatchContentType {
		t.Fatalf("patches=%v content-types=%v, want one JSON patch", patches, cts)
	}
	if !strings.Contains(patches[0], podHostAliasesJSONPointer) {
		t.Fatalf("patch does not target the pod hostAliases: %s", patches[0])
	}
	if !HostAliasesConverged(live, gheAliases()) {
		t.Fatalf("deployment not converged after patch: %+v", live)
	}
	patched, err = EnsureHostAliasesSelf(slog.Default(), gheAliases())
	if err != nil || patched {
		t.Fatalf("second ensure patched=%v err=%v, want idempotent no-op", patched, err)
	}
	if _, patches, _ = stub.snapshot(); len(patches) != 1 {
		t.Fatalf("idempotent ensure issued another patch: %v", patches)
	}
}

// Manual drift (wrong IP for a GHE host, a missing hostname) converges, and
// an unrelated operator alias survives.
func TestEnsureHostAliasesSelf_DriftConvergesAndKeepsUnrelated(t *testing.T) {
	stub := stubAliasDeployment(t, []HostAlias{
		{IP: "10.9.9.9", Hostnames: []string{"github.ibm.com", "other.example"}},
		{IP: testGHEProxyIP, Hostnames: []string{"github.ibm.com"}},
	})
	patched, err := EnsureHostAliasesSelf(slog.Default(), gheAliases())
	if err != nil || !patched {
		t.Fatalf("patched=%v err=%v, want a patch", patched, err)
	}
	live, _, _ := stub.snapshot()
	pairs := hostAliasPairs(live)
	for _, want := range []string{testGHEProxyIP + "|github.ibm.com", testGHEProxyIP + "|api.github.ibm.com", "10.9.9.9|other.example"} {
		if !pairs[want] {
			t.Errorf("missing %s after convergence: %+v", want, live)
		}
	}
	if pairs["10.9.9.9|github.ibm.com"] {
		t.Errorf("github.ibm.com must resolve only to the proxy IP: %+v", live)
	}
}

func TestEnsureHostAliasesSelf_NoAliasesLeavesDeploymentUntouched(t *testing.T) {
	stub := stubAliasDeployment(t, []HostAlias{{IP: "10.0.0.1", Hostnames: []string{"x.example"}}})
	for name, want := range map[string][]HostAlias{
		"nil":       nil,
		"empty":     {},
		"malformed": {{IP: "not-an-ip", Hostnames: []string{"a.example"}}, {IP: "10.0.0.2", Hostnames: []string{" "}}},
	} {
		patched, err := EnsureHostAliasesSelf(slog.Default(), want)
		if err != nil || patched {
			t.Errorf("%s: patched=%v err=%v, want untouched", name, patched, err)
		}
	}
	if _, patches, _ := stub.snapshot(); len(patches) != 0 {
		t.Fatalf("no desired aliases must never patch: %v", patches)
	}
}

func TestEnsureHostAliasesSelf_Errors(t *testing.T) {
	t.Run("off-cluster", func(t *testing.T) {
		old := k8sTokenPath
		k8sTokenPath = filepath.Join(t.TempDir(), "absent")
		t.Cleanup(func() { k8sTokenPath = old })
		if patched, err := EnsureHostAliasesSelf(slog.Default(), gheAliases()); err != nil || patched {
			t.Fatalf("off-cluster: patched=%v err=%v, want silent no-op", patched, err)
		}
	})
	t.Run("rbac denied", func(t *testing.T) {
		stub := stubAliasDeployment(t, nil)
		stub.setStatus(http.StatusOK, http.StatusForbidden)
		if _, err := EnsureHostAliasesSelf(slog.Default(), gheAliases()); err == nil {
			t.Fatal("forbidden patch must surface an error")
		}
	})
	t.Run("get fails", func(t *testing.T) {
		stub := stubAliasDeployment(t, nil)
		stub.setStatus(http.StatusInternalServerError, http.StatusOK)
		if _, err := EnsureHostAliasesSelf(slog.Default(), gheAliases()); err == nil {
			t.Fatal("failed GET must surface an error")
		}
	})
	t.Run("missing namespace", func(t *testing.T) {
		stubAliasDeployment(t, nil)
		k8sNamespacePath = filepath.Join(t.TempDir(), "absent")
		if _, err := EnsureHostAliasesSelf(slog.Default(), gheAliases()); err == nil {
			t.Fatal("unreadable namespace must surface an error")
		}
	})
	t.Run("empty namespace", func(t *testing.T) {
		stubAliasDeployment(t, nil)
		p := filepath.Join(t.TempDir(), "ns")
		if err := os.WriteFile(p, []byte("  "), 0o600); err != nil {
			t.Fatal(err)
		}
		k8sNamespacePath = p
		if _, err := EnsureHostAliasesSelf(slog.Default(), gheAliases()); err == nil {
			t.Fatal("empty namespace must surface an error")
		}
	})
}

func TestMergeAndConvergedHostAliases(t *testing.T) {
	want := gheAliases()
	if !HostAliasesConverged(nil, nil) {
		t.Error("no desired aliases is always converged")
	}
	if HostAliasesConverged(nil, want) {
		t.Error("missing aliases must not be converged")
	}
	upper := []HostAlias{{IP: testGHEProxyIP, Hostnames: []string{"API.GITHUB.IBM.COM", "github.ibm.com"}}, {IP: "10.0.0.9", Hostnames: []string{"o.example"}}}
	if !HostAliasesConverged(upper, want) {
		t.Error("same pairs in another order or case plus unrelated entries must be converged")
	}
	extra := []HostAlias{{IP: testGHEProxyIP, Hostnames: []string{"github.ibm.com", "api.github.ibm.com", "stale.example"}}}
	if HostAliasesConverged(extra, want) {
		t.Error("an extra hostname on the managed IP is drift")
	}
	merged := MergeHostAliases(extra, want)
	if len(merged) != 1 || strings.Join(merged[0].Hostnames, ",") != "github.ibm.com,api.github.ibm.com" {
		t.Fatalf("merge must set exactly the desired hostnames: %+v", merged)
	}
	moved := MergeHostAliases([]HostAlias{{IP: "10.1.1.1", Hostnames: []string{"github.ibm.com"}}}, want)
	if len(moved) != 1 || moved[0].IP != testGHEProxyIP {
		t.Fatalf("an entry emptied by the merge must be dropped: %+v", moved)
	}
}

// The heartbeat dispatch runs one check per desired set per interval, never
// concurrently, re-checks at once when the set changes, and WARNs on failure.
func TestApplyHubHostAliases_ThrottleAndWarn(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	oldFn, oldNow := ensureHostAliasesSelfFn, hostAliasesNow
	hostAliasesNow = func() time.Time { return now }
	calls := make(chan []HostAlias, 4)
	fail := false
	ensureHostAliasesSelfFn = func(_ *slog.Logger, want []HostAlias) (bool, error) {
		calls <- want
		if fail {
			return false, io.ErrUnexpectedEOF
		}
		return true, nil
	}
	hostAliasesSelfState.mu.Lock()
	hostAliasesSelfState.running, hostAliasesSelfState.lastKey, hostAliasesSelfState.lastCheck = false, "", time.Time{}
	hostAliasesSelfState.mu.Unlock()
	t.Cleanup(func() { ensureHostAliasesSelfFn, hostAliasesNow = oldFn, oldNow })

	var logBuf safeBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	waitCall := func(msg string) {
		t.Helper()
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatal(msg)
		}
		waitIdle(t)
	}
	noCall := func(msg string) {
		t.Helper()
		select {
		case <-calls:
			t.Fatal(msg)
		default:
		}
	}

	applyHubHostAliases(logger, nil)
	noCall("empty desired set must not check")

	applyHubHostAliases(logger, gheAliases())
	waitCall("first beat with aliases must check")
	applyHubHostAliases(logger, gheAliases())
	noCall("unchanged set inside the interval must not re-check")

	changed := []HostAlias{{IP: "10.2.2.2", Hostnames: []string{"ghe.example"}}}
	fail = true
	applyHubHostAliases(logger, changed)
	waitCall("a changed desired set must check at once")
	if !strings.Contains(logBuf.String(), "could not apply the cluster host aliases") || !strings.Contains(logBuf.String(), "level=WARN") {
		t.Fatalf("failure must log a WARN, got %q", logBuf.String())
	}

	fail = false
	now = now.Add(hostAliasesSelfCheckInterval)
	applyHubHostAliases(logger, changed)
	waitCall("after the interval the same set must be re-checked (drift)")
}

func waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hostAliasesSelfState.mu.Lock()
		running := hostAliasesSelfState.running
		hostAliasesSelfState.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("host aliases check never finished")
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The wire field decodes from the hub's JSON key.
func TestHeartbeatResponseDecodesHostAliases(t *testing.T) {
	var resp HeartbeatResponse
	if err := json.Unmarshal([]byte(`{"ok":true,"host_aliases":[{"ip":"129.34.104.21","hostnames":["github.ibm.com"]}]}`), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.HostAliases) != 1 || resp.HostAliases[0].IP != testGHEProxyIP {
		t.Fatalf("host_aliases not decoded: %+v", resp.HostAliases)
	}
}
