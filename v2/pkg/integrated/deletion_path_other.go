//go:build !windows

package integrated

func deletionPathIsReparsePoint(string) bool { return false }
