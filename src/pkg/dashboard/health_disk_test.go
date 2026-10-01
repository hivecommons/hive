package dashboard

// Early warning for a filling /data volume (#9869): the data_disk health
// check must surface in both HealthSummary (what the hub alerts on) and
// /api/health/deep, graded by the 75/85/95% thresholds, and must be omitted
// — not reported as healthy — when the mount cannot be read.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stubDataDisk(t *testing.T, usedPct float64, ok bool) {
	t.Helper()
	const total = uint64(100 << 30)
	used := uint64(float64(total) * usedPct / 100)
	dataDiskUsageFn = func() (dataDiskUsage, bool) {
		return dataDiskUsage{Path: dataVolumePath, TotalBytes: total, UsedBytes: used}, ok
	}
	t.Cleanup(func() {
		dataDiskUsageFn = func() (dataDiskUsage, bool) {
			return dataDiskUsage{Path: dataVolumePath, TotalBytes: 100 << 30, UsedBytes: 10 << 30}, true
		}
	})
}

func TestDataDiskUsage_Pct(t *testing.T) {
	if got := (dataDiskUsage{}).Pct(); got != 0 {
		t.Fatalf("Pct of empty sample = %v, want 0", got)
	}
	u := dataDiskUsage{TotalBytes: 200, UsedBytes: 50}
	if got := u.Pct(); got != 25 {
		t.Fatalf("Pct = %v, want 25", got)
	}
}

func TestDataDiskHealth_Thresholds(t *testing.T) {
	cases := []struct {
		pct        float64
		wantStatus string
		wantPhrase string
	}{
		{10, "pass", "10% used"},
		{74.9, "pass", "75% used"},
		{75, "warn", "filling"},
		{84.9, "warn", "filling"},
		{85, "warn", "high"},
		{94.9, "warn", "high"},
		{95, "fail", "critically full"},
		{100, "fail", "critically full"},
	}
	for _, tc := range cases {
		total := uint64(1000 << 20)
		u := dataDiskUsage{Path: "/data", TotalBytes: total, UsedBytes: uint64(float64(total) * tc.pct / 100)}
		st, detail := dataDiskHealth(u)
		if st != tc.wantStatus {
			t.Errorf("pct=%v: status = %q, want %q (detail %q)", tc.pct, st, tc.wantStatus, detail)
		}
		if !strings.Contains(detail, tc.wantPhrase) {
			t.Errorf("pct=%v: detail = %q, want it to mention %q", tc.pct, detail, tc.wantPhrase)
		}
		if !strings.HasPrefix(detail, "/data ") {
			t.Errorf("pct=%v: detail = %q, want it to name the mount first", tc.pct, detail)
		}
	}
}

func TestHealthSummary_DataDiskPassBelowWarn(t *testing.T) {
	stubDataDisk(t, 50, true)
	deps := testDeps(t)
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	status, detail := healthCheckOf(t, s, healthCheckDataDisk)
	if status != "pass" {
		t.Fatalf("data_disk status = %q (%s), want pass", status, detail)
	}
}

func TestHealthSummary_DataDiskWarnRaisesWarningVerdict(t *testing.T) {
	stubDataDisk(t, 80, true)
	deps := testDeps(t)
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	status, detail := healthCheckOf(t, s, healthCheckDataDisk)
	if status != "warn" || !strings.Contains(detail, "80% used") {
		t.Fatalf("data_disk = %q %q, want warn naming 80%% used", status, detail)
	}
	summary := s.HealthSummary()
	if w, _ := summary["warns"].(int); w < 1 {
		t.Fatalf("warns = %v, want ≥1 once the disk check warns", summary["warns"])
	}
}

func TestHealthSummary_DataDiskCriticalFails(t *testing.T) {
	stubDataDisk(t, 97, true)
	deps := testDeps(t)
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	status, detail := healthCheckOf(t, s, healthCheckDataDisk)
	if status != "fail" || !strings.Contains(detail, "eviction") {
		t.Fatalf("data_disk = %q %q, want fail that warns of eviction", status, detail)
	}
	summary := s.HealthSummary()
	if f, _ := summary["fails"].(int); f < 1 {
		t.Fatalf("fails = %v, want ≥1 once the disk is critically full", summary["fails"])
	}
	if overall, _ := summary["status"].(string); overall == "ok" || overall == "warning" {
		t.Fatalf("overall = %q, want degraded or critical when /data is at 97%%", overall)
	}
}

func TestHealthSummary_DataDiskOmittedWhenUnreadable(t *testing.T) {
	stubDataDisk(t, 0, false)
	deps := testDeps(t)
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	for _, c := range healthChecksOf(t, s) {
		if c.Name == healthCheckDataDisk {
			t.Fatalf("data_disk reported %q when the mount is unreadable; want the check omitted", c.Status)
		}
	}
}

func TestHandleHealthDeep_DataDiskCheck(t *testing.T) {
	stubDataDisk(t, 90, true)
	srv := newFullServer(t)
	srv.MarkReady()
	srv.UpdateStatus(minimalPayload())

	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, httptest.NewRequest(http.MethodGet, "/api/health/deep", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var result struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Status     string  `json:"status"`
			Detail     string  `json:"detail"`
			Path       string  `json:"path"`
			UsedPct    float64 `json:"used_pct"`
			TotalBytes uint64  `json:"total_bytes"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	disk, ok := result.Checks[healthCheckDataDisk]
	if !ok {
		t.Fatalf("no data_disk check in /api/health/deep: %+v", result.Checks)
	}
	if disk.Status != "warn" || disk.Path != dataVolumePath || disk.UsedPct != 90 || disk.TotalBytes != 100<<30 {
		t.Fatalf("data_disk = %+v, want warn on /data at 90%% of 100 GiB", disk)
	}
	if result.Status == "ok" {
		t.Fatalf("overall = %q, want degraded while /data is at 90%%", result.Status)
	}
}

func TestHandleHealthDeep_DataDiskOmittedWhenUnreadable(t *testing.T) {
	stubDataDisk(t, 0, false)
	srv := newFullServer(t)
	srv.MarkReady()
	srv.UpdateStatus(minimalPayload())

	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, httptest.NewRequest(http.MethodGet, "/api/health/deep", nil))
	var result struct {
		Checks map[string]json.RawMessage `json:"checks"`
	}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := result.Checks[healthCheckDataDisk]; present {
		t.Fatalf("data_disk present when the mount is unreadable; want omitted")
	}
}

// sampleDataDiskUsage reads the real host: it must either report a coherent
// sample (used ≤ total, a known path) or decline, never a nonsense one.
func TestSampleDataDiskUsage_Coherent(t *testing.T) {
	u, ok := sampleDataDiskUsage()
	if !ok {
		t.Skip("no readable /data (or /) on this host")
	}
	if u.Path != dataVolumePath && u.Path != rootFSPath {
		t.Fatalf("path = %q, want %q or %q", u.Path, dataVolumePath, rootFSPath)
	}
	if u.TotalBytes == 0 || u.UsedBytes > u.TotalBytes {
		t.Fatalf("incoherent sample: %+v", u)
	}
	if pct := u.Pct(); pct < 0 || pct > 100 {
		t.Fatalf("pct = %v, want within [0,100]", pct)
	}
}
