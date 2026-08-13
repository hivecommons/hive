package github

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func jsonBodyResponse(statusCode int, body any) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(string(data))),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestStartDeviceFlowSuccess(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		resp := DeviceFlowState{
			DeviceCode:      "ABCD-1234",
			UserCode:        "EFGH-5678",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		}
		return jsonBodyResponse(200, resp), nil
	}, func() {
		state, err := StartDeviceFlow("test-client-id", "", "")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if state.DeviceCode != "ABCD-1234" {
			t.Errorf("DeviceCode = %q", state.DeviceCode)
		}
		if state.UserCode != "EFGH-5678" {
			t.Errorf("UserCode = %q", state.UserCode)
		}
		if state.Interval != 5 {
			t.Errorf("Interval = %d", state.Interval)
		}
	})
}

func TestStartDeviceFlowDefaultInterval(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		resp := DeviceFlowState{
			DeviceCode: "code",
			UserCode:   "user",
			Interval:   0,
		}
		return jsonBodyResponse(200, resp), nil
	}, func() {
		state, err := StartDeviceFlow("test-client-id", "", "")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if state.Interval != 5 {
			t.Errorf("zero interval should default to 5, got %d", state.Interval)
		}
	})
}

func TestStartDeviceFlowServerError(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return jsonBodyResponse(500, map[string]string{"error": "server error"}), nil
	}, func() {
		_, err := StartDeviceFlow("test-client-id", "", "")
		if err == nil {
			t.Error("expected error on 500")
		}
	})
}

func TestPollDeviceFlowComplete(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return jsonBodyResponse(200, pollResponse{
			AccessToken: "gho_test_token_12345",
			TokenType:   "bearer",
			Scope:       "repo",
		}), nil
	}, func() {
		token, status, err := PollDeviceFlow("client-id", "device-code", "", "")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if token != "gho_test_token_12345" {
			t.Errorf("token = %q", token)
		}
		if status != "complete" {
			t.Errorf("status = %q", status)
		}
	})
}

func TestPollDeviceFlowPending(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return jsonBodyResponse(200, pollResponse{
			Error:     "authorization_pending",
			ErrorDesc: "waiting for user",
		}), nil
	}, func() {
		token, status, err := PollDeviceFlow("client-id", "device-code", "", "")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if token != "" {
			t.Error("pending should return empty token")
		}
		if status != "authorization_pending" {
			t.Errorf("status = %q", status)
		}
	})
}

func TestPollDeviceFlowSlowDown(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return jsonBodyResponse(200, pollResponse{
			Error: "slow_down",
		}), nil
	}, func() {
		_, status, err := PollDeviceFlow("client-id", "device-code", "", "")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if status != "slow_down" {
			t.Errorf("status = %q", status)
		}
	})
}

func TestPollDeviceFlowError(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return jsonBodyResponse(200, pollResponse{
			Error:     "access_denied",
			ErrorDesc: "user denied",
		}), nil
	}, func() {
		_, _, err := PollDeviceFlow("client-id", "device-code", "", "")
		if err == nil {
			t.Error("expected error on access_denied")
		}
	})
}

func TestValidateTokenSuccess(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return jsonBodyResponse(200, GitHubUser{
			Login:     "testuser",
			AvatarURL: "https://example.com/avatar.png",
		}), nil
	}, func() {
		user, err := ValidateToken("gho_valid_token", "")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if user.Login != "testuser" {
			t.Errorf("login = %q", user.Login)
		}
	})
}

func TestValidateTokenInvalid(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return jsonBodyResponse(401, map[string]string{"message": "Bad credentials"}), nil
	}, func() {
		_, err := ValidateToken("invalid-token", "")
		if err == nil {
			t.Error("expected error on 401")
		}
	})
}
