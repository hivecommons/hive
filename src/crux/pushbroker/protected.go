package pushbroker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// guardChangedFiles records the outgoing file list on res and refuses an
// empty push or one that touches a protected path.
func (b *Broker) guardChangedFiles(ctx context.Context, res *Result) error {
	files, err := b.changedFiles(ctx)
	if err != nil {
		return err
	}
	res.ChangedFiles = files
	if len(files) == 0 {
		return errors.New("pushbroker: no committed changes to push")
	}
	if rejected := ProtectedPathViolations(files, b.protectedPaths()); len(rejected) > 0 {
		res.ProtectedReject = rejected
		return fmt.Errorf("pushbroker: protected paths changed: %s", strings.Join(rejected, ", "))
	}
	return nil
}

func (b *Broker) protectedPaths() []string {
	if len(b.ProtectedPaths) > 0 {
		return b.ProtectedPaths
	}
	return DefaultProtectedPaths
}

func ProtectedPathViolations(files, protected []string) []string {
	var rejected []string
	for _, file := range files {
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(file)), "./")
		for _, guard := range protected {
			g := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(guard)), "./")
			if strings.HasSuffix(guard, "/") || strings.HasSuffix(g, "/") {
				g = strings.TrimSuffix(g, "/") + "/"
				if strings.HasPrefix(clean, g) {
					rejected = append(rejected, clean)
					break
				}
				continue
			}
			if clean == g || strings.HasPrefix(clean, strings.TrimSuffix(g, "/")+"/") && strings.HasSuffix(guard, "/") {
				rejected = append(rejected, clean)
				break
			}
		}
	}
	return rejected
}
