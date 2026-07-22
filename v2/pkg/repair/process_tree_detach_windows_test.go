//go:build windows

package repair

import "fmt"

func detachRepairTestSession() error {
	return fmt.Errorf("setsid is unavailable on Windows")
}
