//go:build !windows

package agent

func specialistProviderPathIsReparsePoint(string) (bool, error) { return false, nil }
