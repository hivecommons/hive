//go:build !windows

package main

import "time"

func waitForInstallerExecutableRelease(_ string, _ time.Duration) error {
	return nil
}
