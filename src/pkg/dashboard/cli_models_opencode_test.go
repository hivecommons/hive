package dashboard

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseOpencodeModels(t *testing.T) {
	out := "\nopencode/hy4-flash-free\nINFO  some banner line\nanthropic/claude-sonnet-5-5\n  openrouter/x/y \n"
	got := parseOpencodeModels(out)
	want := []string{"opencode/hy4-flash-free", "anthropic/claude-sonnet-5-5", "openrouter/x/y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestDiscoverOpencodeModelsFallback(t *testing.T) {
	prev := runOpencodeModelsProbe
	defer func() { runOpencodeModelsProbe = prev }()
	s := &Server{}

	runOpencodeModelsProbe = func() ([]string, error) { return nil, errors.New("no binary") }
	if r := s.discoverOpencodeModels(); !r.fallback || len(r.models) != 0 {
		t.Fatalf("probe error must serve fallback, got %+v", r)
	}
	runOpencodeModelsProbe = func() ([]string, error) { return []string{"opencode/a", "opencode/a", "x/b"}, nil }
	r := s.discoverOpencodeModels()
	if r.fallback || len(r.models) != 2 {
		t.Fatalf("live probe must be authoritative and deduped, got %+v", r)
	}
}
