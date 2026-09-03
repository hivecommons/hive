package dashboard

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/config"
)

// ---------- validation constants ----------

const (
	// maxDisplayNameLen is the maximum length for agent display names.
	maxDisplayNameLen = 64
	// maxDescriptionLen is the maximum length for agent descriptions.
	maxDescriptionLen = 2000
	// maxEmojiLen is the maximum length for emoji fields (single emoji + variation selectors).
	maxEmojiLen = 4
	// maxSortOrder is the upper bound for agent sort order.
	maxSortOrder = 999
	// minStaleTimeout is the minimum non-zero stale timeout in seconds.
	minStaleTimeout = 60
	// maxStaleTimeout is the maximum stale timeout in seconds (24 hours).
	maxStaleTimeout = 86400

	// minHealthcheckInterval is the minimum healthcheck interval in seconds.
	minHealthcheckInterval = 60
	// maxHealthcheckInterval is the maximum healthcheck interval in seconds (1 hour).
	maxHealthcheckInterval = 3600
	// minRestartCooldown is the minimum restart cooldown in seconds.
	minRestartCooldown = 10
	// maxRestartCooldown is the maximum restart cooldown in seconds (1 hour).
	maxRestartCooldown = 3600

	// minBudgetPeriodDays is the minimum budget period in days.
	minBudgetPeriodDays = 1
	// maxBudgetPeriodDays is the maximum budget period in days.
	maxBudgetPeriodDays = 365
	// minCriticalPct is the minimum critical budget percentage.
	minCriticalPct = 1
	// maxCriticalPct is the maximum critical budget percentage.
	maxCriticalPct = 100

	// minLoggingMaxSizeMB is the minimum log file size in MB.
	minLoggingMaxSizeMB = 1
	// maxLoggingMaxSizeMB is the maximum log file size in MB.
	maxLoggingMaxSizeMB = 1000
	// minLoggingMaxAgeDays is the minimum log retention in days.
	minLoggingMaxAgeDays = 1
	// maxLoggingMaxAgeDays is the maximum log retention in days.
	maxLoggingMaxAgeDays = 365

	// requiredLoggingDirPrefix is the required prefix for logging directories.
	requiredLoggingDirPrefix = "/data/"
)

// ---------- regex patterns ----------

var (
	// colorPattern matches valid 3-digit or 6-digit hex color codes.
	colorPattern = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
	// displayNamePattern matches alphanumeric characters, spaces, hyphens, and underscores.
	displayNamePattern = regexp.MustCompile(`^[a-zA-Z0-9 _-]+$`)
	// kickTemplatePattern matches valid kick template filenames.
	kickTemplatePattern = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+\.md$`)
	// shellOperatorPattern matches dangerous shell operators in commands.
	shellOperatorPattern = regexp.MustCompile(`[;|<>` + "`" + `]|\$\(|&&|\|\|`)
)

// knownRoles is the set of valid agent roles.
var knownRoles = map[string]bool{
	"guide":         true,
	"scanner":       true,
	"supervisor":    true,
	"quality":       true,
	"ci-maintainer": true,
	"sec-check":     true,
	"telemetry":     true,
	"operations":    true,
	"architect":     true,
	"strategist":    true,
	"outreach":      true,
	"brainstorm":    true,
	"worker":        true,
}

// validBeadRoles is the set of valid bead roles.
var validBeadRoles = map[string]bool{
	"worker":     true,
	"supervisor": true,
}

// validRestartStrategies is the set of valid restart strategies.
var validRestartStrategies = map[string]bool{
	"":          true,
	"immediate": true,
}

// containsHTMLTags returns true if the string contains HTML/XML tags.
func containsHTMLTags(s string) bool {
	return htmlTagPattern.MatchString(s)
}

// ---------- agent config validation ----------

// validateAgentGeneralInput validates all fields in an agent general config
// update request. Returns nil if all fields are valid, or an error describing
// the first invalid field found.
func validateAgentGeneralInput(body map[string]interface{}) error {
	if v, ok := body["displayName"]; ok {
		if s, ok := v.(string); ok {
			if err := validateDisplayName(s); err != nil {
				return err
			}
		}
	}
	if v, ok := body["description"]; ok {
		if s, ok := v.(string); ok {
			if err := validateDescription(s); err != nil {
				return err
			}
		}
	}
	if v, ok := body["emoji"]; ok {
		if s, ok := v.(string); ok {
			if utf8.RuneCountInString(s) > maxEmojiLen {
				return fmt.Errorf("emoji must be at most %d characters", maxEmojiLen)
			}
		}
	}
	if v, ok := body["color"]; ok {
		if s, ok := v.(string); ok && s != "" {
			if !colorPattern.MatchString(s) {
				return fmt.Errorf("color must be a valid hex color (e.g. #FF5733)")
			}
		}
	}
	if v, ok := body["role"]; ok {
		if s, ok := v.(string); ok {
			if s == "" {
				return fmt.Errorf("role must not be empty")
			}
			if !knownRoles[s] {
				return fmt.Errorf("role must be one of: %s", knownRolesList())
			}
		}
	}
	if v, ok := body["launchCmd"]; ok {
		if s, ok := v.(string); ok {
			if err := validateLaunchCmd(s); err != nil {
				return err
			}
		}
	}
	if v, ok := body["kickTemplate"]; ok {
		if s, ok := v.(string); ok && s != "" {
			if !kickTemplatePattern.MatchString(s) {
				return fmt.Errorf("kickTemplate must match pattern: alphanumeric/underscore/dot/hyphen ending in .md")
			}
		}
	}
	if v, ok := body["beadRole"]; ok {
		if s, ok := v.(string); ok && s != "" {
			if !validBeadRoles[s] {
				return fmt.Errorf("beadRole must be \"worker\" or \"supervisor\"")
			}
		}
	}
	if v, ok := body["restartStrategy"]; ok {
		if s, ok := v.(string); ok {
			if !validRestartStrategies[s] {
				return fmt.Errorf("restartStrategy must be empty or \"immediate\"")
			}
		}
	}
	if v, ok := body["sortOrder"]; ok {
		if f, ok := v.(float64); ok {
			n := int(f)
			if n < 0 || n > maxSortOrder {
				return fmt.Errorf("sortOrder must be between 0 and %d", maxSortOrder)
			}
		}
	}
	if v, ok := body["replicas"]; ok {
		if f, ok := v.(float64); ok {
			n := int(f)
			if n < 1 || n > config.MaxAgentReplicas {
				return fmt.Errorf("replicas must be between 1 and %d", config.MaxAgentReplicas)
			}
		}
	}
	if v, ok := body["staleTimeout"]; ok {
		if f, ok := v.(float64); ok {
			n := int(f)
			if n != 0 && (n < minStaleTimeout || n > maxStaleTimeout) {
				return fmt.Errorf("staleTimeout must be 0 (disabled) or between %d and %d seconds", minStaleTimeout, maxStaleTimeout)
			}
		}
	}
	return nil
}

func validateDisplayName(s string) error {
	if len(s) > maxDisplayNameLen {
		return fmt.Errorf("displayName must be at most %d characters", maxDisplayNameLen)
	}
	if s != "" && !displayNamePattern.MatchString(s) {
		return fmt.Errorf("displayName must contain only alphanumeric characters, spaces, hyphens, and underscores")
	}
	if containsHTMLTags(s) {
		return fmt.Errorf("displayName must not contain HTML tags")
	}
	return nil
}

func validateDescription(s string) error {
	if len(s) > maxDescriptionLen {
		return fmt.Errorf("description must be at most %d characters", maxDescriptionLen)
	}
	if containsHTMLTags(s) {
		return fmt.Errorf("description must not contain HTML tags")
	}
	return nil
}

func validateLaunchCmd(s string) error {
	if shellOperatorPattern.MatchString(s) {
		return fmt.Errorf("launchCmd must not contain shell operators (;, &&, ||, |, `, $())")
	}
	return nil
}

// ---------- governor config validation ----------

// validateGovernorThresholds validates that threshold values are non-negative
// and that quiet <= busy <= surge ordering is maintained when all three are present.
func validateGovernorThresholds(body map[string]int) error {
	for name, val := range body {
		if val < 0 {
			return fmt.Errorf("threshold %q must be >= 0, got %d", name, val)
		}
	}
	// Check ordering if multiple thresholds are provided
	quiet, qOk := body["quiet"]
	busy, bOk := body["busy"]
	surge, sOk := body["surge"]

	if qOk && bOk && quiet > busy {
		return fmt.Errorf("quiet threshold (%d) must be <= busy threshold (%d)", quiet, busy)
	}
	if bOk && sOk && busy > surge {
		return fmt.Errorf("busy threshold (%d) must be <= surge threshold (%d)", busy, surge)
	}
	if qOk && sOk && quiet > surge {
		return fmt.Errorf("quiet threshold (%d) must be <= surge threshold (%d)", quiet, surge)
	}
	return nil
}

// validateGovernorHealth validates health configuration values.
func validateGovernorHealth(healthcheckInterval, restartCooldown int) error {
	if healthcheckInterval != 0 && (healthcheckInterval < minHealthcheckInterval || healthcheckInterval > maxHealthcheckInterval) {
		return fmt.Errorf("healthcheckInterval must be between %d and %d seconds", minHealthcheckInterval, maxHealthcheckInterval)
	}
	if restartCooldown != 0 && (restartCooldown < minRestartCooldown || restartCooldown > maxRestartCooldown) {
		return fmt.Errorf("restartCooldown must be between %d and %d seconds", minRestartCooldown, maxRestartCooldown)
	}
	return nil
}

// validateGovernorBudget validates budget configuration values.
func validateGovernorBudget(totalTokens int64, periodDays, criticalPct int) error {
	if totalTokens < 0 {
		return fmt.Errorf("totalTokens must be >= 0")
	}
	if periodDays < minBudgetPeriodDays || periodDays > maxBudgetPeriodDays {
		return fmt.Errorf("periodDays must be between %d and %d", minBudgetPeriodDays, maxBudgetPeriodDays)
	}
	if criticalPct < minCriticalPct || criticalPct > maxCriticalPct {
		return fmt.Errorf("criticalPct must be between %d and %d", minCriticalPct, maxCriticalPct)
	}
	return nil
}

// validateNotificationURL validates that a notification URL is either empty
// or starts with https:// and doesn't target internal/private addresses.
func validateNotificationURL(rawURL, fieldName string) error {
	if rawURL == "" {
		return nil
	}
	if strings.HasPrefix(rawURL, "•") {
		return nil
	}
	if !strings.HasPrefix(rawURL, "https://") {
		return fmt.Errorf("%s must start with https:// or be empty", fieldName)
	}
	host := strings.TrimPrefix(rawURL, "https://")
	if idx := strings.IndexAny(host, ":/"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.ToLower(host)
	blockedPrefixes := []string{"localhost", "127.", "10.", "172.16.", "172.17.", "172.18.", "172.19.",
		"172.20.", "172.21.", "172.22.", "172.23.", "172.24.", "172.25.", "172.26.", "172.27.",
		"172.28.", "172.29.", "172.30.", "172.31.", "192.168.", "169.254.", "[::1]", "0.0.0.0"}
	for _, p := range blockedPrefixes {
		if strings.HasPrefix(host, p) {
			return fmt.Errorf("%s must not target private/internal addresses", fieldName)
		}
	}
	return nil
}

// validateGovernorLogging validates logging configuration values.
func validateGovernorLogging(dir string, maxSizeMB, maxAgeDays int) error {
	if dir != "" && !strings.HasPrefix(dir, requiredLoggingDirPrefix) {
		return fmt.Errorf("logging dir must start with %s", requiredLoggingDirPrefix)
	}
	if maxSizeMB != 0 && (maxSizeMB < minLoggingMaxSizeMB || maxSizeMB > maxLoggingMaxSizeMB) {
		return fmt.Errorf("maxSizeMB must be between %d and %d", minLoggingMaxSizeMB, maxLoggingMaxSizeMB)
	}
	if maxAgeDays != 0 && (maxAgeDays < minLoggingMaxAgeDays || maxAgeDays > maxLoggingMaxAgeDays) {
		return fmt.Errorf("maxAgeDays must be between %d and %d", minLoggingMaxAgeDays, maxLoggingMaxAgeDays)
	}
	return nil
}

// validateGovernorLabels validates that label strings do not contain HTML tags.
func validateGovernorLabels(labels []string) error {
	for _, label := range labels {
		if containsHTMLTags(label) {
			return fmt.Errorf("label %q must not contain HTML tags", label)
		}
	}
	return nil
}

// ---------- helpers ----------

func knownRolesList() string {
	roles := make([]string, 0, len(knownRoles))
	for r := range knownRoles {
		roles = append(roles, r)
	}
	return strings.Join(roles, ", ")
}
