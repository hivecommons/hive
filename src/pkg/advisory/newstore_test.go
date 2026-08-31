package advisory

import "testing"

// TestNewStore covers the constructor (previously 0% covered): every field a
// later Store method dereferences must be initialized, or the first
// ReadNewFindings/digest call on a fresh hive panics instead of reporting.
func TestNewStore(t *testing.T) {
	s := NewStore()
	if s == nil {
		t.Fatal("NewStore returned nil")
	}
	if s.dir != advisoryDir {
		t.Errorf("dir = %q, want %q", s.dir, advisoryDir)
	}
	if s.lastReadPos == nil {
		t.Error("lastReadPos map not initialized — ReadNewFindings would panic on assignment")
	}
	if s.latestDigest != nil {
		t.Errorf("latestDigest = %+v on a fresh store, want nil", s.latestDigest)
	}
}
