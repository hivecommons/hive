package hub

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// hivecommons/hive#19617: the hive list linked a self-hosted spoke straight
// to its dashboard, so the hub's /open SSO handoff never ran and the user
// landed on the spoke's own GitHub sign-in page. hiveOpenURL is now the one
// href decision dashboardLink, spokeOpenLink and the hive-name cell share;
// these cases run the real functions from dashboardHTML under node.

type hiveOpenLinkResult struct {
	Open      string `json:"open"`
	SpokeLink string `json:"spokeLink"`
	DashLink  string `json:"dashLink"`
}

func runHiveOpenLinkJS(t *testing.T, hives map[string]string, spokeDomain string) map[string]hiveOpenLinkResult {
	t.Helper()
	var cases []string
	for name, hive := range hives {
		cases = append(cases, "  "+jsString(name)+": "+hive+",")
	}
	src := strings.Join([]string{
		"var SELFSERVICE_ID_PREFIX = 'byo-';",
		"var SSO_NAV_ATTR = 'data-sso-open';",
		"var _hubSpokeDomain = " + jsString(spokeDomain) + ";",
		// esc builds a DOM node in the browser; this stand-in escapes the same
		// markup characters without a document.
		"function esc(s) { return String(s || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }",
		jsFunc(t, "resolvedBase"),
		jsFunc(t, "hiveOpenURL"),
		jsFunc(t, "ssoDisplayLink"),
		jsFunc(t, "spokeOpenLink"),
		jsFunc(t, "dashboardLink"),
		"var cases = {",
		strings.Join(cases, "\n"),
		"};",
		"var result = {};",
		"Object.keys(cases).forEach(function(k) { var h = cases[k]; result[k] = {open: hiveOpenURL(h), spokeLink: spokeOpenLink(h, 'x', 'hive-name-link'), dashLink: dashboardLink(h)}; });",
		"process.stdout.write(JSON.stringify(result));",
	}, "\n")
	cmd := exec.Command("node", "-e", src)
	stdout, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Skipf("node unavailable: %v", err)
	}
	got := map[string]hiveOpenLinkResult{}
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatalf("bad JSON from node: %v\ngot: %s", err, stdout)
	}
	return got
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestHiveListOpensSpokesThroughSSOHandoff(t *testing.T) {
	got := runHiveOpenLinkJS(t, map[string]string{
		// The reported case: plain id, hub-assigned type "local", reports a
		// dashboard URL. Must go through /open like a hosted spoke.
		"selfHosted": `{id: 'acme-prod', hiveType: 'local', dashboardUrl: 'https://hive.acme.example'}`,
		// Hosted rows keep the handoff, including before any URL is known.
		"hosted":       `{id: 'hosted-abc', hiveType: 'hosted', dashboardUrl: 'https://abc.hive.example'}`,
		"hostedNoURL":  `{id: 'saas-xyz', hiveType: ''}`,
		"encodedID":    `{id: 'a/b c', hiveType: 'local', dashboardUrl: 'https://odd.example'}`,
		"selfService":  `{id: 'byo-alice-lab', hiveType: 'self-hosted', dashboardUrl: 'https://lab.alice.example'}`,
		"noURL":        `{id: 'acme-dev', hiveType: 'local'}`,
		"localhostURL": `{id: 'acme-laptop', hiveType: 'local', dashboardUrl: 'http://localhost:3001'}`,
	}, "")

	wantOpen := map[string]string{
		"selfHosted":   "/api/saas/hives/acme-prod/open",
		"hosted":       "/api/saas/hives/hosted-abc/open",
		"hostedNoURL":  "/api/saas/hives/saas-xyz/open",
		"encodedID":    "/api/saas/hives/a%2Fb%20c/open",
		"selfService":  "",
		"noURL":        "",
		"localhostURL": "",
	}
	for name, want := range wantOpen {
		r, ok := got[name]
		if !ok {
			t.Fatalf("case %s missing from node output", name)
		}
		if r.Open != want {
			t.Errorf("%s: hiveOpenURL = %q, want %q", name, r.Open, want)
		}
		if want != "" {
			// A left-click on the name/callout link and the Dashboard menu
			// link must both reach the handoff.
			if !strings.Contains(r.SpokeLink, `data-sso-open="`+want+`"`) && !strings.Contains(r.SpokeLink, `href="`+want+`"`) {
				t.Errorf("%s: spokeOpenLink does not open %s: %s", name, want, r.SpokeLink)
			}
			if !strings.Contains(r.DashLink, `href="`+want+`"`) {
				t.Errorf("%s: dashboardLink does not open %s: %s", name, want, r.DashLink)
			}
		}
	}

	// The self-hosted row keeps its dashboard URL as the hover/copy target,
	// exactly as a hosted row shows its vanity host.
	if s := got["selfHosted"].SpokeLink; !strings.Contains(s, `href="https://hive.acme.example"`) {
		t.Errorf("selfHosted: spokeOpenLink should display the dashboard URL: %s", s)
	}

	// byo- spokes never hold the hub secret (#18060): direct link, no handoff.
	ss := got["selfService"]
	if strings.Contains(ss.SpokeLink, "data-sso-open") || !strings.Contains(ss.SpokeLink, `href="https://lab.alice.example"`) {
		t.Errorf("selfService: spokeOpenLink should link the dashboard directly: %s", ss.SpokeLink)
	}
	if !strings.Contains(ss.DashLink, `href="https://lab.alice.example"`) {
		t.Errorf("selfService: dashboardLink should link the dashboard directly: %s", ss.DashLink)
	}

	// A row with nowhere to go stays plain text.
	for _, name := range []string{"noURL", "localhostURL"} {
		if got[name].SpokeLink != "" {
			t.Errorf("%s: spokeOpenLink = %q, want empty", name, got[name].SpokeLink)
		}
		if strings.Contains(got[name].DashLink, "<a ") {
			t.Errorf("%s: dashboardLink should not be a link: %s", name, got[name].DashLink)
		}
	}
}
