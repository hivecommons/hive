//go:build windows

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func waitForInstallerExecutableRelease(executable string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stableChecks := 0
	for {
		blockedPath := ""
		var openErr error
		for _, probe := range []struct {
			path       string
			attributes uint32
		}{
			{path: executable, attributes: windows.FILE_ATTRIBUTE_NORMAL},
			{path: filepath.Dir(executable), attributes: windows.FILE_FLAG_BACKUP_SEMANTICS},
		} {
			openErr = probeInstallerDeleteShare(probe.path, probe.attributes)
			if openErr != nil {
				blockedPath = probe.path
				break
			}
		}
		if openErr == nil {
			stableChecks++
			if stableChecks >= 2 {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		stableChecks = 0
		if !errors.Is(openErr, windows.ERROR_SHARING_VIOLATION) && !errors.Is(openErr, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("probe scheduler path release for %s: %w", blockedPath, openErr)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("scheduler path %s remained in use after %s: %w", blockedPath, timeout, openErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func probeInstallerDeleteShare(path string, attributes uint32) error {
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		attributes,
		0,
	)
	if err != nil {
		return err
	}
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("close scheduler release probe: %w", err)
	}
	return nil
}
