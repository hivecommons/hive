//go:build windows

package hostedstate

import (
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func platformFileMetadata(path string, _ fs.FileInfo) (device uint64, hasDevice bool, links uint64, hasLinks bool, err error) {
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return 0, false, 0, false, err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return 0, false, 0, false, err
	}
	defer windows.CloseHandle(handle)
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return 0, false, 0, false, err
	}
	return uint64(information.VolumeSerialNumber), true, uint64(information.NumberOfLinks), true, nil
}

// Windows ACLs, not os.FileMode permission bits, are authoritative. The
// package still creates destination content with the narrowest portable mode;
// callers may additionally apply an ACL appropriate to their runner identity.
func permissionSafetyApplicable() bool { return false }
