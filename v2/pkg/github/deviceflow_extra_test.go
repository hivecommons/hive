package github

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestValidateTokenBadJSON(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{invalid json`)),
		}, nil
	}, func() {
		_, err := ValidateToken("ghp_test", "")
		if err == nil {
			t.Error("should error on invalid JSON response")
		}
		if !strings.Contains(err.Error(), "parsing user response") {
			t.Errorf("error should mention parsing: %v", err)
		}
	})
}

func TestStartDeviceFlowBadJSON(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`not json`)),
		}, nil
	}, func() {
		_, err := StartDeviceFlow("test-client-id", "", "")
		if err == nil {
			t.Error("should error on invalid JSON response")
		}
	})
}

func TestPollDeviceFlowBadJSON(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`not json`)),
		}, nil
	}, func() {
		_, _, err := PollDeviceFlow("test-client-id", "test-device-code", "", "")
		if err == nil {
			t.Error("should error on invalid JSON response")
		}
	})
}

func TestStartDeviceFlowNetworkError(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return nil, &http.MaxBytesError{}
	}, func() {
		_, err := StartDeviceFlow("test-client-id", "", "")
		if err == nil {
			t.Error("should error on network failure")
		}
	})
}

func TestPollDeviceFlowNetworkError(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return nil, &http.MaxBytesError{}
	}, func() {
		_, _, err := PollDeviceFlow("test-client-id", "test-device-code", "", "")
		if err == nil {
			t.Error("should error on network failure")
		}
	})
}

func TestValidateTokenNetworkError(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return nil, &http.MaxBytesError{}
	}, func() {
		_, err := ValidateToken("ghp_test", "")
		if err == nil {
			t.Error("should error on network failure")
		}
	})
}
