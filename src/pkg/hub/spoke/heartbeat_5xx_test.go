package spoke

import (
	"net/http"
	"testing"
)

func TestHubHeartbeatStatusIsDefect(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, false},
		{http.StatusForbidden, false},
		{http.StatusInternalServerError, true},
		{http.StatusNotImplemented, true},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
		{http.StatusGatewayTimeout, false},
	}
	for _, tt := range tests {
		if got := hubHeartbeatStatusIsDefect(tt.status); got != tt.want {
			t.Errorf("hubHeartbeatStatusIsDefect(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}
