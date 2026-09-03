package hub

import "testing"

// A cluster that changed domain (kubestellar.io -> hivecommons.dev) left every
// hive provisioned before the change pointing at a hostname no ingress serves,
// and the hub pushed that dead URL down to the spoke on every heartbeat. These
// pin the re-derivation and, just as importantly, the cases it must NOT touch.
func TestReconciledDashboardURLDomain(t *testing.T) {
	const (
		oldDomain = "hive.kubestellar.io"
		newDomain = "hive.hivecommons.dev"
		hiveID    = "hosted-daviddiaz0317-visual-hive--1jos"
	)
	cluster := &ClusterConfig{Domain: newDomain}
	hive := &SaaSHive{ID: hiveID}

	t.Run("re-domains a host minted from the old cluster domain", func(t *testing.T) {
		got := reconciledDashboardURLDomain("https://"+hiveID+"."+oldDomain, hive, cluster)
		want := "https://" + hiveID + "." + newDomain
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("returns empty when already on the current domain", func(t *testing.T) {
		if got := reconciledDashboardURLDomain("https://"+hiveID+"."+newDomain, hive, cluster); got != "" {
			t.Fatalf("expected no change, got %q", got)
		}
	})

	t.Run("leaves a vanity host to the vanity reconciler", func(t *testing.T) {
		// Leading label is a different hive-facing name, not the hive ID.
		// Rewriting it here would fight reconcileStaleVanityURL and churn the
		// user-visible hostname on every beat.
		vanity := "https://hosted-projectbluefin-common-nmq5." + oldDomain
		if got := reconciledDashboardURLDomain(vanity, &SaaSHive{ID: "hosted-projectbluefin-knuckle-gjvq"}, cluster); got != "" {
			t.Fatalf("expected vanity host untouched, got %q", got)
		}
	})

	t.Run("re-derives an unassigned placeholder pointing at another hive's host", func(t *testing.T) {
		// Pool slots are recycled, and a recycled slot kept the previous
		// tenant's URL — placeholders were advertising a foreign hive's
		// hostname. An unclaimed slot has no legitimate vanity name, so the
		// hive ID is authoritative.
		ph := &SaaSHive{ID: "hosted-available-oke-01-placeholder-bb95", Status: statusAvailable}
		got := reconciledDashboardURLDomain("https://hosted-tradingasbuddies-falcon-core-k2zn."+oldDomain, ph, cluster)
		want := "https://hosted-available-oke-01-placeholder-bb95." + newDomain
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("re-derives a placeholder identified by its available- org", func(t *testing.T) {
		ph := &SaaSHive{ID: "hosted-available-oke-07-placeholder-wlj4", Org: "available-oke-07"}
		got := reconciledDashboardURLDomain("https://hosted-mattsweetibm-s1netops-lzkm."+oldDomain, ph, cluster)
		want := "https://hosted-available-oke-07-placeholder-wlj4." + newDomain
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("preserves path and port while swapping the domain", func(t *testing.T) {
		got := reconciledDashboardURLDomain("https://"+hiveID+"."+oldDomain+"/snapshot", hive, cluster)
		want := "https://" + hiveID + "." + newDomain + "/snapshot"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("no-ops on missing inputs", func(t *testing.T) {
		valid := "https://" + hiveID + "." + oldDomain
		cases := map[string]struct {
			stored  string
			hive    *SaaSHive
			cluster *ClusterConfig
		}{
			"empty url":     {"", hive, cluster},
			"nil hive":      {valid, nil, cluster},
			"empty hive id": {valid, &SaaSHive{}, cluster},
			"nil cluster":   {valid, hive, nil},
			"empty domain":  {valid, hive, &ClusterConfig{}},
			"unparsable":    {"://not a url", hive, cluster},
			"no host":       {"not-a-url", hive, cluster},
		}
		for name, c := range cases {
			if got := reconciledDashboardURLDomain(c.stored, c.hive, c.cluster); got != "" {
				t.Errorf("%s: expected no change, got %q", name, got)
			}
		}
	})
}
