package cline

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	ProviderSecretEnv      = "HIVE_CLINE_PROVIDERS_B64"
	maxProviderSecretBytes = 1 << 20
)

func selectedProviderSecret(provider string) ([]byte, error) {
	encoded := os.Getenv(ProviderSecretEnv)
	_ = os.Unsetenv(ProviderSecretEnv)
	if encoded == "" {
		return nil, errors.New("Cline sandbox Job requires HIVE_CLINE_PROVIDERS_B64 in an operator-selected job.env_from_secrets Secret; no credential file or ambient provider fallback is used")
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > maxProviderSecretBytes {
		return nil, errors.New("Cline provider Secret exceeds the supported size")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("Cline provider Secret must contain base64-encoded providers.json")
	}
	return narrowCredentials(raw, provider)
}

func secretStateOutsideWorkspace(stateDir, workspace string) error {
	state, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return errors.New("cannot resolve Cline private state directory")
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return errors.New("cannot resolve Cline workspace")
	}
	rel, err := filepath.Rel(root, state)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("Cline provider Secret state must be outside the Git workspace, including through symlinks")
	}
	return nil
}
