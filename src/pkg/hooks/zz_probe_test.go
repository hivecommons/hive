package hooks

import (
	"testing"
	"time"
)

// Probe 1: does the sliding window let you exceed the ceiling at a boundary?
func TestProbeWindowCeiling(t *testing.T) {
	rl := newRateLimiter()
	base := time.Now()
	limit := 3
	// burst 3 at t=0
	for i := 0; i < limit; i++ {
		if !rl.allow("h", limit, base) {
			t.Fatalf("burst %d denied", i)
		}
	}
	if rl.allow("h", limit, base) {
		t.Fatalf("4th at t=0 allowed")
	}
	// at exactly t=60s, cutoff = base; ts.After(base) is false -> all dropped
	if !rl.allow("h", limit, base.Add(time.Minute)) {
		t.Fatalf("t=60s denied")
	}
	// so in the window (base, base+60s] we got 3 + 1 = 4 firings
	t.Logf("firings in a 60s span: 4 with limit=3 (boundary inclusive-exclusive)")
}

// Probe 2: shrinking limit on reload — can capacity get stuck?
func TestProbeShrinkLimit(t *testing.T) {
	rl := newRateLimiter()
	base := time.Now()
	for i := 0; i < 10; i++ {
		rl.allow("h", 10, base.Add(time.Duration(i)*time.Second))
	}
	// operator reloads with limit 2
	if rl.allow("h", 2, base.Add(11*time.Second)) {
		t.Fatalf("expected denial")
	}
	t.Logf("len after denial: %d", len(rl.firings["h"]))
	// recovers once old entries age out
	if !rl.allow("h", 2, base.Add(70*time.Second)) {
		t.Fatalf("no recovery")
	}
}

// Probe 3: the recent[:0] aliasing — is the stored slice ever corrupted?
func TestProbeAliasing(t *testing.T) {
	rl := newRateLimiter()
	base := time.Now()
	for i := 0; i < 5; i++ {
		rl.allow("h", 12, base.Add(time.Duration(i)*time.Second))
	}
	got := rl.firings["h"]
	if len(got) != 5 {
		t.Fatalf("len=%d", len(got))
	}
	// age out the first 2
	rl.allow("h", 12, base.Add(62*time.Second))
	for i, ts := range rl.firings["h"] {
		t.Logf("[%d] %v", i, ts.Sub(base))
	}
}

// Probe 4: attrImpl on odd inputs
func TestProbeAttrImpl(t *testing.T) {
	for _, expr := range []string{
		`attr(t.attrs, "nope") == ""`,
		`attr(t.attrs, "pr") == "7"`,
		`size(attr(t.attrs, "pr")) == 1`,
	} {
		p, err := compilePredicate(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if !p.matches(Payload{Attrs: map[string]string{"pr": "7"}}, nil) {
			t.Errorf("%s did not match", expr)
		}
	}
	// nil attrs map
	p, err := compilePredicate(`attr(t.attrs, "pr") == ""`)
	if err != nil {
		t.Fatal(err)
	}
	if !p.matches(Payload{}, nil) {
		t.Error("nil attrs did not match")
	}
}
