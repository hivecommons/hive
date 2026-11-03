package pushbroker

import (
	"context"
	"errors"
	"fmt"
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
