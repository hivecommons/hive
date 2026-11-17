package hub

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	lfLegalCopyright = "Copyright Hive Commons Series of LF Projects, LLC"
	lfLegalPolicies  = `For website terms of use, trademark policy and other project policies please see <a href="https://lfprojects.org/policies/">lfprojects.org/policies/</a>.`
)

func assertLFLegalFooter(t *testing.T, name, page string) {
	t.Helper()
	if strings.Count(page, lfLegalCopyright) != 1 {
		t.Errorf("%s: want exactly one %q, got %d", name, lfLegalCopyright, strings.Count(page, lfLegalCopyright))
	}
	if !strings.Contains(page, lfLegalPolicies) {
		t.Errorf("%s: missing LF Projects policies line linking https://lfprojects.org/policies/", name)
	}
}

// Every embedded hub HTML page carries the LF Projects legal footer.
func TestLFLegalFooterOnEveryStaticHubPage(t *testing.T) {
	pages, err := fs.Glob(staticFS, "static/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("no embedded static HTML pages found")
	}
	for _, p := range pages {
		data, err := staticFS.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		assertLFLegalFooter(t, p, string(data))
	}
	assertLFLegalFooter(t, "assets/dashboard.html", dashboardHTML)
}

func TestLFLegalFooterConstantIsFmtSafe(t *testing.T) {
	if strings.Contains(lfProjectsLegalFooterHTML, "%") {
		t.Fatal("lfProjectsLegalFooterHTML is concatenated into fmt templates and must not contain '%'")
	}
	assertLFLegalFooter(t, "lfProjectsLegalFooterHTML", lfProjectsLegalFooterHTML)
}

func TestLFLegalFooterOnHubErrorPage(t *testing.T) {
	rec := httptest.NewRecorder()
	(&HubServer{}).writeHubErrorPage(rec, http.StatusNotFound)
	body := rec.Body.String()
	if strings.Contains(body, "%!") {
		t.Fatalf("error page has a fmt formatting error:\n%s", body)
	}
	assertLFLegalFooter(t, "hub error page", body)
}

func TestLFLegalFooterOnContributeLanding(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hive.hivecommons.dev")
	s := newHubServerForTest(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://hive.hivecommons.dev/contribute", nil)
	s.handleContributeLanding(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "%!") {
		t.Fatalf("contribute landing has a fmt formatting error:\n%s", body)
	}
	assertLFLegalFooter(t, "contribute landing", body)
}

func TestLFLegalFooterOnSignInAndPreviewPages(t *testing.T) {
	rec := httptest.NewRecorder()
	(&HubServer{}).writeProviderPicker(rec, nil, "")
	assertLFLegalFooter(t, "sign-in page", rec.Body.String())

	rec = httptest.NewRecorder()
	writeLinkPreview(rec)
	assertLFLegalFooter(t, "link preview page", rec.Body.String())

	rec = httptest.NewRecorder()
	newHubServerForTest(t).writeHiveAccessDeniedPage(rec, "octo", "demo", "demo.example.test")
	assertLFLegalFooter(t, "hive access denied page", rec.Body.String())
}
