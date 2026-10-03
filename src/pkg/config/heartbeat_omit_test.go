package config

import (
	"reflect"
	"testing"
)

func TestHeartbeatOmitClasses(t *testing.T) {
	got, err := HeartbeatOmitClasses([]string{" Users ", "repos", "users", ""})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"repos", "users"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got, err := HeartbeatOmitClasses(nil); err != nil || got != nil {
		t.Fatalf("nil input: %v %v", got, err)
	}
	if _, err := HeartbeatOmitClasses([]string{"repo"}); err == nil {
		t.Fatal("expected error for unknown class")
	}
}
