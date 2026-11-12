//go:build !linux

package egressmark

// setSockMark is a no-op off Linux: SO_MARK is Linux-only, but the package must
// still compile on the CI matrix and developer machines.
func setSockMark(fd uintptr, mark int) error {
	return nil
}
