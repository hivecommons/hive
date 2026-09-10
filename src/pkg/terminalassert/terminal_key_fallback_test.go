package terminalassert_test

import (
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/terminalassert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoProvisionKey_ErrorBranches(t *testing.T) {
	t.Run("fails when key storage path is read-only or invalid", func(t *testing.T) {
		// Test auto-provisioning failure branch when persistence fails
		err := terminalassert.ProvisionTerminalKeyWithStore(&mockFailingStore{
			shouldFailSave: true,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to persist terminal key")
	})

	t.Run("fails on cryptographic provider error during key generation", func(t *testing.T) {
		// Test auto-provisioning crypto generation failure branch
		err := terminalassert.ProvisionTerminalKeyWithGenerator(&mockFailingGenerator{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "crypto generation failed")
	})
}

type mockFailingStore struct {
	shouldFailSave bool
}

func (m *mockFailingStore) SaveKey(key []byte) error {
	if m.shouldFailSave {
		return errors.New("failed to persist terminal key")
	}
	return nil
}

type mockFailingGenerator struct{}

func (m *mockFailingGenerator) GenerateKey() ([]byte, error) {
	return nil, errors.New("crypto generation failed")
}