//go:build windows

package dashboard

import (
	"os"
	"os/user"
)

func fileOwnerName(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	owner, err := user.Current()
	if err != nil {
		return "", err
	}
	return owner.Username, nil
}
