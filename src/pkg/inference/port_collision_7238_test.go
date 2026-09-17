package inference

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/proxy"
)

// TestLocalLiteLLMPortDoesNotCollideWithTranslator pins an invariant that,
// until #7238 stage 3, existed only as a sentence in a comment.
//
// The bundled LiteLLM proxy and the Go inference translator both listen on
// loopback, and the whole design depends on them being different processes on
// different ports: agents talk to the translator (18444), which forwards to
// LiteLLM (18445). That indirection is what preserves per-agent attribution,
// mode enforcement and the MITM proxy path.
//
// If the two constants ever became equal, one of the two listeners would fail
// to bind and the survivor would be forwarding to itself. Nothing in the build
// would object, because the constants live in packages that do not import each
// other — and the extraction made that worse, not better: the port moved from
// cmd/hive, which imports both, into pkg/inference, which does not import
// pkg/proxy at all. The comment asserting the ports are "distinct" now sits in
// a package with no way to know.
//
// This test is the only thing tying them together, so it is deliberately a
// test rather than a doc line.
func TestLocalLiteLLMPortDoesNotCollideWithTranslator(t *testing.T) {
	if LocalLiteLLMProxyPort == proxy.InferenceTranslatePort {
		t.Fatalf("local LiteLLM proxy and the inference translator both claim port %d — "+
			"agents reach LiteLLM THROUGH the translator, so sharing a port means one "+
			"listener fails to bind and the other forwards to itself",
			LocalLiteLLMProxyPort)
	}
}

// TestLocalLiteLLMProxyURLUsesThePortConstant pins that the URL the translator
// forwards to is derived from the port constant rather than restating it.
//
// A hard-coded URL would keep passing the collision test above while silently
// sending traffic somewhere else the moment the constant is changed — the
// constant would look authoritative and be ignored. That is the failure mode
// worth preventing: a port change that appears to take effect and does not.
func TestLocalLiteLLMProxyURLUsesThePortConstant(t *testing.T) {
	got := LocalLiteLLMProxyURL()
	want := fmt.Sprintf("http://127.0.0.1:%d", LocalLiteLLMProxyPort)
	if got != want {
		t.Fatalf("LocalLiteLLMProxyURL() = %q, want %q — it must be built from "+
			"LocalLiteLLMProxyPort, not a literal", got, want)
	}
	// Loopback is not incidental: the bundled proxy holds upstream provider
	// keys and must never be reachable off-host.
	if !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Errorf("LocalLiteLLMProxyURL() = %q must stay bound to loopback — the bundled "+
			"proxy holds upstream provider keys", got)
	}
	if !strings.HasSuffix(got, ":"+strconv.Itoa(LocalLiteLLMProxyPort)) {
		t.Errorf("LocalLiteLLMProxyURL() = %q does not carry the configured port", got)
	}
}
