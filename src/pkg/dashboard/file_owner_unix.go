//go:build !windows

package dashboard

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

func fileOwnerName(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("not a unix stat")
	}
	owner, err := user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10))
	if err != nil {
		return "", err
	}
	return owner.Username, nil
}
