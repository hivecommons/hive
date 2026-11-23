package ioscan

import (
	"fmt"
	"testing"
	"time"
)

func TestSourceMemoryFlagsAuthorAndThreadUntilExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := NewSourceMemory(24 * time.Hour)
	m.now = func() time.Time { return now }

	m.Flag(Source{Author: "Mallory", Thread: "o/r#1"})
	if !m.Flagged(Source{Author: "mallory"}) {
		t.Fatal("author flag should match case-insensitively")
	}
	if !m.Flagged(Source{Author: "someone-else", Thread: "o/r#1"}) {
		t.Fatal("thread flag should cover other authors on the thread")
	}
	if m.Flagged(Source{Author: "alice", Thread: "o/r#2"}) {
		t.Fatal("unrelated source must not be flagged")
	}
	if m.Flagged(Source{Author: "mallory", TrustedAuthor: true}) {
		t.Fatal("trusted source must never count as flagged")
	}

	now = now.Add(24 * time.Hour)
	if m.Flagged(Source{Author: "mallory"}) {
		t.Fatal("flag should expire after the ttl")
	}
}

func TestSourceMemoryIgnoresTrustedAndAnonymousSources(t *testing.T) {
	m := NewSourceMemory(time.Hour)
	m.Flag(Source{Author: "owner", Thread: "o/r#3", TrustedAuthor: true})
	m.Flag(Source{})
	if len(m.authors) != 0 || len(m.threads) != 0 {
		t.Fatalf("nothing should be stored, got authors=%v threads=%v", m.authors, m.threads)
	}
}

func TestSourceMemoryNilAndDisabled(t *testing.T) {
	if NewSourceMemory(0) != nil {
		t.Fatal("zero ttl should disable the memory")
	}
	var m *SourceMemory
	m.Flag(Source{Author: "mallory"})
	if m.Flagged(Source{Author: "mallory"}) {
		t.Fatal("nil memory flags nothing")
	}
}

func TestSourceMemoryIsBounded(t *testing.T) {
	m := NewSourceMemory(time.Hour)
	m.max = 3
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		m.now = func() time.Time { return at }
		m.Flag(Source{Author: fmt.Sprintf("u%d", i)})
	}
	if len(m.authors) != 3 {
		t.Fatalf("authors = %d, want 3", len(m.authors))
	}
	if !m.Flagged(Source{Author: "u4"}) || m.Flagged(Source{Author: "u0"}) {
		t.Fatalf("oldest entries should be evicted first: %v", m.authors)
	}
}

func TestShouldFlagSource(t *testing.T) {
	high := Verdict{Blocked: true, Findings: []Finding{{Kind: KindInjection, Severity: SeverityHigh, Rule: "x"}}}
	medium := Verdict{Blocked: true, Findings: []Finding{{Kind: KindInjection, Severity: SeverityMedium, Rule: "x"}}}
	secret := Verdict{Blocked: true, Findings: []Finding{{Kind: KindSecret, Severity: SeverityCritical, Rule: "x"}}}
	unblocked := Verdict{Findings: high.Findings}
	for name, tc := range map[string]struct {
		v    Verdict
		want bool
	}{
		"high injection":         {high, true},
		"semantic block":         {SemanticVerdict(InjectionScore{Score: 0.9}, false), true},
		"medium injection":       {medium, false},
		"secret":                 {secret, false},
		"not blocked":            {unblocked, false},
		"classifier unavailable": {UnavailableVerdict("down"), false},
		"already flagged":        {SourceFlaggedVerdict("no_classifier"), false},
	} {
		if got := ShouldFlagSource(tc.v); got != tc.want {
			t.Errorf("%s: ShouldFlagSource = %v, want %v", name, got, tc.want)
		}
	}
}
