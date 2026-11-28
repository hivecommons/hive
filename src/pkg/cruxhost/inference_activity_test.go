package cruxhost

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api/callback"
)

func TestInferenceActivityCallback(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	caps := New(Deps{InferenceActivity: func() map[string]time.Time { return map[string]time.Time{"alice": at} }})
	srv := httptest.NewServer(callback.NewServer(caps, "secret").Handler())
	defer srv.Close()
	client, err := callback.NewClient(srv.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.InferenceActivity(context.Background())
	if err != nil || !got["alice"].Equal(at) || len(got) != 1 {
		t.Fatalf("activity = %v, %v", got, err)
	}
	unauthorized, err := callback.NewClient(srv.URL, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unauthorized.InferenceActivity(context.Background()); err == nil {
		t.Fatal("unauthorized activity read succeeded")
	}
}
