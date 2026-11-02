package sandbox

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hivecommons/hive/crux/cline"
)

const clineContainerCleanupTimeout = 10 * time.Second

func clineProviderArgs(spec LaunchSpec, mount, workdir string) ([]string, error) {
	if mount != DefaultWorkspaceMount || workdir != DefaultWorkspaceMount {
		return nil, errors.New("sandbox: Cline provider capability requires the canonical workspace mount")
	}
	file, err := spec.ClineProvider.MountFile(spec.Workspace)
	if err != nil {
		return nil, err
	}
	if err := validateClineProviderCommand(spec.Command, spec.ClineProvider.Model()); err != nil {
		return nil, err
	}
	// Relabel only this private per-run copy, never the operator's auth source.
	return []string{"-v", file + ":" + cline.ProviderMountPath + ":ro,Z",
		"--tmpfs", fmt.Sprintf("%s:rw,nosuid,nodev,noexec,mode=0700,uid=%d,gid=%d", cline.ProviderStatePath, os.Getuid(), os.Getgid()),
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--env", "HOME=" + cline.ProviderStatePath}, nil
}

func validateClineProviderCommand(command []string, model string) error {
	fail := errors.New("sandbox: selected Cline provider may only be mounted for the canonical bounded cline-turn command")
	if len(command) < 2 || command[0] != "crux" || command[1] != cline.Subcommand {
		return fail
	}
	seen := map[string]string{}
	for i := 2; i < len(command); i += 2 {
		if i+1 == len(command) {
			return fail
		}
		key, value := command[i], command[i+1]
		if _, duplicate := seen[key]; duplicate {
			return fail
		}
		switch key {
		case "--provider-file":
			if value != cline.ProviderMountPath {
				return fail
			}
		case "--state-dir":
			if value != cline.ProviderStatePath {
				return fail
			}
		case "--model":
			if value != model {
				return fail
			}
		case "--prompt-file":
			if value != ".hive/kick-prompt.txt" {
				return fail
			}
		case "--thinking":
			switch value {
			case "none", "low", "medium", "high", "xhigh":
			default:
				return fail
			}
		case "--timeout":
			d, err := time.ParseDuration(value)
			if err != nil || d <= 0 {
				return fail
			}
		default:
			return fail
		}
		seen[key] = value
	}
	for _, required := range []string{"--provider-file", "--state-dir", "--model", "--prompt-file"} {
		if _, ok := seen[required]; !ok {
			return fail
		}
	}
	return nil
}
