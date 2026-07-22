//go:build windows

package hostedcontrol

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func sourceFileMetadata(path string, _ fs.FileInfo) (device uint64, hasDevice bool, links uint64, hasLinks bool, err error) {
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return 0, false, 0, false, err
	}
	handle, err := windows.CreateFile(pointer, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, false, 0, false, err
	}
	defer windows.CloseHandle(handle)
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return 0, false, 0, false, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return 0, false, 0, false, errors.New("hosted runtime state entry is a link or reparse point")
	}
	return uint64(information.VolumeSerialNumber), true, uint64(information.NumberOfLinks), true, nil
}

// Windows ACLs, not os.FileMode permission bits, are authoritative. Hosted
// Actions provides a single-use runner identity and the bundle remains HMAC
// authenticated; callers may additionally install a narrower ACL.
func sourcePermissionsSafe(fs.FileInfo) bool { return true }
func pathCaseInsensitive() bool              { return true }
