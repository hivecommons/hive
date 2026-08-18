package checkpoint

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestTransportCheckpointsWritesAndFailsClosed(t *testing.T) {
	called, sent := 0, 0
	ctx := WithCallback(context.Background(), func(context.Context, string) error {
		called++
		return errors.New("state branch unavailable")
	})
	transport := Transport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: http.StatusOK}, nil
	})}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.test/resource", nil)
	if _, err := transport.RoundTrip(request); err == nil || called != 1 || sent != 0 {
		t.Fatalf("write did not fail closed before transport: called=%d sent=%d err=%v", called, sent, err)
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/resource", nil)
	if _, err := transport.RoundTrip(request); err != nil || called != 1 || sent != 1 {
		t.Fatalf("read unexpectedly checkpointed: called=%d sent=%d err=%v", called, sent, err)
	}
}
