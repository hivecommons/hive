package dashboard

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func resetDossierPublicCaches() {
	heraldryCacheMu.Lock()
	heraldryCache = map[string]heraldryCacheEntry{}
	heraldryCacheMu.Unlock()

	githubUserCacheMu.Lock()
	githubUserCache = map[string]githubUserCacheEntry{}
	githubUserCacheMu.Unlock()

	heraldryFlight = newFetchFlight()
	githubUserFlight = newFetchFlight()
}

func useDossierCacheMax(t *testing.T, maxEntries int) {
	t.Helper()
	old := dossierCacheMaxEntries
	dossierCacheMaxEntries = maxEntries
	t.Cleanup(func() { dossierCacheMaxEntries = old })
}

func TestDossierPublicCachesBoundEvictExpiredAndPreserveFresh(t *testing.T) {
	useDossierCacheMax(t, 8)
	resetDossierPublicCaches()
	t.Cleanup(resetDossierPublicCaches)

	now := time.Now()
	olderFresh := now.Add(-time.Hour)

	heraldryCacheMu.Lock()
	heraldryCache["expired-positive"] = heraldryCacheEntry{linked: true, fetchedAt: now.Add(-heraldryCacheTTL - time.Second)}
	heraldryCache["expired-negative"] = heraldryCacheEntry{linked: false, fetchedAt: now.Add(-heraldryNegativeTTL - time.Second)}
	heraldryCacheMu.Unlock()
	storeHeraldryCache("keeper", heraldryCacheEntry{
		badges: []HeraldryBadge{{Name: "Keeper"}},
		linked: true, fetchedAt: now,
	})
	for i := 0; i < effectiveDossierCacheMaxEntries()*3; i++ {
		storeHeraldryCache(fmt.Sprintf("heraldry-%02d", i), heraldryCacheEntry{
			badges: []HeraldryBadge{{Name: fmt.Sprintf("Badge %02d", i)}},
			linked: true, fetchedAt: olderFresh.Add(time.Duration(i) * time.Second),
		})
	}

	heraldryCacheMu.Lock()
	heraldryLen := len(heraldryCache)
	heraldryMax := effectiveDossierCacheMaxEntries()
	_, heraldryExpiredPositive := heraldryCache["expired-positive"]
	_, heraldryExpiredNegative := heraldryCache["expired-negative"]
	heraldryCacheMu.Unlock()
	if heraldryLen > heraldryMax {
		t.Fatalf("heraldry cache has %d entries, want <= %d", heraldryLen, heraldryMax)
	}
	if heraldryExpiredPositive {
		t.Fatal("expired positive heraldry entry was not removed")
	}
	if heraldryExpiredNegative {
		t.Fatal("expired negative heraldry entry was not removed")
	}
	if badges, linked, ok := heraldryCached("keeper"); !ok || !linked || len(badges) != 1 || badges[0].Name != "Keeper" {
		t.Fatalf("fresh heraldry keeper was not served after eviction pressure: badges=%+v linked=%v ok=%v", badges, linked, ok)
	}

	githubUserCacheMu.Lock()
	githubUserCache["expired-positive"] = githubUserCacheEntry{ok: true, fetchedAt: now.Add(-githubUserCacheTTL - time.Second)}
	githubUserCache["expired-negative"] = githubUserCacheEntry{ok: false, fetchedAt: now.Add(-heraldryNegativeTTL - time.Second)}
	githubUserCacheMu.Unlock()
	storeGitHubUserCache("keeper", githubUserCacheEntry{serviceYears: 7, followers: 42, ok: true, fetchedAt: now})
	for i := 0; i < effectiveDossierCacheMaxEntries()*3; i++ {
		storeGitHubUserCache(fmt.Sprintf("github-%02d", i), githubUserCacheEntry{
			serviceYears: i, followers: i + 1, ok: true,
			fetchedAt: olderFresh.Add(time.Duration(i) * time.Second),
		})
	}

	githubUserCacheMu.Lock()
	githubLen := len(githubUserCache)
	githubMax := effectiveDossierCacheMaxEntries()
	_, githubExpiredPositive := githubUserCache["expired-positive"]
	_, githubExpiredNegative := githubUserCache["expired-negative"]
	githubUserCacheMu.Unlock()
	if githubLen > githubMax {
		t.Fatalf("github user cache has %d entries, want <= %d", githubLen, githubMax)
	}
	if githubExpiredPositive {
		t.Fatal("expired positive github entry was not removed")
	}
	if githubExpiredNegative {
		t.Fatal("expired negative github entry was not removed")
	}
	if years, followers, ok, fresh := githubUserCached("keeper"); !fresh || !ok || years != 7 || followers != 42 {
		t.Fatalf("fresh github keeper was not served after eviction pressure: years=%d followers=%d ok=%v fresh=%v", years, followers, ok, fresh)
	}
}

func TestDossierPublicCachesConcurrentAccess(t *testing.T) {
	const workers = 64
	useDossierCacheMax(t, workers*2)
	resetDossierPublicCaches()
	t.Cleanup(resetDossierPublicCaches)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, workers*2)
	now := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			credlyName := fmt.Sprintf("credly-%02d", i)
			storeHeraldryCache(credlyName, heraldryCacheEntry{
				badges:    []HeraldryBadge{{Name: fmt.Sprintf("Concurrent Badge %02d", i)}},
				linked:    true,
				fetchedAt: now.Add(time.Duration(i) * time.Millisecond),
			})
			if badges, linked, ok := heraldryCached(credlyName); !ok || !linked || len(badges) != 1 || badges[0].Name != fmt.Sprintf("Concurrent Badge %02d", i) {
				errs <- fmt.Sprintf("heraldry worker %d got ok=%v linked=%v badges=%d", i, ok, linked, len(badges))
			}

			githubName := fmt.Sprintf("spray-%02d", i)
			storeGitHubUserCache(githubName, githubUserCacheEntry{
				serviceYears: i,
				followers:    9,
				ok:           true,
				fetchedAt:    now.Add(time.Duration(i) * time.Millisecond),
			})
			if years, followers, ok, fresh := githubUserCached(githubName); !fresh || !ok || years != i || followers != 9 {
				errs <- fmt.Sprintf("github worker %d got fresh=%v ok=%v years=%d followers=%d", i, fresh, ok, years, followers)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	heraldryCacheMu.Lock()
	heraldryLen := len(heraldryCache)
	heraldryCacheMu.Unlock()
	githubUserCacheMu.Lock()
	githubLen := len(githubUserCache)
	githubUserCacheMu.Unlock()
	if max := effectiveDossierCacheMaxEntries(); heraldryLen > max || githubLen > max {
		t.Fatalf("cache bounds after concurrent spray: heraldry=%d github=%d max=%d", heraldryLen, githubLen, max)
	}
}

func TestDossierPublicCachesConcurrentBound(t *testing.T) {
	const (
		workers = 64
		max     = 8
	)
	useDossierCacheMax(t, max)
	resetDossierPublicCaches()
	t.Cleanup(resetDossierPublicCaches)

	start := make(chan struct{})
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fetchedAt := now.Add(time.Duration(i) * time.Millisecond)
			storeHeraldryCache(fmt.Sprintf("heraldry-%02d", i), heraldryCacheEntry{
				badges:    []HeraldryBadge{{Name: fmt.Sprintf("Badge %02d", i)}},
				linked:    true,
				fetchedAt: fetchedAt,
			})
			storeGitHubUserCache(fmt.Sprintf("github-%02d", i), githubUserCacheEntry{
				serviceYears: i,
				followers:    i + 1,
				ok:           true,
				fetchedAt:    fetchedAt,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	heraldryCacheMu.Lock()
	heraldryLen := len(heraldryCache)
	for i := workers - max; i < workers; i++ {
		if _, ok := heraldryCache[fmt.Sprintf("heraldry-%02d", i)]; !ok {
			t.Errorf("newest heraldry entry %02d was evicted", i)
		}
	}
	heraldryCacheMu.Unlock()

	githubUserCacheMu.Lock()
	githubLen := len(githubUserCache)
	for i := workers - max; i < workers; i++ {
		if _, ok := githubUserCache[fmt.Sprintf("github-%02d", i)]; !ok {
			t.Errorf("newest github entry %02d was evicted", i)
		}
	}
	githubUserCacheMu.Unlock()

	if heraldryLen > max || githubLen > max {
		t.Fatalf("cache bounds after concurrent overflow: heraldry=%d github=%d max=%d", heraldryLen, githubLen, max)
	}
}
