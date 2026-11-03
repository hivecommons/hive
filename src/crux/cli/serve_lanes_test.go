package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/hivecommons/hive/crux/lanes"
)

func TestLaneBrokerFailureIsTypedAndRedacted(t *testing.T) {
	addressError := &url.Error{
		Op:  "dial",
		URL: "tls://dummy-user:dummy-password@broker.example:4222",
		Err: errors.New("dummy-backend-detail"),
	}
	for _, test := range []struct {
		name     string
		err      error
		category string
	}{
		{"conflict", lanes.ErrConflict, "state conflict"},
		{"unavailable", lanes.ErrUnavailable, "broker unavailable"},
		{"wrapped conflict", fmt.Errorf("%w: %w", lanes.ErrConflict, addressError), "state conflict"},
		{"wrapped unavailable", fmt.Errorf("%w: %w", lanes.ErrUnavailable, addressError), "broker unavailable"},
		{"address credentials", addressError, "initialization error"},
		{"arbitrary error", errors.New("dummy-arbitrary-secret"), "initialization error"},
		{"untyped conflict text", errors.New("lane state conflict"), "initialization error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			e := &Env{Stdout: &stdout, Stderr: &stderr}
			if code := e.laneBrokerFailure(test.err); code != ExitError {
				t.Fatalf("exit = %d, want %d", code, ExitError)
			}
			want := "crux: lanes: cannot initialize durable broker (" + test.category + ")\n"
			if stderr.String() != want || stdout.Len() != 0 {
				t.Fatalf("stdout = %q, stderr = %q; want only %q", stdout.String(), stderr.String(), want)
			}
		})
	}
}
