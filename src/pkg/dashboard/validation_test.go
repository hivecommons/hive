package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// ---------- unit tests for validation functions ----------

func TestValidateDisplayName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "scanner", false},
		{"valid with spaces", "My Scanner Agent", false},
		{"valid with hyphens", "ci-maintainer", false},
		{"valid with underscores", "my_agent", false},
		{"valid empty", "", false},
		{"too long", strings.Repeat("a", maxDisplayNameLen+1), true},
		{"exactly max", strings.Repeat("a", maxDisplayNameLen), false},
		{"html tags", "<script>alert(1)</script>", true},
		{"special chars", "agent;id;", true},
		{"underscores allowed", "__proto__", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDisplayName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateDisplayName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateDescription(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "A helpful agent that scans repos", false},
		{"valid empty", "", false},
		{"too long", strings.Repeat("x", maxDescriptionLen+1), true},
		{"exactly max", strings.Repeat("x", maxDescriptionLen), false},
		{"html tags", "Hello <b>world</b>", true},
		{"script tag", "<script>alert(1)</script>", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDescription(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateDescription(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateLaunchCmd(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid claude", "claude --model sonnet --dangerously-skip-permissions", false},
		{"valid copilot", "/usr/bin/copilot --allow-all --model gpt-4o", false},
		{"command injection semicolon", ";id;", true},
		{"command injection and", "cmd && rm -rf /", true},
		{"command injection or", "cmd || evil", true},
		{"command injection pipe", "cmd | evil", true},
		{"command injection backtick", "cmd `evil`", true},
		{"command injection subshell", "cmd $(evil)", true},
		{"valid empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLaunchCmd(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateLaunchCmd(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateAgentGeneralInput(t *testing.T) {
	tests := []struct {
		name    string
		body    map[string]interface{}
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid body",
			body:    map[string]interface{}{"displayName": "scanner", "color": "#FF5733", "sortOrder": float64(5)},
			wantErr: false,
		},
		{
			name:    "invalid color red",
			body:    map[string]interface{}{"color": "red"},
			wantErr: true,
			errMsg:  "color must be a valid hex color",
		},
		{
			name:    "invalid color ZZZ",
			body:    map[string]interface{}{"color": "#ZZZ"},
			wantErr: true,
			errMsg:  "color must be a valid hex color",
		},
		{
			name:    "valid color 6-digit",
			body:    map[string]interface{}{"color": "#AABBCC"},
			wantErr: false,
		},
		{
			name:    "valid color 3-digit",
			body:    map[string]interface{}{"color": "#FFF"},
			wantErr: false,
		},
		{
			name:    "valid color 3-digit lowercase",
			body:    map[string]interface{}{"color": "#abc"},
			wantErr: false,
		},
		{
			name:    "invalid role __proto__",
			body:    map[string]interface{}{"role": "__proto__"},
			wantErr: true,
			errMsg:  "role must be one of",
		},
		{
			name:    "valid role",
			body:    map[string]interface{}{"role": "scanner"},
			wantErr: false,
		},
		{
			name:    "negative sortOrder",
			body:    map[string]interface{}{"sortOrder": float64(-1)},
			wantErr: true,
			errMsg:  "sortOrder must be between 0 and",
		},
		{
			name:    "sortOrder too large",
			body:    map[string]interface{}{"sortOrder": float64(1000)},
			wantErr: true,
			errMsg:  "sortOrder must be between 0 and",
		},
		{
			name:    "valid sortOrder max",
			body:    map[string]interface{}{"sortOrder": float64(999)},
			wantErr: false,
		},
		{
			name:    "invalid beadRole",
			body:    map[string]interface{}{"beadRole": "x"},
			wantErr: true,
			errMsg:  "beadRole must be",
		},
		{
			name:    "valid beadRole worker",
			body:    map[string]interface{}{"beadRole": "worker"},
			wantErr: false,
		},
		{
			name:    "valid beadRole supervisor",
			body:    map[string]interface{}{"beadRole": "supervisor"},
			wantErr: false,
		},
		{
			name:    "command injection in launchCmd",
			body:    map[string]interface{}{"launchCmd": ";id;"},
			wantErr: true,
			errMsg:  "shell operators",
		},
		{
			name:    "invalid kickTemplate",
			body:    map[string]interface{}{"kickTemplate": "../../etc/passwd"},
			wantErr: true,
			errMsg:  "kickTemplate must match",
		},
		{
			name:    "valid kickTemplate",
			body:    map[string]interface{}{"kickTemplate": "scanner-kick.md"},
			wantErr: false,
		},
		{
			name:    "invalid restartStrategy",
			body:    map[string]interface{}{"restartStrategy": "delayed"},
			wantErr: true,
			errMsg:  "restartStrategy must be",
		},
		{
			name:    "valid restartStrategy empty",
			body:    map[string]interface{}{"restartStrategy": ""},
			wantErr: false,
		},
		{
			name:    "valid restartStrategy immediate",
			body:    map[string]interface{}{"restartStrategy": "immediate"},
			wantErr: false,
		},
		{
			name:    "emoji too long",
			body:    map[string]interface{}{"emoji": "12345"},
			wantErr: true,
			errMsg:  "emoji must be at most",
		},
		{
			name:    "flag emoji accepted (multi-byte rune)",
			body:    map[string]interface{}{"emoji": "\U0001F1FA\U0001F1F8"},
			wantErr: false,
		},
		{
			name:    "single emoji accepted",
			body:    map[string]interface{}{"emoji": "\U0001F680"},
			wantErr: false,
		},
		{
			name:    "empty role rejected",
			body:    map[string]interface{}{"role": ""},
			wantErr: true,
			errMsg:  "role must not be empty",
		},
		{
			name:    "staleTimeout negative",
			body:    map[string]interface{}{"staleTimeout": float64(-1)},
			wantErr: true,
			errMsg:  "staleTimeout",
		},
		{
			name:    "staleTimeout too small",
			body:    map[string]interface{}{"staleTimeout": float64(10)},
			wantErr: true,
			errMsg:  "staleTimeout",
		},
		{
			name:    "staleTimeout zero allowed",
			body:    map[string]interface{}{"staleTimeout": float64(0)},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAgentGeneralInput(tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateAgentGeneralInput() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errMsg != "" && err != nil {
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			}
		})
	}
}

func TestValidateGovernorThresholds(t *testing.T) {
	tests := []struct {
		name    string
		body    map[string]int
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid ascending",
			body:    map[string]int{"quiet": 2, "busy": 10, "surge": 20},
			wantErr: false,
		},
		{
			name:    "negative value",
			body:    map[string]int{"quiet": -1},
			wantErr: true,
			errMsg:  "must be >= 0",
		},
		{
			name:    "all zero",
			body:    map[string]int{"quiet": 0, "busy": 0, "surge": 0},
			wantErr: false,
		},
		{
			name:    "quiet > busy",
			body:    map[string]int{"quiet": 15, "busy": 10},
			wantErr: true,
			errMsg:  "quiet threshold",
		},
		{
			name:    "busy > surge",
			body:    map[string]int{"busy": 25, "surge": 20},
			wantErr: true,
			errMsg:  "busy threshold",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGovernorThresholds(tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateGovernorThresholds() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errMsg != "" && err != nil && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
			}
		})
	}
}

func TestValidateGovernorHealth(t *testing.T) {
	tests := []struct {
		name               string
		healthcheckInterval int
		restartCooldown    int
		wantErr            bool
	}{
		{"valid", 120, 30, false},
		{"zero values", 0, 0, false},
		{"healthcheck too low", 10, 0, true},
		{"healthcheck too high", 5000, 0, true},
		{"cooldown too low", 0, 5, true},
		{"cooldown too high", 0, 5000, true},
		{"min healthcheck", minHealthcheckInterval, 0, false},
		{"max healthcheck", maxHealthcheckInterval, 0, false},
		{"min cooldown", 0, minRestartCooldown, false},
		{"max cooldown", 0, maxRestartCooldown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGovernorHealth(tt.healthcheckInterval, tt.restartCooldown)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateGovernorHealth(%d, %d) error = %v, wantErr %v",
					tt.healthcheckInterval, tt.restartCooldown, err, tt.wantErr)
			}
		})
	}
}

func TestValidateGovernorBudget(t *testing.T) {
	tests := []struct {
		name        string
		totalTokens int64
		periodDays  int
		criticalPct int
		wantErr     bool
		errMsg      string
	}{
		{"valid", 1000000, 30, 80, false, ""},
		{"zero tokens", 0, 1, 90, false, ""},
		{"negative tokens", -1, 1, 90, true, "totalTokens must be >= 0"},
		{"criticalPct 200", 0, 1, 200, true, "criticalPct must be between"},
		{"criticalPct 0", 0, 1, 0, true, "criticalPct must be between"},
		{"criticalPct 1", 0, 1, 1, false, ""},
		{"criticalPct 100", 0, 1, 100, false, ""},
		{"periodDays too high", 0, 400, 90, true, "periodDays must be between"},
		{"periodDays 0", 0, 0, 90, true, "periodDays must be between"},
		{"periodDays 1", 0, 1, 90, false, ""},
		{"periodDays 365", 0, 365, 90, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGovernorBudget(tt.totalTokens, tt.periodDays, tt.criticalPct)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateGovernorBudget() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errMsg != "" && err != nil && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
			}
		})
	}
}

func TestValidateNotificationURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://ntfy.sh", false},
		{"valid empty", "", false},
		{"valid masked", "••••1234", false},
		{"xss javascript", "javascript:alert(1)", true},
		{"http not https", "http://example.com", true},
		{"no protocol", "example.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNotificationURL(tt.url, "ntfyServer")
			if (err != nil) != tt.wantErr {
				t.Errorf("validateNotificationURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestValidateGovernorLogging(t *testing.T) {
	tests := []struct {
		name       string
		dir        string
		maxSizeMB  int
		maxAgeDays int
		wantErr    bool
	}{
		{"valid", "/data/logs", 100, 30, false},
		{"empty dir", "", 100, 30, false},
		{"bad dir prefix", "/tmp/logs", 100, 30, true},
		{"path traversal rejected", "/etc/passwd", 100, 30, true},
		{"size too small", "/data/logs", 0, 30, false}, // 0 means not set
		{"size too large", "/data/logs", 1001, 30, true},
		{"age too large", "/data/logs", 100, 400, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGovernorLogging(tt.dir, tt.maxSizeMB, tt.maxAgeDays)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateGovernorLogging() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateGovernorLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		wantErr bool
	}{
		{"valid", []string{"hold", "wip", "do-not-merge"}, false},
		{"html in label", []string{"hold", "<script>alert(1)</script>"}, true},
		{"empty list", []string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGovernorLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateGovernorLabels() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// ---------- integration tests via HTTP handlers ----------

func TestAgentConfigGeneralValidation(t *testing.T) {
	s, _ := apiServer(t)

	tests := []struct {
		name       string
		body       map[string]interface{}
		wantStatus int
	}{
		{
			name:       "command injection in launchCmd rejected",
			body:       map[string]interface{}{"launchCmd": ";id;"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "prototype pollution role rejected",
			body:       map[string]interface{}{"role": "__proto__"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid color red rejected",
			body:       map[string]interface{}{"color": "red"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid hex color rejected",
			body:       map[string]interface{}{"color": "#ZZZ"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "negative sortOrder rejected",
			body:       map[string]interface{}{"sortOrder": float64(-1)},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid beadRole rejected",
			body:       map[string]interface{}{"beadRole": "x"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "xss in displayName rejected",
			body:       map[string]interface{}{"displayName": "<script>alert(1)</script>"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid config accepted",
			body:       map[string]interface{}{"displayName": "Scanner Bot", "color": "#FF5733", "role": "scanner", "sortOrder": float64(5)},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPut(s, "/api/config/agent/scanner/general", tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("PUT general %s: status = %d, want %d, body = %s",
					tt.name, rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestGovernorThresholdsValidation(t *testing.T) {
	s, _ := apiServer(t)

	tests := []struct {
		name       string
		body       map[string]int
		wantStatus int
	}{
		{
			name:       "negative threshold rejected",
			body:       map[string]int{"quiet": -5},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "quiet > busy rejected",
			body:       map[string]int{"quiet": 20, "busy": 10},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid thresholds accepted",
			body:       map[string]int{"quiet": 2, "busy": 10, "surge": 20},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPut(s, "/api/config/governor/thresholds", tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("PUT thresholds %s: status = %d, want %d, body = %s",
					tt.name, rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestGovernorBudgetValidation(t *testing.T) {
	s, _ := apiServer(t)

	tests := []struct {
		name       string
		body       map[string]interface{}
		wantStatus int
	}{
		{
			name:       "criticalPct 200 rejected",
			body:       map[string]interface{}{"criticalPct": float64(200)},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "negative totalTokens rejected",
			body:       map[string]interface{}{"totalTokens": float64(-1)},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid budget accepted",
			body:       map[string]interface{}{"totalTokens": float64(1000000), "periodDays": float64(30), "criticalPct": float64(80)},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPut(s, "/api/config/governor/budget", tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("PUT budget %s: status = %d, want %d, body = %s",
					tt.name, rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestGovernorNotificationsValidation(t *testing.T) {
	s, _ := apiServer(t)

	tests := []struct {
		name       string
		body       map[string]interface{}
		wantStatus int
	}{
		{
			name:       "javascript XSS in ntfyServer rejected",
			body:       map[string]interface{}{"ntfyServer": "javascript:alert(1)"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid https ntfyServer accepted",
			body:       map[string]interface{}{"ntfyServer": "https://ntfy.sh"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "empty ntfyServer accepted",
			body:       map[string]interface{}{"ntfyServer": ""},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPut(s, "/api/config/governor/notifications", tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("PUT notifications %s: status = %d, want %d, body = %s",
					tt.name, rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestGovernorHealthValidation(t *testing.T) {
	s, _ := apiServer(t)

	tests := []struct {
		name       string
		body       map[string]interface{}
		wantStatus int
	}{
		{
			name:       "healthcheck too low rejected",
			body:       map[string]interface{}{"healthcheckInterval": float64(10)},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid health accepted",
			body:       map[string]interface{}{"healthcheckInterval": float64(120), "restartCooldown": float64(30)},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPut(s, "/api/config/governor/health", tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("PUT health %s: status = %d, want %d, body = %s",
					tt.name, rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestGovernorLabelsValidation(t *testing.T) {
	s, _ := apiServer(t)

	rec := doPut(s, "/api/config/governor/labels", map[string]interface{}{
		"labels": []interface{}{"hold", "<script>alert(1)</script>"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PUT labels with HTML: status = %d, want %d, body = %s",
			rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	rec = doPut(s, "/api/config/governor/labels", map[string]interface{}{
		"labels": []interface{}{"hold", "wip"},
	})
	if rec.Code != http.StatusOK {
		t.Errorf("PUT labels valid: status = %d, want %d, body = %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}
}
