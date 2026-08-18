//go:build windows

package dashboard

import "golang.org/x/sys/windows"

func filesystemUsage(path string) (totalBytes, freeBytes uint64, err error) {
	if path == "/" || path == "/data" {
		path = `C:\`
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var callerFree, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &callerFree, &total, &totalFree); err != nil {
		return 0, 0, err
	}
	return total, callerFree, nil
}
