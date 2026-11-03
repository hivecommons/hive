package issueclaim

import (
	"strings"
	"testing"
	"time"
)

// Fixed instants so every expectation is explicit; nothing here reads the
// wall clock (hivecommons/hive#8380).
var (
	started = time.Date(2026, 9, 23, 1, 30, 0, 0, time.UTC)
	expires = started.Add(DefaultTTL)
)

func TestMarker_RoundTrip(t *testing.T) {
	body := CommentBody("clubanderson", started, expires)
	if !strings.HasPrefix(body, MarkerPrefix) {
		t.Fatalf("comment must open with the marker, got %q", body)
	}
	if !strings.Contains(body, "Claimed by clubanderson until 2026-09-23T05:30:00Z") {
		t.Fatalf("human line must name the claimant and the expiry, got %q", body)
	}
	claim, ok := ParseMarker(body)
	if !ok {
		t.Fatal("rendered marker did not parse back")
	}
	if claim.Identity != "clubanderson" || !claim.StartedAt.Equal(started) || !claim.ExpiresAt.Equal(expires) {
		t.Fatalf("round trip lost fields: %+v", claim)
	}
	if claim.Source != SourceMarker {
		t.Fatalf("source = %q, want %q", claim.Source, SourceMarker)
	}
}

func TestParseMarker_FindsMarkerInsideLongerComment(t *testing.T) {
	body := "Some prose first.\n\n" + Marker("agent-1", started, expires) + "\nClaimed by agent-1."
	claim, ok := ParseMarker(body)
	if !ok || claim.Identity != "agent-1" {
		t.Fatalf("marker embedded in prose must parse, got ok=%v claim=%+v", ok, claim)
	}
}

func TestParseMarker_ReadsKindAndHive(t *testing.T) {
	body := "<!-- hive:claim who=mrbobbytables kind=contributor id=mrbobbytables:sess hive=hive-prod at=2026-09-23T01:30:00Z until=2026-09-23T05:30:00Z -->\n🔒 Claimed."
	claim, ok := ParseMarker(body)
	if !ok {
		t.Fatal("hub-shaped marker did not parse")
	}
	if claim.Identity != "mrbobbytables" || claim.Kind != "contributor" || claim.Hive != "hive-prod" {
		t.Fatalf("kind/hive lost: %+v", claim)
	}
	legacy, ok := ParseMarker(Marker("agent-1", started, expires))
	if !ok || legacy.Kind != "contributor" || legacy.Hive != "" {
		t.Fatalf("Marker() has kind=contributor and no hive, got ok=%v %+v", ok, legacy)
	}
}

func TestParseMarker_AcceptsLegacyMarker(t *testing.T) {
	body := LegacyMarker("agent-1", started, expires) + "\nClaimed by agent-1."
	claim, ok := ParseMarker(body)
	if !ok || claim.Identity != "agent-1" || !claim.StartedAt.Equal(started) || !claim.ExpiresAt.Equal(expires) {
		t.Fatalf("legacy marker did not parse, got ok=%v claim=%+v", ok, claim)
	}
}

// A malformed marker is not a claim. Every one of these must be rejected
// rather than withholding an issue on garbage.
func TestParseMarker_RejectsMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"no marker":             "just a comment",
		"unterminated":          MarkerPrefix + " who=who at=2026-09-23T01:30:00Z until=2026-09-23T05:30:00Z",
		"missing who":           MarkerPrefix + " at=2026-09-23T01:30:00Z until=2026-09-23T05:30:00Z " + MarkerSuffix,
		"missing until":         MarkerPrefix + " who=who at=2026-09-23T01:30:00Z " + MarkerSuffix,
		"bare field":            MarkerPrefix + " who at=2026-09-23T01:30:00Z until=2026-09-23T05:30:00Z " + MarkerSuffix,
		"bad started":           MarkerPrefix + " who=who at=yesterday until=2026-09-23T05:30:00Z " + MarkerSuffix,
		"bad expires":           MarkerPrefix + " who=who at=2026-09-23T01:30:00Z until=later " + MarkerSuffix,
		"empty record":          MarkerPrefix + " " + MarkerSuffix,
		"different marker":      "<!-- hive:preempt target=bot issue=1 -->",
		"pr claim ledger":       "Fixes #8380",
		"prefix only":           MarkerPrefix,
		"legacy too few fields": LegacyMarkerPrefix + " who 2026-09-23T01:30:00Z " + MarkerSuffix,
	} {
		t.Run(name, func(t *testing.T) {
			if claim, ok := ParseMarker(body); ok {
				t.Fatalf("malformed marker parsed as a claim: %+v", claim)
			}
		})
	}
}

func TestLive_ExpiryIsExclusiveAndZeroIsNeverLive(t *testing.T) {
	claim := Claim{Identity: "who", StartedAt: started, ExpiresAt: expires}
	if !claim.Live(expires.Add(-time.Second)) {
		t.Error("claim a second before expiry must be live")
	}
	if claim.Live(expires) {
		t.Error("claim AT its expiry must not be live")
	}
	if claim.Live(expires.Add(time.Second)) {
		t.Error("claim after expiry must not be live")
	}
	if (Claim{Identity: "who"}).Live(started) {
		t.Error("a claim with no expiry must never be live — unbounded claims are the bug")
	}
	if (Claim{ExpiresAt: expires}).Live(started) {
		t.Error("a claim with no identity must never be live")
	}
}

func TestLatest_NewestStartWins(t *testing.T) {
	older := Marker("first", started, started.Add(time.Hour))
	newer := Marker("second", started.Add(2*time.Hour), started.Add(6*time.Hour))
	claim, ok := Latest([]string{newer, "noise", older}, DefaultTTL)
	if !ok || claim.Identity != "second" {
		t.Fatalf("newest start must win regardless of order, got ok=%v %+v", ok, claim)
	}
	if _, ok := Latest([]string{"nothing", "here"}, DefaultTTL); ok {
		t.Fatal("no markers must yield no claim")
	}
	if _, ok := Latest(nil, DefaultTTL); ok {
		t.Fatal("nil bodies must yield no claim")
	}
}

func TestLatest_ClampsMarkerExpiryToTTL(t *testing.T) {
	for name, tc := range map[string]struct {
		markerExpires time.Time
		ttl           time.Duration
		wantExpires   time.Time
	}{
		"beyond ttl clamps": {
			markerExpires: started.Add(24 * time.Hour),
			ttl:           90 * time.Minute,
			wantExpires:   started.Add(90 * time.Minute),
		},
		"within ttl unchanged": {
			markerExpires: started.Add(30 * time.Minute),
			ttl:           90 * time.Minute,
			wantExpires:   started.Add(30 * time.Minute),
		},
		"nonpositive ttl leaves parser result unchanged": {
			markerExpires: started.Add(24 * time.Hour),
			ttl:           0,
			wantExpires:   started.Add(24 * time.Hour),
		},
	} {
		t.Run(name, func(t *testing.T) {
			claim, ok := Latest([]string{Marker("agent", started, tc.markerExpires)}, tc.ttl)
			if !ok {
				t.Fatal("marker did not parse")
			}
			if !claim.ExpiresAt.Equal(tc.wantExpires) {
				t.Fatalf("ExpiresAt = %v, want %v", claim.ExpiresAt, tc.wantExpires)
			}
		})
	}
}

func TestFromAssignees(t *testing.T) {
	claim, ok := FromAssignees([]string{" ", "alice", "bob"}, started, DefaultTTL)
	if !ok || claim.Identity != "alice" || claim.Source != SourceAssignee {
		t.Fatalf("first non-blank assignee must claim, got ok=%v %+v", ok, claim)
	}
	if !claim.ExpiresAt.Equal(started.Add(DefaultTTL)) {
		t.Fatalf("assignee claim must expire ttl after last activity, got %v", claim.ExpiresAt)
	}
	if _, ok := FromAssignees(nil, started, DefaultTTL); ok {
		t.Error("no assignees must yield no claim")
	}
	if _, ok := FromAssignees([]string{"alice"}, time.Time{}, DefaultTTL); ok {
		t.Error("unknown last activity must yield no claim — an expiry the hub cannot compute is not assumed")
	}
	if _, ok := FromAssignees([]string{"alice"}, started, 0); ok {
		t.Error("zero ttl must yield no claim")
	}
}

func TestResolve(t *testing.T) {
	now := started.Add(time.Hour)
	live := Marker("marker-holder", started, expires)
	lapsed := Marker("marker-holder", started.Add(-2*DefaultTTL), started.Add(-DefaultTTL))

	t.Run("live marker outranks assignee", func(t *testing.T) {
		claim, ok := Resolve([]string{live}, []string{"assignee"}, started, DefaultTTL, now)
		if !ok || claim.Identity != "marker-holder" {
			t.Fatalf("got ok=%v %+v", ok, claim)
		}
	})
	t.Run("expired marker releases the issue even with an assignee", func(t *testing.T) {
		if claim, ok := Resolve([]string{lapsed}, []string{"assignee"}, started, DefaultTTL, now); ok {
			t.Fatalf("expired marker must not fall back to the assignee, got %+v", claim)
		}
	})
	t.Run("assignee claims when no marker", func(t *testing.T) {
		claim, ok := Resolve(nil, []string{"assignee"}, started, DefaultTTL, now)
		if !ok || claim.Identity != "assignee" || claim.Source != SourceAssignee {
			t.Fatalf("got ok=%v %+v", ok, claim)
		}
	})
	t.Run("stale assignee is not a claim", func(t *testing.T) {
		if _, ok := Resolve(nil, []string{"assignee"}, started.Add(-2*DefaultTTL), DefaultTTL, now); ok {
			t.Fatal("assignee with no activity inside the ttl must not withhold")
		}
	})
	t.Run("nothing claims nothing", func(t *testing.T) {
		if _, ok := Resolve(nil, nil, started, DefaultTTL, now); ok {
			t.Fatal("no evidence must yield no claim")
		}
	})
}

// tier= and verified= are read from contributor markers; a marker written
// before they existed reads as the lowest tier and unverified (#12041).
func TestParseMarker_ContributorTierAndVerified(t *testing.T) {
	for name, tc := range map[string]struct {
		body         string
		wantTier     string
		wantVerified bool
	}{
		"stamped":           {body: "<!-- hive:claim who=a kind=contributor hive=h tier=Trusted verified=true until=2026-09-23T05:30:00Z -->", wantTier: "trusted", wantVerified: true},
		"unverified":        {body: "<!-- hive:claim who=a kind=contributor hive=h tier=merger verified=false until=2026-09-23T05:30:00Z -->", wantTier: "merger"},
		"legacy fields":     {body: "<!-- hive:claim who=a kind=contributor hive=h until=2026-09-23T05:30:00Z -->", wantTier: ContributorTierDefault},
		"odd verified":      {body: "<!-- hive:claim who=a kind=contributor tier=trusted verified=yes until=2026-09-23T05:30:00Z -->", wantTier: "trusted"},
		"agent has none":    {body: "<!-- hive:claim who=a kind=agent tier=trusted verified=true until=2026-09-23T05:30:00Z -->"},
		"legacy positional": {body: LegacyMarker("a", started, expires)},
	} {
		claim, ok := ParseMarker(tc.body)
		if !ok {
			t.Fatalf("%s: did not parse", name)
		}
		if claim.Tier != tc.wantTier || claim.Verified != tc.wantVerified {
			t.Fatalf("%s: tier=%q verified=%v, want %q/%v", name, claim.Tier, claim.Verified, tc.wantTier, tc.wantVerified)
		}
	}
}

// hivecommons/hive#13069: pr_started= is the marker's PR evidence; absent
// or anything but "true" is false.
func TestParseMarker_ReadsPRStarted(t *testing.T) {
	with := "<!-- hive:claim who=scanner kind=agent hive=h pr_started=true at=2026-09-23T01:30:00Z until=2026-09-23T05:30:00Z -->"
	claim, ok := ParseMarker(with)
	if !ok || !claim.PRStarted || claim.Kind != "agent" {
		t.Fatalf("pr_started=true lost: ok=%v %+v", ok, claim)
	}
	without := "<!-- hive:claim who=scanner kind=agent hive=h at=2026-09-23T01:30:00Z until=2026-09-23T05:30:00Z -->"
	if claim, ok := ParseMarker(without); !ok || claim.PRStarted {
		t.Fatalf("absent pr_started parsed true: ok=%v %+v", ok, claim)
	}
	odd := "<!-- hive:claim who=scanner kind=agent pr_started=yes until=2026-09-23T05:30:00Z -->"
	if claim, ok := ParseMarker(odd); !ok || claim.PRStarted {
		t.Fatalf("pr_started=yes parsed true: ok=%v %+v", ok, claim)
	}
}

func TestLatest_ReleaseMarkerEndsTheClaimItNames(t *testing.T) {
	started := time.Date(2026, 10, 20, 14, 21, 15, 0, time.UTC)
	claim := Marker("scanner", started, started.Add(2*time.Hour))
	release := "<!-- hive:release who=scanner kind=agent reason=holding_agent_relaunched at=2026-10-20T15:35:00Z -->\n🔓 released"
	now := started.Add(30 * time.Minute)

	got, ok := Latest([]string{claim, release}, 4*time.Hour)
	if !ok || !got.Released || got.Live(now) {
		t.Fatalf("release after claim: ok=%v released=%v live=%v", ok, got.Released, got.Live(now))
	}
	if c, live := Resolve([]string{claim, release}, []string{"alice"}, started, 4*time.Hour, now); live {
		t.Fatalf("Resolve fell back to a claim after release: %+v", c)
	}

	// A release that predates the claim (an earlier hold by the same agent
	// that was let go) does not end the newer claim.
	stale := "<!-- hive:release who=scanner kind=agent reason=x at=2026-10-20T13:00:00Z -->"
	if got, _ := Latest([]string{stale, claim}, 4*time.Hour); got.Released || !got.Live(now) {
		t.Fatalf("release before claim must not end it: %+v", got)
	}
	// Nor does a release of another holder.
	other := "<!-- hive:release who=quality kind=agent reason=x at=2026-10-20T15:35:00Z -->"
	if got, _ := Latest([]string{claim, other}, 4*time.Hour); got.Released {
		t.Fatalf("release of another holder ended the claim: %+v", got)
	}
	// A legacy release marker without at= is ordered by position.
	legacy := "<!-- hive:release who=scanner kind=agent reason=x -->"
	if got, _ := Latest([]string{legacy, claim}, 4*time.Hour); got.Released {
		t.Fatal("positional release before the claim must not end it")
	}
	if got, _ := Latest([]string{claim, legacy}, 4*time.Hour); !got.Released {
		t.Fatal("positional release after the claim must end it")
	}
}

func TestParseReleaseMarker(t *testing.T) {
	rel, ok := ParseReleaseMarker("text <!-- hive:release who=scanner kind=agent at=2026-10-20T15:35:00Z --> more")
	if !ok || rel.Identity != "scanner" || !rel.At.Equal(time.Date(2026, 10, 20, 15, 35, 0, 0, time.UTC)) {
		t.Fatalf("ParseReleaseMarker: %+v %v", rel, ok)
	}
	if _, ok := ParseReleaseMarker("<!-- hive:release kind=agent -->"); ok {
		t.Fatal("marker without who= must not parse")
	}
	if _, ok := ParseReleaseMarker("<!-- hive:release who=x at=yesterday -->"); ok {
		t.Fatal("unparseable at= must not parse")
	}
	if _, ok := ParseReleaseMarker("<!-- hive:release who=x"); ok {
		t.Fatal("unterminated marker must not parse")
	}
	if _, ok := ParseReleaseMarker("<!-- hive:claim who=x until=2026-10-20T15:35:00Z -->"); ok {
		t.Fatal("claim marker is not a release")
	}
}
