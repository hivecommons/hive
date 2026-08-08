package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBuildUsageBuckets covers the rollup math: grouping, share percentages,
// descending sort, deterministic tie-breaking, and the empty fleet. Keyed with
// usageOrgRepoKey, the same func the org rollup uses in production.
func TestBuildUsageBuckets(t *testing.T) {
	tests := []struct {
		name      string
		hives     []RegistryEntry
		total     int64
		wantKeys  []string
		wantToks  []int64
		wantHives []int
		wantShare []float64
	}{
		{
			name:     "empty fleet",
			hives:    nil,
			total:    0,
			wantKeys: []string{},
		},
		{
			name:      "single hive keys on org/repo",
			hives:     []RegistryEntry{{Org: "acme", PrimaryRepo: "widgets", TotalTokens24h: 100}},
			total:     100,
			wantKeys:  []string{"acme/widgets"},
			wantToks:  []int64{100},
			wantHives: []int{1},
			wantShare: []float64{100},
		},
		{
			name: "sorted by consumption descending",
			hives: []RegistryEntry{
				{Org: "acme", PrimaryRepo: "small", TotalTokens24h: 10},
				{Org: "acme", PrimaryRepo: "big", TotalTokens24h: 70},
				{Org: "acme", PrimaryRepo: "mid", TotalTokens24h: 20},
			},
			total:     100,
			wantKeys:  []string{"acme/big", "acme/mid", "acme/small"},
			wantToks:  []int64{70, 20, 10},
			wantHives: []int{1, 1, 1},
			wantShare: []float64{70, 20, 10},
		},
		{
			// THE CASE THAT MOTIVATED THE FIX: same org, different repos. Under
			// the old org-only key these collapsed into one "acme" bucket worth
			// 50 tokens across 2 hives; they must now be separate rows.
			name: "same org different repos are separate buckets",
			hives: []RegistryEntry{
				{Org: "acme", PrimaryRepo: "widgets", TotalTokens24h: 30},
				{Org: "acme", PrimaryRepo: "gadgets", TotalTokens24h: 20},
				{Org: "other", PrimaryRepo: "thing", TotalTokens24h: 50},
			},
			total:     100,
			wantKeys:  []string{"other/thing", "acme/widgets", "acme/gadgets"},
			wantToks:  []int64{50, 30, 20},
			wantHives: []int{1, 1, 1},
			wantShare: []float64{50, 30, 20},
		},
		{
			name: "same org same repo still aggregates into one bucket",
			hives: []RegistryEntry{
				{Org: "acme", PrimaryRepo: "widgets", TotalTokens24h: 30},
				{Org: "acme", PrimaryRepo: "widgets", TotalTokens24h: 20},
				{Org: "other", PrimaryRepo: "thing", TotalTokens24h: 50},
			},
			total: 100,
			// 50/50 tie → tie-break on Key ascending: "acme/..." < "other/...".
			wantKeys:  []string{"acme/widgets", "other/thing"},
			wantToks:  []int64{50, 50},
			wantHives: []int{2, 1},
			wantShare: []float64{50, 50},
		},
		{
			// Multi-repo hive: PrimaryRepo alone decides the key. Joining the
			// whole Repos slice would mint a bucket per repo combination.
			name: "multi-repo hive keys on the primary repo only",
			hives: []RegistryEntry{
				{Org: "acme", PrimaryRepo: "widgets", Repos: []string{"widgets", "gadgets", "sprockets"}, TotalTokens24h: 100},
			},
			total:     100,
			wantKeys:  []string{"acme/widgets"},
			wantToks:  []int64{100},
			wantHives: []int{1},
		},
		{
			name: "org with no primary repo renders as bare org, never a trailing slash",
			hives: []RegistryEntry{
				{Org: "acme", TotalTokens24h: 60},
				{Org: "acme", PrimaryRepo: "   ", TotalTokens24h: 40},
			},
			total: 100,
			// Both fold to the same bare-org bucket; whitespace is not a repo.
			wantKeys:  []string{"acme"},
			wantToks:  []int64{100},
			wantHives: []int{2},
			wantShare: []float64{100},
		},
		{
			name: "repo with no org keys on the bare repo",
			hives: []RegistryEntry{
				{PrimaryRepo: "orphan", TotalTokens24h: 100},
			},
			total:     100,
			wantKeys:  []string{"orphan"},
			wantToks:  []int64{100},
			wantHives: []int{1},
		},
		{
			name: "ties break on key ascending for stable ordering",
			hives: []RegistryEntry{
				{Org: "zebra", PrimaryRepo: "r", TotalTokens24h: 5},
				{Org: "alpha", PrimaryRepo: "r", TotalTokens24h: 5},
				{Org: "mango", PrimaryRepo: "r", TotalTokens24h: 5},
			},
			total:     15,
			wantKeys:  []string{"alpha/r", "mango/r", "zebra/r"},
			wantToks:  []int64{5, 5, 5},
			wantHives: []int{1, 1, 1},
		},
		{
			name: "neither org nor repo folds into the unattributed bucket, not dropped",
			hives: []RegistryEntry{
				{Org: "", PrimaryRepo: "", TotalTokens24h: 40},
				{Org: "   ", PrimaryRepo: "  ", TotalTokens24h: 10},
				{Org: "acme", PrimaryRepo: "widgets", TotalTokens24h: 50},
			},
			total: 100,
			// 50/50 tie → Key ascending; "(" sorts before letters.
			wantKeys:  []string{usageUnattributed, "acme/widgets"},
			wantToks:  []int64{50, 50},
			wantHives: []int{2, 1},
		},
		{
			name: "zero fleet total yields zero shares, never NaN",
			hives: []RegistryEntry{
				{Org: "idle", PrimaryRepo: "a", TotalTokens24h: 0},
				{Org: "idle", PrimaryRepo: "b", TotalTokens24h: 0},
			},
			total:     0,
			wantKeys:  []string{"idle/a", "idle/b"},
			wantToks:  []int64{0, 0},
			wantHives: []int{1, 1},
			wantShare: []float64{0, 0},
		},
		{
			name: "negative tokens clamped to zero",
			hives: []RegistryEntry{
				{Org: "acme", PrimaryRepo: "corrupt", TotalTokens24h: -500},
				{Org: "acme", PrimaryRepo: "good", TotalTokens24h: 100},
			},
			total:     100,
			wantKeys:  []string{"acme/good", "acme/corrupt"},
			wantToks:  []int64{100, 0},
			wantHives: []int{1, 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildUsageBuckets(tc.hives, usageOrgRepoKey, tc.total)
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("bucket count = %d, want %d (%+v)", len(got), len(tc.wantKeys), got)
			}
			for i := range got {
				if got[i].Key != tc.wantKeys[i] {
					t.Errorf("bucket[%d].Key = %q, want %q", i, got[i].Key, tc.wantKeys[i])
				}
				if tc.wantToks != nil && got[i].Tokens != tc.wantToks[i] {
					t.Errorf("bucket[%d].Tokens = %d, want %d", i, got[i].Tokens, tc.wantToks[i])
				}
				if tc.wantHives != nil && got[i].Hives != tc.wantHives[i] {
					t.Errorf("bucket[%d].Hives = %d, want %d", i, got[i].Hives, tc.wantHives[i])
				}
				if tc.wantShare != nil && got[i].SharePct != tc.wantShare[i] {
					t.Errorf("bucket[%d].SharePct = %v, want %v", i, got[i].SharePct, tc.wantShare[i])
				}
			}
		})
	}
}

// TestBuildUsageBucketsNilKeyFunc guards the nil-func path.
func TestBuildUsageBucketsNilKeyFunc(t *testing.T) {
	if got := buildUsageBuckets([]RegistryEntry{{Org: "a"}}, nil, 0); len(got) != 0 {
		t.Fatalf("nil keyOf should yield no buckets, got %+v", got)
	}
}

// TestBuildUsageBucketsCarryHiveIDs pins the bucket→hive back-links the
// dashboard uses to jump from a usage row to the hive rows in the fleet
// table: every contributing hive's id appears (sorted, deterministic), a
// blank id is dropped rather than emitted as "", and the JSON key is the
// hiveIds the UI reads.
func TestBuildUsageBucketsCarryHiveIDs(t *testing.T) {
	hives := []RegistryEntry{
		{ID: "h-2", Org: "acme", PrimaryRepo: "widgets", TotalTokens24h: 60},
		{ID: "h-1", Org: "acme", PrimaryRepo: "widgets", TotalTokens24h: 40},
		{ID: "", Org: "acme", PrimaryRepo: "widgets"}, // blank id: counted, not linked
		{ID: "h-3", Org: "other", PrimaryRepo: "app", TotalTokens24h: 100},
	}
	got := buildUsageBuckets(hives, usageOrgRepoKey, 200)
	if len(got) != 2 {
		t.Fatalf("buckets = %d, want 2 (%+v)", len(got), got)
	}
	// acme/widgets sorts first on tokens tie-break? 100 in each bucket — tie
	// breaks on key ascending, so acme/widgets first.
	acme := got[0]
	if acme.Key != "acme/widgets" {
		t.Fatalf("bucket[0].Key = %q, want acme/widgets", acme.Key)
	}
	if len(acme.HiveIDs) != 2 || acme.HiveIDs[0] != "h-1" || acme.HiveIDs[1] != "h-2" {
		t.Fatalf("acme/widgets HiveIDs = %v, want [h-1 h-2] sorted, blank dropped", acme.HiveIDs)
	}
	if acme.Hives != 3 {
		t.Fatalf("acme/widgets Hives = %d, want 3 (blank-id hive still counts)", acme.Hives)
	}
	b, err := json.Marshal(acme)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"hiveIds":["h-1","h-2"]`) {
		t.Fatalf("bucket JSON missing hiveIds: %s", b)
	}
}

// TestUsageInt64NoOverflow proves the fleet sum stays correct at magnitudes
// that would overflow a 32-bit int.
func TestUsageInt64NoOverflow(t *testing.T) {
	// Each hive at the ingest clamp ceiling (100e9); 42 of them far exceeds
	// math.MaxInt32 (~2.1e9).
	const perHive int64 = 100_000_000_000
	const hiveCount = 42
	hives := make([]RegistryEntry, 0, hiveCount)
	for i := 0; i < hiveCount; i++ {
		hives = append(hives, RegistryEntry{Org: "acme", Owner: "o", TotalTokens24h: perHive})
	}
	got := buildUsageRollup(hives, nil, nil, "fleet")
	want := perHive * hiveCount
	if got.TotalTokens != want {
		t.Fatalf("TotalTokens = %d, want %d", got.TotalTokens, want)
	}
	if got.TotalTokens <= 0 {
		t.Fatal("fleet total overflowed or went negative")
	}
}

// TestBuildUsageRollup covers whole-report behavior: placeholder exclusion,
// zero-consumption surfacing, and multi-dimension grouping.
func TestBuildUsageRollup(t *testing.T) {
	placeholderOf := func(h RegistryEntry) bool { return isUnassignedPlaceholder(h.Org, "") }

	t.Run("empty fleet returns non-nil slices", func(t *testing.T) {
		got := buildUsageRollup(nil, placeholderOf, nil, "fleet")
		if got.TotalTokens != 0 || got.HiveCount != 0 {
			t.Errorf("empty fleet: total=%d count=%d, want 0/0", got.TotalTokens, got.HiveCount)
		}
		// Non-nil so the JSON is [] not null — null would break `.length` in JS.
		if got.ByOrg == nil || got.ByOwner == nil || got.ByCluster == nil ||
			got.ZeroConsumption == nil || got.History == nil {
			t.Error("empty fleet must yield non-nil slices, not nil")
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"byOrg", "byOwner", "byCluster", "zeroConsumption", "history"} {
			if raw[k] == nil {
				t.Errorf("field %q serialized as null, want []", k)
			}
		}
	})

	t.Run("unassigned placeholders excluded entirely", func(t *testing.T) {
		hives := []RegistryEntry{
			{ID: "1", Name: "real", Org: "acme", Owner: "amy", TotalTokens24h: 100},
			{ID: "2", Name: "slot-a", Org: "available-pool", TotalTokens24h: 0},
			{ID: "3", Name: "slot-b", Org: "available-pool", TotalTokens24h: 0},
		}
		got := buildUsageRollup(hives, placeholderOf, nil, "fleet")
		if got.HiveCount != 1 {
			t.Errorf("HiveCount = %d, want 1 (placeholders must not count)", got.HiveCount)
		}
		// Placeholders legitimately consume nothing — they must NOT be flagged.
		if len(got.ZeroConsumption) != 0 {
			t.Errorf("placeholders were flagged as zero-consumption: %+v", got.ZeroConsumption)
		}
		for _, b := range got.ByOrg {
			if strings.HasPrefix(b.Key, "available-pool") {
				t.Error("placeholder org leaked into the org/repo rollup")
			}
		}
	})

	t.Run("claimed hive burning nothing is surfaced", func(t *testing.T) {
		hives := []RegistryEntry{
			{ID: "1", Name: "busy", Org: "acme", TotalTokens24h: 100},
			{ID: "2", Name: "idle", Org: "acme", TotalTokens24h: 0, Online: true},
			{ID: "3", Name: "slot", Org: "available-x", TotalTokens24h: 0},
		}
		got := buildUsageRollup(hives, placeholderOf, nil, "fleet")
		if len(got.ZeroConsumption) != 1 {
			t.Fatalf("ZeroConsumption = %+v, want exactly the claimed idle hive", got.ZeroConsumption)
		}
		if got.ZeroConsumption[0].Name != "idle" {
			t.Errorf("flagged %q, want \"idle\"", got.ZeroConsumption[0].Name)
		}
		if !got.ZeroConsumption[0].Online {
			t.Error("Online should be preserved to distinguish idle from unreachable")
		}
	})

	t.Run("groups by owner and cluster with name preferred over id", func(t *testing.T) {
		hives := []RegistryEntry{
			{ID: "1", Owner: "amy", ClusterID: "c1", ClusterName: "prod", TotalTokens24h: 60},
			{ID: "2", Owner: "bob", ClusterID: "c1", ClusterName: "prod", TotalTokens24h: 30},
			{ID: "3", Owner: "amy", ClusterID: "c2", TotalTokens24h: 10},
		}
		got := buildUsageRollup(hives, placeholderOf, nil, "fleet")
		if got.TotalTokens != 100 {
			t.Fatalf("TotalTokens = %d, want 100", got.TotalTokens)
		}
		if got.ByOwner[0].Key != "amy" || got.ByOwner[0].Tokens != 70 || got.ByOwner[0].Hives != 2 {
			t.Errorf("ByOwner[0] = %+v, want amy/70/2", got.ByOwner[0])
		}
		if got.ByCluster[0].Key != "prod" || got.ByCluster[0].Tokens != 90 {
			t.Errorf("ByCluster[0] = %+v, want prod/90", got.ByCluster[0])
		}
		// Cluster with no friendly name falls back to the ID.
		if got.ByCluster[1].Key != "c2" {
			t.Errorf("ByCluster[1].Key = %q, want fallback to id \"c2\"", got.ByCluster[1].Key)
		}
	})

	t.Run("shares sum to 100 across a rollup", func(t *testing.T) {
		hives := []RegistryEntry{
			{Org: "a", PrimaryRepo: "r", TotalTokens24h: 33},
			{Org: "b", PrimaryRepo: "r", TotalTokens24h: 33},
			{Org: "c", PrimaryRepo: "r", TotalTokens24h: 34},
		}
		got := buildUsageRollup(hives, placeholderOf, nil, "fleet")
		var sum float64
		for _, b := range got.ByOrg {
			sum += b.SharePct
		}
		if sum < 99.99 || sum > 100.01 {
			t.Errorf("shares sum to %v, want ~100", sum)
		}
	})

	// The motivating regression, exercised end-to-end through the rollup rather
	// than the bucket builder: splitting one org into per-repo rows must not
	// lose or double-count tokens.
	t.Run("two hives in one org split by repo and still sum to the fleet total", func(t *testing.T) {
		hives := []RegistryEntry{
			{ID: "1", Org: "kubestellar", PrimaryRepo: "console", TotalTokens24h: 70},
			{ID: "2", Org: "kubestellar", PrimaryRepo: "docs", TotalTokens24h: 30},
			// No primary repo: must appear as the bare org, not "kubestellar/".
			{ID: "3", Org: "kubestellar", TotalTokens24h: 0},
		}
		got := buildUsageRollup(hives, placeholderOf, nil, "fleet")
		if got.TotalTokens != 100 {
			t.Fatalf("TotalTokens = %d, want 100", got.TotalTokens)
		}
		wantKeys := []string{"kubestellar/console", "kubestellar/docs", "kubestellar"}
		if len(got.ByOrg) != len(wantKeys) {
			t.Fatalf("ByOrg = %+v, want %d buckets %v", got.ByOrg, len(wantKeys), wantKeys)
		}
		var sumTokens int64
		var sumHives int
		for i, b := range got.ByOrg {
			if b.Key != wantKeys[i] {
				t.Errorf("ByOrg[%d].Key = %q, want %q", i, b.Key, wantKeys[i])
			}
			if strings.HasSuffix(b.Key, usageOrgRepoSep) {
				t.Errorf("ByOrg[%d].Key = %q has a trailing separator", i, b.Key)
			}
			sumTokens += b.Tokens
			sumHives += b.Hives
		}
		if sumTokens != got.TotalTokens {
			t.Errorf("bucket tokens sum to %d, want the fleet total %d", sumTokens, got.TotalTokens)
		}
		if sumHives != got.HiveCount {
			t.Errorf("bucket hives sum to %d, want HiveCount %d", sumHives, got.HiveCount)
		}
	})
}

// TestUsageOrgRepoKey pins the four org/repo edge cases directly.
func TestUsageOrgRepoKey(t *testing.T) {
	tests := []struct {
		name string
		in   RegistryEntry
		want string
	}{
		{"org and primary repo", RegistryEntry{Org: "acme", PrimaryRepo: "widgets"}, "acme/widgets"},
		{"org only renders bare, no trailing slash", RegistryEntry{Org: "acme"}, "acme"},
		{"whitespace repo is not a repo", RegistryEntry{Org: "acme", PrimaryRepo: "  "}, "acme"},
		{"repo only renders bare, no leading slash", RegistryEntry{PrimaryRepo: "orphan"}, "orphan"},
		{"neither yields empty for usageGroupKey to fold", RegistryEntry{}, ""},
		{"whitespace both yields empty", RegistryEntry{Org: " ", PrimaryRepo: " "}, ""},
		{"values are trimmed", RegistryEntry{Org: " acme ", PrimaryRepo: " widgets "}, "acme/widgets"},
		{
			"Repos slice is ignored in favor of PrimaryRepo",
			RegistryEntry{Org: "acme", PrimaryRepo: "widgets", Repos: []string{"a", "b", "c"}},
			"acme/widgets",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageOrgRepoKey(tc.in); got != tc.want {
				t.Errorf("usageOrgRepoKey(%+v) = %q, want %q", tc.in, got, tc.want)
			}
			// Whatever the key, the rendered bucket label must never be blank
			// and must never be a bare or dangling separator.
			label := usageGroupKey(usageOrgRepoKey(tc.in))
			if label == "" || label == usageOrgRepoSep ||
				strings.HasPrefix(label, usageOrgRepoSep) || strings.HasSuffix(label, usageOrgRepoSep) {
				t.Errorf("bucket label %q is blank or a dangling separator", label)
			}
		})
	}
}

// TestIsUnassignedPlaceholder mirrors the dashboard's isPlaceholderHive().
func TestIsUnassignedPlaceholder(t *testing.T) {
	tests := []struct {
		name       string
		org        string
		provStatus string
		want       bool
	}{
		{"provStatus available is authoritative", "acme", "available", true},
		{"provStatus available case-insensitive", "acme", "Available", true},
		{"available- org prefix fallback", "available-pool-3", "", true},
		{"real org with running status", "acme", "running", false},
		{"empty everything", "", "", false},
		{"org merely containing available is not a placeholder", "unavailable-corp", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnassignedPlaceholder(tc.org, tc.provStatus); got != tc.want {
				t.Errorf("isUnassignedPlaceholder(%q,%q) = %v, want %v", tc.org, tc.provStatus, got, tc.want)
			}
		})
	}
}

// TestAppendUsageSnapshot covers interval rate-limiting and bounded retention.
func TestAppendUsageSnapshot(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	t.Run("first sample always recorded", func(t *testing.T) {
		got := appendUsageSnapshot(nil, 500, base)
		if len(got) != 1 || got[0].V != 500 {
			t.Fatalf("got %+v, want one point of 500", got)
		}
	})

	t.Run("sample inside the interval is skipped", func(t *testing.T) {
		h := appendUsageSnapshot(nil, 500, base)
		h = appendUsageSnapshot(h, 900, base.Add(usageSnapshotInterval-time.Second))
		if len(h) != 1 {
			t.Fatalf("len = %d, want 1 (too soon to resample)", len(h))
		}
	})

	t.Run("sample after the interval is recorded", func(t *testing.T) {
		h := appendUsageSnapshot(nil, 500, base)
		h = appendUsageSnapshot(h, 900, base.Add(usageSnapshotInterval))
		if len(h) != 2 || h[1].V != 900 {
			t.Fatalf("got %+v, want two points ending at 900", h)
		}
	})

	t.Run("retention bounded to usageSnapshotMaxPoints", func(t *testing.T) {
		var h []UsageSnapshot
		// Overfill by 50 samples.
		for i := 0; i < usageSnapshotMaxPoints+50; i++ {
			h = appendUsageSnapshot(h, int64(i), base.Add(time.Duration(i)*usageSnapshotInterval))
		}
		if len(h) != usageSnapshotMaxPoints {
			t.Fatalf("len = %d, want %d", len(h), usageSnapshotMaxPoints)
		}
		// Trimming must drop the OLDEST, keeping the newest value.
		if h[len(h)-1].V != int64(usageSnapshotMaxPoints+49) {
			t.Errorf("newest = %d, want %d", h[len(h)-1].V, usageSnapshotMaxPoints+49)
		}
	})

	t.Run("no trend is fabricated from a single data point", func(t *testing.T) {
		got := buildUsageRollup([]RegistryEntry{{Org: "a", TotalTokens24h: 5}}, nil, nil, "fleet")
		if len(got.History) != 0 {
			t.Errorf("History = %+v, want empty when nothing has been sampled", got.History)
		}
	})
}

// TestHandleUsageAuthorization is the security-critical test: fleet-wide usage
// must be admin-only, and a non-admin must see only their own hives.
func TestHandleUsageAuthorization(t *testing.T) {
	fleet := []RegistryEntry{
		{ID: "1", Name: "admin-hive", Org: "acme", Owner: hubAdminUsername, TotalTokens24h: 700},
		{ID: "2", Name: "amy-hive", Org: "amyco", Owner: "amy", TotalTokens24h: 200},
		{ID: "3", Name: "bob-hive", Org: "bobco", Owner: "bob", TotalTokens24h: 100},
	}

	// callUsage exercises the exact scoping logic handleUsage applies, without
	// requiring a full authenticated HTTP stack.
	callUsage := func(username string) UsageRollup {
		isAdmin := username == hubAdminUsername
		scoped := make([]RegistryEntry, 0, len(fleet))
		for _, h := range fleet {
			if isAdmin || h.Owner == username {
				scoped = append(scoped, h)
			}
		}
		scope := "own"
		var history []UsageSnapshot
		if isAdmin {
			scope = "fleet"
			history = []UsageSnapshot{{T: 1, V: 1000}}
		}
		return buildUsageRollup(scoped, func(h RegistryEntry) bool {
			return isUnassignedPlaceholder(h.Org, "")
		}, history, scope)
	}

	tests := []struct {
		name        string
		user        string
		wantScope   string
		wantTotal   int64
		wantHives   int
		wantHistory bool
		mustNotSee  []string
	}{
		{
			name: "admin sees the whole fleet", user: hubAdminUsername,
			wantScope: "fleet", wantTotal: 1000, wantHives: 3, wantHistory: true,
		},
		{
			name: "non-admin sees only their own hive", user: "amy",
			wantScope: "own", wantTotal: 200, wantHives: 1, wantHistory: false,
			mustNotSee: []string{"acme", "bobco"},
		},
		{
			name: "other non-admin is likewise scoped", user: "bob",
			wantScope: "own", wantTotal: 100, wantHives: 1, wantHistory: false,
			mustNotSee: []string{"acme", "amyco"},
		},
		{
			name: "unknown user sees nothing at all", user: "nobody",
			wantScope: "own", wantTotal: 0, wantHives: 0, wantHistory: false,
			mustNotSee: []string{"acme", "amyco", "bobco"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := callUsage(tc.user)
			if got.Scope != tc.wantScope {
				t.Errorf("Scope = %q, want %q", got.Scope, tc.wantScope)
			}
			if got.TotalTokens != tc.wantTotal {
				t.Errorf("TotalTokens = %d, want %d (leaked other users' usage?)", got.TotalTokens, tc.wantTotal)
			}
			if got.HiveCount != tc.wantHives {
				t.Errorf("HiveCount = %d, want %d", got.HiveCount, tc.wantHives)
			}
			if hasHistory := len(got.History) > 0; hasHistory != tc.wantHistory {
				t.Errorf("history present = %v, want %v (fleet trend is admin-only)", hasHistory, tc.wantHistory)
			}
			// No other tenant's org may appear anywhere in the response.
			blob, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, forbidden := range tc.mustNotSee {
				if containsStr(string(blob), forbidden) {
					t.Errorf("response leaked %q to user %q: %s", forbidden, tc.user, blob)
				}
			}
		})
	}
}

// TestHandleUsageRequiresAuthenticatedUser verifies an unauthenticated caller
// (no cookie, no bearer token) is scoped to nothing rather than the fleet.
func TestHandleUsageRequiresAuthenticatedUser(t *testing.T) {
	s := &HubServer{
		registry: Registry{Hives: []RegistryEntry{
			{ID: "1", Name: "secret", Org: "acme", Owner: "amy", TotalTokens24h: 999},
		}},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/saas/usage", nil)
	rec := httptest.NewRecorder()
	s.handleUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got UsageRollup
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Scope != "own" {
		t.Errorf("Scope = %q, want \"own\" for an anonymous caller", got.Scope)
	}
	if got.TotalTokens != 0 || got.HiveCount != 0 {
		t.Errorf("anonymous caller saw total=%d hives=%d, want 0/0", got.TotalTokens, got.HiveCount)
	}
	if len(got.History) != 0 {
		t.Error("anonymous caller must not receive the fleet history")
	}
}

// TestSampleUsageHistoryNoDeadlock verifies the sampler acquires s.mu itself
// and does not deadlock, and that it excludes placeholders from the total.
func TestSampleUsageHistoryNoDeadlock(t *testing.T) {
	s := &HubServer{
		registry: Registry{Hives: []RegistryEntry{
			{ID: "1", Org: "acme", TotalTokens24h: 400},
			{ID: "2", Org: "beta", TotalTokens24h: 100},
			{ID: "3", Org: "available-pool", TotalTokens24h: 0},
		}},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.sampleUsageHistory(time.Unix(1_700_000_000, 0))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sampleUsageHistory deadlocked — it must not be called while s.mu is held")
	}

	h := s.usageHistorySnapshot()
	if len(h) != 1 {
		t.Fatalf("history = %+v, want one sample", h)
	}
	if h[0].V != 500 {
		t.Errorf("sampled total = %d, want 500", h[0].V)
	}
}

// TestUsageHistorySnapshotIsACopy proves callers cannot mutate hub state.
func TestUsageHistorySnapshotIsACopy(t *testing.T) {
	s := &HubServer{usageHistory: []UsageSnapshot{{T: 1, V: 10}}}
	got := s.usageHistorySnapshot()
	got[0].V = 999
	if s.usageHistory[0].V != 10 {
		t.Fatal("usageHistorySnapshot returned the live slice; callers can corrupt hub state")
	}
}

// TestUsageCurrencyNoteIsHonest guards against someone later slipping an
// invented per-token price into the rollup.
func TestUsageCurrencyNoteIsHonest(t *testing.T) {
	got := buildUsageRollup([]RegistryEntry{{Org: "a", TotalTokens24h: 10}}, nil, nil, "fleet")
	if got.CurrencyNote == "" {
		t.Fatal("CurrencyNote must explain why no dollar figure is present")
	}
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The rollup must expose no USD/cost field — the hub has no per-model
	// data to derive one from.
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"usd", "cost", "costUsd", "totalUsd"} {
		if _, present := raw[forbidden]; present {
			t.Errorf("rollup exposes %q — the hub cannot derive currency", forbidden)
		}
	}
}

func containsStr(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
