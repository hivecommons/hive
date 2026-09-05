package hub

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

// #5977: a fleet wildcard certificate existed and served nothing, because every
// provisioned spoke Ingress carried its own tls: block and an explicit tls:
// block always wins over the controller's --default-ssl-certificate. ~195
// Ingress TLS references and ~60 Certificate objects were doing what one
// wildcard already covered, against an ACME cap of 50 issuances per week.
//
// The asymmetry these tests are built around: wrongly minting a certificate
// costs one issuance out of fifty; wrongly OMITTING one makes ingress-nginx
// serve its self-signed default and takes down every dashboard on the cluster
// at once. So every uncertain case must resolve to "keep the per-host cert".

// ── wildcard coverage ──────────────────────────────────────────────────────

func TestWildcardCoversHost(t *testing.T) {
	const domain = "hive.hivecommons.dev"
	cases := []struct {
		name string
		host string
		want bool
	}{
		{"placeholder spoke host", "hosted-acme-web-ab12.hive.hivecommons.dev", true},
		{"vanity spoke host", "myorg-myrepo.hive.hivecommons.dev", true},
		{"case and trailing dot are not significant", "Hosted-ACME.hive.hivecommons.dev.", true},

		// RFC 6125 §6.4.3: a wildcard matches exactly ONE label.
		{"the apex itself is not covered", "hive.hivecommons.dev", false},
		{"a deeper name is not covered", "a.b.hive.hivecommons.dev", false},

		// #5925's host sits one level ABOVE the wildcard's scope. A check that
		// answered on the domain suffix alone would wrongly say yes, and that
		// mistake is exactly what would drop the tls: block on a host nothing
		// serves.
		{"a name above the wildcard scope is not covered", "dibs.hivecommons.dev", false},

		{"a different domain is not covered", "hosted-x.hive.kubestellar.io", false},
		{"a suffix that is not a label boundary is not covered", "evilhive.hivecommons.dev", false},
		{"empty host", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wildcardCoversHost(tc.host, domain); got != tc.want {
				t.Errorf("wildcardCoversHost(%q, %q) = %v, want %v", tc.host, domain, got, tc.want)
			}
		})
	}
}

func TestWildcardCoversHostEmptyDomainCoversNothing(t *testing.T) {
	// A cluster with no Domain is already rejected by validateClusterEntries,
	// but the coverage check must not treat "" as a domain that matches
	// everything — that would omit the tls: block for every host on earth.
	if wildcardCoversHost("anything.example.com", "") {
		t.Error("an empty domain covered a host")
	}
}

// ── the per-cluster opt-in ─────────────────────────────────────────────────

func wildcardCluster() ClusterConfig {
	return ClusterConfig{
		ID:                "hive-oke",
		IngressType:       "nginx",
		IngressClass:      "nginx",
		Domain:            "hive.hivecommons.dev",
		WildcardTLSSecret: "hive-hub/hive-wildcard-tls",
	}
}

// The opt-in is the whole safety mechanism: until an operator has put the
// wildcard secret on the cluster AND pointed --default-ssl-certificate at it,
// omitting the tls: block serves a self-signed certificate fleet-wide.
func TestServesHostFromWildcardRequiresTheOptIn(t *testing.T) {
	c := wildcardCluster()
	host := "hosted-x.hive.hivecommons.dev"

	if !c.servesHostFromWildcard(host) {
		t.Fatal("positive control: a fully configured cluster should serve this host from the wildcard")
	}

	c.WildcardTLSSecret = ""
	if c.servesHostFromWildcard(host) {
		t.Error("a cluster that has NOT opted in dropped the per-host certificate — this is the fleet-wide self-signed outage")
	}
	c.WildcardTLSSecret = "   "
	if c.servesHostFromWildcard(host) {
		t.Error("a whitespace-only secret name counted as an opt-in")
	}
}

// OpenShift Routes terminate TLS with edge termination and no secret, so they
// never minted a per-host certificate through this path and
// --default-ssl-certificate is an ingress-nginx concept that does not apply.
// Nothing to change there, so nothing is changed.
func TestServesHostFromWildcardSkipsOpenShiftRoutes(t *testing.T) {
	c := wildcardCluster()
	c.IngressType = ingressTypeOpenShiftRoute
	if c.servesHostFromWildcard("hosted-x.hive.hivecommons.dev") {
		t.Error("an OpenShift Route cluster was treated as wildcard-served")
	}
}

// Coverage is decided per HOST, not per cluster: a host outside the wildcard's
// single-label scope keeps its own certificate even on an opted-in cluster.
// This is what keeps hive.kubestellar.io and dibs working while every spoke
// stops minting.
func TestServesHostFromWildcardIsPerHost(t *testing.T) {
	c := wildcardCluster()
	for _, host := range []string{
		"hive.hivecommons.dev",     // the apex
		"a.b.hive.hivecommons.dev", // deeper than one label
		"hive.kubestellar.io",      // another domain entirely
		"dibs.hivecommons.dev",     // above the wildcard scope (#5925)
	} {
		if c.servesHostFromWildcard(host) {
			t.Errorf("%s was treated as wildcard-served on an opted-in cluster", host)
		}
	}
}

// ── the rendered manifest ──────────────────────────────────────────────────

// renderManifestWildcard renders the nginx branch with the wildcard decision
// set either way, so both shapes are asserted against the real template.
func renderManifestWildcard(t *testing.T, useWildcard bool) string {
	t.Helper()
	tmpl, err := template.New("manifests").Parse(k8sManifestTemplate)
	if err != nil {
		t.Fatalf("template parse: %v", err)
	}
	data := map[string]interface{}{
		"ID":               "hosted-hive-x",
		"HubPublicURL":     "https://hive.hivecommons.dev",
		"Namespace":        "hive-hosted-hosted-hive-x",
		"DashboardHost":    "hosted-hive-x.hive.hivecommons.dev",
		"DashboardPort":    8080,
		"TerminalPort":     7681,
		"CertIssuer":       "letsencrypt-dns01",
		"IngressClass":     "nginx",
		"IsNginxIngress":   true,
		"IsOpenShiftRoute": false,
		"UseWildcardTLS":   useWildcard,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("template exec: %v", err)
	}
	return buf.String()
}

// ingressBlocks returns the four provisioned nginx Ingress documents by name.
func ingressBlocks(t *testing.T, manifest string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, block := range docBlocks(manifest) {
		if !strings.Contains(block, "kind: Ingress") {
			continue
		}
		for _, name := range []string{"hive-api", "hive-contribute", "hive-terminal", "hive"} {
			if strings.Contains(block, "name: "+name+"\n") {
				out[name] = block
				break
			}
		}
	}
	return out
}

var provisionedIngresses = []string{"hive", "hive-api", "hive-contribute", "hive-terminal"}

// The default must be byte-for-byte the historical behaviour: a cluster that
// has not opted in keeps every tls: block and every issuer annotation.
func TestManifestKeepsPerHostTLSWithoutOptIn(t *testing.T) {
	blocks := ingressBlocks(t, renderManifestWildcard(t, false))
	if len(blocks) != len(provisionedIngresses) {
		t.Fatalf("found %d ingress blocks, want %d", len(blocks), len(provisionedIngresses))
	}
	for _, name := range provisionedIngresses {
		block := blocks[name]
		if !strings.Contains(block, "secretName: hive-tls") {
			t.Errorf("%s lost its tls: block without the opt-in:\n%s", name, block)
		}
		if !strings.Contains(block, "cert-manager.io/cluster-issuer: letsencrypt-dns01") {
			t.Errorf("%s lost its issuer annotation without the opt-in:\n%s", name, block)
		}
	}
}

// With the opt-in, BOTH halves go: the tls: block (which is what beats
// --default-ssl-certificate) and the issuer annotation (which is what makes
// cert-manager's ingress-shim mint the Certificate in the first place).
func TestManifestOmitsPerHostTLSWithOptIn(t *testing.T) {
	manifest := renderManifestWildcard(t, true)
	blocks := ingressBlocks(t, manifest)
	if len(blocks) != len(provisionedIngresses) {
		t.Fatalf("found %d ingress blocks, want %d", len(blocks), len(provisionedIngresses))
	}
	for _, name := range provisionedIngresses {
		block := blocks[name]
		if strings.Contains(block, "secretName:") {
			t.Errorf("%s still names a TLS secret; an explicit tls: block beats --default-ssl-certificate, so the wildcard would stay inert:\n%s", name, block)
		}
		if strings.Contains(block, "cert-manager.io/cluster-issuer") {
			t.Errorf("%s still carries the issuer annotation; ingress-shim would keep minting a per-host Certificate:\n%s", name, block)
		}
	}
	// Nothing else may be lost with them. The terminal's per-hive authorization
	// is the one that matters most: it is a CWE-862 control, not decoration.
	term := blocks["hive-terminal"]
	if !strings.Contains(term, "auth-url") || !strings.Contains(term, "hive=hosted-hive-x") {
		t.Errorf("hive-terminal lost its per-hive auth-url alongside the TLS change:\n%s", term)
	}
	if !strings.Contains(blocks["hive-contribute"], "proxy-read-timeout") {
		t.Errorf("hive-contribute lost an unrelated annotation:\n%s", blocks["hive-contribute"])
	}
}

// Every host still routes. Dropping TLS must not drop a rule — a spoke with no
// rule is a 404, which would be a worse outcome than the certificate churn.
func TestManifestKeepsRoutingWithOptIn(t *testing.T) {
	blocks := ingressBlocks(t, renderManifestWildcard(t, true))
	for _, name := range provisionedIngresses {
		if !strings.Contains(blocks[name], "host: hosted-hive-x.hive.hivecommons.dev") {
			t.Errorf("%s lost its host rule:\n%s", name, blocks[name])
		}
	}
}

// The template is hand-written YAML with conditional blocks, so the thing most
// likely to break is indentation — and a manifest that does not parse fails at
// provision time, on a cluster, not here. Parse every document in both modes.
func TestManifestIsValidYAMLInBothModes(t *testing.T) {
	for _, useWildcard := range []bool{false, true} {
		manifest := renderManifestWildcard(t, useWildcard)
		for i, block := range docBlocks(manifest) {
			if strings.TrimSpace(block) == "" {
				continue
			}
			var doc map[string]any
			if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
				t.Fatalf("useWildcard=%v: document %d does not parse as YAML: %v\n%s", useWildcard, i, err, block)
			}
		}
	}
}

// hive-api's ONLY annotation is the issuer, so the opt-in has to remove the
// `annotations:` key itself rather than leave it dangling with nothing under
// it. Asserted on the parsed object, because "annotations:" with an empty body
// parses as null and would slip past a string check.
func TestManifestHiveAPIDropsEmptyAnnotationsMap(t *testing.T) {
	blocks := ingressBlocks(t, renderManifestWildcard(t, true))
	var doc struct {
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(blocks["hive-api"]), &doc); err != nil {
		t.Fatalf("hive-api does not parse: %v\n%s", err, blocks["hive-api"])
	}
	if len(doc.Metadata.Annotations) != 0 {
		t.Errorf("hive-api still carries annotations %v", doc.Metadata.Annotations)
	}
}

// The parsed shape is the real contract: with the opt-in there must be no
// spec.tls at all, and without it there must be exactly one entry naming the
// dashboard host.
func TestManifestTLSShapeParsed(t *testing.T) {
	type ingressDoc struct {
		Spec struct {
			TLS []struct {
				Hosts      []string `yaml:"hosts"`
				SecretName string   `yaml:"secretName"`
			} `yaml:"tls"`
		} `yaml:"spec"`
	}

	for _, name := range provisionedIngresses {
		var with, without ingressDoc
		if err := yaml.Unmarshal([]byte(ingressBlocks(t, renderManifestWildcard(t, true))[name]), &with); err != nil {
			t.Fatalf("%s (wildcard) does not parse: %v", name, err)
		}
		if err := yaml.Unmarshal([]byte(ingressBlocks(t, renderManifestWildcard(t, false))[name]), &without); err != nil {
			t.Fatalf("%s (per-host) does not parse: %v", name, err)
		}
		if len(with.Spec.TLS) != 0 {
			t.Errorf("%s has %d tls entries with the opt-in, want 0", name, len(with.Spec.TLS))
		}
		if len(without.Spec.TLS) != 1 {
			t.Fatalf("%s has %d tls entries without the opt-in, want 1", name, len(without.Spec.TLS))
		}
		if without.Spec.TLS[0].SecretName != "hive-tls" {
			t.Errorf("%s per-host secret = %q, want hive-tls", name, without.Spec.TLS[0].SecretName)
		}
		if len(without.Spec.TLS[0].Hosts) != 1 || without.Spec.TLS[0].Hosts[0] != "hosted-hive-x.hive.hivecommons.dev" {
			t.Errorf("%s per-host TLS hosts = %v", name, without.Spec.TLS[0].Hosts)
		}
	}
}

// The OpenShift branch must be untouched in both modes: its Routes never used
// this path, and a stray change there would alter clusters the issue explicitly
// leaves alone.
func TestOpenShiftRoutesUnaffected(t *testing.T) {
	tmpl, err := template.New("manifests").Parse(k8sManifestTemplate)
	if err != nil {
		t.Fatalf("template parse: %v", err)
	}
	render := func(useWildcard bool) string {
		data := map[string]interface{}{
			"ID": "hosted-hive-x", "HubPublicURL": "https://hive.hivecommons.dev",
			"Namespace": "hive-hosted-hosted-hive-x", "DashboardHost": "hosted-hive-x.apps.example.com",
			"DashboardPort": 8080, "TerminalPort": 7681, "CertIssuer": "letsencrypt-dns01",
			"IngressClass": "nginx", "IsNginxIngress": false, "IsOpenShiftRoute": true,
			"UseWildcardTLS": useWildcard,
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			t.Fatalf("template exec: %v", err)
		}
		return buf.String()
	}
	if render(true) != render(false) {
		t.Error("the wildcard decision changed the OpenShift Route output; Routes use edge termination and never minted a per-host certificate here")
	}
	if !strings.Contains(render(true), "termination: edge") {
		t.Error("OpenShift Route lost its edge termination")
	}
}
