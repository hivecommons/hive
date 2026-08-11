package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubImportResolver points the shared privateURLResolver at a fixed map so
// these tests never depend on live DNS, restoring it afterwards.
func stubImportResolver(t *testing.T, hosts map[string][]string) {
	t.Helper()
	orig := privateURLResolver
	t.Cleanup(func() { privateURLResolver = orig })
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		if addrs, ok := hosts[strings.ToLower(host)]; ok {
			return addrs, nil
		}
		return nil, errors.New("no such host")
	}
}

// The core vulnerability: the agent-import handler fetched whatever URL the
// caller supplied and returned the body, so pointing it at the cloud metadata
// endpoint exfiltrated instance credentials.
func TestValidateImportFetchURL_BlocksMetadataIPLiteral(t *testing.T) {
	stubImportResolver(t, map[string][]string{})
	err := validateImportFetchURL(context.Background(), "http://169.254.169.254/latest/meta-data/")
	if err == nil {
		t.Fatal("import URL pointing at cloud metadata was accepted (SSRF)")
	}
	if !strings.Contains(err.Error(), "private/internal") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An IP-literal check alone is not enough: the guard must resolve DNS so a
// hostname pointing at an internal address is rejected too.
func TestValidateImportFetchURL_BlocksHostnameResolvingInternally(t *testing.T) {
	stubImportResolver(t, map[string][]string{"metadata.attacker.example": {"169.254.169.254"}})
	if err := validateImportFetchURL(context.Background(), "https://metadata.attacker.example/x"); err == nil {
		t.Fatal("hostname resolving to metadata IP was accepted (SSRF)")
	}
}

// Cluster-internal services are the other high-value target.
func TestValidateImportFetchURL_BlocksClusterInternal(t *testing.T) {
	stubImportResolver(t, map[string][]string{"hive.hive-hub.svc.cluster.local": {"10.0.1.7"}})
	if err := validateImportFetchURL(context.Background(), "http://hive.hive-hub.svc.cluster.local/api/config"); err == nil {
		t.Fatal("cluster-internal service URL was accepted (SSRF)")
	}
}

// file:// is never a legitimate import transport and is the classic pivot for
// reading local files.
func TestValidateImportFetchURL_BlocksNonHTTPScheme(t *testing.T) {
	stubImportResolver(t, map[string][]string{})
	if err := validateImportFetchURL(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("file:// import URL was accepted")
	}
}

// DNS failure must fail closed, matching isPrivateURL's contract.
func TestValidateImportFetchURL_FailsClosedOnDNSError(t *testing.T) {
	stubImportResolver(t, map[string][]string{})
	if err := validateImportFetchURL(context.Background(), "https://unresolvable.attacker.example/x"); err == nil {
		t.Fatal("unresolvable host was accepted; guard must fail closed")
	}
}

// A genuinely public import URL must still be accepted, so the guard does not
// break legitimate agent-definition imports.
func TestValidateImportFetchURL_AllowsPublicURL(t *testing.T) {
	stubImportResolver(t, map[string][]string{"raw.githubusercontent.com": {"185.199.108.133"}})
	if err := validateImportFetchURL(context.Background(), "https://raw.githubusercontent.com/o/r/main/agent.yaml"); err != nil {
		t.Fatalf("public import URL rejected: %v", err)
	}
}

// The preflight only sees the URL the caller supplied. This pins the second
// half of the defence: a public URL that 302s to an internal address must not
// be followed.
func TestNoRedirectToPrivate_BlocksRedirectToInternalHostname(t *testing.T) {
	stubImportResolver(t, map[string][]string{"metadata.attacker.example": {"169.254.169.254"}})
	req := httptest.NewRequest(http.MethodGet, "http://metadata.attacker.example/latest/meta-data/", nil)
	if err := noRedirectToPrivate(req, nil); err == nil {
		t.Fatal("redirect to hostname resolving to metadata IP was allowed (SSRF)")
	}
}

// End-to-end: a real server 302s to an internal target and the client must
// refuse to follow it.
func TestImportClient_RefusesRedirectToInternal(t *testing.T) {
	stubImportResolver(t, map[string][]string{
		"metadata.attacker.example": {"169.254.169.254"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.attacker.example/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: noRedirectToPrivate}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("client followed a redirect to an internal host")
	}
}

// The chain-depth cap still terminates a redirect loop between public hosts.
func TestNoRedirectToPrivate_CapsChainDepth(t *testing.T) {
	stubImportResolver(t, map[string][]string{"docs.example.com": {"93.184.216.34"}})
	req := httptest.NewRequest(http.MethodGet, "https://docs.example.com/a", nil)
	via := []*http.Request{req, req, req}
	if err := noRedirectToPrivate(req, via); err == nil || !strings.Contains(err.Error(), "stopped after") {
		t.Fatalf("chain depth not capped: %v", err)
	}
}
