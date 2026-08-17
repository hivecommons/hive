package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/reach"
)

func TestHandleReachDashboard(t *testing.T) {
	s := newTestServer()
	now := time.Now()

	s.deps = &Dependencies{
		ReachReportsFunc: func() []reach.PRReachReport {
			delta := -0.05
			return []reach.PRReachReport{
				{
					PR:             3994,
					Title:          "feat(reach): PR mapping",
					MergeCommit:    "abc1234",
					MergedAt:       now.Add(-2 * time.Hour),
					DeployWindow:   "def5678",
					Deployed:       true,
					ReachCount:     3,
					ReachHives:     []string{"hive-1", "hive-2", "hive-3"},
					ErrorRateDelta: &delta,
					Attribution: reach.Attribution{
						Components: []string{"governor"},
						Coverage:   1.0,
					},
				},
			}
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/reach", nil)
	w := httptest.NewRecorder()
	s.handleReach(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp reachDashboardResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(resp.Reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(resp.Reports))
	}
	if resp.Reports[0].PR != 3994 {
		t.Errorf("PR = %d, want 3994", resp.Reports[0].PR)
	}
	if resp.Reports[0].ReachCount != 3 {
		t.Errorf("ReachCount = %d, want 3", resp.Reports[0].ReachCount)
	}
	if resp.Reports[0].ErrorRateDelta == nil || *resp.Reports[0].ErrorRateDelta != -0.05 {
		t.Errorf("ErrorRateDelta = %v, want -0.05", resp.Reports[0].ErrorRateDelta)
	}
}

func TestBuildACMMStatusInputs_PRReachRate(t *testing.T) {
	cases := []struct {
		name string
		fn   func() []reach.PRReachReport
		want float64
	}{
		{
			name: "measured reach rate flows through",
			fn: func() []reach.PRReachReport {
				return []reach.PRReachReport{
					{PR: 1, Deployed: true, Attribution: reach.Attribution{Components: []string{"governor"}}, ReachCount: 2},
					{PR: 2, Deployed: true, Attribution: reach.Attribution{Components: []string{"agent"}}, ReachCount: 0},
					{PR: 3, Deployed: true, Attribution: reach.Attribution{Components: []string{"main"}}, ReachCount: 1},
					{PR: 4, Deployed: true, Attribution: reach.Attribution{Components: []string{"proxy"}}, ReachCount: 1},
				}
			},
			want: 0.75, // 3 reached out of 4 deployed
		},
		{
			name: "nil reach function stays zero",
			fn:   nil,
			want: 0.0,
		},
		{
			name: "empty reports stay zero",
			fn: func() []reach.PRReachReport {
				return []reach.PRReachReport{}
			},
			want: 0.0,
		},
		{
			name: "all reached reads as measured 1.0",
			fn: func() []reach.PRReachReport {
				return []reach.PRReachReport{
					{PR: 1, Deployed: true, Attribution: reach.Attribution{Components: []string{"governor"}}, ReachCount: 2},
				}
			},
			want: 1.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			s.deps = &Dependencies{ReachReportsFunc: tc.fn}
			in := s.buildACMMStatusInputs()
			if in.PRReachRate != tc.want {
				t.Fatalf("PRReachRate = %v, want %v", in.PRReachRate, tc.want)
			}
		})
	}
}
