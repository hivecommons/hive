//go:build !linux

package posture

// Check cannot inspect containment off Linux and reports no gaps.
func Check() []string { return nil }
