//go:build !windows && !linux

package main

import (
	"context"
	"errors"
	"runtime"
)

type unsupportedDaemonServiceManager struct{}

func newPlatformDaemonServiceManager() daemonServiceManager { return unsupportedDaemonServiceManager{} }

func platformDaemonServiceName(stateDir string) string {
	return "hive-integrated-" + stableDaemonServiceSuffix(stateDir)
}

func unsupportedDaemonServiceError() error {
	return errors.New("persistent Hive scheduler services are not supported on " + runtime.GOOS + "; use Windows or Linux")
}

func (unsupportedDaemonServiceManager) Install(context.Context, daemonServiceSpec) error {
	return unsupportedDaemonServiceError()
}

func (unsupportedDaemonServiceManager) Start(context.Context, daemonServiceSpec) error {
	return unsupportedDaemonServiceError()
}

func (unsupportedDaemonServiceManager) Inspect(context.Context, daemonServiceSpec) (daemonServiceStatus, error) {
	return daemonServiceStatus{SchemaVersion: daemonServiceSchema, Platform: runtime.GOOS}, unsupportedDaemonServiceError()
}

func (unsupportedDaemonServiceManager) Uninstall(context.Context, daemonServiceSpec) error {
	return nil
}
