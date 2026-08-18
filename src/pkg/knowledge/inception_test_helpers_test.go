package knowledge

import (
	"log/slog"
	"testing"
)

func newTestEngine(t *testing.T) *InceptionEngine {
	t.Helper()
	return NewInceptionEngine(t.TempDir(), nil, slog.Default())
}
