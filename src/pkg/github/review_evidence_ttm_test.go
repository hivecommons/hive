package github

// Time to merge (RFC #12062): the merge recorder stamps merge.opened_at from
// the PR's GitHub creation time so merge.at - merge.opened_at is measurable
// per bundle. The bundle's own created_at is when review started and is not
// a substitute.

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

func TestRecordReviewEvidenceMergeStampsOpenedAt(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	opened := time.Date(2026, 9, 29, 6, 30, 0, 0, time.UTC)
	tests := []struct {
		name   string
		opened string
		want   time.Time
	}{
		{"created_at recorded as opened_at", opened.Format(time.RFC3339), opened},
		{"missing created_at leaves opened_at unset", "", time.Time{}},
		{"created_at after the merge is dropped", "2999-01-01T00:00:00Z", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withEvidenceRoot(t)
			writeTestEvidence(t, root, "h1", "", t0)
			f := &evidenceContextFixture{head: "h1", opened: tt.opened}
			c := newTestClient(t, newEvidenceContextServer(t, f), "o", []string{"o/r"})
			c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })

			c.RecordPRMergedAudit("o/r", 7, "squash", "m1", PRAuditPathSweep)

			b := mustLoadBundle(t, root, "h1")
			if b.Merge == nil {
				t.Fatal("merge not recorded")
			}
			if !b.Merge.OpenedAt.Equal(tt.want) {
				t.Fatalf("merge.opened_at = %v, want %v", b.Merge.OpenedAt, tt.want)
			}
			d, ok := b.TimeToMerge()
			if ok != !tt.want.IsZero() {
				t.Fatalf("TimeToMerge ok = %v for opened_at %v", ok, b.Merge.OpenedAt)
			}
			if ok && d != b.Merge.At.Sub(opened) {
				t.Fatalf("TimeToMerge = %v, want %v", d, b.Merge.At.Sub(opened))
			}
			if err := b.Validate(); err != nil {
				t.Fatalf("bundle invalid: %v", err)
			}
			if h, _ := evidence.Hash(b); h != b.Hash {
				t.Fatalf("hash not resealed: %s vs %s", h, b.Hash)
			}
		})
	}
}
