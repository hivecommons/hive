package config

import (
	"strings"
	"testing"
)

func TestValidateBobDisplayName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty inherits agent name", "", false},
		{"hive prefix", "hive-scanner", false},
		{"dots and underscores", "hive.scanner_01", false},
		{"max length", strings.Repeat("a", MaxBobDisplayNameLen), false},
		{"too long", strings.Repeat("a", MaxBobDisplayNameLen+1), true},
		{"space", "hive scanner", true},
		{"newline", "hive\nscanner", true},
		{"slash", "hive/scanner", true},
		{"shell metachar", "hive$(id)", true},
		{"non-ascii", "hivé", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBobDisplayName(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateBobDisplayName(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
		})
	}
}
