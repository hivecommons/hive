package github

import (
	"crypto/ed25519"
	"fmt"

	"github.com/hivecommons/hive/pkg/evidence"
)

// MergeReviewEvidenceShadow folds the non-nil gates of obs into the on-disk
// bundle at path, replacing an earlier observation of the same gate, then
// re-validates, re-seals (and re-signs when key is set) and rewrites it. It
// only records; callers must not use the result to gate a merge.
func MergeReviewEvidenceShadow(path string, obs evidence.Shadow, key ed25519.PrivateKey) error {
	if obs.ReviewGate == nil && obs.Intent == nil {
		return nil
	}
	reviewEvidenceMu.Lock()
	defer reviewEvidenceMu.Unlock()
	b, err := LoadReviewEvidence(path)
	if err != nil {
		return err
	}
	if b.Shadow == nil {
		b.Shadow = &evidence.Shadow{}
	}
	for _, g := range []struct {
		dst **evidence.ShadowGate
		src *evidence.ShadowGate
	}{
		{&b.Shadow.ReviewGate, obs.ReviewGate},
		{&b.Shadow.Intent, obs.Intent},
	} {
		if g.src == nil {
			continue
		}
		c := *g.src
		*g.dst = &c
		if c.EvaluatedAt.After(b.UpdatedAt) {
			b.UpdatedAt = c.EvaluatedAt
		}
	}
	if err := b.Validate(); err != nil {
		return fmt.Errorf("shadow observation invalid: %w", err)
	}
	if err := evidence.Seal(b, key); err != nil {
		return err
	}
	return writeReviewEvidence(path, b)
}
