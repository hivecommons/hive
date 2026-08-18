package hub

import (
	"regexp"
	"strings"
	"testing"
)

// This file exists because of a live regression, and the shape of that
// regression is the reason the test below is static analysis rather than a
// behavioural HTTP test.
//
// #2369 made full_name required on POST /api/saas/request-provision and updated
// the get-started wizard, which was believed to be the only in-tree caller. It
// was not. The hub dashboard had its own "Request a Hive" modal posting to the
// same endpoint, added AFTER the wizard and reachable by every logged-in user.
// It was never updated, so it began returning
// 400 "your name is required" with no field on screen that could satisfy it.
//
// Nothing caught it. The modal was inline JavaScript inside the dashboardHTML
// Go raw string, and no test named a single one of its symbols — so neither the
// compiler, `go vet`, nor any handler test could see it. The modal has since
// been removed (the wizard is the single supported request path), but the gap
// that hid it is generic: the next inline-JS caller of a tightened endpoint
// would be just as invisible.
//
// A behavioural test cannot close that gap. There is no Go call graph reaching
// this code — the callers are string literals compiled into a Go constant and a
// file in an embedded FS. Executing them would mean a headless browser and a
// logged-in session, which is far outside a unit test and would not have run in
// CI on the PR that broke it. Grepping the JS is what actually observes the
// thing that broke: whether every in-tree fetch() to this endpoint sends the
// fields the handler now demands.

// requestProvisionPath is the endpoint whose in-tree callers this file audits.
const requestProvisionPath = "/api/saas/request-provision"

// requestProvisionRequiredJSONKeys are the JSON keys handleRequestProvision
// rejects a request for omitting. Keep in step with the validation in
// handleRequestProvision: adding a required field there without adding it here
// means the next caller can silently regress exactly as the dashboard modal
// did.
//
// full_name is checked with an explicit `body.FullName == ""` guard; org,
// github_host and repos are each validated above it. acmm_level is NOT listed:
// it is clamped rather than rejected, so omitting it is legal.
var requestProvisionRequiredJSONKeys = []string{"org", "github_host", "repos", "full_name"}

// jsBodyWindow is how many bytes after a fetch() call site we scan for the
// request body. The body is built inline in the same call expression in every
// caller here; this window comfortably spans the longest of them while stopping
// well short of the following function, so an unrelated later fetch() cannot
// lend its keys to this one.
const jsBodyWindow = 900

// inTreeJSSource is a JavaScript-bearing source file audited by this test.
type inTreeJSSource struct {
	name string
	body string
}

// collectInTreeJSSources returns every in-tree source that can originate a
// browser request to the hub API: the dashboard's embedded JS and the static
// files under static/ (which includes the get-started wizard).
//
// Both halves matter. The dashboard raw string is where the missed caller
// lived, and static/ is where the surviving one lives. Reading the wizard from
// staticFS rather than from disk means this asserts against the bytes actually
// shipped in the binary.
func collectInTreeJSSources(t *testing.T) []inTreeJSSource {
	t.Helper()

	srcs := []inTreeJSSource{{name: "pkg/hub/saas.go:dashboardHTML", body: dashboardHTML}}

	entries, err := staticFS.ReadDir("static")
	if err != nil {
		t.Fatalf("read embedded static dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".html") && !strings.HasSuffix(name, ".js") {
			continue
		}
		b, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatalf("read embedded static/%s: %v", name, err)
		}
		srcs = append(srcs, inTreeJSSource{name: "pkg/hub/static/" + name, body: string(b)})
	}

	// A guard on the guard: if the embed ever stops delivering files, every
	// scan below would vacuously pass. Require the wizard specifically, since
	// it is the caller we expect to find.
	var sawWizard bool
	for _, s := range srcs {
		if strings.HasSuffix(s.name, "get-started.html") {
			sawWizard = true
		}
	}
	if !sawWizard {
		t.Fatal("get-started.html not found in embedded static FS — the audit below would scan nothing")
	}
	return srcs
}

// TestRequestProvisionInTreeCallersSendRequiredFields is the regression test
// for the dashboard-modal breakage: it fails if any in-tree JS caller of
// request-provision omits a field the handler requires.
//
// Had this existed, #2369 would have failed CI — the dashboard modal's body
// carried no full_name — instead of shipping a dead button to production.
func TestRequestProvisionInTreeCallersSendRequiredFields(t *testing.T) {
	fetchRe := regexp.MustCompile(`fetch\(\s*['"` + "`" + `][^'"` + "`" + `]*` + regexp.QuoteMeta(requestProvisionPath))

	var callers int
	for _, src := range collectInTreeJSSources(t) {
		for _, loc := range fetchRe.FindAllStringIndex(src.body, -1) {
			callers++
			end := loc[1] + jsBodyWindow
			if end > len(src.body) {
				end = len(src.body)
			}
			window := src.body[loc[1]:end]

			for _, key := range requestProvisionRequiredJSONKeys {
				// Match the key as a JS object property, the form every caller
				// uses to build the body (`full_name: name` / `"full_name":`).
				keyRe := regexp.MustCompile(`['"` + "`" + `]?` + regexp.QuoteMeta(key) + `['"` + "`" + `]?\s*:`)
				if !keyRe.MatchString(window) {
					t.Errorf("%s: caller of POST %s does not send required field %q.\n"+
						"handleRequestProvision rejects requests missing it, so this caller "+
						"gets a 400 the user cannot fix from the UI.\n"+
						"Either send the field or, if this caller is being retired, remove it.\n"+
						"body scanned:\n%s",
						src.name, requestProvisionPath, key, window)
				}
			}
		}
	}

	// Without this the test would pass if every caller vanished or the regex
	// silently stopped matching — the same class of false green as the
	// `_ = w.Code` assertions this PR also repairs.
	if callers == 0 {
		t.Fatalf("found no in-tree JS caller of POST %s; the scan is not matching anything and would never catch a regression", requestProvisionPath)
	}
	t.Logf("audited %d in-tree JS caller(s) of POST %s", callers, requestProvisionPath)
}

// TestRequestProvisionDashboardModalRemoved pins the removal itself.
//
// The dashboard's Request-a-Hive modal was deliberately retired so the
// 3-step get-started wizard is the single supported request path. This asserts
// none of its symbols creep back into the dashboard: a reintroduced modal would
// be a second request path, and — being inline JS — invisible to everything
// except the audit above.
func TestRequestProvisionDashboardModalRemoved(t *testing.T) {
	// btn-request-hive is deliberately absent from this list. The retired modal
	// used that id on a <button onclick="openRequestHiveModal()"> that opened the
	// inline modal. #2693 reused the id for a plain <a href="/get-started"> link
	// that routes to the wizard — it opens no modal and posts nothing. The modal's
	// own symbols below (openRequestHiveModal, submitProvisionRequest, etc.) remain
	// the real guard; pinning a bare element id would fail on a benign navigation
	// link that is the opposite of a second request path.
	removed := []string{
		"request-modal",
		"btn-request-go",
		"openRequestHiveModal",
		"submitProvisionRequest",
		"renderRequestHiveButton",
		"renderRequestForgeOptions",
		"requestForgeValue",
		"syncRequestForgePreview",
		"renderRequestLevelOptions",
	}
	for _, sym := range removed {
		if strings.Contains(dashboardHTML, sym) {
			t.Errorf("dashboardHTML still references %q — the Request-a-Hive modal was removed on purpose "+
				"so the get-started wizard is the only request path; re-adding it reintroduces a second, "+
				"untested caller of POST %s", sym, requestProvisionPath)
		}
	}
}
