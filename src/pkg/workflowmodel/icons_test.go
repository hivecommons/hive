package workflowmodel

import (
	"errors"
	"strings"
	"testing"
)

// A column's icon is the empty default, an allowlisted id or a single
// emoji; letters, markup and multi-glyph strings are refused so the SPA's
// chevron renderer never sees anything it would have to escape
// (hivecommons/hive#18033).
func TestValidateColumnIcon(t *testing.T) {
	for _, ok := range []string{"", "bolt", "merge", " eye ", "✅", "🚀", "👍🏽", "👩‍💻", "🇺🇸", "⏸️"} {
		if err := ValidateColumnIcon(ok); err != nil {
			t.Errorf("ValidateColumnIcon(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"sparkle", "Bolt", "<svg>", "<img src=x onerror=alert(1)>", "a", "✅✅✅", "ok ✅", "🚀 launch", "javascript:", "✅\n"} {
		if err := ValidateColumnIcon(bad); err == nil {
			t.Errorf("ValidateColumnIcon(%q) = nil, want error", bad)
		}
	}
}

func TestValidateRejectsBadColumnIcon(t *testing.T) {
	m := HiveDefault(testScope())
	m.Spec.Columns[0].Icon = "<b>x</b>"
	m.Spec.Columns[1].Icon = "bolt"
	err := m.Validate()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), `column "`+m.Spec.Columns[0].ID+`": icon "<b>x</b>"`) {
		t.Errorf("error does not name the column and icon: %v", err)
	}
	if strings.Contains(err.Error(), `"bolt"`) {
		t.Errorf("allowlisted id reported: %v", err)
	}
	m.Spec.Columns[0].Icon = "📥"
	if err := m.Validate(); err != nil {
		t.Errorf("emoji icon rejected: %v", err)
	}
}

func TestColumnIconIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range ColumnIconIDs {
		if seen[id] {
			t.Errorf("duplicate icon id %q", id)
		}
		seen[id] = true
		if strings.TrimSpace(id) != id || strings.ToLower(id) != id {
			t.Errorf("icon id %q is not a trimmed lower-case token", id)
		}
	}
	if !IsColumnIconID(ColumnIconDefault) {
		t.Errorf("default %q is not in the allowlist", ColumnIconDefault)
	}
}
