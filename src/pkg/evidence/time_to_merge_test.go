package evidence

import (
	"testing"
	"time"
)

func mergedAfter(opened time.Duration) *Bundle {
	b := sample()
	b.Merge.OpenedAt = b.Merge.At.Add(-opened)
	return b
}

func TestTimeToMerge(t *testing.T) {
	tests := []struct {
		name   string
		bundle *Bundle
		want   time.Duration
		wantOK bool
	}{
		{"nil bundle", nil, 0, false},
		{"not merged", func() *Bundle { b := sample(); b.Merge = nil; return b }(), 0, false},
		{"opened_at not captured", sample(), 0, false},
		{"merge at missing", func() *Bundle { b := mergedAfter(time.Hour); b.Merge.At = time.Time{}; return b }(), 0, false},
		{"opened after merge", func() *Bundle { b := sample(); b.Merge.OpenedAt = t0.Add(time.Minute); return b }(), 0, false},
		{"opened and merged together", mergedAfter(0), 0, true},
		{"open a day", mergedAfter(24 * time.Hour), 24 * time.Hour, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.bundle.TimeToMerge()
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("TimeToMerge() = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestMedianTimeToMerge(t *testing.T) {
	unmeasured := sample()
	unmerged := sample()
	unmerged.Merge = nil
	tests := []struct {
		name    string
		bundles []*Bundle
		want    time.Duration
		wantN   int
	}{
		{"empty", nil, 0, 0},
		{"none measurable", []*Bundle{nil, unmeasured, unmerged}, 0, 0},
		{"single", []*Bundle{mergedAfter(3 * time.Hour)}, 3 * time.Hour, 1},
		{"odd count is the middle value", []*Bundle{
			mergedAfter(10 * time.Hour), mergedAfter(time.Hour), mergedAfter(48 * time.Hour),
		}, 10 * time.Hour, 3},
		{"even count is the mean of the middle pair", []*Bundle{
			mergedAfter(4 * time.Hour), mergedAfter(time.Hour), mergedAfter(2 * time.Hour), mergedAfter(100 * time.Hour),
		}, 3 * time.Hour, 4},
		{"unmeasurable bundles are skipped, not zero", []*Bundle{
			unmeasured, mergedAfter(5 * time.Hour), unmerged, mergedAfter(7 * time.Hour), nil,
		}, 6 * time.Hour, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, n := MedianTimeToMerge(tc.bundles)
			if got != tc.want || n != tc.wantN {
				t.Fatalf("MedianTimeToMerge() = %v, %d; want %v, %d", got, n, tc.want, tc.wantN)
			}
		})
	}
}
