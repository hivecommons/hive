package knowledge

import (
	"testing"
)

func TestMaturityLevelString(t *testing.T) {
	tests := []struct {
		level MaturityLevel
		want  string
	}{
		{MaturityIdea, "idea"},
		{MaturityDev, "development"},
		{MaturityCI, "ci-cd"},
		{MaturityFullAuto, "full-auto"},
		{MaturityLevel(99), "unknown(99)"},
	}

	for _, tt := range tests {
		if got := tt.level.String(); got != tt.want {
			t.Errorf("MaturityLevel(%d).String() = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestMaturityLevelTestMode(t *testing.T) {
	tests := []struct {
		level MaturityLevel
		want  string
	}{
		{MaturityIdea, "suggest"},
		{MaturityDev, "suggest"},
		{MaturityCI, "gate"},
		{MaturityFullAuto, "tdd"},
	}

	for _, tt := range tests {
		if got := tt.level.TestMode(); got != tt.want {
			t.Errorf("MaturityLevel(%d).TestMode() = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestClassifyMaturity(t *testing.T) {
	tests := []struct {
		name    string
		signals MaturitySignals
		want    MaturityLevel
	}{
		{
			name:    "no tests, no CI → idea",
			signals: MaturitySignals{},
			want:    MaturityIdea,
		},
		{
			name: "tests but no CI → dev",
			signals: MaturitySignals{
				HasTests:      true,
				TestFileCount: 5,
			},
			want: MaturityDev,
		},
		{
			name: "CI without coverage config → ci",
			signals: MaturitySignals{
				HasTests:        true,
				HasCI:           true,
				TestFileCount:   10,
				CIWorkflowCount: 2,
			},
			want: MaturityCI,
		},
		{
			name: "CI with coverage config but no TDD markers → ci",
			signals: MaturitySignals{
				HasTests:          true,
				HasCI:             true,
				HasCoverageConfig: true,
				TestFileCount:     20,
				CIWorkflowCount:   3,
			},
			want: MaturityCI,
		},
		{
			name: "CI with coverage and TDD markers → full-auto",
			signals: MaturitySignals{
				HasTests:          true,
				HasCI:             true,
				HasCoverageConfig: true,
				HasTDDMarkers:     true,
				TestFileCount:     50,
				CIWorkflowCount:   5,
			},
			want: MaturityFullAuto,
		},
		{
			name: "CI without tests (unusual) → ci",
			signals: MaturitySignals{
				HasCI:           true,
				CIWorkflowCount: 1,
			},
			want: MaturityCI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMaturity(tt.signals)
			if got != tt.want {
				t.Errorf("classifyMaturity() = %s, want %s", got.String(), tt.want.String())
			}
		})
	}
}
