package hub

import (
	"net/url"
	"testing"
)

func mhq(t *testing.T, raw string) myHivesQuery {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parseMyHivesQuery(v)
}

func testMyHivesEntries() []MyHiveEntry {
	mk := func(id, org, owner, cluster, prov, beat string, online bool) MyHiveEntry {
		e := MyHiveEntry{ProvStatus: prov}
		e.ID = id
		e.Org = org
		e.Owner = owner
		e.ClusterID = cluster
		e.Online = online
		e.LastHeartbeat = beat
		return e
	}
	return []MyHiveEntry{
		func() MyHiveEntry {
			e := mk("hosted-alpha", "acme", "alice", "vllm-d", "assigned", "2026-08-20T10:00:00Z", true)
			e.ClusterName = "VLLM Dallas"
			e.Repos = []string{"alpha-api", "alpha-ui"}
			return e
		}(),
		func() MyHiveEntry {
			e := mk("hosted-beta", "acme", "bob", "hive-oke", statusAvailable, "", false)
			e.ClusterName = "OKE East"
			e.Repos = []string{"beta-worker"}
			return e
		}(),
		mk("hosted-gamma", "zorg", "alice", "vllm-d", "provisioning", "2026-08-20T12:00:00Z", false),
		mk("hosted-delta", "zorg", "carol", "vllm-d", "assigned", "2026-08-20T11:00:00Z", true),
	}
}

func TestMyHivesQueryInactiveByDefault(t *testing.T) {
	if mhq(t, "").active() {
		t.Error("empty query must be inactive (full-set back-compat)")
	}
	if !mhq(t, "q=alpha").active() || !mhq(t, "per_page=10").active() {
		t.Error("q / per_page must activate scoping")
	}
}

func TestApplyMyHivesQueryFilters(t *testing.T) {
	entries := testMyHivesEntries()

	view, matched := applyMyHivesQuery(entries, mhq(t, "q=ALPHA"))
	if matched != 1 || len(view) != 1 || view[0].ID != "hosted-alpha" {
		t.Errorf("q=ALPHA: got %d matched, view %v", matched, view)
	}

	_, matched = applyMyHivesQuery(entries, mhq(t, "status=online"))
	if matched != 2 {
		t.Errorf("status=online matched %d, want 2", matched)
	}
	_, matched = applyMyHivesQuery(entries, mhq(t, "status=available"))
	if matched != 1 {
		t.Errorf("status=available matched %d, want 1", matched)
	}
	_, matched = applyMyHivesQuery(entries, mhq(t, "cluster=hive-oke"))
	if matched != 1 {
		t.Errorf("cluster=hive-oke matched %d, want 1", matched)
	}
	_, matched = applyMyHivesQuery(entries, mhq(t, "q=OKE East"))
	if matched != 1 {
		t.Errorf("q=OKE East matched %d, want 1", matched)
	}
	_, matched = applyMyHivesQuery(entries, mhq(t, "q=alpha-ui"))
	if matched != 1 {
		t.Errorf("q=alpha-ui matched %d, want 1", matched)
	}
	_, matched = applyMyHivesQuery(entries, mhq(t, "q=alice&cluster=vllm-d&status=online"))
	if matched != 1 {
		t.Errorf("combined filter matched %d, want 1", matched)
	}
}

func TestApplyMyHivesQuerySortAndPage(t *testing.T) {
	entries := testMyHivesEntries()

	view, _ := applyMyHivesQuery(entries, mhq(t, "sort=last_seen&order=desc"))
	if view[0].ID != "hosted-gamma" || view[len(view)-1].ID != "hosted-beta" {
		t.Errorf("last_seen desc order wrong: %s ... %s", view[0].ID, view[len(view)-1].ID)
	}

	view, matched := applyMyHivesQuery(entries, mhq(t, "sort=id&per_page=2&page=2"))
	if matched != 4 {
		t.Errorf("matched = %d, want 4 (pre-pagination)", matched)
	}
	if len(view) != 2 || view[0].ID != "hosted-delta" || view[1].ID != "hosted-gamma" {
		t.Errorf("page 2 wrong: %v", []string{view[0].ID, view[1].ID})
	}

	view, matched = applyMyHivesQuery(entries, mhq(t, "per_page=2&page=99"))
	if matched != 4 || len(view) != 0 {
		t.Errorf("past-the-end page: matched %d view %d, want 4/0", matched, len(view))
	}
}

func TestParseMyHivesQueryCapsPerPage(t *testing.T) {
	q := mhq(t, "per_page=999999")
	if q.perPage != maxMyHivesPerPage {
		t.Errorf("perPage = %d, want capped %d", q.perPage, maxMyHivesPerPage)
	}
	if q.page != 1 {
		t.Errorf("page defaults to 1 when per_page set, got %d", q.page)
	}
}

func TestMyHivesSummary(t *testing.T) {
	entries := testMyHivesEntries()
	entries[3].AssignedUnclaimed = true
	entries[0].Upgrading = true
	s := myHivesSummary(entries)
	want := map[string]int{
		"total": 4, "online": 2, "offline": 2,
		"pool_available": 1, "provisioning": 1,
		"assigned_unclaimed": 1, "upgrading": 1,
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("summary[%s] = %d, want %d", k, s[k], v)
		}
	}
}
