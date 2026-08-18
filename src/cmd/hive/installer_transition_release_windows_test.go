//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWaitForInstallerExecutableReleaseWaitsForDeleteSharing(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "hive.exe")
	if err := os.WriteFile(executable, []byte("release probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForInstallerExecutableRelease(executable, 150*time.Millisecond); err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("release probe accepted an executable whose open handle denied delete sharing")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	directoryPath, err := windows.UTF16PtrFromString(filepath.Dir(executable))
	if err != nil {
		t.Fatal(err)
	}
	directoryHandle, err := windows.CreateFile(
		directoryPath,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForInstallerExecutableRelease(executable, 150*time.Millisecond); err == nil {
		_ = windows.CloseHandle(directoryHandle)
		t.Fatal("release probe accepted an installation directory whose open handle denied delete sharing")
	}
	if err := windows.CloseHandle(directoryHandle); err != nil {
		t.Fatal(err)
	}
	if err := waitForInstallerExecutableRelease(executable, time.Second); err != nil {
		t.Fatalf("release probe rejected an executable after its blocking handle closed: %v", err)
	}
}
