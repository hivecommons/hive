package main

import (
	"errors"
	"os"

	"github.com/kubestellar/hive/v2/pkg/agent"
)

type normalVisualRuntimeConfigurer func(*agent.Manager) (bool, error)

// claimNormalVisualWorkOwnership gives the normal Hive runtime exclusive
// ownership before its existing ordinary Manager is configured for Visual Hive
// work. A legacy scheduler holding the same daemon lease makes this fail closed;
// configuration failures and non-repair modes release ownership immediately.
func claimNormalVisualWorkOwnership(stateDir string, ordinaryManager *agent.Manager, configure normalVisualRuntimeConfigurer) (*os.File, error) {
	if ordinaryManager == nil || configure == nil {
		return nil, errors.New("normal Visual Hive runtime requires the existing ordinary Manager and a configurer")
	}
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		return nil, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			releaseDaemonLease(lease)
		}
	}()
	configured, err := configure(ordinaryManager)
	if err != nil {
		return nil, err
	}
	if !configured {
		return nil, nil
	}
	keepLease = true
	return lease, nil
}
