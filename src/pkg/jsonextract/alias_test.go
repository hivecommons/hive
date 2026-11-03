package jsonextract

import "testing"

func TestObjectIsTheCruxHelper(t *testing.T) {
	if got := Object("prose {\"a\":1} tail"); got != `{"a":1}` {
		t.Fatalf("Object() = %q", got)
	}
	if got := Object("no json"); got != "" {
		t.Fatalf("Object() on no-JSON text = %q, want empty", got)
	}
}
