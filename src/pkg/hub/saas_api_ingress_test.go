package hub

import (
	"strings"
	"testing"
)

func TestHostedNginxIngressBypassesBrowserAuthOnlyForAPIV1(t *testing.T) {
	apiStart := strings.Index(k8sManifestTemplate, "name: hive-api")
	if apiStart < 0 {
		t.Fatal("hosted manifest is missing a dedicated hive-api ingress")
	}
	apiEnd := strings.Index(k8sManifestTemplate[apiStart:], "\n---")
	if apiEnd < 0 {
		t.Fatal("hive-api ingress is not a bounded manifest document")
	}
	apiIngress := k8sManifestTemplate[apiStart : apiStart+apiEnd]
	if !strings.Contains(apiIngress, "path: /api/v1") {
		t.Fatal("hive-api ingress must expose only the versioned GitHub-token API prefix")
	}
	if strings.Contains(apiIngress, "auth-url") || strings.Contains(apiIngress, "auth-signin") {
		t.Fatal("hive-api ingress must not apply browser session authentication")
	}

	dashboardStart := strings.Index(k8sManifestTemplate, "name: hive\n")
	if dashboardStart < 0 || dashboardStart >= apiStart {
		t.Fatal("hosted manifest is missing the authenticated dashboard ingress")
	}
	dashboardIngress := k8sManifestTemplate[dashboardStart:apiStart]
	if !strings.Contains(dashboardIngress, "auth-url") || !strings.Contains(dashboardIngress, "auth-signin") {
		t.Fatal("dashboard ingress must retain browser session authentication")
	}
}
