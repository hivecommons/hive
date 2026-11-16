package dashboard

import (
	"testing"
	"time"
)

func TestWarnGateDedupesWithinInterval(t *testing.T) {
	g := &warnGate{interval: time.Minute}
	t0 := time.Unix(1000, 0)
	if _, ok := g.allow("k", t0); !ok {
		t.Fatal("first call must log")
	}
	for i := 0; i < 5; i++ {
		if _, ok := g.allow("k", t0.Add(time.Second)); ok {
			t.Fatal("repeat within interval must be suppressed")
		}
	}
	if _, ok := g.allow("other", t0); !ok {
		t.Fatal("distinct key must log")
	}
	n, ok := g.allow("k", t0.Add(2*time.Minute))
	if !ok || n != 5 {
		t.Fatalf("got n=%d ok=%v, want 5 true", n, ok)
	}
}
