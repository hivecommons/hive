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

// A malformed marker is not a claim. Every one of these must be rejected
// rather than withholding an issue on garbage.
func TestParseMarker_RejectsMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"no marker":          "just a comment",
		"unterminated":       MarkerPrefix + " who 2026-09-23T01:30:00Z 2026-09-23T05:30:00Z",
		"too few fields":     MarkerPrefix + " who 2026-09-23T01:30:00Z " + MarkerSuffix,
		"too many fields":    MarkerPrefix + " who a b c " + MarkerSuffix,
		"bad started":        MarkerPrefix + " who yesterday 2026-09-23T05:30:00Z " + MarkerSuffix,
		"bad expires":        MarkerPrefix + " who 2026-09-23T01:30:00Z later " + MarkerSuffix,
		"empty record":       MarkerPrefix + " " + MarkerSuffix,
		"different marker":   "<!-- hive:preempt target=bot issue=1 -->",
		"pr claim ledger":    "Fixes #8380",
		"prefix only":        MarkerPrefix,
		"suffix before":      MarkerSuffix + " " + MarkerPrefix + " who 2026-09-23T01:30:00Z 2026-09-23T05:30:00Z",
		"identity has space": MarkerPrefix + " two words 2026-09-23T01:30:00Z 2026-09-23T05:30:00Z " + MarkerSuffix,
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
