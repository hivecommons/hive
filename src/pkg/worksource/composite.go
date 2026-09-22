package worksource

import (
	"context"
	"fmt"
)

// Composite appends additive work sources, preserving the primary source's
// byte-for-byte output when no extras are configured.
type Composite struct {
	primary WorkSource
	extras  []WorkSource
}

func NewComposite(primary WorkSource, extras ...WorkSource) *Composite {
	return &Composite{primary: primary, extras: extras}
}

func (c *Composite) SourceType() string {
	if c == nil || c.primary == nil {
		return "composite"
	}
	return c.primary.SourceType()
}

func (c *Composite) ListIssues(ctx context.Context) ([]Issue, error) {
	if c == nil || c.primary == nil {
		return []Issue{}, nil
	}
	out, err := c.primary.ListIssues(ctx)
	if err != nil {
		return nil, err
	}
	for _, extra := range c.extras {
		if extra == nil {
			continue
		}
		items, err := extra.ListIssues(ctx)
		if err != nil {
			return nil, fmt.Errorf("worksource/%s: %w", extra.SourceType(), err)
		}
		out = append(out, items...)
	}
	return out, nil
}
