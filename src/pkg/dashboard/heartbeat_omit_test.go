package dashboard

import (
	"reflect"
	"testing"
)

func TestHeartbeatOmitForDisplay(t *testing.T) {
	if got := heartbeatOmitForDisplay(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil should become empty slice, got %#v", got)
	}
	if got := heartbeatOmitForDisplay([]string{"users", "bogus"}); len(got) != 0 {
		t.Fatalf("invalid config should display none, got %v", got)
	}
	if got := heartbeatOmitForDisplay([]string{"users", "repos"}); !reflect.DeepEqual(got, []string{"repos", "users"}) {
		t.Fatalf("got %v", got)
	}
}
