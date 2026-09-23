//go:build !extwork_omp

package main

import (
	"context"

	"github.com/hivecommons/hive/pkg/dashboard"
)

func attachExternalOMPPeer(context.Context, dashboard.ExternalExecutionPeer) error {
	return nil
}
