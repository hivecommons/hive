//go:build !unix

package spoke

// FDSoftLimit has no meaningful value off unix (dev/Windows builds); return 0,
// which readers already treat as UNKNOWN. The production target is Linux.
func FDSoftLimit() uint64 { return 0 }
