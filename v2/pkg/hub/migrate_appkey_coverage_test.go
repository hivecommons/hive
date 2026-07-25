package hub

import (
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// ============================================================
// saas_provision.go — migrateHive GitHub-App-credential passthrough
//
// The source-cluster secret read returns a base64 PEM app key and a config map
// with app_id/installation_id, so migrateHive's app-auth branch runs.
// ============================================================

func installMigrateKubectl(t *testing.T, pem string) {
	t.Helper()
	dir := t.TempDir()
	b64 := base64.StdEncoding.EncodeToString([]byte(pem))
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *github-token*) echo '' ;;\n" +
		"  *gh-app-key*) printf '%s' '" + b64 + "' ;;\n" +
		"  *hive-config*|*hive.yaml*) printf 'app_id: 12345\\ninstallation_id: 67890\\n' ;;\n" +
		"  *) echo '' ;;\n" +
		"esac\n" +
		"exit 0\n"
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestMigrateHiveWithAppCreds(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----"
	installMigrateKubectl(t, pem)

	s := &HubServer{
		logger: slog.Default(),
		saveCh: make(chan struct{}, 1),
		clusters: map[string]ClusterConfig{
			"hive-oke": *dynamicCluster(),
			"target":   {ID: "target", Name: "Target", InCluster: true, StorageType: storageTypeDynamic, Domain: "t.example.com"},
		},
	}
	h := &SaaSHive{ID: "hosted-appmig", Owner: "alice", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 2, ClusterID: "hive-oke"}
	saveSaaSHive(h)
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"hosted-appmig": "owner"}})
	s.registry.Hives = []RegistryEntry{{ID: "hosted-appmig", ClusterID: "hive-oke", IsPublic: true}}

	from := s.clusters["hive-oke"]
	to := s.clusters["target"]
	s.migrateHive(h, &from, &to)

	got := loadSaaSHive("hosted-appmig")
	if got == nil || got.ClusterID != "target" {
		t.Errorf("migration with app creds did not complete: %+v", got)
	}
}
