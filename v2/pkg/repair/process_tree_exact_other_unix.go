//go:build !windows && !linux

package repair

import (
	"context"
	"errors"
	"os/exec"
)

func startExactRepairProcessTree(context.Context, *exec.Cmd, codexProviderFileIdentity, codexProviderFileIdentity) (func() error, error) {
	return nil, errors.New("exact specialist process-tree containment is unavailable on this Unix host")
}
