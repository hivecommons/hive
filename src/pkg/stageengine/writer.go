package stageengine

import (
	"context"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageWriter is the ONLY code that adds or removes a stage/* label.
// Implementations wrap stagelabels.Enforce (fresh read, removes before adds,
// kind clamp). The admission test allows exactly one implementation.
type StageWriter interface {
	Apply(ctx context.Context, ref Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error)
}
