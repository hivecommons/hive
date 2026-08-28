package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// hosted_namespace_identity.go — sanitizeLabelValue /
// hiveNamespaceIdentityLabels / stampHostedNamespaceIdentity, plus the three
// call sites that must stamp a hosted namespace's identity: provisionHive
// (creation), handleApproveProvision (claim), handleAssignHive (assign).
// ============================================================

func TestSanitizeLabelValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"already valid", "acme-repo", "acme-repo"},
		{"caps folded", "TradingAsBuddies", "tradingasbuddies"},
		{"spaces stripped", "Trading As Buddies", "tradingasbuddies"},
		{"unicode dropped", "café-org", "caf-org"},
		{"leading/trailing dash trimmed", "-acme-", "acme"},
		{"leading/trailing dot trimmed", ".acme.", "acme"},
		{"only separators", "---", ""},
		{"dots and dashes preserved mid-string", "acme.corp-inc", "acme.corp-inc"},
		{"underscore dropped", "acme_corp", "acmecorp"},
		{"whitespace-only", "   ", ""},
		{"exactly 63 chars kept whole", strings.Repeat("a", 63), strings.Repeat("a", 63)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeLabelValue(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeLabelValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > maxLabelValueLen {
				t.Errorf("sanitizeLabelValue(%q) = %q exceeds %d chars", tc.in, got, maxLabelValueLen)
			}
		})
	}
}

func TestSanitizeLabelValueLongInputTruncatedAndRetrimmed(t *testing.T) {
	// 70 'a's followed by a run of dashes that would land exactly at the cut
	// point — truncation must not leave a trailing separator.
	in := strings.Repeat("a", 60) + "----------"
	got := sanitizeLabelValue(in)
	if len(got) > maxLabelValueLen {
		t.Fatalf("result exceeds max length: %d", len(got))
	}
	if strings.HasSuffix(got, "-") || strings.HasSuffix(got, ".") {
		t.Errorf("sanitizeLabelValue(%q) = %q ends in a separator after truncation", in, got)
	}
	if got != strings.Repeat("a", 60) {
		t.Errorf("sanitizeLabelValue(%q) = %q, want %q", in, got, strings.Repeat("a", 60))
	}
}

func TestSanitizeLabelValueNeverProducesInvalidValue(t *testing.T) {
	// Fuzz-lite: a battery of adversarial inputs must never yield a value
	// starting/ending in '-'/'.' or exceeding the length limit.
	inputs := []string{
		"", " ", "\t\n", "----...----", "A.B.C", "..hidden", "trailing.",
		"日本語", "MiXeD_Case-Name.With.Dots", strings.Repeat("x-", 40),
		"🎉party🎉", "a", "-", ".", "a-",
	}
	for _, in := range inputs {
		got := sanitizeLabelValue(in)
		if got == "" {
			continue
		}
		if len(got) > maxLabelValueLen {
			t.Errorf("sanitizeLabelValue(%q) = %q too long (%d)", in, got, len(got))
		}
		if strings.HasPrefix(got, "-") || strings.HasPrefix(got, ".") ||
			strings.HasSuffix(got, "-") || strings.HasSuffix(got, ".") {
			t.Errorf("sanitizeLabelValue(%q) = %q has leading/trailing separator", in, got)
		}
		for _, r := range got {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' {
				t.Errorf("sanitizeLabelValue(%q) = %q contains disallowed rune %q", in, got, r)
			}
		}
	}
}

func TestHiveNamespaceIdentityLabels(t *testing.T) {
	labels, annotations := hiveNamespaceIdentityLabels("TradingAsBuddies", "falcon-core", "hosted-falcon-buddies-a1b2")

	if got := labels[hiveNameLabel]; got != "tradingasbuddies" {
		t.Errorf("hive-name label = %q, want %q", got, "tradingasbuddies")
	}
	if got := labels[hiveOrgLabel]; got != "falcon-core" {
		t.Errorf("org label = %q, want %q", got, "falcon-core")
	}
	if got := labels[hiveIDLabel]; got != "hosted-falcon-buddies-a1b2" {
		t.Errorf("hive-id label = %q, want %q", got, "hosted-falcon-buddies-a1b2")
	}
	if got := annotations[hiveDisplayNameAnnotation]; got != "TradingAsBuddies" {
		t.Errorf("display-name annotation = %q, want exact unsanitized %q", got, "TradingAsBuddies")
	}
}

func TestHiveNamespaceIdentityLabelsOmitsEmptyFields(t *testing.T) {
	// A hive with no name yet (a bare pool placeholder) must not get an empty
	// or invalid hive-name label, and must not get a display-name annotation
	// with an empty value.
	labels, annotations := hiveNamespaceIdentityLabels("", "", "hosted-oke-03-placeholder-y99x")

	if _, ok := labels[hiveNameLabel]; ok {
		t.Errorf("hive-name label present with empty input: %v", labels)
	}
	if _, ok := labels[hiveOrgLabel]; ok {
		t.Errorf("org label present with empty input: %v", labels)
	}
	if got := labels[hiveIDLabel]; got != "hosted-oke-03-placeholder-y99x" {
		t.Errorf("hive-id label = %q, want %q", got, "hosted-oke-03-placeholder-y99x")
	}
	if _, ok := annotations[hiveDisplayNameAnnotation]; ok {
		t.Errorf("display-name annotation present with empty input: %v", annotations)
	}
}

func TestHiveNamespaceIdentityLabelsAllEmpty(t *testing.T) {
	labels, annotations := hiveNamespaceIdentityLabels("", "", "")
	if len(labels) != 0 {
		t.Errorf("expected no labels for all-empty input, got %v", labels)
	}
	if len(annotations) != 0 {
		t.Errorf("expected no annotations for all-empty input, got %v", annotations)
	}
}

// installLoggingKubectl writes a fake kubectl that appends its full argument
// list to a log file (one line per invocation) and exits 0, so tests can
// assert exactly what stampHostedNamespaceIdentity ran without a real
// cluster. Returns the log file path.
func installLoggingKubectl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl.log")
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
echo "{}"
exit 0
`
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestStampHostedNamespaceIdentityInvokesKubectlLabelAndAnnotate(t *testing.T) {
	logPath := installLoggingKubectl(t)
	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true}

	stampHostedNamespaceIdentity(cluster, "hive-hosted-hosted-falcon-buddies-a1b2",
		"TradingAsBuddies", "falcon-core", "hosted-falcon-buddies-a1b2", slog.Default())

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading kubectl log: %v", err)
	}
	logged := string(data)

	if !strings.Contains(logged, "label namespace hive-hosted-hosted-falcon-buddies-a1b2 --overwrite") {
		t.Errorf("expected a kubectl label invocation, got:\n%s", logged)
	}
	if !strings.Contains(logged, hiveNameLabel+"=tradingasbuddies") {
		t.Errorf("expected hive-name label in kubectl args, got:\n%s", logged)
	}
	if !strings.Contains(logged, hiveOrgLabel+"=falcon-core") {
		t.Errorf("expected org label in kubectl args, got:\n%s", logged)
	}
	if !strings.Contains(logged, hiveIDLabel+"=hosted-falcon-buddies-a1b2") {
		t.Errorf("expected hive-id label in kubectl args, got:\n%s", logged)
	}
	if !strings.Contains(logged, "annotate namespace hive-hosted-hosted-falcon-buddies-a1b2 --overwrite") {
		t.Errorf("expected a kubectl annotate invocation, got:\n%s", logged)
	}
	if !strings.Contains(logged, hiveDisplayNameAnnotation+"=TradingAsBuddies") {
		t.Errorf("expected display-name annotation with EXACT unsanitized name, got:\n%s", logged)
	}
}

func TestStampHostedNamespaceIdentityNoopOnNilClusterOrEmptyNamespace(t *testing.T) {
	logPath := installLoggingKubectl(t)

	stampHostedNamespaceIdentity(nil, "hive-hosted-x", "name", "org", "id", slog.Default())
	stampHostedNamespaceIdentity(&ClusterConfig{ID: "c", InCluster: true}, "", "name", "org", "id", slog.Default())

	if data, err := os.ReadFile(logPath); err == nil && len(data) != 0 {
		t.Errorf("expected no kubectl invocations, got:\n%s", string(data))
	}
}

func TestStampHostedNamespaceIdentityToleratesKubectlFailure(t *testing.T) {
	// A failing kubectl must not panic and must be swallowed (best-effort).
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Must not panic.
	stampHostedNamespaceIdentity(&ClusterConfig{ID: "c", InCluster: true}, "hive-hosted-x", "n", "o", "i", slog.Default())
}

func TestHostedNamespaceForHive(t *testing.T) {
	if got := hostedNamespaceForHive(nil); got != "" {
		t.Errorf("hostedNamespaceForHive(nil) = %q, want empty", got)
	}
	h := &SaaSHive{ID: "hosted-acme-repo-abcd"}
	if got := hostedNamespaceForHive(h); got != "hive-hosted-hosted-acme-repo-abcd" {
		t.Errorf("hostedNamespaceForHive = %q, want %q", got, "hive-hosted-hosted-acme-repo-abcd")
	}
}

// TestProvisionHiveStampsNamespaceIdentity guards entry point 1: automatic
// (pool) provisioning must stamp what's known at creation time, at minimum
// the hive_id.
func TestProvisionHiveStampsNamespaceIdentity(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	logPath := installLoggingKubectl(t)

	h := &SaaSHive{ID: "hosted-acme-repo-abcd", Owner: "alice", Org: "acme", Repos: []string{"repo"}, PrimaryRepo: "repo", ACMMLevel: 2}
	req := &CreateHiveRequest{Org: "acme", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 2, ClusterID: "hive-oke"}

	if err := provisionHive(h, req, dynamicCluster(), nil, slog.Default()); err != nil {
		t.Fatalf("provisionHive: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading kubectl log: %v", err)
	}
	logged := string(data)
	if !strings.Contains(logged, "label namespace hive-hosted-hosted-acme-repo-abcd") {
		t.Errorf("expected provisionHive to label the namespace it just created, got:\n%s", logged)
	}
	if !strings.Contains(logged, hiveIDLabel+"=hosted-acme-repo-abcd") {
		t.Errorf("expected hive-id label stamped at creation, got:\n%s", logged)
	}
}

// TestHandleApproveProvisionStampsNamespaceIdentity guards entry point 2: the
// claim path is where the hive's real name/org first become known, so the
// namespace labels must be (re)written here.
func TestHandleApproveProvisionStampsNamespaceIdentity(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	logPath := installLoggingKubectl(t)
	mkUser(t, hubAdminUsername)

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, saveCh: make(chan struct{}, 1), clusters: map[string]ClusterConfig{"hive-oke": *dynamicCluster()}}

	saveProvisionRequest(&ProvisionRequest{
		Username: "bob", Org: "falcon-core", Repos: "buddies", PrimaryRepo: "buddies", ACMMLevel: 2,
		AuthMethod: "public", RequestedAt: time.Now().UTC().Format(time.RFC3339), Status: provisionStatusPending,
	})
	saveSaaSHive(&SaaSHive{ID: "hosted-avail-y99x", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/approve-prov", "", hubAdminUsername), "username", "bob")
	s.handleApproveProvision(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body=%s", rec.Code, rec.Body.String())
	}
	// The stamp runs in the background now (kickClaimClusterWorkAsync) so the
	// approve dialog is never held behind kubectl — drain it before asserting.
	provisionWG.Wait()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading kubectl log: %v", err)
	}
	logged := string(data)
	if !strings.Contains(logged, "label namespace hive-hosted-hosted-avail-y99x --overwrite") {
		t.Errorf("expected claim to (re)label the placeholder's namespace, got:\n%s", logged)
	}
	if !strings.Contains(logged, hiveOrgLabel+"=falcon-core") {
		t.Errorf("expected org label with the newly-claimed org, got:\n%s", logged)
	}
}

// ============================================================
// Option B — hiveNameHostLabel / hiveNameVanityHost /
// vanityHostDomainFromExistingRoute, plus handleAssignHive preferring a
// name-bearing vanity host when the hive has a display name.
// ============================================================

func TestHiveNameHostLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"already valid", "acme-repo", "acme-repo"},
		{"caps folded", "TradingAsBuddies", "tradingasbuddies"},
		{"spaces stripped", "Trading As Buddies", "tradingasbuddies"},
		{"dots dropped (unlike label sanitizer)", "acme.corp", "acmecorp"},
		{"leading/trailing dash trimmed", "-acme-", "acme"},
		{"only dashes", "----", ""},
		{"underscore dropped", "acme_corp", "acmecorp"},
		{"unicode dropped", "café", "caf"},
		{"exactly 63 chars kept whole", strings.Repeat("a", 63), strings.Repeat("a", 63)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hiveNameHostLabel(tc.in)
			if got != tc.want {
				t.Errorf("hiveNameHostLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > maxLabelValueLen {
				t.Errorf("hiveNameHostLabel(%q) = %q exceeds %d chars", tc.in, got, maxLabelValueLen)
			}
			for _, r := range got {
				if r == '.' {
					t.Errorf("hiveNameHostLabel(%q) = %q contains a dot, which is not a valid DNS-label character", tc.in, got)
				}
			}
		})
	}
}

func TestHiveNameHostLabelLongInputTruncatedAndRetrimmed(t *testing.T) {
	in := strings.Repeat("a", 60) + "----------"
	got := hiveNameHostLabel(in)
	if len(got) > maxLabelValueLen {
		t.Fatalf("result exceeds max length: %d", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("hiveNameHostLabel(%q) = %q ends in a dash after truncation", in, got)
	}
}

func TestHiveNameVanityHost(t *testing.T) {
	host := hiveNameVanityHost("TradingAsBuddies", "apps.example.com")
	if !strings.HasPrefix(host, "tradingasbuddies-") {
		t.Errorf("hiveNameVanityHost = %q, want prefix %q", host, "tradingasbuddies-")
	}
	if !strings.HasSuffix(host, ".apps.example.com") {
		t.Errorf("hiveNameVanityHost = %q, want suffix %q", host, ".apps.example.com")
	}
	// label-suffix-domain: exactly one more dash-delimited random suffix
	// between the sanitized label and the domain.
	labelAndSuffix := strings.TrimSuffix(host, ".apps.example.com")
	parts := strings.Split(labelAndSuffix, "-")
	if len(parts) < 2 {
		t.Fatalf("hiveNameVanityHost = %q, want a label-suffix pair before the domain", host)
	}
	suffix := parts[len(parts)-1]
	if len(suffix) != vanityHostSuffixLen {
		t.Errorf("suffix %q has length %d, want %d", suffix, len(suffix), vanityHostSuffixLen)
	}
}

func TestHiveNameVanityHostUniquenessAcrossCalls(t *testing.T) {
	// Two hives with the SAME sanitized name must not collide on the same
	// host — the random suffix is what prevents that.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		h := hiveNameVanityHost("TradingAsBuddies", "apps.example.com")
		if seen[h] {
			t.Fatalf("hiveNameVanityHost produced a duplicate host across calls: %q", h)
		}
		seen[h] = true
	}
}

func TestHiveNameVanityHostEmptyOnUnusableInput(t *testing.T) {
	if got := hiveNameVanityHost("TradingAsBuddies", ""); got != "" {
		t.Errorf("hiveNameVanityHost with empty domain = %q, want empty (never fabricate a domain)", got)
	}
	if got := hiveNameVanityHost("", "apps.example.com"); got != "" {
		t.Errorf("hiveNameVanityHost with unsanitizable name = %q, want empty", got)
	}
	if got := hiveNameVanityHost("---", "apps.example.com"); got != "" {
		t.Errorf("hiveNameVanityHost(%q) = %q, want empty (name sanitizes to nothing)", "---", got)
	}
}

func TestVanityHostDomainFromExistingRoute(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hosted-acme-repo-abcd.apps.example.com", "apps.example.com"},
		{"hosted-x.hive.kubestellar.io", "hive.kubestellar.io"},
		{"no-dot-here", ""},
		{"", ""},
		{"trailing-dot.", ""},
	}
	for _, tc := range cases {
		if got := vanityHostDomainFromExistingRoute(tc.in); got != tc.want {
			t.Errorf("vanityHostDomainFromExistingRoute(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHandleAssignHivePrefersNameBearingVanityHost guards Option B: at
// assignment, a hive with a display name (ProjectName) gets a vanity host
// built from that name rather than the org/repo-derived one, without
// changing the namespace, and via the SAME get-or-create route-creation seam
// (vanityHostServable, stubbed here exactly as the existing vanity-URL tests
// stub it) — so this only proves the HOST CHOICE changed, not the mechanism.
func TestHandleAssignHivePrefersNameBearingVanityHost(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)

	s := &HubServer{
		logger: slog.Default(), hubSecret: testHubSecret, saveCh: make(chan struct{}, 1),
		clusters: map[string]ClusterConfig{"hive-oke": *dynamicCluster()},
	}
	var gotHost string
	s.vanityHostServable = func(_, host string, _ *ClusterConfig) error {
		gotHost = host
		return nil
	}
	saveSaaSHive(&SaaSHive{ID: "hosted-avail-nb01", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})

	body := `{"owner":"carol","org":"falcon-core","repos":"buddies","primary_repo":"buddies","acmm_level":2,"project_name":"TradingAsBuddies"}`
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/hosted-avail-nb01/assign", body, hubAdminUsername), "id", "hosted-avail-nb01")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d body=%s", rec.Code, rec.Body.String())
	}
	// The mint runs in the background now (kickClaimClusterWorkAsync) so the
	// assign dialog is never held behind kubectl — drain it before asserting.
	provisionWG.Wait()

	if !strings.HasPrefix(gotHost, "tradingasbuddies-") {
		t.Errorf("vanity host = %q, want a name-bearing host prefixed %q", gotHost, "tradingasbuddies-")
	}

	got := loadSaaSHive("hosted-avail-nb01")
	if got == nil {
		t.Fatal("expected saved hive")
	}
	if !strings.Contains(got.VanityURL, "tradingasbuddies-") {
		t.Errorf("VanityURL = %q, want it to contain the name-bearing host", got.VanityURL)
	}
	// The namespace itself must be untouched — same id as before assignment.
	if got.ID != "hosted-avail-nb01" {
		t.Errorf("hive ID changed from %q to %q — namespace must never be renamed/migrated", "hosted-avail-nb01", got.ID)
	}
}

// TestHandleAssignHiveFallsBackToOrgRepoHostWithoutDisplayName guards that a
// hive assigned with no project_name behaves EXACTLY as before this feature:
// the org/repo-derived vanity host, unchanged.
func TestHandleAssignHiveFallsBackToOrgRepoHostWithoutDisplayName(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)

	s := &HubServer{
		logger: slog.Default(), hubSecret: testHubSecret, saveCh: make(chan struct{}, 1),
		clusters: map[string]ClusterConfig{"hive-oke": *dynamicCluster()},
	}
	var gotHost string
	s.vanityHostServable = func(_, host string, _ *ClusterConfig) error {
		gotHost = host
		return nil
	}
	saveSaaSHive(&SaaSHive{ID: "hosted-avail-nb02", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})

	body := `{"owner":"dave","org":"acme","repos":"widgets","primary_repo":"widgets","acmm_level":2}`
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/hosted-avail-nb02/assign", body, hubAdminUsername), "id", "hosted-avail-nb02")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d body=%s", rec.Code, rec.Body.String())
	}
	provisionWG.Wait() // background mint — drain before asserting the host

	if !strings.HasPrefix(gotHost, "hosted-acme-widgets-") {
		t.Errorf("vanity host = %q, want the unchanged org/repo-derived host prefixed %q", gotHost, "hosted-acme-widgets-")
	}
}

// TestK8sManifestTemplateSetsPodNamespaceDownwardAPI guards that the spoke
// Deployment template wires POD_NAMESPACE via the Kubernetes downward API
// (fieldRef: metadata.namespace) — the spoke-side source of truth
// pkg/dashboard/pod_namespace.go's podNamespace() reads first, for the Hub
// tab's namespace row.
func TestK8sManifestTemplateSetsPodNamespaceDownwardAPI(t *testing.T) {
	if !strings.Contains(k8sManifestTemplate, "name: POD_NAMESPACE") {
		t.Error("k8sManifestTemplate is missing a POD_NAMESPACE env var")
	}
	if !strings.Contains(k8sManifestTemplate, "fieldPath: metadata.namespace") {
		t.Error("k8sManifestTemplate's POD_NAMESPACE is not wired via the downward API (fieldRef: metadata.namespace)")
	}
}

// TestStatusHoverRendersNamespace pins the hub dashboard's status hover
// (healthBadge in the inline dashboardHTML JS) to include the hosted
// namespace line — the hub-side surface an operator uses to map a
// Kubernetes namespace back to a hive without touching the cluster. Mirrors
// the structure-pin style of TestHoverPanelRendersConsolidatedSections in
// hover_timeline_relay_test.go.
func TestStatusHoverRendersNamespace(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"namespace line is composed into the hover", "lines.push('ns: ' + hns);"},
		{"hiveNamespace helper exists", "function hiveNamespace(h)"},
		{"hiveNamespace derives from hive-hosted- + id", "return 'hive-hosted-' + h.id;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}

// TestHandleAssignHiveStampsNamespaceIdentity guards entry point 3: the
// assign/reassign path must refresh the namespace labels too.
func TestHandleAssignHiveStampsNamespaceIdentity(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	logPath := installLoggingKubectl(t)
	mkUser(t, hubAdminUsername)

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, saveCh: make(chan struct{}, 1), clusters: map[string]ClusterConfig{"hive-oke": *dynamicCluster()}}
	saveSaaSHive(&SaaSHive{ID: "hosted-avail-z01a", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})

	body := `{"owner":"carol","org":"acme","repos":"repo1","primary_repo":"repo1","acmm_level":2}`
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/hosted-avail-z01a/assign", body, hubAdminUsername), "id", "hosted-avail-z01a")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d body=%s", rec.Code, rec.Body.String())
	}
	provisionWG.Wait() // background stamp — drain before reading the log

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading kubectl log: %v", err)
	}
	logged := string(data)
	if !strings.Contains(logged, "label namespace hive-hosted-hosted-avail-z01a --overwrite") {
		t.Errorf("expected assign to (re)label the placeholder's namespace, got:\n%s", logged)
	}
	if !strings.Contains(logged, hiveOrgLabel+"=acme") {
		t.Errorf("expected org label with the newly-assigned org, got:\n%s", logged)
	}
}
