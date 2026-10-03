//go:build !linux

package dashboard

// Inheritable/ambient capabilities are a Linux concept; elsewhere the stage
// command runs unchanged.
func spekHubDropCapsCommand(cmd []string) ([]string, error) { return cmd, nil }
