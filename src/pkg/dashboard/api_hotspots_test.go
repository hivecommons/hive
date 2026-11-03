package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type fakeHotspotSource struct {
	conflicts map[int][]string
}

func (f *fakeHotspotSource) HotspotsJSON(repo string) any {
	if repo != "" && repo != "o/r" {
		return []any{}
	}
	return []map[string]any{{"repo": "o/r", "file": "pkg/dashboard/api.go", "prs": 8, "overlaps": 11, "recommendation": "split-code", "issue_number": 42}}
}

func (f *fakeHotspotSource) HotspotCounts() (int, int, int) { return 3, 1, 1 }

func (f *fakeHotspotSource) ObserveConflict(_ string, number int, files []string) {
	if f.conflicts == nil {
		f.conflicts = map[int][]string{}
	}
	f.conflicts[number] = files
}

func TestHotspotsEndpointAndStatusBlock(t *testing.T) {
	s := covApiServer(t)
	// Unwired: an empty list, never a 500, and no status block.
	rec := doGet(s, "/api/hotspots")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hotspots":[]`) {
		t.Fatalf("unwired = %d %s", rec.Code, rec.Body.String())
	}
	if s.HotspotStatusBlock() != nil {
		t.Fatal("status block must be nil when the advisor is not wired")
	}

	s.SetHotspotSource(&fakeHotspotSource{})
	rec = doGet(s, "/api/hotspots?repo=o/r")
	var resp struct {
		Repo       string           `json:"repo"`
		Count      int              `json:"count"`
		Actionable int              `json:"actionable"`
		Filed      int              `json:"filed"`
		Hotspots   []map[string]any `json:"hotspots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("hotspots = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if resp.Repo != "o/r" || resp.Count != 3 || resp.Actionable != 1 || resp.Filed != 1 || len(resp.Hotspots) != 1 || resp.Hotspots[0]["file"] != "pkg/dashboard/api.go" {
		t.Fatalf("payload = %+v", resp)
	}
	if rec := doGet(s, "/api/hotspots?repo=x/y"); !strings.Contains(rec.Body.String(), `"hotspots":[]`) {
		t.Fatalf("other repo = %s", rec.Body.String())
	}
	if b := s.HotspotStatusBlock(); b == nil || b.Count != 3 || b.Actionable != 1 || b.Filed != 1 {
		t.Fatalf("status block = %+v", b)
	}
}
