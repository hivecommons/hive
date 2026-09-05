package dashboard

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brandingGuardServer returns a Server whose warnings land in buf, with the
// log-once map cleared so each test observes its own refusal. The map is
// package-global by design (it suppresses per-request log spam in production),
// which makes clearing it a test responsibility.
func brandingGuardServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	brandingWarnOnce.Range(func(k, _ any) bool {
		brandingWarnOnce.Delete(k)
		return true
	})
	buf := &bytes.Buffer{}
	return &Server{logger: slog.New(slog.NewTextHandler(buf, nil))}, buf
}

func getBrandingCSS(s *Server) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleBrandingCSS(w, httptest.NewRequest(http.MethodGet, "/branding/custom.css", nil))
	return w
}

// A world-writable stylesheet is the core threat in #5849: anything sharing the
// data volume (agents) can replace it, and the dashboard CSP permits
// `img-src https:`, so the injected CSS can beacon out.
func TestBrandingCSSRefusesWorldWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.css")
	t.Setenv("HIVE_BRANDING_CSS", path)
	if err := os.WriteFile(path, []byte(":root{--accent:#f00}"), 0o666); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to umask; force the bits the test is about.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	s, logs := brandingGuardServer(t)
	w := getBrandingCSS(s)

	if w.Code != http.StatusNotFound {
		t.Errorf("world-writable custom.css: got %d, want 404 (refused)", w.Code)
	}
	if strings.Contains(w.Body.String(), "--accent") {
		t.Error("world-writable custom.css was SERVED; the mode guard is not enforcing")
	}
	// The refusal must be actionable, not silent: path + offending mode + fix.
	for _, want := range []string{"group- or world-writable", "chmod go-w", path, "0666"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("refusal log missing %q; got:\n%s", want, logs.String())
		}
	}
}

// Group-writable is refused for the same reason as world-writable: somebody
// other than the owner can replace the served bytes.
func TestBrandingCSSRefusesGroupWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.css")
	t.Setenv("HIVE_BRANDING_CSS", path)
	if err := os.WriteFile(path, []byte(":root{--accent:#f00}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}

	s, _ := brandingGuardServer(t)
	if w := getBrandingCSS(s); w.Code != http.StatusNotFound {
		t.Errorf("group-writable custom.css: got %d, want 404 (refused)", w.Code)
	}
}

// Oversize must be REFUSED, not truncated — half a stylesheet is a
// differently-broken page, and the operator would have no idea why.
func TestBrandingCSSRefusesOversizeRatherThanTruncating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.css")
	t.Setenv("HIVE_BRANDING_CSS", path)
	big := bytes.Repeat([]byte("a"), brandingMaxBytes+1)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}

	s, logs := brandingGuardServer(t)
	w := getBrandingCSS(s)

	if w.Code != http.StatusNotFound {
		t.Errorf("oversize custom.css: got %d, want 404 (refused)", w.Code)
	}
	// The 404 body is net/http's own text; what must NOT appear is any of the
	// stylesheet, which is what "truncated instead of refused" would look like.
	if strings.Contains(w.Body.String(), "aaaa") {
		t.Errorf("oversize custom.css was served/truncated rather than refused: %d bytes", w.Body.Len())
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "text/css") {
		t.Errorf("oversize custom.css answered as CSS (%q); it must be refused", ct)
	}
	if !strings.Contains(logs.String(), "refusing to serve it rather than truncating") {
		t.Errorf("oversize refusal not logged actionably; got:\n%s", logs.String())
	}
}

// Exactly at the limit is legal: the cap is inclusive, and an off-by-one here
// would refuse a file the docs describe as acceptable.
func TestBrandingCSSServesExactlyAtLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.css")
	t.Setenv("HIVE_BRANDING_CSS", path)
	body := bytes.Repeat([]byte("a"), brandingMaxBytes)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	s, _ := brandingGuardServer(t)
	w := getBrandingCSS(s)
	if w.Code != http.StatusOK || w.Body.Len() != brandingMaxBytes {
		t.Errorf("at-limit custom.css: got %d with %d bytes, want 200 with %d",
			w.Code, w.Body.Len(), brandingMaxBytes)
	}
}

// THE DOCS EXAMPLE. src/docs/branding.md recommends a read-only Kubernetes
// Secret mount: root-owned, defaultMode 0444. That is the *hardened* shape the
// guard exists to encourage, so the guard must not break it. /etc/hosts has
// exactly those properties (uid 0, 0644, not group/world-writable) and lets
// this assert the ACCEPT direction without the test needing to be root.
func TestBrandingCSSAcceptsRootOwnedReadOnlyMount(t *testing.T) {
	const rootOwnedReadOnly = "/etc/hosts"
	fi, err := os.Stat(rootOwnedReadOnly)
	if err != nil {
		t.Skipf("no root-owned fixture available: %v", err)
	}
	if uid := brandingFileOwnerUID(fi); uid != 0 {
		t.Skipf("%s is owned by uid %d, not root; fixture assumption does not hold",
			rootOwnedReadOnly, uid)
	}
	if os.Getuid() == 0 {
		t.Skip("running as root makes this case indistinguishable from owner-match")
	}
	if perm := fi.Mode().Perm(); perm&brandingUnsafeModeBits != 0 {
		t.Skipf("%s is mode %04o; fixture assumption does not hold", rootOwnedReadOnly, perm)
	}

	t.Setenv("HIVE_BRANDING_CSS", rootOwnedReadOnly)
	s, logs := brandingGuardServer(t)
	w := getBrandingCSS(s)

	if w.Code != http.StatusOK {
		t.Errorf("root-owned read-only mount (the documented Secret example) was REFUSED: got %d, want 200.\nlogs:\n%s",
			w.Code, logs.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("root-owned read-only mount Content-Type = %q, want text/css", ct)
	}
}

// The ordinary case must keep working unchanged: owner-written, 0644, small.
func TestBrandingCSSServesNormalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.css")
	t.Setenv("HIVE_BRANDING_CSS", path)
	css := ":root{--accent:#0aa}"
	if err := os.WriteFile(path, []byte(css), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	s, logs := brandingGuardServer(t)
	w := getBrandingCSS(s)
	if w.Code != http.StatusOK || w.Body.String() != css {
		t.Errorf("normal custom.css: got %d %q, want 200 %q.\nlogs:\n%s",
			w.Code, w.Body.String(), css, logs.String())
	}
}

// A symlink pointing out of the branding directory must not turn the branding
// path into an arbitrary-file reader.
func TestBrandingCSSRefusesSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("NOT-CSS-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "custom.css")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HIVE_BRANDING_CSS", path)

	s, logs := brandingGuardServer(t)
	w := getBrandingCSS(s)

	if w.Code != http.StatusNotFound {
		t.Errorf("escaping symlink: got %d, want 404 (refused)", w.Code)
	}
	if strings.Contains(w.Body.String(), "NOT-CSS-SECRET") {
		t.Error("escaping symlink was FOLLOWED and its target served")
	}
	if !strings.Contains(logs.String(), "outside its own directory") {
		t.Errorf("symlink escape not logged actionably; got:\n%s", logs.String())
	}
}

// A symlink that stays inside the branding directory is a legitimate layout
// (Kubernetes projected volumes are built entirely out of these: the mount is
// a tree of symlinks into ..data/). Refusing them would break ConfigMap and
// Secret mounts, which is the opposite of the goal.
func TestBrandingCSSAllowsSymlinkWithinBrandingDir(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "actual.css")
	css := ":root{--accent:#123}"
	if err := os.WriteFile(real, []byte(css), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "custom.css")
	if err := os.Symlink(real, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HIVE_BRANDING_CSS", path)

	s, logs := brandingGuardServer(t)
	w := getBrandingCSS(s)
	if w.Code != http.StatusOK || w.Body.String() != css {
		t.Errorf("in-directory symlink: got %d %q, want 200 %q.\nlogs:\n%s",
			w.Code, w.Body.String(), css, logs.String())
	}
}

// The refusal must not tell the browser why — that would leak the deployment's
// file layout to anyone who can reach the endpoint. The reason belongs in the
// operator's log.
func TestBrandingCSSRefusalDoesNotLeakReasonToClient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.css")
	t.Setenv("HIVE_BRANDING_CSS", path)
	if err := os.WriteFile(path, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	s, _ := brandingGuardServer(t)
	body := getBrandingCSS(s).Body.String()
	for _, leak := range []string{path, "writable", "chmod"} {
		if strings.Contains(body, leak) {
			t.Errorf("response body leaks %q to the client: %s", leak, body)
		}
	}
}

// Repeated requests for a bad file must log once, not once per page load.
func TestBrandingCSSLogsRefusalOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.css")
	t.Setenv("HIVE_BRANDING_CSS", path)
	if err := os.WriteFile(path, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	s, logs := brandingGuardServer(t)
	for i := 0; i < 5; i++ {
		getBrandingCSS(s)
	}
	if n := strings.Count(logs.String(), "chmod go-w"); n != 1 {
		t.Errorf("refusal logged %d times across 5 requests; want exactly 1", n)
	}
}

// branding.json shares the directory, the volume and the writer, and its
// strings are substituted into the served document — so it gets the same
// guards. A world-writable strings file must not reach applyBranding.
func TestLoadBrandingRefusesWorldWritableJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_BRANDING_CSS", filepath.Join(dir, "custom.css"))
	jsonPath := filepath.Join(dir, "branding.json")
	if err := os.WriteFile(jsonPath, []byte(`{"product_name":"PWNED"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(jsonPath, 0o666); err != nil {
		t.Fatal(err)
	}

	s, _ := brandingGuardServer(t)
	if got := s.loadBranding(); got.ProductName != "" {
		t.Errorf("world-writable branding.json was APPLIED: ProductName = %q, want empty", got.ProductName)
	}
}

// The normal strings file still loads — the guard must not break branding.
func TestLoadBrandingServesNormalJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_BRANDING_CSS", filepath.Join(dir, "custom.css"))
	jsonPath := filepath.Join(dir, "branding.json")
	if err := os.WriteFile(jsonPath, []byte(`{"product_name":"REEF"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, _ := brandingGuardServer(t)
	if got := s.loadBranding(); got.ProductName != "REEF" {
		t.Errorf("ProductName = %q, want REEF", got.ProductName)
	}
}

// Oversize strings file is refused rather than truncated — truncated JSON is
// invalid JSON, which would surface as a confusing parse warning instead of
// the real reason.
func TestLoadBrandingRefusesOversizeJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_BRANDING_CSS", filepath.Join(dir, "custom.css"))
	jsonPath := filepath.Join(dir, "branding.json")
	padding := strings.Repeat("x", brandingMaxBytes)
	if err := os.WriteFile(jsonPath, []byte(`{"product_name":"`+padding+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, logs := brandingGuardServer(t)
	if got := s.loadBranding(); got.ProductName != "" {
		t.Error("oversize branding.json was applied")
	}
	if !strings.Contains(logs.String(), "exceeds the") {
		t.Errorf("oversize JSON refusal not logged; got:\n%s", logs.String())
	}
}

// A missing file stays the quiet, cheap, normal case — the index links to the
// CSS path unconditionally, so absence must not log anything at all.
func TestBrandingAbsentFileIsSilent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_BRANDING_CSS", filepath.Join(dir, "custom.css"))

	s, logs := brandingGuardServer(t)
	if w := getBrandingCSS(s); w.Code != http.StatusNotFound {
		t.Errorf("absent custom.css: got %d, want 404", w.Code)
	}
	if logs.Len() != 0 {
		t.Errorf("absent override logged something; it must be silent:\n%s", logs.String())
	}
}
