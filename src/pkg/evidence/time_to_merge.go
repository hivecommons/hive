package evidence

import (
	"slices"
	"time"
)

// TimeToMerge is how long the PR was open before it merged (merge.at minus
// merge.opened_at). ok is false when the bundle has no merge, the merge
// predates opened_at capture, or the timestamps are out of order.
func (b *Bundle) TimeToMerge() (d time.Duration, ok bool) {
	if b == nil || b.Merge == nil || b.Merge.OpenedAt.IsZero() || b.Merge.At.IsZero() {
		return 0, false
	}
	d = b.Merge.At.Sub(b.Merge.OpenedAt)
	if d < 0 {
		return 0, false
	}
	return d, true
}

// MedianTimeToMerge is the median TimeToMerge of the bundles that have one,
// and how many did. Bundles without a measurable time to merge are skipped,
// never counted as zero; n is 0 (and the median 0) when none qualify. With an
// even count the median is the mean of the two middle values.
func MedianTimeToMerge(bundles []*Bundle) (median time.Duration, n int) {
	var ds []time.Duration
	for _, b := range bundles {
		if d, ok := b.TimeToMerge(); ok {
			ds = append(ds, d)
		}
	}
	if len(ds) == 0 {
		return 0, 0
	}
	slices.Sort(ds)
	mid := len(ds) / 2
	if len(ds)%2 == 1 {
		return ds[mid], len(ds)
	}
	return ds[mid-1] + (ds[mid]-ds[mid-1])/2, len(ds)
}
