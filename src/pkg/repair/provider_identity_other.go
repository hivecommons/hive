//go:build !windows

package repair

func codexProviderPathIsReparsePoint(string) (bool, error) { return false, nil }
