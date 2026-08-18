//go:build windows

package normalservice

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func durableReplaceLedger(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("durably replace normal Visual Hive ledger: %w", err)
	}
	return nil
}
