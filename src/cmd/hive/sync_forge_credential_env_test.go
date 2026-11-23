package main

import (
	"slices"
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/config"
)

func TestSyncForgeCredentialEnvKeepsRenamedTokenEnv(t *testing.T) {
	cfg := &config.Config{}
	cfg.GitLab.TokenEnv = "RENAME_TEST_GITLAB_OLD"
	cfg.Gitea.TokenEnv = "RENAME_TEST_GITEA_OLD"
	syncForgeCredentialEnv(cfg)

	cfg.GitLab.TokenEnv = "RENAME_TEST_GITLAB_NEW"
	cfg.Gitea.TokenEnv = "RENAME_TEST_GITEA_NEW"
	syncForgeCredentialEnv(cfg)

	got := cruxconfig.ExtraCredentialEnvVars()
	for _, name := range []string{"RENAME_TEST_GITLAB_OLD", "RENAME_TEST_GITLAB_NEW", "RENAME_TEST_GITEA_OLD", "RENAME_TEST_GITEA_NEW"} {
		if !slices.Contains(got, name) {
			t.Errorf("registry = %v, want it to include %s after a reload renamed the token env", got, name)
		}
	}
}
