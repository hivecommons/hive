package webstatic

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func dashboardScriptFixture() []byte {
	return []byte("<html><head></head><body><script>window.boot = true;</script><main>Loading</main>\n  " +
		dashboardMainScriptMarker + "\n    const lexical = '🐝';\n    function action() { return lexical; }\n  </script>\n</body></html>")
}

func requireDashboardScript(t *testing.T, raw []byte) ([]byte, *DashboardScript) {
	t.Helper()
	shell, asset := ExternalizeDashboardScript(raw)
	if asset == nil {
		t.Fatal("main script was not externalized")
	}
	return shell, asset
}

func TestExternalizeDashboardScriptPreservesBytesAndOrder(t *testing.T) {
	raw := InjectBranding(dashboardScriptFixture())
	shell, asset := requireDashboardScript(t, raw)
	originalScripts := ExtractInlineScripts(raw)
	retainedScripts := ExtractInlineScripts(shell)
	if len(originalScripts) != 2 || len(retainedScripts) != 2 {
		t.Fatalf("script counts before=%d after=%d, want original boot and recovery bootstrap", len(originalScripts), len(retainedScripts))
	}
	if !bytes.Equal(retainedScripts[0], originalScripts[0]) {
		t.Fatal("inline boot script changed")
	}
	if !bytes.Equal(asset.document.raw, originalScripts[1]) {
		t.Fatal("main script text, whitespace, lexical bindings or Unicode changed")
	}
	sum := sha256.Sum256(originalScripts[1])
	if want := "/assets/dashboard-main." + hex.EncodeToString(sum[:]) + ".js"; asset.Path() != want {
		t.Fatalf("asset URL = %q, want full content address %q", asset.Path(), want)
	}
	tag := []byte(`<script id="hive-dashboard-main-script" defer src="` + asset.Path() +
		`" integrity="sha256-` + base64.StdEncoding.EncodeToString(sum[:]) + `"></script>`)
	replacement := append([]byte(dashboardScriptBootstrap), tag...)
	recovered := bytes.Replace(shell, replacement, append(append([]byte("<script>"), asset.document.raw...), []byte("</script>")...), 1)
	if !bytes.Equal(recovered, raw) {
		t.Fatal("replacing external tag with recovered script did not reproduce exact original HTML")
	}
	if !bytes.Contains(shell, []byte(BrandingLinkTag)) || bytes.Index(shell, retainedScripts[0]) > bytes.Index(shell, tag) {
		t.Fatal("branding or inline boot order was lost")
	}
	_, same := requireDashboardScript(t, raw)
	if same.Path() != asset.Path() {
		t.Fatal("identical script bytes produced a different content address")
	}
	changed := bytes.Replace(raw, []byte("'🐝'"), []byte("'branded bee'"), 1)
	_, branded := requireDashboardScript(t, changed)
	if branded.Path() == asset.Path() || !bytes.Contains(branded.document.raw, []byte("'branded bee'")) {
		t.Fatal("final branded script bytes were not used for content addressing")
	}
	if bytes.Contains(shell, []byte("type=\"module\"")) || bytes.Contains(shell, []byte("async")) {
		t.Fatal("main script must remain an ordered deferred classic script")
	}
	policy := ScriptSrcElemSources(shell)
	if !strings.HasPrefix(policy, "'self' ") || !strings.Contains(policy, CSPScriptHash(retainedScripts[0])) {
		t.Fatal("shell CSP must authorize same-origin asset and exact inline boot")
	}
	if !strings.Contains(policy, CSPScriptHash(retainedScripts[1])) ||
		bytes.Index(shell, retainedScripts[1]) > bytes.Index(shell, tag) {
		t.Fatal("trusted recovery bootstrap must be CSP-authorized before the main script")
	}
	if strings.Contains(policy, CSPScriptHash(originalScripts[1])) || strings.Contains(policy, "unsafe-inline") {
		t.Fatal("shell CSP should not need main-script hash or inline weakening")
	}
}

func TestExternalizeDashboardScriptUnexpectedMarkupIsUnchanged(t *testing.T) {
	for name, raw := range map[string][]byte{
		"empty":        nil,
		"missing":      []byte("<body><script>other()</script></body>"),
		"unclosed":     []byte("<body>" + dashboardMainScriptMarker + "\nmain()"),
		"ambiguous":    append(dashboardScriptFixture(), dashboardScriptFixture()...),
		"later inline": append(dashboardScriptFixture(), []byte("<script>action()</script>")...),
		"later external": append(dashboardScriptFixture(),
			[]byte(`<SCRIPT src="/after.js"></SCRIPT>`)...),
		"changed tag": bytes.Replace(dashboardScriptFixture(), []byte(dashboardMainScriptMarker),
			[]byte(strings.Replace(dashboardMainScriptMarker, "<script>", `<script type="module">`, 1)), 1),
	} {
		t.Run(name, func(t *testing.T) {
			got, asset := ExternalizeDashboardScript(raw)
			if asset != nil || !bytes.Equal(got, raw) {
				t.Fatal("unexpected markup must retain the original inline document with no asset")
			}
		})
	}
}

func TestDashboardScriptRealIndexShellBudget(t *testing.T) {
	raw, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	shell, asset := requireDashboardScript(t, InjectBranding(raw))
	const maxShellBytes = 1024 * 1024
	const maxShellPercent = 25
	if len(shell) > maxShellBytes || len(shell)*100 > len(raw)*maxShellPercent {
		t.Fatalf("initial HTML = %d bytes from %d-byte canonical index; want <= %d bytes and <= %d%%",
			len(shell), len(raw), maxShellBytes, maxShellPercent)
	}
	scripts := ExtractInlineScripts(InjectBranding(raw))
	if len(scripts) != 2 || !bytes.Equal(asset.document.raw, scripts[1]) {
		t.Fatal("real main script bytes or script boundary changed")
	}
	retained := ExtractInlineScripts(shell)
	if len(retained) != 2 || !bytes.Equal(retained[0], scripts[0]) {
		t.Fatal("real inline boot script changed")
	}
	for _, global := range []string{
		"window.HIVE_MATRIX", "window.HIVE_KIOSK", "window.HIVE_POPOUT",
		"window.HIVE_DISPLAY", "window.hiveURLWithHash", "window.hiveKioskCTAOn",
	} {
		if !bytes.Contains(retained[0], []byte(global)) ||
			bytes.Index(shell, []byte(global)) > bytes.Index(shell, []byte(asset.scriptTag())) {
			t.Fatalf("ordinary early boot global %q must remain inline before application script", global)
		}
	}
	// Revalidate the actual small shell, not the canonical monolith.
	index := NewIndexDocument(shell)
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; script-src-elem 'self'; script-src-attr 'none'")
	ApplyDocumentScriptSrcElem(rec, shell)
	index.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !bytes.Equal(rec.Body.Bytes(), shell) || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("served shell must retain index revalidation semantics")
	}
	policy := rec.Header().Get("Content-Security-Policy")
	for _, script := range retained {
		if !strings.Contains(policy, CSPScriptHash(script)) {
			t.Fatal("served shell CSP must authorize boot and failure listener exactly")
		}
	}
	if !strings.Contains(policy, "script-src-elem 'self' ") ||
		!strings.Contains(policy, "script-src-attr 'none'") || strings.Contains(policy, "unsafe-inline") {
		t.Fatal("external script source or event-handler CSP boundary changed")
	}
	req := httptest.NewRequest(http.MethodHead, "/index.html", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec = httptest.NewRecorder()
	index.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatal("shell conditional HEAD must return an empty 304")
	}
}

func TestDashboardScriptHTTPRepresentations(t *testing.T) {
	_, asset := requireDashboardScript(t, dashboardScriptFixture())
	var identityETag string
	for _, encoding := range []string{"", "gzip", "gzip;q=0"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, asset.Path(), nil)
			req.Header.Set("Accept-Encoding", encoding)
			rec := httptest.NewRecorder()
			rec.Header().Set("Vary", "Cookie")
			asset.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			for key, want := range map[string]string{
				"Content-Type":           "text/javascript; charset=utf-8",
				"X-Content-Type-Options": "nosniff",
				"Cache-Control":          dashboardScriptCache,
				"Content-Length":         strconv.Itoa(rec.Body.Len()),
				"ETag":                   ETagFor(rec.Body.Bytes()),
			} {
				if got := rec.Header().Get(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if got := strings.Join(rec.Header().Values("Vary"), ", "); got != "Cookie, Accept-Encoding" {
				t.Fatalf("Vary = %q, want existing Cookie plus Accept-Encoding", got)
			}
			body := rec.Body.Bytes()
			if encoding == "gzip" {
				if rec.Header().Get("Content-Encoding") != "gzip" {
					t.Fatal("gzip response not labeled")
				}
				zr, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				body, err = io.ReadAll(zr)
				_ = zr.Close()
				if err != nil {
					t.Fatal(err)
				}
				if rec.Header().Get("ETag") == identityETag {
					t.Fatal("gzip and identity must have distinct strong validators")
				}
			} else {
				identityETag = rec.Header().Get("ETag")
				if rec.Header().Get("Content-Encoding") != "" {
					t.Fatal("identity representation labeled as compressed")
				}
			}
			if !bytes.Equal(body, asset.document.raw) {
				t.Fatal("served main script bytes changed")
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				head := httptest.NewRequest(method, asset.Path(), nil)
				head.Header.Set("Accept-Encoding", encoding)
				if method == http.MethodHead {
					out := httptest.NewRecorder()
					asset.ServeHTTP(out, head)
					if out.Code != http.StatusOK || out.Body.Len() != 0 ||
						out.Header().Get("Content-Length") != rec.Header().Get("Content-Length") {
						t.Fatal("HEAD must advertise GET length with no body")
					}
				}
				for _, validator := range []string{
					rec.Header().Get("ETag"), "W/" + rec.Header().Get("ETag"),
					`"stale", ` + rec.Header().Get("ETag"), "*",
				} {
					head.Header.Set("If-None-Match", validator)
					out := httptest.NewRecorder()
					asset.ServeHTTP(out, head)
					if out.Code != http.StatusNotModified || out.Body.Len() != 0 ||
						out.Header().Get("Cache-Control") != dashboardScriptCache ||
						out.Header().Get("ETag") != rec.Header().Get("ETag") ||
						out.Header().Get("Vary") != "Accept-Encoding" ||
						out.Header().Get("Content-Length") != "" {
						t.Fatalf("%s conditional response lost cache metadata or carried body: %+v", method, out)
					}
				}
			}
			miss := httptest.NewRequest(http.MethodGet, asset.Path(), nil)
			miss.Header.Set("Accept-Encoding", encoding)
			miss.Header.Set("If-None-Match", `"stale"`)
			out := httptest.NewRecorder()
			asset.ServeHTTP(out, miss)
			if out.Code != http.StatusOK || out.Body.Len() == 0 {
				t.Fatal("stale validator must receive the selected full representation")
			}
		})
	}
}

func TestDashboardScriptOnlyServesItsExactPath(t *testing.T) {
	_, asset := requireDashboardScript(t, dashboardScriptFixture())
	for _, path := range []string{"/", "/index.html", "/assets/", asset.Path() + "/extra", "/assets/../server.go"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("If-None-Match", "*")
		rec := httptest.NewRecorder()
		asset.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || rec.Header().Get("ETag") != "" ||
			strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("path %q served or cached an asset: %+v", path, rec)
		}
	}

	req := httptest.NewRequest(http.MethodPost, asset.Path(), nil)
	req.Header.Set("If-None-Match", "*")
	rec := httptest.NewRecorder()
	asset.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("asset must reject writes before evaluating validators")
	}
}

func TestDashboardScriptFailureBootstrap(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the bootstrap behavior regression")
	}
	scripts := ExtractInlineScripts([]byte(dashboardScriptBootstrap))
	if len(scripts) != 1 {
		t.Fatal("expected exactly one trusted recovery bootstrap")
	}
	const harness = `
	const assert = require('node:assert/strict');
	const vm = require('node:vm');
	const source = require('node:fs').readFileSync(0, 'utf8');
	function realm(nativeDialog = true) {
	  const handlers = {};
	  const appended = [];
	  let reloads = 0;
	  function element(tag) {
	    const el = {tag, attrs: {}, events: {}, children: [],
	      setAttribute(name, value) { this.attrs[name] = value; },
	      appendChild(child) { this.children.push(child); },
	      addEventListener(type, fn) { this.events[type] = fn; },
	      focus() { this.focused = true; }
	    };
	    if (nativeDialog) el.showModal = function() { this.modal = true; };
	    return el;
	  }
	  const root = element('main');
	  const main = {id: 'hive-dashboard-main-script', src: 'https://hive.example/assets/dashboard-main.hash.js'};
	  const window = {
	    location: {reload() { reloads++; }},
	    addEventListener(type, fn, capture) { assert.equal(capture, true); handlers[type] = fn; },
	    removeEventListener(type, fn, capture) {
	      assert.equal(capture, true);
	      assert.equal(handlers[type], fn);
	      delete handlers[type];
	    }
	  };
	  const document = {
	    getElementById(id) { return id === main.id ? main : id === 'hive-dashboard-root' ? root : null; },
	    createElement: element,
	    body: {appendChild(el) { appended.push(el); }}
	  };
	  vm.runInNewContext(source, {window, document});
	  return {handlers, appended, root, main, reloads: () => reloads};
	}
	for (const mode of ['resource', 'runtime', 'no-native-dialog']) {
	  const r = realm(mode !== 'no-native-dialog');
	  r.handlers.error({target: {id: 'unrelated-image'}});
	  r.handlers.error({target: {}, filename: 'https://hive.example/unrelated.js'});
	  assert.equal(r.appended.length, 0, 'unrelated failures must not hide a working dashboard');
	  r.handlers.error(mode === 'runtime' ? {target: {}, filename: r.main.src} : {target: r.main});
	  assert.equal(r.root.hidden, true, 'inert shell must no longer appear usable');
	  assert.ok('inert' in r.root.attrs);
	  assert.equal(r.appended.length, 1);
	  assert.deepEqual(Object.keys(r.handlers), [], 'failure listeners must be removed after handling');
	  const dialog = r.appended[0];
	  assert.equal(dialog.tag, 'dialog');
	  assert.equal(dialog.attrs.role, 'alertdialog');
	  assert.equal(dialog.id, 'hive-dashboard-load-error');
	  assert.equal(dialog.attrs['aria-labelledby'], dialog.children[0].id);
	  assert.equal(dialog.attrs['aria-describedby'], dialog.children[1].id);
	  assert.match(dialog.children[0].textContent, /could not finish loading/);
	  assert.match(dialog.children[1].textContent, /update or connection problem/);
	  assert.match(dialog.children[1].textContent, /contact your hive operator/);
	  const button = dialog.children[2];
	  assert.equal(button.textContent, 'Reload dashboard');
	  assert.equal(button.focused, true);
	  if (mode === 'no-native-dialog') assert.ok('open' in dialog.attrs);
	  else assert.equal(dialog.modal, true);
	  let prevented = false;
	  dialog.events.cancel({preventDefault() { prevented = true; }});
	  assert.equal(prevented, true, 'Escape must not reveal a broken dashboard');
	  assert.equal(r.reloads(), 0, 'no automatic reload loop');
	  button.events.click();
	  assert.equal(r.reloads(), 1);
	}
	const good = realm();
	good.handlers.load({target: {id: 'unrelated-image'}});
	assert.equal(typeof good.handlers.error, 'function');
	good.handlers.load({target: good.main});
	assert.deepEqual(Object.keys(good.handlers), []);
	assert.equal(good.root.hidden, undefined);
	assert.equal(good.appended.length, 0, 'successful load must not display an error');
	`
	cmd := exec.Command(node, "-e", harness)
	cmd.Stdin = bytes.NewReader(scripts[0])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap behavior regression failed: %v\n%s", err, out)
	}
}
