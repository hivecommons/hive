package hubbackup

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain isolates the hubbackup test suite from real clusters and the real
// internet.
//
// This package shells out to kubectl (KubectlSecretCollector,
// KubectlSpokeCollector) and talks to OCI Object Storage. Backup code only
// READS, but a stray real kubectl still authenticates with the hub pod's
// ServiceAccount when the suite runs inside a hive pod — the same exposure that
// produced ~196 stray namespaces on live clusters and drove TestMain in
// pkg/hub (#2287).
//
// Two guards, matching that precedent:
//   - Kubernetes service-account env vars are cleared and KUBECONFIG is pointed
//     at /dev/null, so a kubectl that escapes the execCommandContext seam has no
//     cluster to reach.
//   - The default HTTP transport refuses any non-loopback dial, so an
//     ObjectStore built without endpointOverride fails loudly instead of
//     reaching OCI.
func TestMain(m *testing.M) {
	os.Unsetenv("KUBERNETES_SERVICE_HOST")
	os.Unsetenv("KUBERNETES_SERVICE_PORT")
	os.Setenv("KUBECONFIG", os.DevNull)
	for _, v := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		os.Unsetenv(v)
	}

	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("hubbackup TestMain: http.DefaultTransport is not *http.Transport")
	}
	guarded := tr.Clone()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	guarded.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf(
				"hubbackup tests must not reach the network: blocked dial to %q; "+
					"set ObjectStore.endpointOverride to an httptest server", addr)
		}
		return dialer.DialContext(ctx, network, addr)
	}
	http.DefaultTransport = guarded
	http.DefaultClient.Transport = guarded

	os.Exit(m.Run())
}

// fakeKubectl installs a stub for execCommandContext that routes every kubectl
// invocation back into this test binary (the standard Go helper-process
// pattern). script maps a substring of the joined argv to the stdout the fake
// should emit; an entry whose value begins with failMarker makes the fake exit
// non-zero with the remainder as stderr.
//
// Matching on a substring rather than the exact argv keeps tests readable while
// still asserting the real command shape — the cluster-selection flags built by
// kubectlArgs are part of the string being matched.
func fakeKubectl(t *testing.T, script map[string]string) {
	t.Helper()
	orig := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		joined := name + " " + strings.Join(args, " ")
		out, fail := "", ""
		for pattern, result := range script {
			if strings.Contains(joined, pattern) {
				if strings.HasPrefix(result, failMarker) {
					fail = strings.TrimPrefix(result, failMarker)
				} else {
					out = result
				}
				break
			}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperKubectl", "--")
		cmd.Env = append(os.Environ(),
			"GO_HELPER_KUBECTL=1",
			"GO_HELPER_STDOUT="+out,
			"GO_HELPER_FAIL="+fail,
		)
		return cmd
	}
	t.Cleanup(func() { execCommandContext = orig })
}

// failMarker prefixes a fakeKubectl script value to mean "exit non-zero".
const failMarker = "\x00FAIL\x00"

// TestHelperKubectl is not a real test: it is the child process spawned by
// fakeKubectl. It prints the scripted stdout and exits with the scripted code.
func TestHelperKubectl(t *testing.T) {
	if os.Getenv("GO_HELPER_KUBECTL") != "1" {
		t.Skip("helper process; not run directly")
	}
	if msg := os.Getenv("GO_HELPER_FAIL"); msg != "" {
		fmt.Fprint(os.Stderr, msg)
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, os.Getenv("GO_HELPER_STDOUT"))
	os.Exit(0)
}
